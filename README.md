# vtcseamless — seamless polygons from a vector-tile cache

Vector-tile servers clip every feature to the tile it is drawn in — and,
because renderers would otherwise show hairline gaps at tile edges, they clip
**with a buffer** of a few pixels. For drawing that is right; for *data* it is
wrong: the per-tile copies of one polygon overlap, their areas double-count
along every tile edge, and a naive union of the copies has the wrong area
wherever the buffer overlapped a neighbour. `vtcseamless` is the small Go
engine that turns such tiles back into seamless, correctly-nested polygons:
each piece is clipped back to its **own** tile rectangle (tiles tessellate ⇒
pieces are disjoint ⇒ areas add), the pieces of one feature id are unioned
with polyclip, the union is **rejected** if its area disagrees with the
additive area of the pieces by more than 2 % (then the pieces are kept, a
seam remains, nothing is invented), and rings are classified by nesting depth
(shell CCW / holes CW, RFC 7946). For layers without a stable feature id, a
seam merge joins pieces from adjacent tiles that share a stretch of the common
edge. The root package is generic; `bevdirect/` is the preset for the Austrian
cadastre tiles and the reason the engine exists.

Go 1.26, deps MIT/BSD (polyclip-go, orb, x/sync, x/text). Code: MIT. Data:
see [Data licence](#data-licence).

```go
import "github.com/raffopenssh/vtcseamless"

tiles := vtcseamless.TilesForBound(bbox, 15)
// for each tile t and each decoded polygon poly of feature id:
pieces[id] = append(pieces[id], vtcseamless.ClipToBound(poly, t.Bound())...)
// then per feature:
mp, guardTripped := vtcseamless.UnionPieces(pieces[id], sumOfPieceAreas)
```

| function | step |
|---|---|
| `TileAt`, `TilesForBound`, `TileSetBound`, `Tile.Bound` | slippy-map tile maths |
| `ClipToBound` (`RectClip` + `FromPolyclip` / `ToPolyclip`) | own-tile clip (Sutherland–Hodgman) |
| `UnionPieces`, `UnionAreaTol` | polyclip union of disjoint pieces with the > 2 % area-mismatch rejection |
| `ClassifyRings` | shell/hole by nesting depth, RFC 7946 orientation |
| `MergeSeams` | seams in id-less layers (adjacent tiles, shared edge stretch, union must dissolve the seam) |
| `AreaSqm`, `RoundCoords`, `ToPolygons` | helpers |

---

# bevdirect — the BEV preset: cadastre straight from the BEV tiles

Go package `github.com/raffopenssh/vtcseamless/bevdirect` + CLI that
assembles parcels / building footprints / land use for a bbox **directly from
the BEV Katastralmappe vector tile cache (VTC, CC BY 4.0)**. It serves as a provenance
proof (everything a client shows can be re-derived from the public tiles with
this code) and as a self-hosted cadastre service for map clients. See
[DEPLOY.md](DEPLOY.md) for running it as a systemd service.

```
go run ./cmd/bevdirect -bbox W,S,E,N -layers parcels,footprints,landuse -cache ./bevcache -o vp.json
go run ./cmd/bevdirect -bbox … -geojson > out.geojson
```

```go
res, err := bevdirect.Fetch(ctx, west, south, east, north, bevdirect.Options{
    Layers: []string{"parcels", "footprints"}, CacheDir: "./bevcache", CacheTTL: 24 * time.Hour})
// res.Parcels[i] = {parcel_id, kg_code, gnr, ez, rstatus, area_sqm, lon, lat, complete, geometry}
// res.Notice must be displayed with the data („© BEV, 2026 … CC BY 4.0, bearbeitet“).
```

## Measured (one dense small-town 0.02° × 0.02° viewport tile ≈ 1.5 × 2.2 km)

| | tiles | bytes | cold | warm (disk cache) |
|---|---|---|---|---|
| parcels (z15 `gst`) | 8 | 605 KB | ~1.5 s | 40 ms |
| + footprints + landuse (z16 `nfl`) | +28 | 1.5 MB | 2.6 s total | 100 ms |

3 288 parcels, 2 686 footprint pieces (2 471 after seam merging), 3 739
landuse pieces. Compared with an independent assembly of the same BEV data
for the same bbox: 2 484 ids in common, EZ agrees on all but 9, area agrees
within ±2 % for ~93 %; the outliers were ring-0-only slivers on the reference
side (one case: reference 88 m², bevdirect 848 m² = the real ring with its
hole). bevdirect uses the guarded union for every KG.

BEV serves `Cache-Control: no-cache`, 1.1–1.4 s per tile; parallelism (default
6) is what makes it tolerable. Be polite: keep `Workers ≤ 8`, use `CacheDir`.

## How it works (the part that is easy to get wrong)

BEV clips every polygon to its tile **with a large buffer**, so the per-tile
copies of one parcel overlap. Each piece is clipped back to its own tile
rectangle (Sutherland–Hodgman), pieces of one `kg-gnr` are unioned with
polyclip, and the union is rejected (pieces kept, seam along the tile edge) if
its area disagrees with the additive area of the disjoint pieces by > 2 %.
Rings are classified by nesting depth (shell CCW / holes CW, RFC 7946). That
is exactly the root package; `bevdirect` adds the download/cache, MVT
decoding and the `gst`/`nfl` layer conventions.

`complete:false` = the parcel touches the edge of the fetched tile set, so
geometry and `area_sqm` are truncated. Enlarge the bbox or treat as unknown.
`area_sqm` is planar on a local tangent plane with ellipsoidal metres/degree
(<0.1 %) — a geometric area, not the legally binding Grundstücksdatenbank area.

## bevdirect-serve / `bevdirect.Service`

`go build -o bevdirect-serve ./cmd/bevdirect-serve && ./bevdirect-serve -addr :8787 -cache ./bevcache`,
or `tools/package.sh` → static tarball + `install.sh` (system unit) — see
**DEPLOY.md**. Or embed it: a Go client calls
`bevdirect.NewService(...).Viewport(ctx, w,s,e,n)` in-process — no HTTP hop.

| endpoint | notes |
|---|---|
| `GET /viewport?west&south&east&north[&layers][&wait=s]` | parcels carry `ez, area_sqm, dominant_ns, landuse_areas{ns:m²}, building_count, total_building_area_sqm, complete`; footprints `parcel_id, obb_length_m, obb_width_m, orientation_deg, ns_code`. bbox ≤ 0.045°. `wait=0` answers from cache at once with `ready:false, pending:true, retry_after_s` for cells still assembling. |
| `GET /parcel/{id}?lon&lat` | resolved from the tiles around the location — **lon/lat is mandatory**, there is no parcel store to ask |
| `GET /ez?kg&ez&lon&lat` (or `&west..north`) | parcels of that folio **inside the given area** only, `partial:true` |
| `GET /municipality?lon&lat` | KG of the nearest parcel → Gemeinde |
| `GET /municipalities?q=` | picker; diacritics-insensitive, bbox + centre |
| `GET /kg/{kg}` | admin row + bbox |
| `GET /health` | `cells_cached, prefetch_queue, tile_ttl_s, tile_cache_swept_at, admin_source, bevdirect_version` |

**How it stays fast without any per-parcel state.** The world is a fixed
0.02° grid. Each cell is assembled once from its tiles (padded 0.004° so
parcels crossing the cell edge are complete in at least one cell) and cached
under `sha256(cell)` for 6 h, LRU-bounded (~3–6 MB per cell). A viewport, a
click (`/parcel`), a folio (`/ez`) and the background prefetch all compose the
*same* cells, so a pan hits cells that were warmed by the previous pan: after
every viewport the ring of neighbouring cells is queued (2 background workers
that yield to foreground requests). Tile downloads are coalesced process-wide
(singleflight + disk cache), total BEV connections are capped (24; BEV
latency is flat up to there), and CPU-heavy assembly is gated to one per core
so the nearest cell finishes first. Enrichment clips land-use pieces to the
parcel bbox before polyclip and runs parcels in parallel (1.4 s → 0.8 s per
dense cell).

Measured, initial load of 12 unaligned 0.02° tiles (4-wide, a dense town
centre, 9 700 parcels), 2 cores:

| | |
|---|---|
| cold (nothing cached, 9 cells) | 14–15 s total; first tile ~10 s; CPU-bound (polyclip) |
| pan one tile after ~10 s (prefetched ring) | 0.1–1 s |
| revisit / warm | 80–100 ms, ~500 KB gzipped |
| sparse alpine cell (1 300 parcels) cold | ~3–5 s |

Non-blocking use (`wait=0` / `ServiceOptions.Wait`) hands the client the
cached part immediately and `ready:false` for the rest; the client retries.

**Compliance posture (why cells, and why nothing per parcel).** The service
holds no database of the cadastre and nothing is keyed by parcel id, EZ or KG.
It does exactly what the BEV web map does — fetch the tiles for what is on
screen — and keeps (a) the tile cache (expired tiles are deleted hourly,
`-tile-ttl` 24 h) and (b) an expiring cache of assembled tile areas keyed by
their coordinates (`-ttl` 6 h). A parcel can only be obtained by
asking for a location, a folio only for the part inside a bbox; nothing is
enumerable or searchable. Every response carries the CC BY notice with year.
That is the footprint of a browser cache of the BEV web map, well inside
the VTC licence, and it never touches the Katasterservice JSON API (§76c
UrhG, not CC BY). The embedded KG→Gemeinde table (names, codes, bboxes,
areas) is built by `tools/build_admin.sh` from BEV OGD *Verwaltungsgrenzen
(VGD) 1:50 000* (CC BY 4.0), so **nothing in this module derives from any
cadastre index**; `/health` reports the VGD Stichtag as `admin_source`.
Not covered: land prices, protected areas, OSM, LiDAR, address search — those
are other upstreams.

## What you don't get from the library alone

With `Options.Enrich` the library computes `landuse_areas` / `dominant_ns`,
`building_count` and footprint shapes itself. Not derivable from tiles at all:
whole-KG EZ summaries, address search, land prices, protected areas, OSM
proximity, toponyms, LiDAR. Whole-KG fetches are expensive (a KG is 20–200
z15 tiles; `MaxTiles` refuses above 64 by default).

## Versioning

`bevdirect.Version` (set by `-ldflags -X main.version=<tag>` in
`tools/package.sh` / `bootstrap.sh`) is reported as `bevdirect_version` in
every `/viewport` document and `/health`. Downstream digest comparisons pin
their source as the string `bevdirect@<Version>`; this string and the package
name are a stable contract. `v0.3.0` is the first tag under this module path;
its output is byte-identical to the last pre-public build (checked on five
viewports, 25 672 parcels / 13 992 footprints / 25 248 land-use pieces, with
and without `Enrich`).

## Data licence

- **Code**: MIT (see [LICENSE](LICENSE)).
- **Tiles fetched at runtime**: BEV *Katastralmappe* vector tile cache (VTC),
  **CC BY 4.0** (the tile host is the one the BEV web map uses; see
  `bevdirect.TileURL`). BEV Nutzungsbedingungen §2.3.3 require
  „© BEV, JJJJ“ on every copy; `Result.Notice` / `X-Data-Attribution` carry
  the string and **must be rendered with the data**. Nothing from the tiles
  is stored in this repository.
- **`bevdirect/admin.json.gz`** (240 KB, the only data file in the repo): the
  KG → Gemeinde/Bezirk/Bundesland table with KG bboxes and areas, derived
  from BEV OGD *Verwaltungsgrenzen (VGD) 1:50 000*, **Stichtag 2026-04-01,
  CC BY 4.0** (`data.bev.gv.at`), by `tools/build_admin.sh` — reproducible
  from that script and the public download. `/health` → `admin_source` names
  the edition.
- **Never point this at the BEV Katasterservice search/info JSON API** — that
  one is not CC BY (§76c UrhG, database right). Only the tile cache is used.
