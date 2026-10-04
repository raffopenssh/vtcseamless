package bevdirect

// Enrichment computed purely from the fetched tiles: per-parcel land-use split,
// footprint↔parcel link, building counts, footprint shape metrics — the
// per-parcel fields a map client needs beside the geometry.

import (
	"math"
	"runtime"
	"sort"
	"sync"

	"github.com/ctessum/polyclip-go"
	"github.com/paulmach/orb"
	"github.com/paulmach/orb/planar"
	"github.com/raffopenssh/vtcseamless"
)

// NSName maps a BEV Nutzungssymbol code to its "Nutzung" name (official list).
var NSName = map[int]string{
	40: "Dauerkulturanlagen oder Erwerbsgärten", 41: "Gebäude", 42: "Parkplätze", 48: "Äcker, Wiesen oder Weiden",
	52: "Gärten", 53: "Weingärten", 54: "Alpen", 55: "Krummholzflächen", 56: "Wälder", 57: "Verbuschte Flächen",
	58: "Forststraßen", 59: "Fließende Gewässer", 60: "Stehende Gewässer", 61: "Feuchtgebiete", 62: "Vegetationsarme Flächen",
	63: "Betriebsflächen", 64: "Gewässerrandflächen", 65: "Verkehrsrandflächen", 72: "Friedhöfe", 83: "Gebäudenebenflächen",
	84: "Abbauflächen, Halden und Deponien", 87: "Fels- und Geröllflächen", 88: "Gletscher", 92: "Schienenverkehrsanlagen",
	95: "Straßenverkehrsanlagen", 96: "Freizeitflächen",
}

// Enrichment is attached to Parcel when footprints/landuse were fetched.
type Enrichment struct {
	DominantNS         string             `json:"dominant_ns,omitempty"`
	LanduseAreas       map[string]float64 `json:"landuse_areas,omitempty"`
	LanduseAreasSource string             `json:"landuse_areas_source,omitempty"`
	BuildingCount      int                `json:"building_count"`
	TotalBuildingArea  float64            `json:"total_building_area_sqm"`
}

// Shape is attached to footprint Pieces.
type Shape struct {
	ParcelID       string  `json:"parcel_id,omitempty"`
	NSCode         string  `json:"ns_code"`
	Lon            float64 `json:"lon"`
	Lat            float64 `json:"lat"`
	PerimeterM     float64 `json:"perimeter_m"`
	Compactness    float64 `json:"compactness"`
	OBBLengthM     float64 `json:"obb_length_m"`
	OBBWidthM      float64 `json:"obb_width_m"`
	OBBElongation  float64 `json:"obb_elongation"`
	OrientationDeg float64 `json:"orientation_deg"`
}

