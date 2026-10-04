# Login-limbo image (LOOHP/Limbo + felis-limbo)

The always-on **login** auth gate. `felis setup` provisions it as a system
service (`DesiredState=Running`, reaper-exempt) when `[velocity] login_image` is
set in `felis.toml`. Every fresh connection lands here first; it is the only safe
fallback (a stopped/starting backend routes here, never past authentication).

> **Code-only.** This image is not built by the Go CI. It compiles the
> `plugins/limbo` plugin (plus the shared `plugins/shared` link core it
> srcDir-includes) and bundles it with a LOOHP/Limbo release.

## What the plugin does

`felis-limbo` does two jobs — readiness and the in-game login flow.

### Readiness

LOOHP/Limbo has no RCON, so Felis cannot use its usual RCON readiness probe
(spec §5). A bare TCP check would report "ready" the instant the socket binds.
The plugin instead serves an HTTP readiness endpoint that flips to `200` only
after the first server tick — i.e. once the limbo has genuinely started. The
`login` MinecraftServer sets `spec.startup.healthHTTPPort: 8080`, so the pod's
HTTP readinessProbe (and, with RCON disabled, the operator's Ready gate) follows
that true signal.

- Endpoint: `GET /healthz` on `:8080` (override with `FELIS_HEALTH_PORT`).
- `503 starting` before the first tick, `200 ok` after.
- Fail-closed: if the endpoint cannot bind, readiness never turns green and the
  operator keeps the gate in `Starting` — it never advertises an unstarted gate.

### In-game login (spec §B3)

A player reaching the limbo has been UUID-verified upstream (Velocity online-mode)
but is not yet linked to a web account. On join, off the tick thread, the plugin:

1. checks the username-collision **blacklist** and disconnects a barred squatter
   UUID (the genuine Mojang player — different UUID — passes);
2. mints a one-time **Bind Code** for the verified UUID via the felis-api internal
   face;
3. opens a **book** with a clickable link to `console.<root_domain>` plus a chat
   line carrying the code, and tells the player to finish in their **system**
   browser — never the WeChat/QQ in-app browser, where passkey/WebAuthn does not
   work (the web entry additionally guards this; see `internal/panel`);
4. **polls** `link/status/{uuid}` until the player redeems the code on the web
   console, then **transfers** them to the lobby via a BungeeCord `Connect` plugin
   message on `bungeecord:main`;
5. **disconnects (fail-closed)** on blacklist, on a mint/transport failure, or when
   the login window elapses — "rather refuse than admit unauthenticated".

Configuration (deployment inputs, never compiled in; env wins over a
`felis-link.properties` template written in the plugin data dir on first run):

| Env | Meaning | Default |
| --- | ------- | ------- |
| `FELIS_API_BASE_URL`        | felis-api **internal** face base URL | *(required for login)* |
| `FELIS_SERVICE_TOKEN`       | internal service token (secret)      | *(required for login)* |
| `FELIS_ROOT_DOMAIN`         | deployment zone, builds `https://console.<zone>` | *(required for login)* |
| `FELIS_LOBBY_SERVER`        | Velocity server name to transfer to  | `lobby` |
| `FELIS_LOGIN_TIMEOUT_SECONDS` | login window (clamped 30–3600)     | `600` |
| `FELIS_HEALTH_PORT`         | readiness port                        | `8080` |
| `FELIS_API_CONNECT_TIMEOUT_SECONDS` | felis-api connect timeout (1–120) | `10` |
| `FELIS_API_REQUEST_TIMEOUT_SECONDS` | felis-api call timeout (1–120)    | `10` |

If the API config **or** the root domain is absent the login flow stays **OFF** and
the plugin runs readiness-only (the same "load un-crippled" fail-safe the other
Felis plugins use), so a bare image still boots — production must supply the config
for the gate to authenticate. Transfer requires Velocity to accept the BungeeCord
plugin-message channel (`bungee-plugin-message-channel` on the proxy).

## Build

LOOHP/Limbo has no official image and no release zip. Its CI
(`ci.loohpjames.com/job/Limbo`) publishes two **loose** artifacts per build —
`target/Limbo-<ver>.jar` and `spawn.schem` — so the image is assembled from those
two URLs (there is no bundled `server.properties`; Limbo writes a default on first
run):

```
docker build -f deploy/limbo/Dockerfile \
  --build-arg LIMBO_JAR_URL=https://ci.loohpjames.com/job/Limbo/<n>/artifact/target/Limbo-<ver>.jar \
  --build-arg LIMBO_SCHEM_URL=https://ci.loohpjames.com/job/Limbo/<n>/artifact/spawn.schem \
  --build-arg LIMBO_VERSION=<maven-api-version> \
  -t felis-limbo:demo .
```

- `LIMBO_JAR_URL` (required) — the server jar; it is saved as `Limbo.jar`.
- `LIMBO_SCHEM_URL` (optional) — the default spawn schematic, saved as
  `spawn.schem` and loaded as the spawn world.
- `LIMBO_VERSION` — the Limbo **maven** API version the plugin compiles against
  (Gradle `-PlimboVersion`). This differs from the jar's CI build-qualified
  filename: e.g. the jar `Limbo-2026.0.2-ALPHA-26.2.jar` corresponds to maven
  version `2026.0.2-ALPHA` (the `-26.2` CI qualifier is not published to the
  maven repo).

Publish it into the cluster's registry and point config at it. On the node
itself (docker treats `127.0.0.1` as insecure by default):

```
docker tag  felis-limbo:demo 127.0.0.1:5000/felis/limbo:demo
docker push 127.0.0.1:5000/felis/limbo:demo
# felis.toml → [velocity] login_image = "registry.felis.svc:5000/felis/limbo:demo"
sudo felis setup
```

The registry keys a repository by the path after the host, so pushing through a
`kubectl -n felis port-forward svc/registry 5000:5000` from another machine is
equivalent. Hosting the image in the registry (rather than only importing it
into containerd) is what lets kubelet re-pull it after an image GC.

## Ports (handled for you)

The entrypoint (`deploy/limbo/entrypoint.sh`) pins Limbo's `server-port` to
`FELIS_GAME_PORT` (default **25565**, the operator's `GamePort`) on every start —
LOOHP/Limbo would otherwise default to `30000`, unreachable through the Velocity
`NetworkPolicy` / Service / probe the operator drives off that one const. It is
idempotent, so a persisted world volume keeps all its other `server.properties`
settings. Do **not** override `FELIS_GAME_PORT` except in lockstep with the operator.

It also pins `max-players=-1` (no cap, Limbo's own default): unbound players wait at
the gate for up to ten minutes and a stopped server's players all fall back here at
once, so a cap left on the volume would turn players away at the door.

## Configure (deployer's responsibility)

One setting this image does **not** guess (it keeps the release's own default):

- **Player forwarding** — align Limbo's forwarding with the off-cluster Velocity
  proxy so authenticated players hand off cleanly, and enable the BungeeCord
  plugin-message channel on the proxy so the login gate's `Connect` transfer to the
  lobby lands.

The login flow's own inputs (`FELIS_API_BASE_URL`, `FELIS_ROOT_DOMAIN`,
`FELIS_LOBBY_SERVER`, `FELIS_SERVICE_TOKEN`; see
**[In-game login](#in-game-login-specb3)** above) are wired in for you — you do not
set them by hand:

- The three **non-secret** vars are baked into the `login` MinecraftServer's
  `spec.env` by `felis setup` (`cmd/felis` derives the internal API URL from the
  control namespace — the platform default `felis`; a renamed control namespace must
  be reflected by hand — and the root domain from `felis.toml`).
- `FELIS_SERVICE_TOKEN` is a **secret**, so it is never written into the CRD. The
  login gate has its own internal-API token, `felis-limbo-token`, which may only mint
  link codes, poll link status and check the blacklist. The installer applies it into
  the minecraft namespace (and `felis setup` refreshes that replica from the control
  namespace), and the operator injects it into the `login` pod (only) as
  `FELIS_SERVICE_TOKEN` via a `secretKeyRef`, keyed off the reserved `login` name.
  `sudo felis rotate-token -yes limbo` replaces it and restarts the pod. Until the token is
  present the plugin fail-safes to readiness-only, so the gate is never broken — it
  simply does not authenticate yet.
- **Service:** the login pod dials `FELIS_API_BASE_URL`, which resolves to the
  ClusterIP Service `felis-api-internal` (control namespace) that fronts the api
  pod's internal port 8081. That Service is deliberately separate from the external
  NodePort `felis-api` (443) so the no-Zero-Trust internal face is never published on
  a node's external IP.
- **NetworkPolicy:** the minecraft namespace is egress-locked
  (`felis-server-egress`: DNS plus the public internet, every private range
  excluded), so the internal API is unreachable from a game server by default.
  `felis-login-to-internal-api` opens exactly the login pod → felis-api (8081) path,
  selecting on the reserved `login` name AND the setup-owned
  `felis.lolicon.best/system-role=login` label the operator copies onto the pod — the
  same pair that decides who receives `FELIS_SERVICE_TOKEN`, so a user server cannot
  match it by picking a name.

The Velocity gate/lobby wiring is printed by `felis setup` and enforces the
invariant: fresh connections hit `login` first, and only an authenticated release
from that gate can enter the post-auth lobby or a remembered user backend.

## Customize in the panel

Administrators open **Login & lobby**, select **Login space**, and stop it before
editing. The form configures the login book title/author/heading/link text/help,
automatic book opening and the login timeout (30–3600 seconds). These settings
persist in `/data/felis-experience.json`; an explicit
`FELIS_LOGIN_TIMEOUT_SECONDS` environment variable takes precedence. The generated
code, generated login URL and chat guidance are preserved. The authentication
and transfer destination are not player-facing customization fields.

Use the linked file manager to upload a replacement `/data/spawn.schem`, edit
Limbo's `server.properties` or add Limbo-compatible plugins, then start the space.
Paper world ZIPs and Paper plugins do not work in Limbo. The page also exposes
logs, backups/restore and image/resource settings. New joins are unavailable
while this front door is stopped; a custom image must retain the login plugin
and support the proxy's forwarding protocol.
