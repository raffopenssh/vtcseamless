package bevdirect

// Static administrative table (KG → Gemeinde/Bezirk/Bundesland + KG bbox + area),
// 7 850 rows built by tools/build_admin.sh from BEV OGD "Verwaltungsgrenzen (VGD)
// 1:50 000" (CC BY 4.0, data.bev.gv.at) — nothing in it derives from the
// cadastre tiles or any cadastre index. It only tells the service where a
// KG / Gemeinde is so it knows which tiles to fetch; AdminSource() names the
// Stichtag.

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/json"
	"math"
	"sort"
	"strings"
	"sync"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

//go:embed admin.json.gz
var adminGz []byte

type KGInfo struct {
	KGCode       string  `json:"kg_code"`
	KGName       string  `json:"kg_name"`
	GemeindeCode string  `json:"gemeinde_code"`
	GemeindeName string  `json:"gemeinde_name"`
	District     string  `json:"district_name"`
	State        string  `json:"state_name"`
	MinLon       float64 `json:"min_lon"`
	MinLat       float64 `json:"min_lat"`
	MaxLon       float64 `json:"max_lon"`
	MaxLat       float64 `json:"max_lat"`
	AreaSqkm     float64 `json:"area_sqkm"`
}

type Gemeinde struct {
	Code     string   `json:"gemeinde_code"`
	Name     string   `json:"gemeinde_name"`
	District string   `json:"district_name"`
	State    string   `json:"state_name"`
	KGs      []string `json:"kg_codes"`
	MinLon   float64  `json:"min_lon"`
	MinLat   float64  `json:"min_lat"`
	MaxLon   float64  `json:"max_lon"`
	MaxLat   float64  `json:"max_lat"`
	Lon      float64  `json:"lon"`
	Lat      float64  `json:"lat"`
	AreaSqkm float64  `json:"area_sqkm"`
}

var (
	adminOnce sync.Once
	kgByCode  map[string]*KGInfo
	gemeinden []*Gemeinde
	gemByCode map[string]*Gemeinde
	adminSrc  string
)

// AdminSource names the BEV VGD edition the embedded table was built from.
func AdminSource() string { loadAdmin(); return adminSrc }

func loadAdmin() {
	adminOnce.Do(func() {
		zr, err := gzip.NewReader(bytes.NewReader(adminGz))
		if err != nil {
			panic(err)
		}
		var doc struct {
			Source string    `json:"source"`
			KGs    []*KGInfo `json:"kgs"`
		}
		if err := json.NewDecoder(zr).Decode(&doc); err != nil {
			panic(err)
		}
		rows := doc.KGs
		adminSrc = doc.Source
		kgByCode = make(map[string]*KGInfo, len(rows))
		gemByCode = map[string]*Gemeinde{}
		for _, r := range rows {
			kgByCode[r.KGCode] = r
			g := gemByCode[r.GemeindeCode]
			if g == nil {
				g = &Gemeinde{Code: r.GemeindeCode, Name: r.GemeindeName, District: r.District, State: r.State, MinLon: 999, MinLat: 999, MaxLon: -999, MaxLat: -999}
				gemByCode[r.GemeindeCode] = g
				gemeinden = append(gemeinden, g)
			}
			g.KGs = append(g.KGs, r.KGCode)
			g.AreaSqkm += r.AreaSqkm
			if r.MinLon > 0 {
				g.MinLon, g.MinLat = minf(g.MinLon, r.MinLon), minf(g.MinLat, r.MinLat)
				g.MaxLon, g.MaxLat = maxf(g.MaxLon, r.MaxLon), maxf(g.MaxLat, r.MaxLat)
			}
		}
		for _, g := range gemeinden {
			g.Lon, g.Lat = (g.MinLon+g.MaxLon)/2, (g.MinLat+g.MaxLat)/2
			g.AreaSqkm = math.Round(g.AreaSqkm*1000) / 1000
		}
		sort.Slice(gemeinden, func(i, j int) bool { return gemeinden[i].Name < gemeinden[j].Name })
	})
}

// KG returns the admin row for a 5-digit KG code.
func KG(code string) *KGInfo { loadAdmin(); return kgByCode[code] }

// GemeindeByCode returns a Gemeinde by its Gemeindekennziffer.
func GemeindeByCode(code string) *Gemeinde { loadAdmin(); return gemByCode[code] }

// SearchGemeinden is a diacritics-insensitive substring search (prefix first).
func SearchGemeinden(q string, limit int) []*Gemeinde {
	loadAdmin()
	q = fold(q)
	if q == "" {
		return nil
	}
	var pre, sub []*Gemeinde
	for _, g := range gemeinden {
		n := fold(g.Name)
		switch {
		case strings.HasPrefix(n, q):
			pre = append(pre, g)
		case strings.Contains(n, q):
			sub = append(sub, g)
		}
	}
	out := append(pre, sub...)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

var folder = transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)

func fold(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.NewReplacer("ß", "ss", "ä", "ae", "ö", "oe", "ü", "ue").Replace(s)
	if f, _, err := transform.String(folder, s); err == nil {
		s = f
	}
	return s
}

func minf(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