// Enrich fills Parcel.Enrichment and Piece.Shape in place. Parcels only get
// landuse_areas when landuse was fetched, building_count when footprints were.
func Enrich(res *Result) {
	type pp struct {
		mp orb.MultiPolygon
		b  orb.Bound
	}
	parcels := make([]pp, len(res.Parcels))
	for i := range res.Parcels {
		mp := toMulti(res.Parcels[i].Geometry.Geometry())
		parcels[i] = pp{mp, mp.Bound()}
		res.Parcels[i].Enrichment = &Enrichment{}
	}
	find := func(pt orb.Point) int {
		for i, p := range parcels {
			if p.b.Contains(pt) && multiContains(p.mp, pt) {
				return i
			}
		}
		return -1
	}

	// footprints: shape metrics + link by centroid
	for i := range res.Footprints {
		fp := &res.Footprints[i]
		mp := toMulti(fp.Geometry.Geometry())
		if len(mp) == 0 {
			continue
		}
		sh := shapeOf(mp[0][0])
		sh.NSCode = "41"
		c, _ := planar.CentroidArea(mp[0])
		sh.Lon, sh.Lat = round7(c[0]), round7(c[1])
		if j := find(c); j >= 0 {
			sh.ParcelID = res.Parcels[j].ParcelID
			res.Parcels[j].Enrichment.BuildingCount++
			res.Parcels[j].Enrichment.TotalBuildingArea = math.Round((res.Parcels[j].Enrichment.TotalBuildingArea+fp.AreaSqm)*10) / 10
		}
		fp.Shape = &sh
	}

	// landuse split: intersect each parcel with overlapping nfl pieces (41 included)
	if len(res.Landuse) == 0 && len(res.Footprints) == 0 {
		return
	}
	var pieces []lp
	for _, lst := range [][]Piece{res.Landuse, res.Footprints} {
		for _, p := range lst {
			for _, poly := range toMulti(p.Geometry.Geometry()) {
				pieces = append(pieces, lp{vtcseamless.ToPolyclip(poly), poly.Bound(), p.NS})
			}
		}
	}
	sort.Slice(pieces, func(i, j int) bool { return pieces[i].b.Min[0] < pieces[j].b.Min[0] })
	if len(res.Landuse) == 0 {
		return
	}
	// Per parcel: pieces are first clipped to the parcel's bbox (cheap
	// Sutherland–Hodgman) so the polyclip sweep only sees the edges near the
	// parcel; parcels are independent, so the loop is spread over the CPUs
	// (1.4 s → 0.8 s wall for a 3 300-parcel cell on 2 cores).
	work := make(chan int, len(res.Parcels))
	for i := range res.Parcels {
		work <- i
	}
	close(work)
	var wg sync.WaitGroup
	for w := 0; w < runtime.GOMAXPROCS(0); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				enrichLanduse(&res.Parcels[i], parcels[i].mp, pieces)
			}
		}()
	}
	wg.Wait()
}

type lp struct {
	pc polyclip.Polygon
	b  orb.Bound
	ns int
}

func enrichLanduse(p *Parcel, mp orb.MultiPolygon, pieces []lp) {
	areas := map[string]float64{}
	for _, poly := range mp {
		ppc := vtcseamless.ToPolyclip(poly)
		pb := poly.Bound()
		lo := sort.Search(len(pieces), func(k int) bool { return pieces[k].b.Min[0] > pb.Max[0] })
		for _, lpc := range pieces[:lo] {
			if !lpc.b.Intersects(pb) {
				continue
			}
			clipped := lpc.pc
			if !pb.Contains(lpc.b.Min) || !pb.Contains(lpc.b.Max) {
				clipped = vtcseamless.RectClip(lpc.pc, pb)
				if len(clipped) == 0 {
					continue
				}
			}
			inter := ppc.Construct(polyclip.INTERSECTION, clipped)
			if len(inter) == 0 {
				continue
			}
			if a := vtcseamless.AreaSqm(vtcseamless.FromPolyclip(inter)); a > 0.05 {
				areas[itoa(lpc.ns)] += a
			}
		}
	}
	best, bestA := "", 0.0
	for k, v := range areas {
		areas[k] = math.Round(v*100) / 100
		if v > bestA {
			best, bestA = k, v
		}
	}
	if len(areas) > 0 {
		p.Enrichment.LanduseAreas, p.Enrichment.DominantNS, p.Enrichment.LanduseAreasSource = areas, best, "bev_tiles"
	}
}

