// Package bevdirect assembles cadastral parcels, building footprints and
// land-use polygons for a bbox DIRECTLY from the BEV vector tiles
// (Katastralmappe VTC, CC BY 4.0). It is the BEV preset of the vtcseamless
// tile-stitching engine: tile download + cache, MVT decoding, the gst/nfl
// layer conventions, enrichment (land-use split, footprint link) and a small
// cell-cached HTTP service. Expect seconds per viewport instead of
// milliseconds — it fetches on the fly.
//
// Licence: BEV Nutzungsbedingungen want „© BEV, JJJJ“ on every copy. Result
// carries Notice; render it. Do NOT point this at the Katasterservice
// search/info JSON API — that one is not CC BY.
//
// The hard part is not the download. BEV clips every polygon to its tile WITH
// A BUFFER, so per-tile copies of one parcel overlap. Every piece is clipped
// back to its own tile rectangle before the union (tiles tesselate ⇒ pieces are
// disjoint ⇒ areas add and polyclip cannot double-count). See the root
// package vtcseamless for the rule and its guard.
package bevdirect

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/encoding/mvt"
	"github.com/paulmach/orb/geojson"
	"github.com/paulmach/orb/maptile"
	"github.com/paulmach/orb/planar"
	"github.com/raffopenssh/vtcseamless"
)

// Tile is the slippy-map tile coordinate of the engine (re-exported for callers).
type Tile = vtcseamless.Tile

const (
	TileURL     = "https://kataster.bev.gv.at/at.gv.bev.kataster/tiles/kataster/%d/%d/%d.pbf"
	ParcelZoom  = 15 // gst layer
	SymboleZoom = 16 // nfl layer (footprints ns=41, landuse); BEV max zoom
	LicenseURL  = "https://creativecommons.org/licenses/by/4.0/"
)

// Notice is the attribution string to show with any output.
func Notice() string {
	return fmt.Sprintf("© BEV, %d – Datenquelle: Bundesamt für Eich- und Vermessungswesen, Kataster (CC BY 4.0), bearbeitet", time.Now().Year())
}

// Options controls a Fetch.
type Options struct {
	Layers     []string      // subset of parcels, footprints, landuse; default parcels
	Workers    int           // parallel tile downloads (default 6; be polite)
	Timeout    time.Duration // per-tile HTTP timeout (default 20 s)
	TileCache  *TileCache    // optional in-memory tile cache (NewTileCache); tiles are never written to disk
	MaxTiles   int           // refuse bboxes needing more tiles (default 64 at z15, i.e. ~30 km²)
	Decimals   int           // coordinate rounding (default 7)
	Enrich     bool          // landuse_areas / building_count / footprint shapes + parcel link
	UserAgent  string
	HTTPClient *http.Client
	Log        func(format string, args ...any)

	assemblyGate chan struct{} // set by Service: bounds concurrent CPU-heavy assemblies
}

func (o *Options) defaults() {
	if len(o.Layers) == 0 {
		o.Layers = []string{"parcels"}
	}
	if o.Workers <= 0 {
		o.Workers = 6
	}
	if o.Timeout <= 0 {
		o.Timeout = 20 * time.Second
	}
	if o.MaxTiles <= 0 {
		o.MaxTiles = 64
	}
	if o.Decimals <= 0 {
		o.Decimals = 7
	}
	if o.UserAgent == "" {
		o.UserAgent = "vtcseamless/" + Version + " (+https://github.com/raffopenssh/vtcseamless)"
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: o.Timeout}
	}
	if o.Log == nil {
		o.Log = func(string, ...any) {}
	}
}

// Parcel is one assembled cadastral parcel (one row of the viewport document).
type Parcel struct {
	ParcelID string            `json:"parcel_id"`
	KGCode   string            `json:"kg_code"`
	GNR      string            `json:"gnr"`
	EZ       string            `json:"ez,omitempty"`
	Status   string            `json:"rstatus,omitempty"`
	AreaSqm  float64           `json:"area_sqm"`
	Lon      float64           `json:"lon"`
	Lat      float64           `json:"lat"`
	Complete bool              `json:"complete"` // false: touches the edge of the fetched tile set → geometry/area truncated
	Parts    int               `json:"parts"`
	Geometry *geojson.Geometry `json:"geometry"`
	*Enrichment
}

// Piece is a tile-clipped nfl polygon (footprint or land use). Footprints have
// no stable id in the tiles; pieces are emitted as-is (seams only where a
// building straddles a z16 tile edge, ~400 m grid).
type Piece struct {
	ID       string            `json:"id"`
	NS       int               `json:"ns"`
	Tile     string            `json:"tile"`
	AreaSqm  float64           `json:"area_sqm"`
	Geometry *geojson.Geometry `json:"geometry"`
	*Shape
}

