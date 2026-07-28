# Felis Troubleshooting Checklist

This file is the spec §28 #23 deliverable: a故障排查清单 (troubleshooting
checklist) for the failure modes the platform actually produces. Every symptom
below is traced to a concrete signal — a `status.conditions` reason, an HTTP
error code, a log string, or a manifest name — so an operator can map what they
see to the code path that emitted it.

## How to read this document

Each entry is **symptom → likely cause → where to look → fix**. Signals are
graded for how far the in-repo Go test suite proves the behaviour:

- **[GO-TESTED]** — a hermetic `*_test.go` exercises this exact path; the
  string/code is asserted in CI.
- **[CODE-ONLY]** — the code path and string exist and are real, but no unit
  test drives them (notably the Velocity Java plugin, which is not compiled or
  tested in this repo).
- **[INTEGRATION-ONLY]** — the symptom is produced by the kubelet, kaniko,
  containerd, Postgres, or the network, not by Felis Go code; you will see it in
  `kubectl describe` / pod logs, never in `MinecraftServer.status`.
- **[INERT]** — the configuration field exists in the CRD but no controller
  reads it. Tuning it does nothing. §12 lists the one field this still applies
  to, alongside the fields that *are* read and the condition each depends on.

The operator never invents the parent domain; routing identity is
`spec.subdomain` under the deployment zone. Examples below use
`<root-domain>` / `registry.<ns>.svc:<port>` placeholders rather than any
concrete host.

---

## 1. Server is stuck in `Starting` and never becomes `Running`

`MinecraftServer.status.phase` stays `Starting`. A start that never succeeds is
requeued every 5s until one of the two startup budgets expires, then escalated
to `Failed` — `StartupTimeout` if the pod never passed TCP readiness,
`ReadinessTimeout` if the RCON probe never succeeded. Both default to **300s**
when `spec.startup.timeoutSeconds` / `spec.startup.readinessTimeoutSeconds` are
unset or `0`, and both are measured from `status.startRequestedAt`.
[GO-TESTED: `TestReconcileRunning_StartupTimeoutConvertsToFailed`,
`TestReconcileRunning_ReadinessTimeoutConvertsToFailed`.]

So `Starting` seen *once* is normal and
`TestReconcileRunning_RconProbeFailureStaysStarting` asserts exactly that — a
single failed probe must not flap the phase. `Starting` seen for longer than the
budget means the reconcile loop is not running at all; check the operator's own
logs before tuning anything. Either way the underlying cause is diagnosed from
pod state, not from `MinecraftServer.status`.

First, read the condition reason:

```
kubectl get minecraftserver <name> -o jsonpath='{.status.conditions}'
```

`markStarting` writes the same reason to both `Ready=False` and
`RconReached=False`. The reason is exactly one of:

| `status.conditions[].reason` | Meaning | Requeue |
|---|---|---|
| `PodNotReady` | Pod not TCP-ready yet (`status.readyReplicas < 1`) | 5s |
| `RconSecretUnavailable` | RCON secret missing or malformed | 10s |
| `RconNotReachable` | RCON dial/auth failed | 5s |

[GO-TESTED for the reason set.]

### 1a. `PodNotReady` — pod never goes ready

The operator cannot tell *why* the pod is not ready; **a broken image, an
unbound PVC, and a backend that simply has not finished booting all surface as
the identical `PodNotReady` signal.** [GO-TESTED that the reason is emitted;
[INTEGRATION-ONLY] for the underlying pod cause.] You must drop to the pod:

```
kubectl get pod -l app.kubernetes.io/name=<name>
kubectl describe pod <pod>     # look at Events + container State
```

- **`ImagePullBackOff` / `ErrImagePull`** → `spec.image` is wrong, the tag does
  not exist, or the registry is unreachable. `spec.image` is copied verbatim into
  the container with **zero validation** by the operator. Fix the image
  reference, or see §7 (registry reachability) and §6 (build push target).
