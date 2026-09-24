#!/usr/bin/env bash
# Refreshes deploy/game-stack.lock to upstream's newest builds, hashed.
#
#   bash deploy/update-game-stack-lock.sh            # rewrite the lock in place
#   bash deploy/update-game-stack-lock.sh --check    # exit 1 if upstream moved on
#
# It runs the same resolver bootstrap.sh uses for FELIS_GAME_STACK=latest (the functions are
# lifted out of bootstrap.sh, so the two cannot drift), then pins Velocity's newest build of
# VELOCITY_LATEST_MINOR. Limbo and LuckPerms publish no digest, so their jars are downloaded
# and hashed here; Paper and Velocity come from Fill's content-addressed URLs.
#
# Review the diff before committing: MC_VERSION moves the login gate's protocol, and the
# lobby, plain-Paper image and every client follow it.
set -Eeuo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BS="${here}/bootstrap.sh"
LOCK="${here}/game-stack.lock"
check=0
case "${1:-}" in
  --check) check=1 ;;
  "") ;;
  *) printf 'usage: %s [--check]\n' "$0" >&2; exit 2 ;;
esac

log()  { printf '[lock] %s\n' "$*" >&2; }
ok()   { printf '[ ok ] %s\n' "$*" >&2; }
warn() { :; }
die()  { printf '[fail] %s\n' "$*" >&2; exit 1; }

lift() { # function-name
  local body
  body="$(awk -v f="$1" '$0 ~ "^" f "\\(\\) \\{" {on=1} on {print} on && /^}/ {exit}' "$BS")"
  [ -n "$body" ] || die "bootstrap.sh no longer defines $1"
  eval "$body"
}
for fn in meta_get papermc_latest_jar luckperms_latest_jar url_sha256 resolve_latest_game_jars; do
  lift "$fn"
done
eval "$(grep '^VELOCITY_LATEST_MINOR=' "$BS")"
[ -n "${VELOCITY_LATEST_MINOR:-}" ] || die "bootstrap.sh no longer sets VELOCITY_LATEST_MINOR"

resolve_latest_game_jars
log "resolving the newest Velocity ${VELOCITY_LATEST_MINOR} build"
velocity="$(papermc_latest_jar velocity "$VELOCITY_LATEST_MINOR")" \
  || die "no Velocity build for ${VELOCITY_LATEST_MINOR}"
# shellcheck disable=SC2034 # read back through ${!key} below
VELOCITY_VERSION="$VELOCITY_LATEST_MINOR"
# shellcheck disable=SC2034
VELOCITY_JAR_URL="${velocity% *}"
# shellcheck disable=SC2034
VELOCITY_JAR_SHA256="${velocity##* }"

tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
# The comment header is kept as it is; only the KEY=value lines are regenerated.
sed -n '/^#/p;/^#/!q' "$LOCK" > "$tmp"
for key in MC_VERSION LIMBO_VERSION LIMBO_JAR_URL LIMBO_JAR_SHA256 LIMBO_SCHEM_URL LIMBO_SCHEM_SHA256 \
  PAPER_JAR_URL PAPER_JAR_SHA256 LUCKPERMS_JAR_URL LUCKPERMS_JAR_SHA256 \
  VELOCITY_VERSION VELOCITY_JAR_URL VELOCITY_JAR_SHA256; do
  printf '%s=%s\n' "$key" "${!key}" >> "$tmp"
done

if cmp -s "$tmp" "$LOCK"; then
  ok "game-stack.lock already pins upstream's newest builds"
  exit 0
fi
diff -u "$LOCK" "$tmp" >&2 || true
if [ "$check" = 1 ]; then
  die "upstream has newer builds than game-stack.lock"
fi
cp "$tmp" "$LOCK"
ok "game-stack.lock updated; run go test . and the bootstrap tests, then commit"