// Result is the assembled viewport.
type Result struct {
	Parcels    []Parcel  `json:"parcels,omitempty"`
	Footprints []Piece   `json:"footprints,omitempty"`
	Landuse    []Piece   `json:"landuse,omitempty"`
	Ready      bool      `json:"ready"`
	Source     string    `json:"source"`
	Notice     string    `json:"notice"`
	LicenseURL string    `json:"license_url"`
	FetchedAt  time.Time `json:"fetched_at"`
	Stats      Stats     `json:"stats"`
}

type Stats struct {
	TilesZ15   int           `json:"tiles_z15"`
	TilesZ16   int           `json:"tiles_z16"`
	TileErrors int           `json:"tile_errors"`
	CacheHits  int           `json:"cache_hits"`
	Bytes      int64         `json:"bytes"`
	Download   time.Duration `json:"download_ns"`
	Assembly   time.Duration `json:"assembly_ns"`
	GuardTrips int           `json:"union_guard_trips"`
	Pieces     int           `json:"parcel_pieces"`
}

// Version is the release tag of this bevdirect build ("v0.3.0"); bevdirect-serve
// sets it from -ldflags "-X main.version=…" and every /viewport document carries
// it as `bevdirect_version`. Downstream digest comparisons pin their source as
// "bevdirect@<Version>": two observers only compare digests when their
// documents name the same tag, because the assembly (seams, union guard) is part
// of the result. "dev" = unpinned build. The source string is a stable
// contract — do not rename it.
var Version = "dev"

// ErrTooManyTiles is returned when the bbox exceeds Options.MaxTiles.
var ErrTooManyTiles = errors.New("bevdirect: bbox needs more tiles than Options.MaxTiles")

// Fetch assembles the requested layers for bbox (west, south, east, north).
func Fetch(ctx context.Context, west, south, east, north float64, opts Options) (*Result, error) {
	opts.defaults()
	bbox := orb.Bound{Min: orb.Point{west, south}, Max: orb.Point{east, north}}
	if west >= east || south >= north {
		return nil, errors.New("bevdirect: empty bbox")
	}
	want := func(l string) bool {
		for _, x := range opts.Layers {
			if x == l {
				return true
			}
		}
		return false
	}
	res := &Result{Source: "bev-direct", Notice: Notice(), LicenseURL: LicenseURL, FetchedAt: time.Now().UTC()}

	var z15, z16 []Tile
	if want("parcels") {
		z15 = vtcseamless.TilesForBound(bbox, ParcelZoom)
	}
	if want("footprints") || want("landuse") {
		z16 = vtcseamless.TilesForBound(bbox, SymboleZoom)
	}
	if len(z15) > opts.MaxTiles || len(z16) > 4*opts.MaxTiles {
		return nil, fmt.Errorf("%w (z15=%d z16=%d max=%d)", ErrTooManyTiles, len(z15), len(z16), opts.MaxTiles)
	}
	res.Stats.TilesZ15, res.Stats.TilesZ16 = len(z15), len(z16)

	t0 := time.Now()
	all := append(append([]Tile{}, z15...), z16...)
	data, errs := downloadAll(ctx, all, &opts, &res.Stats)
	res.Stats.Download = time.Since(t0)
	res.Stats.TileErrors = errs
	if errs == len(all) && len(all) > 0 {
		return nil, errors.New("bevdirect: every tile download failed")
	}
	res.Ready = errs == 0

	if opts.assemblyGate != nil { // downloads overlap freely; assembly is serialised per core so the nearest cell finishes first
		select {
		case opts.assemblyGate <- struct{}{}:
			defer func() { <-opts.assemblyGate }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	t1 := time.Now()
	if want("parcels") {
		fetched := vtcseamless.TileSetBound(z15)
		res.Parcels = assembleParcels(z15, data, fetched, &opts, &res.Stats)
	}
	if want("footprints") || want("landuse") {
		fp, lu := assembleNFL(z16, data, want("footprints"), want("landuse"), &opts)
		res.Footprints, res.Landuse = fp, lu
	}
	if opts.Enrich {
		Enrich(res)
	}
	res.Stats.Assembly = time.Since(t1)
	return res, nil
}

// ── download ────────────────────────────────────────────────────────────

func downloadAll(ctx context.Context, tiles []Tile, opts *Options, st *Stats) (map[Tile][]byte, int) {
	out := make(map[Tile][]byte, len(tiles))
	var mu sync.Mutex
	var errs int
	var bytes int64
	var hits int
	sem := make(chan struct{}, opts.Workers)
	var wg sync.WaitGroup
	for _, t := range tiles {
		t := t
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			b, hit, err := downloadTile(ctx, t, opts)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs++
				opts.Log("tile %d/%d/%d: %v", t.Z, t.X, t.Y, err)
				return
			}
			if hit {
				hits++
			}
			bytes += int64(len(b))
			out[t] = b
		}()
	}
	wg.Wait()
	st.Bytes, st.CacheHits = bytes, hits
	return out, errs
}

