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
- **[PG-TESTED]** — a `-tags pgint` test in `internal/pgint` drives it against a
  real Postgres with the shipped migrations (CONTRIBUTING.md has the command).
- **[CODE-ONLY]** — the code path and string exist and are real, but no unit
  test drives them (notably the Velocity Java plugin, which is not compiled or
  tested in this repo).
- **[INTEGRATION-ONLY]** — the symptom is produced by the kubelet, kaniko,
  containerd, Postgres, or the network, not by Felis Go code; you will see it in
  `kubectl describe` / pod logs, never in `MinecraftServer.status`.
- **[INERT]** — the configuration field exists in the CRD but no controller
  reads it. Tuning it does nothing. **No CRD field carries this status today**;
  the last one, `spec.storage.retainOnDelete`, was removed rather than
  implemented (§13 records why). A field whose change seems ignored is almost
  always a condition instead — §12 lists the fields that *are* read and the
  condition each depends on.

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
- **Stuck in `Init:` or `AccessDeniedException` / `Permission denied` in the
  log** → the pod runs as uid/gid **1000** (`naming.GameUID`) with every
  capability dropped, whatever `USER` the image declares. Before the server
  starts, the `prepare-data` initContainer (`felis init-volume`, root with only
  `CHOWN` + `DAC_OVERRIDE`) hands every world entry not yet owned by 1000:1000
  to that uid, so a world written by an older root-run release or extracted by a
  restore Job is fixed on its next start:
  `kubectl logs <pod> -c prepare-data` prints how many entries it changed and
  lists up to 20 it could not. An image that writes outside `/data` and `/tmp`
  (a directory baked into the image as root) cannot run as uid 1000; rebuild it
  to keep its state under `/data`. [GO-TESTED: `TestBuildStatefulSetRunsGameAsNonRoot`,
  `TestChownTreeHandsOverMismatchedEntries`; INTEGRATION-ONLY for the walk on a
  live volume.]
- **`FailedCreate … violates PodSecurity "baseline"`** on the StatefulSet or a
  Job → the minecraft namespace enforces the PodSecurity `baseline` profile
  (`pod-security.kubernetes.io/enforce=baseline`, set by the install bundle).
  Everything Felis renders there fits it; a pod that is refused was edited or
  created outside Felis (hostPath, hostPort, privileged, extra capabilities).
  `kubectl get events -n minecraft --field-selector reason=FailedCreate` names
  the field. [GO-TESTED: `TestObjects_MinecraftNamespaceEnforcesBaseline`.]

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
| `409` | `maintenance_in_progress` | A restore, backup or file write holds the server's world volume (§3b) | Wait for the Job to finish |
| `429` | (cooldown) | Wake retried within the 30s per-server `WakeCooldown` | Wait out the cooldown |
| `503` | `at_capacity` | Global `MaxRunningServers` cap reached | Stop another server or raise the cap |

[GO-TESTED: `handlers_internal_wake_test.go`, cooldown, running-cap shape.] The
operator's RCON probe — **not** the wake call — is the authoritative readiness
gate; the proxy polls `GET /api/v1/internal/servers/{name}/status` every ~2s and
teleports when `ready=true`.

