package bevdirect

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Handler exposes the Service over HTTP (what bevdirect-serve runs):
//
//	GET /viewport?west&south&east&north[&layers=parcels,footprints,landuse]
//	GET /parcel/{id}?lon&lat            id = 63349-348/6; resolved from the tiles around lon/lat
//	GET /ez?kg&ez&lon&lat | &west..     parcels of that folio within the tiles around the location (partial)
//	GET /municipality?lon&lat           KG of the parcel under the point → Gemeinde (static table)
//	GET /municipalities?q=              picker
//	GET /kg/{kg}                        admin row + bbox
//	GET /health
//
// Responses are gzipped when accepted and carry X-Data-Attribution + CORS *.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /viewport", s.handleViewport)
	mux.HandleFunc("GET /parcel/{id...}", s.handleParcel)
	mux.HandleFunc("GET /ez", s.handleEZ)
	mux.HandleFunc("GET /municipality", s.handleMunicipality)
	mux.HandleFunc("GET /municipalities", func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 {
			limit = 20
		}
		writeJSON(w, 200, map[string]any{"results": SearchGemeinden(r.URL.Query().Get("q"), limit), "notice": Notice()})
	})
	mux.HandleFunc("GET /kg/{kg}", func(w http.ResponseWriter, r *http.Request) {
		if k := KG(Pad5(r.PathValue("kg"))); k != nil {
			writeJSON(w, 200, map[string]any{"kg": k, "notice": Notice()})
			return
		}
		writeJSON(w, 404, map[string]any{"error": "unknown kg"})
	})
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "cells_cached": s.CellsCached(), "prefetch_queue": s.PrefetchQueued(), "tile_ttl_s": int(s.o.TileTTL.Seconds()), "tile_cache_swept_at": s.TileCacheSweptAt(), "source": "bev-direct", "bevdirect_version": Version, "admin_source": AdminSource(), "notice": Notice()})
	})
	return gzipCORS(mux)
}

func parseBBox(q url.Values) (w, s, e, n float64, err error) {
	f := func(k string) (float64, error) { return strconv.ParseFloat(q.Get(k), 64) }
	if w, err = f("west"); err != nil {
		return
	}
	if s, err = f("south"); err != nil {
		return
	}
	if e, err = f("east"); err != nil {
		return
	}
	if n, err = f("north"); err != nil {
		return
	}
	err = checkBBox(w, s, e, n)
	return
}

func parsePoint(q url.Values) (lon, lat float64, ok bool) {
	lon, e1 := strconv.ParseFloat(q.Get("lon"), 64)
	lat, e2 := strconv.ParseFloat(q.Get("lat"), 64)
	return lon, lat, e1 == nil && e2 == nil
}

func (s *Service) handleViewport(w http.ResponseWriter, r *http.Request) {
	west, south, east, north, err := parseBBox(r.URL.Query())
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	want := map[string]bool{}
	for _, l := range AllLayers {
		want[l] = true
	}
	if l := r.URL.Query().Get("layers"); l != "" {
		want = map[string]bool{}
		for _, x := range strings.Split(l, ",") {
			want[x] = true
		}
	}
	ctx, cancel := reqCtx(r, 60*time.Second)
	defer cancel()
	if ws := r.URL.Query().Get("wait"); ws != "" { // seconds; 0 = answer from cache immediately
		if sec, err := strconv.ParseFloat(ws, 64); err == nil {
			var c context.CancelFunc
			ctx, c = context.WithTimeout(ctx, time.Duration(sec*float64(time.Second))+50*time.Millisecond)
			defer c()
		}
	}
	t0 := time.Now()
	res, err := s.Viewport(ctx, west, south, east, north)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error(), "ready": false, "pending": true})
		return
	}
	if !want["parcels"] {
		res.Parcels = nil
	}
	if !want["footprints"] {
		res.Footprints = nil
	}
	if !want["landuse"] {
		res.Landuse = nil
	}
	w.Header().Set("X-Data-Attribution", res.Notice)
	body := map[string]any{
		"parcels": res.Parcels, "footprints": res.Footprints, "landuse": res.Landuse,
		"ready": res.Ready, "truncated": false, "count": len(res.Parcels), "source": res.Source,
		"bevdirect_version": Version, "coord_decimals": 7,
		"notice": res.Notice, "license_url": res.LicenseURL, "fetched_at": res.FetchedAt, "stats": res.Stats,
		"query_time_ms": time.Since(t0).Milliseconds(),
	}
	if !res.Ready {
		body["pending"], body["retry_after_s"] = true, 3
		w.Header().Set("Retry-After", "3")
		w.Header().Set("Cache-Control", "no-store")
	}
	writeJSON(w, 200, body)
}

