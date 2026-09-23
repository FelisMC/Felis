# Felis server-side plugins

These are the in-cluster and edge plugins for Felis. The Velocity proxy and the three loader
mods ship the in-game first leg of the §10 account-link flow: a player who is already online
(so Mojang has verified their UUID) runs `/link`; the plugin asks felis-api to
mint a one-time code for that UUID and shows it in chat. The player then enters
the code on the web console → **Account** page (the second leg), which binds the
code to their logged-in account. The web side is already built.

The **limbo** module reaches the same felis-api endpoint without a command: it is the login
gate, so it mints the code on join for anyone not yet linked and holds them until they redeem
it. The **paper** lobby ships neither — see below.

The **Velocity** module additionally carries the §11 domain-autostart routing
loop — recognizing each server's subdomain, registering backends dynamically,
waking a sleeping target and holding the player until it is ready. It is a full
proxy plugin, not just `/link`; see **[Velocity routing](#velocity-routing-§11)**
below. The Fabric / Forge / NeoForge mods are `/link`-only.

The **Paper** module is different in kind: it is the §12 lobby UI face. It ships
**no** `/link` and holds **no** felis-api token — it only paints the `/menu`
(and `/server`) chest GUI and speaks the `felis:control` plugin-message channel
to Velocity, which is the only side that ever talks to felis-api. See
**[Lobby menu](#lobby-menu-§12)** below.

| Module             | Platform                    | Target                              | Jar                          | Built by the installer |
| ------------------ | --------------------------- | ----------------------------------- | ---------------------------- | ---------------------- |
| `velocity/`        | Velocity proxy plugin       | velocity-api 3.3.0-SNAPSHOT         | `felis-velocity-0.1.0.jar`   | yes                    |
| `limbo/`           | LOOHP/Limbo plugin (login)  | Limbo API / Java 17 bytecode        | `felis-limbo-0.1.0.jar`      | yes                    |
| `paper/`           | Paper server plugin (lobby) | paper-api 1.21.4-R0.1-SNAPSHOT      | `felis-paper-0.1.0.jar`      | yes                    |
| `fabric/`          | Fabric server mod           | MC 1.20.1 / fabric-loader 0.16.x    | `felis-fabric-0.1.0.jar`     | no                     |
| `forge/`           | Forge server mod            | MC 1.20.1 / Forge 47.3.0            | `felis-forge-0.1.0.jar`      | no                     |
| `neoforge/`        | NeoForge server mod         | MC 1.20.4 / NeoForge 20.4.251       | `felis-neoforge-0.1.0.jar`   | no                     |
| `shared/`          | *(not built on its own)*    | —                                   | source compiled into each    | source only            |

"Built by the installer" is what `deploy/bootstrap.sh` produces, and it is the same set
`bootstrap_asset.go` embeds into the felis binary for the TUI install path, which has no source
checkout to build from. **The three loader mods are not in that set** — a finished install has
no `felis-fabric`/`felis-forge`/`felis-neoforge` jar anywhere. They build from this checkout with
the commands under [Building](#building) and are deployed by hand; the account-link flow they
carry works, but nothing installs them for you.

## Architecture

Each platform is an **independent** Gradle build with its own `settings.gradle`,
not one root project mixing loader plugins (the loader Gradle plugins have
conflicting Gradle-version requirements — see below). The platform-neutral link
core lives in `shared/src/main/java` and is pulled into every module via:

```groovy
sourceSets { main { java { srcDir '../shared/src/main/java' } } }
```

The core (`best.lolicon.felis.link`) has **zero third-party dependencies** — it
uses the JDK's `java.net.http.HttpClient` and a small hand-written JSON parser —
so there is nothing to shade and each jar is self-contained.

- `LinkClient` — `POST {apiBaseUrl}/api/v1/internal/account/link/code` with
  `Authorization: Bearer <service-token>` and body `{"mc_uuid":"<uuid>"}`;
  `201 → {code, expires_at}`, otherwise the `{error:{code,message}}` envelope.
- `LinkConfigLoader` — reads `FELIS_API_BASE_URL` / `FELIS_SERVICE_TOKEN` (env
  wins) or a `felis-link.properties` file written as a commented template on
  first run. **The API URL and service token are deployment inputs and are never
  compiled in.**

Threading: the command runs on the server thread; the HTTP call is dispatched to
a daemon single-thread executor and the reply is hopped back onto the server
thread, so a slow felis-api never stalls the tick loop. If config is missing the
plugin loads but never registers `/link`, so the server runs un-crippled.

All three mods use **official Mojang mappings**, so the MC class/method names are
identical across Fabric/Forge/NeoForge and the command handler is uniform; only
the `@Mod`/event-bus/config-dir glue differs per loader.

## Velocity routing (§11)

Velocity sits on the player-facing edge, off-cluster, so it is where
domain-autostart routing lives. Beyond `/link`, the Velocity plugin recognizes
each felis server by its subdomain, registers backends into Velocity's dynamic
server registry, and decides — per join — whether to send the player straight in,
wake a sleeping server and park them, or ask them to reconnect. It drives §9 wake
and §11 routing over the felis-api **internal** face (service-token auth), and
additionally terminates the `felis:control` plugin-message channel that backs the
§12 lobby menu — translating each lobby frame into the same wake/claim/status
calls, against the player's connection-derived identity rather than anything the
lobby claims. See **[Lobby menu](#lobby-menu-§12)** below.

Two preconditions gate routing, **each fails safe** (routing turns off, `/link`
keeps working):

- **online mode** — `online-mode=true` in `velocity.toml`. The autostartPolicy
  and allowlist gates trust Mojang-verified UUIDs; under offline mode the plugin
  refuses to route on spoofable identities and logs an error.
- **root-domain** — the deployment zone (e.g. `mc.example.net`). This is the only
  place the zone enters the proxy and is **never compiled in**; without it,
  host-based routing has nothing to match and stays off.

What it does when routing is active:

| Surface | Behavior |
| ------- | -------- |
| Backend registry | Polls `GET /api/v1/servers` every 15 s and reconciles Velocity's dynamic registry. A failed poll **keeps existing registrations** — a control-plane blip never deregisters live backends. The API advertises each backend Service's host-routable ClusterIP, avoiding cluster-DNS names on the host-run proxy. |
| Join (`PlayerChooseInitialServerEvent`) | Resolves `subdomain.<root-domain>` and remembers the target, but every fresh connection still enters `login`. When the login gate requests its post-auth lobby transfer, Velocity re-checks link status: a ready remembered target is selected immediately; an asleep target is woken and queued from the lobby. |
| Waiting queue | One scheduled drain every 2 s polls status once per distinct waited-on server; a waiter drops out on transfer, on the player leaving, or after a 120 s timeout. |
| Wake gate | The wake is `POST /api/v1/internal/servers/{name}/wake` keyed on the player's online-mode UUID. **403** (policy refused) tells the player and stops; **429** (wake already in flight) keeps waiting. |
| Server-list ping (`ProxyPingEvent`) | Answers from the cached lifecycle view with a phase-aware MOTD (online / starting / sleeping) — **read-only, never wakes** anything. Mirroring each backend's own MOTD by background-pinging ready servers is a later slice. |
| Join report (`ServerConnectedEvent`) | Reports real joins to a felis backend via `POST …/join-event`, so the reaper sees activity and the player is auto-added to the server allowlist. |
| `/felis`, `/felis list` | Operator status: online-mode, root-domain, lobby, and the known server set with phase/ready. |

Velocity-only config keys (read from the same `felis-link.properties` / env as
`/link`; env wins):

| Key | Env | Meaning |
| --- | --- | ------- |
| `root-domain`  | `FELIS_ROOT_DOMAIN`  | Routing zone, e.g. `mc.example.net`. Unset → routing off. |
| `login-server` | `FELIS_LOGIN_SERVER` | The system auth gate every fresh connection must pass. Defaults to `login`. |
| `lobby-server` | `FELIS_LOBBY_SERVER` | The distinct post-auth holding server used while a backend wakes. Defaults to `lobby`; it must not equal `login-server`. |

## Lobby menu (§12)

The `paper/` module is the lobby's player-facing face for §27 scenario 10
(`/menu → plugin msg → velocity → api → 共用等待队列 → ready 后 Connect`). It runs
on the Paper lobby server and gives players a chest GUI instead of a command
line: `/menu` (alias `/server`) opens a grid of one tile per configured server,
and clicking a tile wakes, claims, or joins that backend.

**Pure UI face.** The lobby holds no felis-api token, opens no HTTP connection,
and keeps no waiting queue. Every action it takes is a single frame on the
`felis:control` plugin-message channel; every piece of state it shows arrives as
a frame on the same channel. Velocity (the `ControlChannel`, above) is the only
side that talks to felis-api. This is enforced **physically** by the build, not
just by convention: the module's `sourceSets` include-filter compiles in only the
paper package plus the three codec classes, so the lobby jar contains exactly
five classes —

```
best/lolicon/felis/link/Control.class        (channel framing)
best/lolicon/felis/link/ControlFrame.class   (the frame model)
best/lolicon/felis/link/Json.class           (codec)
best/lolicon/felis/paper/FelisPaperPlugin.class
best/lolicon/felis/paper/MenuHolder.class
```

— and **no** `FelisApiClient`, `LinkClient`, or token-config class. If a codec
class ever grew a dependency on the API client, compilation would fail here
rather than silently widen the lobby's reach.

**Frames.** Upstream (lobby → velocity) carries `WakeRequest`, `ClaimRequest`,
and `StatusQuery`; downstream (velocity → lobby) carries `StatusUpdate`,
`TransferReady`, and `Error`. Opening the menu paints a grey "loading" tile per
server and fires a `StatusQuery` for each; the proxy answers with `StatusUpdate`
frames that repaint each tile by phase + ownership.

**Anti-spoof (§14).** The `player` field a lobby puts in a frame is **not**
trusted. Velocity derives the acting player and UUID from the `ServerConnection`
the plugin message arrived on, and the server-side autostartPolicy / ownership
gates authorize against that verified identity. The frame's `server` field is the
trusted payload — it only names *which* tile was clicked. A fully compromised
lobby therefore cannot act as another player or reach the API directly.

**Button rules** (the tile a click sends depends on the last `StatusUpdate`):

| Tile state | Label | Frame sent |
| ---------- | ----- | ---------- |
| ownerless + stopped (`claimable`) | **Claim & Start** | `ClaimRequest{server}` |
| owned + running (`ready`)         | **Join**          | `WakeRequest{server}` |
| owned + stopped                   | **Wake**          | `WakeRequest{server}` |

"Join" and "Wake" are the **same** upstream frame (`WakeRequest`) — only the
label differs; the proxy treats a wake of an already-running owned server as a
join. A refusal comes back as an `Error` frame (`not_linked` / `quota_exceeded` /
`already_claimed` → a friendly message), which is the only place a claim/quota/
policy failure surfaces to the player; readiness arrives as `TransferReady` just
before the proxy Connects them.

> **Status.** This slice is **code-complete and compile-verified** (paper jar
> builds green on a Java-21 toolchain; the velocity end compiles the full shared
> tree; the wire codec round-trips; the fabric/forge/neoforge mods compile through
> their vendored wrappers and boot real dedicated servers with `/link` registered —
> all of it gated by CI). It is **not** client-verified:
> no real game client has joined through the stack, so §27 scenario 10 stays
> **FAIL (live-unverified)** until such a join is exercised. The client-independent
> faces (proxy edge, subdomain MOTD, login boundary, backend registration) are
> exercised on a live deployment — see `AUDIT-2026-09-22.md`.

## Building

The platforms need different Gradle versions (a real, measured constraint, not a
preference):

| Module      | Gradle      | Why                                                              |
| ----------- | ----------- | --------------------------------------------------------------- |
| `velocity`  | 9.5.1 (system) | plain `java` plugin — no loader Gradle plugin                |
| `limbo`     | 9.5.1 (system), **JDK 21 toolchain** | plain `java` plugin; current LOOHP/Limbo releases ship class-file major 65, so the compiler JDK must be ≥ 21 to read them. It emits `release 17` bytecode, so the jar still loads on any Limbo running Java 17+ |
| `fabric`    | 8.8 (wrapper)  | loom 1.7.4 uses `Problems.forNamespace`, removed in Gradle 9 |
| `forge`     | 8.8 (wrapper)  | ForgeGradle 6 is Gradle-8-only                               |
| `neoforge`  | 8.14 (wrapper) | NeoGradle 7.1.38 requires Gradle API ≥ 8.14                  |
| `paper`     | 9.5.1 (system), **JDK 21 toolchain** | plain `java` plugin, but paper-api 1.21.4 is published for Java 21, so it declares a `JavaLanguageVersion.of(21)` toolchain — Gradle picks a detected JDK 21 to compile regardless of which JDK runs Gradle |

```bash
# Velocity — system Gradle is fine
gradle -p plugins/velocity build

# Paper and limbo — system Gradle too, but both compile on a Java-21 toolchain (see table).
# limbo also needs the LOOHP/Limbo API release it compiles against: the module's `+`
# default cannot resolve (LOOHP's repository publishes no maven-metadata), so pass the
# release that matches the Limbo.jar you bundle, exactly as deploy/bootstrap.sh does:
gradle -p plugins/paper build
gradle -p plugins/limbo build -PlimboVersion=<release, e.g. 2026.0.3-ALPHA>

# Fabric / Forge / NeoForge — use the per-module wrapper. Nothing installs these; the jar you
# want is the one this produces.
plugins/fabric/gradlew   -p plugins/fabric   build
plugins/forge/gradlew    -p plugins/forge    build
plugins/neoforge/gradlew -p plugins/neoforge build
```

Requires JDK 17 — **except `paper` and `limbo`, which need a Java-21 toolchain available to
Gradle** (paper-api 1.21.4 is a Java-21 artifact and the Limbo API is compiled to major 65; the
rest of the suite is Java 17). The first build of each mod downloads and remaps/decompiles Minecraft, so it
takes a few minutes; subsequent builds are fast. Jars land in each module's
`build/libs`. CI runs both gates: `bash plugins/test.sh` (JDK 21 — the install-time
plugins plus the codec/invite tests) and `bash plugins/test-mods.sh` (JDK 17 — the
three loader mods, via the wrappers above).

## Deploying

Drop the matching jar into the server/proxy mods or plugins directory, start
once to generate `config/felis-link.properties` (or `plugins/felis-link/…` on
Velocity), then set `api-base-url` and `service-token` — or provide
`FELIS_API_BASE_URL` and `FELIS_SERVICE_TOKEN` in the environment, which take
precedence. The service token is the same one felis-api compares for its
internal endpoints; treat it as a secret.

On **Velocity**, also set `root-domain` (and optionally `lobby-server`) in the
same file to turn on §11 routing, and make sure `online-mode=true` in
`velocity.toml` — without either, the proxy still serves `/link` but routing
stays off (see **[Velocity routing](#velocity-routing-§11)**). The config dir is
`plugins/felis-link/` because the plugin id is `felis-link` (kept stable across
the 0.1 → 0.2 jar so existing config carries over).

On the **Paper lobby** there is no token to set, because the lobby never talks to
felis-api. Drop `felis-paper-…jar` into `plugins/`, start once to generate
`plugins/FelisPaper/config.yml`, and list the felis server names (the CRD
`metadata.name`, not the display title) you want as tiles under `servers:`. The
lobby must sit behind the same Velocity proxy as the backends — it reaches the
control plane only through the proxy's `felis:control` terminus — so it needs no
`api-base-url` and no `service-token` of its own.