- **PVC `Pending`** → `kubectl get pvc -l app.kubernetes.io/name=<name>`. A
  nonexistent `spec.storage.storageClassName`, or a request larger than any class
  can satisfy, leaves the PVC unbound. The operator does **not** error on this
  (only a malformed *quantity* errors — §2); it waits in `Starting` indefinitely.
  Fix the StorageClass name or capacity. [INTEGRATION-ONLY.]
- **Container crash-looping before the readiness port opens** → check container
  logs; this is a backend/entrypoint problem, not a Felis problem.

### 1b. `RconSecretUnavailable` — RCON secret missing or malformed

The probe needs the RCON password from `spec.rcon.secretRef`. The message is the
verbatim error: [CODE-ONLY for these branches]

- `rcon.secretRef.name and .key are required when rcon is enabled` — you enabled
  `spec.rcon.enabled` but left `secretRef.name` or `secretRef.key` empty.
- `secret "<name>" has no key "<key>"` — the Secret exists but lacks the named
  key.

Fix: create the Secret with the referenced key, or correct `secretRef`. Verify:

```
kubectl get secret <secretRef.name> -o jsonpath='{.data.<key>}' | base64 -d | wc -c
```

### 1c. `RconNotReachable` — RCON dial or auth failed

The probe is a TCP connect **plus** RCON auth handshake, then immediate close —
**no command is ever run; a successful auth IS the entire readiness gate.** The
message is the verbatim dial error:

- `rcon: authentication failed` → the password in the Secret does not match the
  backend's `rcon.password`. Reconcile the two. [GO-TESTED that this maps to
  `RconNotReachable`.]
- `connection refused` / `i/o timeout` → the backend has not opened the RCON
  port yet, RCON is disabled in `server.properties`, or `spec.rcon.port`
  (default 25575) is wrong. [INTEGRATION-ONLY for the live handshake.]

The per-probe timeout is a fixed 5s in code (`prober.go:45`, shortened further if
the reconcile context has a nearer deadline). It is **not** derived from
`spec.startup.readinessTimeoutSeconds`, which is the deadline for the whole
start, not for one probe — see §12.

---

## 2. Server is in `Failed`

There is exactly **one** path to `PhaseFailed`: `markFailed(server,
"InvalidSpec", err)`, reached only when the StatefulSet cannot be built. In
practice this means **a malformed `spec.storage.size`** (an unparseable resource
quantity), surfaced as:

```
invalid storage size "<value>": <parse error>
```

`status.conditions` will show `Ready=False` and `Provisioned=False`, both with
reason `InvalidSpec`. Fix the quantity (e.g. `10Gi`, not `10 GB`) and the server
leaves `Failed` on the next reconcile. [GO path is real; the specific branch is
[CODE-ONLY] — the reconciler test fixture uses a valid size.]

Two caveats when a server has been in `Failed`:

- A pod-not-ready or unreachable-RCON server is **never** `Failed`; it is
  `Starting` (§1). If you see `Failed`, it is a spec problem, not a runtime one.
- `markFailed` does **not** reset `status.endpoint`. A server that fails after
  having been `Running` keeps a stale `endpoint.mode=direct`. The proxy should
  treat any non-`Running` phase as "do not route direct" rather than trusting a
  lingering `direct` endpoint (§4).

---

## 3. Players can't join / get sent to the wrong place

Routing is driven by `status.endpoint`:

- `markRunningReady` is the **only** writer of `endpoint.mode=direct`
  (`address=<game address>`), and only while `phase=Running` and RCON-ready.
- `markStarting`, `markStopping`, `markStopped` all write
  `endpoint.mode=fallback`, `address=spec.fallbackServer`.

So the proxy should route `direct` **only** when `phase=Running`; otherwise it
gets a `fallback` endpoint. [GO-TESTED for the direct/fallback toggle via
`markRunningReady`/`markStopped`.]

Two pitfalls:

1. **`spec.fallbackServer` is empty** → the fallback endpoint `address` is `""`,
   so during `Starting`/`Stopping`/`Stopped` the proxy has no lobby to park the
   player in. Set `spec.fallbackServer` to a registered Velocity server name.
