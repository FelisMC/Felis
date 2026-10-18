#!/bin/sh
# Pin Limbo's game port and require signed modern forwarding before accepting identities.
set -eu

PORT="${FELIS_GAME_PORT:-25565}"
SECRET="${FELIS_FORWARDING_SECRET:-}"
RUNTIME_DIR="/limbo"
DATA_DIR="/data"
PROPS="server.properties"

if [ -z "$SECRET" ]; then
  echo "felis-limbo: FATAL — FELIS_FORWARDING_SECRET is empty." >&2
  echo "  The login gate authenticates the Owner, so it must not run without Velocity modern" >&2
  echo "  forwarding: an unverified UUID would let anyone claim any Minecraft identity." >&2
  echo "  Provision the secret with deploy/bootstrap.sh, then re-run 'sudo felis setup'." >&2
  exit 1
fi

# Keep mutable server state on the operator-mounted PVC. Refresh code artifacts on
# every boot so an image upgrade takes effect without replacing worlds or config.
mkdir -p "$DATA_DIR/plugins"
cp -f "$RUNTIME_DIR/Limbo.jar" "$DATA_DIR/Limbo.jar"
cp -f "$RUNTIME_DIR/plugins/felis-limbo.jar" "$DATA_DIR/plugins/felis-limbo.jar"
if [ -f "$RUNTIME_DIR/spawn.schem" ] && [ ! -f "$DATA_DIR/spawn.schem" ]; then
  cp "$RUNTIME_DIR/spawn.schem" "$DATA_DIR/spawn.schem"
fi
cd "$DATA_DIR"

# set_prop KEY VALUE — replace the key's line, or append it if absent.
set_prop() {
  if [ -f "$PROPS" ] && grep -q "^$1=" "$PROPS"; then
    # The secret is base64/hex-ish, but '/' or '&' would break a bare sed s///. The value's '|', backslash and '&' are escaped, and '|' is the delimiter.
    esc=$(printf '%s' "$2" | sed 's/[|\\&]/\\&/g')
    # Through a temp file rather than sed -i, which BSD sed reads differently, so the same function runs under the entrypoint tests on any machine.
    sed "s|^$1=.*|$1=${esc}|" "$PROPS" > "$PROPS.tmp"
    cat "$PROPS.tmp" > "$PROPS"
    rm -f "$PROPS.tmp"
  else
    printf '%s=%s\n' "$1" "$2" >> "$PROPS"
  fi
}

set_prop server-port "$PORT"
# Disable legacy forwarding explicitly so persisted settings cannot downgrade signature verification.
set_prop bungeecord false
set_prop bungee-guard false
set_prop velocity-modern true
set_prop forwarding-secrets "$SECRET"
# Pin Limbo's unlimited player cap so reconnect bursts cannot fill the authentication fallback.
set_prop max-players -1

echo "felis-limbo: server-port=${PORT}, velocity-modern=true (forwarding secret loaded, UUIDs are Mojang-verified)"
JAVA_MEMORY_ARG=""
if [ -n "${JAVA_MEMORY:-}" ]; then
  JAVA_MEMORY_ARG="-Xmx${JAVA_MEMORY}"
fi
set -f
# JAVA_FLAGS is emitted by the operator as a whitespace-separated JVM argument list.
# shellcheck disable=SC2086
exec java $JAVA_MEMORY_ARG ${JAVA_FLAGS:-} -jar Limbo.jar --nogui "$@"
