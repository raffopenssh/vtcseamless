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

## Updating the admin table

Twice a year BEV publishes a new VGD Stichtag (1 April / 1 October): run
`tools/build_admin.sh <YYYYMMDD> <file-date>`, commit `bevdirect/admin.json.gz`,
tag and release.
