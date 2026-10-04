package bevdirect

// Service answers viewport / point questions by composing a fixed world grid
// of assembled cells, each built straight from BEV tiles. It is what
// bevdirect-serve exposes over HTTP; a Go client can embed it in-process.
//
// State, deliberately minimal:
//   - the on-disk tile cache (Options.CacheDir) — what a browser of
//     kataster.bev.gv.at holds;
//   - an expiring LRU of assembled cells keyed by sha256(cell coordinates).
//
// Nothing is keyed by parcel id, EZ or KG. A parcel can only be obtained by
// asking for a location; a folio only for the part of it inside a bbox.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/paulmach/orb"
	"golang.org/x/sync/singleflight"
)

const (
	MaxSpan   = 0.045 // ° per request edge (~3.5 × 5 km); typical client tiles are 0.02°
	CellSize  = 0.02  // ° — fixed world grid; every answer is composed from cached cells
	cellPad   = 0.004 // ° margin so parcels crossing a cell edge are complete in at least one cell
	cellTiles = 48    // MaxTiles per padded cell (needs ~12 z15 / ~48 z16)
)

var AllLayers = []string{"parcels", "footprints", "landuse"}

// ServiceOptions configures a Service.
type ServiceOptions struct {
	CacheDir  string        // tile cache dir (required for sane performance)
	TileTTL   time.Duration // default 24 h
	CellTTL   time.Duration // default 6 h
	MaxCells  int           // assembled cells kept in memory, ~3–6 MB each (default 64)
	Workers   int           // parallel tile downloads per cell (default 16)
	MaxConns  int           // total concurrent connections to BEV (default 24; measured latency is flat up to there)
	Prefetch  int           // ring of neighbouring cells warmed in the background after a viewport (default 1; 0 = off)
	Wait      time.Duration // how long a request blocks on cold cells before answering partial + Ready:false (default 20 s)
	UserAgent string
	Log       func(format string, args ...any)
}

type Service struct {
	o       ServiceOptions
	opts    Options
	cells   *Store
	sf      singleflight.Group
	pre     chan cell
	active  atomic.Int32 // foreground composes in flight; the prefetcher yields to them
	sweptAt atomic.Int64
}

func NewService(o ServiceOptions) *Service {
	if o.TileTTL <= 0 {
		o.TileTTL = 24 * time.Hour
	}
	if o.CellTTL <= 0 {
		o.CellTTL = 6 * time.Hour
	}
	if o.MaxCells <= 0 {
		o.MaxCells = 64
	}
	if o.Workers <= 0 {
		o.Workers = 16
	}
	if o.MaxConns <= 0 {
		o.MaxConns = 24
	}
	if o.Wait <= 0 {
		o.Wait = 20 * time.Second
	}
	if o.Log == nil {
		o.Log = func(string, ...any) {}
	}
	s := &Service{o: o, cells: NewStore(o.CellTTL, o.MaxCells), pre: make(chan cell, 256)}
	s.opts = Options{Layers: AllLayers, Workers: o.Workers, CacheDir: o.CacheDir, CacheTTL: o.TileTTL, Enrich: true,
		MaxTiles: cellTiles, UserAgent: o.UserAgent, Log: o.Log, HTTPClient: limitedClient(o.MaxConns),
		assemblyGate: make(chan struct{}, runtime.GOMAXPROCS(0))}
	if o.Prefetch > 0 {
		go s.prefetcher()
		go s.prefetcher()
	}
	if o.CacheDir != "" {
		go s.tileSweeper()
	}
	return s
}

// tileSweeper deletes tile files older than TileTTL (default 24 h), once at
// start and then hourly. Without it an expired tile is merely ignored and
// re-fetched, and the disk cache — and with it stale tiles/ETag-like state —
// grows forever. BEV's tiles change at most monthly, so the TTL governs how
// quickly a freshly published cadastre reaches clients.
func (s *Service) tileSweeper() {
	for {
		n, b := SweepTileCache(s.o.CacheDir, s.o.TileTTL)
		if n > 0 {
			s.o.Log("tile cache sweep: removed %d expired tiles (%.1f MB, ttl %s)", n, float64(b)/1e6, s.o.TileTTL)
		}
		s.sweptAt.Store(time.Now().Unix())
		time.Sleep(time.Hour)
	}
}

// SweepTileCache removes *.pbf files under dir whose mtime is older than ttl
// and prunes empty x/ and z/ directories. Returns files removed and bytes freed.
func SweepTileCache(dir string, ttl time.Duration) (int, int64) {
	if ttl <= 0 {
		return 0, 0
	}
	cutoff := time.Now().Add(-ttl)
	n, bytes := 0, int64(0)
	var dirs []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != dir {
				dirs = append(dirs, p)
			}
			return nil
		}
		if !strings.HasSuffix(p, ".pbf") {
			return nil
		}
		fi, err := d.Info()
		if err != nil || !fi.ModTime().Before(cutoff) {
			return nil
		}
		if os.Remove(p) == nil {
			n++
			bytes += fi.Size()
		}
		return nil
	})
	// deepest first so emptied x/ dirs let their z/ parent go too
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, d := range dirs {
		_ = os.Remove(d) // fails (harmlessly) when not empty
	}
	return n, bytes
}

