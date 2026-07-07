# felis_* Prometheus metrics (§23) (ledger backfill)

- **Type:** feature — retroactive ledger entry
- **Date:** 2026-06-30
- **Area:** `internal/metrics` + the emit sites in build, platform/fleet, and the start lifecycle
- **Commits:**
  - `75642d9` feat(metrics): named `felis_*` Prometheus collectors
  - `2a93a9e` feat(metrics): record `felis_image_build_failures_total` on failed builds
  - `79eae7f` feat(metrics): publish `felis_servers_total` from a fleet snapshot
  - `8ac5e64` feat(metrics): observe `felis_start_duration_seconds` across the start lifecycle
- **Tasks:** #17 (§23 felis_* metrics decision)

## What it did

Added the named `felis_*` collector set and wired the three emit points that make it
non-empty: a counter incremented on image-build failure, a gauge published from a fleet
snapshot, and a histogram observed across the server start lifecycle.

## Why

§23 calls for first-class operational metrics under a stable `felis_` namespace rather than
ad-hoc logging, so an operator can alert on build failures, fleet size, and start latency.

> **Backfill note.** Reconstructed 2026-07-07 from the commit history. Not independently
> re-verified for this doc; current tree green at `9911b8c` (WSL oracle, go1.26.4).
