#!/bin/bash
# demo-up.sh — one-shot Felis demo bring-up.
#
# Collapses the four manual steps (bootstrap -> build/import limbo+lobby images ->
# edit felis.toml -> felis setup) into a single command:
#
#     sudo bash deploy/demo-up.sh
#
# It ends by exec'ing the interactive `felis setup` TUI (create the Owner account) —
# that human step is the only thing this script cannot do for you.
#
# Image source, in order of preference:
#   1. Prebuilt tars at deploy/images/felis-limbo.tar + felis-lobby.tar (imported as-is).
#   2. Otherwise built on this host with docker, resolving the LOOHP/Limbo CI jar and
#      the latest stable Paper jar automatically. Override any of:
#        LIMBO_JAR_URL LIMBO_SCHEM_URL LIMBO_VERSION PAPER_JAR_URL PAPER_MC_VERSION
#
# Toggles: SKIP_BOOTSTRAP=1 (base already up), SKIP_SETUP=1 (stop before the TUI).
set -Eeuo pipefail

SRC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
STATE_DIR=/etc/felis
HOST_TOML="$STATE_DIR/felis.host.toml"
IMG_DIR="$SRC_DIR/deploy/images"
LIMBO_IMAGE="felis-limbo:demo"
LOBBY_IMAGE="felis-lobby:demo"
K3S=/usr/local/bin/k3s
FELIS=/usr/local/bin/felis

log() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }
die() { printf '\033[1;31mERROR: %s\033[0m\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "run as root (sudo bash deploy/demo-up.sh)"

# 1. base platform (k3s + felis + docker + cloudflared + control plane) ----------
if [ "${SKIP_BOOTSTRAP:-0}" = 1 ]; then
  log "SKIP_BOOTSTRAP=1 — assuming the base platform is already up"
else
  log "bringing up the base platform (deploy/bootstrap.sh)"
  bash "$SRC_DIR/deploy/bootstrap.sh"
fi

command -v "$K3S"   >/dev/null 2>&1 || K3S=k3s
[ -x "$FELIS" ] || FELIS=felis
command -v "$K3S"   >/dev/null 2>&1 || die "k3s not found — did bootstrap complete?"
command -v "$FELIS" >/dev/null 2>&1 || die "felis not found — did bootstrap complete?"

# 2. get the two game images into k3s containerd --------------------------------
if [ -f "$IMG_DIR/felis-limbo.tar" ] && [ -f "$IMG_DIR/felis-lobby.tar" ]; then
  log "importing prebuilt image tars from $IMG_DIR"
  "$K3S" ctr images import "$IMG_DIR/felis-limbo.tar"
  "$K3S" ctr images import "$IMG_DIR/felis-lobby.tar"
else
  log "no prebuilt tars in $IMG_DIR — building on this host with docker"
  command -v docker >/dev/null 2>&1 || die "docker not found; cannot build images"

  rel=$(curl -fsSL --max-time 30 "https://ci.loohpjames.com/job/Limbo/lastSuccessfulBuild/api/json" \
          | grep -oE 'target/Limbo-[0-9][^"]+\.jar' | head -1) || true
  : "${LIMBO_JAR_URL:=https://ci.loohpjames.com/job/Limbo/lastSuccessfulBuild/artifact/$rel}"
  : "${LIMBO_SCHEM_URL:=https://ci.loohpjames.com/job/Limbo/lastSuccessfulBuild/artifact/spawn.schem}"
  : "${LIMBO_VERSION:=$(basename "$rel" | sed -E 's/^Limbo-//; s/\.jar$//; s/-[0-9]+\.[0-9]+$//')}"
  [ -n "$rel" ] || [ -n "${LIMBO_JAR_URL##*artifact/}" ] || die "could not resolve the Limbo jar; set LIMBO_JAR_URL"
  log "building $LIMBO_IMAGE (Limbo $LIMBO_VERSION)"
  docker build -f "$SRC_DIR/deploy/limbo/Dockerfile" \
    --build-arg LIMBO_JAR_URL="$LIMBO_JAR_URL" \
    --build-arg LIMBO_SCHEM_URL="$LIMBO_SCHEM_URL" \
    --build-arg LIMBO_VERSION="$LIMBO_VERSION" \
    -t "$LIMBO_IMAGE" "$SRC_DIR"
  docker save "$LIMBO_IMAGE" | "$K3S" ctr images import -

  : "${PAPER_MC_VERSION:=1.21.8}"
  : "${PAPER_JAR_URL:=$(curl -fsSL --max-time 30 "https://fill.papermc.io/v3/projects/paper/versions/${PAPER_MC_VERSION}/builds/latest" | grep -oE 'https://fill-data\.papermc\.io/[^"]+\.jar' | head -1)}"
  [ -n "$PAPER_JAR_URL" ] || die "could not resolve the Paper jar; set PAPER_JAR_URL"
  log "building $LOBBY_IMAGE (Paper $PAPER_MC_VERSION)"
  docker build -f "$SRC_DIR/deploy/lobby/Dockerfile" \
    --build-arg PAPER_JAR_URL="$PAPER_JAR_URL" \
    -t "$LOBBY_IMAGE" "$SRC_DIR"
  docker save "$LOBBY_IMAGE" | "$K3S" ctr images import -
fi

# 3. wire the images into the config `felis setup` reads ------------------------
log "wiring [velocity] images into $HOST_TOML"
[ -f "$HOST_TOML" ] || die "missing $HOST_TOML — did bootstrap run?"
if grep -q '^\[velocity\]' "$HOST_TOML"; then
  echo "  [velocity] table already present — leaving it untouched"
else
  cat >> "$HOST_TOML" <<EOF

[velocity]
login_image = "$LIMBO_IMAGE"
lobby_image = "$LOBBY_IMAGE"
EOF
  echo "  appended login_image=$LIMBO_IMAGE / lobby_image=$LOBBY_IMAGE"
fi

# 4. interactive Owner creation + system-server provisioning -------------------
if [ "${SKIP_SETUP:-0}" = 1 ]; then
  log "SKIP_SETUP=1 — base + images + config ready. Finish with:  sudo felis setup"
else
  log "launching 'felis setup' — create the Owner account (this is the only interactive step)"
  exec "$FELIS" setup
fi