// TileCacheSweptAt is the unix time of the last sweep (0 = none yet), for /health.
func (s *Service) TileCacheSweptAt() int64 { return s.sweptAt.Load() }

// CellsCached / PrefetchQueued are for health endpoints.
func (s *Service) CellsCached() int    { return s.cells.Len() }
func (s *Service) PrefetchQueued() int { return len(s.pre) }

// cell is one square of the fixed world grid.
type cell struct{ ix, iy int }

func cellOf(lon, lat float64) cell {
	return cell{int(math.Floor(lon / CellSize)), int(math.Floor(lat / CellSize))}
}
func (c cell) bbox() (w, s, e, n float64) {
	w, s = float64(c.ix)*CellSize, float64(c.iy)*CellSize
	return w, s, w + CellSize, s + CellSize
}
func (c cell) key() string { return Hash(fmt.Sprintf("cell|%d|%d", c.ix, c.iy)) }

func cellsFor(w, s, e, n float64) []cell {
	lo, hi := cellOf(w, s), cellOf(e-1e-9, n-1e-9)
	var out []cell
	for ix := lo.ix; ix <= hi.ix; ix++ {
		for iy := lo.iy; iy <= hi.iy; iy++ {
			out = append(out, cell{ix, iy})
		}
	}
	return out
}

// ErrBBox is returned for empty or oversized bboxes.
var ErrBBox = errors.New("bevdirect: bbox empty or larger than MaxSpan")

func checkBBox(w, s, e, n float64) error {
	if e <= w || n <= s || e-w > MaxSpan+1e-9 || n-s > MaxSpan+1e-9 {
		return fmt.Errorf("%w (%.4f×%.4f°, max %.3f°)", ErrBBox, e-w, n-s, MaxSpan)
	}
	return nil
}