The Velocity-side consumption of these codes (`403` → "You're not allowed to
start «server»"; `409 maintenance_in_progress` → "«server» is under
maintenance", not queued; `429` → re-queue; other → "Couldn't start … Try again
shortly.") lives in the Java plugin and is **[CODE-ONLY]** — the codes it reacts
to are produced by the Go-tested `authorizeWakeByUUID` / cooldown limiter, so
grade the two halves separately.

### 3b. Wake, restore, backup or file save refused with `maintenance_in_progress`

A server's world volume is ReadWriteOnce, and on a single node RWO lets a game
pod and a restore Job mount it side by side. So felis-api serialises them per
server: a restore, a backup, or a file write takes the world, and until its Job
finishes every wake (panel or join) and every other world operation on that
server gets `409 maintenance_in_progress`. File reads and listings never hold
it. The operator applies the same rule when `desiredState` is flipped to
`Running` by anything other than felis-api: the StatefulSet is not scaled up,
and the `Ready` condition reads `MaintenanceInProgress` until the Job ends.

What holds the world, in order:

1. An unfinished Job labelled `felis.lolicon.best/server=<name>` with
   `app.kubernetes.io/managed-by` `felis-restore`, `felis-backup`, or
   `felis-files` plus `felis.lolicon.best/files-mode=write`:

   ```sh
   kubectl -n minecraft get jobs -l felis.lolicon.best/server=<name>
   ```

   A Job that is genuinely wedged is ended by its own `activeDeadlineSeconds`;
   deleting it by hand releases the world at once (`kubectl -n minecraft delete
   job <job>`), at the cost of whatever it was writing.

2. The admission lock `felis.lolicon.best/maintenance=<kind>@<RFC3339>` on the
   MinecraftServer. felis-api sets it for the milliseconds between admitting an
   operation and creating its Job; it holds for at most two minutes if felis-api
   died in between, and the next wake clears a stale one. To drop it by hand:

   ```sh
   kubectl -n minecraft annotate minecraftserver <name> felis.lolicon.best/maintenance-
   ```

3. The idle-world reaper (§10), which has no Job of its own: it writes the same
   annotation as `reap@<RFC3339>` while it archives and deletes an idle world,
   and rewrites it every 30 seconds, so the lock stays fresh however long the
   archive takes. The refusal names `the idle-world reaper`. A reaper pod that
   dies mid-archive stops rewriting it, and the lock lapses two minutes after
   the last write. Dropping it by hand also ends the reap: the reaper checks
   the lock before it deletes the world volume, keeps the world and retries the
   next day.

A restore, backup or file write refused with `409 not_stopped` although the
panel shows `Stopped` means the game pod is still terminating (its shutdown save
can take a while); retry once `kubectl -n minecraft get pods -l
felis.lolicon.best/server=<name>` shows nothing.

[GO-TESTED: `internal/maintenance`, `k8scluster_maintenance_test.go`,
`handlers_maintenance_test.go`, operator `maintenance_test.go`.]

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
cause — a kaniko build error, the **Trivy CRITICAL-CVE gate** failing the build
(spec §16), or the final push — is in the Job's pod logs and is
[INTEGRATION-ONLY]. The pod runs `egress-gate` (§8f), `context-fetch`, `kaniko`
(builds a tarball, never pushes) and `trivy` (scans that tarball) as init
containers, then `push` — so a CVE-rejected image never reaches the registry. Inspect every step:

```
kubectl logs -n felis-build job/<build-job> --all-containers --prefix
```

A `push` that fails with `403` means the target repository is under `felis/` or
`mirror/` — the registry gate reserves those for the platform (§9); `401` means
the `felis-registry-push` Secret in `felis-build` is missing or stale (re-run the
installer).

### 8e. Build Pods never start: executor images and the scan DBs

A build Job runs Kaniko and Trivy, and Trivy reads two databases: the
vulnerability DB and, for any image with Java artifacts (every real modpack),
the Java DB. The build namespace has no internet egress, so all four come from
the internal registry, under `mirror/`:

| Tool | Upstream | Copy |
|---|---|---|
| kaniko | `gcr.io/kaniko-project/executor:v1.24.0@sha256:4e7a52dd…` | `mirror/kaniko-executor:v1.24.0` |
| trivy | `ghcr.io/aquasecurity/trivy:0.74.0@sha256:62b1e65e…` | `mirror/trivy:0.74.0` |
| vulnerability DB | `mirror.gcr.io/aquasec/trivy-db:2` | `mirror/trivy-db:2` |
| Java DB | `mirror.gcr.io/aquasec/trivy-java-db:1` | `mirror/trivy-java-db:1` |

The executor images are pinned by digest (`internal/build/tools.go`), so a moved
upstream tag never changes what a build runs; the DBs follow their tag. The
installer copies all four with `felis mirror-build-tools`, and
`felis-build-tools.timer` repeats the copy at 04:00 and 16:00, which is what
keeps the DBs current. The watchdog mails a warning when three days pass without
a clean run: scans still gate, but against old advisories.

| Symptom | Cause | Fix |
|---|---|---|
| Build Pods in `ImagePullBackOff` on `mirror/kaniko-executor` or `mirror/trivy` | the first copy has not finished, or the node had no internet during install | `journalctl -u felis-build-tools -n 50`; `sudo felis mirror-build-tools` once the node can reach gcr.io, ghcr.io and mirror.gcr.io |
| Scan fails with `failed to download vulnerability DB` | `mirror/trivy-db:2` is missing | same |
| Watchdog: `the vulnerability DB was last refreshed … ago` | the timer's runs fail (network, registry down, disk) | read the error in the mail or in `/var/lib/felis/build-tools/status.json`; `sudo felis mirror-build-tools` to retry now |

`sudo felis mirror-build-tools -only trivy-db` refreshes one tool. The copy is
single-platform (the node's architecture), written as the `platform` principal
through the loopback hostPort with the token from `/etc/felis/secrets.env`.

**An air-gapped node** cannot fetch anything. Copy the four references above
into the registry from a machine that can (`kubectl -n felis port-forward
svc/registry 5000:5000`, then push to `localhost:5000/mirror/...` as `platform`,
password `kubectl -n felis get secret felis-registry-auth -o
jsonpath='{.data.platform}' | base64 -d`), and repeat that for the DBs as often
as advisories matter to you. The watchdog warning stays until the timer can
reach upstream; that is accurate.

**Kaniko is archived upstream** (June 2025); v1.24.0 is its last release and
gets no security fixes. To run a maintained fork, copy it under `mirror/` and
point the override at it. The other `[registry]` keys in `felis.toml`:

```toml
[registry]
url = "registry.felis.svc:5000"
build_namespace = "felis-build"
kaniko_image = ""                  # empty: the mirror/ copy above
trivy_image = ""
trivy_db_repository = ""
trivy_java_db_repository = ""
build_cpu_limit = "2"
build_mem_limit = "4Gi"
build_disk_limit = "12Gi"          # §8f
build_user_namespaces = "auto"     # §8f: auto | on | off
build_runtime_class = ""           # §8f: e.g. "gvisor"
max_concurrent_builds = 2          # §8f: 1-6; later builds queue
user_uploads_max_bytes = "4Gi"     # every user's uploaded contexts together; 507 uploads_full past it
```

Put them in **both** `/etc/felis/felis.host.toml` (host-side CLI) and
`/etc/felis/felis.pod.toml` (the file rendered into the API's `felis-config`
Secret — the two differ only in the database URL; the setup screens re-render
the Secret from the pod file, so edits made only through `kubectl` on the live
Secret are lost at the next reconfigure). A Deployment restart alone is NOT
enough — the API Pod mounts the Secret, never the host file. Re-render the
Secret from the pod file, then roll `felis-api`:

```sh
kubectl -n felis create secret generic felis-config \
  --from-file=felis.toml=/etc/felis/felis.pod.toml --dry-run=client -o yaml | kubectl apply -f -
kubectl -n felis rollout restart deployment/felis-api
```

Unset fields keep the defaults. The Job's Trivy container runs with
`--insecure`, so the internal registry's plain HTTP works for the DB pulls;
reads need no credential. The registry pruner keeps every tool reference the
api resolves (§9).

### 8f. Build isolation model and residual risk

A Dockerfile's `RUN` steps execute inside the kaniko container, as root. Kaniko
is a daemonless image builder; it is **not** a sandbox. What stands between an
approved-but-hostile Dockerfile and the node is the pod around it:

| Layer | What it does | Where |
|---|---|---|
| Admin approval | Nothing builds until an administrator approves the submission | submit lane |
| Reviewed bytes | the approval names the context's sha256; a re-upload after review fails the approval, and the build refuses any other bytes | submit lane, `felis fetch-context` |
| Weak identity | `felis-build` SA, no Role anywhere, no token mounted | §8b |
| Egress lock | `felis-build-egress`: cluster DNS, the registry, the api internal face, nothing else | §8c |
| Egress gate | first init container; holds the pod until the lock is enforced for it | `felis egress-gate` |
| Capabilities | every container drops ALL; kaniko gets back only CHOWN, DAC_OVERRIDE, FOWNER to unpack base images | jobspec |
| seccomp | the whole pod runs under the runtime's default profile (no `unshare`, `mount`, `keyctl`, `bpf`, …) | jobspec |
| User namespace | with `build_user_namespaces` on, root in the pod is an unprivileged uid on the node | below |
| Sandbox runtime | optional `build_runtime_class` (gVisor, Kata) | below |
| Credentials | the registry credential lives only in the `push` container; the service token only in `context-fetch` | jobspec |
| Resources | CPU, memory and ephemeral-storage limits per container; `activeDeadlineSeconds`; the context extraction stops at 4 GiB or 200 000 entries | jobspec, `felis fetch-context` |
| Namespace backstop | `felis-build-limits` LimitRange gives any container without limits 1 CPU / 1 GiB / 1 GiB disk; `felis-build-quota` allows 8 running pods and no PVCs | bundle |
| Concurrency | at most `[registry] max_concurrent_builds` (default 2, at most 6) builds run; later ones wait as `pending` (Queued) and start oldest first | `build.Builder` |

**Reviewed bytes.** Every upload records the sha256 of the archive, and the
review page shows it. The context download carries the same value in the
`X-Felis-Context-Sha256` header; the API cuts the transfer off if the stored
bytes no longer match it. Approving sends that digest back as
`expected_digest`, and the approval fails with `409 context_changed` when the
submitter has uploaded again since: download and review the new upload. The
approved digest is pinned on the build, and `felis fetch-context --sha256`
hashes every byte it receives; a mismatch fails the build before Kaniko starts:

```
felis fetch-context: the context's sha256 is 3f…, the approved digest is 9a…: it changed after approval; refusing to build
```

The panel approves with the digest of the file it downloaded in the same
session. When the review happened elsewhere (a CLI download, another browser),
it approves with the digest the list shows, so compare that value with
`sha256sum` of the file you actually read. A submission uploaded before digests
were recorded cannot be approved until the submitter uploads it again.

**Egress gate.** The CNI programs a new pod's NetworkPolicy a moment after the
pod starts. On k3s (kube-router), a pod in `felis-build` could reach the internet
and the Kubernetes API for its first ~0.7 s. `egress-gate` dials the Kubernetes
API Service, which the build policy never admits, and exits once it stops
answering. The build pod log shows the wait:

```
felis egress-gate: 10.43.0.1:443 is unreachable after 612ms (...); the egress lock is in effect
```

If the probe still answers after two minutes the gate exits 1 and the build
fails: `the build namespace's NetworkPolicy is not enforced`. The cluster is
running without NetworkPolicy enforcement (a CNI without it, or k3s started with
`--disable-network-policy`); fix the cluster, not the gate.

**User namespaces (`build_user_namespaces`).** With `hostUsers: false`, uid 0
in the build pod maps to an unprivileged uid range on the node, so a container
escape lands as nobody. It needs Kubernetes ≥ 1.33, containerd 2.x, and a kernel
with idmapped mounts on the node filesystem (5.19+ upstream; the RHEL/CentOS
Stream 9 kernels carry the backport). `auto`, the default, lets felis-api decide
at startup: it runs one `userns-probe-*` Job in `felis-build` and turns the
feature on only when that pod ran. The api log says which way it went:

```
felis api: build pods run in a user namespace (hostUsers: false)
felis api: build pods run without a user namespace: this node cannot start a pod with hostUsers: false
```

`on` forces it (builds then fail to start on a node that cannot do it), `off`
never uses it.

**Sandbox runtime (`build_runtime_class`).** Naming a RuntimeClass runs build
pods under it, for example gVisor (`runsc`) or Kata. The class must exist
(`kubectl get runtimeclass`), and kaniko must work under it: gVisor needs its
default `overlay2` rootfs, and Kata needs nested virtualization on a VM node.
Leave it empty unless you have installed and tested one.

**Disk (`build_disk_limit`, default `12Gi`).** This caps kaniko's writable layer
(the unpacked base image) and, as the largest limit in the pod, the pod's total
disk: extracted context, unpacked base image and image tarball together. The
kubelet enforces it by eviction on its housekeeping sweep, so a build that runs
past it is killed within seconds. The build then fails with `Evicted` in
`kubectl -n felis-build describe pod`. Raise it for very large modpacks, and
keep the node's free disk above it.

**Residual risk.** Without a user namespace or a sandbox runtime, the build runs
as root in a container on the same kernel as the game servers and the control
plane. Seccomp and the dropped capabilities remove the common escape primitives.
A kernel vulnerability reachable through the remaining syscalls still reaches
the node, and on a single-node install the node is the whole platform. Admin
approval is the control that remains: read the Dockerfile and download the
context before approving. On a node
where the probe comes back negative, a kernel upgrade that brings idmapped
mounts is the cheapest hardening available.

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
- **Storage:** PVC is RWO, `10Gi` by default (`FELIS_REGISTRY_STORAGE` on the
  first install, `--registry-storage` for `felis manifests`), mounted at
  `/var/lib/registry`, **no `storageClassName`** → binds the cluster default
  class. If the cluster has no default StorageClass the PVC stays `Pending` and
  the registry never starts. On k3s local-path the size is a label: the volume
  is a directory on the node disk and can outgrow it. What bounds the registry
  there is the pruner and the garbage collector below. The uploads PVC
  (`FELIS_UPLOADS_STORAGE`, 5Gi) and the world-archive PVC
  (`FELIS_BACKUP_STORAGE`, 10Gi) work the same way; re-running the installer
  keeps an existing claim's size and warns when the variable asks for another.
  Uploaded build contexts are bounded by `user_uploads_max_bytes` (4Gi for all
  users together, §8e), 2 GiB per user, and 10% free space on the volume; past
  any of them an upload answers `507 uploads_full` or `403 submission_quota_exceeded`. A
  rejected submission's upload is deleted 7 days after the verdict; an approved
  one stays as the source for a rebuild.
- **Unused images are deleted, in two steps.** Every 6 hours felis-api deletes
  the manifests nothing uses (the log says `registry prune finished … deleted=N`).
  It keeps: every whitelist entry (tag, `:*` wildcard, digest), every server's
  pinned image, running builds, the control-plane, Kaniko and Trivy images, the
  Trivy DB copies, anything pushed in the last 24 hours, and the 5 newest tagged
  builds of each `felis/` and `mirror/` repository. To keep an older game build,
  whitelist its versioned tag (§15b). Once a day the `registry-gc` sidecar then
  frees the layers no remaining manifest names: it asks the gate for a read-only
  window after 2 minutes without writes, runs `registry garbage-collect`, and
  hands the window back (`kubectl -n felis logs deploy/registry -c registry-gc`).
  During the window pulls work and every write answers `503` with
  `Retry-After`; the installer and build pushes wait it out. A gate restarted
  mid-sweep comes back read-only until the lease ends. To sweep now:
  `kubectl -n felis exec deploy/registry -c registry-gc -- rm -f /var/lib/registry/.felis-last-gc`
  and restart the pod.
- **The registry volume is lost:** re-run the installer; it pushes every
  platform image again. With the off-site copy on (§16),
  `sudo felis offsite fetch-images` pushes the user images back at their old
  digests, so servers pinned to them pull again; it pushes only what the
  registry lacks, so a second run after an interruption is cheap. Without an
  off-site copy of the images, user images come back from their approved
  submissions (whose uploads the off-site copy also carries):
  the uploaded context of an approved submission stays on the uploads PVC
  (`GET /api/v1/submissions/{id}/context`, its `context_ref` and `image_ref`
  are in `GET /api/v1/submissions`), so an admin can build it again through
  `POST /api/v1/images/build`. Servers pinned to a digest the new registry
  lacks fail to pull until an admin picks a current image for them (§15b).
- **Node-side pulls:** containerd cannot dial the Service VIP (the live stack
  answered "Empty reply"), so the registry Deployment binds a loopback hostPort
  (`127.0.0.1:<port>`) and the installer writes a `/etc/rancher/k3s/registries.yaml`
  mirror relaying `registry.<ns>.svc:<port>` onto it. That pair is what lets
  kubelet re-pull a garbage-collected image; both halves must survive together
  (remove either and every pull after an image GC fails).
- **Memory:** the registry's limit is 2Gi, deliberately larger than the other
  control-plane pods' 256Mi — a live 475MB-layer push OOM-killed the 256Mi
  template mid-upload (audit #46). Very large layers need headroom here, not
  more CPU.
- **Write authorization:** registry:2 listens on the pod's loopback only; the
  `registry-gate` sidecar (`felis registry-gate`, the felis image) owns the port and
  the hostPort. Reads are anonymous — containerd, Kaniko and Trivy pull without a
  credential — but an anonymous `GET /v2/` answers `401 Basic` so docker knows to
  send the credential on a push. Every write needs HTTP basic auth against a token
  in the `felis-registry-auth` Secret: `platform` may write anything (the
  installer's own images, the `mirror/` DB copies); `build` (a build Job's `push`
  container, via `felis-registry-push` in `felis-build`) may write anything outside
  `felis/` and `mirror/` and may never delete. A missing Secret leaves the registry
  read-only rather than down. The tokens persist in `/etc/felis/secrets.env`;
  rotating one means editing it there and re-running the installer, then
  `kubectl -n felis rollout restart deployment/registry` (the gate reads its tokens
  at start).
- **Who can connect:** `felis-registry-ingress` admits only the `felis-build`
  namespace to the registry pod. Node-local traffic (containerd pulls, the
  installer's pushes through the hostPort) is always allowed by Kubernetes; game
  servers cannot reach it at all (`felis-server-egress`).
- **GC pinning:** the registry pod's own images (registry:2 and the felis image
  its gate runs) cannot be pulled from the registry they make up, so the installer
  labels both `io.cri-containerd.pinned=pinned` in containerd and kubelet's image
  GC never collects them. Check with `k3s ctr images ls | grep pinned`.
- **The registry image is pinned by digest**
  (`docker.io/library/registry:2.8.3@sha256:a3d8aaa6…`), and the installer
  caches it with `k3s crictl pull`. On an air-gapped node, carry it over with
  containerd's own export, which keeps the digest ref. On a connected machine
  with k3s or containerd (`R` is the full `docker.io/library/registry@sha256:…`
  ref from `internal/platform/identities.go`):
  `ctr images pull --all-platforms $R && ctr images export --all-platforms
  registry.tar $R`; on the node: `k3s ctr images import --all-platforms
  registry.tar`, then re-run the installer to pin it. A `docker save` round trip
  rewrites the manifest, and kubelet will not match it to the digest.
- **Selector quirk worth knowing:** the registry Service selector is only
  `name + component=registry` — it deliberately lacks the
  `part-of=felis-control-plane` label, so the registry is *invisible* to the
  RCON-peer NetworkPolicy selector. This is intended isolation, not a bug; do not
  "fix" it by adding the label.

Registry manifest rendering is [GO-TESTED]; actual serving is
[CODE-ONLY/INTEGRATION-ONLY].

---

## 10. World reaper: false-deletes and skipped backups (spec §18)

The reaper is a **run-once daily CronJob batch**, not an operator controller.
Every install with an archive store gets it. Without `FELIS_WORLDS_HOST_PATH`
it runs `felis reaper --retention-only`: it deletes backups past their expiry,
reads archives back and sweeps the store (the second half of "A failed reaper
Job" below), never looks at a server, and its summary shows `evaluated=0`. The
installer says so (`idle-world retention is off`). Setting the worlds root on a
re-run turns world reaping on. [GO-TESTED: `TestRunRetentionTouchesNoWorld`,
`TestReaperCronJob_Gating`, `TestReaperCronJob_RetentionOnlyShape`.]

With a worlds root the reaper
reaps a world only when `now - last_active_at > 15d` (`inactive_15d`); the 15-day
deadline is **hard-fixed in code** (only `warn_before` / `retention` /
`max_local_bytes` and the on-demand backup keys `manual_retention` /
`manual_keep` / `manual_cooldown` are configurable from `felis.toml [archive]`).

### What a "backup" contains

A backup tars the server's ENTIRE data volume — the same volume the server mounts
at `/data`: world folders, `server.properties`, plugins/mods, configs, jars,
libraries, logs and cache, not just the `world/` directory. A restore replaces the
volume's contents with the archive (files added since the backup are pruned), so a
restore also rolls config/plugin changes back. Sizes are dominated by
libraries/cache on stock Paper servers (~170MB for a fresh instance before any
world growth) — do not size the archive PVC as if only world data were stored.

### The backup-before-delete invariant

The reap sequence (all [GO-TESTED] hermetically) preserves the world unless a
**confirmed, DB-recorded backup exists**:

0. The world must be at rest before it is archived. A server still meant to
   run is told to stop (`desiredState: Stopped`) and left for the next run; one
   still stopping, whose game pod still exists, or whose world a restore,
   backup or file write holds is left too. Each of these counts in
   `awaiting_stop=` and does not fail the run. Once the server is down, the
   reaper takes the world's maintenance lock (§3b) and holds it through the
   archive and the volume delete: nothing can start the server or touch its
   world in between. A lock the reaper can no longer rewrite, or one someone
   else removed, ends the reap before `DeletePVC`. [GO-TESTED:
   `TestHoldWorldStopsARunningServer`, `TestHoldWorldWaitsUntilQuiet`,
   `TestReapLostHoldKeepsWorld`; live-drilled: a running server was told to
   stop on the first run and archived and deleted under `reap@…` on the next.]
1. `ensureCapacity` (only if `max_local_bytes > 0`) frees room by evicting
   owners' on-demand backups first, oldest first, then reaper archives whose
   off-site copy is confirmed. The only copy of a reaped world is never
   evicted: it stays until `retention` expires it. Still full ⇒ world
   **preserved** (not deleted), counted in `store_full=`. [GO-TESTED:
   `TestCapacityEvictionOrderSparesSoleCopies`; PG-TESTED:
   `TestManualBackupRationing`]
2. `Archiver.Archive` fails ⇒ world **preserved**, PVC untouched.
3. `InsertBackup` (DB) fails ⇒ the orphan archive is deleted, PVC **untouched**.
4. With an `[offsite]` bucket configured (§16), the archive must also be in the
   bucket: until `felis offsite sync` has copied it and set `offsite_at` on its
   `world_backups` row, the run logs `world archived, kept until the archive's
   off-site copy is confirmed`, counts it in `awaiting_offsite=` and leaves the
   PVC alone. The next daily run after the copy reuses the same archive and
   deletes. [GO-TESTED: `TestReapWaitsForOffsiteCopy`]
5. **Only then**, with the lock still held, `DeletePVC` → `ReleaseWorld` →
   audit → `felis_reaper_worlds_deleted_total++`, and the lock is dropped.

An archive the run reuses (step 4's second run, or a reap interrupted after
its archive) is read back end to end and checked against the sha256 recorded
when it was written before the world goes. One that does not match is marked
corrupt, never offered for restore, and replaced by a fresh archive; one that
cannot be read at all keeps the world until the next run. [GO-TESTED:
`TestReapReadsBackReusedArchive`, `TestReapReplacesCorruptArchive`,
`TestReapKeepsWorldWhenReadBackFails`.]

So a missing backup never results in a deleted world, and with a bucket
configured neither does a backup that exists on this disk only. [GO-TESTED:
`TestReapArchiveFailurePreservesWorld`,
`TestReapInsertBackupFailurePreservesWorld`, `TestReapIdleWorldFullSequence`.]

`awaiting_offsite` that stays above zero for more than a day means the copy is
failing: `sudo felis offsite status` (§16).

### A failed reaper Job

Each run ends with one line:

```
felis reaper: evaluated=12 reaped=1 awaiting_offsite=0 awaiting_stop=0 warned=2 skipped=0 store_full=0 evicted=0 expired=3 expire_failed=0 verified=6 corrupt=0 verify_failed=0 swept=0 orphan_archives=0
```

`skipped` counts servers the run failed on (steps 1–3 above, or the cluster or
the database answering with an error; exempt servers and rows whose CRD is gone
are not counted), `store_full` the subset kept because the backup store is full,
and `expire_failed` expired backups it could not remove. `awaiting_stop` counts
idle servers left for the next run because they were not yet down (step 0); it
does not fail the run, but a server that stays there for days is being started
again by something, or a Job keeps holding its world.

After the servers, every run looks after the archive store itself:

- `verified` archives were read back and matched their recorded sha256; each
  run reads back up to ten archives not checked in the past week, oldest check
  first. `corrupt` ones did
  not match (or are gone from the volume) and are marked so: the backup page
  shows them as damaged and refuses to restore them. `verify_failed` ones could
  not be read at all and are retried the next run.
- `swept` counts leftovers of an interrupted archive (`*.partial` files older
  than six hours) the run removed. `orphan_archives` counts finished archives
  no backup record points to; they are kept until they are older than
  `retention`, then removed, and each run lists the first few by path.

`skipped`, `expire_failed`, `corrupt`, `verify_failed` above zero, or a sweep
that could not finish, make the process exit 1: the worlds are safe, but the
Job fails so the watchdog mails `world reaper Job … failed` and
`FelisWorldJobFailed` fires. The Job retries
twice (`backoffLimit`), each retry re-running the whole batch, which is safe
because every step is idempotent. Read the error above the summary:

```sh
kubectl -n minecraft logs job/<the failed felis-reaper-… Job>
```

A server that fails every day keeps its world and is retried every day, so the
Job fails every day until the cause is fixed; after 26 hours the watchdog also
reports `the world reaper has not succeeded for …`. [GO-TESTED:
`TestReportReaperRunFailsTheJob`, `TestExpiryFailureFailsTheRun`,
`TestCapacityStillFullSkipsReap`.]

### On-demand backups ("Back up now")

An owner's "Back up now" writes a `manual` backup into the same store and onto
the same disk as the worlds and the database, so it is rationed:

| Key | Default | Effect |
|---|---|---|
| `manual_retention` | `30d` | when a manual backup expires (reaper archives use `retention`) |
| `manual_keep` | `5` | manual backups kept per server; after each backup the Job removes older ones and prints `removed older backup …` |
| `manual_cooldown` | `10m` | one owner-started backup per server per window; the next one gets `429 backup_cooldown` with `Retry-After` (`0s` disables) |

While the present backups add up to `max_local_bytes` or more, owners get
`507 backup_store_full`. Admins and the break-glass console are exempt from the
cooldown and the cap. Whoever starts it, the backup Job refuses to write an
archive that would leave less than 10% of the archive filesystem free: it fails
with `not enough free disk for the archive`. A failed backup or restore shows
the error its container exited on under Recent operations on the server's
backup page. [GO-TESTED: `TestBackupNow`, `TestCheckRoom`,
`TestReaperConfigManualKeys`, `TestLatestJobsExplainsFailures`]

### Exemptions (world never reaped)

- `spec.reaperExempt=true` → skipped entirely (system servers). [GO-TESTED
  `TestReapExemptServerNeverTouched`.]
- CRD missing → logs `reaper: CRD missing, skipping`, skipped.
- Idle `≤ 15d` → not yet eligible.

### Pre-reap warnings (the `warn_before` offsets)

An OWNED server inside a warning window gets an email notice (`3d`/`1d` before
the deadline, `warn_before` from `[archive]`) to the owner's **verified** email —
the same `[smtp]` relay felis-api uses. The `warned_3d_at` / `warned_1d_at`
stamps record a **delivered** notice:

- No `[smtp]` configured (or owner has no verified address): the run logs
  `reaper: warning suppressed — no warner wired` / a delivery error and does
  NOT stamp. Nothing is falsely recorded as sent, and the day SMTP is
  configured the pending warning can still go out.
- Delivery failure (relay down): logged and retried on the next daily run —
  bounded by the warning window, since the reap removes the candidate anyway.
- `warned=` in the run output counts DELIVERED notices, not attempts.

The reaper runs in the minecraft namespace and reads the **mirrors** of
`felis-smtp` and `felis-config` there (a `secretKeyRef` is namespace-local). The
installed `felis setup`'s "configure email" screen refreshes both mirrors when it
applies, so configuring SMTP after install is enough; a manual edit of the
control-namespace Secret alone is not. [GO-TESTED: the delivered/retried/
suppressed matrix in `internal/reaper`; live-drilled end to end against a local
SMTP sink.]

### Genuine false-delete risk vectors

- **Stale `last_active_at`.** The keep-alive is `RecordJoin`, called from the
  internal `join-event` handler. **If join events are not delivered to the API,
  the activity clock never resets** and an actively-played world becomes
  reap-eligible after 15 days. Verify join events are flowing (§3a) — this is the
  most important reaper check. [INTEGRATION-ONLY for the live Postgres write.]
  A claim also resets it: `ClaimServer` sets `last_active_at` to the claim and
  clears `warned_*`, so a world reaped long ago and claimed again gets a full 15
  days, and a later reap archives the new owner's world rather than reusing the
  previous owner's archive (`FreshBackup` counts only `inactive_15d` archives
  taken since the claim). [PG-TESTED `TestReclaimRestartsReaperClock`.]
- **Unowned servers are still reaped.** A server with `owner_id=""` gets **no
  pre-deletion warning** (`maybeWarn` skips unowned), but is still reaped at 15d.
  [GO-TESTED `TestReapUnownedServerStillReaped`.] Claim or exempt servers you
  want to keep.
- `DeletePVC` is idempotent (missing PVC is not an error), so a re-run will not
  fail on already-reaped worlds.
- **An idle server whose world volume is already gone.** With a fresh reaper
  archive taken since the last activity, the run takes it as a reap that was
  interrupted after the delete and finishes it (release, audit, count). An
  unowned server with no such archive only has its idle clock restarted, so it
  is neither counted nor audited again every day. An owned one is released and
  audited once, since there is no world to archive. [GO-TESTED:
  `TestReapFinishesInterruptedReap`, `TestReapUnownedWithoutWorldRestartsClock`,
  `TestReapOwnedWithoutWorldReleases`.]

Only `TarLocal` (tar+gzip) archiving is implemented; VolumeSnapshot/Longhorn
backends return `not implemented in this build`. The live PVC delete / Postgres
store paths are [INTEGRATION-ONLY].

### Where worlds are read from (hostPath resolution)

The CronJob mounts `--worlds-host-path` read-only at `/worlds` through a static
PersistentVolume (`felis-worlds-root-<digest>`, hostPath type `Directory`,
`Retain`, pre-bound to the same-named PVC in the minecraft namespace), since the
namespace's PodSecurity baseline refuses an inline hostPath in any pod. A
re-install with a different worlds root or `--reaper-node` renders a new pair
under a new digest; the old PV/PVC pair is left behind unused and can be
deleted by hand (Retain: deleting it never touches the directory). The resolver
runs `cmd/felis/reaper.resolveWorldDir`: it looks for `<root>/<pvc>`, then for
the stock local-path directory `<root>/<pv-name>_<ns>_<pvc-name>` derived from
the live PVC's `spec.volumeName` (never a glob — a leftover directory of a
deleted PV must not stand in for the world the PVC currently binds). Pointing
the flag at k3s's storage root (`/var/lib/rancher/k3s/storage`) is therefore the
supported way to enable retention on a stock install. Two deployment facts the
resolver cannot fix:

- **Permissions.** The reaper Pod runs as **root** and carries `DAC_OVERRIDE`:
  k3s's storage root is `0700 root:root`, worlds are written by the game uid
  (1000) — or by root, for a world an older release wrote — and Paper saves
  `level.dat` mode-0600, so a fixed non-root identity (the previous uid-1000
  convention, and the ACL setup that went with it) could neither walk the tree
  nor read the files — every archive failed `open …/level.dat: permission
  denied`. The installer no longer grants uid 1000 any access to the storage
  root: that uid is now the game servers'. If a world is still **preserved**
  while a reap was expected, it is a different cause: check the run's ERROR
  logs for the resolver's `lstat` messages before suspecting permissions.
- **Node placement.** Multi-node clusters: the world's directory exists only on
  the node holding its volume, so pass `--reaper-node`; it pins both the
  CronJob's pod and the PV (single-node starters are pinned implicitly).

---

## 11. Idle auto-stop never fires; player count always shows 0

Every server created from the panel or `felis apply` stops itself after 600 s
with nobody online (`spec.idle`), and the next join wakes it. An admin changes
or turns it off under **Edit server → Idle auto-stop**. A server created before
this default has no `spec.idle` and never stops; `sudo felis converge` gives it
the default (§12b). The login gate and the lobby never idle out, whatever their
spec says.

```sh
kubectl get minecraftserver <name> -o jsonpath='{.spec.idle}'
```

Both idle stop and the player count also hang off **`spec.rcon.enabled`**.
Check it next.

```sh
kubectl get minecraftserver <name> -o jsonpath='{.spec.rcon.enabled}'
```

The player tally is a by-product of the RCON readiness probe: `prober.go` runs
`list` on the same connection that just authenticated, and `parseListReply`
reads the tally from the vanilla/Paper/Fabric/Forge reply (`There are 3 of a
max of 20 players online`), the 1.12/Bukkit reply (`There are 3/20 players
online`) or the EssentialsX reply (`There are 3 out of maximum 20 players
online`, vanished players included), with `§` color codes stripped. With RCON
disabled the probe never runs and no tally is ever read, so a permanent 0 means
"never sampled", not "nobody online".

A tally that could not be read counts as **unknown**, never as zero. Idle
auto-stop only acts on a count it actually read:

```go
if players.Known && server.Spec.Idle.AutoStopEnabled && server.Spec.Idle.EmptySecondsBeforeStop > 0 {
```

An unknown tally leaves `status.players` at the last real count, neither starts
nor clears the empty countdown, and sets the `PlayersCounted` condition to
`False` with reason `ListUnreadable`. So enabling `spec.idle.*` without RCON is
a no-op by design, and a server whose `list` reply is in a format Felis does
not know (a plugin that rewrites `/list`, a translated reply) never idles out:

```sh
kubectl get minecraftserver <name> -o jsonpath='{.status.conditions[?(@.type=="PlayersCounted")]}'
# Reason ListUnreadable: run `list` in the server's panel console to see
# what the server actually answers.
```

Fix it by restoring a supported `/list` (for example, drop the plugin's
override or its translation of that one message). The server keeps running
either way; only the idle stop waits.

Both fields set and still nothing happens? Then the probe is failing rather than
disabled: the server would be stuck in `Starting` with `RconNotReachable`
(`reconciler.go:156`), which is §1's symptom, not this one.

This path used to fail even with everything configured correctly, through three
stacked defects proven and fixed on a live cluster (auditfix21/22): the
`emptySince` stamp was pruned by a missing CRD status field, a quiescent empty
server produced no watch events to re-check the timer, and the Role lacked the
`minecraftservers:patch` grant the stop write needs. If auto-stop ever looks
dead again, check these three in order (each is now pinned by a test):

```sh
# ① The stamp must persist — should print a timestamp, not an empty string,
#    a few seconds after a server goes Ready with zero players.
kubectl get minecraftserver <name> -o jsonpath='{.status.emptySince}'

# ② The operator must be able to write spec.desiredState (403 in the operator
#    log = missing patch grant on Role felis-operator).
kubectl auth can-i patch minecraftservers -n <ns> --as=system:serviceaccount:<ctl-ns>:felis-operator

# ③ A wake-up must be scheduled: while empty, expect whatever you set
#    as emptySecondsBeforeStop to elapse and the box to flip to Stopped without
#    any external action.
```

While players are online the operator re-probes on a 30s cadence so it notices
the moment the last one leaves; while empty it schedules a wake-up exactly at
the deadline. Quiet operator logs on an idle server are normal — the action is
the scheduled wake-up, not a stream of reconciles.

Note the reaper's `last_active_at` (§10) is a *different* subsystem (Postgres
business layer, bumped by join events) — it keeps worlds alive against the
reaper, but it does **not** auto-stop empty running servers.

---

## 12. A configuration field seems to be ignored

Every field below is read by a controller. What varies is the condition that
decides whether setting it does anything.

| Field | What you might expect | Reality |
|---|---|---|
| `spec.startup.timeoutSeconds` | Start budget before `Failed` | Read by `startupTimedOut` (`reconciler.go:479`), called at `:126`. `0` or unset falls back to **300s**, then `markFailed("StartupTimeout")` |
| `spec.startup.readinessTimeoutSeconds` | First-probe budget | Read by `readinessTimedOut` (`reconciler.go:490`), called at `:157`. `0` or unset falls back to **300s**, then `markFailed("ReadinessTimeout")`. Not to be confused with the prober's own 5s dial timeout (`prober.go:45`) |
| `spec.idle.autoStopEnabled` | Auto-stop empty servers | Read at `reconciler.go:175` — but gated on `spec.rcon.enabled`, since the player tally comes from the RCON probe (§11) |
| `spec.idle.emptySecondsBeforeStop` | Empty grace period | Same branch. Must be `> 0`; the guard treats `0` as "off", not "stop immediately" |

Both startup budgets are measured from the same `status.startRequestedAt`, so
`readinessTimeoutSeconds` is not a budget *after* pod readiness — it is a
deadline for the whole start, applied on the RCON-probe branch.

---

## 12b. A newer field never reaches an already-installed system server (`felis converge`)

Provisioning is create-if-absent: `felis setup` never rewrites an existing
`login`/`lobby` `MinecraftServer` beyond the config-derived env it owns, so a
field the desired spec gained after your install sits absent forever — this is
how a deployment ends up with a lobby that has no `spec.rcon` (a dead console
and an online-player count that is always 0) and a login gate without
`spec.startup.healthHTTPPort`. `felis converge` is the explicit pass that fills
exactly those zero-valued fields (and re-adds a derived env key that is
missing). It never overwrites a value that already holds one — an operator's
RCON secretRef or tuning survives.

```
sudo felis converge
```

Run it **after the images are in place**. Enabling RCON, or the HTTP readiness
gate, on a server whose image predates the listener would hold that server in
`Starting` until the operator marks it `Failed` — that ordering is the reason
this is a command you run rather than something setup does on every re-run.
System servers that are already current report `already converged`.

The same pass gives every **user** server whose `spec.idle` was never set (one
created before idle stop became the default) the default: stop after 600 s
empty. A server whose idle stop an admin turned off keeps a duration on its
spec (`autoStopEnabled: false`, `emptySecondsBeforeStop` set), so converge
leaves it off; only servers it actually filled get a line.

---

## 13. World PVC survives after I deleted the MinecraftServer

This is expected. The world PVC is a StatefulSet `VolumeClaimTemplate`. There is
**no `persistentVolumeClaimRetentionPolicy` and no finalizer** anywhere in the
operator. Deleting the `MinecraftServer` garbage-collects the StatefulSet, but
StatefulSet deletion does **not** cascade to its template PVCs, and nothing else
cleans them up. So the world PVC **always survives** server deletion. The
**only** code that deletes a world PVC is the reaper, and only after a verified
backup (§10). To reclaim a world PVC manually:

```
kubectl get pvc -l app.kubernetes.io/name=<name>
kubectl delete pvc <pvc>      # irreversible — the world is gone
```

`spec.storage.retainOnDelete` sat in the CRD and reached no controller. Spec
v4.1 §5 asks for it — 「删除:finalizer 清 Service/STS/ConfigMap,PVC 按
`retainOnDelete`」 — and neither half was ever built: there is no finalizer, and
nothing read the field. It was removed rather than implemented, which is a
deliberate departure from that line, recorded here because the spec is a frozen
document and still says otherwise.

The reasoning is that implementing it buys a second path that deletes a world —
one that skips the reaper's verified-backup check — in order to restore a
finalizer whose other listed duties (Service, StatefulSet, ConfigMap)
ownerReference GC already performs. A CR still carrying the field keeps working:
the API server prunes the unknown key on its next write, and nothing above
changes, because retention was never conditional in the first place.

---

## 13b. Node runs out of disk: what survives, and how to recover

A full disk is the most destructive failure this stack sees: kubelet evicts game
pods (the control plane is protected below), and its image GC then collects
images nothing is running. The images have a pull source now — the in-cluster
registry — so they come back without an operator re-import; freeing space is
what completes the recovery.

**Eviction.** Every control-plane pod (api, operator, reaper, registry) runs
under the BUILT-IN `system-cluster-critical` PriorityClass (value 2e9). Kubelet's
node-pressure eviction refuses to touch those pods — the log shows
*"Eviction manager: cannot evict a critical pod"* for each of them — while
game-server pods at the default priority 0 are evicted first. A drill that filled
the disk to 1.7G free saw exactly this: login/lobby evicted, the whole control
plane still Running (before the fix the same drill evicted the api, operator and
registry too, and the image-GC stage below followed). User-defined
PriorityClasses cannot substitute: the API caps them at 1e9, below kubelet's
critical threshold. The built-in class allows preemption (its policy is fixed),
so a control-plane pod that cannot fit may preempt a game pod — deliberate: the
management plane must be placeable.

**The pressure condition clears slowly.** After you free space, the node can stay
`DiskPressure:True` for up to ~5 minutes
(`--eviction-pressure-transition-period` defaults to 5m, to stop the condition
flapping); pods that need scheduling wait for it. This is the bulk of the
"recovery takes minutes" observation, not a stuck node.

**The images may be gone — they come back on their own.** If pods were evicted,
the kubelet can garbage-collect their images (unused > 2 minutes under imagefs
pressure). Every image this platform runs is ALSO hosted in the in-cluster
registry: the installer builds each one as `registry.<ns>.svc:5000/felis/…`
and mirrors it there, and the node's containerd is configured (a registries.yaml
mirror onto the registry's loopback hostPort) to relay those refs back through
it. So a GC'd image is re-pulled on the next attempt with no operator action —
delete the stuck pod to force an immediate retry (or wait out the backoff), and
the workload converges.

If a pull does NOT come back:

1. Free disk on the node (`df -h /var/lib/rancher`; the biggest consumers are
   `k3s ctr images ls -q` and the world/backup PVCs under
   `/var/lib/rancher/k3s/storage`).
2. Check the registry: `kubectl -n felis get pods -l
   app.kubernetes.io/component=registry` and, on the node,
   `curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:5000/healthz`
   (expect `200`: the registry gate answers it only while registry:2 behind it
   does). An anonymous `GET /v2/` answers `401` by design — see §9.
3. Check the mirror file: `/etc/rancher/k3s/registries.yaml` must map
   `registry.felis.svc:5000` to `http://127.0.0.1:5000`. Missing or changed:
   re-run the installer (it rewrites the file and restarts k3s only when the
   content changed).
4. Re-mirror a tag the registry does not have (hand-built images were never
   pushed): `sudo systemctl start docker` (the installer leaves the daemon
   stopped), log in as `platform` (password: `kubectl -n felis get secret
   felis-registry-auth -o jsonpath='{.data.platform}' | base64 -d`), then `docker tag <ref>
   127.0.0.1:5000/<repo>:<tag> && docker push 127.0.0.1:5000/<repo>:<tag>`.

For an image that is in neither place, the old fallback still stands: re-run the
installer (it rebuilds/re-imports from the local Docker store AND mirrors into
the registry), or for a single image
`docker save felis:<tag> | k3s ctr images import -`. The Docker store remains a
deliberate second copy on the node; treat it as the recovery path, not as free
space.

---

## 14. Health alerts, and metrics for diagnosis (spec §23)

Every full install runs `felis watchdog` from `felis-watchdog.timer`, which
mails the owners when something breaks, with no monitoring stack needed. The
metrics and Prometheus rules below are for a deployment that also runs its own
Prometheus.

### The watchdog: what mails the owners

Every two minutes the host checks:

| Check | Mailed after | Severity |
|---|---|---|
| `felis-api`, `felis-operator` or `registry` Deployment has no available pod (or is missing) | 5 min | critical |
| Kubernetes API unreachable (k3s down): the cluster checks below are then unknown and keep their state | 5 min | critical |
| Login gate not `Running` while it should be | 10 min | critical |
| Other system servers (lobby) not `Running` while they should be | 10 min | warning |
| A user server in `Failed` (§2) | 5 min | warning |
| A backup, restore or reaper Job failed in the last 24h | at once, once | warning |
| The reaper CronJob last succeeded over 26h ago (§10) | at once | warning |
| Node `NotReady`, or kubelet reports Disk/Memory/PID pressure (§13b) | 2–5 min | critical |
| PostgreSQL unreachable | 3 min | critical |
| The game proxy (`felis-velocity`) refuses connections on the game port | 3 min | critical |
| Newest control-plane database backup over 26h old, or none (§16) | 10 min | critical |
| A watched filesystem below 15% free (below 5%: critical) | 15 min (5 min) | warning |
| Host memory available below 10% | 15 min | warning |

How it mails:

- **Recipients** are the verified email addresses of enabled owner accounts.
  The relay is the `[smtp]` one sign-in codes use.
- **One mail per run**, holding everything that came due: new problems, a
  daily reminder for each problem still open, and a resolved notice once a
  problem has stayed gone for 10 minutes. A condition that heals before its
  delay is never mailed; one that turns critical is mailed again at once.
- **During an install** nothing is mailed. `bootstrap.sh` writes
  `/run/felis/watchdog-quiet-until` and removes it when it exits.
- **Caching:** the relay password and the recipient list are cached in
  `/var/lib/felis/watchdog/state.json` (root-only). An outage of PostgreSQL or
  of the API server can therefore still be mailed.
- **No relay or no verified owner address:** each alert is written to the
  journal only.

Commands:

```bash
journalctl -u felis-watchdog -n 40          # every check of the latest runs
sudo felis watchdog -dry-run                # run the checks now; mail nothing, change nothing
systemctl list-timers felis-watchdog.timer  # when it last and next runs
```

A healthy run logs `every check passed`. Otherwise it logs one line per
finding, and the mail's subject once one is sent.

A `-dry-run` from a shell uses the command's defaults, and those do not include
the game-proxy check. The unit carries `-proxy-addr 127.0.0.1:<game port>` and
the disk list the install chose. `systemctl cat felis-watchdog` shows both.

### Metrics

All four mandated metrics have real producers; scrape them when triaging:

- `felis_servers_total` — managed server count.
- `felis_server_phase{server,role,phase,desired}` — 1 for each server's
  current phase. `role` is `login`/`lobby` for system servers and empty for
  user servers. `desired` tells a server that is down on purpose from one that
  failed to come up.
- `felis_build_info{component,version}` — 1 on the process serving it
  (`operator` or `api`). Its absence is how an alert tells which process is down.
- `felis_start_duration_seconds` — histogram, observed once per start when
  readiness is first reached (`ReadySignalAt − StartRequestedAt`). A start that
  never completes (§1) contributes **nothing** here — absence of observations is
  itself the signal that starts are hanging.
- `felis_image_build_failures_total` — increments on build Job failure (§8d).
- `felis_reaper_worlds_deleted_total` — increments only after a world PVC is
  actually deleted post-backup (§10); a spike here means worlds crossed the 15d
  idle line — cross-check that join events are flowing (§10 risk vectors).
- `felis_http_requests_total{face,method,route,code}` and
  `felis_http_request_duration_seconds{face,route}` — every API request, by the
  route pattern it matched (`/api/v1/servers/{name}/status`, never the raw
  path; `unmatched` for a path no route serves). `face` is `internal` or
  `external`. Log streams count as requests but stay out of the latency
  histogram. A rise in `code=~"5.."` on one route narrows a failure to one
  handler; `route="unmatched"` rising is someone scanning.
- The Go runtime and process series (`go_*`, `process_*`) of `felis-api`:
  goroutines, heap, open file descriptors. Goroutines that climb without
  falling back usually mean streams or uploads that never end.

The API also writes one access-log line per request to its log, in logfmt:
`face`, `method`, `route`, `path`, `status`, `duration_ms`, `bytes`,
`request_id` (the id in every error envelope) and `principal` (the user id,
once signed in). Successful probes and scrapes are left out.

```bash
kubectl -n felis logs deploy/felis-api | grep 'msg=request' | grep 'status=5'
kubectl -n felis logs deploy/felis-api | grep 'request_id=<id from the error>'
```

### Scraping

The series come from two processes. Both Services carry the
`prometheus.io/scrape|port|path` annotations, so a Prometheus that discovers
annotated Service endpoints picks them up as is.

- `felis-operator` `:8080/metrics` (Service `felis-operator-metrics`) —
  `felis_servers_total`, `felis_server_phase`, `felis_start_duration_seconds`,
  `felis_build_info{component="operator"}`, and controller-runtime's
  `controller_runtime_reconcile_*` / `workqueue_*` series.
- `felis-api` internal face `:8081/metrics` (Service `felis-api-internal`) —
  `felis_build_info{component="api"}`, the `felis_http_*` request series, the
  `go_*`/`process_*` runtime series,
  `felis_image_build_failures_total`, and the sign-in series of §17
  (`felis_mail_total`, `felis_rate_limited_total`,
  `felis_auth_otp_lockouts_total`, `felis_auth_failures_total`,
  `felis_sessions_revoked_total`, `felis_audit_write_failures_total`).
  Unauthenticated like the probes;
  ClusterIP-only, and the external face never serves it.
- `felis_reaper_worlds_deleted_total` is produced inside the one-shot reaper
  CronJob, which exits long before any scrape interval — without a pushgateway
  it has no scrape path. Read the reaper Pod log or the `world_backups` table
  for deletions instead.

### Alert rules

`deploy/alerts/` ships ready-made rules for these groups:

- **Control plane:** `felis-operator` or `felis-api` down or unscraped
  (`FelisOperatorDown`, `FelisAPIDown`).
- **Servers:** the login gate or another system server not running
  (`FelisLoginGateDown`, `FelisSystemServerDown`), and a user server in
  `Failed` (`FelisServerFailed`).
- **Operator reconcile:** errors piling up (`FelisReconcileErrors`), and a
  pass stuck past its 3-minute bound (`FelisReconcileStuck`). The operator's
  liveness probe restarts a pod whose pass passes 10 minutes; its readiness
  waits for the informer caches to sync.
- **World Jobs (kube-state-metrics):** a failed backup/restore/reaper Job
  (`FelisWorldJobFailed`) and a reaper that has not succeeded in 26h
  (`FelisReaperStale`).
- **Builds and starts:** build failures and slow starts.
- **Node:** disk and memory thresholds, and the kubelet `DiskPressure`
  condition.
- **Database backups:** control-plane backup freshness (§16; needs
  node-exporter's textfile collector).
- **Sign-in abuse (§17):** the mail budget, relay failures, throttled floods,
  account code locks and the refused sign-in rate, plus lost audit rows.

- Plain Prometheus: add `felis-alerts.yaml` to `rule_files`. Check and unit-test
  it standalone with `promtool check rules felis-alerts.yaml` and
  `promtool test rules felis-alerts_test.yml` (the tests pin exactly when each
  alert fires).
- kube-prometheus-stack / prometheus-operator: `kubectl apply -f
  felis-prometheusrule.yaml` (adjust its `release:` label to your stack's
  ruleSelector).

---

## 15. Control-plane upgrades, and rolling back a bad one

There is no in-place updater: an upgrade is re-running the installer
(`curl -fsSL <installer URL> | sudo bash`), which rebuilds/re-imports the image
and re-applies the bundle. `felis update --panel` prints that command with the
script read at the newest release's tag, so the installer and the binary it
downloads come from the same release. (`sudo felis setup` is not this path; on a completed
install it only opens the config console.) The channel is not persisted across
the re-run, so pass `FELIS_VERSION_BOOTSTRAP=dev` on a host that tracks main.
Two properties of the control plane matter when you do:

- Both Deployments use strategy **Recreate** (single replica, no leader election:
  two overlapping instances would fight over the same cluster). An upgrade takes
  the panel/API down for the rollout window — seconds normally, longer if the new
  image still has to be imported.
- If the new pod cannot start (bad tag, missing image), the installer's rollout
  wait fails after 180s and prints `kubectl describe` diagnostics: you see
  `ErrImagePull`/`ImagePullBackOff` there instead of a silent hang.

**What the installer checks before it runs anything it downloaded:**

| Download | Check |
|---|---|
| `felis-linux-<arch>` (release channel) | its sha256 must match the release's `SHA256SUMS`; a release without one, or a mismatch, is compiled from the same tag instead |
| k3s (fresh install, or `FELIS_UPGRADE_DEPS=1`) | the install script is read at `FELIS_K3S_VERSION`'s tag (default `v1.36.4+k3s1`), and it checks the binary against that release's sha256 list |
| cloudflared (when absent, or `FELIS_UPGRADE_DEPS=1`) | release `FELIS_CLOUDFLARED_VERSION` (default `2026.9.1`) against a pinned sha256; another version needs `FELIS_CLOUDFLARED_SHA256` |
| Go toolchain (nano, source builds) | pinned sha256 per architecture; another version needs `FELIS_GO_SHA256` |
| the registry image | pinned by digest (`registry:2.8.3@sha256:a3d8…`) |
| Limbo, its spawn schematic, Paper, LuckPerms, Velocity | the builds and sha256s in `deploy/game-stack.lock`; each image build and the proxy install refuse a download that hashes differently (§15b) |
| the Temurin JRE the proxy runs on | release `25.0.4.1+1` against a pinned sha256 per architecture |
| base images of the felis, limbo, lobby and paper images | pinned by digest in each `Dockerfile` |

On a public repository each release also carries a signed build-provenance
attestation. Check a downloaded binary with
`gh attestation verify felis-linux-amd64 --repo FelisMC/Felis`; it names the
workflow run and commit that built it.

Each release runs under its own image tag, `felis/felis:v1.2.3` (a source
build's stamp `v1.2.3+gabc1234` becomes `v1.2.3-gabc1234`; a build that reports
no version uses `:demo`). At the end of an upgrade the installer prints the tag
it moved away from, and keeps it in `/etc/felis/previous-felis-image`. Roll
back with:

```
kubectl -n felis rollout undo deployment/felis-api deployment/felis-operator
kubectl -n felis rollout status deploy/felis-api
```

`rollout undo` returns each Deployment to its previous ReplicaSet, which names
the previous release's tag. That image is normally still on the node; if the
image GC collected it, the registry re-serves it (§13b): the pruner keeps the
five newest `felis/felis` tags and every image a game pod still runs. A rerun
of the same version reuses its tag and restarts both Deployments onto the
rebuilt image, so after such a rerun undo lands on that same tag again. The
next installer run re-applies the bundle and moves the image to whatever that
run installs.

A platform upgrade leaves running game servers alone. Their init containers
run the felis image too; a running server keeps the one it started with and
picks up the new release on its next start. A release that changes the game
pod in any other way still restarts running servers once, as a server edit
does.

**What a rerun restarts.** Each of these drops every connected player (or cuts
felis-api's open transactions), so the installer restarts one only when what it
runs changed:

| Component | Restarted when |
|---|---|
| `felis-velocity` (the proxy) | its unit, the JRE, `velocity.jar`, `velocity.toml`, the forwarding secret, the felis-link settings or a plugin jar changed, or it was not running. The fingerprint lives in `/etc/felis/velocity.fingerprint`; delete it to force a restart. |
| login and lobby pods | the rebuilt limbo or lobby image has a new image ID (`/etc/felis/system-server-images`). Each restarts on its own. The installer turns off BuildKit's default provenance attestation (`BUILDX_NO_DEFAULT_ATTESTATIONS=1`): it records the build time, which would give every rebuild a new ID. |
| PostgreSQL | first install only (`listen_addresses` needs a restart). A rerun reloads the configuration, which keeps connections open. |
| felis-api, felis-operator, the registry pod (its gate and GC containers run the felis binary) | the image tag changed (an upgrade), or a same-version rerun rebuilt it. |

**PostgreSQL across reruns.** On hosts without firewalld the installer loads an
nftables table, `inet felis_postgres`, from `felis-postgres-firewall.service`:
port 5432 accepts loopback, the pod network and the node's own address and drops
everything else (`nft list table inet felis_postgres`). firewalld hosts already
keep 5432 closed to the network. The installer also refuses to start a
PostgreSQL whose major version differs from the cluster in the data directory,
and prints the `pg_upgrade` steps; distributions that move the server package to
a new major (Arch, Fedora) would otherwise leave the database unable to start.
On Arch the installer's `pacman -Syu` holds `postgresql` back once a cluster
exists, so the database is upgraded only when you run `pg_upgrade` yourself.

`rollout undo` reverts the image only. The upgrade's database migrations stay
applied; when they are the problem, restore the `pre-migrate` bundle the upgrade
took (§16, "Roll back an upgrade that broke the database").

**Schema guard.** felis-api, the reaper, the off-site copy and `felis migrate up`
compare the migrations the database records with the ones their build embeds, as
sets. A database a newer release migrated stops them with `database schema is
newer than this felis build: it records migration 0026, and this build knows
migrations up to 0025`, so an undo across an upgrade that migrated shows up as
felis-api in CrashLoopBackOff until the `pre-migrate` bundle is restored. A
database still missing migrations stops felis-api, the reaper and the off-site
copy with `database schema is behind this felis build` until `felis migrate up`
runs; `felis setup`'s preflight applies them itself. [PG-TESTED]

**A failed rerun and the host binary.** The installer replaces
`/usr/local/bin/felis` early (the steps after it run the new binary) and keeps
the old one as `felis.prev` until the new one is in use. A run that fails before
the database migrations start puts the old binary back, so the host timers and
`felis setup` keep matching the database and the control plane that are still
running; a rerun continues from there. Once migrations have started, the new
binary stays. [VM-VERIFIED]

## 15b. Game images, pinned builds, and moving a world to a newer Minecraft

Each release pins the upstream builds it installs in `deploy/game-stack.lock`:
the Limbo CI build and its Minecraft version, the Paper build for that version,
LuckPerms and Velocity, each with its sha256. Every host installing one release
builds the same login gate, lobby and plain-Paper image, and a rerun of the same
release rebuilds nothing. Moving the stack to newer upstream builds is a release
change: `bash deploy/update-game-stack-lock.sh` resolves and hashes upstream's
newest builds and rewrites the lock (`--check` only reports whether upstream
moved on). Two installer knobs leave the lock:

- `FELIS_GAME_STACK=latest` resolves upstream's newest builds at install time,
  hashes the ones that publish no digest, and warns that they are not the
  release's builds.
- `FELIS_VELOCITY_VERSION=<minor>` installs that minor's newest Velocity build
  (content-addressed, so still sha256-checked).

`FELIS_JRE_VERSION` picks the proxy's Java feature release (default 25, pinned
to Temurin `25.0.4.1+1`). A JRE the installer put there moves to the pinned
build on the next rerun; a JRE from any other vendor is left in place.

The platform's game images live under mutable tags
(`registry.felis.svc:5000/felis/paper:demo`): an installer run that builds a
different stack pushes the new build over the same tag. A server never follows
that tag on its own. felis-api stores the image a server is
created with pinned to the digest the tag named at that moment
(`…/felis/paper:demo@sha256:…`), and the installer's `pin_user_server_images`
step pins any older server still on a bare tag *before* it pushes the new
builds. Kubernetes pulls a digest-qualified ref by the digest, so a pinned
server wakes on exactly the build it was created on, however often the tag moves.

Each installer run also pushes every game build under a tag no later run
rewrites, `<Minecraft version>-<12 hex of the image id>`
(`felis/paper:26.2-3f9c0a1b2c4d`). Whitelist one of those to create servers on a
specific Minecraft version after `:demo` has moved on.

Moving an existing world to a newer build is an explicit step: pick the image in
the panel's **Edit server** dialog (re-picking the current tag moves it to that
tag's newest build) and tick the backup confirmation. Over the API the same
PATCH needs `"confirmImageChange": true`, or it is refused with `409
image_change_unconfirmed`. Back the world up first: Minecraft upgrades chunks
as it loads them, and the old version cannot open them again. The audit log keeps
`image_from`/`image_to` for every change, so the exact previous build can be set
back (it is admitted by its tag) together with a restore of the pre-upgrade
backup. That rollback works while the registry still holds the old build: a
build no server and no whitelist entry names is pruned after 24 hours, and the
5 newest builds of each `felis/` repository are kept regardless (§9).

| Symptom | Cause | Fix |
|---|---|---|
| Create/edit refused with `image_not_in_registry` | the whitelisted tag was never pushed to the internal registry, or was deleted | push or rebuild the image, then retry |
| Create/edit refused with `registry_unavailable` | felis-api could not reach `registry.felis.svc:5000` | `kubectl -n felis get pods -l app.kubernetes.io/component=registry`; check the `felis-registry-ingress` NetworkPolicy still admits felis-api |
| Installer warns `could not pin every user server` | the registry was down, or a server names a tag the registry lost | fix the registry, then `sudo felis pin-images` before starting those servers; a server whose tag is gone keeps its bare tag until an admin picks a new image |
| A running server restarted during an installer re-run | it was pinned in place: the operator rolled it onto the pinned ref, the build it already ran | nothing; it happens once per server |
| Create/edit refused with `the registry no longer holds build …` | the image names a digest the pruner deleted: nothing referenced it for 24 hours (§9) | pick a current tag; whitelist the versioned tag of a build you want kept |

## 16. Control-plane database backups and disaster recovery

The PostgreSQL database behind felis-api holds everything that is not a world:
accounts, passkeys, Minecraft account links, server ownership, quotas, audit
logs, and the `world_backups` index that maps an archive (§10) back to its
owner. Losing it orphans every world archive. It lives on the host (not in
k3s), so it is backed up on the host too.

### What runs, and where the bundles go

- **`felis-db-backup.timer`** runs `felis db backup` daily at
  `FELIS_DB_BACKUP_TIME` (default `*-*-* 03:30:00`, plus up to 15 min random
  delay). `Persistent=true` catches up at boot after the host was off at that
  time. The installer takes the first backup during the install, so a broken
  pipeline shows up there. [CODE-ONLY; unit and timer content GO-TESTED in
  `deploy/bootstrap_test.sh`]
- **Every upgrade** (`felis migrate up`, which the installer runs) takes a
  `pre-migrate` bundle first when the database already has data and a
  migration is pending, and **applies nothing** if that backup fails
  (`pre-migration backup failed, nothing applied`). [GO-TESTED]
- **Every restore** takes a `pre-restore` bundle of the database it is about to
  replace (skip with `-no-safety-backup`). [GO-TESTED]

Bundles land in `FELIS_DB_BACKUP_DIR` (default `/var/lib/felis/db-backups`,
mode 0700; outside `/var/lib/rancher` so a k3s reinstall cannot take them
along). One bundle is `felis-db-<UTC stamp>-<label>.tar`:

| Member | Content |
|---|---|
| `MANIFEST.json` | version, schema version, `pg_dump --version`, sha256 of every member |
| `db.dump` | `pg_dump --format=custom` of the `felis` database |
| `state/etc/felis/...` | `secrets.env` (DB password, session/forwarding secrets, registry tokens), `felis.host.toml`, `felis.pod.toml`, the `felis.toml` symlink, the panel TLS pair. `bootstrap.done` is left out on purpose |
| `k8s/minecraftservers.json` | every MinecraftServer, status and server-side metadata stripped, ready for `kubectl apply` (best effort: when the cluster did not answer, the manifest records why) |

next to a `.sha256` sidecar in `sha256sum` format. **A bundle contains the
secrets; treat it like `/etc/felis` itself.** Retention per label: `daily` 14
(`FELIS_DB_BACKUP_KEEP`), `pre-migrate` 10, `pre-restore` 5, `manual` never
pruned.

Installer knobs: `FELIS_DB_BACKUP_DIR`, `FELIS_DB_BACKUP_KEEP`,
`FELIS_DB_BACKUP_TIME`, `FELIS_DB_BACKUP_METRICS` and
`FELIS_PRE_MIGRATE_BACKUP` (below).

### Is the newest backup fresh?

Three places answer, all with the same 26 h limit:

- The panel: **管理 → 维护与备份** shows the newest backup, its kind and size,
  and turns red with the fix commands when it is missing or overdue (read from
  the `db_backup_last` platform setting each backup writes).
- `sudo felis db check` exits 1 with the reason; `sudo felis db list` shows every
  bundle with its age.
- Prometheus: `FelisDBBackupStale` (critical) and `FelisDBBackupMetricMissing`
  (warning) in `deploy/alerts/`. They read
  `felis_db_backup_last_success_timestamp_seconds`, which each daily run writes
  to `FELIS_DB_BACKUP_METRICS` (default
  `/var/lib/node_exporter/textfile_collector/felis_db_backup.prom`). Point
  node-exporter's `--collector.textfile.directory` at that directory, or
  `FelisDBBackupMetricMissing` fires after 2 h.

When a backup is overdue:

```
sudo systemctl status felis-db-backup.timer          # enabled? next run?
sudo journalctl -u felis-db-backup -n 50 --no-pager   # why the last run failed
sudo felis db backup                                  # take one now (label manual)
```

Common failures: PostgreSQL down (`pg_dump: ... connection refused`); the
backup directory's disk full (the half-written `.partial` is removed and the
previous bundles stay intact); `pg_dump: server version mismatch` when an
external database is newer than the host's client tools (install the matching
`postgresql` client package).

