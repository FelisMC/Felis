#!/bin/bash
# demo-up.sh — one-shot Felis demo bring-up.
#
#     sudo bash deploy/demo-up.sh
#
# Every piece a demo box needs — base platform (k3s + felis + docker + cloudflared +
# control plane), the limbo/lobby/paper images, the felis-velocity proxy plugin, and
# the [velocity] wiring in felis.host.toml — is built by deploy/bootstrap.sh. This
# wrapper adds only the one step the installer cannot do: the interactive
# `felis setup` TUI that creates the Owner account.
#
# It used to rebuild the game images here with its own copy of that logic, written
# before bootstrap grew the job. The copy drifted: it pinned Paper 1.21.8 while the
# installer derives one version from the Limbo login gate (both hops of a login must
# speak one protocol), never built felis-velocity.jar (so the proxy it wired had
# nowhere to route), and left docker running. The installer is the single origin.
#
# Toggles: SKIP_BOOTSTRAP=1 (base + game stack already installed by a full bootstrap),
#          SKIP_SETUP=1 (stop before the TUI).
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

# SKIP_BOOTSTRAP=1 trusts an earlier run to be complete. Check that it actually left
# the full stack behind: a base from before the game-stack installer, or one whose
# pieces were pruned by hand, must fail here with a pointer — not present as a proxy
# that accepts logins and routes nowhere, with nothing in any log to say why.
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
