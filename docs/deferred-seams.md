# Deferred integration seams

`INTEGRATION-ONLY` and `KNOWN-LIMITATION` are grep-able markers in the Go source.
This file is the index of what each one currently means, so that reading the
unfinished face of the system does not require re-deriving it from 34 comment
sites.

It exists for two reasons. The markers do not all mean the same thing — four
distinct states share them, and "declared, nothing implements it" reads exactly
like "implemented, but its I/O cannot be exercised from this repo". And a marker
outlives the condition it describes: two of them were stale when this index was
first assembled, both claiming as future work something that had already shipped.

The pattern here is the one `internal/updater/doc.go` already uses for its own
package — state the verification boundary in buckets, so a green test suite is not
mistaken for a finished integration. This file is the same idea across the whole
tree.

## The marker collides with a different vocabulary in troubleshooting.md

`docs/troubleshooting.md` uses `[INTEGRATION-ONLY]` for something else, defined in
its own opening at `:19`: the symptom is produced by the kubelet, kaniko or a live
handshake, so it cannot be reproduced from the repository. Those twelve marks say
where a failure comes from. They are not unfinished work and are not indexed below.
A grep across `*.md` and `*.go` returns both sets; only the Go ones are seams.

## Declared, nothing implements it

- `internal/updates/seams.go:32` — `Notifier`. `internal/mail` sends OTP over SMTP,
  but nothing adapts it to this interface and no in-game channel exists. `felis
  update` passes nil deliberately: a human typing the command is the notification.
- `internal/updates/seams.go:43` — `Applier`. Nothing applies an update anywhere. A
  nil applier is not silent — `Run` records `errNoApplier` against every planned
  apply, so a mis-scheduled apply is loud rather than lost.
- `internal/updater/gatherer_integration.go:22` — the two current-version seams
  `NewSysGatherer` leaves nil, for the in-cluster path: the k8s read of the
  control-plane Deployment image, and the Velocity jar inspection. Both are answered
  on the host path (see "Built" below), so this gap is specific to a caller that has
  a cluster client instead of the node.
- `internal/api/handlers_updates.go` — the maintenance window is advisory: no
  in-cluster runner applies updates. `felis update` reads the stored window, prints
  where now sits against it and warns before an apply outside it; the runner itself
  still runs with a zero window, so no path can claim an apply is under way.
- `internal/submit/blobstore.go` — CLOSED 2026-09-22. The uploads PVC still cannot
  cross namespaces, so the transport went through the API instead of a mount: the
  derived context ref is now the internal-face URL
  (`/api/v1/internal/submissions/{id}/context`, service-token gated), the build
  Job's `context-fetch` initContainer streams it with `felis fetch-context` and
  extracts under a zip-slip guard into a size-limited emptyDir, and Kaniko builds
  `--context=/context`. The token reaches the build namespace through the same
  Secret-replica mechanism the login gate uses (bootstrap + `felis setup`), and the
  build egress lock allows exactly the control namespace on the internal port.
  Uniform for local and s3:// stores — neither hands the sandboxed build Pod a
  filesystem view or object-store credentials. Kaniko, Trivy and Trivy's two DBs
  come from the registry's `mirror/` copies, which the installer and
  felis-build-tools.timer keep current (`felis mirror-build-tools`,
  docs/troubleshooting.md §8e); the `[registry]` keys override them.

## Built; only its I/O is unverifiable from this repo

Code exists and is unit-tested against fakes. What is missing is a host, a cluster
or a real upstream account to run it against — not an implementation.

- `cmd/felis/tui_edge_apply.go:246,274,295` — the `nft` edge fence, its idempotent
  teardown, and the cloudflared invocation.
- `internal/cfsetup/runner.go:18` and `internal/cfsetup/cfsetup.go:329` — the real
  Cloudflare Tunnel and Access API calls; `internal/cfsetup/cfsetup_test.go:11`
  drives the whole flow through a fake.
