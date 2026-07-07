# §B4 phase close — S3 archive backend deferred by design (decision record)

- **Type:** decision / scope record (no code changed)
- **Date:** 2026-07-08
- **Area:** `internal/config`, `internal/backup` — the archive-store backend selection
- **Task:** #31 Phase B4 break-glass ops — **closes the phase**, superseding the phase-2b
  note in [break-glass-backup-peer](2026-07-07-break-glass-backup-peer.md) ("S3 remains
  open in B4; this does not close the phase").

## Decision

The three operational B4 break-glass ops are built, wired into the recovery console menu,
and oracle-verified:

| Op | Menu enum | Commit |
|---|---|---|
| Provision/reset Owner | `bgProvisionOwner` | (Phase B1 lineage) |
| Add Operator ("OP create") | `bgAddOperator` | recovery-console menu |
| Halt a running server | `bgHaltServer` | `c2ee21a` |
| Back up a world now ("Sync") | `bgSyncBackup` | `7a7c0d5` / `f2fc57c` / `fc748d3` |

The fourth B4 line item — **S3 archive backend (`tarS3`)** — is **deferred by design**, not
left as a silent gap. It is closed as a documented deferral and **#31 is done**.

## Why deferring is safe (not a loose end)

- **Fail-closed at config load, frozen by a test.** `config.Load` rejects
  `store = "tarS3"` (and `volumeSnapshot`, `longhorn`) with an error that points the
  operator at the `tarLocal` remediation. `TestLoadRejectsUnimplementedArchiveStore`
  freezes exactly this: a config naming an unimplemented backend fails at load, so
  felis-api can never boot green while the reaper CronJob fails every run and restore
  silently 503s. tarS3 cannot be selected into a broken state.
- **Peer to two other deferred backends.** `tarS3` sits beside `volumeSnapshot` and
  `longhorn` as recognized-but-unimplemented store names. The spec's own phasing is
  tarLocal-first ("起步 `tarLocal` … 要异地/跨集群 → `tarS3`"): the baseline single-node
  path is `tarLocal` (tar → backup PVC), which is implemented, tested, and the backend
  every built backup/restore path uses today.
- **Offsite/cross-cluster DR is the only capability gap**, and it is opt-in future work,
  not a correctness hole in the shipped baseline.

## The build path, when offsite DR is wanted

`minio-go/v7` is already vendored (the modpack upload lane's `internal/submit/s3store.go`),
so tarS3 adds no dependency. A future build is bounded:

1. `internal/backup/tars3.go` — a `WorldArchiver` reusing the existing package-level
   `writeTarGz`/`readTarGz`/`pruneToManifest`, streaming the tar to an object via
   `PutObject` (size −1, multipart) and reading it back via `GetObject`, mirroring
   `s3store.go`'s fakeable-client testability.
2. Store-selection factory in the backup/reaper entrypoint (`store = "tarS3"` → construct
   the minio-backed archiver) + remove tarS3 from the config fail-closed list (update
   `TestLoadRejectsUnimplementedArchiveStore` to keep only volumeSnapshot/longhorn).
3. Inject the S3 Secret into the backup/reaper Job Pods (integration wiring, like the
   Kaniko S3-context credential path).

The live-S3 upload + Secret-into-Pod would be integration-only verified, exactly as the
modpack S3 backend and the pgrepo SQL are.
