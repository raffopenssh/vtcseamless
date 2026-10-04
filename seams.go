package vtcseamless

// Step 4 — seams in layers WITHOUT a stable feature id. When a tile layer
// carries no id, pieces can only be emitted per tile, and a feature that
// straddles a tile edge arrives as two pieces. MergeSeams unions pieces from
// adjacent tiles that touch the shared edge over a common stretch (> ~0.3 m)
// and whose union actually dissolves the seam (fewer polygons out than in).
// Pieces of two different features only ever meet the edge at a point, so
// they stay apart.

import (
	"math"
	"sort"

	"github.com/ctessum/polyclip-go"
	"github.com/paulmach/orb"
)

const seamEps = 2e-6     // ≈ 0.2 m: "touches the tile edge"
const seamOverlap = 3e-6 // ≈ 0.3 m of shared edge required

// SeamPiece is one tile-clipped feature of an id-less layer: the tile it came
// from and its polygons (as produced by ClipToBound).
type SeamPiece struct {
	Tile     Tile
	Polygons []orb.Polygon
}

// SeamGroup is one output of MergeSeams. Members are indices into the input
// (ascending). Merged is the dissolved geometry (coordinates rounded), or nil
// when the members are to be kept exactly as they were — either because the
// group is a singleton or because the union did not reduce the polygon count.
type SeamGroup struct {
	Members []int
	Merged  orb.MultiPolygon
}

// MergeSeams partitions pieces into groups that belong to one feature split
// by a tile edge and unions each group. Every input index appears in exactly
// one group; groups are ordered by their smallest member, so emitting the
// groups in order preserves the input order for everything untouched.
// decimals is the coordinate rounding applied to merged geometries.
func MergeSeams(pieces []SeamPiece, decimals int) []SeamGroup {
	n := len(pieces)
	singletons := func() []SeamGroup {
		out := make([]SeamGroup, n)
		for i := range out {
			out[i] = SeamGroup{Members: []int{i}}
		}
		return out
	}
	if n < 2 {
		return singletons()
	}
	bounds := make([]orb.Bound, n)
	for i := range pieces {
		bounds[i] = orb.MultiPolygon(pieces[i].Polygons).Bound()
	}
	// edge → [piece idx, side] where side 0 = tile on the low (west/south) side
	type ref struct {
		idx  int
		side int
	}
	type seamEdge struct {
		vertical bool
		x, y     int
	}
	edges := map[seamEdge][]ref{}
	for i := range pieces {
		t := pieces[i].Tile
		tb := t.Bound()
		b := bounds[i]
		if math.Abs(b.Max[0]-tb.Max[0]) < seamEps { // east edge → shared with X+1
			e := seamEdge{true, t.X + 1, t.Y}
			edges[e] = append(edges[e], ref{i, 0})
		}
		if math.Abs(b.Min[0]-tb.Min[0]) < seamEps { // west edge
			e := seamEdge{true, t.X, t.Y}
			edges[e] = append(edges[e], ref{i, 1})
		}
		if math.Abs(b.Max[1]-tb.Max[1]) < seamEps { // north edge → shared with tile Y-1 (tile Y grows south)
			e := seamEdge{false, t.X, t.Y}
			edges[e] = append(edges[e], ref{i, 1})
		}
		if math.Abs(b.Min[1]-tb.Min[1]) < seamEps { // south edge
			e := seamEdge{false, t.X, t.Y + 1}
			edges[e] = append(edges[e], ref{i, 0})
		}
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
	joined := 0
	for e, refs := range edges {
		for a := 0; a < len(refs); a++ {
			for b := a + 1; b < len(refs); b++ {
				if refs[a].side == refs[b].side {
					continue
				}
				ba, bb := bounds[refs[a].idx], bounds[refs[b].idx]
				var ov float64
				if e.vertical {
					ov = math.Min(ba.Max[1], bb.Max[1]) - math.Max(ba.Min[1], bb.Min[1])
				} else {
					ov = math.Min(ba.Max[0], bb.Max[0]) - math.Max(ba.Min[0], bb.Min[0])
				}
				if ov < seamOverlap {
					continue
				}
				ra, rb := find(refs[a].idx), find(refs[b].idx)
				if ra != rb {
					parent[ra] = rb
					joined++
				}
			}
		}
	}
	if joined == 0 {
		return singletons()
	}
	groups := map[int][]int{}
	for i := range pieces {
		r := find(i)
		groups[r] = append(groups[r], i)
	}
	out := make([]SeamGroup, 0, n)
	done := make([]bool, n)
	for i := range pieces {
		if done[i] {
			continue
		}
		grp := groups[find(i)]
		sort.Ints(grp)
		if len(grp) == 1 {
			done[i] = true
			out = append(out, SeamGroup{Members: grp})
			continue
		}
		var acc polyclip.Polygon
		inPolys := 0
		for k, idx := range grp {
			done[idx] = true
			for _, p := range pieces[idx].Polygons {
				inPolys++
				pc := ToPolyclip(p)
				if k == 0 && len(acc) == 0 {
					acc = pc
				} else {
					acc = acc.Construct(polyclip.UNION, pc)
				}
			}
		}
		merged := FromPolyclip(acc)
		if len(merged) == 0 || len(merged) >= inPolys {
			out = append(out, SeamGroup{Members: grp})
			continue
		}
		RoundCoords(merged, decimals)
		out = append(out, SeamGroup{Members: grp, Merged: merged})
	}
	return out
}
