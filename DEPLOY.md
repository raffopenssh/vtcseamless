# Deploying bevdirect-serve as a systemd service

`bevdirect-serve` is a single static binary. It needs outbound HTTPS to
the BEV tile host (`bevdirect.TileURL`) and nothing else — no database, no credentials, no other
upstream. The only state is in RAM: a bounded tile cache and an expiring cache
of assembled cells. **Nothing is written to disk** — no tile files, no database.

## One-line install

```
curl -fsSL https://raw.githubusercontent.com/raffopenssh/vtcseamless/main/bootstrap.sh \
  | PREFIX=/opt/bevdirect PORT=8787 bash
journalctl -u bevdirect-serve -f
```

`bootstrap.sh` downloads the latest GitHub Release tarball for the host
architecture (linux amd64/arm64) and falls back to a source build (installing
Go if missing). Pin a release with `VERSION=v0.3.0`; force a source build with
`FROM_SOURCE=1`. Releases: https://github.com/raffopenssh/vtcseamless/releases
(built by `tools/package.sh` + `tools/release.sh`).

Manual alternatives:

- unpack a release tarball and run `./install.sh` (same thing, no download);
- from a clone: `./install.sh` builds `cmd/bevdirect-serve` first;
- `go install github.com/raffopenssh/vtcseamless/cmd/bevdirect-serve@latest`
  and write your own unit;
- embed `bevdirect.NewService(...)` in your Go process (no HTTP hop at all).

`install.sh` fills the placeholders in `bevdirect-serve.service`
(`__PREFIX__`, `__PORT__`, `__USER__`), installs the unit, enables it and
checks `/health`. Override with `PREFIX=`, `PORT=`, `RUN_USER=`.

## Sizing

2 cores / 3 GB RAM (the unit sets `MemoryMax=3G`, `GOMEMLIMIT=2560MiB`):
160 cached cells ≈ 0.5–1 GB plus the in-memory tile cache, `-tile-cache-mb`
(unit: 1024). Sizing the tile cache: a dense town is ≈ 450 KB of tiles per km²
(parcels z15 + footprints/landuse z16), rural land far less; an average KG is
≈ 11 km², so **1 GiB holds the tiles of roughly 200 KGs of mixed terrain**
(≈ 100 dense-only). The cache is an LRU — beyond the budget the least recently
used tiles are refetched, nothing breaks. Tiles expire after `-tile-ttl`
(24 h) and are dropped by an hourly sweeper (`/health` → `tile_cache`,
`tile_cache_swept_at`), so nothing stale outlives a day; assembled cells expire
after `-ttl` (6 h). No disk is used; a restart starts cold.

Batch builders (e.g. `vtcseamless observe` over many KGs, the NE cell
baseline) only ever read tiles through this process's `/viewport` — keep **one**
bevdirect-serve running for the whole batch (the unit, or `Server` from the
Python package started once) so the tiles of neighbouring KGs stay hot.

## Operating

```
curl localhost:8787/health          # cells_cached, prefetch_queue, tile_cache{tiles,bytes,max_bytes,hits,misses}, admin_source
./bevdirect-serve -version          # binary version + admin table edition
systemctl restart bevdirect-serve   # state is RAM only; restart is free (cold first viewport ~10 s)
```

Flags (see `-h`): `-addr`, `-ttl`, `-tile-ttl`, `-tile-cache-mb`, `-cells`,
`-workers`, `-max-conns`, `-prefetch`. `-cache` is accepted and ignored
(pre-v0.3.1 clients still pass it).

## Response contract

`ready:false, pending:true, retry_after_s` on `/viewport?wait=0` means
*unknown, still assembling* — never "no parcels"; retry after
`retry_after_s`. `complete:false` on a parcel, footprint or landuse piece =
truncated at the fetched tile edge. Every response carries `notice` / `X-Data-Attribution`
(`© BEV, <year> … CC BY 4.0, bearbeitet`), which must be shown with the data.

## Versions

Every `/viewport` document and `/health` carry `bevdirect_version` (the git
tag the binary was built from, via `-ldflags -X main.version=`). Downstream
consumers that compare digests of assembled cells pin their source as
`bevdirect@<tag>` and only compare documents from the same tag, because the
assembly (seams, union guard) is part of the result.

- `v0.3.0` — first public release under `github.com/raffopenssh/vtcseamless`;
  cell output identical to the last pre-public build (verified byte-for-byte
  on five viewports, see README).