### Check a bundle

```
sudo felis db verify felis-db-20260924T033012Z-daily.tar   # bare names resolve in the backup dir
sha256sum -c felis-db-20260924T033012Z-daily.tar.sha256    # on a copy, without felis
```

`verify` checks the sidecar, every member against the manifest, that nothing
is missing or unlisted, and that the manifest comes first. It does not touch
the database.

### Restore on the same host (undo a mistake)

```
kubectl -n felis scale deployment felis-api felis-operator --replicas=0
sudo felis db restore -yes felis-db-20260924T033012Z-daily.tar
sudo felis migrate up -config /etc/felis/felis.host.toml
kubectl -n felis scale deployment felis-api felis-operator --replicas=1
```

- Without `-yes`, restore prints what the bundle holds and exits 2.
- It refuses while other clients are connected (`other clients are connected to the database (N)`)
  and prints the scale command; `-force` overrides, for a client you know is
  idle.
- The replay is one transaction: it drops everything the `felis` role owns and
  loads the dump. **Any failure rolls back and leaves the database exactly as it
  was** (`rolled back, the database is unchanged`, with the psql and pg_restore
  errors). [GO-TESTED]
- The database before the restore is in the `pre-restore` bundle it names;
  restoring that one undoes the restore.
- `migrate up` brings an older bundle's schema up to the running release.
  Nothing migrates at startup, so skip it only when you are rolling back to the
  release that wrote the bundle (next section).
