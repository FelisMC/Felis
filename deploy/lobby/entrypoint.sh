#!/bin/sh
# Felis lobby (Paper) entrypoint.
#
# The lobby sits BEHIND the login gate: a player only reaches it once the limbo has
# authenticated them and Velocity transferred them onward. For that transfer to arrive
# with a real identity, Paper has to be told to verify the proxy's signed handshake —
# otherwise it derives an offline UUID from the username and every /menu action would be
# attributed to whoever typed the name. So, exactly as in deploy/limbo/entrypoint.sh:
# NO SECRET, NO START. Refusing to boot is the safe failure; a lobby that came up in
# offline mode would look healthy while trusting forged identities.
#
# Two files carry the settings:
#
#   config/paper-global.yml  proxies.velocity.{enabled,online-mode,secret} — enable modern
#                            forwarding and share the proxy's HMAC key. online-mode mirrors
#                            the proxy's own online-mode (true: Velocity did the Mojang
#                            auth), which is what makes the forwarded UUID trustworthy.
#
#   server.properties        online-mode=false — the PROXY authenticated the player, so the
#                            backend must not try to reach Mojang itself (Paper refuses to
#                            start with velocity forwarding on and online-mode=true). This
#                            is not a downgrade: the trust comes from the signed handshake.
#                            server-port is pinned to the operator's GamePort (25565), the
#                            single const the Service, probes and NetworkPolicy all key off.
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
printf 'eula=true\n' > "$DATA_DIR/eula.txt"
cd "$DATA_DIR"

# set_prop KEY VALUE — replace the key's line in server.properties, or append it if absent.
set_prop() {
  if [ -f "$PROPS" ] && grep -q "^$1=" "$PROPS"; then
    sed -i "s|^$1=.*|$1=$2|" "$PROPS"
  else
    printf '%s=%s\n' "$1" "$2" >> "$PROPS"
  fi
}

set_prop server-port "$PORT"
set_prop online-mode false

# ponytail: rewritten whole, not merged. Paper loads this file and fills every key it does
# not find with the default, then writes the full tree back — so a proxies-only file is a
# complete, stable input, and the lobby's other globals are simply always the defaults.
# That is true of a system server Felis owns end to end; if admins are ever allowed to tune
# the lobby's globals, this has to become a real YAML merge (yq) instead.
mkdir -p config
cat > config/paper-global.yml <<YAML
# Written by felis-lobby's entrypoint on every boot. Do not hand-edit: the forwarding
# secret is injected from the felis-forwarding-secret Secret and must match the proxy.
proxies:
  velocity:
    enabled: true
    online-mode: true
    secret: "${SECRET}"
YAML

echo "felis-lobby: server-port=${PORT}, velocity modern forwarding on (UUIDs are Mojang-verified)"
JAVA_MEMORY_ARG=""
if [ -n "${JAVA_MEMORY:-}" ]; then
  JAVA_MEMORY_ARG="-Xmx${JAVA_MEMORY}"
fi
set -f
# JAVA_FLAGS is emitted by the operator as a whitespace-separated JVM argument list.
# shellcheck disable=SC2086
exec java $JAVA_MEMORY_ARG ${JAVA_FLAGS:-} -jar paper.jar --nogui "$@"
