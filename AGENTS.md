# AGENTS.md — vtcseamless / bevdirect (read first; keep short)

Layout: root = seamless-polygon engine (`geom.go seams.go union.go`) · `bevdirect/` = the BEV
preset (tile fetch, assembly, service, in-memory caches) · `cmd/bevdirect` (one-shot CLI) ·
`cmd/bevdirect-serve` (HTTP service, flags in `main.go`) · `DEPLOY.md`, `bootstrap.sh`,
`install.sh` (static binary, no credentials, no disk state). Python side:
github.com/raffopenssh/vtcseamless-py (client + frozen `ne_cells` reference).
`go test ./...` is deterministic (`bevdirect/determinism_test.go`).

## Downstream findings (operator peer fleet, Oct 2026)

Peers run `bevdirect-serve` as a child process **inside the same memory cgroup** as the
caller's NE-cell builder; the builder's peak scales with KG size (~3.4 KB per res-12 cell,
5–6 GB for a 1.5 M-cell KG) and was OOM-killed with the default `-tile-cache-mb 1024`
`-cells 160` (≈ 1 GB + 160 × 3–6 MB) resident beside it. Consequences for this repo:

1. Document the service's resident budget as a formula in `DEPLOY.md` / `-help`
   (`tile-cache-mb + cells × ~5 MB + Go overhead`) so callers can size the cgroup; the Python
   side now builds huge KGs in tiles (see vtcseamless-py `AGENTS.md`) — nothing to change in the
   assembly, but keep `-tile-cache-mb` / `-cells` honoured strictly (hard caps, not hints).
2. A caller that keeps a fresh `.nec` re-fetches only the assembled cell documents
   (objects-only pass, no rebuild); make sure the 6 h cell cache / 24 h tile cache serve such a
   pass without re-downloading tiles when nothing expired, and expose a cheap
   `/health`-level counter (cells served from cache vs rebuilt) so peers can verify it.
3. Peers log `bevdirect_version` and the per-cell `tiles[{z,x,y,sha256}]` provenance with every
   build (needs ≥ v0.3.4); keep both stable across releases — the operator distinguishes
   "land changed" from "build changed" with them.