- `v0.3.2` — multi-cell `/viewport` no longer emits a clipped duplicate of a
  footprint that straddles a z16 tile edge inside a neighbour cell's pad
  (pieces now carry `complete` + `members`; compose dedups by member id,
  complete copy wins, then larger area). **Single-cell (aligned 0.02°)
  documents are unchanged apart from the two new fields**, so per-cell
  `ne_cells` digests from v0.3.0/v0.3.1 still match; only unaligned /
  multi-cell viewports change (they now equal the union of their cells —
  Guntrams 23308 viewport `16.1402,47.7068,16.168,47.728`: 431 → 419
  footprints). Builders that fed unaligned viewports to `ne_cells` must
  rebuild. Until a peer runs ≥ v0.3.2, feed `ne_cells` only aligned 0.02°
  cell requests (`west=ix*0.02, south=iy*0.02, +0.02`).
- `v0.3.3` — empty / unrequested layers are `[]`, never `null` (the frozen
  `ne_cells/canon.py` iterates every layer and crashed on cells without
  footprints). No geometry change. **`ne_cells`-comparable builds must use
  aligned 0.02° cells on EVERY version**, including ≥ v0.3.2: a multi-cell
  `/viewport` is the union of its cells *after* compose's parcel dedupe, which
  keeps ONE truncated copy of a parcel wider than cell + pad (`complete:false`
  in every cell — alpine parcels of several km²), whereas `ne_cells` unions all
  truncated copies from the aligned cells. Measured on 75110 (13 such parcels):
  0.045° windows lose 3 070 of 168 243 cells and shift 230 + 34 res-10 chunk
  digests; the operator's `/report` flags such builds `change_suspect:
  coverage_lossy` (cells_n below the best build seen for the bbox) and does
  not count their diffs as change.
- `v0.3.4` — **tile provenance, no assembly change.** Every `/viewport`
  document gains `tiles: [{z, x, y, sha256}]` — the z15 + z16 kataster tiles
  the answer was assembled from, sha256 over the raw pbf bytes as fetched
  (before decoding; a 204/404 tile hashes the empty string), sorted by
  (z, x, y); a multi-cell viewport lists the union of its cells' tiles. `stats`
  gains `tiles_expected` (tiles the padded bbox needs), `tiles_fetched` (bytes
  known: HTTP 200, or 204/404 = empty, counted again in `tiles_empty`); an
  incomplete fetch is `tiles_fetched < tiles_expected` (and `ready:false`).
  A cached cell keeps the hashes it was built with — the hash describes the
  bytes the assembly consumed, never a re-fetch. `TestAssemblyDeterministic`
  (CI) proves that identical tile bytes give byte-identical documents; the one
  map-order dependence it found (`dominant_ns` on an exact area tie) is now
  key-ordered. Geometry, areas, seams and dedupe are untouched: **documents
  and downstream digests from v0.3.4 are cell-equivalent to v0.3.0–v0.3.3**
  (the operator treats `bevdirect@*` as one source class). Consumers: a
  downstream digest that changes while the tile hashes of its cells do not is
  a pipeline defect, not land-use change; a build with `tiles_fetched <
  tiles_expected` is never change.
- `v0.3.6` — **aligned-cell lookup fix (srtm-lidar-at report, 2026-10-24).**
  `cellOf` floored `lon/0.02` without an epsilon, so for 1 214 of 18 000 columns
  (and as many rows) an exactly aligned request such as
  `west=15.54 … east=15.56` (15.54/0.02 = 776.999…) resolved to cells 776 AND
  777. compose then deduped across both ("complete wins, then larger area")
  and could keep the *neighbour* cell's truncated copy of a parcel wider than
  cell + pad — truncated inside the requested cell, and which copy won
  depended on the requester's cell block (12117 parcel 685: area 758 557 m²
  bbox E 15.5456543 via /viewport vs 719 269 m² / 15.5467808 in the cell's own
  fetch; `stats.tiles_z15` 30 instead of 12). Downstream `ne_cells` border
  rows therefore differed per KG (12117 chunk `8a1e33288007fff`: 40 vs 49
  cells). Fix: `cellOf` adds 1e-9 cell units before flooring;
  `TestCellsForAlignedIsSingleCell` asserts every aligned request maps to
  exactly one cell (also after 7-decimal client rounding). **Documents and
  digests of affected cells change once** (legitimate, not land-use change);
  unaffected cells are byte-identical. The operator keeps `bevdirect@v0.3.6`
  in the same source class; its per-chunk rule absorbs the one-time flip.

## Updating the admin table

Twice a year BEV publishes a new VGD Stichtag (1 April / 1 October): run
`tools/build_admin.sh <YYYYMMDD> <file-date>`, commit `bevdirect/admin.json.gz`,
tag and release.