func shapeOf(ring orb.Ring) Shape {
	var s Shape
	if len(ring) < 3 {
		return s
	}
	lat := ring[0][1] * math.Pi / 180
	kx, ky := 111412.84*math.Cos(lat)-93.5*math.Cos(3*lat), 111132.954-559.822*math.Cos(2*lat)
	pts := make([][2]float64, 0, len(ring))
	for _, p := range ring {
		pts = append(pts, [2]float64{(p[0] - ring[0][0]) * kx, (p[1] - ring[0][1]) * ky})
	}
	if pts[0] != pts[len(pts)-1] {
		pts = append(pts, pts[0])
	}
	var a2, per float64
	for i := 0; i < len(pts)-1; i++ {
		a2 += pts[i][0]*pts[i+1][1] - pts[i+1][0]*pts[i][1]
		per += math.Hypot(pts[i+1][0]-pts[i][0], pts[i+1][1]-pts[i][1])
	}
	area := math.Abs(a2) / 2
	s.PerimeterM = math.Round(per*100) / 100
	if per > 0 {
		s.Compactness = math.Round(math.Min(1, 4*math.Pi*area/(per*per))*1e4) / 1e4
	}
	hull := convexHull(pts[:len(pts)-1])
	if len(hull) < 3 {
		return s
	}
	bestA, bl, bw, ldx, ldy := math.Inf(1), 0.0, 0.0, 0.0, 0.0
	for i := range hull {
		j := (i + 1) % len(hull)
		ex, ey := hull[j][0]-hull[i][0], hull[j][1]-hull[i][1]
		el := math.Hypot(ex, ey)
		if el == 0 {
			continue
		}
		ux, uy := ex/el, ey/el
		minU, maxU, minV, maxV := math.Inf(1), math.Inf(-1), math.Inf(1), math.Inf(-1)
		for _, p := range hull {
			u, v := p[0]*ux+p[1]*uy, -p[0]*uy+p[1]*ux
			minU, maxU, minV, maxV = math.Min(minU, u), math.Max(maxU, u), math.Min(minV, v), math.Max(maxV, v)
		}
		w, h := maxU-minU, maxV-minV
		if w*h < bestA {
			bestA = w * h
			if w >= h {
				bl, bw, ldx, ldy = w, h, ux, uy
			} else {
				bl, bw, ldx, ldy = h, w, -uy, ux
			}
		}
	}
	s.OBBLengthM, s.OBBWidthM = math.Round(bl*100)/100, math.Round(bw*100)/100
	if bw > 0 {
		s.OBBElongation = math.Round(bl/bw*1e4) / 1e4
	}
	deg := math.Atan2(ldx, ldy) * 180 / math.Pi
	for deg < 0 {
		deg += 180
	}
	for deg >= 180 {
		deg -= 180
	}
	s.OrientationDeg = math.Round(deg*100) / 100
	return s
}

func convexHull(pts [][2]float64) [][2]float64 {
	n := len(pts)
	if n < 3 {
		return pts
	}
	p := append([][2]float64(nil), pts...)
	sort.Slice(p, func(i, j int) bool { return p[i][0] < p[j][0] || (p[i][0] == p[j][0] && p[i][1] < p[j][1]) })
	cross := func(o, a, b [2]float64) float64 { return (a[0]-o[0])*(b[1]-o[1]) - (a[1]-o[1])*(b[0]-o[0]) }
	h := make([][2]float64, 0, 2*n)
	for _, pt := range p {
		for len(h) >= 2 && cross(h[len(h)-2], h[len(h)-1], pt) <= 0 {
			h = h[:len(h)-1]
		}
		h = append(h, pt)
	}
	for i, t := n-2, len(h)+1; i >= 0; i-- {
		for len(h) >= t && cross(h[len(h)-2], h[len(h)-1], p[i]) <= 0 {
			h = h[:len(h)-1]
		}
		h = append(h, p[i])
	}
	return h[:len(h)-1]
}

func toMulti(g orb.Geometry) orb.MultiPolygon {
	switch v := g.(type) {
	case orb.Polygon:
		return orb.MultiPolygon{v}
	case orb.MultiPolygon:
		return v
	}
	return nil
}

func multiContains(mp orb.MultiPolygon, pt orb.Point) bool {
	for _, p := range mp {
		if planar.PolygonContains(p, pt) {
			return true
		}
	}
	return false
}

func round7(f float64) float64 { return math.Round(f*1e7) / 1e7 }
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	s := ""
	for i > 0 {
		s = string(rune('0'+i%10)) + s
		i /= 10
	}
	return s
}
