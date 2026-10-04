package vtcseamless

// Planar geometry: tile maths, own-tile clip, ring classification, area.
// Kept dependency-light on purpose: orb for planar math, polyclip for the
// boolean operations.

import (
	"math"
	"sort"

	"github.com/ctessum/polyclip-go"
	"github.com/paulmach/orb"
	"github.com/paulmach/orb/planar"
)

// Tile is a slippy-map tile coordinate.
type Tile struct{ Z, X, Y int }

// Bound is the exact lon/lat bbox of the tile.
func (t Tile) Bound() orb.Bound {
	n := math.Pow(2, float64(t.Z))
	lon := func(x float64) float64 { return x/n*360.0 - 180.0 }
	lat := func(y float64) float64 { return math.Atan(math.Sinh(math.Pi*(1-2*y/n))) * 180.0 / math.Pi }
	return orb.Bound{
		Min: orb.Point{lon(float64(t.X)), lat(float64(t.Y + 1))},
		Max: orb.Point{lon(float64(t.X + 1)), lat(float64(t.Y))},
	}
}

// TileAt is the tile at zoom z that contains lon/lat.
func TileAt(lon, lat float64, z int) Tile {
	n := math.Pow(2, float64(z))
	x := int(math.Floor((lon + 180.0) / 360.0 * n))
	latRad := lat * math.Pi / 180
	y := int(math.Floor((1.0 - math.Log(math.Tan(latRad)+1/math.Cos(latRad))/math.Pi) / 2.0 * n))
	return Tile{Z: z, X: x, Y: y}
}

// TilesForBound lists the tiles at zoom z covering b (row-major).
func TilesForBound(b orb.Bound, z int) []Tile {
	a := TileAt(b.Min[0], b.Max[1], z) // NW
	c := TileAt(b.Max[0], b.Min[1], z) // SE
	var out []Tile
	for y := a.Y; y <= c.Y; y++ {
		for x := a.X; x <= c.X; x++ {
			out = append(out, Tile{Z: z, X: x, Y: y})
		}
	}
	return out
}

// TileSetBound is the union bbox of a tile set.
func TileSetBound(tiles []Tile) orb.Bound {
	var b orb.Bound
	for i, t := range tiles {
		if i == 0 {
			b = t.Bound()
		} else {
			b = b.Union(t.Bound())
		}
	}
	return b
}

// ToPolyclip converts an orb.Polygon to polyclip's contour list.
func ToPolyclip(p orb.Polygon) polyclip.Polygon {
	result := make(polyclip.Polygon, len(p))
	for i, ring := range p {
		contour := make(polyclip.Contour, len(ring))
		for j, pt := range ring {
			contour[j] = polyclip.Point{X: pt[0], Y: pt[1]}
		}
		result[i] = contour
	}
	return result
}

// FromPolyclip turns polyclip's flat, unordered contour list into a
// MultiPolygon by nesting depth (orientation-independent; see ClassifyRings).
// Degenerate contours (< 3 points) are dropped; nil if nothing remains.
func FromPolyclip(p polyclip.Polygon) orb.MultiPolygon {
	rings := make([]orb.Ring, 0, len(p))
	for _, contour := range p {
		if len(contour) < 3 {
			continue
		}
		ring := make(orb.Ring, 0, len(contour)+1)
		for _, pt := range contour {
			ring = append(ring, orb.Point{pt.X, pt.Y})
		}
		if ring[0] != ring[len(ring)-1] {
			ring = append(ring, ring[0])
		}
		rings = append(rings, ring)
	}
	if len(rings) == 0 {
		return nil
	}
	return orb.MultiPolygon(ClassifyRings(rings))
}

// ClipToBound clips poly to the axis-aligned rectangle b (its own tile) and
// returns the result as a MultiPolygon with classified rings; nil if nothing
// is left. This is step 1 of the assembly: pieces clipped to tessellating
// tiles are disjoint, so their areas add.
func ClipToBound(poly orb.Polygon, b orb.Bound) orb.MultiPolygon {
	pc := RectClip(ToPolyclip(poly), b)
	if len(pc) == 0 {
		return nil
	}
	return FromPolyclip(pc)
}

// RectClip is Sutherland–Hodgman of each contour against an axis-aligned
// rectangle. Contours that collapse below 3 points are dropped.
func RectClip(pc polyclip.Polygon, b orb.Bound) polyclip.Polygon {
	out := make(polyclip.Polygon, 0, len(pc))
	for _, c := range pc {
		cur := c
		for edge := 0; edge < 4 && len(cur) >= 3; edge++ {
			next := make(polyclip.Contour, 0, len(cur)+4)
			n := len(cur)
			for i := 0; i < n; i++ {
				p1, p2 := cur[i], cur[(i+1)%n]
				in1, in2 := rectInside(p1, b, edge), rectInside(p2, b, edge)
				if in1 {
					next = append(next, p1)
				}
				if in1 != in2 {
					next = append(next, rectIntersect(p1, p2, b, edge))
				}
			}
			cur = next
		}
		if len(cur) >= 3 {
			out = append(out, cur)
		}
	}
	return out
}

func rectInside(p polyclip.Point, b orb.Bound, edge int) bool {
	switch edge {
	case 0:
		return p.X >= b.Min[0]
	case 1:
		return p.X <= b.Max[0]
	case 2:
		return p.Y >= b.Min[1]
	default:
		return p.Y <= b.Max[1]
	}
}