2. **Stale `direct` after `Failed`** → see §2; the proxy must not honour a
   `direct` endpoint unless `phase=Running`.

### 3a. Wake-on-join is refused, slow, or rate-limited

When a player joins a stopped server, the proxy parks them in the fallback and
calls the internal wake API. The authorization gate order is **autostartPolicy →
per-server cooldown → global running cap**. Map the API result:

| HTTP | Code | Cause | Fix |
|---|---|---|---|
| `403` | `forbidden` | `autostartPolicy=allowlist` and UUID not allowlisted, or `ownerOnly` and caller is not owner | Add the UUID / claim the server / set `autostartPolicy=public` |
| `429` | (cooldown) | Wake retried within the 30s per-server `WakeCooldown` | Wait out the cooldown |
| `503` | `at_capacity` | Global `MaxRunningServers` cap reached | Stop another server or raise the cap |

[GO-TESTED: `handlers_internal_wake_test.go`, cooldown, running-cap shape.] The
operator's RCON probe — **not** the wake call — is the authoritative readiness
gate; the proxy polls `GET /api/v1/internal/servers/{name}/status` every ~2s and
teleports when `ready=true`.

The Velocity-side consumption of these codes (`403` → "You're not allowed to
start «server»"; `429` → re-queue; other → "Couldn't start … Try again
shortly.") lives in the Java plugin and is **[CODE-ONLY]** — the codes it reacts
to are produced by the Go-tested `authorizeWakeByUUID` / cooldown limiter, so
grade the two halves separately.

---

## 4. Routing is disabled even though servers are up (online-mode coupling)

Wake/claim/allowlist semantics trust **Mojang-verified online-mode UUIDs**. If
the proxy runs `online-mode=false`, those identities are spoofable, so the
Velocity plugin **refuses to activate routing**:

```
Felis routing DISABLED: the proxy is in offline mode (online-mode=false).
Domain autostart and the allowlist trust Mojang-verified UUIDs; refusing to
route on spoofable identities. /link remains available. Set online-mode=true to
enable routing.
```

[CODE-ONLY — `FelisVelocityPlugin.onProxyInitialize`.] `/link` still works
(account binding does not depend on routing), but no domain autostart happens.
The summary line prints `routing: disabled (offline mode)` or
`(no root-domain set)`. Fix: set `online-mode=true` on the proxy, or configure
the root domain if the log says `no root-domain set`.

On the server side, `spec.autostartPolicy` and `spec.onlineMode` are documented
as only meaningful when the proxy enforces `online-mode=true`. Setting them does
not by itself make an offline proxy safe — the proxy guard is the enforcement
point.

---

## 5. Web panel returns 401 / 403 (Zero-Trust / Cloudflare Access)

The external face accepts either a Cloudflare Access JWT
(`Cf-Access-Jwt-Assertion` header) **or** a local session cookie. The error
envelope is always `{"error":{"code","message","request_id"}}`. [GO-TESTED.]

- **`401 unauthorized`** — not authenticated: no/invalid Access JWT and no valid
  session. [GO-TESTED.]
- **`403 forbidden`** — authenticated but not permitted (e.g. a non-admin
  principal hitting an admin route; `IsAdmin()` requires `role=admin` **and**
  arrival via the admin Access audience/host). [GO-TESTED.]

### 5a. Every external request 401s on a fresh deploy

The Access verifier is wired **fail-closed**: `Keyfunc` (the JWKS key function)
is `nil` until deployment wiring supplies it. With a nil Keyfunc, **every** JWT
verification fails, and startup logs:

```
felis api: external face fails closed (Access JWKS key function not configured)
```

[INTEGRATION-ONLY — the live JWKS path is a deployment point.] This is intended:
the panel rejects all callers until JWKS is configured. Fix by wiring the
Access JWKS key function for `cfg.Auth.AccessJWTAud`.

### 5b. Token rejected with audience error

```
token audience does not include "<aud>"
```

The JWT's `aud` claim does not contain the configured `cfg.Auth.AccessJWTAud`
(or the admin audience for admin routes). [GO-TESTED.] Confirm the Access
application audience matches `cfg.Auth.AccessJWTAud`.

**Trust-model note for operators:** verification is **expiration-required +
audience + signing-key (JWKS)**. There is **no `iss` (issuer) check** anywhere in
the verifier. Trust rests entirely on the audience claim plus the JWKS signing
key. When documenting or auditing the trust boundary, do not assume issuer is
validated — it is not.

### 5c. Local-password login fails or is silently rejected

Local sessions use the `felis_session` cookie (HttpOnly, Secure, SameSite=Lax,
12h TTL, host-only). They are gated by the `local_auth_enabled` row in
`platform_settings`, read live per request and **fail-closed** (missing or
unparseable → treated as disabled). Symptoms:

- Cookie present but login rejected with `local auth disabled` → the
  `local_auth_enabled` setting is false/absent. A present cookie under disabled
  local-auth is **rejected outright**, not fallen through to the JWT path.
- `invalid session: …` → bad/forged session hash.

Fix: set `local_auth_enabled=true` in `platform_settings` if local password auth
is intended. [GO-TESTED for the session/QR-login logic.]

---

## 6. Internal API rejects Velocity / proxy callers (service-token)

The internal face (`--internal-addr :8081`, routes under
`/api/v1/internal/...`) is **never** Zero-Trust; it authenticates a single
service token via `Authorization: Bearer <token>`, compared in constant time.

In-cluster it is reached through the ClusterIP Service `felis-api-internal` (port
8081), which is separate from the external NodePort `felis-api` (443) precisely so
the no-Zero-Trust face is never exposed on a node. On the control-plane node the
break-glass console reaches it by resolving that Service's ClusterIP and dialing
`:8081`.

- **Internal calls fail to *connect* (not 401)** → the `felis-api-internal` Service
  is missing or its selector no longer matches the api pods. `kubectl -n felis get
  svc felis-api-internal` must show a ClusterIP with 8081; a bare `felis-api` name
  serves only 443 and every internal call would hang/refuse.

- **All internal calls 401** → the token is unset or wrong. The API reads env
  `FELIS_SERVICE_TOKEN`. If unset, startup logs:

  ```
  felis api: warning: FELIS_SERVICE_TOKEN unset — internal face will reject all callers
  ```

  and wires an empty token, which rejects **everyone** (no bypass). [GO-TESTED
  for the constant-time compare / empty-token rejection.]

In-cluster, the token's source of truth is the Secret `felis-service-token`
(key `token`), injected as `FELIS_SERVICE_TOKEN` on the API Deployment. Fix:

```
kubectl get secret felis-service-token -o jsonpath='{.data.token}' | base64 -d
```

Ensure the proxy is configured with the identical value.

---

## 7. Account-link and claim API errors

Codes from `handlers_account.go` / `handlers_internal.go`. [GO-TESTED.]

| HTTP | Code | When |
|---|---|---|
| `400` | `bad_request` | Missing `mc_uuid`, empty code, or invalid `auth_source` (must be `mojang`/`thirdparty`) |
| `400` | `invalid_code` | Link code unknown or expired (10-min TTL, 8-symbol code) |
| `409` | `already_linked` | That MC UUID is already linked to **another** user |
| `412` | `not_linked` | Claim/owner op by a caller with no verified account link |
| `403` | `quota_exceeded` | Claim would exceed the user's server quota |
| `409` | `already_claimed` | Atomic `UPDATE … WHERE owner_id IS NULL` affected 0 rows |
| `404` | `not_found` | Unknown server/resource |

Flow reminder: the **code is minted in-game** on the internal face
(`POST /api/v1/internal/account/link/code`, proves the UUID) and **verified on
the web** external face (`POST /api/v1/account/link/verify`, proves the user).
A `409 already_linked` rolls the transaction back and **preserves** the code so a
different user can still use it. The claim's race-safety is the single
conditional `UPDATE` under READ COMMITTED — `1` row → `200`, `0` rows → `409`.
The real SQL execution is [INTEGRATION-ONLY] (no sqlmock/dockertest in repo); the
handler logic is [GO-TESTED] via an in-memory fake repo.

---

## 8. Image build fails (Kaniko, spec §14/§16)

### 8a. Build push rejected at submission with `400`

Pre-build validation rejects any push target that is not the internal registry:

```
must target the internal registry "<registry.<ns>.svc:<port>>", not "<your-target>"
```

[GO-TESTED via the `validate` gate.] Fix the image reference to push to
`cfg.Registry.URL` (the internal registry — §9).

### 8b. Build Job's ServiceAccount can do nothing (RBAC "denial" by design)

The build/restore Job SAs (`felis-build`, `felis-restore`, namespace
`felis-build`) have **no Role and no RoleBinding anywhere** — isolation is the
*absence* of permissions (spec §16/§21). If you see the build SA denied a
namespaced API operation, **that is correct, not a misconfiguration.** [GO-TESTED
that the rendered manifests give these SAs no Role.] Do not "fix" it by granting
the build SA permissions.

### 8c. Build hangs then fails fetching base image / packages

The build namespace runs a default-deny egress NetworkPolicy
(`felis-build-egress`); the **only** allowed egress is the package-mirror CIDRs
from `--package-cidr`, which **defaults to none** (fail-closed, no internet).
[GO-TESTED for the netpol shape.] A kaniko run that hangs pulling a base image or
OS package from a non-allowlisted host is the egress policy doing its job — the
hang/failure text comes from kaniko/containerd and is [INTEGRATION-ONLY]. Fix:
add the mirror CIDR via `--package-cidr`, or pre-stage the base image in the
internal registry.

### 8d. Build reaches `Failed` phase

`reconcileBuilds` polls the Job; a Job reaching `Failed` is surfaced via
`writeBuildError` (JobPhase→Failed). [GO-TESTED for the mapping.] The underlying
cause — a kaniko build error or the **Trivy CRITICAL-CVE gate** failing the build
before push (spec §16) — is in the Job's pod logs and is [INTEGRATION-ONLY].
Inspect:

```
kubectl logs -n felis-build job/<build-job>
```

---

## 9. Registry push/pull failures (spec §15)

The in-cluster registry is Deployment/Service/PVC named `registry` in the
control namespace (or `--registry-namespace`):

- **Push/pull target (the exact string to match):**
  `registry.<ns>.svc:<port>` (port from `--registry-port`, default `5000`;
  e.g. `--felis-image registry.felis.svc:5000/felis:v1`). A *wrong* push URL is
  caught at build time by the validate gate (§8a, `400`). An *unreachable*
  registry at runtime (wrong DNS/port, PVC unbound, missing default StorageClass)
  surfaces as kaniko push or kubelet pull errors — [INTEGRATION-ONLY], **not** a
  Felis-emitted string.
- **Storage:** PVC is RWO, `10Gi`, mounted at `/var/lib/registry`, **no
  `storageClassName`** → binds the cluster default class. If the cluster has no
  default StorageClass the PVC stays `Pending` and the registry never starts.
- **Selector quirk worth knowing:** the registry Service selector is only
  `name + component=registry` — it deliberately lacks the
  `part-of=felis-control-plane` label, so the registry is *invisible* to the
  RCON-peer NetworkPolicy selector. This is intended isolation, not a bug; do not
  "fix" it by adding the label.

Registry manifest rendering is [GO-TESTED]; actual serving is
[CODE-ONLY/INTEGRATION-ONLY].

---

## 10. World reaper: false-deletes and skipped backups (spec §18)

The reaper is a **run-once daily CronJob batch**, not an operator controller. It
reaps a world only when `now - last_active_at > 15d` (`inactive_15d`); the 15-day
deadline is **hard-fixed in code** (only `warn_before` / `retention` /
`max_local_bytes` are configurable from `felis.toml [archive]`).

### The backup-before-delete invariant

The reap sequence (all [GO-TESTED] hermetically) preserves the world unless a
**confirmed, DB-recorded backup exists**:

1. `ensureCapacity` (only if `max_local_bytes > 0`) → store full ⇒ world
   **preserved** (not deleted).
2. `Archiver.Archive` fails ⇒ world **preserved**, PVC untouched.
3. `InsertBackup` (DB) fails ⇒ the orphan archive is deleted, PVC **untouched**.
4. **Only then** `DeletePVC` → `ReleaseWorld` → `Stop` (cosmetic) → audit →
   `felis_reaper_worlds_deleted_total++`.

So a missing backup never results in a deleted world. [GO-TESTED:
`TestReapArchiveFailurePreservesWorld`,
`TestReapInsertBackupFailurePreservesWorld`, `TestReapIdleWorldFullSequence`.]

### Exemptions (world never reaped)

- `spec.reaperExempt=true` → skipped entirely (system servers). [GO-TESTED
  `TestReapExemptServerNeverTouched`.]
- CRD missing → logs `reaper: CRD missing, skipping`, skipped.
- Idle `≤ 15d` → not yet eligible.

### Genuine false-delete risk vectors

- **Stale `last_active_at`.** The keep-alive is `RecordJoin`, called from the
  internal `join-event` handler. **If join events are not delivered to the API,
  the activity clock never resets** and an actively-played world becomes
  reap-eligible after 15 days. Verify join events are flowing (§3a) — this is the
  most important reaper check. [INTEGRATION-ONLY for the live Postgres write.]
- **Unowned servers are still reaped.** A server with `owner_id=""` gets **no
  pre-deletion warning** (`maybeWarn` skips unowned), but is still reaped at 15d.
  [GO-TESTED `TestReapUnownedServerStillReaped`.] Claim or exempt servers you
  want to keep.
- `DeletePVC` is idempotent (missing PVC is not an error), so a re-run will not
  fail on already-reaped worlds; and `Stop` failure is only logged, so a reaped
  world's `MinecraftServer` may not be flipped to `Stopped`.

Only `TarLocal` (tar+gzip) archiving is implemented; VolumeSnapshot/Longhorn
backends return `not implemented in this build`. The live PVC delete / Postgres
store paths are [INTEGRATION-ONLY].

---

## 11. Idle auto-stop never fires; player count always shows 0

Both are implemented, and both hang off the same switch: **`spec.rcon.enabled`**.
Check it first.

```sh
kubectl get minecraftserver <name> -o jsonpath='{.spec.rcon.enabled}'
```

The player tally is a by-product of the RCON readiness probe — `prober.go:63`
runs `list` on the same connection that just authenticated, and `parseListReply`
extracts the tally from `There are (\d+) of a max of (\d+) players online`. With
RCON disabled the probe never runs, `players` keeps its zero value, and
`markRunningReady` (`reconciler.go:413`) writes that zero into
`status.players.online`. So a permanent 0 means "never sampled", not "nobody
online".

Idle auto-stop (`reconciler.go:175`) reads that same tally, which is why it
carries the RCON condition explicitly:

```go
if server.Spec.Rcon.Enabled && server.Spec.Idle.AutoStopEnabled && server.Spec.Idle.EmptySecondsBeforeStop > 0 {
```

The comment above it says why: with RCON off the zero tally "would read as
'empty' and use to stop a server full of people". So the guard is deliberate —
enabling `spec.idle.*` without RCON is a no-op by design, not a missing feature.

Both fields set and still nothing happens? Then the probe is failing rather than
disabled: the server would be stuck in `Starting` with `RconNotReachable`
(`reconciler.go:156`), which is §1's symptom, not this one.

Note the reaper's `last_active_at` (§10) is a *different* subsystem (Postgres
business layer, bumped by join events) — it keeps worlds alive against the
reaper, but it does **not** auto-stop empty running servers.