- Restore replaces the database only. World data (PVCs and archives, §10, §13)
  is not in the bundle and is not touched; a server created after the bundle
  keeps its PVC but loses its owner row.

### Roll back an upgrade that broke the database

The installer's migrations only roll forward. The `pre-migrate` bundle taken by
the upgrade is the way back:

```
kubectl -n felis scale deployment felis-api felis-operator --replicas=0
sudo felis db list | grep pre-migrate                  # newest one is the upgrade's
sudo felis db restore -yes <that bundle>
kubectl -n felis rollout undo deploy/felis-api
kubectl -n felis rollout undo deploy/felis-operator
kubectl -n felis scale deployment felis-api felis-operator --replicas=1
```

Do **not** run `felis migrate up` here: the host binary is already the new
release and would re-apply the migrations you are rolling back. Re-run the
older installer version to bring the host binary back in line.

### Rebuild on a new host (the old one is gone)

This needs the off-site copy (next section) or a bundle you copied off the old
host yourself, plus the off-site encryption key if the copy is in the bucket.

1. Get the newest bundle. From the bucket, with any `felis` binary of the same
   or a newer release (the `felis-linux-<arch>` release asset runs on its own;
   this works from any machine that can reach the bucket):

   ```
   export FELIS_OFFSITE_ACCESS_KEY=... FELIS_OFFSITE_SECRET_KEY=... FELIS_OFFSITE_KEY=...
   sudo -E felis offsite fetch-db -endpoint https://s3.example.com -bucket felis-backups \
        [-region ...] [-prefix ...] latest
   ```

   It writes the bundle into `/var/lib/felis/db-backups` (`-dir` to change),
   checks it (`felis db verify`) and names it. A wrong key fails with
   `object does not decrypt with this key` and writes nothing. For a copy you
   made yourself, check it with `sha256sum -c felis-db-....tar.sha256`.
