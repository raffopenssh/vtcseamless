package bevdirect

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/encoding/mvt"
	"github.com/paulmach/orb/geojson"
	"github.com/paulmach/orb/maptile"
	"github.com/raffopenssh/vtcseamless"
)

// fixtureTiles is a synthetic BEV: a village of square parcels (gst, kg/gnr)
// with footprints (nfl ns=41) and land use (nfl ns≠41) on a 0.0004° grid
// over bbox. Every tile carries every feature whose bbox intersects the tile
// grown by a buffer, unclipped — the overlap the assembler must clip away,
// as BEV does. Returns raw pbf bytes per tile.
func fixtureTiles(bbox orb.Bound, seed int64) map[Tile][]byte {
	rnd := rand.New(rand.NewSource(seed))
	const step = 0.0004
	type feat struct {
		layer string
		props geojson.Properties
		poly  orb.Polygon
	}
	var feats []feat
	n := 0
	for x := bbox.Min[0]; x < bbox.Max[0]; x += step {
		for y := bbox.Min[1]; y < bbox.Max[1]; y += step {
			n++
			dx, dy := step*(0.6+0.8*rnd.Float64()), step*(0.6+0.8*rnd.Float64())
			sq := orb.Polygon{{{x, y}, {x + dx, y}, {x + dx, y + dy}, {x, y + dy}, {x, y}}}
			kg := 63300 + n%3
			feats = append(feats, feat{"gst", geojson.Properties{"kg": float64(kg), "gnr": strconv.Itoa(n), "ez": float64(n / 4), "rstatus": "G"}, sq})
			ns := []float64{48, 52, 56, 95, 63, 83}[rnd.Intn(6)]
			feats = append(feats, feat{"nfl", geojson.Properties{"ns": ns}, sq})
			if n%3 == 0 {
				fx, fy := x+dx*0.2, y+dy*0.2
				feats = append(feats, feat{"nfl", geojson.Properties{"ns": 41.0}, orb.Polygon{{{fx, fy}, {fx + dx*0.5, fy}, {fx + dx*0.5, fy + dy*0.5}, {fx, fy + dy*0.5}, {fx, fy}}}})
			}
			// a second land use strip inside the parcel so dominant_ns can tie
			if n%5 == 0 {
				feats = append(feats, feat{"nfl", geojson.Properties{"ns": 52.0}, orb.Polygon{{{x, y}, {x + dx/2, y}, {x + dx/2, y + dy}, {x, y + dy}, {x, y}}}})
			}
		}
	}
	out := map[Tile][]byte{}
	for _, z := range []int{ParcelZoom, SymboleZoom} {
		for _, t := range vtcseamless.TilesForBound(bbox.Pad(0.01), z) {
			tb := t.Bound()
			buf := (tb.Max[0] - tb.Min[0]) * 0.05
			grown := tb.Pad(buf)
			layers := mvt.Layers{}
			byName := map[string]*mvt.Layer{}
			for _, f := range feats {
				if (z == ParcelZoom) != (f.layer == "gst") {
					continue
				}
				if !f.poly.Bound().Intersects(grown) {
					continue
				}
				l, ok := byName[f.layer]
				if !ok {
					l = mvt.NewLayer(f.layer, geojson.NewFeatureCollection())
					l.Version, l.Extent = 2, 4096
					byName[f.layer] = l
					layers = append(layers, l)
				}
				gf := geojson.NewFeature(f.poly.Clone())
				gf.Properties = f.props
				l.Features = append(l.Features, gf)
			}
			layers.ProjectToTile(maptile.New(uint32(t.X), uint32(t.Y), maptile.Zoom(t.Z)))
			b, err := mvt.Marshal(layers)
			if err != nil {
				panic(err)
			}
			out[t] = b
		}
	}
	return out
}

// tileServer serves fixture tiles; status overrides a tile's answer.
func tileServer(t *testing.T, tiles map[Tile][]byte, status map[Tile]int) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), ".pbf"), "/")
		z, _ := strconv.Atoi(parts[0])
		x, _ := strconv.Atoi(parts[1])
		y, _ := strconv.Atoi(parts[2])
		mu.Lock()
		defer mu.Unlock()
		tile := Tile{Z: z, X: x, Y: y}
		if st, ok := status[tile]; ok {
			w.WriteHeader(st)
			return
		}
		b, ok := tiles[tile]
		if !ok {
			w.WriteHeader(404)
			return
		}
		w.Write(b)
	}))
	old := TileURL
	TileURL = srv.URL + "/%d/%d/%d.pbf"
	t.Cleanup(func() { TileURL = old; srv.Close() })
	return srv
}