---

## 12. A configuration field seems to be ignored

One CRD field exists and validates but is read by no controller; the rest of
this table records fields that *are* read, together with the condition that
decides whether setting them does anything.

| Field | What you might expect | Reality |
|---|---|---|
| `spec.startup.timeoutSeconds` | Start budget before `Failed` | Read by `startupTimedOut` (`reconciler.go:479`), called at `:126`. `0` or unset falls back to **300s**, then `markFailed("StartupTimeout")` |
| `spec.startup.readinessTimeoutSeconds` | First-probe budget | Read by `readinessTimedOut` (`reconciler.go:490`), called at `:157`. `0` or unset falls back to **300s**, then `markFailed("ReadinessTimeout")`. Not to be confused with the prober's own 5s dial timeout (`prober.go:45`) |
| `spec.idle.autoStopEnabled` | Auto-stop empty servers | Read at `reconciler.go:175` — but gated on `spec.rcon.enabled`, since the player tally comes from the RCON probe (§11) |
| `spec.idle.emptySecondsBeforeStop` | Empty grace period | Same branch. Must be `> 0`; the guard treats `0` as "off", not "stop immediately" |
| `spec.storage.retainOnDelete` | Keep/drop PVC on delete | **[INERT]** — world PVCs **always** survive server deletion; only the reaper ever deletes a world PVC (§13) |

