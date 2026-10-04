package bevdirect

import (
	"os"
	"path/filepath"
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

func TestSweepTileCache(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "15", "1")
	os.MkdirAll(old, 0o755)
	os.MkdirAll(filepath.Join(dir, "16", "2"), 0o755)
	os.WriteFile(filepath.Join(old, "7.pbf"), []byte("stale"), 0o644)
	os.WriteFile(filepath.Join(dir, "16", "2", "9.pbf"), []byte("fresh"), 0o644)
	os.Chtimes(filepath.Join(old, "7.pbf"), time.Now().Add(-48*time.Hour), time.Now().Add(-48*time.Hour))
	n, b := SweepTileCache(dir, 24*time.Hour)
	if n != 1 || b != 5 {
		t.Fatalf("sweep: %d files %d bytes", n, b)
	}
	if _, err := os.Stat(filepath.Join(dir, "15")); !os.IsNotExist(err) {
		t.Fatalf("empty z dir should be pruned")
	}
	if _, err := os.Stat(filepath.Join(dir, "16", "2", "9.pbf")); err != nil {
		t.Fatalf("fresh tile removed")
	}
	if n, _ := SweepTileCache(dir, 0); n != 0 {
		t.Fatalf("ttl 0 must keep everything")
	}
}