func rectIntersect(p1, p2 polyclip.Point, b orb.Bound, edge int) polyclip.Point {
	switch edge {
	case 0:
		t := (b.Min[0] - p1.X) / (p2.X - p1.X)
		return polyclip.Point{X: b.Min[0], Y: p1.Y + t*(p2.Y-p1.Y)}
	case 1:
		t := (b.Max[0] - p1.X) / (p2.X - p1.X)
		return polyclip.Point{X: b.Max[0], Y: p1.Y + t*(p2.Y-p1.Y)}
	case 2:
		t := (b.Min[1] - p1.Y) / (p2.Y - p1.Y)
		return polyclip.Point{X: p1.X + t*(p2.X-p1.X), Y: b.Min[1]}
	default:
		t := (b.Max[1] - p1.Y) / (p2.Y - p1.Y)
		return polyclip.Point{X: p1.X + t*(p2.X-p1.X), Y: b.Max[1]}
	}
}

// ClassifyRings assigns shell/hole by nesting depth (even = shell, odd = hole)
// and normalises orientation (shell CCW, holes CW) — RFC 7946. Input
// orientation is ignored; containment is tested geometrically. Rings are
// modified in place (reversed where needed).
func ClassifyRings(rings []orb.Ring) []orb.Polygon {
	if len(rings) == 1 {
		r := rings[0]
		if r.Orientation() == orb.CW {
			r.Reverse()
		}
		return []orb.Polygon{{r}}
	}
	type ringInfo struct {
		ring   orb.Ring
		bound  orb.Bound
		area   float64
		parent int
		depth  int
	}
	infos := make([]ringInfo, 0, len(rings))
	for _, r := range rings {
		infos = append(infos, ringInfo{ring: r, bound: r.Bound(), area: math.Abs(ringSignedArea(r)), parent: -1})
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].area > infos[j].area })
	for i := 1; i < len(infos); i++ {
		for j := i - 1; j >= 0; j-- {
			if !infos[j].bound.Contains(infos[i].bound.Min) || !infos[j].bound.Contains(infos[i].bound.Max) {
				continue
			}
			if ringContainsRing(infos[j].ring, infos[i].ring) {
				infos[i].parent = j
				infos[i].depth = infos[j].depth + 1
				break
			}
		}
	}
	polyOf := make([]int, len(infos))
	out := make([]orb.Polygon, 0, 1)
	for i := range infos {
		if infos[i].depth%2 == 0 {
			r := infos[i].ring
			if r.Orientation() == orb.CW {
				r.Reverse()
			}
			polyOf[i] = len(out)
			out = append(out, orb.Polygon{r})
		} else {
			polyOf[i] = -1
		}
	}
	for i := range infos {
		if polyOf[i] != -1 || infos[i].parent < 0 {
			continue
		}
		shell := polyOf[infos[i].parent]
		if shell < 0 {
			continue
		}
		r := infos[i].ring
		if r.Orientation() == orb.CCW {
			r.Reverse()
		}
		out[shell] = append(out[shell], r)
	}
	return out
}

// ringContainsRing samples several vertices because adjacent cadastral rings
// share vertices and a single ray cast on a shared vertex is undefined.
func ringContainsRing(outer, inner orb.Ring) bool {
	n := len(inner)
	if n == 0 {
		return false
	}
	if n > 1 && inner[0] == inner[n-1] {
		n--
	}
	step := 1
	if n > 8 {
		step = n / 8
	}
	bound := outer.Bound()
	for k := 0; k < n; k += step {
		if bound.Contains(inner[k]) && planar.RingContains(outer, inner[k]) {
			return true
		}
	}
	return false
}

func ringSignedArea(r orb.Ring) float64 {
	if len(r) < 3 {
		return 0
	}
	sum := 0.0
	ox, oy := r[0][0], r[0][1]
	for i := 0; i < len(r)-1; i++ {
		sum += (r[i][0]-ox)*(r[i+1][1]-oy) - (r[i+1][0]-ox)*(r[i][1]-oy)
	}
	return sum / 2
}

// AreaSqm is the area of a lon/lat MultiPolygon in m² on a local tangent
// plane (ellipsoidal metres-per-degree at the bbox centre; <0.1 % at parcel
// scale). Holes (rings after the first) are subtracted. It is a geometric
// area, not a legally binding one.
func AreaSqm(mp orb.MultiPolygon) float64 {
	if len(mp) == 0 {
		return 0
	}
	c := mp.Bound().Center()
	phi := c[1] * math.Pi / 180
	// metres per degree on the WGS84 ellipsoid (series expansion), ±0.01 %
	ky := 111132.954 - 559.822*math.Cos(2*phi) + 1.175*math.Cos(4*phi)
	kx := 111412.84*math.Cos(phi) - 93.5*math.Cos(3*phi) + 0.118*math.Cos(5*phi)
	total := 0.0
	for _, poly := range mp {
		for ri, ring := range poly {
			a := 0.0
			for i := 0; i < len(ring)-1; i++ {
				x1, y1 := (ring[i][0]-c[0])*kx, (ring[i][1]-c[1])*ky
				x2, y2 := (ring[i+1][0]-c[0])*kx, (ring[i+1][1]-c[1])*ky
				a += x1*y2 - x2*y1
			}
			a = math.Abs(a / 2)
			if ri == 0 {
				total += a
			} else {
				total -= a
			}
		}
	}
	return total
}

// RoundCoords rounds every coordinate in place to dec decimals.
func RoundCoords(mp orb.MultiPolygon, dec int) {
	f := math.Pow(10, float64(dec))
	for _, poly := range mp {
		for _, ring := range poly {
			for i := range ring {
				ring[i][0] = math.Round(ring[i][0]*f) / f
				ring[i][1] = math.Round(ring[i][1]*f) / f
			}
		}
	}
}