func layersJSON(t *testing.T, r *Result) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"parcels": r.Parcels, "footprints": r.Footprints, "landuse": r.Landuse, "tiles": r.Tiles})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestAssemblyDeterministic — the same bbox built twice from identical tile
// bytes is byte-identical GeoJSON (parcels, footprints, landuse, enrichment,
// tiles). This is what makes "same tile hashes, different downstream digest"
// a pipeline signal: without it the comparison means nothing.
func TestAssemblyDeterministic(t *testing.T) {
	bbox := orb.Bound{Min: orb.Point{15.0, 47.0}, Max: orb.Point{15.02, 47.02}}
	tiles := fixtureTiles(bbox, 7)
	tileServer(t, tiles, nil)
	build := func(cache *TileCache) *Result {
		res, err := Fetch(t.Context(), bbox.Min[0]-cellPad, bbox.Min[1]-cellPad, bbox.Max[0]+cellPad, bbox.Max[1]+cellPad,
			Options{Layers: AllLayers, Enrich: true, TileCache: cache, MaxTiles: cellTiles, Workers: 8})
		if err != nil {
			t.Fatal(err)
		}
		if !res.Ready || res.Stats.TileErrors != 0 {
			t.Fatalf("not ready: %+v", res.Stats)
		}
		return res
	}
	first := build(nil)
	if len(first.Parcels) < 100 || len(first.Footprints) < 30 || len(first.Landuse) < 100 {
		t.Fatalf("fixture too small: %d parcels %d fp %d lu", len(first.Parcels), len(first.Footprints), len(first.Landuse))
	}
	ref := layersJSON(t, first)
	cache := NewTileCache(1<<26, 0)
	for i := 0; i < 4; i++ {
		var r *Result
		if i%2 == 0 {
			r = build(nil) // fresh download each time
		} else {
			r = build(cache) // via the tile cache (second pass = cache hits)
		}
		if got := layersJSON(t, r); string(got) != string(ref) {
			t.Fatalf("run %d differs from run 0 (len %d vs %d)", i+1, len(got), len(ref))
		}
	}
	// same through the Service cell path (what /viewport serves)
	var cellRef []byte
	for i := 0; i < 2; i++ {
		svc := NewService(ServiceOptions{MaxCells: 4, Prefetch: 0})
		res, err := svc.Viewport(t.Context(), bbox.Min[0], bbox.Min[1], bbox.Max[0], bbox.Max[1])
		if err != nil || !res.Ready {
			t.Fatal(err, res)
		}
		b := layersJSON(t, res)
		if i == 0 {
			cellRef = b
		} else if string(b) != string(cellRef) {
			t.Fatal("Service.Viewport differs between two fresh services on identical tile bytes")
		}
	}
	// provenance: every expected tile hashed, sorted (z, x, y), hash = sha256 of the served bytes
	want := len(vtcseamless.TilesForBound(bbox.Pad(cellPad), ParcelZoom)) + len(vtcseamless.TilesForBound(bbox.Pad(cellPad), SymboleZoom))
	if first.Stats.TilesExpected != want || first.Stats.TilesFetched != want || len(first.Tiles) != want {
		t.Fatalf("tiles expected=%d fetched=%d listed=%d want %d", first.Stats.TilesExpected, first.Stats.TilesFetched, len(first.Tiles), want)
	}
	for i, tr := range first.Tiles {
		if i > 0 && !tileRefLess(first.Tiles[i-1], tr) {
			t.Fatalf("tiles not sorted at %d: %+v %+v", i, first.Tiles[i-1], tr)
		}
		h := sha256.Sum256(tiles[Tile{Z: tr.Z, X: tr.X, Y: tr.Y}])
		if tr.SHA256 != hex.EncodeToString(h[:]) {
			t.Fatalf("tile %d/%d/%d hash mismatch", tr.Z, tr.X, tr.Y)
		}
	}
}