func (s *Service) handleParcel(w http.ResponseWriter, r *http.Request) {
	id, _ := url.PathUnescape(r.PathValue("id"))
	if i := strings.Index(id, "-"); i > 0 {
		id = Pad5(id[:i]) + id[i:]
	}
	lon, lat, ok := parsePoint(r.URL.Query())
	if !ok {
		writeJSON(w, 400, map[string]any{"error": "lon,lat required: parcels are resolved from the tiles around a location, nothing is stored per parcel"})
		return
	}
	ctx, cancel := reqCtx(r, 30*time.Second)
	defer cancel()
	p, err := s.ParcelAt(ctx, id, lon, lat)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error(), "pending": true})
		return
	}
	if p == nil {
		writeJSON(w, 404, map[string]any{"error": "no such parcel near lon/lat", "matched": false})
		return
	}
	writeJSON(w, 200, map[string]any{"parcel": p, "from": "bev", "notice": Notice()})
}

func (s *Service) handleEZ(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	kg, ez := Pad5(q.Get("kg")), strings.TrimSpace(q.Get("ez"))
	bw, bs, be, bn, err := parseBBox(q)
	if err != nil {
		lon, lat, ok := parsePoint(q)
		if !ok {
			writeJSON(w, 400, map[string]any{"error": "lon,lat (or west,south,east,north) required: folios are resolved from the tiles around a location"})
			return
		}
		const d = 0.006
		bw, bs, be, bn = lon-d, lat-d, lon+d, lat+d
	}
	ctx, cancel := reqCtx(r, 30*time.Second)
	defer cancel()
	parcels, err := s.EZIn(ctx, kg, ez, bw, bs, be, bn)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error(), "pending": true})
		return
	}
	total := 0.0
	for _, p := range parcels {
		total += p.AreaSqm
	}
	writeJSON(w, 200, map[string]any{"kg_code": kg, "ez": ez, "parcels": parcels, "parcel_count": len(parcels),
		"total_area_sqm": total, "partial": true, "bbox": []float64{bw, bs, be, bn},
		"note": "only parcels of the folio within the fetched tiles; the folio may own land elsewhere", "notice": Notice()})
}

func (s *Service) handleMunicipality(w http.ResponseWriter, r *http.Request) {
	lon, lat, ok := parsePoint(r.URL.Query())
	if !ok {
		writeJSON(w, 400, map[string]any{"error": "lon,lat required"})
		return
	}
	ctx, cancel := reqCtx(r, 30*time.Second)
	defer cancel()
	kg, err := s.KGAt(ctx, lon, lat)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error(), "pending": true})
		return
	}
	k := KG(kg)
	if k == nil {
		writeJSON(w, 404, map[string]any{"error": "no cadastre at point", "matched": false})
		return
	}
	writeJSON(w, 200, map[string]any{"kg": k, "gemeinde": GemeindeByCode(k.GemeindeCode), "notice": Notice()})
}

func reqCtx(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

// Pad5 zero-pads a numeric KG code to 5 digits.
func Pad5(s string) string {
	s = strings.TrimSpace(s)
	if n, err := strconv.Atoi(s); err == nil && len(s) < 5 {
		return fmt.Sprintf("%05d", n)
	}
	return s
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	var out io.Writer = w
	if gz, ok := w.(*gzipWriter); ok && code == 200 {
		gz.used = true
		out = gz.gz
	}
	_ = json.NewEncoder(out).Encode(v)
}

type gzipWriter struct {
	http.ResponseWriter
	gz   *gzip.Writer
	used bool
}

func (g *gzipWriter) WriteHeader(code int) {
	if code == 200 {
		g.Header().Set("Content-Encoding", "gzip")
		g.Header().Del("Content-Length")
	}
	g.ResponseWriter.WriteHeader(code)
}

func gzipCORS(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Vary", "Accept-Encoding")
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			h.ServeHTTP(w, r)
			return
		}
		gz, _ := gzip.NewWriterLevel(w, gzip.BestSpeed)
		gw := &gzipWriter{ResponseWriter: w, gz: gz}
		h.ServeHTTP(gw, r)
		if gw.used {
			_ = gz.Close()
		}
	})
}
