# Deploying bevdirect-serve as a systemd service

`bevdirect-serve` is a single static binary. It needs outbound HTTPS to
`kataster.bev.gv.at` and nothing else — no database, no credentials, no other
upstream. The only state is the tile cache and an expiring in-memory cache of
assembled cells.

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

2 cores / 1 GB RAM is enough (the unit sets `MemoryMax=1G`; 160 cached cells
≈ 0.5–1 GB). Disk: the tile cache grows with the area served; 1 GB covers a
few hundred km². Tiles expire after `-tile-ttl` (unit: 24 h) and are
**deleted** by an hourly sweeper (`/health` → `tile_cache_swept_at`), so
nothing stale outlives a day; assembled cells expire after `-ttl` (6 h).

## Operating

```
curl localhost:8787/health          # cells_cached, prefetch_queue, tile_ttl_s, tile_cache_swept_at, admin_source
./bevdirect-serve -version          # binary version + admin table edition
systemctl restart bevdirect-serve   # state is only caches; restart is free (cold first viewport ~10 s)
```

Flags (see `-h`): `-addr`, `-cache`, `-ttl`, `-tile-ttl`, `-cells`, `-workers`,
`-max-conns`, `-prefetch`.

## Response contract

`ready:false, pending:true, retry_after_s` on `/viewport?wait=0` means
*unknown, still assembling* — never "no parcels"; retry after
`retry_after_s`. `complete:false` on a parcel = truncated at the fetched tile
edge. Every response carries `notice` / `X-Data-Attribution`
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

## Updating the admin table

Twice a year BEV publishes a new VGD Stichtag (1 April / 1 October): run
`tools/build_admin.sh <YYYYMMDD> <file-date>`, commit `bevdirect/admin.json.gz`,
tag and release.
