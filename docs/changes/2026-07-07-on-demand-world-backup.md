# On-demand world backup (§B4 break-glass "Sync"; felis-api endpoint + Job executor)

- **Type:** feature (addition)
- **Date:** 2026-07-07
- **Area:** `internal/backupjob` (new pkg), `internal/api`, `cmd/felis`, `docs/openapi.yaml` — Go, oracle-verified
- **Commit:** `7a7c0d5`
- **Task:** #31 Phase B4 break-glass ops — the "Sync" operation, resolved with the user as **immediate/on-demand world backup**. Per the user's "两者都要" decision this is built in two halves: **(this change) the felis-api endpoint that does the real backup-Job orchestration**, and (a follow-up) a break-glass menu peer that calls it while the API is alive.

## What it does

Adds `POST /api/v1/servers/{name}/backup`: an owner or admin snapshots a **stopped**
server's world into the archive store on demand, recorded as a first-class
`world_backups` row (reason `manual`) — restorable later by the existing restore path
and expired by the reaper's retention pass, so it never leaks as an orphan archive.

The backup runs asynchronously as a one-shot Kubernetes Job (the new
`internal/backupjob` package), mirroring how restore and image builds hand off to
Jobs. The handler answers **202 `backing_up`**.

## Why

felis-api cannot archive a world in-process: the world PVC is **RWO** and owned by the
operator's StatefulSet, so the API has nothing to mount at request time — the same
constraint that already makes `internal/restore` a Job. The break-glass console (which
runs direct-to-Postgres) likewise lacks the deployment coordinates (`FELIS_IMAGE`,
`FELIS_BACKUP_PVC`) needed to render the Job. Both point to the same home: the
orchestration belongs in felis-api, which holds those coordinates; other callers
invoke the endpoint.

## Design decisions

- **Backup Job self-records its `world_backups` row.** Unlike the restore Job — which
  is deliberately DB-blind because it processes a potentially poisoned archive — the
  backup Job **does** mount the felis config Secret and inserts its own backup row,
  exactly like the reaper (the only other component holding both a world mount and the
  database). This avoids the archive-then-async-record split that would otherwise leak
  orphan archives on a crash. The security review for that one departure lives in
  `internal/backupjob/jobspec.go` and is frozen by `jobspec_test.go`. Rationale: a
  backup only **reads** a world the operator already owns and tars it (bytes, never
  executed), so restore's poisoned-input threat does not apply; its blast radius (DB +
  two PVCs) is a strict subset of the reaper's, and it never deletes a PVC nor calls
  the K8s API (SA token stays un-mounted).
- **World mounted read-only, backup PVC read-write** — the mirror image of restore.
- **Stopped-gate (409 `not_stopped`).** The world PVC is RWO and held by a running
  server, so a backup Job cannot double-mount it; the handler refuses unless the server
  is fully stopped (`info.Ready || DesiredState != Stopped`). This also guarantees a
  quiescent, non-torn archive. Mirrors `handleRestoreBackup`'s gate.
- **Authorization is restore's front half, minus the former-owner match.** Backup is
  initiated by the **current** owner and records **their** ownership, so there is no
  prior owner's data to leak — the leak guard that restore needs does not apply here.
  An admin may back up an unowned (released) world; the recorded former owner is then
  empty, exactly as the reaper records for an unowned reap.
- **Unique Job name per request.** Each backup Job is named `backup-<server>-<rand>`,
  not a deterministic `backup-<server>`. A deterministic name would collide with a
  just-finished Job still inside its `TTLSecondsAfterFinished` window (10m), and the
  `AlreadyExists → 202` path would then silently produce **no** archive — the exact
  window a user (or the console "立即备份" button) retries in. Unique names make every
  request produce its own archive; `ErrAlreadyExists` remains only as a defensive
  no-op on the ~impossible suffix collision. Ceiling (documented in `backup.go`): two
  truly simultaneous taps may schedule two backup Pods — both mount the world PVC
  read-only, so neither corrupts anything; single-flight-on-running is the upgrade
  path if a double-tap storm ever appears.
- **One retention clock.** The entrypoint reuses the reaper's `reaperConfig` derivation
  so a manual backup expires on the same schedule as an inactivity backup — one policy,
  not two. The `"bk-"+hex` id scheme also matches, so manual and inactivity backups are
  indistinguishable downstream.
- **Fail-safe on record failure.** If the row insert fails, the entrypoint deletes the
  just-written archive so a failed backup leaves no unrecorded bytes.
- **Optional executor, honest 503.** Wired only when `FELIS_IMAGE` + `FELIS_BACKUP_PVC`
  are supplied (same gate as restore); otherwise `API.Backuper` is nil and the endpoint
  returns 503 `backup_unavailable`, so the authorization boundary is exercised before
  the Job executor is deployable.

## Files

| File | Change |
|---|---|
| `internal/backupjob/jobspec.go` | **new** — `BackupJob` renderer + `BackupJobName`; weak SA, token off, hardened container, world RO / backup RW, config-Secret mount |
| `internal/backupjob/backup.go` | **new** — `Backuper` (idempotent enqueue) + `Config`/`withDefaults` |
| `internal/backupjob/k8sjobs.go` | **new** — controller-runtime `CreateBackupJob` (AlreadyExists → idempotent) |
| `internal/backupjob/jobspec_test.go` | **new** — freezes the Job's security shape incl. the deliberate config-Secret mount |
| `internal/backupjob/backup_test.go` | **new** — asserts each `Backup` call mints a unique Job name (repeat-tap must not silently no-op) |
| `cmd/felis/backup.go` | **new** — `felis backup` in-Pod entrypoint: archive + self-record + orphan-cleanup |
| `cmd/felis/run.go` | dispatch `case "backup"` + usage line |
| `internal/api/backuper.go` | **new** — the narrow `Backuper` port |
| `internal/api/handlers_backups.go` | **+`handleBackupNow`** |
| `internal/api/api.go` | `Backuper` field + `POST /servers/{name}/backup` route |
| `internal/api/handlers_backup_now_test.go` | **new** — `fakeBackuper` + handler subtests |
| `internal/api/backuper_wire_test.go` | **new** — compile-time `Backuper = (*backupjob.Backuper)(nil)` |
| `cmd/felis/api.go` | wire `backuper` under the `FELIS_IMAGE`+`FELIS_BACKUP_PVC` gate; `backupConfig` helper |
| `docs/openapi.yaml` | document the `backupNow` operation |

## Verification

WSL oracle (go1.26.4, authoritative for Go):

```
go build ./...  &&  go vet ./...  &&  go test ./...   → ALL GREEN
```

The `internal/api` OpenAPI served-route contract test (`TestOpenAPIMatchesServedRoutes`)
initially failed — the new route was served but undocumented — and passes after adding
the `backupNow` operation to `docs/openapi.yaml`. `internal/backupjob` and the new
handler subtests pass. The controller-runtime `K8sJobs` binding is integration-only
(needs a live cluster) and is exercised only by the interface conformance test.

## Self-review outcome

- **ponytail (over-engineering):** the backup Job is a near-mirror of the restore Job,
  not a shared parameterization — deliberate, because its security shape differs (it
  holds DB creds) and must be asserted independently, not hidden behind a shared knob.
  No speculative config; `Config.withDefaults` fills only real deployment values.
- **correctness:** the RWO stopped-gate and the self-recording atomicity were traced to
  the reaper and restore before writing; the former-owner asymmetry vs restore is
  justified above.