2. Put the old host's state in place **before** installing, so the installer
   reuses the same DB password, session secret, forwarding secret and the
   `[offsite]` bucket with its credentials and key (`offsite.env`):

   ```
   sudo install -d -m 0700 /etc/felis
   sudo tar -xpf felis-db-....tar -C / --strip-components=1 state/etc/felis
   ```

3. Run the installer as for a first install. `bootstrap.done` is not in the
   bundle, so it takes the fresh-install path, creates the empty database with
   the restored password and migrates it. It finds `[offsite]` in the restored
   `felis.host.toml` and turns the hourly copy back on.
4. Push the user images back into the new registry:

   ```
   sudo felis offsite fetch-images
   ```

   It restores the newest image list in the bucket (`-at <stamp>` for an
   older one; `felis offsite list` shows them) through the registry's loopback
   port as the platform principal, verifying every manifest and layer against
   its digest, and pushes only what the registry lacks. The pruner counts a
   restored image as freshly pushed and keeps it for 24 hours; finish the next
   step within that window so the restored servers and whitelist entries keep
   naming it.
5. Put the submission uploads back:

   ```
   sudo felis offsite fetch-uploads
   ```

   It writes every upload of the newest upload list (`-at <stamp>` for an
   older one) into the `felis-uploads` volume, owned by the control plane's
   uid, checking each against its sha256, and leaves one already in place
   alone.
