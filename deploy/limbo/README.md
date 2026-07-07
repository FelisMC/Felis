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
| `FELIS_LOGIN_TIMEOUT_SECONDS` | login window (clamped 30–3600)     | `300` |
| `FELIS_HEALTH_PORT`         | readiness port                        | `8080` |

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

Import into k3s and point config at it:

```
docker save felis-limbo:demo | sudo k3s ctr images import -
# felis.toml → [velocity] login_image = "felis-limbo:demo"
sudo felis setup
```

## Ports (handled for you)

The entrypoint (`deploy/limbo/entrypoint.sh`) pins Limbo's `server-port` to
`FELIS_GAME_PORT` (default **25565**, the operator's `GamePort`) on every start —
LOOHP/Limbo would otherwise default to `30000`, unreachable through the Velocity
`NetworkPolicy` / Service / probe the operator drives off that one const. It is
idempotent, so a persisted world volume keeps all its other `server.properties`
settings. Do **not** override `FELIS_GAME_PORT` except in lockstep with the operator.

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
- `FELIS_SERVICE_TOKEN` is a **secret**, so it is never written into the CRD. `felis
  setup` replicates the `felis-service-token` Secret from the control namespace into
  the minecraft namespace, and the operator injects it into the `login` pod (only)
  via a `secretKeyRef`, keyed off the reserved `login` name. Until the token is
  present the plugin fail-safes to readiness-only, so the gate is never broken — it
  simply does not authenticate yet.
- **Service:** the login pod dials `FELIS_API_BASE_URL`, which resolves to the
  ClusterIP Service `felis-api-internal` (control namespace) that fronts the api
  pod's internal port 8081. That Service is deliberately separate from the external
  NodePort `felis-api` (443) so the no-Zero-Trust internal face is never published on
  a node's external IP.
- **NetworkPolicy:** none is required today — neither the minecraft-namespace egress
  nor the control-namespace ingress is policy-locked, so the login pod's call to the
  API internal port is reachable. If a future deployment adds a minecraft egress lock
  or a control-namespace ingress fence, it must also open the login-pod →
  felis-api-internal (8081) path.

The Velocity default-landing and waiting-park wiring is printed by `felis setup`
and enforces the invariant: fresh connections hit `login` first; nothing falls
back to the lobby.