- `internal/api/console.go:39`, `internal/api/logstream.go:236`,
  `internal/api/logstream.go:306`, `internal/fileedit/k8sjobs.go:45` — each needs a
  live cluster (RCON, `pods/log` follow, a Job). **Verified live 2026-09-22/23
  (auditfix7–25):** the RCON command spine (wake → probe → `command`/access
  mutations/stop), the log SSE stream, and the fileedit Job have each run
  end-to-end on the drill cluster.
- `internal/api/handlers_access.go:170,490` — parsing real vanilla and LuckPerms
  command output. **Verified live 2026-09-23:** players / whitelist / banlist
  parses matched a live Paper server's replies (LuckPerms not installed → the raw
  reply falls through as documented; the input guards held on four negative cases).

## Deliberately accepted, not scheduled to close

These are decisions, not backlog. Each names the condition under which it would be
worth revisiting.

- ~~`internal/api/pgrepo.go:281` — the quota check and `ClaimServer` are two statements
  (audit #4 TOCTOU). Closeable only against a real Postgres.~~ **Closed** — the gate
  moved inside `ClaimServer` (advisory lock + re-check + UPDATE in one transaction),
  red-then-green in the pgint suite, which is exactly the real-Postgres harness this
  line was waiting for.
- `internal/api/api.go:773` — `cooldownLimiter` is process-local, so across N api
  replicas a caller could draw up to N OTP codes per window. The intra-replica burst
  is closed; cross-replica bounding needs a shared store, out of scope for a
  single-replica install. Revisit before the api Deployment runs more than one
  replica.
- `internal/submit/submit.go:524` — the per-user upload storage budget reads the
  stored bytes, then writes. On one replica the API's per-user upload reservation
  serializes it; across replicas a burst can overshoot by one blob per interleaved
  upload, each still under the single-blob cap. The pending-submission cap no
  longer has this shape: `CreateSubmission` counts and inserts under a
  per-submitter advisory lock (pgint `TestSubmitPendingCapHoldsUnderConcurrency`).
  Revisit with the cooldown above, before scaling api replicas: a reservation row
  per upload in the same kind of transaction closes it.
- `internal/submit/submit.go:436` and `internal/submit/submit_test.go:351` — a
  post-CAS `Approve`
  failure leaves a row indistinguishable from the benign case, so `Approve` returns a
  distinct error naming the running build rather than allowing a blind re-drive that
  would double-push. The alternative ordering is worse.
- `cmd/felis/tui_edge_apply.go:246`, second marker on the same site — the fence
  targets nftables. On a firewalld host a reload can flush the standalone table;
  firewalld-native coordination is not handled. A missing `nft` binary fails loud
  rather than leaving the port open.
- `internal/api/handlers_account.go:169` — the reclaim "start fresh vs inherit"
  choice is CODE-ONLY on the Java/Velocity side; the link-status endpoint reports
  link completion only and does not surface it.

## Wired since the marker was written

- `internal/api/handlers_email_otp.go` and `internal/api/api.go` — SMTP shipped on
  2026-07-20 (`internal/mail`, wired in `cmd/felis/api.go`). An install with no
  `[smtp]` section leaves the `Mailer` nil, and every door that mails a code answers
  503 `mail_unavailable`; codes are never logged. The reading "Felis cannot send
  mail" is stale.
- `internal/config/config.go:117` — was stale. It described the upload transport as a
  deferred integration after both backends had shipped (`LocalContextStore`,
  `S3ContextStore`, selected in `cmd/felis/api.go` by the shape of the configured
  base). Corrected in the change that added this file; what remains deferred is only
  Kaniko's read, indexed above.
- `internal/updater/doc.go:44` — was stale. Its REMAINING INTEGRATION bullet listed
  the `felis update` CLI and the off-cluster Velocity jar read, both of which exist
  (`cmd/felis/update.go`, `internal/updater/gatherer_host.go`). Corrected in the same
  change; the two nil seams it also names are real and remain above.

## Recorded outside the code

- The minecraft-namespace egress is locked (`felis-server-egress`, DNS plus the
  public internet with every private range and the node's own global addresses
  excluded) and `felis-login-to-internal-api` opens the one platform path a game pod
  needs — login → felis-api:8081. Any new in-cluster service a game server must call
  needs its own allow policy next to that one (`internal/platform/netpol.go`).
