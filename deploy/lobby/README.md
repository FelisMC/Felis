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
  --build-arg PAPER_JAR_URL=https://<mirror>/paper-1.21.x-<build>.jar \
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

## Configure (deployer's responsibility)

- Game port must be `25565` (the CRD `GamePort`).
- Align `online-mode` / player forwarding with the off-cluster Velocity proxy.
- The lobby speaks only the `felis:control` plugin-message channel; it holds no
  felis-api token by design (spec §12).

## What the lobby allows

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

The entrypoint pins `max-players=200` on every boot, over Paper's default of 20: every
authenticated player passes through here, and a stopped server's players arrive all at
once.