// tileFlight coalesces concurrent downloads of the same tile process-wide:
// neighbouring padded cells assembled at the same time share each BEV request.
var tileFlight singleflight.Group

func downloadTile(ctx context.Context, t Tile, opts *Options) ([]byte, bool, error) {
	type dl struct {
		b   []byte
		hit bool
	}
	v, err, shared := tileFlight.Do(fmt.Sprintf("%d/%d/%d", t.Z, t.X, t.Y), func() (any, error) {
		b, hit, err := downloadTileRaw(ctx, t, opts)
		return dl{b, hit}, err
	})
	if err != nil {
		return nil, false, err
	}
	d := v.(dl)
	return d.b, d.hit || shared, nil
}

func downloadTileRaw(ctx context.Context, t Tile, opts *Options) ([]byte, bool, error) {
	cacheKey := fmt.Sprintf("%d/%d/%d", t.Z, t.X, t.Y)
	if b, ok := opts.TileCache.get(cacheKey); ok {
		return b, true, nil
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, false, ctx.Err()
			case <-time.After(time.Duration(attempt) * 700 * time.Millisecond):
			}
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf(TileURL, t.Z, t.X, t.Y), nil)
		req.Header.Set("User-Agent", opts.UserAgent)
		resp, err := opts.HTTPClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		b, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		switch {
		case resp.StatusCode == 404 || resp.StatusCode == 204:
			b = []byte{} // empty tile (outside Austria / nothing mapped)
		case resp.StatusCode != 200:
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
			continue
		case err != nil:
			lastErr = err
			continue
		}
		opts.TileCache.put(cacheKey, b)
		return b, false, nil
	}
	return nil, false, lastErr
}

// ── parcels (gst @ z15) ─────────────────────────────────────────────────

type parcelGroup struct {
	kg, gnr, ez, status string
	pieces              []orb.Polygon
	targetArea          float64 // deg², authoritative (pieces are disjoint)
	complete            bool
}

