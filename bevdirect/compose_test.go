package bevdirect

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/geojson"
)

func sqGeom(x, y, dx, dy float64) *geojson.Geometry {
	return geojson.NewGeometry(orb.Polygon{{{x, y}, {x + dx, y}, {x + dx, y + dy}, {x, y + dy}, {x, y}}})
}

// geomSet is the content view ne_cells uses: footprints by geometry, landuse by (ns, geometry).
func geomSet(ps []Piece) map[string]int {
	m := map[string]int{}
	for _, p := range ps {
		m[fmt.Sprint(p.NS, p.Geometry.Geometry())]++
	}
	return m
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// assertViewportEqualsCellUnion checks the compose invariant: a viewport over
// N×M cells equals the union of the N×M single-cell documents after dedupe,
// compared by content (geometry), and contains no duplicate geometries.
func assertViewportEqualsCellUnion(t *testing.T, svc *Service, w, s, e, n float64) {
	t.Helper()
	vp, err := svc.Viewport(t.Context(), w, s, e, n)
	if err != nil {
		t.Fatal(err)
	}
	if !vp.Ready {
		t.Fatal("viewport not ready")
	}
	inBox := func(b orb.Bound) bool { return b.Max[0] >= w && b.Min[0] <= e && b.Max[1] >= s && b.Min[1] <= n }
	// union of single-cell documents, each trimmed to the viewport bbox, then deduped by content
	uF, uL, uP := map[string]int{}, map[string]int{}, map[string]Parcel{}
	for _, c := range cellsFor(w, s, e, n) {
		cw, cs, ce, cn := c.bbox()
		r, err := svc.Viewport(t.Context(), cw, cs, ce, cn)
		if err != nil {
			t.Fatal(err)
		}
		if !r.Ready {
			t.Fatal("cell not ready")
		}
		for _, f := range r.Footprints {
			if inBox(f.Geometry.Geometry().Bound()) {
				uF[fmt.Sprint(f.NS, f.Geometry.Geometry())] = 1
			}
		}
		for _, l := range r.Landuse {
			if inBox(l.Geometry.Geometry().Bound()) {
				uL[fmt.Sprint(l.NS, l.Geometry.Geometry())] = 1
			}
		}
		for _, p := range r.Parcels {
			if !inBox(p.Geometry.Geometry().Bound()) {
				continue
			}
			if q, ok := uP[p.ParcelID]; !ok || (p.Complete && !q.Complete) || (p.Complete == q.Complete && p.AreaSqm > q.AreaSqm) {
				uP[p.ParcelID] = p
			}
		}
	}
	// a fragment that only a single-cell document shows (whole copy lives in the neighbour) must not
	// survive in the union either: drop incomplete fragments whose geometry is covered by a complete copy.
	// (For synthetic + live data this is already guaranteed by compose on 1 cell; the union is a set.)
	check := func(layer string, got []Piece, want map[string]int) {
		g := geomSet(got)
		for k, c := range g {
			if c > 1 {
				t.Errorf("%s: duplicate geometry ×%d: %.80s", layer, c, k)
			}
		}
		var missing, extra []string
		for _, k := range sortedKeys(want) {
			if _, ok := g[k]; !ok {
				missing = append(missing, k)
			}
		}
		for _, k := range sortedKeys(g) {
			if _, ok := want[k]; !ok {
				extra = append(extra, k)
			}
		}
		if len(missing)+len(extra) > 0 {
			t.Errorf("%s: viewport ≠ union of cells: %d missing %d extra\n  missing: %.300s\n  extra: %.300s",
				layer, len(missing), len(extra), strings.Join(missing, "\n"), strings.Join(extra, "\n"))
		}
	}
	check("footprints", vp.Footprints, uF)
	check("landuse", vp.Landuse, uL)
	if len(vp.Parcels) != len(uP) {
		t.Errorf("parcels: viewport %d ≠ union %d", len(vp.Parcels), len(uP))
	}
	for _, p := range vp.Parcels {
		if q := uP[p.ParcelID]; q.Complete != p.Complete || q.AreaSqm != p.AreaSqm {
			t.Errorf("parcel %s: viewport %+v ≠ union %+v", p.ParcelID, p.Complete, q.Complete)
		}
	}
}

// TestComposeEqualsCellUnion: a footprint straddling the z16 tile edge at lon
// 16.1554 (inside cell 808's west pad) is merged whole in cell 807 (holds both
// tiles) and clipped in cell 808 (holds only the eastern tile). Before the
// member-id dedupe the viewport over both cells emitted both copies, and when
// the merged piece happened to carry the fragment's id the fragment won.
func TestComposeEqualsCellUnion(t *testing.T) {
	const edge = 16.1554 // west edge of z16 tile 35709 (approx.)
	whole := Piece{ID: "16_35708_22800#3", NS: 41, Tile: "16/35708/22800+16/35709/22800", AreaSqm: 300, Complete: true,
		Members: []string{"16_35708_22800#3", "16_35709_22800#7"}, Geometry: sqGeom(edge-0.0002, 47.715, 0.0004, 0.0002)}
	frag := Piece{ID: "16_35709_22800#7", NS: 41, Tile: "16/35709/22800", AreaSqm: 150, Complete: false,
		Geometry: sqGeom(edge, 47.715, 0.0002, 0.0002)}
	// an unrelated per-tile landuse piece present identically in both cells (tile is in both fetched sets)
	lu := Piece{ID: "16_35709_22801#2", NS: 11, Tile: "16/35709/22801", AreaSqm: 900, Complete: true, Geometry: sqGeom(16.1570, 47.716, 0.0005, 0.0005)}
	// a landuse piece at cell 808's fetched edge: incomplete in 808, complete (and larger) in 807
	luEdgeA := Piece{ID: "16_35709_22802#0", NS: 20, Tile: "16/35709/22802", AreaSqm: 500, Complete: true, Geometry: sqGeom(edge-0.0003, 47.717, 0.0006, 0.0003)}
	luEdgeB := Piece{ID: "16_35709_22802#0", NS: 20, Tile: "16/35709/22802", AreaSqm: 250, Complete: false, Geometry: sqGeom(edge, 47.717, 0.0003, 0.0003)}
	own807 := Piece{ID: "16_35700_22800#1", NS: 41, Tile: "16/35700/22800", AreaSqm: 80, Complete: true, Geometry: sqGeom(16.142, 47.71, 0.0002, 0.0002)}
	own808 := Piece{ID: "16_35712_22800#1", NS: 41, Tile: "16/35712/22800", AreaSqm: 80, Complete: true, Geometry: sqGeom(16.165, 47.71, 0.0002, 0.0002)}

	for _, order := range []string{"whole-first", "fragment-first"} {
		for _, sameID := range []bool{false, true} {
			t.Run(order+fmt.Sprintf("/sameID=%v", sameID), func(t *testing.T) {
				wh := whole
				if sameID { // seam merge kept the eastern member as first → merged piece carries the fragment's id
					wh.ID = frag.ID
					wh.Members = []string{"16_35709_22800#7", "16_35708_22800#3"}
				}
				svc := NewService(ServiceOptions{MaxCells: 8, Prefetch: 0})
				a := &Result{Ready: true, Footprints: []Piece{own807, wh}, Landuse: []Piece{lu, luEdgeA}}
				b := &Result{Ready: true, Footprints: []Piece{frag, own808}, Landuse: []Piece{luEdgeB, lu}}
				svc.cells.Put(cell{807, 2385}.key(), a)
				svc.cells.Put(cell{808, 2385}.key(), b)
				// note: cells are composed in cellsFor order (807 then 808); "fragment-first" swaps the contents
				if order == "fragment-first" {
					svc.cells.Put(cell{807, 2385}.key(), b)
					svc.cells.Put(cell{808, 2385}.key(), a)
				}
				res, err := svc.Viewport(t.Context(), 16.1402, 47.7068, 16.168, 47.719) // cells 807–808 / 2385 only
				if err != nil {
					t.Fatal(err)
				}
				if len(res.Footprints) != 3 {
					t.Fatalf("want 3 footprints (whole + 2 own), got %d: %+v", len(res.Footprints), res.Footprints)
				}
				found := false
				for _, f := range res.Footprints {
					if f.AreaSqm == 150 {
						t.Fatalf("fragment survived: %+v", f)
					}
					if f.AreaSqm == 300 && f.Complete && len(f.Members) == 2 {
						found = true
					}
				}
				if !found {
					t.Fatalf("whole merged footprint missing: %+v", res.Footprints)
				}
				if len(res.Landuse) != 2 {
					t.Fatalf("want 2 landuse pieces, got %d: %+v", len(res.Landuse), res.Landuse)
				}
				for _, l := range res.Landuse {
					if l.NS == 20 && (!l.Complete || l.AreaSqm != 500) {
						t.Fatalf("complete landuse copy should win: %+v", l)
					}
				}
			})
		}
	}
}

// TestComposeEqualsCellUnionLive runs the invariant against BEV tiles for the
// viewport that exposed the bug (Guntrams, KG 23308; 2×2 cells). Needs network:
// BEVDIRECT_LIVE=1 go test ./bevdirect -run Live -v
func TestComposeEqualsCellUnionLive(t *testing.T) {
	if os.Getenv("BEVDIRECT_LIVE") == "" {
		t.Skip("set BEVDIRECT_LIVE=1 to fetch BEV tiles")
	}
	svc := NewService(ServiceOptions{MaxCells: 64, Wait: 180 * time.Second, Prefetch: 0})
	assertViewportEqualsCellUnion(t, svc, 16.1402, 47.7068, 16.168, 47.728)
	single, _ := svc.Viewport(t.Context(), 16.14, 47.70, 16.16, 47.72)
	vp, _ := svc.Viewport(t.Context(), 16.1402, 47.7068, 16.168, 47.728)
	t.Logf("single cell 807/2385: %d fp %d lu · viewport: %d fp %d lu", len(single.Footprints), len(single.Landuse), len(vp.Footprints), len(vp.Landuse))
}
