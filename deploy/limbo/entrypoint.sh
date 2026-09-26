#!/bin/sh
# Felis login-limbo entrypoint.
#
# Two things must be true before Limbo accepts a connection, and both are settings
# Limbo writes into server.properties on first run:
#
#   server-port      — pinned to the pod-facing port the operator contract uses. LOOHP/Limbo
#                      defaults it to 30000, but the Felis operator drives everything (the
#                      Service Port/TargetPort, the readiness probe, the container port and
#                      the Velocity NetworkPolicy) off a single GamePort const (25565). A
#                      backend that bound 30000 would be unreachable through that fence.
#
#   velocity-modern  — Velocity modern player-info forwarding. This is the ONLY reason the
#   forwarding-secrets  login gate can be trusted to know WHO joined. With it on, Limbo
#                      verifies the proxy's HMAC over the login payload and takes the
#                      player's UUID from that signed payload (ClientConnection.java:
#                      validateVelocityModernResponse → getVelocityDataFrom → new Player(...,
#                      data.getUuid())). With it off, Limbo derives an OFFLINE UUID from the
#                      username — and the felis-limbo plugin would then mint a /link code
#                      bound to the WRONG Minecraft identity. The Owner IS a Minecraft
#                      account, claimed by joining this gate, so that is account takeover,
#                      not a cosmetic bug.
#
# Hence: NO SECRET, NO START. Refusing to boot is the safe failure — the operator marks the
# server Failed, `felis setup` surfaces the reason and stops before asking for a link code.
# A limbo that came up in offline mode would look perfectly healthy while handing out
# forgeable identities. forwarding-secrets is Limbo's ';'-separated list; Felis writes one.
#
# Idempotent by design: it runs on every start and rewrites only the keys below, so a
# persisted volume that already carries a server.properties keeps its other settings (and
# the spawn schematic still loads from ./spawn.schem).
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
    # The secret is base64/hex-ish, but a '/' or '&' would still break a bare sed s///.
    # '|' as the delimiter plus escaping it is enough for every value we write.
    esc=$(printf '%s' "$2" | sed 's/[|\\&]/\\&/g')
    # Through a temp file rather than sed -i, which BSD sed reads differently, so the
    # same function runs under the entrypoint tests on any machine.
    sed "s|^$1=.*|$1=${esc}|" "$PROPS" > "$PROPS.tmp"
    cat "$PROPS.tmp" > "$PROPS"
    rm -f "$PROPS.tmp"
  else
    printf '%s=%s\n' "$1" "$2" >> "$PROPS"
  fi
}

set_prop server-port "$PORT"
# velocity-modern is mutually exclusive with the two legacy schemes in Limbo's own
# check — pin them off so a stale persisted properties file cannot silently downgrade
# the gate to a forwarding mode that carries no signature at all.
set_prop bungeecord false
set_prop bungee-guard false
set_prop velocity-modern true
set_prop forwarding-secrets "$SECRET"
# Unbound players wait here for up to ten minutes, and a stopped server's players all
# fall back here at once, so the gate must never be full. -1 (no cap) is Limbo's own
# default; it is pinned so a hand-edited properties file on the volume cannot bring
# a cap back.
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
