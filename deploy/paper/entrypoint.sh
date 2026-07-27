#!/bin/sh
# Felis general-purpose Paper server entrypoint.
#
# A plain Paper backend for a user's OWN world — NOT a system server. Unlike deploy/limbo
# and deploy/lobby it writes no Velocity forwarding config and has no secret gate: the
# operator injects a root `felis init-forwarding` initContainer that writes
# config/paper-global.yml + server.properties online-mode=false onto /data BEFORE this
# container starts, so forwarding is configured externally and this stays a drop-in Paper
# image. With no initContainer (no FELIS_IMAGE) Paper just boots standalone-online —
# degraded, not broken; a user's own world is not identity-critical, so refusing to boot
# would be the wrong failure here.
#
# What this DOES own: eula, the game-port pin, and the RCON control channel. The operator
# injects RCON_PASSWORD/RCON_PORT into every server whose spec.rcon is enabled
# (operator.buildEnv), and Paper only reads these keys from server.properties — so without
# writing them here the console, the online-player list and permission commands go dark;
# env alone does nothing. This is the exact trap the lobby entrypoint documents.
set -eu

PORT="${FELIS_GAME_PORT:-25565}"
RUNTIME_DIR="/paper"
DATA_DIR="/data"
PROPS="server.properties"

cd "$DATA_DIR"
printf 'eula=true\n' > eula.txt

# set_prop KEY VALUE — replace the key's line in server.properties, or append it if absent.
# The escaping matches deploy/limbo: an RCON password is operator-provisioned arbitrary
# bytes, so a '|', '\' or '&' in the value would corrupt a bare `sed s|...|...|` and
# silently break the key (the bug the lobby's original set_prop carried).
set_prop() {
  if [ -f "$PROPS" ] && grep -q "^$1=" "$PROPS"; then
    esc=$(printf '%s' "$2" | sed 's/[|\\&]/\\&/g')
    sed -i "s|^$1=.*|$1=${esc}|" "$PROPS"
  else
    printf '%s=%s\n' "$1" "$2" >> "$PROPS"
  fi
}

# Pin the port the operator's Service, readiness probe and NetworkPolicy all key off
# (GamePort). A stale persisted properties file with a different port would be unreachable
# through that fence.
set_prop server-port "$PORT"

# RCON is the control plane's write channel (spec §8 写=RCON): the operator probes it for
# readiness and the player tally, and felis-api runs console/permission commands over it.
# Rewritten on EVERY boot from the Secret, so the value is derived state — an owner who
# edits or clobbers these lines through the panel file editor cannot lock the control plane
# out of their own server, because the next restart restores the real password.
#
# No password, no RCON: an empty enable-rcon=true would admit anything that reaches the port
# unauthenticated. Unlike the forwarding secret this is not fatal — a server without the
# write channel still serves players — so it warns and starts rather than refusing.
if [ -n "${RCON_PASSWORD:-}" ]; then
  set_prop enable-rcon true
  set_prop rcon.port "${RCON_PORT:-25575}"
  set_prop rcon.password "$RCON_PASSWORD"
  echo "felis-paper: rcon enabled on port ${RCON_PORT:-25575}"
else
  set_prop enable-rcon false
  echo "felis-paper: WARNING — RCON_PASSWORD is empty, so the console, the online-player" >&2
  echo "  list and permission changes will be unavailable for this server. The operator" >&2
  echo "  injects it from the <server>-rcon Secret when spec.rcon.enabled is true." >&2
fi

echo "felis-paper: server-port=${PORT} (Velocity forwarding is applied by the init-forwarding initContainer)"
JAVA_MEMORY_ARG=""
if [ -n "${JAVA_MEMORY:-}" ]; then
  JAVA_MEMORY_ARG="-Xmx${JAVA_MEMORY}"
fi
set -f
# JAVA_FLAGS is emitted by the operator as a whitespace-separated JVM argument list. The jar
# stays in the immutable image seed (/paper); only worlds/config live on the PVC (cwd).
# shellcheck disable=SC2086
exec java $JAVA_MEMORY_ARG ${JAVA_FLAGS:-} -jar "$RUNTIME_DIR/paper.jar" --nogui "$@"
