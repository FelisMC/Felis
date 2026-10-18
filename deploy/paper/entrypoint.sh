#!/bin/sh
# Forwarding belongs to the init container; standalone user servers retain normal online authentication.
set -eu

PORT="${FELIS_GAME_PORT:-25565}"
RUNTIME_DIR="/paper"
DATA_DIR="/data"
PROPS="server.properties"

cd "$DATA_DIR"
printf 'eula=true\n' > eula.txt

# Escape arbitrary property values so passwords cannot corrupt sed substitutions.
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

# Regenerate RCON credentials each boot and disable RCON when no password is supplied.
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
