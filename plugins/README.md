# Felis server-side plugins

These are the in-cluster and edge plugins for Felis. The Velocity proxy and the three loader
mods (these on a standalone online-mode server only, see the warning under the module table)
ship the in-game first leg of the §10 account-link flow: a player who is already online
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

| Module             | Platform                    | Compiles against                         | Jar                          | Built by the installer |
| ------------------ | --------------------------- | ---------------------------------------- | ---------------------------- | ---------------------- |
| `velocity/`        | Velocity proxy plugin       | velocity-api 3.5.1 (Java 21 bytecode)    | `felis-velocity-0.1.0.jar`   | yes                    |
| `limbo/`           | LOOHP/Limbo plugin (login)  | Limbo API 2026.0.3-ALPHA (Java 17 bytecode) | `felis-limbo-0.1.0.jar`   | yes                    |
| `paper/`           | Paper server plugin (lobby) | paper-api 26.3.build.40-alpha            | `felis-paper-0.1.0.jar`      | yes                    |
| `fabric/`          | Fabric server mod           | MC 1.20.1 / fabric-loader 0.16.5 / fabric-api 0.92.2+1.20.1 | `felis-fabric-0.1.0.jar` | no          |
| `forge/`           | Forge server mod            | MC 1.20.1 / Forge 47.3.0                 | `felis-forge-0.1.0.jar`      | no                     |
| `neoforge/`        | NeoForge server mod         | MC 1.20.4 / NeoForge 20.4.251            | `felis-neoforge-0.1.0.jar`   | no                     |
| `shared/`          | *(not built on its own)*    | —                                        | source compiled into each    | source only            |