func assembleParcels(tiles []Tile, data map[Tile][]byte, fetched orb.Bound, opts *Options, st *Stats) []Parcel {
	groups := map[string]*parcelGroup{}
	var order []string
	const edgeEps = 1e-7
	for _, t := range tiles {
		raw, ok := data[t]
		if !ok || len(raw) == 0 {
			continue
		}
		layers, err := mvt.Unmarshal(raw)
		if err != nil {
			opts.Log("tile %v: mvt: %v", t, err)
			continue
		}
		tb := t.Bound()
		for _, layer := range layers {
			if layer.Name != "gst" {
				continue
			}
			layer.ProjectToWGS84(maptile.New(uint32(t.X), uint32(t.Y), maptile.Zoom(t.Z)))
			for _, f := range layer.Features {
				kg := propString(f.Properties, "kg")
				gnr := propString(f.Properties, "gnr")
				if kg == "" || gnr == "" {
					continue
				}
				if n, err := strconv.Atoi(kg); err == nil {
					kg = fmt.Sprintf("%05d", n)
				}
				key := kg + "-" + gnr
				g, ok := groups[key]
				if !ok {
					g = &parcelGroup{kg: kg, gnr: gnr, ez: propString(f.Properties, "ez"), status: propString(f.Properties, "rstatus"), complete: true}
					groups[key] = g
					order = append(order, key)
				}
				for _, poly := range vtcseamless.ToPolygons(f.Geometry) {
					for _, piece := range vtcseamless.ClipToBound(poly, tb) {
						if len(piece) == 0 || len(piece[0]) < 4 {
							continue
						}
						pb := piece.Bound()
						if pb.Min[0] <= fetched.Min[0]+edgeEps || pb.Max[0] >= fetched.Max[0]-edgeEps ||
							pb.Min[1] <= fetched.Min[1]+edgeEps || pb.Max[1] >= fetched.Max[1]-edgeEps {
							g.complete = false
						}
						g.pieces = append(g.pieces, piece)
						g.targetArea += math.Abs(planar.Area(piece))
						st.Pieces++
					}
				}
			}
		}
	}

	out := make([]Parcel, 0, len(order))
	for _, key := range order {
		g := groups[key]
		if len(g.pieces) == 0 {
			continue
		}
		mp, tripped := vtcseamless.UnionPieces(g.pieces, g.targetArea)
		if tripped {
			st.GuardTrips++
		}
		if len(mp) == 0 {
			continue
		}
		vtcseamless.RoundCoords(mp, opts.Decimals)
		var geom orb.Geometry = mp
		if len(mp) == 1 {
			geom = mp[0]
		}
		c := centroidOf(mp)
		out = append(out, Parcel{
			ParcelID: key, KGCode: g.kg, GNR: g.gnr, EZ: g.ez, Status: g.status,
			AreaSqm: math.Round(vtcseamless.AreaSqm(mp)*10) / 10,
			Lon:     math.Round(c[0]*1e7) / 1e7, Lat: math.Round(c[1]*1e7) / 1e7,
			Complete: g.complete, Parts: len(mp), Geometry: geojson.NewGeometry(geom),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ParcelID < out[j].ParcelID })
	return out
}

// ── nfl (footprints ns=41, landuse) @ z16 ───────────────────────────────

func assembleNFL(tiles []Tile, data map[Tile][]byte, wantFP, wantLU bool, opts *Options) (fps, lus []Piece) {
	for _, t := range tiles {
		raw, ok := data[t]
		if !ok || len(raw) == 0 {
			continue
		}
		layers, err := mvt.Unmarshal(raw)
		if err != nil {
			continue
		}
		tb := t.Bound()
		tileKey := fmt.Sprintf("%d/%d/%d", t.Z, t.X, t.Y)
		for _, layer := range layers {
			if layer.Name != "nfl" {
				continue
			}
			layer.ProjectToWGS84(maptile.New(uint32(t.X), uint32(t.Y), maptile.Zoom(t.Z)))
			for i, f := range layer.Features {
				ns := int(propFloat(f.Properties, "ns"))
				isFP := ns == 41
				if (isFP && !wantFP) || (!isFP && !wantLU) {
					continue
				}
				var mp orb.MultiPolygon
				for _, poly := range vtcseamless.ToPolygons(f.Geometry) {
					mp = append(mp, vtcseamless.ClipToBound(poly, tb)...)
				}
				if len(mp) == 0 {
					continue
				}
				vtcseamless.RoundCoords(mp, opts.Decimals)
				var geom orb.Geometry = mp
				if len(mp) == 1 {
					geom = mp[0]
				}
				p := Piece{ID: fmt.Sprintf("%s#%d", strings.ReplaceAll(tileKey, "/", "_"), i), NS: ns, Tile: tileKey,
					AreaSqm: math.Round(vtcseamless.AreaSqm(mp)*10) / 10, Geometry: geojson.NewGeometry(geom)}
				if isFP {
					fps = append(fps, p)
				} else {
					lus = append(lus, p)
				}
			}
		}
	}
	if wantFP && len(fps) > 1 {
		tileOf := make(map[string]Tile, len(tiles))
		for _, t := range tiles {
			tileOf[fmt.Sprintf("%d/%d/%d", t.Z, t.X, t.Y)] = t
		}
		fps = mergeFootprintSeams(fps, tileOf, opts.Decimals)
	}
	return fps, lus
}

// ── helpers ─────────────────────────────────────────────────────────────

func centroidOf(mp orb.MultiPolygon) orb.Point {
	best, bestA := orb.Polygon(nil), -1.0
	for _, p := range mp {
		if a := math.Abs(planar.Area(p)); a > bestA {
			best, bestA = p, a
		}
	}
	if best == nil {
		return mp.Bound().Center()
	}
	c, _ := planar.CentroidArea(best)
	return c
}

func propString(p geojson.Properties, k string) string {
	switch v := p[k].(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		if v == math.Trunc(v) {
			return strconv.FormatInt(int64(v), 10)
		}
		return strconv.FormatFloat(v, 'f', -1, 64)
	case int64:
		return strconv.FormatInt(v, 10)
	case int:
		return strconv.Itoa(v)
	case uint64:
		return strconv.FormatUint(v, 10)
	}
	return ""
}

func propFloat(p geojson.Properties, k string) float64 {
	switch v := p[k].(type) {
	case float64:
		return v
	case int64:
		return float64(v)
	case uint64:
		return float64(v)
	case int:
		return float64(v)
	case string:
		f, _ := strconv.ParseFloat(v, 64)
		return f
	}
	return 0
}