6. Restore the database and bring the servers back:

   ```
   kubectl -n felis scale deployment felis-api felis-operator --replicas=0
   sudo felis db restore -yes -no-safety-backup /var/lib/felis/db-backups/felis-db-....tar
   sudo felis migrate up -config /etc/felis/felis.host.toml
   kubectl -n felis scale deployment felis-api felis-operator --replicas=1
   tar -xOf felis-db-....tar k8s/minecraftservers.json | kubectl apply -f -
   ```

7. Bring the world archives back into the archive volume:

   ```
   sudo felis offsite fetch-worlds
   ```

   It fetches every archive the restored `world_backups` index lists as present
   and the volume lacks, provisioning the `felis-backups` volume first if
   nothing has used it yet (a short-lived `felis-bind-felis-backups-*` pod). It
   lists any it could not find in the bucket. Restore a world from its archive
   as usual (§10, §13).

### Keep a copy somewhere else

A bundle on the same disk as the database protects against mistakes and bad
upgrades, and a world archive on the same disk as the worlds protects against
a deleted server. Neither survives losing the disk, and neither do the user
images in the platform registry or the modpacks users uploaded for review. The
installer's off-site copy sends all four to an S3-compatible bucket (AWS S3,
Cloudflare R2, Backblaze B2, MinIO, ...), encrypted on this host:

```
FELIS_OFFSITE_ENDPOINT=https://<account>.r2.cloudflarestorage.com \
FELIS_OFFSITE_BUCKET=felis-backups \
FELIS_OFFSITE_ACCESS_KEY=... FELIS_OFFSITE_SECRET_KEY=... \
  bash deploy/bootstrap.sh          # or the curl | sudo bash one-liner
```

Optional: `FELIS_OFFSITE_REGION`, `FELIS_OFFSITE_PREFIX` (a key prefix, so one
bucket can hold several installs) and `FELIS_OFFSITE_DB_KEEP` (default 30).
The installer writes `[offsite]` into `felis.toml`, keeps the credentials and
a generated encryption key in `/etc/felis/offsite.env` (mode 0600), and
**prints the key once**. Store it in a password manager: the bucket holds only
sealed objects, and without the key they cannot be read. A later re-run keeps
the key; it refuses a `FELIS_OFFSITE_KEY` that differs from the one in
`offsite.env`, since every object already in the bucket is sealed with it.
Without a bucket the installer ends with `NO OFF-SITE COPY`.

What runs:

- **`felis-offsite.timer`** runs `felis offsite sync` hourly (plus up to
  10 min random delay, `Persistent=true`). Each run copies every world archive
  whose row has no `offsite_at` yet and records it, copies the newest
  `db_keep` database bundles the bucket lacks and prunes older ones there, and
  deletes a world archive from the bucket once its row has been deleted and
  its retention (`expires_at`) has passed. An object already in the bucket at
  the right size is recorded without being sent again, so a run cut short
  resumes. [GO-TESTED: `internal/offsite`]
