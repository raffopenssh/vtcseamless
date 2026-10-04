package vtcseamless

import (
	"math"
	"testing"

	"github.com/paulmach/orb"
)

func TestTilesForBound(t *testing.T) {
	b := orb.Bound{Min: orb.Point{15.075, 47.055}, Max: orb.Point{15.095, 47.075}}
	ts := TilesForBound(b, 15)
	if len(ts) == 0 {
		t.Fatal("no tiles")
	}
	u := TileSetBound(ts)
	if !u.Contains(b.Min) || !u.Contains(b.Max) {
		t.Fatalf("tile set %v does not cover %v", u, b)
	}
	for _, tc := range ts {
		if !tc.Bound().Intersects(b) {
			t.Fatalf("tile %v outside bbox", tc)
		}
	}
}

func TestAreaSqm(t *testing.T) {
	// 0.001° × 0.001° square at 47°N ≈ 111.2 m × 75.9 m ≈ 8 440 m²
	r := orb.Ring{{15, 47}, {15.001, 47}, {15.001, 47.001}, {15, 47.001}, {15, 47}}
	got := AreaSqm(orb.MultiPolygon{{r}})
	if math.Abs(got-8440)/8440 > 0.005 {
		t.Fatalf("area %.1f", got)
	}
}

func TestUnionAcrossTileEdge(t *testing.T) {
	// one square split along x=0.5 into two disjoint pieces → union is a single polygon with the sum area
	a := orb.Polygon{{{0, 0}, {0.5, 0}, {0.5, 1}, {0, 1}, {0, 0}}}
	b := orb.Polygon{{{0.5, 0}, {1, 0}, {1, 1}, {0.5, 1}, {0.5, 0}}}
	mp, tripped := UnionPieces([]orb.Polygon{a, b}, 1.0)
	if tripped || len(mp) != 1 {
		t.Fatalf("tripped=%v parts=%d", tripped, len(mp))
	}
	if math.Abs(math.Abs(ringSignedArea(mp[0][0]))-1) > 1e-9 {
		t.Fatalf("area %v", ringSignedArea(mp[0][0]))
	}
}

func TestUnionGuardRejectsOverlap(t *testing.T) {
	// two OVERLAPPING copies (what un-clipped buffered tiles produce): the union's
	// area (1.5) disagrees with the additive area (2.0) by far more than 2 %, so the
	// pieces are kept and the guard reports a trip.
	a := orb.Polygon{{{0, 0}, {1, 0}, {1, 1}, {0, 1}, {0, 0}}}
	b := orb.Polygon{{{0.5, 0}, {1.5, 0}, {1.5, 1}, {0.5, 1}, {0.5, 0}}}
	mp, tripped := UnionPieces([]orb.Polygon{a, b}, 2.0)
	if !tripped || len(mp) != 2 {
		t.Fatalf("tripped=%v parts=%d", tripped, len(mp))
	}
}

func TestClipToTile(t *testing.T) {
	// polygon bleeding past the rectangle (tile buffer) is cut back to it
	pc := ToPolyclip(orb.Polygon{{{-1, -1}, {2, -1}, {2, 2}, {-1, 2}, {-1, -1}}})
	got := RectClip(pc, orb.Bound{Min: orb.Point{0, 0}, Max: orb.Point{1, 1}})
	mp := FromPolyclip(got)
	if len(mp) != 1 || math.Abs(math.Abs(ringSignedArea(mp[0][0]))-1) > 1e-9 {
		t.Fatalf("clip: %v", mp)
	}
	if mp2 := ClipToBound(orb.Polygon{{{-1, -1}, {2, -1}, {2, 2}, {-1, 2}, {-1, -1}}}, orb.Bound{Min: orb.Point{0, 0}, Max: orb.Point{1, 1}}); !mp2.Equal(mp) {
		t.Fatalf("ClipToBound differs: %v vs %v", mp2, mp)
	}
	if ClipToBound(orb.Polygon{{{5, 5}, {6, 5}, {6, 6}, {5, 5}}}, orb.Bound{Min: orb.Point{0, 0}, Max: orb.Point{1, 1}}) != nil {
		t.Fatal("disjoint polygon should clip to nil")
	}
}

func TestClassifyRings(t *testing.T) {
	// outer shell, a hole, an island inside the hole — all given clockwise
	outer := orb.Ring{{0, 0}, {0, 10}, {10, 10}, {10, 0}, {0, 0}}
	hole := orb.Ring{{2, 2}, {2, 8}, {8, 8}, {8, 2}, {2, 2}}
	island := orb.Ring{{4, 4}, {4, 6}, {6, 6}, {6, 4}, {4, 4}}
	polys := ClassifyRings([]orb.Ring{island, hole, outer})
	if len(polys) != 2 || len(polys[0]) != 2 || len(polys[1]) != 1 {
		t.Fatalf("classification: %d polys, rings %v", len(polys), polys)
	}
	if polys[0][0].Orientation() != orb.CCW || polys[0][1].Orientation() != orb.CW || polys[1][0].Orientation() != orb.CCW {
		t.Fatalf("orientation not RFC 7946: %v", polys)
	}
}

func TestMergeSeams(t *testing.T) {
	// one building split by the vertical edge between two z16 tiles
	left := Tile{Z: 16, X: 35512, Y: 22900}
	right := Tile{Z: 16, X: 35513, Y: 22900}
	lb, rb := left.Bound(), right.Bound()
	x := lb.Max[0] // shared edge
	y0 := (lb.Min[1] + lb.Max[1]) / 2
	a := orb.Polygon{{{x - 0.0002, y0}, {x, y0}, {x, y0 + 0.0001}, {x - 0.0002, y0 + 0.0001}, {x - 0.0002, y0}}}
	b := orb.Polygon{{{x, y0}, {x + 0.0002, y0}, {x + 0.0002, y0 + 0.0001}, {x, y0 + 0.0001}, {x, y0}}}
	// a second building in the right tile touching the edge only at a point
	c := orb.Polygon{{{x, y0 + 0.0001}, {x + 0.0001, y0 + 0.0001}, {x + 0.0001, y0 + 0.0002}, {x, y0 + 0.0002}, {x, y0 + 0.0001}}}
	_ = rb
	groups := MergeSeams([]SeamPiece{{left, []orb.Polygon{a}}, {right, []orb.Polygon{b}}, {right, []orb.Polygon{c}}}, 7)
	if len(groups) != 2 || len(groups[0].Members) != 2 || groups[0].Merged == nil || len(groups[0].Merged) != 1 {
		t.Fatalf("seam not merged: %+v", groups)
	}
	if len(groups[1].Members) != 1 || groups[1].Members[0] != 2 || groups[1].Merged != nil {
		t.Fatalf("unrelated piece touched: %+v", groups[1])
	}
	// singletons when nothing joins
	if g := MergeSeams([]SeamPiece{{left, []orb.Polygon{a}}, {right, []orb.Polygon{c}}}, 7); len(g) != 2 || g[0].Merged != nil || g[1].Merged != nil {
		t.Fatalf("expected two singletons: %+v", g)
	}
}