// TestTileProvenanceIncomplete — a failing tile is visible per cell
// (tiles_fetched < tiles_expected, ready:false, not in tiles[]); an empty
// (404) tile counts as fetched with the hash of zero bytes.
func TestTileProvenanceIncomplete(t *testing.T) {
	bbox := orb.Bound{Min: orb.Point{15.0, 47.0}, Max: orb.Point{15.02, 47.02}}
	tiles := fixtureTiles(bbox, 3)
	z16 := vtcseamless.TilesForBound(bbox.Pad(cellPad), SymboleZoom)
	bad, empty := z16[0], z16[1]
	tileServer(t, tiles, map[Tile]int{bad: 500, empty: 404})
	res, err := Fetch(t.Context(), bbox.Min[0]-cellPad, bbox.Min[1]-cellPad, bbox.Max[0]+cellPad, bbox.Max[1]+cellPad,
		Options{Layers: AllLayers, MaxTiles: cellTiles, Workers: 4, Timeout: 0})
	if err != nil {
		t.Fatal(err)
	}
	st := res.Stats
	if res.Ready || st.TileErrors != 1 || st.TilesFetched != st.TilesExpected-1 || st.TilesEmpty < 1 || len(res.Tiles) != st.TilesFetched {
		t.Fatalf("stats %+v ready=%v tiles=%d", st, res.Ready, len(res.Tiles))
	}
	seenEmpty := false
	for _, tr := range res.Tiles {
		if tr.Z == bad.Z && tr.X == bad.X && tr.Y == bad.Y {
			t.Fatal("failed tile listed in tiles[]")
		}
		if tr.Z == empty.Z && tr.X == empty.X && tr.Y == empty.Y {
			seenEmpty = true
			if tr.SHA256 != emptySHA256 {
				t.Fatalf("empty tile hash %s", tr.SHA256)
			}
		}
	}
	if !seenEmpty {
		t.Fatal("404 tile missing from tiles[] — it is a known (empty) input")
	}
	// the HTTP document carries tiles[] and the counts
	svc := NewService(ServiceOptions{MaxCells: 4, Prefetch: 0, Wait: 0})
	svc.cells.Put(cell{750, 2350}.key(), res)
	rec := httptest.NewRecorder()
	svc.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/viewport?west=15.0&south=47.0&east=15.02&north=47.02&wait=0", nil))
	var body struct {
		Tiles []TileRef `json:"tiles"`
		Stats Stats     `json:"stats"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Tiles) != len(res.Tiles) || body.Stats.TilesExpected != st.TilesExpected || body.Stats.TilesFetched != st.TilesFetched {
		t.Fatalf("document: %d tiles, stats %+v", len(body.Tiles), body.Stats)
	}
}

// TestAssemblyDeterministicLive repeats the check on real BEV tiles for one
// cell (same bytes via the tile cache). Needs network: BEVDIRECT_LIVE=1.
func TestAssemblyDeterministicLive(t *testing.T) {
	if os.Getenv("BEVDIRECT_LIVE") == "" {
		t.Skip("set BEVDIRECT_LIVE=1 to fetch BEV tiles")
	}
	cache := NewTileCache(1<<28, 0)
	var ref []byte
	for _, c := range []cell{{807, 2385}, {750, 2385}} {
		w, s, e, n := c.bbox()
		for i := 0; i < 3; i++ {
			res, err := Fetch(t.Context(), w-cellPad, s-cellPad, e+cellPad, n+cellPad,
				Options{Layers: AllLayers, Enrich: true, TileCache: cache, MaxTiles: cellTiles, Workers: 8})
			if err != nil || !res.Ready {
				t.Fatal(err, res.Stats)
			}
			b := layersJSON(t, res)
			if i == 0 {
				ref = b
				t.Logf("cell %d/%d: %d parcels %d fp %d lu, %d tiles (%d empty), guard trips %d", c.ix, c.iy, len(res.Parcels), len(res.Footprints), len(res.Landuse), len(res.Tiles), res.Stats.TilesEmpty, res.Stats.GuardTrips)
			} else if string(b) != string(ref) {
				t.Fatalf("cell %d/%d run %d differs", c.ix, c.iy, i)
			}
		}
	}
}