// fetchCell assembles one padded cell once, coalesced across callers.
func (s *Service) fetchCell(ctx context.Context, c cell) (*Result, error) {
	if v, ok := s.cells.Get(c.key()); ok {
		return v.(*Result), nil
	}
	v, err, _ := s.sf.Do(c.key(), func() (any, error) {
		if v, ok := s.cells.Get(c.key()); ok {
			return v, nil
		}
		w, so, e, n := c.bbox()
		t0 := time.Now()
		res, err := Fetch(context.WithoutCancel(ctx), w-cellPad, so-cellPad, e+cellPad, n+cellPad, s.opts)
		if err != nil {
			return nil, err
		}
		if res.Ready {
			s.cells.Put(c.key(), res)
		}
		s.o.Log("bevdirect cell %d/%d: %d parcels %d fp %d lu in %s (dl %s, asm %s, ready=%v)", c.ix, c.iy,
			len(res.Parcels), len(res.Footprints), len(res.Landuse), time.Since(t0).Round(time.Millisecond),
			res.Stats.Download.Round(time.Millisecond), res.Stats.Assembly.Round(time.Millisecond), res.Ready)
		return res, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*Result), nil
}

// Viewport composes the cells covering bbox and returns the features that
// intersect it: parcels dedup by id (complete copy wins, then larger area),
// footprint/landuse pieces dedup by their tile-scoped id. Afterwards the ring
// of neighbouring cells is queued for background warming.
func (s *Service) Viewport(ctx context.Context, w, so, e, n float64) (*Result, error) {
	if err := checkBBox(w, so, e, n); err != nil {
		return nil, err
	}
	res, err := s.compose(ctx, w, so, e, n)
	if err == nil {
		s.warmAround(w, so, e, n)
	}
	return res, err
}

func (s *Service) compose(ctx context.Context, w, so, e, n float64) (*Result, error) {
	s.active.Add(1)
	defer s.active.Add(-1)
	cs := cellsFor(w, so, e, n)
	type done struct {
		i   int
		res *Result
		err error
	}
	ch := make(chan done, len(cs))
	for i, c := range cs {
		go func(i int, c cell) { r, err := s.fetchCell(ctx, c); ch <- done{i, r, err} }(i, c)
	}
	// Block at most Wait: cells still assembling keep going in the background
	// (fetchCell detaches from ctx) and the answer is partial with Ready:false
	// — the client re-asks shortly.
	results := make([]*Result, len(cs))
	errs := make([]error, len(cs))
	pending := len(cs)
	timer := time.NewTimer(s.o.Wait)
	defer timer.Stop()
	for pending > 0 {
		select {
		case d := <-ch:
			results[d.i], errs[d.i] = d.res, d.err
			pending--
		case <-timer.C:
			pending = 0
		case <-ctx.Done():
			pending = 0
		}
	}
	out := &Result{Ready: true, Source: "bev-direct", Notice: Notice(), LicenseURL: LicenseURL, FetchedAt: time.Now().UTC()}
	inBox := func(b orb.Bound) bool {
		return b.Max[0] >= w && b.Min[0] <= e && b.Max[1] >= so && b.Min[1] <= n
	}
	seenP := map[string]int{}
	seenF, seenL := map[string]bool{}, map[string]bool{}
	for i, r := range results {
		if errs[i] != nil {
			if len(cs) == 1 {
				return nil, errs[i]
			}
			out.Ready = false
			continue
		}
		if r == nil { // still assembling
			out.Ready = false
			continue
		}
		out.Ready = out.Ready && r.Ready
		st := &out.Stats
		st.TilesZ15 += r.Stats.TilesZ15
		st.TilesZ16 += r.Stats.TilesZ16
		st.TileErrors += r.Stats.TileErrors
		st.CacheHits += r.Stats.CacheHits
		st.Bytes += r.Stats.Bytes
		st.Pieces += r.Stats.Pieces
		st.GuardTrips += r.Stats.GuardTrips
		if r.Stats.Download > st.Download {
			st.Download = r.Stats.Download
		}
		if r.Stats.Assembly > st.Assembly {
			st.Assembly = r.Stats.Assembly
		}
		if r.FetchedAt.Before(out.FetchedAt) {
			out.FetchedAt = r.FetchedAt
		}
		for j := range r.Parcels {
			p := &r.Parcels[j]
			if p.Geometry == nil || !inBox(p.Geometry.Geometry().Bound()) {
				continue
			}
			if k, ok := seenP[p.ParcelID]; ok {
				q := &out.Parcels[k]
				if (p.Complete && !q.Complete) || (p.Complete == q.Complete && p.AreaSqm > q.AreaSqm) {
					out.Parcels[k] = *p
				}
				continue
			}
			seenP[p.ParcelID] = len(out.Parcels)
			out.Parcels = append(out.Parcels, *p)
		}
		for _, f := range r.Footprints {
			if !seenF[f.ID] && inBox(f.Geometry.Geometry().Bound()) {
				seenF[f.ID] = true
				out.Footprints = append(out.Footprints, f)
			}
		}
		for _, l := range r.Landuse {
			if !seenL[l.ID] && inBox(l.Geometry.Geometry().Bound()) {
				seenL[l.ID] = true
				out.Landuse = append(out.Landuse, l)
			}
		}
	}
	return out, nil
}

// ParcelAt resolves id from the tiles around lon/lat. Nil, nil = not there.
func (s *Service) ParcelAt(ctx context.Context, id string, lon, lat float64) (*Parcel, error) {
	const d = 0.004
	res, err := s.compose(ctx, lon-d, lat-d, lon+d, lat+d)
	if err != nil {
		return nil, err
	}
	for i := range res.Parcels {
		if res.Parcels[i].ParcelID == id {
			return &res.Parcels[i], nil
		}
	}
	return nil, nil
}

// EZIn lists the parcels of folio kg/ez inside bbox. Always partial by nature.
func (s *Service) EZIn(ctx context.Context, kg, ez string, w, so, e, n float64) ([]Parcel, error) {
	if err := checkBBox(w, so, e, n); err != nil {
		return nil, err
	}
	res, err := s.compose(ctx, w, so, e, n)
	if err != nil {
		return nil, err
	}
	var out []Parcel
	for _, p := range res.Parcels {
		if p.KGCode == kg && p.EZ == ez {
			out = append(out, p)
		}
	}
	return out, nil
}

// KGAt returns the KG of the parcel whose centroid is nearest lon/lat ("" if none).
func (s *Service) KGAt(ctx context.Context, lon, lat float64) (string, error) {
	const d = 0.002
	res, err := s.compose(ctx, lon-d, lat-d, lon+d, lat+d)
	if err != nil {
		return "", err
	}
	best, bestD := "", math.Inf(1)
	for _, p := range res.Parcels {
		dd := (p.Lon-lon)*(p.Lon-lon) + (p.Lat-lat)*(p.Lat-lat)
		if dd < bestD {
			best, bestD = p.KGCode, dd
		}
	}
	return best, nil
}

func (s *Service) warmAround(w, so, e, n float64) {
	ring := s.o.Prefetch
	if ring <= 0 {
		return
	}
	lo, hi := cellOf(w, so), cellOf(e-1e-9, n-1e-9)
	for ix := lo.ix - ring; ix <= hi.ix+ring; ix++ {
		for iy := lo.iy - ring; iy <= hi.iy+ring; iy++ {
			c := cell{ix, iy}
			if _, ok := s.cells.Get(c.key()); ok {
				continue
			}
			select {
			case s.pre <- c:
			default:
				return // queue full; the next request will try again
			}
		}
	}
}

func (s *Service) prefetcher() {
	for c := range s.pre {
		for s.active.Load() > 0 { // never compete with a player's request for BEV connections
			time.Sleep(100 * time.Millisecond)
		}
		if _, ok := s.cells.Get(c.key()); ok {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		_, _ = s.fetchCell(ctx, c)
		cancel()
	}
}

// limitedClient caps total concurrent connections to BEV across all cells.
func limitedClient(maxConns int) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxConnsPerHost = maxConns
	tr.MaxIdleConnsPerHost = maxConns
	return &http.Client{Timeout: 20 * time.Second, Transport: tr}
}
