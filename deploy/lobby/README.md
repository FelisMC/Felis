# Lobby image (Paper + felis-paper)

The always-on **lobby** hub. `felis setup` provisions it as a system service
(`DesiredState=Running`, reaper-exempt) when `[velocity] lobby_image` is set in
`felis.toml`.

> **Code-only.** Not built by the Go CI. It compiles the `plugins/paper`
> `/menu` face and bundles it onto a Paper server.

## Topology & the one invariant

```
connect → login (limbo auth gate) → lobby (/menu hub) → target backend
```

The lobby is reached **only** when the login gate transfers an authenticated
player onward. It is never a fallback or waiting-park target — routing a fresh
connection to the lobby would drop the player past authentication. Felis enforces
this at every layer:

- the login system service has **no** fallback (refuse if down);
- the lobby and every user server fall back to **login**, never to the lobby;
- `buildSystemServer` refuses to construct any service whose fallback is `lobby`;
- `felis setup` prints the off-cluster Velocity wiring: default landing and
  waiting-park both point at `login`.

## Build

```
docker build -f deploy/lobby/Dockerfile \
  --build-arg PAPER_JAR_URL=https://<mirror>/paper-26.3-<build>.jar \
  --build-arg PAPER_JAR_SHA256=<sha256 of that jar> \
  -t felis-lobby:demo .
# Publish into the cluster's registry (on the node; docker treats 127.0.0.1 as
# insecure by default — or through a `kubectl -n felis port-forward svc/registry
# 5000:5000`, which is equivalent: only the path after the host matters).
docker tag  felis-lobby:demo 127.0.0.1:5000/felis/lobby:demo
docker push 127.0.0.1:5000/felis/lobby:demo
# felis.toml → [velocity] lobby_image = "registry.felis.svc:5000/felis/lobby:demo"
sudo felis setup
```

## Customize in the panel

Administrators open **Login & lobby** (`/admin/lobby`). Stop the selected space
before reading or saving its settings, then start it to apply them. The lobby
form configures welcome text, menu titles, join behavior, game mode, building
protection, damage/hunger/void handling, difficulty, time/weather and world rules.
Settings live in `/data/felis-experience.json`, independently of the image, and
retain unknown keys when saved. Existing installations without this file use the
same protected-lobby defaults as before.

The page also exposes the existing file manager (including upload and ZIP
extraction), console, backups/restore, builder permissions and image/resource
settings. To replace a map: back up and stop the lobby, upload a world ZIP,
extract it at the volume root, verify the world directory directly contains
`level.dat`, and set `level-name` in `server.properties`. Use `setworldspawn x y z`
in the running lobby console to set its spawn. Plugin JARs go in `plugins/` and
must match Paper's version; the bundled Felis and LuckPerms JARs are refreshed
from the image at boot. A custom image must retain the menu/control plugin.

The operator reuses its `init-forwarding` YAML merge for the lobby, preserving
custom Paper globals while refreshing mandatory authentication settings. The
image only rewrites that file for standalone runs without a managed forwarding
initContainer. RCON secrets and the proxy forwarding secret remain managed.

## Configure (deployer's responsibility)

- Game port must be `25565` (the CRD `GamePort`).
- Align `online-mode` / player forwarding with the off-cluster Velocity proxy.
- The lobby speaks only the `felis:control` plugin-message channel; it holds no
  felis-api token by design (spec §12).

## Default lobby behavior

felis-paper's `LobbyGuard` keeps the lobby a hub that nobody can hurt, get hurt in,
or leave a mark on:

- every world is peaceful, with natural spawning, PvP, mob griefing and TNT off,
  time frozen at noon, clear weather and inventories kept;
- players take no damage and never go hungry; a fall into the void lands at spawn;
- a player without `felis.lobby.build` joins at spawn in adventure mode and cannot
  break or place blocks, use buckets, trample farmland, light fires, or harm mobs,
  item frames, paintings, armor stands or vehicles. Buttons, doors, pressure plates
  and containers keep working;
- every join gets a chat line with a click that runs `/menu`.

`felis.lobby.build` defaults to ops. To let an admin build the lobby, grant it with
LuckPerms (`lp user <name> permission set felis.lobby.build true` on the lobby console)
or op them.

The entrypoint seeds `max-players=200` when absent, over Paper's default of 20;
subsequent file-editor changes survive restarts: every
authenticated player passes through here, and a stopped server's players arrive all at
once.
