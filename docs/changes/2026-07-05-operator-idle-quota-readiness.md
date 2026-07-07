# Operator: idle auto-stop, quotas, startup/readiness timeouts, /readyz (ledger backfill)

- **Type:** feature + fix — retroactive ledger entry
- **Date:** 2026-07-05
- **Area:** `internal/operator` (idle stop, timeouts), `internal/api` (quotas, /readyz)
- **Commits:**
  - `91bfa27` feat(operator): idle auto-stop (§8)
  - `e574749` feat(api): enforce CPU/memory/storage quotas (§9.3, §22)
  - `7f7e459` fix(operator): enforce startup and readiness timeouts (§5, §8)
  - `7becb38` fix(api): implement `/readyz` with real DB + K8s API + CRD checks (§7)
- **Tasks:** §5/§7/§8/§9.3/§22 operator + resource-governance spec items

## What it did

Rounded out the operator's lifecycle governance: stop idle servers automatically, enforce
per-resource CPU/memory/storage quotas at claim/create, bound how long a server may sit in
startup/readiness before the operator gives up, and make `/readyz` a real dependency check
(DB, Kubernetes API, and the CRD) rather than a static 200.

## Why

An orchestrator that never reclaims idle capacity or bounds startup will accumulate stuck
and wasteful workloads; a `/readyz` that always returns 200 tells the load balancer a
broken control plane is healthy. These are the spec's resource-governance and
readiness-correctness requirements (§5/§7/§8/§9.3/§22).

> **Backfill note.** Reconstructed 2026-07-07 from the commit history. Covered by Go unit
> tests at each commit. Not independently re-verified for this doc; current tree green at
> `9911b8c` (WSL oracle, go1.26.4).
