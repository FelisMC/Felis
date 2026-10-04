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
# Only the jar is refreshed — LuckPerms keeps its H2 database and config under
# $DATA_DIR/plugins/LuckPerms/, which is exactly the state the PVC exists to preserve.
# Every grant the panel has ever issued lives there, so this must never be a wipe.
cp -f "$RUNTIME_DIR/plugins/LuckPerms.jar" "$DATA_DIR/plugins/LuckPerms.jar"
printf 'eula=true\n' > "$DATA_DIR/eula.txt"
cd "$DATA_DIR"

# set_prop KEY VALUE — replace the key's line in server.properties, or append it if absent.
set_prop() {
  if [ -f "$PROPS" ] && grep -q "^$1=" "$PROPS"; then
    # The RCON password is operator-provisioned arbitrary bytes: a '|', '\' or '&' would
    # otherwise corrupt this bare sed s||| and silently break the key. Same escaping as
    # deploy/limbo — without it an unlucky password kills the console/permission channel.
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
set_prop online-mode false

# Every authenticated player passes through the lobby, and a stopped server's players
# arrive together (they fall back to the login gate, which sends them straight on).
# Paper's default cap of 20 would turn the 21st away at the door. 200 is far above
# what one node serves at once, and a flood beyond it is refused at the door instead
# of running the 1Gi lobby out of memory. What the world itself allows (no damage, no
# building, the /menu hint) is felis-paper's LobbyGuard.
# Seed capacity once; administrators can tune it in the panel file editor.
if ! grep -q '^max-players=' "$PROPS"; then
  set_prop max-players 200
fi

# RCON is the control plane's write channel (spec §8 写=RCON): the operator probes it
# for readiness and the player tally, and felis-api runs console/permission commands over
# it. Paper only reads these three keys from server.properties, so the operator's injected
# RCON_PASSWORD has to be written here to take effect — env alone does nothing.
#
# Rewritten on EVERY boot from the Secret, deliberately. That makes the value in the world
# volume derived state rather than the source of truth: an owner who edits (or clobbers)
# these lines through the panel's file editor cannot lock the control plane out of their
# own server, because the next restart restores the real password. The editor is also kept
# from reading the password back out — see internal/fileedit/exec.go (spec §286: RCON
# 密码绝不下发前端).
#
# No password, no RCON: an empty enable-rcon=true would let anything that reaches the port
# in unauthenticated. Unlike the forwarding secret this is not fatal — a server without the
# write channel still serves players — so it warns and starts rather than refusing.
if [ -n "${RCON_PASSWORD:-}" ]; then
  set_prop enable-rcon true
  set_prop rcon.port "${RCON_PORT:-25575}"
  set_prop rcon.password "$RCON_PASSWORD"
  echo "felis-lobby: rcon enabled on port ${RCON_PORT:-25575}"
else
  set_prop enable-rcon false
  echo "felis-lobby: WARNING — RCON_PASSWORD is empty, so the console, the online-player" >&2
  echo "  list and permission changes will be unavailable for this server. The operator" >&2
  echo "  injects it from the <server>-rcon Secret when spec.rcon.enabled is true." >&2
fi

# The operator's existing init-forwarding step merges the proxy keys on every
# start, preserving other Paper globals. Standalone runs retain the mandatory
# rewrite because no initContainer has verified their forwarding settings.
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
