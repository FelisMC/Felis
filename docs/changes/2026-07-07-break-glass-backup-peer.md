# Break-glass "back up a world now" console peer (§B4 "Sync", phase 2b)

- **Type:** feature (addition)
- **Date:** 2026-07-07
- **Area:** `cmd/felis` (Go, oracle-verified); `internal/api` + `docs/openapi.yaml`
  (the internal endpoint's `os_user` accountability extension)
- **Commit:** `fc748d3`
- **Task:** #31 Phase B4 break-glass ops — the "Sync" operation. Per the user's
  **"两者都要"** decision the feature was built in two halves: the felis-api endpoint
  that does the real backup-Job orchestration (phase 1 `7a7c0d5` external face, phase
  2a `f2fc57c` internal face) and a break-glass menu peer that calls it while the API
  is alive. **This change is that peer — phase 2b — the last build step of "Sync".**
  (S3 remains open in B4; this does not close the phase.)

## What it does

Adds a **"Back up a world now (Sync)"** operation to the root-gated break-glass
console. The operator picks a server from the live fleet; the peer resolves the
`felis-api-internal` ClusterIP Service and the service token from the control
namespace, then POSTs the internal backup endpoint to snapshot that server's world
while felis-api is alive. It is the on-node counterpart to the halt op: an emergency
"snapshot this now" lever for when the panel is unreachable but the box still has
`root` + a kubeconfig and the API is running.

It also extends the internal backup endpoint (phase 2a) to accept an optional
`{"os_user":"..."}` body so the audit row names the operator at the keyboard rather
than the generic `break-glass`.

## Why

The console cannot render the backup Job itself — it lacks the deployment coordinates
(`FELIS_IMAGE`, `FELIS_BACKUP_PVC`) that only felis-api holds. So, unlike the halt peer
(which writes the `MinecraftServer` CRD directly), the backup peer must go **through**
the API. The internal face exists precisely so an on-node machine caller with the
service token — no browser session, no Cloudflare-Access Principal — can reach that
orchestration. This change is the client that knocks on that door.

## Design decisions

- **Goes through the API, does not orchestrate locally.** Mirrors the phase-2a
  rationale: deployment coordinates live only in felis-api. The peer's job is to
  resolve the endpoint, authenticate with the token, and translate the HTTP result
  into a friendly outcome card — not to build a Job.
- **ClusterIP resolution, not DNS.** `resolveInternalAPI` `Get`s the
  `platform.APIInternalServiceName` Service and dials its `Spec.ClusterIP:APIInternalPort`
  directly, erroring on an empty or `None` (headless) ClusterIP. The on-node console's
  host resolver is not CoreDNS, so the in-cluster Service DNS name would not resolve
  from the host; the ClusterIP is routable from the node and is what the `2ba9948`
  dedicated ClusterIP Service exists to provide.
- **`os_user` attribution, parity with halt.** The console sends the escalated OS user
  in the request body; the endpoint makes it the audit actor. The body is decoded
  whenever `ContentLength != 0` — **not** gated on `Content-Type` (`decodeJSON` checks
  only for unknown/trailing fields, not the header) — so a console that forgets the
  header still records the operator. Absent/blank falls back to `break-glass`. The
  console does **not** double-audit: the API audits at the boundary, single-sourced.
- **Stopped-gate stays server-side.** The world PVC is RWO, so the server must be
  stopped. The peer does not pre-check this; it lets the API's stopped-gate return
  `409 not_stopped` and renders that as a "must be stopped — halt it first" card. The
  safety check is single-sourced in the API, never duplicated (and possibly drifting)
  in the console.
- **A running pick ends the session with exit 1 — deliberately accepted.** The picker
  lists all servers (a running pick is easy to hit), and a `409` surfaces as an error
  through `backupResultMsg{err}` → root sets `m.err` → `breakglass.go` prints the
  friendly card to stderr and exits non-zero. A dedicated `not_stopped` non-error path
  was considered and **rejected**: every break-glass op ends the session anyway (all
  `tea.Quit`), so `409`-vs-success differs only in exit code — marginal for an
  interactive TUI. No error is swallowed; the friendly card is shown either way. Adding
  a soft-landing state machine for one status code is complexity the interactive
  surface does not earn.
- **Core/shell split, mirrors halt.** All decision logic (`resolveInternalAPI`,
  `requestBackup`, `backupErrorFromResponse`, `performBackupNow`) lives in `backupnow.go`
  and is unit-tested against a controller-runtime fake client + `httptest`.
  `tui_backupnow.go` is thin bubbletea/huh glue (untested by house convention, mirrors
  `tui_halt.go`). The picker **reuses** `listServersForHalt`/`haltableServer` rather
  than cloning a second server-listing path.

## Files

| File | Change |
|---|---|
| `cmd/felis/backupnow.go` | **new** — pure core: `resolveInternalAPI` (Service ClusterIP + token secret), `requestBackup` (POST + Bearer + `os_user` body, status→outcome), `backupErrorFromResponse` (409/503/404/error-body mapping), `performBackupNow` |
| `cmd/felis/backupnow_test.go` | **new** — table tests against a fake client + `httptest`: happy resolve, headless/empty-token errors; 202 asserts Bearer + `os_user` + path; 409/503/404 + transport-failure mapping |
| `cmd/felis/tui_backupnow.go` | **new** — bubbletea/huh shell mirroring `tui_halt.go`: load → pick → work → outcome card; empty-fleet guard; friendly error card |
| `cmd/felis/tui_menu.go` | `bgSyncBackup` enum + "Back up a world now (Sync)" menu option after the halt option |
| `cmd/felis/tui_root.go` | `bgSyncBackup` dispatch to `newBackupModel`; `backupResultMsg` terminal handling into `breakGlassResult` |
| `cmd/felis/breakglass.go` | `backedUp`/`backupServer`/`backupStatus` result fields; cancel guard; post-exit backup summary |
| `internal/api/handlers_backups.go` | `handleInternalBackup` decodes the optional `os_user` body (gated on `ContentLength`, not `Content-Type`) and passes it as the audit actor; defaults `break-glass` |
| `internal/api/handlers_backup_now_test.go` | **+subtest** in `TestInternalBackup`: `os_user` body attributes the audit to the operator |
| `docs/openapi.yaml` | document the `internalBackupNow` optional `os_user` request body |

## Verification

WSL oracle (go1.26.4, FedoraLinux-44, authoritative for Go):

```
go build ./...  &&  go vet ./...  &&  go test ./...   → ALL_GREEN
```

`cmd/felis` and `internal/api` both re-ran (not cached), so the new `backupnow_test.go`
and the added `TestInternalBackup` subtest executed. `TestOpenAPIMatchesServedRoutes`
still passes: the `os_user` body is an addition to an already-documented operation, so
the served⇔documented route match is unchanged. The core is covered against a real
`fake.Client` (resolves the Service/Secret) + `httptest.Server` (asserts the wire
request and maps every status), so the tests exercise persisted/observable behaviour,
not merely that a call was made. `tui_backupnow.go` is thin glue, untested per the
`tui_halt.go` convention.

## Self-review outcome

- **ponytail (over-engineering):** lean — no new abstraction beyond the four core
  functions two call sites (test + TUI) already justify; the picker reuses halt's
  server-list core rather than cloning it; the deliberate rejection of a `not_stopped`
  soft-landing path kept the state machine at four steps. Nothing cut.
- **correctness:** the `os_user` decode is gated on `ContentLength`, matching how
  `decodeJSON` actually works (no `Content-Type` check), so a header-less console still
  attributes correctly; the stopped-gate and audit stay single-sourced server-side, so
  the peer cannot drift from the endpoint on the RWO safety check or the audit record.
