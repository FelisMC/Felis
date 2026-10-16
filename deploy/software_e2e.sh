#!/bin/bash
# End to end check of server software installed from the runtime image. For each case it
# runs the three steps a server pod runs (felis install-server, felis init-forwarding, felis
# launch) in the Temurin image the pod would use, waits for the server to finish starting,
# then logs in through cmd/forwardcheck the way the Velocity proxy does and asks RCON for
# the player list, the operator's readiness probe.
#
#   deploy/software_e2e.sh FELIS_BIN FORWARDCHECK_BIN [CASE...]
#
# FELIS_BIN and FORWARDCHECK_BIN are linux binaries for this host's architecture. A CASE
# is type:minecraft:build:java:identity, with "-" for no build and identity "forwarded"
# (the server took the proxy's player identity) or "offline" (it logged the player in
# under the offline-mode UUID of the name). A custom case runs the vanilla 1.21.4 jar as
# the owner's own. Without cases it runs the matrix below, whose builds are fixed
# upstream releases.
#
# FELIS_E2E_RUNTIME  container runtime (podman; rootless, the game uid mapped to the caller; mounts are relabelled for SELinux)
# FELIS_E2E_CACHE    artifact cache base URL as the containers reach it (empty: direct)
# FELIS_E2E_WORK     scratch directory (default a new one under /tmp; kept on failure)
# FELIS_E2E_MEMORY   heap per server (default 1024M)
set -euo pipefail

felis="${1:?usage: software_e2e.sh FELIS_BIN FORWARDCHECK_BIN [CASE...]}"
check="${2:?usage: software_e2e.sh FELIS_BIN FORWARDCHECK_BIN [CASE...]}"
shift 2

runtime="${FELIS_E2E_RUNTIME:-podman}"
cache="${FELIS_E2E_CACHE:-}"
memory="${FELIS_E2E_MEMORY:-1024M}"
work="${FELIS_E2E_WORK:-$(mktemp -d /tmp/felis-software-e2e.XXXXXX)}"
secret="e2e-forwarding-secret"
rcon_password="e2e-rcon-password"
game_port=25600
rcon_port=25601

cases=("$@")
if [ "${#cases[@]}" -eq 0 ]; then
  cases=(
    vanilla:26.3:-:25:offline
    vanilla:1.16.5:-:8:offline
    paper:1.21.4:232:21:forwarded
    paper:1.16.5:794:8:forwarded
    paper:1.12.2:1620:8:offline
    purpur:1.21.4:2416:21:forwarded
    fabric:1.21.4:0.19.5:21:forwarded
    fabric:26.3:0.19.5:25:forwarded
    quilt:1.20.1:0.30.1:17:forwarded
    quilt:1.21.4:0.30.1:21:offline
    forge:1.20.1:47.4.10:17:forwarded
    forge:1.15.2:31.2.57:8:forwarded
    forge:1.12.2:14.23.5.2859:8:offline
    forge:26.3:66.0.12:25:offline
    neoforge:1.21.4:21.4.150:21:forwarded
    neoforge:26.3:26.3.0.85:25:offline
    custom:-:-:21:offline
  )
fi

mkdir -p "$work/bin"
install -m 0555 "$felis" "$work/bin/felis"
results="$work/results.txt"
: > "$results"
failed=0

# The vanilla 1.21.4 server jar, pinned by the sha1 Mojang's manifest gives for it.
custom_jar_url="https://piston-data.mojang.com/v1/objects/4707d00eb834b446575d89a61a11b5d548d8c001/server.jar"
custom_jar_sha1="4707d00eb834b446575d89a61a11b5d548d8c001"

# run_step NAME IMAGE ENV... -- CMD... runs one init step to completion, as the pod would.
run_step() {
  local image="$1"; shift
  local -a env=()
  while [ "$1" != "--" ]; do env+=(-e "$1"); shift; done
  shift
  "$runtime" run --rm --userns=keep-id:uid=1000,gid=1000 --user 1000:1000 \
    -v "$work/bin:/felis/bin:ro,z" -v "$data:/data:z" -w /data "${env[@]}" "$image" "$@"
}

