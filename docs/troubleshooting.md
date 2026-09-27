# Felis Troubleshooting Checklist

This file is the spec §28 #23 deliverable: a故障排查清单 (troubleshooting
checklist) for the failure modes the platform actually produces. Every symptom
below is traced to a concrete signal — a `status.conditions` reason, an HTTP
error code, a log string, or a manifest name — so an operator can map what they
see to the code path that emitted it.

## How to read this document

Each entry is **symptom → likely cause → where to look → fix**. The `kubectl`
commands run as root on the node (`sudo -i`, or `sudo k3s kubectl …`): the admin
kubeconfig `/etc/rancher/k3s/k3s.yaml` is readable by root only (§13c). Signals are
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
requeued every 2s until one of the two startup budgets expires, then escalated
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
`RconReached=False`. The reason is one of:

| `status.conditions[].reason` | Meaning | Requeue |
|---|---|---|
| `PodNotReady` | Pod not TCP-ready yet (`status.readyReplicas < 1`) | 2s |
| `SpecChanged` | The spec changed while the pod was not ready; the operator recreated the pod from the new template (§1a) | 2s |
| `AutoRestart` / `StartRetried` | A timed-out start was retried, automatically or from the panel, by recreating the pod | 2s |
| `ServiceAddressPending` | The client Service has no ClusterIP yet | 2s |
| `RconSecretUnavailable` | RCON secret missing or malformed | 10s |
| `RconNotReachable` | RCON dial/auth failed | 2s |

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
  Editing the spec is enough, even once the server is `Failed` with its retries
  spent: the StatefulSet (OrderedReady) never rolls a pod that is not ready, so
  the operator deletes a not-ready `<name>-0` made from an older template itself
  and the StatefulSet recreates it from the new one. The server goes back to
  `Starting` with reason `SpecChanged`, a fresh start timeout and its restart
  budget at zero, and the MinecraftServer gets a `PodReplaced` Event.
  [GO-TESTED: `TestSpecChangeReplacesAPodThatGaveUp`, `TestStalePodReplacement`.]
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
  port yet, RCON is disabled in `server.properties`, or the image listens on a
  port other than 25575 (the CRD accepts only that one for `spec.rcon.port`,
  operations.md §4). [INTEGRATION-ONLY for the live handshake.]

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
| `409` | `world_reclaiming` | The idle reaper is archiving the world (§3b item 3); afterwards the server is released with an empty world | Nothing to wait for; the old world stays in the archive |
| `429` | (cooldown) | Wake retried within the 30s per-server `WakeCooldown` | Wait out the cooldown |
| `503` | `at_capacity` | Global `MaxRunningServers` cap reached | Stop another server or raise the cap |

[GO-TESTED: `handlers_internal_wake_test.go`, cooldown, running-cap shape.] The
operator's RCON probe — **not** the wake call — is the authoritative readiness
gate; the proxy polls `GET /api/v1/internal/servers/{name}/status` every ~2s and
teleports when `ready=true`.

The Velocity-side consumption of these codes (`403` → "You're not allowed to
start «server»"; `409 maintenance_in_progress` → "«server» is under
maintenance", not queued; `409 world_reclaiming` → "«server» sat idle too
long and its world is being archived", not queued; `429` → re-queue; other → "Couldn't start … Try again
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

A stop that leaves the server `Running` for about 30 s is the players' warning.
When desiredState turns `Stopped` and the RCON tally does not say the server is
empty, the operator sends everyone online a yellow chat line (`tellraw @a`),
stamps `status.stopNoticeAt`, records a `StopNotice` Event, and scales down
once `StopNoticeWindow` (30 s) has passed, with a last "stopping now" line
before the save. Starting the server again inside the window calls the stop
off, and the players are told so. An empty server, RCON off, or a probe or
broadcast that fails stops at once. Idle auto-stop only fires on an empty
server, so it never waits.

[GO-TESTED: `internal/maintenance`, `k8scluster_maintenance_test.go`,
`handlers_maintenance_test.go`, operator `maintenance_test.go`,
`stopnotice_test.go`.]

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

The external face has one credential: the `felis_session` cookie the sign-in
doors mint. Cloudflare Access, when the install sits behind it, is enforced at
the Cloudflare edge only — felis-api does not read the `Cf-Access-Jwt-Assertion`
header, so a request that reaches the origin some other way still has to sign in,
and the account and its role always come from the `users` table. The edge setup
fences the panel NodePort to loopback (the `felis_edge` nftables table), so every
request reaches the API through cloudflared and Access stays in front of the
operator console; check `nft list table inet felis_edge` if you doubt it. The error envelope is always
`{"error":{"code","message","request_id"}}`. [GO-TESTED.]

- **`401 unauthorized`** — no valid session cookie. [GO-TESTED.]
- **`403 forbidden`** — authenticated but not permitted (e.g. a non-admin
  principal hitting an admin route; `IsAdmin()` requires a staff role **and** a
  request on the operator console host). [GO-TESTED.]

### 5a. Staff routes 403 on a local IP URL

The operator console is recognised by the request's host: `admin_hostname`
(default `op.console.<root_domain>`). A bare IP counts only when the install
names it — the address a `<ip>.nip.io` / `<ip>.sslip.io` root domain embeds
(the local panel URL `felis setup` prints), or an `admin_hostname` set to that
IP. Any other address, loopback included, is served as the player console, so a
staff account signed in at `https://127.0.0.1:30443` through an SSH tunnel gets
403 on admin routes. Open the console by its hostname instead (an `/etc/hosts`
entry or `curl --resolve` pointing it at the tunnel), or set
`[auth] admin_hostname` to the IP you use. [GO-TESTED]

### 5c. Local-password login fails or is silently rejected

Local sessions use the `felis_session` cookie (HttpOnly, Secure, SameSite=Lax,
12h TTL, host-only). They are gated by the `local_auth_enabled` row in
`platform_settings`, read live per request and **fail-closed** (missing or
unparseable → treated as disabled). Symptoms:

- Cookie present but login rejected with `local auth disabled` → the
  `local_auth_enabled` setting is false/absent. A present cookie under disabled
  local-auth is **rejected outright**.
- `invalid session: …` → bad/forged session hash.

Fix: set `local_auth_enabled=true` in `platform_settings` if local password auth
is intended. [GO-TESTED for the session/QR-login logic.]

---

## 6. Internal API rejects Velocity / proxy callers (service-token)

The internal face (`--internal-addr :8081`, routes under
`/api/v1/internal/...`) is **never** Zero-Trust; it authenticates a bearer token
(`Authorization: Bearer <token>`, compared in constant time). Each internal caller
has its own token, and each route serves only the callers listed for it
(`x-felis-callers` in `docs/openapi.yaml`):

| Caller | Secret (key `token`) | API env | Where the caller reads it | Routes |
|---|---|---|---|---|
| `velocity` (proxy felis-link) | `felis/felis-service-token` | `FELIS_SERVICE_TOKEN` | `service-token` in the host's `felis-link.properties` | server list, wake/claim/ready/status, join events, menu, op-login, migrate, reclaim, link codes, blacklist |
| `limbo` (login gate) | `felis/felis-limbo-token`, replica in `minecraft` | `FELIS_LIMBO_TOKEN` | login pod env `FELIS_SERVICE_TOKEN` | link codes, link status, blacklist |
| `build` (build Job fetch) | `felis/felis-build-token`, replica in `felis-build` | `FELIS_BUILD_TOKEN` | fetch initContainer env | submission build context |
| `ops` (`felis backup-now`) | `felis/felis-ops-token` | `FELIS_OPS_TOKEN` | read from the Secret on each run | break-glass backup |

The installer generates all four into `/etc/felis/secrets.env` (`SERVICE_TOKEN`,
`LIMBO_TOKEN`, `BUILD_TOKEN`, `OPS_TOKEN`) and applies them on every run. The audit
log records internal actions with the source `internal:<caller>`. [GO-TESTED]

In-cluster it is reached through the ClusterIP Service `felis-api-internal` (port
8081), which is separate from the external NodePort `felis-api` (443) precisely so
the no-Zero-Trust face is never exposed on a node. On the control-plane node the
break-glass console reaches it by resolving that Service's ClusterIP and dialing
`:8081`.

The face speaks plain HTTP, and the tokens cross it in the clear. That holds up because
no hop leaves the node: the proxy runs on the node and dials the ClusterIP, and the
login pod and the build Job are pods on the same node, so reading the traffic takes root
there, which also reads the tokens from disk. Pods reach the face only where a policy
opens it: `felis-login-to-internal-api` for the login pod and `felis-build-egress` for
the build Job; `felis-server-egress` keeps every other game server (lobby included) off
all private ranges, 8081 among them. A Velocity on another host would put the `velocity`
token on the wire, which is one more reason the proxy belongs on the node (§1 of
`docs/operations.md`); a multi-node shape would need TLS on this face first.

- **Internal calls fail to *connect* (not 401)** → the `felis-api-internal` Service
  is missing or its selector no longer matches the api pods. `kubectl -n felis get
  svc felis-api-internal` must show a ClusterIP with 8081; a bare `felis-api` name
  serves only 443 and every internal call would hang/refuse.

- **One caller's calls all 401** → its token is unset or differs from the api's
  copy. The api logs one line per unset token at startup:

  ```
  felis api: warning: FELIS_LIMBO_TOKEN unset — the internal face turns the limbo caller away
  ```

  An unset token never matches anything (no bypass). Compare the caller's value
  with its Secret, e.g. for the proxy:

  ```
  kubectl -n felis get secret felis-service-token -o jsonpath='{.data.token}' | base64 -d
  ```

- **`403 wrong_caller`** → the token is valid but belongs to a caller that route
  does not serve, e.g. the build token calling a proxy route. Configure the caller
  with its own token from the table above.

- **api pods stuck in `CreateContainerConfigError`** → one of the four Secrets is
  missing in `felis`. Re-run the installer; it applies them before the bundle.

- **Two tokens with the same value** → `felis api` refuses to start with
  `the X and Y tokens are the same value; each caller needs its own`, since a shared
  value would make the caller ambiguous. Rotate one of them.

### Rotating a token

`sudo felis rotate-token <kind>` replaces one credential the installer generated.
Without `-yes` it only prints what it would write and what that interrupts;
`sudo felis rotate-token -yes <kind>` rotates. Every rotation writes the new value
into `/etc/felis/secrets.env` first, so whatever fails after that, a re-run of the
installer (`sudo bash deploy/bootstrap.sh`) puts the value everywhere. [GO-TESTED]

| kind | replaces | interrupts |
|---|---|---|
| `velocity` | the proxy's token: Secret `felis-service-token`, `service-token` in the host proxy's `felis-link.properties` | felis-api restarts (one replica: the panel, sign-in and the proxy's calls stop for a few seconds). The proxy re-reads its file and keeps its players |
| `limbo` | the login gate's token, `felis-limbo-token` and its minecraft replica | felis-api restarts, then the login pod |
| `build` | the build Jobs' token, `felis-build-token` and its build-namespace replica | felis-api restarts; a build fetching its context at that moment fails and can be submitted again |
| `ops` | `felis backup-now`'s token, `felis-ops-token` | felis-api restarts |
| `registry` | the registry's `platform`, `build` and `prune` write tokens: Secret `felis-registry-auth`, the build Jobs' `felis-registry-push` | the registry restarts (a push at that moment fails, a pull retries), then felis-api |
| `forwarding` | the Velocity forwarding secret: `/opt/felis/velocity/forwarding.secret`, Secret `felis-forwarding-secret` in both namespaces | every running server restarts (saving its world) and the proxy restarts: everyone online is disconnected |
| `db` | the database role's password: the role in felis-postgres, `[database] url` in `felis.host.toml` and `felis.pod.toml`, Secret `felis-config` in both namespaces | felis-api restarts; a backup, restore or file Job that connects in those seconds fails and can be run again |

- **velocity** — the rotation waits for the proxy's log line
  `Felis: service-token reloaded from … (fingerprint <12 hex digits>)`. A proxy
  that has not logged it within 60 s of felis-api's restart (a plugin from before
  this release) is restarted, which disconnects everyone online. The file keeps
  its modification time, so `felis domain check` does not read the proxy as
  stale. A proxy on another host is left alone: set `service-token` in its
  `felis-link.properties` to the value in Secret `felis/felis-service-token`, and
  it takes it within a few seconds.
- **forwarding** — a server whose world a backup, restore or file write holds is
  left running and named in the plan and the output; players cannot join it until
  it restarts, so stop and start it from the panel once that finishes. The next
  installer run restarts the proxy once more (its record of what the proxy was
  started from predates the rotation); an upgrade restarts it for the new plugin
  jar anyway. A proxy on another host needs the value in Secret
  `felis/felis-forwarding-secret` in its forwarding secret file, and a restart.
- **db** — the password reaches PostgreSQL as a SCRAM-SHA-256 verifier, never as
  text a failed statement could log. The config copies change only after a
  connection with the new password succeeds; when it does not, the command stops
  and says so, and the installer re-run sets the password from `secrets.env` in
  the role and every copy. A database the installer does not run (`[database]
  deployment` unset) is refused: change its password where it runs, then in each
  copy's `[database] url`. `felis.pod.toml` must be a file of its own, since the
  pods reach the database at another address.
- The old value stops working as felis-api (or the registry) restarts; a caller
  still presenting it is turned away until it has the new one.

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
`writeBuildError` (JobPhase→Failed). [GO-TESTED for the mapping.] The
build's error names the cause: the step that failed with the last lines of its
output, the deadline, or the scan verdict (spec §16). The pod runs `egress-gate`
(§8f), `context-fetch`, `kaniko` (builds a tarball, never pushes), `trivy`
(writes the full JSON report of that tarball), `sbom` (converts the report to a
CycloneDX SBOM) and `scan-gate` (applies the scan policy) as init containers,
then `push` — so an image the scan blocks never reaches the registry. Inspect
every step:

```
kubectl logs -n felis-build job/<build-job> --all-containers --prefix
```

A `push` that fails with `403` means the target repository is under `felis/` or
`mirror/` — the registry gate reserves those for the platform (§9); `401` means
the `felis-registry-push` Secret in `felis-build` is missing or stale (re-run the
installer).

**The scan gate.** `scan-gate` blocks the image when a vulnerability or a
leaked secret has a severity listed in `[registry] scan_fail_on` (§8e; default
`CRITICAL`). A vulnerability with no fixed release is listed without
blocking unless `scan_fail_unfixed = true`, since nothing can be upgraded to
clear it. A blocked build ends with an error such as:

```
the scan blocked the image: 1 CRITICAL, 1 HIGH (CVE-2026-12345, CVE-2025-24813)
```

`HIGH` is opt-in because the platform's own `felis/paper` image carries five
fixable HIGH findings inside upstream `paper.jar` (its bundled commons-compress
1.5 and plexus-utils 3.5.1). Adding `HIGH` to `scan_fail_on` blocks every build
`FROM` it until those ids are accepted as known risks in `scan_accept`:

```toml
scan_fail_on = ["CRITICAL", "HIGH"]
scan_accept = ["CVE-2021-35515", "CVE-2021-35516", "CVE-2021-35517", "CVE-2021-36090", "CVE-2025-67030"]
```

An accepted id (a CVE, GHSA or similar advisory id, or a secret rule id such as
`aws-access-key-id`) never blocks; its findings are still counted, listed and
marked **Accepted** on the panel, and scan-gate's log names the accepted ids.
Review the list whenever the base image is upgraded.

felis-api keeps each finished build's scan: the verdict, up to 100 findings with
the blocking ones first, the full Trivy report and the SBOM. On the panel,
**Build Pipeline → Scan & logs** on a finished build shows them, with both files to download. The
API serves the same data (admin only):

| Endpoint | Returns |
|---|---|
| `GET /api/v1/images/build/{id}/scan` | the verdict and findings; `404 scan_not_found` for a build that stopped before the scan or ran before builds kept scans |
| `GET /api/v1/images/build/{id}/scan/report` | the Trivy JSON report as `<id>-trivy.json` |
| `GET /api/v1/images/build/{id}/sbom` | the CycloneDX SBOM as `<id>.cdx.json` |

The report and SBOM travel to felis-api inside the scan-gate container's log, so
one image gets at most 6 MiB of them compressed. Past that the gate drops the
SBOM first, then the report, says so in its log, and the download answers
`404 scan_document_not_kept`; the verdict and findings are always kept. A
`scan-gate` exit 2 means the report was missing or unreadable; the build fails
closed and the error says why.

A scan reflects the vulnerability DB on the day of the build. An admitted image
is not scanned again when the DB learns of a new CVE; to rescan it, start a new
build of the same context (`POST /api/v1/images/build`), after
`felis mirror-build-tools -only trivy-db` if the DB copy is old (§8e).

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

**The platform's own images on an air-gapped node.** The registry pod and the database
pod run images from Docker Hub by digest (`REGISTRY_IMAGE` and `POSTGRES_IMAGE` in
`bootstrap.sh`), which the installer imports into k3s's containerd from the release's
`felis-image-base-linux-<arch>.tar` (or, without one, pulls) and pins there so the
kubelet's image GC never collects them. On an air-gapped node the simplest way is to
install from the release's assets copied to the node (`FELIS_ARTIFACT_DIR`, operations
§1). When the pull fails (`could not pull …`) and no release assets are at hand, fetch
the same digest on a machine that can, for the node's architecture, keeping the manifest
as it is, and import it on the node:

