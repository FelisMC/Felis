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

A restore, backup or file write refused with `409 not_stopped` although the
panel shows `Stopped` means the game pod is still terminating (its preStop save
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
[INTEGRATION-ONLY]. The pod runs `kaniko` (builds a tarball, never pushes) and
`trivy` (scans that tarball) as init containers, then `push` — so a CVE-rejected
image never reaches the registry. Inspect every step:

```
kubectl logs -n felis-build job/<build-job> --all-containers --prefix
```

A `push` that fails with `403` means the target repository is under `felis/` or
`mirror/` — the registry gate reserves those for the platform (§9); `401` means
the `felis-registry-push` Secret in `felis-build` is missing or stale (re-run the
installer).

### 8e. Build Pods never start: executor images and air-gapped installs

The build Job runs Kaniko and Trivy from external registries by default
(`gcr.io/kaniko-project/executor:latest`, `aquasec/trivy:latest`). On a box whose
build namespace cannot reach those registries (the egress policy allows only
DNS, the internal registry and `--package-cidr` mirrors — and an air-gapped box
has no route at all), the Pods sit in `ImagePullBackOff`/`ErrImagePull` and the
build stays `building` until its deadline. Point the overrides at images **in
the internal registry** — the one pull source that survives an image GC (a bare
node-containerd import does not: kubelet's image GC collects unused images under
disk pressure, and an air-gapped box then has nothing to restore them from) —
in `felis.toml`:

```toml
[registry]
url = "registry.felis.svc:5000"
build_namespace = "felis-build"
kaniko_image = "registry.felis.svc:5000/mirror/kaniko-executor:v1.24.0"
trivy_image  = "registry.felis.svc:5000/mirror/trivy:0.74.0"
trivy_db_repository = "registry.felis.svc:5000/mirror/trivy-db:2"
trivy_java_db_repository = "registry.felis.svc:5000/mirror/trivy-java-db:1"
build_cpu_limit = "2"
build_mem_limit = "4Gi"
```

Mirror the executor images into the registry once. On the node itself, push
through the loopback hostPort the registry Deployment binds (docker treats
`127.0.0.1` as insecure by default; the installer leaves the daemon stopped, so
`sudo systemctl start docker` first). The registry takes writes only from an
authenticated principal, and `mirror/` only from `platform`, so log in with the
platform token first:

```sh
kubectl -n felis get secret felis-registry-auth -o jsonpath='{.data.platform}' | base64 -d \
  | docker login --username platform --password-stdin 127.0.0.1:5000
docker pull gcr.io/kaniko-project/executor:v1.24.0   # any versions you pin
docker pull aquasec/trivy:0.74.0
docker pull mirror.gcr.io/aquasec/trivy-java-db:1
docker tag gcr.io/kaniko-project/executor:v1.24.0 127.0.0.1:5000/mirror/kaniko-executor:v1.24.0
docker tag aquasec/trivy:0.74.0                   127.0.0.1:5000/mirror/trivy:0.74.0
docker tag mirror.gcr.io/aquasec/trivy-java-db:1  127.0.0.1:5000/mirror/trivy-java-db:1
docker push 127.0.0.1:5000/mirror/kaniko-executor:v1.24.0
docker push 127.0.0.1:5000/mirror/trivy:0.74.0
docker push 127.0.0.1:5000/mirror/trivy-java-db:1
docker logout 127.0.0.1:5000
```

From another machine, port-forward the registry instead (`kubectl -n felis
port-forward svc/registry 5000:5000`) and push to `localhost:5000/...` — the
registry keys a repository by the path after the host, so pushes through either
door land in the same place the build Pods will pull from.

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

Unset fields keep the defaults.

`trivy_db_repository` is not optional on an egress-locked box. Trivy fetches its
vulnerability DB from `mirror.gcr.io`/`ghcr.io` unless told otherwise, and the
build egress policy denies those hosts — so the scan step fails closed
(`failed to download vulnerability DB`), nothing is pushed, and NO build ever
completes. Mirror the DB into the internal registry once:

```
# On the node (docker treats 127.0.0.1 as insecure by default), or through the
# port-forward above, logged in as platform (see the block above):
#   docker pull mirror.gcr.io/aquasec/trivy-db:2
#   docker tag  mirror.gcr.io/aquasec/trivy-db:2 127.0.0.1:5000/mirror/trivy-db:2
#   docker push 127.0.0.1:5000/mirror/trivy-db:2
```

The Job's Trivy container runs with `--insecure`, so the internal registry's plain
HTTP works for the DB pull; reads need no credential. Re-mirror the tag periodically (Trivy refreshes the DB several times a
day upstream; a stale mirror only means stale CVE data, never a failed gate).

`trivy_java_db_repository` is the same story one step lazier: Trivy downloads
the Java DB on demand the first time it scans an image containing Java
artifacts — every real modpack — and that download fails closed too. Mirror
`mirror.gcr.io/aquasec/trivy-java-db:1` alongside the vulnerability DB (commands
above); the Java DB refreshes far less often than the vulnerability DB, so a
one-off mirror is usually fine.

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

### Where worlds are read from (hostPath resolution)

The CronJob mounts `--worlds-host-path` read-only at `/worlds`; the resolver
runs `cmd/felis/reaper.resolveWorldDir`: it looks for `<root>/<pvc>`, then for
the stock local-path directory `<root>/<pv-name>_<ns>_<pvc-name>` derived from
the live PVC's `spec.volumeName` (never a glob — a leftover directory of a
deleted PV must not stand in for the world the PVC currently binds). Pointing
the flag at k3s's storage root (`/var/lib/rancher/k3s/storage`) is therefore the
supported way to enable retention on a stock install. Two deployment facts the
resolver cannot fix:

