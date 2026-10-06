package bevdirect

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/geojson"
)

func TestCellsFor(t *testing.T) {
	// a client tile of 0.02° starting off-grid touches 2×2 cells
	cs := cellsFor(15.075, 47.055, 15.095, 47.075)
	if len(cs) != 4 {
		t.Fatalf("want 4 cells, got %d: %v", len(cs), cs)
	}
	// exactly grid-aligned → 1 cell
	if cs := cellsFor(15.08, 47.06, 15.10, 47.08); len(cs) != 1 {
		t.Fatalf("aligned tile should be 1 cell, got %v", cs)
	}
	w, s, e, n := cellOf(15.0812, 47.0611).bbox()
	if w > 15.0812 || e < 15.0812 || s > 47.0611 || n < 47.0611 {
		t.Fatalf("cell bbox %v %v %v %v does not contain point", w, s, e, n)
	}
	if checkBBox(15, 47, 15.05, 47.01) == nil {
		t.Fatal("oversized bbox accepted")
	}
}

func TestComposeDedup(t *testing.T) {
	svc := NewService(ServiceOptions{MaxCells: 4})
	sq := func(x, y, d float64) *geojson.Geometry {
		return geojson.NewGeometry(orb.Polygon{{{x, y}, {x + d, y}, {x + d, y + d}, {x, y + d}, {x, y}}})
	}
	// parcel A straddles the cell edge at lon 15.04: incomplete copy in cell 751, complete copy in cell 752
	a0 := Parcel{ParcelID: "1-1", Complete: false, AreaSqm: 10, Geometry: sq(15.039, 47.001, 0.002)}
	a1 := Parcel{ParcelID: "1-1", Complete: true, AreaSqm: 20, Geometry: sq(15.039, 47.001, 0.002)}
	b := Parcel{ParcelID: "1-2", Complete: true, AreaSqm: 5, Geometry: sq(15.0295, 47.0095, 0.001)}
	fp := Piece{ID: "16/1/1#0", Geometry: sq(15.0395, 47.0015, 0.0005)}
	svc.cells.Put(cell{751, 2350}.key(), &Result{Ready: true, Parcels: []Parcel{a0, b}, Footprints: []Piece{fp}})
	svc.cells.Put(cell{752, 2350}.key(), &Result{Ready: true, Parcels: []Parcel{a1}, Footprints: []Piece{fp}})
	res, err := svc.Viewport(t.Context(), 15.02, 47.0, 15.06, 47.02)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Parcels) != 2 || len(res.Footprints) != 1 {
		t.Fatalf("dedup failed: %d parcels %d footprints", len(res.Parcels), len(res.Footprints))
	}
	for _, p := range res.Parcels {
		if p.ParcelID == "1-1" && (!p.Complete || p.AreaSqm != 20) {
			t.Fatalf("complete copy should win: %+v", p)
		}
	}
	// trimming: a bbox that excludes parcel B
	res, _ = svc.Viewport(t.Context(), 15.035, 47.0, 15.06, 47.02)
	if len(res.Parcels) != 1 || res.Parcels[0].ParcelID != "1-1" {
		t.Fatalf("bbox trim failed: %+v", res.Parcels)
	}
	// nothing per-parcel is retrievable: only a location resolves an id
	if p, _ := svc.ParcelAt(t.Context(), "1-2", 15.03, 47.01); p == nil {
		t.Fatal("parcel at its own location not found")
	}
	if p, _ := svc.ParcelAt(t.Context(), "1-2", 15.05, 47.01); p != nil {
		t.Fatal("parcel found away from its location — a per-parcel index must not exist")
	}
}

func TestTileCacheInMemoryOnly(t *testing.T) {
	c := NewTileCache(100, time.Hour)
	c.put("15/1/7", make([]byte, 60))
	c.put("16/2/9", make([]byte, 30))
	if _, ok := c.get("15/1/7"); !ok {
		t.Fatal("miss on cached tile")
	}
	c.put("16/2/10", make([]byte, 30)) // 120 > 100 → evicts LRU (16/2/9)
	if _, ok := c.get("16/2/9"); ok {
		t.Fatal("LRU eviction did not fire")
	}
	if st := c.Stats(); st.Tiles != 2 || st.Bytes != 90 || st.MaxBytes != 100 {
		t.Fatalf("stats %+v", st)
	}
	c.put("15/9/9", make([]byte, 101)) // larger than the whole budget: never stored
	if st := c.Stats(); st.Tiles != 2 {
		t.Fatalf("oversized tile stored: %+v", st)
	}
	// expiry
	c.mu.Lock()
	for _, el := range c.items {
		el.Value.(*tileEntry).at = time.Now().Add(-2 * time.Hour)
	}
	c.mu.Unlock()
	if n, b := c.Sweep(); n != 2 || b != 90 {
		t.Fatalf("sweep %d %d", n, b)
	}
	var nilc *TileCache
	if _, ok := nilc.get("x"); ok {
		t.Fatal("nil cache hit")
	}
	nilc.put("x", []byte{1}) // must not panic
	if NewTileCache(0, time.Hour) != nil {
		t.Fatal("budget 0 must mean no cache")
	}
}

// Nothing in this package may persist tiles: the only on-disk artefact a
// bevdirect process leaves behind is none.
func TestNoTileFilesOnDisk(t *testing.T) {
	for _, f := range []string{"bevdirect.go", "service.go", "service_http.go", "tile_cache.go", "store.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{"os.WriteFile", "os.Create(", "os.MkdirAll", ".pbf\")"} {
			if strings.Contains(string(src), bad) {
				t.Fatalf("%s contains %q — tiles must stay in memory", f, bad)
			}
		}
	}
}