- The same run copies the user images in the platform registry: every
  repository outside `felis/` and `mirror/`, each manifest the registry's index
  lists and every layer it names, read through the loopback hostPort. A layer
  shared by many images is stored once. The installer pushes `felis/` and
  `mirror/` again on a new host, but at new digests, so from those the run
  copies only the revisions a MinecraftServer or a whitelist entry pins by
  digest (a server created from the platform's Paper image, for one), without
  their tags; a restore puts them back by digest and leaves the installer's
  tags alone.
  When the set changed, a new version of the image list is written; versions
  replaced more than 14 days ago are dropped together with the layers only
  they named, so an image deleted by mistake stays restorable for two weeks
  (`fetch-images -at`). A manifest the registry lost mid-run keeps its earlier
  copy and is listed under `not whole:` in `status`. The copy covers the
  in-cluster registry that `[registry] url` names; `-registry host:port` points
  it elsewhere, `-registry off` skips images. [VM-TESTED: 16 images, 638 MiB,
  restored into an empty registry at the same digests]
- It also copies the submission uploads (`sub-*/context.tar.gz` on the
  `felis-uploads` volume, §8), the source an admin rebuilds an approved image
  from. Identical uploads are stored once; the upload list is versioned and
  kept for 14 days like the image list (`fetch-uploads -at`). A context is read
  again only when its size or modification time changed. It runs when
  `[registry] user_uploads_context` is a local path (the installer's default);
  `-uploads-dir` names the directory by hand, `-uploads-pvc ""` skips it.
  [VM-TESTED: 17 uploads in 10 objects, 200 MiB, restored byte-identical]
- Objects are `worlds/<archive>.fenc`, `db/<bundle>.fenc`,
  `registry/blobs/<sha256>.fenc`, `registry/manifests/<sha256>.fenc`,
  `registry/index/<stamp>.json.fenc`, `uploads/blobs/<sha256>.fenc` and
  `uploads/index/<stamp>.json.fenc`: AES-256-GCM in 64 KiB segments, so
  truncation, reordering and a wrong key are all refused on the way back.
- The reaper deletes an idle world only after its archive is in the bucket
  (§10).
- The watchdog mails the owners when no sync has completed for 12 hours
  (`the off-site copy last completed ... ago`).

Checking it:

```
sudo felis offsite status        # last run, errors, what the bucket holds, what waits
sudo felis offsite list          # the bundles, image lists and upload lists in the bucket, newest first
sudo journalctl -u felis-offsite -n 50 --no-pager
sudo systemctl start felis-offsite.service   # run one now
```

`status` exits 1 when no sync has completed in 12 hours. `missing:` lines are
archives the database records but the volume no longer has (an archive
removed by hand); there is nothing left to copy for those.

To change the bucket, edit `[offsite]` in `/etc/felis/felis.host.toml` (and
`offsite.env` for new credentials) and re-run the installer; to turn the copy
off, delete the section and re-run. Keeping the same key across buckets keeps
old copies readable.

Without a bucket, copy the backup directory off the host on a schedule of your
own (`rsync -a root@felis-host:/var/lib/felis/db-backups/ /backups/felis-db/`,
with the `.sha256` sidecars; `sha256sum -c` on the far side proves the copy).
That covers the database only; the world archives are under the
`felis-backups` volume's directory in `/var/lib/rancher/k3s/storage/`, the
registry's images under the `registry` volume's (`*_felis_registry`) and the
uploads under the `felis-uploads` volume's (`*_felis_felis-uploads`).

### `FELIS_PRE_MIGRATE_BACKUP=0`

Skips the pre-migration snapshot (`migrate up -no-backup`). The installer warns
loudly when it is set. Use it only when the snapshot cannot work and you have
another backup, e.g. an external database newer than the host's `pg_dump`.

## 17. Sign-in refused with 429, mail budget, account code locks, failed sign-ins

The public sign-in doors (`/api/v1/auth/*` except logout and the op-login
status poll) have three limits of their own. Each answers 429 with a
`Retry-After` header and a distinct error code.

### `rate_limited`: one address called the doors too often

Each client address gets 20 calls at once, refilled at 20 a minute, shared
across every door. A person signing in makes three or four calls, so this only
bites scripts. IPv6 clients share one limit per /64. Refusals count in
`felis_rate_limited_total{scope="auth_door"}`; `FelisSignInFlood` fires when
more than 10 a minute are refused for 10 minutes.

The address comes from `[auth] client_ip_header`:

- Behind the Cloudflare tunnel it is `CF-Connecting-IP`. The edge setup writes
  it, and an install with an `access_jwt_aud` implies it. The header is
  trustworthy there because the same setup fences the panel NodePort to
  loopback, so every request reaching the API came through cloudflared.
- Behind your own reverse proxy it is `X-Forwarded-For` (the rightmost entry,
  the one your proxy appended). Firewall the NodePort so only the proxy reaches
  it, or a direct caller can write any address it likes.
- Unset, the TCP peer is used. Behind any proxy every visitor then shares the
  proxy's address and one limit, so **everyone gets `rate_limited` at once**.
  The `felis api` log says at start which it keys on (`sign-in rate limit keys
  on ...`). Set the header in `/etc/felis/felis.toml` and
  `/etc/felis/felis.pod.toml`, then `felis converge`.

### `mail_rate_limited`: the install-wide mail budget is spent

Every code and notice the API mails spends one token of a single budget,
`[smtp] max_per_hour` (default 120; a quarter of it may go at once), so a flood
cannot burn the relay's quota and get the sending account suspended. While it
is spent, every address gets the same 429 and nothing reaches the relay.
`felis_mail_total{result="throttled"}` counts refusals and
`FelisMailBudgetExhausted` fires on the first one. Look at
`felis_rate_limited_total` first: a flood shows there. If sign-ins are real,
raise `max_per_hour` to what your relay allows.

`FelisMailDeliveryFailing` is the other half: the relay itself refused mail
(`felis_mail_total{result="failed"}`, 502 `mail_undeliverable` to the caller).
The relay's reason is in the `felis-api` log.

### `otp_account_locked`: ten wrong codes in 24 hours

Ten wrong email codes for one account within 24 hours, counted across every
code it was sent, lock that account's email-code sign-in until 24 hours after
the first miss. The public doors answer a locked account exactly like a wrong
code, and the owner gets one mail saying so. Signed-in doors (email
verification, migration step-up) answer 429 `otp_account_locked`. Passkey
sign-in keeps working. Each lock is audited as `auth.otp.locked` and counted in
`felis_auth_otp_lockouts_total{purpose}` (`FelisOTPAccountLocked`).

To lift a lock early once you have confirmed the owner locked themselves out:

```sh
sudo -u postgres psql felis -c \
  "DELETE FROM otp_failure_windows WHERE user_id = (SELECT id FROM users WHERE username = '<name>');"
```

### Who tried: the audit trail

Every refused sign-in writes an `auth.<door>.failed` row (payload `reason`:
`bad_code`, `no_account`, `staff_account`, `not_staff`, `bad_assertion`, ...)
and counts in `felis_auth_failures_total{door,reason}`; `FelisSignInFailures`
fires above 30 in 15 minutes. The first refusal of each throttled burst writes
`auth.rate_limited` with the source address. Rows carry `actor_user_id` (the
account, the column to attribute by), `client_ip` (the same address the limit
keys on) and `user_agent`. `actor` is display text: a verified email or the
username, never an address the caller set without verifying.

```sh
sudo -u postgres psql felis -c "
  SELECT created_at, action, actor, client_ip, payload->>'reason' AS reason
  FROM audit_logs
  WHERE action LIKE 'auth.%' AND created_at > now() - interval '1 hour'
  ORDER BY created_at DESC LIMIT 50;"
```

A failed audit write does not fail the action; it logs `audit: lost ...` in
`felis-api` and counts in `felis_audit_write_failures_total`
(`FelisAuditWriteFailing`). The cause is almost always PostgreSQL (§16).

### Optional: a Cloudflare rate limiting rule in front

The limits above live in the API, so they hold on any edge. Behind Cloudflare
you can also stop floods before they reach the tunnel: Security → WAF → Rate
limiting rules, match URI Path starts with `/api/v1/auth/` on the console and
op.console hostnames, count by IP, 30 requests per 10 seconds, action Block
for 10 seconds (the Free plan's limits).

---

## Quick reference: symptom → section

| Symptom | Section |
|---|---|
| Stuck `Starting`, never `Running` | §1 |
| `Starting` with `PodNotReady` (image? PVC? boot?) | §1a |
| RCON secret/auth/port errors | §1b, §1c |
| Phase `Failed` | §2 |
| Players land in lobby / wrong place | §3, §4 |
| Wake refused / rate-limited (403/409/429/503) | §3a |
| `maintenance_in_progress`; server won't start after a restore | §3b |
| Routing disabled, offline-mode | §4 |
| Panel 401/403; fails-closed; audience error | §5 |
| Local password login rejected | §5c |
| Internal callers 401 (service token) | §6 |
| Link/claim 400/409/412/403/404 | §7 |
| Build push 400 / SA denied / egress hang / Failed / executor ImagePullBackOff | §8, §8e |
| Registry push/pull unreachable | §9 |
| World deleted unexpectedly / backup skipped | §10 |
| Reaper `awaiting_stop` stays above 0; `corrupt=` / backup shown as damaged; `orphan_archives` | §10 |
| Idle auto-stop not firing; player count 0; `PlayersCounted=False` | §11 |
| A config field seems ignored | §12 |
| PVC left behind after delete | §13 |
| Node out of disk; pods evicted / ImagePullBackOff | §13b |
| Which metric to scrape | §14 |
| An alert mail from the watchdog; nothing is mailed when something breaks | §14 |
| `FelisOperatorDown` / `FelisAPIDown` / `FelisLoginGateDown` / `FelisReconcileStuck` | §14, §1, §2 |
| Upgrade / roll back a bad control-plane image | §15 |
| `image_change_unconfirmed` / `image_not_in_registry` / `registry_unavailable`; move a world to a newer Minecraft | §15b |
| Database backup overdue / `FelisDBBackupStale` / panel shows 从未备份 | §16 |
| `pre-migration backup failed, nothing applied` during an upgrade | §16 |
| Undo a mistaken change / restore the control-plane database | §16 |
| Host lost: rebuild from a database bundle | §16 |
| `the off-site copy last completed ... ago` / `NO OFF-SITE COPY` / reaper `awaiting_offsite` stays above 0 | §16, §10 |
| Sign-in 429 `rate_limited` for everyone at once | §17 |
| 429 `mail_rate_limited` / `FelisMailBudgetExhausted` | §17 |
| Right code refused; `otp_account_locked` / `FelisOTPAccountLocked` | §17 |
| `FelisSignInFailures` / who is guessing, from where | §17 |
| `FelisAuditWriteFailing` | §17 |