Both startup budgets are measured from the same `status.startRequestedAt`, so
`readinessTimeoutSeconds` is not a budget *after* pod readiness — it is a
deadline for the whole start, applied on the RCON-probe branch.

---

## 13. World PVC survives after I deleted the MinecraftServer

This is expected. The world PVC is a StatefulSet `VolumeClaimTemplate`. There is
**no `persistentVolumeClaimRetentionPolicy` and no finalizer** anywhere in the
operator. Deleting the `MinecraftServer` garbage-collects the StatefulSet, but
StatefulSet deletion does **not** cascade to its template PVCs, and nothing else
cleans them up. So the world PVC **always survives** server deletion, regardless
of `spec.storage.retainOnDelete` ([INERT], §12). The **only** code that deletes a
world PVC is the reaper, and only after a verified backup (§10). To reclaim a
world PVC manually:

```
kubectl get pvc -l app.kubernetes.io/name=<name>
kubectl delete pvc <pvc>      # irreversible — the world is gone
```

---

## 14. Metrics for diagnosis (spec §23)

All four mandated metrics have real producers; scrape them when triaging:

- `felis_servers_total` — managed server count.
- `felis_start_duration_seconds` — histogram, observed once per start when
  readiness is first reached (`ReadySignalAt − StartRequestedAt`). A start that
  never completes (§1) contributes **nothing** here — absence of observations is
  itself the signal that starts are hanging.
