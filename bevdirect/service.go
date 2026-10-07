package bevdirect

// Service answers viewport / point questions by composing a fixed world grid
// of assembled cells, each built straight from BEV tiles. It is what
// bevdirect-serve exposes over HTTP; a Go client can embed it in-process.
//
// State, deliberately minimal:
//   - a bounded in-memory tile cache (Options.TileCache) — what a browser of
//     the BEV web map holds; tiles never touch the disk;
//   - an expiring LRU of assembled cells keyed by sha256(cell coordinates).
//
// Nothing is keyed by parcel id, EZ or KG. A parcel can only be obtained by
// asking for a location; a folio only for the part of it inside a bbox.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"runtime"
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
	TileCacheBytes int64         // in-memory raw-tile LRU budget (default 1 GiB ≈ 200 KGs of mixed terrain; <0 = no tile cache)
	TileTTL        time.Duration // tiles older than this are refetched (default 24 h)
	CellTTL        time.Duration // default 6 h
	MaxCells       int           // assembled cells kept in memory, ~3–6 MB each (default 64)
	Workers        int           // parallel tile downloads per cell (default 16)
	MaxConns       int           // total concurrent connections to BEV (default 24; measured latency is flat up to there)
	Prefetch       int           // ring of neighbouring cells warmed in the background after a viewport (default 1; 0 = off)
	Wait           time.Duration // how long a request blocks on cold cells before answering partial + Ready:false (default 20 s)
	UserAgent      string
	Log            func(format string, args ...any)
}

type Service struct {
	o       ServiceOptions
	opts    Options
	cells   *Store
	sf      singleflight.Group
	pre     chan cell
	active  atomic.Int32 // foreground composes in flight; the prefetcher yields to them
	tiles   *TileCache
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
	if o.TileCacheBytes == 0 {
		o.TileCacheBytes = 1 << 30
	}
	s := &Service{o: o, cells: NewStore(o.CellTTL, o.MaxCells), pre: make(chan cell, 256)}
	s.tiles = NewTileCache(o.TileCacheBytes, o.TileTTL)
	s.opts = Options{Layers: AllLayers, Workers: o.Workers, TileCache: s.tiles, Enrich: true,
		MaxTiles: cellTiles, UserAgent: o.UserAgent, Log: o.Log, HTTPClient: limitedClient(o.MaxConns),
		assemblyGate: make(chan struct{}, runtime.GOMAXPROCS(0))}
	if o.Prefetch > 0 {
		go s.prefetcher()
		go s.prefetcher()
	}
	if s.tiles != nil {
		go s.tileSweeper()
	}
	return s
}

// tileSweeper drops expired tiles from the in-memory cache hourly so an idle
// server does not pin a day-old cadastre in RAM. BEV's tiles change at most
// monthly, so the TTL governs how quickly a freshly published cadastre reaches
// clients.
func (s *Service) tileSweeper() {
	for {
		n, b := s.tiles.Sweep()
		if n > 0 {
			s.o.Log("tile cache sweep: dropped %d expired tiles (%.1f MB, ttl %s)", n, float64(b)/1e6, s.o.TileTTL)
		}
		s.sweptAt.Store(time.Now().Unix())
		time.Sleep(time.Hour)
	}
}

// TileCacheSweptAt is the unix time of the last sweep (0 = none yet), for /health.
func (s *Service) TileCacheSweptAt() int64 { return s.sweptAt.Load() }

// TileCacheStats reports the in-memory tile cache (tiles, bytes, budget, hit rate).
func (s *Service) TileCacheStats() TileCacheStats { return s.tiles.Stats() }

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
// intersect it: parcels dedup by id, footprint/landuse pieces by their
// tile-scoped member ids (Piece.Keys), both with the same rule — complete copy
// wins, then larger area. Afterwards the ring of neighbouring cells is queued
// for background warming.
//
// Invariant (TestComposeEqualsCellUnion): a viewport over N×M cells is the
// union of the N×M single-cell documents after this dedupe. A footprint
// crossing a z16 tile edge inside a neighbour's 0.004° pad is merged whole in
// the cell holding both tiles and emitted as a clipped fragment by the cell
// holding only one; the fragment is Complete:false and shares a member id, so
// the whole copy wins.
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
	fps, lus := newPieceDedup(), newPieceDedup()
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
			if f.Geometry != nil && inBox(f.Geometry.Geometry().Bound()) {
				fps.add(f)
			}
		}
		for _, l := range r.Landuse {
			if l.Geometry != nil && inBox(l.Geometry.Geometry().Bound()) {
				lus.add(l)
			}
		}
	}
	out.Footprints, out.Landuse = fps.result(), lus.result()
	return out, nil
}

// pieceDedup merges pieces from several cells by tile-scoped member id. A
// piece that shares any member id with an already-kept piece is the same
// feature (or a fragment of it): the complete copy wins, then the larger area,
// then the earlier one. Keeping a better copy evicts every kept piece it
// overlaps by id (a whole building may replace two fragments from two cells).
type pieceDedup struct {
	kept    []Piece
	dropped []bool
	byKey   map[string]int // member id → index into kept
}

func newPieceDedup() *pieceDedup { return &pieceDedup{byKey: map[string]int{}} }

func pieceBetter(p, q *Piece) bool {
	if p.Complete != q.Complete {
		return p.Complete
	}
	return p.AreaSqm > q.AreaSqm
}

func (d *pieceDedup) add(p Piece) {
	keys := p.Keys()
	var hits []int
	for _, k := range keys {
		if i, ok := d.byKey[k]; ok && !d.dropped[i] {
			hits = append(hits, i)
		}
	}
	for _, i := range hits {
		if !pieceBetter(&p, &d.kept[i]) {
			return // an existing copy is at least as good
		}
	}
	for _, i := range hits {
		d.dropped[i] = true
	}
	d.kept = append(d.kept, p)
	d.dropped = append(d.dropped, false)
	for _, k := range keys {
		d.byKey[k] = len(d.kept) - 1
	}
}

func (d *pieceDedup) result() []Piece {
	if len(d.kept) == 0 {
		return nil
	}
	out := d.kept[:0]
	for i := range d.kept {
		if !d.dropped[i] {
			out = append(out, d.kept[i])
		}
	}
	return out
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
