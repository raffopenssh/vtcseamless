package bevdirect

// Footprint seams. BEV's nfl layer has no stable footprint id, so pieces are
// emitted per z16 tile and a building that straddles a tile edge arrives as
// two pieces (≈10 % of the buildings in a village: the z16 grid is ~400 m).
// mergeFootprintSeams hands the pieces to vtcseamless.MergeSeams and rebuilds
// the merged Piece (id/ns of the first member, tile keys joined with "+").

import (
	"math"
	"strings"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/geojson"
	"github.com/raffopenssh/vtcseamless"
)

func mergeFootprintSeams(fps []Piece, tileOf map[string]Tile, dec int) []Piece {
	if len(fps) < 2 {
		return fps
	}
	in := make([]vtcseamless.SeamPiece, len(fps))
	for i := range fps {
		in[i] = vtcseamless.SeamPiece{Tile: tileOf[fps[i].Tile], Polygons: vtcseamless.ToPolygons(fps[i].Geometry.Geometry())}
	}
	groups := vtcseamless.MergeSeams(in, dec)
	out := make([]Piece, 0, len(fps))
	for _, g := range groups {
		if g.Merged == nil {
			for _, idx := range g.Members {
				out = append(out, fps[idx])
			}
			continue
		}
		merged := g.Merged
		var geom orb.Geometry = merged
		if len(merged) == 1 {
			geom = merged[0]
		}
		tiles := make([]string, 0, len(g.Members))
		for _, idx := range g.Members {
			tiles = append(tiles, fps[idx].Tile)
		}
		first := fps[g.Members[0]]
		out = append(out, Piece{ID: first.ID, NS: first.NS, Tile: strings.Join(tiles, "+"),
			AreaSqm: math.Round(vtcseamless.AreaSqm(merged)*10) / 10, Geometry: geojson.NewGeometry(geom)})
	}
	return out
}