for c in "${cases[@]}"; do
  IFS=: read -r type mc build java identity <<<"$c"
  name="$type-${mc//./_}"
  data="$work/$name"
  rm -rf "$data"
  mkdir -p "$data"
  image="docker.io/library/eclipse-temurin:${java}-jre"
  forwarding=none
  [ "$identity" = forwarded ] && forwarding=velocity
  if [ "$type" = custom ]; then
    curl -fsSL -o "$data/myserver.jar" "$custom_jar_url"
    echo "$custom_jar_sha1  $data/myserver.jar" | sha1sum -c --quiet
    spec="{\"type\":\"custom\",\"java\":$java,\"jar\":\"myserver.jar\",\"forwarding\":\"none\"}"
  else
    b=""
    [ "$build" != "-" ] && b=",\"build\":\"$build\""
    spec="{\"type\":\"$type\",\"minecraftVersion\":\"$mc\"$b,\"java\":$java,\"forwarding\":\"$forwarding\"}"
  fi
  echo "== $c"
  started=$(date +%s)
  if ! run_step "$image" "FELIS_SOFTWARE=$spec" "FELIS_ARTIFACT_CACHE=$cache" -- \
      /felis/bin/felis install-server --data /data > "$data.install.log" 2>&1; then
    echo "FAIL $c install (see $data.install.log)" | tee -a "$results"
    failed=1
    continue
  fi
  installed=$(( $(date +%s) - started ))
  if ! run_step "$image" "FELIS_SOFTWARE=$spec" "FELIS_FORWARDING_SECRET=$secret" -- \
      /felis/bin/felis init-forwarding --data /data > "$data.forwarding.log" 2>&1; then
    echo "FAIL $c init-forwarding (see $data.forwarding.log)" | tee -a "$results"
    failed=1
    continue
  fi
  ctr="felis-e2e-$name"
  "$runtime" rm -f "$ctr" >/dev/null 2>&1 || true
  "$runtime" run -d --name "$ctr" --userns=keep-id:uid=1000,gid=1000 --user 1000:1000 \
    -p "127.0.0.1:$game_port:25565" -p "127.0.0.1:$rcon_port:25575" \
    -v "$work/bin:/felis/bin:ro,z" -v "$data:/data:z" -w /data \
    -e "FELIS_SOFTWARE=$spec" -e "FELIS_FORWARDING_SECRET=$secret" \
    -e "RCON_PORT=25575" -e "RCON_PASSWORD=$rcon_password" -e "JAVA_MEMORY=$memory" \
    "$image" /felis/bin/felis launch --data /data >/dev/null
  ready=""
  for _ in $(seq 1 120); do
    if "$runtime" logs "$ctr" 2>&1 | grep -E 'Done \([0-9.]+s\)!' >/dev/null; then
      ready=yes
      break
    fi
    if [ "$("$runtime" inspect -f '{{.State.Running}}' "$ctr" 2>/dev/null)" != "true" ]; then
      break
    fi
    sleep 5
  done
  "$runtime" logs "$ctr" > "$data.server.log" 2>&1 || true
  if [ -z "$ready" ]; then
    echo "FAIL $c did not finish starting (see $data.server.log)" | tee -a "$results"
    "$runtime" rm -f "$ctr" >/dev/null 2>&1 || true
    failed=1
    continue
  fi
  booted=$(( $(date +%s) - started - installed ))
  out=$("$check" -addr "127.0.0.1:$game_port" -secret "$secret" \
    -rcon "127.0.0.1:$rcon_port" -rcon-password "$rcon_password" 2>&1) || true
  "$runtime" stop -t 60 "$ctr" >/dev/null 2>&1 || true
  "$runtime" logs "$ctr" > "$data.server.log" 2>&1 || true
  "$runtime" rm -f "$ctr" >/dev/null 2>&1 || true
  if printf '%s' "$out" | grep -F "\"identity\":\"$identity\"" >/dev/null \
      && printf '%s' "$out" | grep -F '"rcon":' >/dev/null; then
    echo "PASS $c install ${installed}s, start ${booted}s: $out" | tee -a "$results"
  else
    echo "FAIL $c login: $out" | tee -a "$results"
    failed=1
  fi
done

echo "results in $results"
if [ "$failed" = 0 ] && [ -z "${FELIS_E2E_WORK:-}" ]; then
  rm -rf "$work"
fi
[ "$failed" = 0 ]
