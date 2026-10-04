package bevdirect

import (
	"math"
	"testing"

	"github.com/paulmach/orb"
)

func TestAdmin(t *testing.T) {
	if k := KG("63349"); k == nil || k.GemeindeName != "Köflach" {
		t.Fatalf("kg: %+v", k)
	}
	if g := SearchGemeinden("koefl", 5); len(g) == 0 || g[0].Code != "61631" {
		t.Fatalf("search: %+v", g)
	}
}

func TestShape(t *testing.T) {
	// 20 m × 10 m rectangle, long axis E–W → orientation ≈ 90°
	r := orb.Ring{{15, 47}, {15.00026, 47}, {15.00026, 47.00009}, {15, 47.00009}, {15, 47}}
	s := shapeOf(r)
	if math.Abs(s.OBBLengthM-19.8) > 0.5 || math.Abs(s.OBBWidthM-10) > 0.5 || math.Abs(s.OrientationDeg-90) > 1 {
		t.Fatalf("%+v", s)
	}
}