- `felis_image_build_failures_total` — increments on build Job failure (§8d).
- `felis_reaper_worlds_deleted_total` — increments only after a world PVC is
  actually deleted post-backup (§10); a spike here means worlds crossed the 15d
  idle line — cross-check that join events are flowing (§10 risk vectors).

---

## Quick reference: symptom → section

| Symptom | Section |
|---|---|
| Stuck `Starting`, never `Running` | §1 |
| `Starting` with `PodNotReady` (image? PVC? boot?) | §1a |
| RCON secret/auth/port errors | §1b, §1c |
| Phase `Failed` | §2 |
| Players land in lobby / wrong place | §3, §4 |
| Wake refused / rate-limited (403/429/503) | §3a |
| Routing disabled, offline-mode | §4 |
| Panel 401/403; fails-closed; audience error | §5 |
| Local password login rejected | §5c |
| Internal callers 401 (service token) | §6 |
| Link/claim 400/409/412/403/404 | §7 |
| Build push 400 / SA denied / egress hang / Failed | §8 |
| Registry push/pull unreachable | §9 |
| World deleted unexpectedly / backup skipped | §10 |
| Idle auto-stop not firing; player count 0 | §11 |
| A config field seems ignored | §12 |
| PVC left behind after delete | §13 |
| Which metric to scrape | §14 |
