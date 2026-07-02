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
  -t felis-lobby:demo .
docker save felis-lobby:demo | sudo k3s ctr images import -
# felis.toml → [velocity] lobby_image = "felis-lobby:demo"
sudo felis setup
```

## Configure (deployer's responsibility)

- Game port must be `25565` (the CRD `GamePort`).
- Align `online-mode` / player forwarding with the off-cluster Velocity proxy.
- The lobby speaks only the `felis:control` plugin-message channel; it holds no
  felis-api token by design (spec §12).
