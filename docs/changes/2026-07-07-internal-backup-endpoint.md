# Internal-face break-glass world backup endpoint (§B4 "Sync", phase 2a)

- **Type:** feature (addition)
- **Date:** 2026-07-07
- **Area:** `internal/api`, `docs/openapi.yaml` — Go, oracle-verified
- **Commit:** `f2fc57c`
- **Task:** #31 Phase B4 break-glass ops — the "Sync" operation. Per the user's
  **"两者都要"** decision the feature is built in two halves: the felis-api endpoint
  that does the real backup-Job orchestration (phase 1, `7a7c0d5`) and a break-glass
  menu peer that calls it while the API is alive (phase 2b, follow-up). **This change
  is phase 2a: the second, internal face of that endpoint** — the door the console
  peer will knock on.

## What it does

Adds `POST /api/v1/internal/servers/{name}/backup`, an **internal-face** twin of the
external `POST /api/v1/servers/{name}/backup`. The on-node break-glass console (root
on the host, holding the service token) POSTs here to snapshot a stopped world while
felis-api is alive. Same 202 `backing_up` / 409 `not_stopped` / 503
`backup_unavailable` / 404 / 400 `bad_name` surface as the external face.

## Why

The console cannot render the backup Job itself: it lacks the deployment coordinates
(`FELIS_IMAGE`, `FELIS_BACKUP_PVC`) that only felis-api holds — the same reason the
endpoint exists at all (phase 1). But the external face requires a Cloudflare-Access
Principal the console does not have. The internal face authenticates with the service
token (a trusted machine caller, no Principal), so the console can reach the same
orchestration without a browser session.

## Design decisions

- **No owner gate on the internal face.** The external handler enforces owner-or-admin
  from the Principal; the internal handler has none — the service token IS the
  authorization (the operator already has root on the node), so a server owned by
  someone else still backs up. This mirrors how the other internal-face handlers
  (op-login approve, QR poll) trust the token rather than a Principal.
- **Shared `enqueueBackup` tail.** The RWO stopped-gate, the optional-Backuper 503,
  the async hand-off, and the audit+202 were refactored out of `handleBackupNow` into
  a single `enqueueBackup(w, r, name, rec, actor, source)` that both faces call. The
  two faces differ **only** in how the caller is authorized and in the audit
  actor/source — the security-critical stopped-gate is single-sourced so the faces
  cannot drift apart.
- **Audit attributed to break-glass/internal.** The internal handler audits directly
  via `Repo.Audit` with `Actor:"break-glass", Source:"internal"` (the `a.audit`
  helper hardcodes `Source:"external"`), so a console-initiated backup is
  distinguishable in the audit log from an owner's self-service one.
- **Console does not double-audit.** Unlike the halt peer — which writes the CRD
  directly and audits locally — the backup peer goes through the API, and the API
  audits at the boundary. Auditing is single-sourced there; the console will not
  emit its own row.

## Files

| File | Change |
|---|---|
| `internal/api/handlers_backups.go` | **+`handleInternalBackup`**, **+`enqueueBackup`**; `handleBackupNow` tail now calls `enqueueBackup(..., p.Email, "external")` |
| `internal/api/api.go` | register `POST /api/v1/internal/servers/{name}/backup` on the internal-face route table |
| `docs/openapi.yaml` | document the `internalBackupNow` operation (`x-felis-face: [internal]`, `serviceToken` security) |
| `internal/api/handlers_backup_now_test.go` | **+`TestInternalBackup`** — no-owner-gate, break-glass/internal audit, stopped-gate/503/404/400 |

## Verification

WSL oracle (go1.26.4, authoritative for Go):

```
go build ./...  &&  go vet ./...  &&  go test ./...   → ALL GREEN
```

`TestOpenAPIMatchesServedRoutes` gates the new route against `docs/openapi.yaml` in
both directions (served⇔documented) and passes. `TestInternalBackup` (5 subtests) and
the existing `TestBackupNow` (10) both pass — the external refactor is
behaviour-preserving (same audit actor `p.Email`/source `external`).

## Self-review outcome

- **ponytail (over-engineering):** the internal face is not a copy of the external
  handler — the shared tail (`enqueueBackup`) collapses the duplication, and the two
  handlers hold only their distinct auth + audit-attribution. No new abstraction
  beyond the one shared function two callers already justify.
- **correctness:** the no-owner-gate difference is deliberate and matches the other
  service-token handlers; the stopped-gate is unchanged and now single-sourced, so the
  external and internal faces cannot diverge on the RWO safety check.