The installer's three versions are the builds `deploy/game-stack.lock` installs (Velocity
`VELOCITY_VERSION`, Limbo `LIMBO_VERSION`, the Paper jar in `PAPER_JAR_URL`), and
`go test .` fails when a pin and the lock drift apart; see
[Dependency verification](#dependency-verification).

### Minecraft versions

| Module     | Runs on                                   | Minecraft                                        |
| ---------- | ----------------------------------------- | ------------------------------------------------ |
| `velocity` | the Felis proxy (Velocity 3.5.1)          | whatever clients the proxy accepts: 26.3 natively, older clients through the ViaVersion stack bootstrap installs |
| `limbo`    | the Felis login gate (Limbo)              | 26.3 only — Limbo speaks exactly one protocol, the lock's `MC_VERSION` |
| `paper`    | the Felis lobby (Paper 26.3)              | 26.3, the lock's `MC_VERSION`                    |
| `fabric`   | a standalone Fabric server                | 1.20.1 (`fabric.mod.json` declares `~1.20.1`)    |
| `forge`    | a standalone Forge server                 | 1.20.1 (`mods.toml` declares `[1.20.1,1.20.2)`)  |
| `neoforge` | a standalone NeoForge server              | 1.20.4 (`mods.toml` declares `[1.20.4,1.20.5)`)  |

The Felis network itself runs Minecraft 26.3. The loader mods target the older 1.20.x
modding lines and belong on a standalone server outside the network; they load on no
26.x server, and nothing in a Felis install loads them.

"Built by the installer" is what `deploy/bootstrap.sh` produces, and it is the same set
`bootstrap_asset.go` embeds into the felis binary for the TUI install path, which has no source
checkout to build from. **The three loader mods are not in that set** — a finished install has
no `felis-fabric`/`felis-forge`/`felis-neoforge` jar anywhere. They build from this checkout with
the commands under [Building](#building) and are deployed by hand; the account-link flow they
carry works, but nothing installs them for you.

> **The loader mods are for a standalone server only.** Inside a Felis network the proxy's
> `/link` already serves every backend and shadows a backend's own, so user game servers need
> no mod. A mod answers `/link` only when its server runs `online-mode=true`: an offline-mode
> server is either behind a proxy (whose `/link` applies) or cracked, where the UUID is
> whatever the client claims.
>
> **The token a mod holds is a real credential.** It mints a link code for any UUID the mod
> asks about, so whoever can read that server's files — its operator, any plugin or mod on
> it, a copied backup — can bind a not-yet-linked player's Minecraft account to their own web
> account. Put a mod only on a server whose operator you would trust with that, and give it
> the `limbo` token: it opens the link-code, link-status and blacklist routes and nothing
> else. The `velocity` token also approves op-logins and wakes or claims servers for any
> player; it stays on the proxy host. `sudo felis rotate-token limbo` replaces a leaked
> token (the login gate restarts onto the new value; copy it to the mod by hand).

## Whose identity each path trusts

Only the **Velocity path** is protected against a forged identity. The proxy runs
`online-mode=true` (bootstrap writes it), so Velocity checks every login with Mojang, and
everything downstream takes its identity from that login:

- the proxy's own `/link`, `/felis` and `/invite` use the UUID of the verified connection;
- the lobby's `felis:control` frames are attributed to the backend connection they arrived
  on, and the `player` field a lobby writes is ignored (see
  [Lobby menu](#lobby-menu-§12));
- the login gate and the lobby run offline-mode behind the proxy and accept only logins
  carrying Velocity's modern-forwarding signature (the shared forwarding secret), so a
  client that bypasses the proxy cannot claim a UUID.

A proxy started with `online-mode=false` refuses to route (it logs an error and turns
routing off); its `/link` then sees only name-derived offline UUIDs, which never equal a
Mojang account's.

The **loader mods** are outside that protection. A mod trusts the UUID its own server
reports (`getUUID()`), so it is exactly as trustworthy as that server: the mod refuses
`/link` unless the server runs `online-mode=true`, and whoever operates the server holds
a token that can mint a code for any UUID (see the warning above).

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
  compiled in.** Two optional keys bound each call, in whole seconds from 1 to 120
  (default 10): `connect-timeout-seconds` / `FELIS_API_CONNECT_TIMEOUT_SECONDS`
  for opening the connection, `request-timeout-seconds` /
  `FELIS_API_REQUEST_TIMEOUT_SECONDS` for the whole call. Anything else fails the
  load with the key named.
- `FelisApiClient` — the routing client. A GET that failed on a dropped
  connection or a 502/503/504 is tried once more after a 100–400 ms jittered
  pause; a GET that timed out, and every POST (wake, claim, join-event,
  approvals), is never repeated.

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
| Waiting queue | One scheduled drain every 2 s polls status once per distinct waited-on server. A waiter stays while its server is on the way up (starting, or Failed inside the operator's restart backoff), hears a progress line each minute, and drops out on transfer, on the player leaving, when the start is given up (`startGaveUp`) or the server is stopped, after 120 s without an answer from felis-api, or at a one-hour backstop. |
| Wake gate | The wake is `POST /api/v1/internal/servers/{name}/wake` keyed on the player's online-mode UUID. **403** (policy refused) tells the player and stops; **429** (wake already in flight) keeps waiting. |
| Server-list ping (`ProxyPingEvent`) | Answers from the cached lifecycle view with a phase-aware MOTD (online / starting / sleeping) — **read-only, never wakes** anything. Mirroring each backend's own MOTD by background-pinging ready servers is a later slice. |
| Join report (`ServerConnectedEvent`) | Reports real joins to a felis backend via `POST …/join-event`, so the reaper sees activity and the player is auto-added to the server allowlist. |
| `/felis`, `/felis list` | Operator status: online-mode, root-domain, lobby, and the known server set with phase/ready. |
| `/felis lobby`, `/felis go <lobby>` | Moves the player back to the lobby from any backend. Nothing is woken, and a wait already queued still moves them when its server is ready. Kept under `/felis` so a user server's own `/lobby` or `/hub` is not shadowed by the proxy. |

Velocity-only config keys (read from the same `felis-link.properties` / env as
`/link`; env wins):

| Key | Env | Meaning |
| --- | --- | ------- |
| `root-domain`  | `FELIS_ROOT_DOMAIN`  | Routing zone, e.g. `mc.example.net`. Unset → routing off. |
| `login-server` | `FELIS_LOGIN_SERVER` | The system auth gate every fresh connection must pass. Defaults to `login`. |
| `lobby-server` | `FELIS_LOBBY_SERVER` | The distinct post-auth holding server used while a backend wakes. Defaults to `lobby`; it must not equal `login-server`. |

Load bounds on the proxy: felis-api calls run on a pool of 8 threads with 64
waiting slots. Past that a call is refused at once — the player reads "busy", a
lobby frame gets a `busy` error, and a dropped join-event is logged at warn —
rather than piling up threads while felis-api is slow. The registration refresh
(15 s) and the waiting-queue poll (2 s) skip a run that falls due while the last
one is still going. Acting commands (`/link`, `/felis claim`, migrate, op
approve) share a per-player budget of 5 then one per 5 s; felis:control frames
from the lobby are metered per player and menu status answers are cached.

Health on the proxy: every 10 minutes that saw any activity the proxy logs one
info line, `Felis: last 10 min: felis-api calls=… (no answer=…, 4xx=…, 5xx=…,
retried=…), avg=… ms, max=… ms, busy refusals=…, join-events failed=…,
join-events dropped=…, transfers failed=…, server-list refreshes failed=…,
waiting now=…`, with only that window's counts. `/felis` run from the console
adds the same felis-api counts since start, the waiting count and the failure
totals. A server-list refresh that keeps failing warns once when it starts, then
every 5 minutes with the running count, and logs at info when it recovers; the
login gate treats its link-status polls the same way.

## Lobby menu (§12)

The `paper/` module is the lobby's player-facing face for §27 scenario 10
(`/menu → plugin msg → velocity → api → 共用等待队列 → ready 后 Connect`). It runs
on the Paper lobby server and gives players a chest GUI instead of a command
line: `/menu` (alias `/server`) opens a grid of one tile per server the proxy routes,
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

**Frames.** Upstream (lobby → velocity) carries `ListRequest`, `WakeRequest`,
`ClaimRequest` and `StatusQuery`; downstream (velocity → lobby) carries `ListUpdate`,
`StatusUpdate`, `TransferReady` and `Error`. `/menu` sends a `ListRequest`, and the
proxy answers with a `ListUpdate` naming every user server it routes, built from the
registry it routes by, so a server created in the panel appears without anyone editing
the lobby. The menu then paints a grey "loading" tile per server (45 per page, arrows
in the bottom row) and fires a `StatusQuery` for each; the proxy answers with
`StatusUpdate` frames that repaint each tile by phase + ownership.

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
> builds green on a Java-25 toolchain; the velocity end compiles the full shared
> tree; the wire codec round-trips; the fabric/forge/neoforge mods compile through
> their vendored wrappers and boot real dedicated servers with `/link` registered —
> all of it gated by CI). It is **not** client-verified:
> no real game client has joined through the stack, so §27 scenario 10 stays
> **FAIL (live-unverified)** until such a join is exercised. The client-independent
> faces (proxy edge, subdomain MOTD, login boundary, backend registration) are
> exercised on a live deployment — see `AUDIT-2026-09-22.md`.

## Building

Every module builds through its own vendored Gradle wrapper, and each wrapper pins its
distribution's sha256 (`distributionSha256Sum`), so a tampered or swapped Gradle download
fails before it runs. The platforms need different Gradle versions (a real, measured
constraint):

| Module      | Gradle | JDK | Why                                                          |
| ----------- | ------ | --- | ------------------------------------------------------------ |
| `velocity`  | 9.8.0  | ≥ 21 runs it, emits Java 21 | plain `java` plugin; velocity-api 3.5.1 declares `jvm.version = 21` |
| `paper`     | 9.8.0  | **25 toolchain** | paper-api 26.3 is published as a Java-25 artifact, so the module declares a `JavaLanguageVersion.of(25)` toolchain |
| `limbo`     | 9.8.0  | ≥ 21 runs it, emits Java 17 | current LOOHP/Limbo releases ship class-file major 65, so the compiler JDK must be ≥ 21 to read them; `release 17` bytecode loads on any Limbo running Java 17+ |
| `fabric`    | 8.8    | 17 | loom 1.7.4 uses `Problems.forNamespace`, removed in Gradle 9 |
| `forge`     | 8.8    | 17 | ForgeGradle 6 is Gradle-8-only                               |
| `neoforge`  | 8.14   | 17 | NeoGradle 7.1.38 requires Gradle API ≥ 8.14                  |

The three plugins the installer bakes in (`velocity`, `paper`, `limbo`) are built with
Gradle 9.8.0 everywhere: through the wrapper locally and in CI, and inside the image
`gradle:9.8.0-jdk25@sha256:…` in the lobby and limbo Dockerfiles and bootstrap's
Velocity build. `bootstrap_asset_test.go` fails when the image, its digest or the
wrapper version drift apart.

```bash
# Velocity and Paper
plugins/velocity/gradlew -p plugins/velocity build
plugins/paper/gradlew    -p plugins/paper    build

# limbo compiles against the LOOHP/Limbo API release the login gate bundles, which has
# to be named: pass deploy/game-stack.lock's LIMBO_VERSION, exactly as bootstrap does.
plugins/limbo/gradlew -p plugins/limbo build -PlimboVersion="$(sed -n 's/^LIMBO_VERSION=//p' deploy/game-stack.lock)"

# Fabric / Forge / NeoForge. Nothing installs these; the jar you want is the one this
# produces.
plugins/fabric/gradlew   -p plugins/fabric   build
plugins/forge/gradlew    -p plugins/forge    build
plugins/neoforge/gradlew -p plugins/neoforge build
```

The first build of each mod downloads and remaps/decompiles Minecraft, so it takes a few
minutes; subsequent builds are fast. Jars land in each module's `build/libs`. CI runs
both gates: `bash plugins/test.sh` (JDK 25 — the install-time plugins, the
codec/invite/server-list tests, and the proxy routing tests that drive ServerRegistry,
WaitingRouter and ControlChannel against the real velocity-api; run those alone with
`plugins/velocity/gradlew -p plugins/velocity routingTest`) and `bash plugins/test-mods.sh` (JDK 17 — the three
loader mods, via the wrappers above).

### Dependency verification

Every dependency version is exact: paper-api is the API of the Paper build
`deploy/game-stack.lock` installs (`paper-26.3-40.jar` → `26.3.build.40-alpha`), limbo
compiles against the lock's `LIMBO_VERSION`, velocity-api is the lock's
`VELOCITY_VERSION`, and ForgeGradle is `6.0.54`. The installer's three modules also
carry `gradle/verification-metadata.xml`, the sha256 of every artifact their build
resolves, and Gradle refuses any artifact whose bytes differ. The Limbo API entry is
the very jar the login gate runs (its sha256 equals the lock's `LIMBO_JAR_SHA256`).

After `deploy/update-game-stack-lock.sh` moves Paper, Limbo or Velocity, bring the pins
along and regenerate the checksums, then review the diff:

```bash
# 1. set paper-api in plugins/paper/build.gradle to the new build (paper-<mc>-<n>.jar → <mc>.build.<n>-<channel>)
# 2. regenerate the three verification files (JDK 25) from an EMPTY Gradle home: with a
#    warm cache Gradle skips the BOMs and parent POMs it already holds, and the image
#    builds, which start empty, then refuse them
rm -f plugins/{velocity,paper,limbo}/gradle/verification-metadata.xml
export GRADLE_USER_HOME="$(mktemp -d)"
plugins/velocity/gradlew -p plugins/velocity --write-verification-metadata sha256 build
plugins/paper/gradlew    -p plugins/paper    --write-verification-metadata sha256 build
plugins/limbo/gradlew    -p plugins/limbo    --write-verification-metadata sha256 build \
  -PlimboVersion="$(sed -n 's/^LIMBO_VERSION=//p' deploy/game-stack.lock)"
unset GRADLE_USER_HOME
# 3. go test . fails until the pins, the checksums and the lock agree
```

The loader mods pin exact plugin and dependency versions and their wrappers' sha256,
but carry no verification file: loom, ForgeGradle and NeoGradle fetch and remap
Minecraft through their own downloaders (checked against Mojang's manifest hashes), and
nothing installs these jars.

## Deploying

Drop the matching jar into the server/proxy mods or plugins directory, start
once to generate `config/felis-link.properties` (or `plugins/felis-link/…` on
Velocity), then set `api-base-url` and `service-token` — or provide
`FELIS_API_BASE_URL` and `FELIS_SERVICE_TOKEN` in the environment, which take
precedence. The token is one of felis-api's per-caller internal tokens: the
Velocity proxy uses the `velocity` token (Secret `felis/felis-service-token`), the
login gate the `limbo` token (`felis-limbo-token`), and each serves only its own
routes (a token on another caller's route gets `403 wrong_caller`). A loader mod
takes the `limbo` token, on a standalone online-mode server only (see the warning
under the module table). Treat it as a secret; `sudo felis rotate-token <caller>`
replaces it.

On **Velocity**, also set `root-domain` (and optionally `lobby-server`) in the
same file to turn on §11 routing, and make sure `online-mode=true` in
`velocity.toml` — without either, the proxy still serves `/link` but routing
stays off (see **[Velocity routing](#velocity-routing-§11)**). The config dir is
`plugins/felis-link/` because the plugin id is `felis-link` (kept stable across
the 0.1 → 0.2 jar so existing config carries over).

On the **Paper lobby** there is no token to set, because the lobby never talks to
felis-api, and no server list to keep: the menu shows the servers the proxy routes,
which the proxy sends over `felis:control` (a `servers:` list left in
`plugins/FelisPaper/config.yml` by an older version is ignored, and the plugin says so
at startup). The lobby must sit behind the same Velocity proxy as the backends — it
reaches the control plane only through the proxy's `felis:control` terminus — so it
needs no `api-base-url` and no `service-token` of its own. The installer builds and
bakes this jar into the lobby image; installing it by hand is for a lobby you run
yourself.
