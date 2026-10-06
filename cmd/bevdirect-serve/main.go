// bevdirect-serve — a small cadastre service assembled live from BEV vector tiles.
//
// Thin HTTP wrapper around bevdirect.Service: everything is answered from BEV
// tiles fetched for a fixed grid of cells (plus a static KG→Gemeinde table).
// The only state is the tile cache and an expiring cache of assembled cells;
// nothing is keyed by parcel, EZ or KG. See bevdirect.Service / Handler.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/raffopenssh/vtcseamless/bevdirect"
)

var version = "dev"

func main() {
	addr := flag.String("addr", ":8787", "listen address")
	cache := flag.String("cache", "", "deprecated, ignored: tiles are cached in memory only, never on disk")
	tileMB := flag.Int("tile-cache-mb", 1024, "in-memory raw-tile LRU budget in MiB (0 = none); 1024 ≈ 200 KGs of mixed terrain")
	ttl := flag.Duration("ttl", 6*time.Hour, "assembled-cell cache TTL")
	tileTTL := flag.Duration("tile-ttl", 24*time.Hour, "in-memory tile TTL; expired tiles are dropped hourly (0 = keep until evicted)")
	cells := flag.Int("cells", 160, "max assembled cells kept in memory (~3–6 MB each)")
	workers := flag.Int("workers", 16, "parallel BEV tile downloads per cell")
	conns := flag.Int("max-conns", 24, "total concurrent connections to BEV across all cells")
	prefetch := flag.Int("prefetch", 1, "ring of neighbouring cells to warm in the background (0 = off)")
	showVer := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	bevdirect.Version = version
	if *showVer {
		fmt.Println("bevdirect-serve", version, "— admin table:", bevdirect.AdminSource())
		return
	}
	if *cache != "" {
		log.Printf("-cache %q ignored: tiles are kept in memory only (see -tile-cache-mb)", *cache)
	}
	tileBytes := int64(*tileMB) << 20
	if tileBytes == 0 {
		tileBytes = -1
	}
	svc := bevdirect.NewService(bevdirect.ServiceOptions{TileCacheBytes: tileBytes, CellTTL: *ttl, TileTTL: *tileTTL, MaxCells: *cells,
		Workers: *workers, MaxConns: *conns, Prefetch: *prefetch, Log: log.Printf})
	log.Printf("bevdirect-serve %s on %s (tile cache %d MiB in memory, cell ttl %s, tile ttl %s, cells %d; %s)", version, *addr, *tileMB, *ttl, *tileTTL, *cells, bevdirect.AdminSource())
	log.Fatal(http.ListenAndServe(*addr, svc.Handler()))
}