- **Permissions.** The reaper Pod runs as **root** and carries `DAC_OVERRIDE`:
  worlds are written by the game image's own UID (root for every Paper image we
  ship), and Paper saves `level.dat` mode-0600, so any fixed non-root identity
  (the previous uid-1000 convention, and the ACL setup that went with it) could
  neither walk the tree nor read the files — every archive failed
  `open …/level.dat: permission denied` and the same defect failed on-demand
  backups/restores. Root is the same identity the game container itself runs as
  (see the operator's forwarding-init note); `DAC_OVERRIDE` extends the archive
  to game images with a different UID. If a world is still **preserved** while a
  reap was expected, it is now a different cause: check the run's ERROR logs for
  the resolver's `lstat` messages before suspecting permissions.
- **Node placement.** Multi-node clusters: the world's directory exists only on
  the node holding its volume, and the CronJob sets no `nodeSelector`, so add
  one (single-node starters are pinned implicitly).

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
   stopped), log in as `platform` (§8e), then `docker tag <ref>
   127.0.0.1:5000/<repo>:<tag> && docker push 127.0.0.1:5000/<repo>:<tag>`.

For an image that is in neither place, the old fallback still stands: re-run the
installer (it rebuilds/re-imports from the local Docker store AND mirrors into
the registry), or for a single image
`docker save felis:<tag> | k3s ctr images import -`. The Docker store remains a
deliberate second copy on the node; treat it as the recovery path, not as free
space.

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

### Scraping

The series come from two processes:

- `felis-operator` pod `:8080/metrics` — `felis_servers_total`,
  `felis_start_duration_seconds` (no Service; scrape pod-scoped, e.g. a
  PodMonitor targeting port `metrics`).
- `felis-api` internal face `:8081/metrics` (Service `felis-api-internal`) —
  `felis_image_build_failures_total`. Unauthenticated like the probes;
  ClusterIP-only, and the external face never serves it.
- `felis_reaper_worlds_deleted_total` is produced inside the one-shot reaper
  CronJob, which exits long before any scrape interval — without a pushgateway
  it has no scrape path. Read the reaper Pod log or the `world_backups` table
  for deletions instead.

### Alert rules

`deploy/alerts/` ships ready-made rules: build failures, slow starts, node
disk/memory thresholds, and the kubelet `DiskPressure` condition.

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
and re-applies the bundle. (`sudo felis setup` is not this path; on a completed
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

Roll back with:

```
kubectl -n felis rollout undo deploy/felis-api
kubectl -n felis rollout status deploy/felis-api
```

(the same for `felis-operator` and `registry`). `rollout undo` returns to the
previous ReplicaSet, whose image is normally still on the node; if the image GC
collected it, the registry re-serves it automatically (§13b) for every tag the
installer built — only hand-built tags need a manual re-mirror.

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
| Idle auto-stop not firing; player count 0 | §11 |
| A config field seems ignored | §12 |
| PVC left behind after delete | §13 |
| Node out of disk; pods evicted / ImagePullBackOff | §13b |
| Which metric to scrape | §14 |
| Upgrade / roll back a bad control-plane image | §15 |
