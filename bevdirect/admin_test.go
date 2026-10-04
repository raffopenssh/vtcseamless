package bevdirect

import "testing"

func TestAdminTable(t *testing.T) {
	loadAdmin()
	if len(kgByCode) != 7850 {
		t.Fatalf("expected 7850 KGs, got %d", len(kgByCode))
	}
	if AdminSource() == "" || !contains(AdminSource(), "Verwaltungsgrenzen") {
		t.Fatalf("admin source missing: %q", AdminSource())
	}
	k := KG("63349")
	if k == nil || k.KGName != "Piber" || k.GemeindeName != "Köflach" || k.State != "Steiermark" {
		t.Fatalf("63349: %+v", k)
	}
	if k.MinLon < 15.0 || k.MaxLon > 15.2 || k.MinLat < 47.0 || k.MaxLat > 47.1 || k.AreaSqkm < 4 || k.AreaSqkm > 8 {
		t.Fatalf("63349 bbox/area off: %+v", k)
	}
	g := SearchGemeinden("koeflach", 5)
	if len(g) == 0 || g[0].Code != "61631" || len(g[0].KGs) < 3 || g[0].AreaSqkm <= k.AreaSqkm {
		t.Fatalf("Köflach: %+v", g)
	}
	for _, r := range kgByCode {
		if len(r.KGCode) != 5 || r.MinLon >= r.MaxLon || r.MinLat >= r.MaxLat || r.AreaSqkm <= 0 || r.GemeindeCode == "" {
			t.Fatalf("bad row %+v", r)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool { return indexOf(s, sub) >= 0 })()
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
