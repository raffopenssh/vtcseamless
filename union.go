package vtcseamless

// Step 2 of the assembly: dissolve the shared tile edges between the DISJOINT
// pieces of one feature, guarded by the additive area of the pieces.

import (
	"math"
	"sort"

	"github.com/ctessum/polyclip-go"
	"github.com/paulmach/orb"
	"github.com/paulmach/orb/planar"
)

// UnionAreaTol is the relative area mismatch above which a union is rejected
// and the pieces are kept as they are.
const UnionAreaTol = 0.02

// UnionPieces dissolves shared tile edges between DISJOINT pieces of one
// feature (each already clipped to its own tile with ClipToBound). Pieces are
// first clustered by touching bboxes; each cluster is unioned with polyclip
// and the union is accepted only if its planar area agrees with the sum of
// the piece areas within UnionAreaTol — otherwise the cluster's pieces are
// emitted unchanged. targetArea is the expected total (deg²; the sum of the
// piece areas as computed by the caller; <= 0 disables the final check).
// The second result reports whether any guard tripped.
func UnionPieces(polys []orb.Polygon, targetArea float64) (orb.MultiPolygon, bool) {
	if len(polys) == 1 {
		return orb.MultiPolygon{polys[0]}, false
	}
	clusters := clusterTouching(polys)
	tripped := false
	var out orb.MultiPolygon
	for _, cl := range clusters {
		if len(cl) == 1 {
			out = append(out, polys[cl[0]])
			continue
		}
		var acc polyclip.Polygon
		want := 0.0
		for i, idx := range cl {
			want += math.Abs(planar.Area(polys[idx]))
			pc := ToPolyclip(polys[idx])
			if i == 0 {
				acc = pc
			} else {
				acc = acc.Construct(polyclip.UNION, pc)
			}
		}
		merged := FromPolyclip(acc)
		if !plausible(merged, want) {
			tripped = true
			for _, idx := range cl {
				out = append(out, polys[idx])
			}
			continue
		}
		out = append(out, merged...)
	}
	if !plausible(out, targetArea) {
		tripped = true
	}
	return out, tripped
}

func plausible(mp orb.MultiPolygon, want float64) bool {
	if len(mp) == 0 {
		return false
	}
	if want <= 0 {
		return true
	}
	got := 0.0
	for _, p := range mp {
		got += math.Abs(planar.Area(p))
	}
	return math.Abs(got-want)/want <= UnionAreaTol
}

func clusterTouching(polys []orb.Polygon) [][]int {
	n := len(polys)
	bounds := make([]orb.Bound, n)
	for i, p := range polys {
		bounds[i] = p.Bound()
	}
	parent := make([]int, n)
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	const eps = 1e-9
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			a, b := bounds[i], bounds[j]
			if a.Min[0] <= b.Max[0]+eps && b.Min[0] <= a.Max[0]+eps && a.Min[1] <= b.Max[1]+eps && b.Min[1] <= a.Max[1]+eps {
				ra, rb := find(i), find(j)
				if ra != rb {
					parent[ra] = rb
				}
			}
		}
	}
	byRoot := map[int][]int{}
	var roots []int
	for i := 0; i < n; i++ {
		r := find(i)
		if _, ok := byRoot[r]; !ok {
			roots = append(roots, r)
		}
		byRoot[r] = append(byRoot[r], i)
	}
	sort.Ints(roots)
	out := make([][]int, 0, len(roots))
	for _, r := range roots {
		out = append(out, byRoot[r])
	}
	return out
}

// ToPolygons flattens a geometry to its polygons (Polygon, MultiPolygon,
// Collection); nil for anything else.
func ToPolygons(g orb.Geometry) []orb.Polygon {
	switch v := g.(type) {
	case orb.Polygon:
		return []orb.Polygon{v}
	case orb.MultiPolygon:
		return v
	case orb.Collection:
		var out []orb.Polygon
		for _, x := range v {
			out = append(out, ToPolygons(x)...)
		}
		return out
	}
	return nil
}
