#!/bin/sh
# Trust Velocity's signed identity, disable backend authentication, and use the operator's game port.
set -eu

PORT="${FELIS_GAME_PORT:-25565}"
SECRET="${FELIS_FORWARDING_SECRET:-}"
RUNTIME_DIR="/paper"
DATA_DIR="/data"
PROPS="server.properties"

if [ -z "$SECRET" ]; then
  echo "felis-lobby: FATAL — FELIS_FORWARDING_SECRET is empty." >&2
  echo "  Without Velocity modern forwarding Paper cannot verify who a joining player is," >&2
  echo "  and would trust an offline UUID derived from the username alone." >&2
  echo "  Provision the secret with deploy/bootstrap.sh, then re-run 'sudo felis setup'." >&2
  exit 1
fi

# Keep worlds, generated config, and plugin data on the operator-mounted PVC while
# refreshing executable artifacts from the immutable image on every boot.
mkdir -p "$DATA_DIR/plugins"
cp -f "$RUNTIME_DIR/paper.jar" "$DATA_DIR/paper.jar"
cp -f "$RUNTIME_DIR/plugins/felis-paper.jar" "$DATA_DIR/plugins/felis-paper.jar"
# Refresh the LuckPerms JAR only; preserve its PVC-backed database and configuration.
cp -f "$RUNTIME_DIR/plugins/LuckPerms.jar" "$DATA_DIR/plugins/LuckPerms.jar"
printf 'eula=true\n' > "$DATA_DIR/eula.txt"
cd "$DATA_DIR"

# set_prop KEY VALUE — replace the key's line in server.properties, or append it if absent.
set_prop() {
  if [ -f "$PROPS" ] && grep -q "^$1=" "$PROPS"; then
    # Escape RCON passwords for sed, including delimiter, backslash, and replacement metacharacters.
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
set_prop online-mode false

# Seed the lobby cap once at 200; preserve administrator changes afterward.
if ! grep -q '^max-players=' "$PROPS"; then
  set_prop max-players 200
fi

# Regenerate RCON properties from Secrets each boot; absent passwords disable RCON without blocking server startup.
if [ -n "${RCON_PASSWORD:-}" ]; then
  set_prop enable-rcon true
  set_prop rcon.port "${RCON_PORT:-25575}"
  set_prop rcon.password "$RCON_PASSWORD"
  # The platform's own RCON commands stay out of operators' chat and the log.
  set_prop broadcast-rcon-to-ops false
  echo "felis-lobby: rcon enabled on port ${RCON_PORT:-25575}"
else
  set_prop enable-rcon false
  echo "felis-lobby: WARNING — RCON_PASSWORD is empty, so the console, the online-player" >&2
  echo "  list and permission changes will be unavailable for this server. The operator" >&2
  echo "  injects it from the <server>-rcon Secret when spec.rcon.enabled is true." >&2
fi

# Let the operator merge Paper globals; standalone runs must configure signed forwarding themselves.
mkdir -p config
if [ "${FELIS_MANAGED_FORWARDING:-false}" = true ]; then
  [ -f config/paper-global.yml ] || { echo "felis-lobby: missing managed forwarding config" >&2; exit 1; }
else
  cat > config/paper-global.yml <<YAML
proxies:
  velocity:
    enabled: true
    online-mode: true
    secret: "${SECRET}"
YAML
fi

echo "felis-lobby: server-port=${PORT}, velocity modern forwarding on (UUIDs are Mojang-verified)"
JAVA_MEMORY_ARG=""
if [ -n "${JAVA_MEMORY:-}" ]; then
  JAVA_MEMORY_ARG="-Xmx${JAVA_MEMORY}"
fi
set -f
# JAVA_FLAGS is emitted by the operator as a whitespace-separated JVM argument list.
# shellcheck disable=SC2086
exec java $JAVA_MEMORY_ARG ${JAVA_FLAGS:-} -jar paper.jar --nogui "$@"