```sh
# elsewhere: the reference as bootstrap.sh names it, e.g. docker.io/library/postgres:18.6-trixie@sha256:…
sudo ctr images pull --platform linux/arm64 "$ref"
sudo ctr images export --platform linux/arm64 image.tar "$ref"
# on the node:
sudo k3s ctr images import image.tar
```

A `docker save` of the image rewrites its manifest, so its copy never matches the digest
the Deployment names. Rerun the installer afterwards; it finds the image and pins it.
[CODE-ONLY]

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
scan_fail_on = ["CRITICAL"]        # §8d: severities that block; any case; empty = CRITICAL
scan_fail_unfixed = false          # §8d: true blocks on vulnerabilities with no fixed release too
scan_accept = []                   # §8d: vulnerability or secret rule ids accepted as known risks; never block
build_cpu_limit = "2"
build_mem_limit = "4Gi"
build_disk_limit = "12Gi"          # §8f
build_user_namespaces = "auto"     # §8f: auto | on | off
build_runtime_class = ""           # §8f: e.g. "gvisor"
max_concurrent_builds = 2          # §8f: 1-6; later builds queue
user_uploads_max_bytes = "4Gi"     # every user's uploaded contexts together; 507 uploads_full past it
context_max_bytes = "1Gi"          # one uploaded context; empty = 1Gi (sent in 32 MiB parts, so the Cloudflare edge's 100 MB body cap does not bind)
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
| Scan gate | the image's Trivy report is judged under `scan_fail_on` before `push` runs; a blocked or unreadable scan keeps the image out of the registry | `felis scan-gate`, §8d |
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
  One uploaded context is capped by `context_max_bytes` (1Gi; the panel checks
  the file against it before uploading). The panel sends a context in parts of
  at most 32 MiB, staged under `.parts/` on the uploads volume, so the
  Cloudflare edge, which answers its own 413 page for request bodies over
  100 MB, never sees a body that large; a dropped connection resumes from the
  staged length, and a staged upload untouched for 24 hours is deleted. The
  staged bytes count toward the budgets below. A script can still POST a whole
  context in one body, which the edge caps at 100 MB. With an `s3://`
  `user_uploads_context`, felis-api sends the finished context on to the bucket
  in 8 MiB parts, so an upload of any size holds 8 MiB of its memory; S3's
  10,000 parts put the ceiling at about 78 GiB.
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
  `sudo felis rotate-token -yes registry` replaces all three and restarts the
  registry (the gate reads its tokens at start) and felis-api, which presents the
  `prune` token (§6 "Rotating a token").
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
`max_local_bytes`, the on-demand backup keys `manual_retention` /
`manual_keep` / `manual_cooldown` and the scheduled backup keys
`scheduled_every` / `scheduled_keep` / `scheduled_retention` are configurable
from `felis.toml [archive]`).

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
felis reaper: evaluated=12 reaped=1 released=0 deleted=0 awaiting_offsite=0 awaiting_stop=0 warned=2 skipped=0 store_full=0 evicted=0 expired=3 expire_failed=0 verified=6 corrupt=0 verify_failed=0 swept=0 orphan_archives=0
```

`released` and `deleted` count retirements carried out: servers their owner gave
up (or an admin released) and servers an admin deleted, each world archived
first as a `released` backup. `skipped` counts servers the run failed on (steps
1–3 above, or the cluster or the database answering with an error; exempt
servers and rows whose CRD is gone are not counted, except a deletion whose
MinecraftServer is gone while its world volume remains), `store_full` the subset kept because the backup store is full,
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

### Every world at once: `felis backup-now`

A world lives only in its volume, and the off-site copy holds only its archives.
Before anything that could lose a volume (growing the disk, moving the data to its
own disk, moving to another host: operations.md §2 and §5), archive every world
from the node:

```bash
sudo felis backup-now                    # the plan; nothing changes
sudo felis backup-now -yes               # archive every stopped world, one at a time
sudo felis backup-now -yes -stop         # stop the running servers first
sudo felis backup-now -yes alpha bravo   # only these servers
```

It runs as root because it reads the ops token (`felis/felis-ops-token`, §6) and
asks felis-api's internal face for each backup. Each archive is an ordinary manual
backup (the same Job, the same 10% free-disk check, the audit action
`backup.create` with the source `internal:ops` and the sudo user as actor), exempt
from the owner cooldown and the `max_local_bytes` cap like the break-glass console.

- **The plan** lists every user server with its phase and what the run does with
  it: `back up`, `stop, then back up`, `skip: running (stop it first, or pass
  -stop)`, or `skip: no world volume (never started, nothing to save)`. With
  nothing to archive it ends `Nothing to back up.`
- **Each archive counts against `manual_keep`**: a server that already holds that
  many manual backups loses its oldest, and the plan says so. Raise
  `[archive] manual_keep` first when those older backups matter.
- **Stopped servers go first**, so a felis-api that cannot take a backup is found
  before anything is stopped for one. The run waits for each Job (`alpha: archived
  in 42s`) before starting the next.
- **`-stop` disconnects the players** and leaves those servers stopped (`Left
  stopped: …`; start them from the panel). Each stop writes `break_glass.halt` to
  the audit log. A server still up after 10 minutes counts as failed, and the run
  moves on.
- **An unreachable felis-api ends the run** (`Stopped: nothing more can be backed
  up until felis-api answers`). Ctrl-C ends it after the current step; a backup Job
  already started runs to its end.
- **The archives stay on the node** until the hourly off-site copy. After a run
  that archived something the command prints `sudo systemctl start
  felis-offsite.service`, which sends them now; `sudo felis offsite status` shows
  what still waits.

It exits 0 when every world with a volume was archived, 1 when a backup failed, a
running server was skipped (no `-stop`) or the run was interrupted, and 2 for a
name that is no user server. [GO-TESTED: `TestBackupNowPlanChangesNothing`,
`TestBackupNowPlanWithNothingToSave`, `TestBackupNowBacksUpEachWorldInTurn`,
`TestBackupNowSkipsRunningServersWithoutStop`,
`TestBackupNowStopsAtAnUnreachableAPI`, `TestBackupNowStopsWhenInterrupted`,
`TestBackupNowNamedServers`]

### Scheduled backups (daily restore points)

A world played every day never idles 15 days, so the reaper never archives it.
felis-api therefore takes a `scheduled` backup of every owned world that
somebody joined since its owner's last intact scheduled backup, once that
backup is `scheduled_every` old. This works without a worlds root: it is the
same backup Job "Back up now" starts, so it needs only `FELIS_IMAGE` and
`FELIS_BACKUP_PVC` (felis-api logs `scheduled backups off` at start when
either is missing or `scheduled_every` is `0s`).

| Key | Default | Effect |
|---|---|---|
| `scheduled_every` | `1d` | how old a world's newest scheduled backup must be before it gets the next one (`0s` turns scheduled backups off) |
| `scheduled_keep` | `7` | scheduled backups kept per server and owner; the Job removes older ones like `manual_keep` |
| `scheduled_retention` | `90d` | when a scheduled backup expires |

How it behaves:

- **Only a stopped server is backed up.** The Job mounts the world volume,
  which a running server holds. Idle auto-stop brings a played world down
  minutes after its last player leaves, so the point normally lands the same
  day. A server with auto-stop turned off gets its point the next time it
  stops; one that never stops never gets one.
- **One Job at a time, cluster-wide.** felis-api checks every 2 minutes and
  starts one scheduled backup only while no backup or restore Job is running,
  worlds without a point first. It holds the world like any backup, so a player
  who wakes the server during those minutes sees the maintenance message and can
  retry once it finishes.
- **Paused while the store is full.** While the present backups add up to
  `max_local_bytes`, no scheduled backup starts (felis-api logs
  `scheduled backups paused` and `resumed` once each). The backup Job's 10%
  free-disk check applies too.
- **A failed backup is retried** after a quarter of `scheduled_every` (6 hours
  by default); the failure shows under Recent operations as "Scheduled backup".
- **Counted per owner.** A backup the world's previous owner took before it was
  reaped and claimed again is no restore point of the new owner's world, and the
  new owner's backups never prune it. The keep-N prune of every reason is scoped
  to the backup's owner the same way.
- Scheduled backups write the audit action `backup.scheduled` (actor
  `scheduler`) and never start the owner's `manual_cooldown`. They are copied
  offsite and evicted by `max_local_bytes` like manual backups.

[GO-TESTED: `TestBackupScheduler`, `TestK8sScheduledBackupJobs`,
`TestBackupJobRecordsAScheduledBackup`, `TestBackupPolicyPerReason`,
`TestReaperConfigScheduledKeys`; PG-TESTED: `TestScheduledBackupCandidates`,
`TestExcessBackupsPerOwner`]

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

The screen also keeps the password in `/etc/felis/smtp-password` (mode 0600),
and the uploads screen keeps the bucket keys in `/etc/felis/uploads-s3-access-key`
and `uploads-s3-secret-key`. Secrets live in k3s's datastore, which a reinstall
or a host rebuilt from a bundle's `state/` starts empty, so **every installer
run applies `felis-smtp` (both namespaces) and `felis-uploads-s3` from these
files**. The files win: a hand edit of either Secret lasts until the next
installer run, so change a credential through `felis setup`. An install from
before these files gets them from the Secrets on its first re-run. When
`[smtp]` has a `username` but no password is on either side, or uploads go to
`s3://` without both keys, the installer says so and names the `felis setup`
screen that takes them again. [SH-TESTED: `deploy/bootstrap_test.sh`, the move
from the Secrets, the host copy winning, both warnings, no value in kubectl's
argv. GO-TESTED: the host copy's mode and replacement.]

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

A **user** server created before every new server got RCON (`spec.rcon` wholly
unset) has a dead console, always shows 0 online, and never idles out, since all
three ride RCON. Plain `felis converge` lists each such server and changes
nothing; `-user-rcon` turns RCON on for them with the block a server created
today gets (the `<name>-rcon` Secret the operator provisions):

```
sudo felis converge -user-rcon
kubectl -n minecraft get minecraftserver -o custom-columns=NAME:.metadata.name,RCON:.spec.rcon.enabled
```

It is opt-in for the reason above: the new start waits on the RCON probe, and a
server whose image does not open the listener that `RCON_PASSWORD` asks for
stays in `Starting` until it is marked `Failed`. Felis's own paper and lobby
images open it; check a server running an image a user brought before filling
it. A server whose RCON someone set, on or off, is never touched. The change
applies at the server's next start.

---

## 13. World PVC survives after I deleted the MinecraftServer

This is expected. The world PVC is a StatefulSet `VolumeClaimTemplate`, and the
operator sets the StatefulSet's `persistentVolumeClaimRetentionPolicy` to
`Retain` on delete and on scale, explicitly rather than by the API default.
There is no finalizer. Deleting the `MinecraftServer` garbage-collects the
StatefulSet and keeps the claim, so a `MinecraftServer` that comes back under
the same name mounts the same world. The **only** code that deletes a world PVC
is the reaper, and only after a verified backup (§10).

A kept claim holds the name: creating a new server with it answers
`409 world_volume_exists`, since the new server would otherwise mount the old
world and hand it to its new owner. List the world claims whose server is gone:

```
comm -23 \
  <(kubectl -n minecraft get pvc -l felis.lolicon.best/server -o jsonpath='{range .items[*]}{.metadata.labels.felis\.lolicon\.best/server}{"\n"}{end}' | sort) \
  <(kubectl -n minecraft get minecraftservers -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | sort)
```

To reclaim one (take a backup first if the world may still matter):

```
kubectl -n minecraft delete pvc world-<name>-0      # irreversible — the world is gone
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

## 13c. The host's address, name or clock changed

An install is bound to the address it was made on. `bootstrap.sh` gives that
address to the k3s node and writes it into the network policies, the panel
certificate and the default `<ip>.nip.io` root domain, and nothing re-addresses
a live install. When the host loses the address (a DHCP lease that came back
different, a moved VM), the cluster and the proxy's path to the game servers
still name the old one, and the panel stops answering on its old name. The watchdog reports it as
`host-address` (critical, after 5 minutes). The installer warns at install time
when the address is a DHCP lease.

Remedy: give the host its old address back, either as a DHCP reservation on
the router or as a static address (`nmcli con mod <con> ipv4.method manual
ipv4.addresses <ip>/<prefix> ipv4.gateway <gw> ipv4.dns <dns> && nmcli con up
<con>` on Rocky), then restart felis-api (`sudo k3s kubectl -n felis rollout restart
deploy/felis-api`) and the proxy (`sudo systemctl restart felis-velocity`). Moving an install
to a new address is a reinstall onto a restored backup (docs/operations.md §5). A
default `<ip>.nip.io` root domain names the old address too; once the install runs on
its new address, `sudo felis domain set <new-ip>.nip.io` moves it (docs/operations.md §6).

The node **name** is pinned. Every local-path volume (worlds, registry,
uploads, backups) is bound to its node by name, and k3s takes the name from the
hostname unless told otherwise, so renaming the host used to bring k3s back as
a second, empty node with every volume Pending on the old one. The installer
pins the name in `/etc/rancher/k3s/config.yaml.d/50-felis.yaml`
(`node-name:`); a hostname change is then harmless. Check the pin with `sudo
k3s kubectl get node -o jsonpath='{.items[0].metadata.annotations.k3s\.io/node-args}'`.
That file also sets `write-kubeconfig-mode: "0600"`: the admin kubeconfig
`/etc/rancher/k3s/k3s.yaml` is cluster-admin and readable by root only, so
use `sudo k3s kubectl` (or `sudo -E kubectl`).

The **clock** must be kept by NTP. Sign-in codes and sessions expire by it,
S3 refuses off-site uploads signed more than 15 minutes off, and certificate
checks fail on a clock far off. The installer turns NTP on (`timedatectl
set-ntp true`, installing chrony where there is no client to enable) unless
`FELIS_MANAGE_TIME_SYNC=0`. The watchdog reports an unsynchronized clock as
`clock` (warning, after 30 minutes). Check with `timedatectl` (want `System
clock synchronized: yes` and `NTP service: active`) and `chronyc sources`;
a firewall that drops outbound UDP 123 keeps it unsynchronized.

The system journal is persistent (`/etc/systemd/journald.conf.d/50-felis.conf`,
capped by `FELIS_JOURNAL_MAX_USE`, default 1G), so `journalctl -b -1` shows the
boot before a reboot.

---

## 13d. k3s certificates expire after a year

k3s issues its own client and serving certificates (the API server's, the
kubelet's, the admin kubeconfig's, etcd's) for 365 days and its CA certificates
for ten years. It renews the client and serving certificates only as it starts:
each start reissues, from the same keys, every one that has expired or is within
120 days of expiring. A host that reboots or takes a k3s upgrade
(`FELIS_UPGRADE_DEPS=1`, docs/operations.md §4) inside that window renews them
unnoticed. A host that runs a year without restarting k3s loses its API server
and its node the day they lapse, and the panel can no longer start, stop or back
up servers. No restart renews a CA certificate; that takes `k3s certificate
rotate-ca` and the procedure in the k3s documentation (Certificate Management).

The watchdog reads the `*.crt` files under `/var/lib/rancher/k3s/server/tls`
(and its `etcd`, `kube-controller-manager` and `kube-scheduler` directories) and
`/var/lib/rancher/k3s/agent`, the set `k3s certificate check` reads, and never
the keys beside them. It reports the certificate that expires first as
`k3s-certs`: a warning 30 days before, critical in the last 7 days and once it
has lapsed, and the hint says whether a restart renews it (a CA it does not).
`felis watchdog -k3s-cert-dirs ""` turns the check off. [GO-TESTED: `TestCertFinding`]
[VM-VERIFIED: against the real certificates of a v1.36.4+k3s1 install, with the
clock moved ahead]

Check and renew:

```sh
sudo k3s certificate check --output table   # every certificate, its expiry and residual time
sudo systemctl restart k3s                  # reissues the ones within 120 days of expiry
sudo k3s certificate check --output table   # the renewed ones show about a year again
```

A restart stops k3s alone. The unit's `KillMode=process` leaves the containers
running, so game servers, PostgreSQL and the control plane keep serving while
the API server comes back within a minute. [VM-VERIFIED: every container ID and
restart count unchanged across `systemctl restart k3s`] `k3s-killall.sh` stops
every container along with k3s; keep it out of this.

To renew ahead of the 120-day window (to line it up with a maintenance slot),
reissue every client and serving certificate at once:

```sh
sudo systemctl stop k3s && sudo k3s certificate rotate && sudo systemctl start k3s
```

The panel's own certificate (`/etc/felis/panel-tls.crt`, which the installer
self-signs for 825 days) is a different certificate, outside this check.

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
| Newest daily control-plane database backup over 26h old, or none (§16); a newer manual or off-site bundle leaves it standing | 10 min | critical |
| A watched filesystem below 15% free (below 5%: critical) | 15 min (5 min) | warning |
| Host memory available below 10% | 15 min | warning |
| The host no longer holds the address the install was made on (§13c) | 5 min | critical |
| The system clock is not synchronized by NTP (§13c) | 30 min | warning |
| A k3s certificate expires within 30 days (within 7 days, or lapsed: critical) (§13d) | at once | warning |
| The watchdog's own runs keep failing (`watchdog/run`, below) | 10 min | critical |
| The watchdog's state file did not parse and was moved aside (`watchdog/state`, below) | at once, once | warning |

How it mails:

- **Recipients** are the verified email addresses of enabled owner accounts.
  The relay is the `[smtp]` one sign-in codes use.
- **One mail per run**, holding everything that came due: new problems, a
  daily reminder for each problem still open, and a resolved notice once a
  problem has stayed gone for 10 minutes. A condition that heals before its
  delay is never mailed; one that turns critical is mailed again at once.
- **During an install** nothing is mailed. `bootstrap.sh` writes
  `/run/felis/watchdog-quiet-until` and removes it when it exits.
- **On a host built from another host's backup** (a rehearsal on a spare
  machine, a rebuild before `felis offsite take-over`) nothing is mailed while
  the host the off-site bucket names ran in the last 3 hours: the owners in
  the restored database are that host's, and it mails them itself. The run
  logs `this host stands by for host ...; holding this mail`. Once that host
  stops running, the standby is mailed like any other finding (§16).
  [GO-TESTED: `TestMailHold`]
- **Caching:** the relay password comes from `/etc/felis/smtp-password`, which
  the watchdog reads even while the API server is down (an install without that
  file reads the `felis-smtp` Secret instead). It and the recipient list are
  cached in `/var/lib/felis/watchdog/state.json` (root-only). An outage of
  PostgreSQL or of the API server can therefore still be mailed. [GO-TESTED: a
  run with the API server and PostgreSQL both down caches the host copy.]
- **No relay or no verified owner address:** each alert is written to the
  journal only, and a run with a heartbeat pings its failure endpoint while an
  alert is open (below). Setup runs without mail, so a fresh install is in
  this state. The install ends with `NO ALERT MAIL`, and the card at the end
  of `sudo felis setup` has an `alerts` row that says where alerts go: `mailed
  to <address> via <relay>`, or what is missing, marked `⚠`. Press `e` there
  to configure email, then verify the Owner's address in the panel (Account →
  Email Verification). The `heartbeat` row below it shows the check that is
  pinged. [SH-TESTED; GO-TESTED: `TestAlertRouteLines`,
  `TestHostAlertRoute`]

Commands:

```bash
journalctl -u felis-watchdog -n 40          # every check of the latest runs
sudo felis watchdog -dry-run                # run the checks now; mail nothing, change nothing
systemctl list-timers felis-watchdog.timer  # when it last and next runs
```

A healthy run logs `every check passed`. Otherwise it logs one line per
finding, and the mail's subject once one is sent.

A `-dry-run` from a shell uses the command's defaults, and those do not include
the game-proxy or the address check. The unit carries `-proxy-addr
127.0.0.1:<game port>`, `-node-ip <install address>` and the disk list the
install chose. `systemctl cat felis-watchdog` shows them.

### The heartbeat: what notices the host itself going down

A host that is off, a timer that stopped, a watchdog that fails before it can
mail: the host reports none of these about itself. A heartbeat covers them.
Every run pings a check at an outside monitoring service (Healthchecks.io, or
one that copies its API), and that service mails you when the pings stop.

1. Create a check there with a period of 2 minutes and a grace of 10 minutes.
2. Re-run the installer with `FELIS_WATCHDOG_HEARTBEAT_URL=<the check's ping
   URL>`. It writes the URL to `/etc/felis/watchdog-heartbeat-url` (root-only,
   0600). The URL's path is the check's key, so the install and the journal
   show only its scheme and host.

A later install without the variable keeps the file, and
`FELIS_WATCHDOG_HEARTBEAT_URL=off` removes it. An install with no heartbeat
ends with a `NO HEARTBEAT` warning. [SH-TESTED: `deploy/bootstrap_test.sh`]

What a run pings:

| The run | Ping |
|---|---|
| Its alerts reach the owners (mailed, or nothing due) | `GET <url>` |
| An alert is open and reaches no one (no relay, no verified owner address), the mail fails, or the state does not save | `POST <url>/fail`, with the reason and the findings in the body |
| One of those failures while the installer runs | nothing |
| A host standing by for another host's off-site bucket (§16) | nothing: the host that writes the bucket pings the check |

A URL with a query (`?`) has no `/fail` endpoint, so a failing run sends no
ping and the check trips once its grace runs out. A ping that times out (10 s)
or gets a non-2xx answer logs `felis watchdog: heartbeat: ...`; the run's exit
status stays that of its checks and its mail. [GO-TESTED:
`TestHeartbeatSend`, `TestWatchdogRunHeartbeat`]

A bundle (§16) carries `/etc/felis` and the heartbeat file with it. A host
rebuilt from one stands by and pings nothing until `felis offsite take-over`;
from then on it pings the same check.

### When the watchdog itself fails

`felis-watchdog.service` carries `OnFailure=felis-watchdog-failed.service`. A
failed run (a crash, the 3 min time limit, a `felis.toml` that no longer loads)
starts `felis watchdog -unit-failed`, which:

- pings the heartbeat's `/fail` at once with systemd's result, e.g.
  `felis-watchdog.service failed: result exit-code, exit status 3`, except
  while the installer runs or the host stands by;
- records `watchdog/run` and mails it once runs have kept failing for 10
  minutes. The relay and recipients come from `felis.toml` when it loads and
  from the watchdog's cache when it does not. The first passing run resolves
  it like any other finding.

[VM-TESTED: on systemd 252, a failing unit with this `OnFailure=` passed
`result exit-code, exit status 3` to the fallback, which posted it to a local
`/fail` endpoint; with the installer's quiet file present it withheld the
ping. GO-TESTED: `TestWatchdogUnitFailedBrokenConfig`, six failures two
minutes apart mail once, at 10 minutes, through the cached relay.]

A state file (`/var/lib/felis/watchdog/state.json`) that does not parse is
renamed to `state.json.unreadable-<unix time>`. The run starts from a fresh
state and mails `watchdog/state` once. The fresh state has lost the open
alerts' history, so each problem still present is mailed again as new. A
power loss right after a save or a hand edit usually causes this; check
`df -h /var/lib/felis`, then delete the set-aside copy. [GO-TESTED: `TestRecoverState`]

A state file that cannot be written (a full or read-only `/var/lib/felis`)
leaves the run's state in `/run/felis/watchdog-state.json` (tmpfs, root-only).
The next run reads whichever of the two was written last, so an alert that was
mailed is not mailed again every two minutes. Each such run logs `save state:
...; kept in /run/felis/watchdog-state.json until the host restarts`, exits 1
and pings the heartbeat's `/fail`; after five in a row `watchdog/run` is mailed
once. The first run whose state file saves again deletes the tmpfs copy. A
restart before that forgets what was mailed, so the open problems are mailed
again as new. [GO-TESTED: `TestSaveStateOr`, `TestWatchdogRunStateThatDoesNotSave`,
`TestWatchdogUnitFailedStateThatDoesNotSave`]

```bash
journalctl -u felis-watchdog-failed -n 20   # what the fallback reported and pinged
systemctl status felis-watchdog             # the failed run's result
```

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
- `go_sql_*{db_name="felis"}` — the API's database pool. It is capped at 25
  connections, and every connection runs with `statement_timeout=15s` and
  `idle_in_transaction_session_timeout=60s` (a `[database] url` that sets
  either keeps its own). `go_sql_in_use_connections` sitting at
  `go_sql_max_open_connections` with `go_sql_wait_count_total` climbing means
  requests are queueing for a connection: look for a slow query or a lock
  (`SELECT pid, state, wait_event, query FROM pg_stat_activity`). A statement
  cut off by the limit logs `canceling statement due to statement timeout`.

The API also writes one access-log line per request to its log, in logfmt:
`face`, `method`, `route`, `path`, `status`, `duration_ms`, `bytes`,
`request_id` (the id in every error envelope) and `principal` (the user id,
once signed in). Successful probes and scrapes are left out.

```bash
kubectl -n felis logs deploy/felis-api | grep 'msg=request' | grep 'status=5'
kubectl -n felis logs deploy/felis-api | grep 'request_id=<id from the error>'
```

The plugins' calls are the `face="internal"` series, one route per call: a
failing join-event, wake or link-status poll shows up as its own route.

```promql
sum by (route, code) (rate(felis_http_requests_total{face="internal", route="/api/v1/internal/servers/{name}/join-event"}[5m]))
sum by (route, code) (rate(felis_http_requests_total{face="internal", route="/api/v1/internal/servers/{name}/wake"}[5m]))
histogram_quantile(0.95, sum by (le, route) (rate(felis_http_request_duration_seconds_bucket{face="internal"}[5m])))
```

A call that never reached felis-api (refused, reset, timed out) is missing from
those series; the caller counts it. The proxy logs one line per active 10 minutes
with felis-api as it saw it plus its own failures, and `/felis` at the proxy
console prints the totals since start:

```bash
journalctl -u felis-velocity | grep 'Felis: last 10 min'
# Felis: last 10 min: felis-api calls=412 (no answer=0, 4xx=3, 5xx=0, retried=0), avg=18 ms, max=240 ms,
#   busy refusals=0, join-events failed=0, join-events dropped=0, transfers failed=0,
#   server-list refreshes failed=0, waiting now=0
journalctl -u felis-velocity | grep 'server list refresh'
kubectl -n minecraft logs login-0 | grep 'link status poll'
```

`no answer` rising with a flat `felis_http_requests_total` means the path to the
internal face is broken (NetworkPolicy, Service, the api Pod down); `busy
refusals` or `join-events dropped` above zero means felis-api is slower than the
proxy's pool of 8 threads can absorb. A refresh or link-status outage warns when it
starts, every 5 minutes while it lasts with the failure count, and at info when
it recovers.

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
  `go_*`/`process_*` runtime series, the `go_sql_*` pool series,
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
(`curl -fsSL <installer URL> | sudo bash`), which imports the release's images
(or rebuilds them on the host, operations §1 "Where the binary and the images come
from") and re-applies the bundle. `felis update --panel` prints that command with the
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
| the image tars `felis-image-*-linux-<arch>.tar`, their listing `felis-images-linux-<arch>.txt`, `felis-velocity.jar` (release channel, `FELIS_ARTIFACT_DIR`) | each sha256 must match the release's `SHA256SUMS`; the listing must give one known role per line with sha256 digests, and each image must be in containerd under the digest the listing names once its tar is imported. An asset that fails is built on the host instead (§15c); from `FELIS_ARTIFACT_DIR` the install stops |
| k3s's own images (`k3s-airgap-images-<arch>.tar.zst`) | against the k3s release's `sha256sum-<arch>.txt`; a mismatch leaves k3s pulling them from Docker Hub |
| k3s (fresh install, or `FELIS_UPGRADE_DEPS=1`) | the install script is read at `FELIS_K3S_VERSION`'s tag (default `v1.36.4+k3s1`), and it checks the binary against that release's sha256 list |
| cloudflared (when absent, or `FELIS_UPGRADE_DEPS=1`) | release `FELIS_CLOUDFLARED_VERSION` (default `2026.9.1`) against a pinned sha256; another version needs `FELIS_CLOUDFLARED_SHA256` |
| Go toolchain (nano, source builds) | pinned sha256 per architecture; another version needs `FELIS_GO_SHA256` |
| the registry and PostgreSQL images | pinned by digest (`registry:2.8.3@sha256:a3d8…`, `postgres:18.6-trixie@sha256:5a5a…`); a release's copy must carry that name and digest |
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
| `felis-velocity` (the proxy) | its unit, the JRE, `velocity.jar`, `velocity.toml`, the forwarding secret, the felis-link settings (all but `service-token`, which the plugin re-reads) or a plugin jar changed, or it was not running. The fingerprint lives in `/etc/felis/velocity.fingerprint`; delete it to force a restart. |
| login and lobby pods | the rebuilt limbo or lobby image has a new image ID (`/etc/felis/system-server-images`). The installer then pins that server's `spec.image` to the digest its tag names now (`felis pin-images --system login\|lobby`) and the operator rolls the pod onto it, each on its own; with the registry unreachable it recreates the pod instead. The installer turns off BuildKit's default provenance attestation (`BUILDX_NO_DEFAULT_ATTESTATIONS=1`): it records the build time, which would give every rebuild a new ID. |
| felis-postgres | the release moved `POSTGRES_IMAGE` or changed the pod; a few seconds without the API. A rerun that changes neither leaves it running. |
| felis-api, felis-operator, the registry pod (its gate and GC containers run the felis binary) | the image tag changed (an upgrade), or a same-version rerun rebuilt it. |

**PostgreSQL across reruns.** The database runs in k3s from the image the release
pins, so a distribution upgrade never moves it. The installer refuses to start a
PostgreSQL whose major version differs from the cluster in `/var/lib/felis/postgres`
and names the dump-and-restore path (docs/operations.md §4). The first rerun of a
release with felis-postgres on a host an earlier release installed moves the
database off the host PostgreSQL (docs/operations.md §4, "The database's move into
k3s"), and removes that release's `felis-postgres-firewall.service`. On Arch the
installer's `pacman -Syu` keeps holding the host `postgresql` package back while its
cluster exists: that cluster is the copy a rollback of the move starts again.

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
The login and lobby servers are pinned the same way, by the installer, each time
it moves them onto a new build, so `kubectl -n minecraft get minecraftserver login
-o jsonpath='{.spec.image}'` names the build the login gate runs.

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
| Installer warns `could not pin the login system server` (or lobby) | the registry did not answer right after the push | the installer recreated the pod instead, which starts the new build only while `spec.image` names the bare tag; rerun the installer once `kubectl -n felis get pods -l app.kubernetes.io/component=registry` is Ready |
| A running server restarted during an installer re-run | it was pinned in place: the operator rolled it onto the pinned ref, the build it already ran | nothing; it happens once per server |
| Create/edit refused with `the registry no longer holds build …` | the image names a digest the pruner deleted: nothing referenced it for 24 hours (§9) | pick a current tag; whitelist the versioned tag of a build you want kept |

## 15c. The installer builds on the host although it installs a release

A release install takes its images and the Velocity plugin from the release's assets
(operations §1, "Where the binary and the images come from"). When one cannot be used the
installer names it and the reason, and builds that image with Docker instead (the registry
and PostgreSQL images are pulled from Docker Hub). Nothing unchecked is used either way
**[SH-TESTED]**. Each download is tried three times, five seconds apart, before it counts as
failed, and a transfer that stalls under 1 KiB/s for a minute is cut off and tried again.

Preflight counts no Docker builds for a release install, so the first build it falls back
to checks the room first: Docker's images and build cache take about 8 GiB under
`/var/lib/containerd` (2 GiB when Docker already has a cache there). Without it the install
stops before Docker is installed, with `building here what the release did not supply takes
about … MiB under /var/lib/containerd, and … has … MiB free; nothing has been built`. Rerun
once the asset downloads (the messages above it say which one failed), free that space, or
set `FELIS_PREFLIGHT=warn` to build anyway **[SH-TESTED]**.

| Message | Meaning | What to do |
|---|---|---|
| `release vX publishes no SHA256SUMS … building them on this host instead` | the release predates release assets, or release.yml is still uploading them | nothing for an old release; for a new one, rerun once the release page lists `SHA256SUMS` |
| `SHA256SUMS lists no <file>` or `could not download <file> from release vX` | the release lacks that asset (a partial upload) | rerun later; the host build is correct meanwhile |
| `downloaded <file> hashes to …, but release vX's SHA256SUMS says …` | the download was corrupted, or the asset was replaced after `SHA256SUMS` was written | rerun: the bad copy is gone and is fetched again; the same mismatch every time means the asset itself is bad, so report it |
| `the release's image listing … is malformed` | `felis-images-linux-<arch>.txt` does not parse | report it; every image is built on the host |
| `the release's registry image is …, but this installer runs …` | the release's base tar carries another digest than this `bootstrap.sh` pins: the installer and the release are from different versions | run the installer read at the release's tag (`felis update --panel` prints that command) |
| `k3s containerd holds no <image> from <tar>` | the tar was imported but did not hold the image under the listed digest | `sudo k3s ctr images ls \| grep felis` shows what it holds; report it |
| `FELIS_ARTIFACT_DIR: …`, and the install stops | an asset is missing from the directory or fails its checksum; nothing is built from a directory | copy the named file from the release again and rerun |

An install that stops part way leaves the downloaded tars in `/var/lib/felis/artifacts`; the
rerun reuses those that still match `SHA256SUMS` and deletes the directory once the images
are in the registry.

## 16. Control-plane database backups and disaster recovery

The PostgreSQL database behind felis-api holds everything that is not a world:
accounts, passkeys, Minecraft account links, server ownership, quotas, audit
logs, and the `world_backups` index that maps an archive (§10) back to its
owner. Losing it orphans every world archive. It runs in k3s as the
`felis-postgres` Deployment, with its cluster on the host in
`/var/lib/felis/postgres`; `felis db` runs `pg_dump`, `psql` and `pg_restore`
inside that pod (the host config's `[database] deployment`) and keeps the
bundles on the host.

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
| `state/etc/felis/...` | every file in `/etc/felis`: `secrets.env` (DB password, forwarding secret, caller and registry tokens), `felis.host.toml`, `felis.pod.toml`, the `felis.toml` symlink, `offsite.env` (bucket credentials and encryption key), `smtp-password`, `uploads-s3-access-key` and `uploads-s3-secret-key` (the mail relay password and the uploads bucket keys `felis setup` took), the panel TLS pair, and the installer's own markers (`system-server-images`, `velocity.fingerprint`). `bootstrap.done` is left out on purpose |
| `k8s/minecraftservers.json` | every MinecraftServer, status and server-side metadata stripped, ready for `kubectl apply`. The export is tried 3 times, 10 s apart; when the cluster still does not answer, the bundle is written without it and the manifest records why (next section) |

next to a `.sha256` sidecar in `sha256sum` format. **A bundle contains the
secrets; treat it like `/etc/felis` itself.** Retention per label: `daily` 14
(`FELIS_DB_BACKUP_KEEP`), `pre-migrate` 10, `pre-restore` 5, `offsite` 1
(taken by the off-site copy, §16), `manual` never pruned.

Installer knobs: `FELIS_DB_BACKUP_DIR`, `FELIS_DB_BACKUP_KEEP`,
`FELIS_DB_BACKUP_TIME`, `FELIS_DB_BACKUP_METRICS` and
`FELIS_PRE_MIGRATE_BACKUP` (below).

### Is the newest backup fresh?

Four places answer, all with the same 26 h limit on the newest **daily**
bundle, the one `felis-db-backup.timer` writes. A manual, `pre-migrate` or
`offsite` bundle taken since counts for a restore and leaves the alarm
standing: the timer has still stopped, and that bundle only ages from here.

- The panel: **管理 → 维护与备份** shows the newest backup, its kind and size,
  and turns red with the fix commands when the daily one is missing or overdue
  (read from the `db_backup_last` platform setting each backup writes; its
  `daily_at` is the newest daily bundle on disk when it was written).
- `sudo felis db check` exits 1 with the reason; `sudo felis db list` shows every
  bundle with its age.
- The watchdog mails the owners (§14).
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
sudo systemctl start felis-db-backup.service         # run the daily backup now; clears the alarm
```

**A bundle without the MinecraftServer objects.** When the cluster does not
answer the export (`k3s kubectl get minecraftservers`) three times running,
the bundle is still written, since it holds the database, but a restore from it
brings back no servers. `felis db backup` then exits 1 (the timer's run shows
failed) after `wrote ...` and the reason; the panel card turns amber
(**不完整**) with the reason and the commands; the watchdog mails the owners
(`the newest control-plane database backup ... lacks the MinecraftServer
objects`) while that bundle is the newest; `felis db verify` and
`felis offsite list`/`fetch-db` name the gap. A pre-migrate or pre-restore
bundle goes the same way without stopping the upgrade or the restore, whose
rollback needs the database alone. The bundle the off-site copy takes after
copying archives refuses to go without them and is tried again next pass. Once
`sudo k3s kubectl get minecraftservers -A` answers, run `sudo felis db backup`.
[GO-TESTED: `internal/dbbackup`, `cmd/felis`, `internal/watchdog`; the panel card
in `DBBackupCard.test.tsx`]

Common failures: felis-postgres not running (`kubectl exec` reports no running
pod, or `pg_dump: ... connection refused`; next section); `k3s: executable file not
found` from a `felis` that runs with neither `/usr/local/bin` on PATH nor k3s
anywhere else; the backup directory's disk full (the half-written `.partial` is
removed and the previous bundles stay intact); `pg_dump: server version mismatch`
when an external database (no `deployment` in `[database]`) is newer than the
host's client tools (install the matching `postgresql` client package).

### felis-postgres is not running, or never became ready

The installer stops with `felis-postgres did not become ready` after 10 minutes
and prints the pod's events; the watchdog reports the Deployment the same way it
reports felis-api. Look at the pod:

```sh
sudo k3s kubectl -n felis get pods -l app.kubernetes.io/component=postgres -o wide
sudo k3s kubectl -n felis describe deploy/felis-postgres | tail -n 30
sudo k3s kubectl -n felis logs deploy/felis-postgres --tail=60
```

| What it says | Cause | Fix |
|---|---|---|
| `ImagePullBackOff` / `ErrImagePull` on `postgres` | the node cannot reach Docker Hub and has no copy | §8e, "The platform's own images on an air-gapped node" |
| `CreateContainerConfigError`, `secret "felis-postgres" not found` | the superuser Secret is gone | rerun the installer, which makes a new one. The image reads it only when it creates a cluster; the installer and `felis db` reach the database over the pod's socket |
| the log says `Permission denied` on `/var/lib/postgresql/18/docker` | the hostPath lost its owner (uid 999) or, with SELinux enforcing, its `container_file_t` label (a restore of `/var/lib/felis` by hand, `restorecon` without the installer's rule) | rerun the installer, which sets both; by hand: `sudo chown -R 999:999 /var/lib/felis/postgres` and `sudo restorecon -R /var/lib/felis/postgres` |
| the log says `database files are incompatible with server` | the cluster was made by another major version | docs/operations.md §4, "PostgreSQL major versions" |
| `Pending`, `Insufficient memory` | the node is full | §13b |

Query the database by hand from inside the pod, as the superuser over its socket:

```sh
sudo k3s kubectl -n felis exec -it deploy/felis-postgres -c postgres -- psql -U postgres felis
```

The copy an earlier release's host PostgreSQL still holds, from before the move
into k3s, stays on the host; docs/operations.md §4 covers going back to it.

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
  errors). [PG-TESTED]
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
release and would re-apply the migrations you are rolling back. Bring the host
back in line by installing the earlier release from its own assets, with the
installer read at that same tag (`v1.2.3` here):

```
curl -fsSL https://raw.githubusercontent.com/FelisMC/Felis/v1.2.3/deploy/bootstrap.sh \
  | sudo FELIS_RELEASE=v1.2.3 bash
```

`FELIS_RELEASE` makes the installer install that release where it would
otherwise resolve the newest one, which would install the new release again and
re-apply its migrations. It is refused together with `FELIS_REF`,
`FELIS_ARTIFACT_DIR`, `FELIS_SKIP_FETCH` or `FELIS_VERSION_BOOTSTRAP=dev`, and
an unpublished tag stops the install before anything changes. [SH-TESTED]

- An installer older than `FELIS_RELEASE` ignores it and installs the newest.
  `curl -fsSL <that URL> | grep -c FELIS_RELEASE` prints `0` for one of those;
  run it with `FELIS_REF=v1.2.3` instead, which builds that tag from source
  (slower, and it needs the build resources of §15c).
- While the repository is private, read the installer through the README's
  token'd form with `?ref=v1.2.3` after `contents/deploy/bootstrap.sh`, and run
  it as `sudo -E FELIS_RELEASE=v1.2.3 bash`.

### Whole-host disaster recovery: what comes back, and from where

When the host (or its disk) is gone, everything Felis needs comes back from the
off-site bucket (next sections) plus a fresh install. What the host holds:

| Data | On the host | In the bucket | Brought back by | Lost at most |
|---|---|---|---|---|
| Control-plane database (accounts, passkeys, ownership, quotas, audit, submissions, the `world_backups` index) | felis-postgres, `/var/lib/felis/postgres` | every bundle, copied within the hour of being written, plus a fresh one after every pass that copied a world archive | `fetch-db`, `db restore` | changes since the newest bundle: up to a day plus an hour with the daily timer |
| Host state (`/etc/felis`: secrets, both `felis.toml` copies, `offsite.env`, the mail relay password and uploads bucket keys, panel TLS pair) | `/etc/felis` | inside every bundle | `tar -x` of the bundle's `state/` | as the database |
| MinecraftServer objects | k3s | inside every bundle (`k8s/minecraftservers.json`) | `kubectl apply` | as the database |
| World archives (reaper, "Back up now", pre-restore snapshots) | `felis-backups` volume | each one within the hour, followed by a bundle listing it | `fetch-worlds` | archives written in the last hour |
| Live worlds | `world-*` volumes under `/var/lib/rancher/k3s/storage` | **only as their archives** | a restore from the newest archive (§10) | everything since that world's newest archive |
| User images | `registry` volume | hourly; image lists kept 14 days | `fetch-images` | images pushed in the last hour |
| Submission uploads (modpacks awaiting or past review) | `felis-uploads` volume | hourly; upload lists kept 14 days | `fetch-uploads` | uploads of the last hour |
| The Cloudflare Tunnel connector (when the panel is behind one) | `/etc/felis/cloudflared.yml`; `/root/.cloudflared/cert.pem` and `<tunnel-id>.json`; `cloudflared-felis.service` | the config inside every bundle; the login certificate, the tunnel's credentials and the unit are not copied | the Cloudflare step of `sudo felis setup` (step 11 below) | nothing: the tunnel, its DNS records and the Access application live at Cloudflare |
| Platform images, Velocity, the JRE, build tools | registry, `/opt/felis` | not copied | the installer builds and pushes them again | nothing |
| k3s itself (its token, CA, datastore, Secrets, Deployments) | `/var/lib/rancher/k3s` | not copied | the installer makes a new single-node cluster and renders every Secret and Deployment from `/etc/felis` | nothing: no Felis data lives only there |

**Live worlds are the gap.** A world's current state exists only in its
volume; the bucket holds the archives the reaper, an owner's "Back up now" or
a pre-restore snapshot wrote. A world that was never archived comes back as a
server with an empty world. Tell owners to back up before anything they would
hate to lose, and treat the newest archive's age (the server's backup page) as
that world's recovery point.

For a smaller database loss window, give the installer a tighter
`FELIS_DB_BACKUP_TIME` (any systemd calendar, e.g. `*-*-* 00/6:30:00` for
every 6 hours) with a matching `FELIS_DB_BACKUP_KEEP`, on every installer
run; the hourly off-site copy picks each bundle up within the hour.

How long a rebuild takes is mostly transfer time. The installer on a blank
host builds and pushes every platform image, so it runs longer than an upgrade
and depends on the host's network; the database restore takes seconds to
minutes; the three fetches move what `sudo felis offsite status` reports the
bucket holding (images, uploads, world archives) at the bucket's bandwidth,
and each skips what is already in place, so an interrupted one resumes. Write
those sizes down with the bucket's download rate and you have the recovery
time for your install. A whole-host rehearsal on a spare machine, once per
release, is the way to know it for sure. The spare follows the steps below
without steps 8 and 11. It finds the production host named in the bucket and
stands by, so it copies nothing into the bucket, prunes nothing there and,
while production keeps writing, mails none of the owners its restored
database holds. A second connector on the production tunnel would take a
share of the real visitors, so the spare leaves the tunnel alone and is
reached by its own address (step 10 moves it to one).

The order below matters: the state goes in before the installer so it reuses
the old secrets and bucket; the images go back before the database so the
servers the database restores find the digests they pin, inside the pruner's
24-hour grace; the MinecraftServers go back with the database that names their
owners; the worlds come after them because a restore needs a server to
restore into. Mail is checked before the domain moves, because a move can
cost passkeys and email codes are the way back in; the domain moves after the
MinecraftServers are applied, since the login gate's env carries it.

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
   checks it (`felis db verify`) and names it, with when it was taken, the
   release and schema that took it, and how many accounts and servers its
   database holds. Read those before going on. A wrong key fails with
   `object does not decrypt with this key` (naming the bucket's key id and this
   key's, once the bucket records one) and writes nothing. For a copy you
   made yourself, check it with `sha256sum -c felis-db-....tar.sha256`.

   `latest` is the newest bundle, unless that one holds no servers and at
   most one account while an older one holds more. That is the database a
   rebuilt host copies off-site when it takes the bucket over before anyone
   restores onto it (with an older release, within its first hour), so
   `fetch-db latest` refuses it and names up to
   three older bundles with their counts; fetch the one you want by name in
   place of `latest` (`felis offsite list` shows them all, each with its
   counts). A bundle fetched by name that looks like a new install's is
   written with a warning. Bundles from releases before the counts were
   recorded show `not recorded`.
2. Put the old host's state in place **before** installing, so the installer
   reuses the same DB password, caller tokens, forwarding secret, the mail
   relay password and uploads bucket keys `felis setup` took, and the
   `[offsite]` bucket with its credentials and key (`offsite.env`):

   ```
   sudo install -d -m 0700 /etc/felis
   sudo tar -xpf felis-db-....tar -C / --strip-components=1 state/etc/felis
   ```

3. Run the installer as for a first install. `bootstrap.done` is not in the
   bundle, so it takes the fresh-install path, creates the empty database with
   the restored password and migrates it. It finds `[offsite]` in the restored
   `felis.host.toml`, turns the hourly copy back on and reports that another
   host writes the bucket: this host was built from its backup, so it stands
   by and copies nothing there until step 8, and ends with `OFF-SITE COPY ON
   STANDBY`.
4. Push the user images back into the new registry:

   ```
   sudo felis offsite fetch-images
   ```

   It restores the newest image list in the bucket (`-at <stamp>` for an
   older one; `felis offsite list` shows them) through the registry's loopback
   port as the platform principal, verifying every manifest and layer against
   its digest, and pushes only what the registry lacks. The pruner counts a
   restored image as freshly pushed and keeps it for 24 hours; finish the
   database step within that window so the restored servers and whitelist
   entries keep naming it. When the new host has already taken the bucket over
   and recorded its still-empty registry, `fetch-images` refuses that newest list and names
   the version to pass with `-at`; `fetch-uploads` does the same.
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

   When `tar` answers `Not found in archive`, the bundle lacks the
   MinecraftServer objects (`fetch-db` said so when it fetched it). Fetch the
   newest bundle `felis offsite list` shows without `no MinecraftServer
   objects` and apply its `k8s/minecraftservers.json` instead; servers that
   bundle does not list do not come back from it.

7. Bring the world archives back into the archive volume:

   ```
   sudo felis offsite fetch-worlds
   ```

   It fetches every archive the restored `world_backups` index lists as present
   and the volume lacks, provisioning the `felis-backups` volume first if
   nothing has used it yet (a short-lived `felis-bind-felis-backups-*` pod). It
   lists any it could not find in the bucket. Restore a world from its archive
   as usual (§10, §13). With the newest bundle restored, every archive in the
   bucket is listed; an older bundle leaves the archives copied after it
   unlisted, and the sync removes those once they pass the longest retention.
8. Make this host the one that writes the bucket, and send its first copy:

   ```
   sudo felis offsite take-over          # names the host the bucket names now
   sudo felis offsite take-over -yes
   sudo systemctl start felis-offsite.service
   ```

   Without `-yes` it names the host the bucket records and when that host last
   wrote it, and exits 4. With `-yes` it records this host; the old host, if it
   ever runs again, copies nothing more and says it was taken over. Skip this
   step on a rehearsal machine.
9. Check that this host can send mail, when the old one had a relay. Email
   codes are how users sign in without a passkey, and step 10 can cost them
   their passkeys. Run `sudo felis setup`, press `e` (configure email) on the
   status screen and save the pre-filled relay with its password typed again
   (the form never shows the stored one). Saving delivers one self-test
   message to the From address and writes nothing unless it arrives. A relay
   that admits senders by address, or an SPF record for the From domain that
   lists the old host's address, refuses this host or sends its mail to spam:
   add the new address there and save again. With no relay at all, an Owner
   who cannot sign in recovers with `sudo felis breakGlass` (§17).
10. Move the install to this host's address when its names still lead to the
    old one. The installer kept the bundle's root domain (its log says
    `(reusing the installed domain)`).

    - The `<old-address>.nip.io` default resolves to the dead host by
      construction. Move it:

      ```
      sudo felis domain set <new-address>.nip.io       # the plan
      sudo felis domain set -yes <new-address>.nip.io
      sudo felis domain check
      ```

      It runs after step 6 because the MinecraftServers applied there carry
      the domain in the login gate's env. The plan counts the passkeys that
      stop working; their users sign in with an email code (step 9) and
      register a new one. docs/operations.md §6 has the rest of what it moves.
    - A domain of your own stays. Point its records at the new address:
      `<root>`, `*.<root>`, `console.<root>` and `op.console.<root>`, or only
      `<root>` and `*.<root>` when the tunnel serves the panel. Confirm each
      with `dig +short <name>` before going on: `felis domain check` warns
      while a name does not resolve and accepts any address once it does.
      Lower the records' TTL beforehand if the old host is still around to
      plan with.
11. Bring the Cloudflare edge back, when the old host served the panel
    through a tunnel. The bundle carries `/etc/felis/cloudflared.yml`; the
    login certificate (`/root/.cloudflared/cert.pem`), the tunnel's
    credentials (`/root/.cloudflared/<tunnel-id>.json`) and the
    `cloudflared-felis` unit stay behind with the old disk, so the panel
    hostnames answer Cloudflare error 1033 until a connector runs here.

    Run `sudo felis setup`, press `c` (change connection) on the status
    screen and choose Cloudflare Tunnel + Access. On the step's first screen
    press `i` to install cloudflared, then `l` for `cloudflared tunnel login`
    (browser consent on your account, which writes `cert.pem`); `enter` opens
    the form once both are in place. Enter an API token and the account ID,
    then the same Admit identity, hostnames and tunnel name as before. The
    hostnames come pre-filled from the restored config and the tunnel name
    defaults to `felis`; for another name, look up the id on the `tunnel:`
    line of the restored `cloudflared.yml` in Zero Trust → Networks → Tunnels.

    The step finds the existing tunnel by name and fetches its credentials
    again, points the panel records at it, finds the existing Access
    application (so its audience stays the one the restored config names)
    and rewrites its policy, installs and starts `cloudflared-felis`, and
    closes the panel's NodePort once the tunnel serves. A kept copy of the
    old `<tunnel-id>.json` can go back into `/root/.cloudflared` (mode 0600)
    first; the step still needs `cert.pem`. A different tunnel name makes a
    second tunnel, moves the panel records to it and leaves the old one idle:
    delete that one in the dashboard afterwards. Skip this step on a
    rehearsal machine.

Check the rebuild before letting players in:

```
sudo felis db check                     # the database answers and has a fresh bundle
kubectl get minecraftservers -A         # every server the bundle held
kubectl -n minecraft get pods           # servers pull their pinned images (no ImagePullBackOff)
sudo felis offsite status               # the hourly copy runs from this host again
sudo felis domain check                 # every name, the certificate and the tunnel on the new address
```

A rehearsal machine that restored a tunnel install and moved to a `nip.io`
name in step 10 fails the Cloudflare tunnel line, since the tunnel still
routes production's names; that one failure is expected there.

In the panel, reached by its hostname (steps 10 and 11):

- Sign in with an old account; accounts and passkeys come back with the
  database, except passkeys step 10 counted as lost.
- Sign out and sign in again with an email code, to an Owner account whose
  inbox you read. The code arriving proves felis-api itself mails through the
  relay to an outside inbox; step 9's self-test came from the setup console
  and went to the From address. A code that never comes: §17.
- Open a restored server's backup page and confirm its archives are listed,
  restore the newest one, start the server and join it at
  `<name>.<root>`.

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
Without a bucket the installer ends with `NO OFF-SITE COPY`. Installers before
the fix for a pipe race in reading `[offsite]` could also end that way on a
host that has a bucket, and removed `felis-offsite.timer` as they did; a re-run
of the current installer puts it back. `systemctl list-timers felis-offsite.timer`
shows whether the timer is there. [SH-TESTED] [VM-TESTED: a re-run that lost the race]

The bucket records which key sealed it: `felis-key-id`, the one object kept in
the clear, holds the key's id (the `key id:` of `felis offsite status`), which
names the key without revealing it. The first sync writes it; a bucket from
before it was recorded is judged by whether the key opens its newest objects.
The installer runs `felis offsite check-key`, and every sync checks before
anything else. A key other than the bucket's (a reinstall that lost
`offsite.env` and was given no `FELIS_OFFSITE_KEY` generates a new one) stops
the sync before it copies or prunes anything, the installer ends with
`OFF-SITE COPY STOPPED`, `status` exits 1 and the watchdog mails the owners
at once. Set `FELIS_OFFSITE_KEY` in `/etc/felis/offsite.env` to the bucket's
key and `sudo systemctl start felis-offsite.service`, or give `[offsite]` an
empty bucket or prefix and re-run the installer.
[GO-TESTED: `TestSyncRefusesAnotherKeysBucket`, `TestCheckKey`] [SH-TESTED]
[VM-TESTED: MinIO, the other key's sync refused with the bucket unchanged, check-key 0/3, fetch-db naming both ids]

The bucket also names the host that writes it: `felis-writer`, rewritten by
every sync, holds that host's id (kept in `/var/lib/felis/offsite/host-id`,
which no bundle carries), its name and the time of its last run. A host
restored from a bundle has the bucket's key and credentials but no id, so it
finds another host named there and stands by: its syncs copy and prune
nothing, `felis offsite status` names the host that writes the bucket and exits
1, and while that host ran in the last 3 hours the watchdog holds this host's
mail (§14). Once it has not run for 3 hours, the watchdog mails this host's
owners that it copies nothing into the bucket. A bucket that holds sealed
objects and names no writer puts a host on standby too, except a host that
copied to it with a release before writers were recorded, which claims it on
its next sync. `sudo felis offsite take-over` names the host that writes the
bucket; with `-yes` it records this host instead (step 8 of a rebuild). The
host it replaced copies nothing from its next sync on: its `status` exits 1,
and its watchdog mails its owners at once that `the off-site copy has
stopped`. When that happened by mistake (a rehearsal machine took the bucket
over), run `sudo felis offsite take-over -yes` on the production host to take
it back. [GO-TESTED: `TestLeasePlan`, `TestSyncStandsBy`, `TestOffsiteTakeOver`,
`TestRestoredHostKeepsStandingBy`] [SH-TESTED]
[VM-TESTED: MinIO, a restored host standing by with the bucket unchanged, take-over, the old host displaced and taking it back, an older release's host claiming its prefix]

What runs:

- **`felis-offsite.timer`** runs `felis offsite sync` hourly (plus up to
  10 min random delay, `Persistent=true`). Each run copies every world archive
  whose row has no `offsite_at` yet and records it, copies the newest
  `db_keep` database bundles the bucket lacks and prunes older ones there, and
  deletes a world archive from the bucket once its row has been deleted and
  its retention (`expires_at`) has passed. An object already in the bucket at
  the right size is recorded without being sent again, so a run cut short
  resumes. [GO-TESTED: `internal/offsite`]
- A run that copied a world archive then takes a fresh `offsite` database
  bundle (`felis-db-<stamp>-offsite.tar`, the same layout as a daily one, host
  state from `-state-dir`, default `/etc/felis`) and sends it, so the newest
  bundle in the bucket lists every archive there and a restore from it fetches
  them all. The host keeps one such bundle locally, and the bucket keeps it
  only while it is the newest; the `db_keep` count covers the other labels, so
  a busy day of snapshots never pushes the dailies out. A run that copied
  nothing, or whose newest bundle already postdates the last copy, takes none.
  The panel's backup card keeps watching `felis-db-backup.timer` alone.
  [GO-TESTED: `internal/offsite`, `TestOffsiteSyncerSnapshotsAndSweeps`]
  [PG-TESTED]
- A world archive in the bucket that no row lists (the database came back
  from a bundle older than the archive) is removed once it has been in the
  bucket longer than the longest `[archive]` retention (`retention`,
  `manual_retention`, `scheduled_retention`); younger ones stay, and the run
  logs how many and when each goes. The sweep skips a database that lists no
  archive at all, so a sync against a database not restored yet removes
  nothing. [GO-TESTED: `internal/offsite`]
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
  `felis-key-id` and `felis-writer` next to them hold the key's id and the
  host writing the bucket in the clear.
- A pass sends the database bundles first, then world archives, the bundle
  listing them, images and uploads. Each object has its own time limit: 10 minutes plus its size at
  512 KiB/s (about 6 hours for 10 GiB). An archive the uplink cannot send in
  that time fails alone, stays pending and is tried again next pass; the rest
  of the pass still goes. A pass over a big archive can run for hours; the
  timer starts no second one meanwhile. An upload cut off restarts from the
  beginning of that object. [GO-TESTED: `internal/offsite`]
- The reaper deletes an idle world only after its archive is in the bucket
  (§10).
- The watchdog mails the owners when no sync has completed for 12 hours
  (`the off-site copy last completed ... ago`), and at once when the bucket
  refuses this host's key or another host took the bucket over.

Checking it:

```
sudo felis offsite status        # last run, errors, what the bucket holds, what waits
sudo felis offsite list          # the bundles, image lists and upload lists in the bucket, newest first
sudo felis offsite check-key     # whether offsite.env holds the key the bucket was sealed with
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

## 17. Sign-in refused: 429 limits, no mail relay, account code locks, failed sign-ins

The public sign-in doors (`/api/v1/auth/*` except logout and the op-login
status poll) have three limits of their own. Each answers 429 with a
`Retry-After` header and a distinct error code. The doors that mail a code
also answer 503 `mail_unavailable` on an install with no mail relay.

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

### `mail_unavailable`: no mail relay

With no `[smtp]` section every door that mails a code answers 503
`mail_unavailable` before minting one: email sign-in, op.console sign-in,
email verification, and the email step-up for sensitive changes and
migration. The public doors answer before looking up the address, so every
address gets the same reply. Sign-in is by passkey only, and a verified email
stops counting as a way into the account (it is no longer offered as a
re-verification factor). Codes are never logged: `felis api` says at start
`[smtp] not configured`. Run `felis setup` and configure email to open the
doors.

### Relay refused for lacking TLS

A relay on port 465 is spoken to over TLS from the first byte. On any other
port Felis upgrades with STARTTLS, and when the relay does not offer it the
send fails with `smtp: <host>:<port> does not offer STARTTLS` (502
`mail_undeliverable` to the caller, the full text in the `felis-api` log, and
the same error on the `felis setup` email screen). Without TLS anyone on the
path reads the codes, and anyone who can rewrite the conversation can strip
the STARTTLS offer, so this is the default for every relay except one on this
host (`localhost`, `127.0.0.0/8`, `::1`). Use port 465 or a relay that offers
STARTTLS. For a relay you reach over a link you trust, set
`require_tls = false` under `[smtp]` in `/etc/felis/felis.toml` and
`/etc/felis/felis.pod.toml`; installer re-runs and the setup email screen keep
it. `felis api` warns at start whenever codes may go out without TLS.

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
sudo k3s kubectl -n felis exec deploy/felis-postgres -c postgres -- psql -U postgres felis -c \
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
sudo k3s kubectl -n felis exec deploy/felis-postgres -c postgres -- psql -U postgres felis -c "
  SELECT created_at, action, actor, client_ip, payload->>'reason' AS reason
  FROM audit_logs
  WHERE action LIKE 'auth.%' AND created_at > now() - interval '1 hour'
  ORDER BY created_at DESC LIMIT 50;"
```

A failed audit write does not fail the action; it logs `audit: lost ...` in
`felis-api` and counts in `felis_audit_write_failures_total`
(`FelisAuditWriteFailing`). The cause is almost always PostgreSQL (§16).

### How long rows are kept

`felis-api` prunes the database a minute after it starts and every 6 hours
after that, and logs `retention: pruned spent rows` with a count per table:

| Rows | Deleted |
|---|---|
| sessions | 30 days after they expired or were signed out |
| email codes, passkey challenges, setup links, op-login requests | 30 days after they expired or were used |
| `/felis link` bind codes | 30 days after they expired |
| account migrations that never completed | 30 days after their last step (completed ones stay) |
| wrong-code windows (`otp_failure_windows`) | 30 days after they began |
| `audit_logs` | once older than `[audit] retention` |

`[audit] retention` defaults to `365d`; it takes days (`90d`), months of 30
days (`18mo`) or `forever`, and refuses anything under `30d`. The daily
`felis db backup` bundles (§16) still hold the rows for as long as the bundles
are kept. To keep audit rows past the retention, export them before they go:

```sh
sudo felis db audit-export -until 2026-01-01 -out /root/audit-2025.jsonl
```

`-since` and `-until` take a day (UTC midnight) or an RFC 3339 instant; the
window is `[since, until)`. Each line is one row as JSON, oldest first. The
file is created `0600` and an existing file is never overwritten.

### `felis breakGlass`: the recovery code and the OVERRIDE [VM-VERIFIED]

Once a staff account exists, `sudo felis breakGlass` asks which admin or owner
is breaking the glass and mails that account's verified address a six-digit
code through the same `[smtp]` relay as the sign-in codes. The code works for
10 minutes and five wrong ones end it. The console reads the relay password
the way the watchdog does (the `password_ref` env var, else
`/etc/felis/smtp-password`, else the `felis-smtp` Secret, else no AUTH), so a
host whose k3s is down still gets its code, and the mail skips the API's
`max_per_hour` budget.
Only the right code makes the run a `recovery` attributed to that account; the
mail says which host and OS user asked, so an admin who did not ask learns
that root there is in other hands.

Every other ending leads to the typed `OVERRIDE`, and the screen says why:
the name matches no staff account, the account has no verified address, no
relay (`[smtp] is not configured in felis.toml`, or the Secret could not be
read), the relay refused the mail, the code expired or took five wrong tries,
or the operator typed `OVERRIDE` at the code prompt. Esc on either screen
starts over with a new code. With the relay down recovery still works, as an
unverified override that records the reason.

The audit row is `break_glass.recovery` or `break_glass.root_override`
(`break_glass.operator_create` for a new Operator account), source
`break-glass`. `verified` is true only for a run a code proved, which also
carries `verified_by: email_otp` and `code_sent_to`. An override carries
`otp_skipped` (`unknown_admin`, `no_verified_email`, `no_relay`,
`send_failed`, `code_expired`, `code_rejected`, `operator_skipped`) and, where
something failed, `otp_skip_detail`. Root can edit the row afterwards, so it
records attribution without proving it.

```sh
sudo k3s kubectl -n felis exec deploy/felis-postgres -c postgres -- psql -U postgres felis -c "
  SELECT created_at, action, actor, payload->>'verified' AS verified,
         payload->>'otp_skipped' AS skipped, payload->>'otp_skip_detail' AS detail
  FROM audit_logs WHERE source = 'break-glass' ORDER BY created_at DESC LIMIT 20;"
```

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
| The host went down and nothing noticed; `NO HEARTBEAT` at install; `felis-watchdog-failed` in the journal; `state.json.unreadable-*` | §14 |
| `FelisOperatorDown` / `FelisAPIDown` / `FelisLoginGateDown` / `FelisReconcileStuck` | §14, §1, §2 |
| Upgrade / roll back a bad control-plane image | §15 |
| `image_change_unconfirmed` / `image_not_in_registry` / `registry_unavailable`; move a world to a newer Minecraft | §15b |
| Database backup overdue / `FelisDBBackupStale` / panel shows 从未备份 | §16 |
| `pre-migration backup failed, nothing applied` during an upgrade | §16 |
| Undo a mistaken change / restore the control-plane database | §16 |
| Host lost: rebuild from a database bundle | §16 |
| `felis-postgres did not become ready`; `kubectl exec` finds no database pod | §16 |
| `the off-site copy last completed ... ago` / `NO OFF-SITE COPY` / reaper `awaiting_offsite` stays above 0 | §16, §10 |
| Sign-in 429 `rate_limited` for everyone at once | §17 |
| 429 `mail_rate_limited` / `FelisMailBudgetExhausted` | §17 |
| Right code refused; `otp_account_locked` / `FelisOTPAccountLocked` | §17 |
| `FelisSignInFailures` / who is guessing, from where | §17 |
| `FelisAuditWriteFailing` | §17 |
| `felis breakGlass` sends no code / shows `Root override`; `otp_skipped` in the audit | §17 |
| How long sessions, codes and audit rows are kept; export audit rows | §17 |
