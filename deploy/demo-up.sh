#!/bin/bash
# SKIP_BOOTSTRAP=1 reuses an installed base and game stack; SKIP_SETUP=1 stops before the TUI.
set -Eeuo pipefail

SRC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
STATE_DIR=/etc/felis
HOST_TOML="$STATE_DIR/felis.host.toml"
PLUGIN_JAR=/opt/felis/velocity/plugins/felis-velocity.jar
K3S=/usr/local/bin/k3s
FELIS=/usr/local/bin/felis

log() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }
die() { printf '\033[1;31mERROR: %s\033[0m\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "run as root (sudo bash deploy/demo-up.sh)"

# 1. base platform + full game stack (deploy/bootstrap.sh) -----------------------
if [ "${SKIP_BOOTSTRAP:-0}" = 1 ]; then
  log "SKIP_BOOTSTRAP=1 — assuming the base platform is already up"
else
  log "bringing up the base platform and the game stack (deploy/bootstrap.sh)"
  bash "$SRC_DIR/deploy/bootstrap.sh"
fi

command -v "$K3S"   >/dev/null 2>&1 || K3S=k3s
[ -x "$FELIS" ] || FELIS=felis
command -v "$K3S"   >/dev/null 2>&1 || die "k3s not found — did bootstrap complete?"
command -v "$FELIS" >/dev/null 2>&1 || die "felis not found — did bootstrap complete?"

# When skipping bootstrap, verify the entire stack exists before opening setup.
[ -f "$HOST_TOML" ] || die "missing $HOST_TOML — did bootstrap run?"
grep -q '^\[velocity\]' "$HOST_TOML" \
  || die "$HOST_TOML has no [velocity] section — re-run the installer without SKIP_BOOTSTRAP so the system servers get wired"
[ -f "$PLUGIN_JAR" ] \
  || die "$PLUGIN_JAR missing — this base did not finish the full installer, and a proxy without it silently routes nothing; re-run the installer without SKIP_BOOTSTRAP"

# 2. interactive Owner creation + system-server provisioning --------------------
if [ "${SKIP_SETUP:-0}" = 1 ]; then
  log "SKIP_SETUP=1 — base + game stack ready. Finish with:  sudo felis setup"
else
  log "launching 'felis setup' — create the Owner account (this is the only interactive step)"
  exec "$FELIS" setup
fi
