#!/bin/sh
# Felis login-limbo entrypoint.
#
# Pin Limbo's game port to the pod-facing port the operator contract uses. LOOHP/Limbo
# defaults server-port to 30000, but the Felis operator drives everything — the Service
# Port/TargetPort, the TCP/HTTP readiness probe, the container port and the Velocity
# NetworkPolicy — off a single GamePort const (25565). A backend that bound 30000 would
# be unreachable through that fence. Limbo writes a full server.properties on first run
# and merges any partial we leave in place, so seeding/patching just server-port here is
# enough; the spawn schematic still loads from ./spawn.schem.
#
# Idempotent by design: it runs on every start and rewrites only the server-port line,
# so a persisted world volume that already carries a server.properties keeps all its
# other settings.
set -eu

PORT="${FELIS_GAME_PORT:-25565}"
PROPS="server.properties"

if [ -f "$PROPS" ]; then
  if grep -q '^server-port=' "$PROPS"; then
    sed -i "s/^server-port=.*/server-port=${PORT}/" "$PROPS"
  else
    printf 'server-port=%s\n' "$PORT" >> "$PROPS"
  fi
else
  printf 'server-port=%s\n' "$PORT" > "$PROPS"
fi

echo "felis-limbo: pinned server-port=${PORT} (operator GamePort)"
exec java -jar Limbo.jar --nogui "$@"
