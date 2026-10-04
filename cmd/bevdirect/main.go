// bevdirect — fetch Austrian cadastre for a bbox straight from BEV tiles.
//
//	bevdirect -bbox 15.08,47.04,15.10,47.06 -layers parcels,footprints -o out.json
//	bevdirect -bbox … -geojson > parcels.geojson
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/paulmach/orb/geojson"
	"github.com/raffopenssh/vtcseamless/bevdirect"
)

func main() {
	bbox := flag.String("bbox", "", "west,south,east,north (WGS84)")
	layers := flag.String("layers", "parcels", "comma list: parcels,footprints,landuse")
	out := flag.String("o", "", "output file (default stdout)")
	asGeoJSON := flag.Bool("geojson", false, "emit a FeatureCollection instead of the viewport JSON")
	cache := flag.String("cache", "", "tile cache dir (optional)")
	workers := flag.Int("workers", 6, "parallel tile downloads")
	maxTiles := flag.Int("max-tiles", 64, "refuse bboxes needing more z15 tiles")
	verbose := flag.Bool("v", false, "log tile errors")
	flag.Parse()

	parts := strings.Split(*bbox, ",")
	if len(parts) != 4 {
		log.Fatal("need -bbox west,south,east,north")
	}
	var f [4]float64
	for i, p := range parts {
		v, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil {
			log.Fatalf("bbox: %v", err)
		}
		f[i] = v
	}
	opts := bevdirect.Options{Layers: strings.Split(*layers, ","), Workers: *workers, CacheDir: *cache, CacheTTL: 24 * time.Hour, MaxTiles: *maxTiles}
	if *verbose {
		opts.Log = log.Printf
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	t0 := time.Now()
	res, err := bevdirect.Fetch(ctx, f[0], f[1], f[2], f[3], opts)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Fprintf(os.Stderr, "%s: %d parcels (%d incomplete), %d footprints, %d landuse · tiles z15=%d z16=%d err=%d cache=%d · %.0f KB · dl %.1fs asm %.2fs · total %.1fs · guard trips %d\n",
		res.Source, len(res.Parcels), countIncomplete(res), len(res.Footprints), len(res.Landuse),
		res.Stats.TilesZ15, res.Stats.TilesZ16, res.Stats.TileErrors, res.Stats.CacheHits, float64(res.Stats.Bytes)/1024,
		res.Stats.Download.Seconds(), res.Stats.Assembly.Seconds(), time.Since(t0).Seconds(), res.Stats.GuardTrips)

	var payload any = res
	if *asGeoJSON {
		fc := geojson.NewFeatureCollection()
		for _, p := range res.Parcels {
			ft := geojson.NewFeature(p.Geometry.Geometry())
			ft.Properties = geojson.Properties{"layer": "parcel", "parcel_id": p.ParcelID, "kg_code": p.KGCode, "gnr": p.GNR, "ez": p.EZ,
				"rstatus": p.Status, "area_sqm": p.AreaSqm, "complete": p.Complete, "attribution": res.Notice}
			fc.Append(ft)
		}
		for _, lst := range [][]bevdirect.Piece{res.Footprints, res.Landuse} {
			for _, p := range lst {
				ft := geojson.NewFeature(p.Geometry.Geometry())
				layer := "landuse"
				if p.NS == 41 {
					layer = "building_footprint"
				}
				ft.Properties = geojson.Properties{"layer": layer, "ns": p.NS, "area_sqm": p.AreaSqm, "tile": p.Tile, "attribution": res.Notice}
				fc.Append(ft)
			}
		}
		fc.ExtraMembers = map[string]any{"attribution": res.Notice, "license": res.LicenseURL, "source": res.Source, "ready": res.Ready}
		payload = fc
	}
	w := os.Stdout
	if *out != "" {
		fh, err := os.Create(*out)
		if err != nil {
			log.Fatal(err)
		}
		defer fh.Close()
		w = fh
	}
	enc := json.NewEncoder(w)
	if err := enc.Encode(payload); err != nil {
		log.Fatal(err)
	}
}

func countIncomplete(r *bevdirect.Result) int {
	n := 0
	for _, p := range r.Parcels {
		if !p.Complete {
			n++
		}
	}
	return n
}
