#!/bin/bash
# Checks a host that deploy/bootstrap.sh just installed (or re-ran on). The e2e workflow
# runs it on a fresh GitHub runner after each of its two installer runs:
#
#   sudo bash deploy/e2e_check.sh install   # after the first run
#   sudo bash deploy/e2e_check.sh rerun     # after the same commit ran again
#   sudo bash deploy/e2e_check.sh release   # after the newest release installed
#   sudo bash deploy/e2e_check.sh upgrade   # after this commit ran over a release
#
# It asks what an operator's first minutes ask: the binary runs, the control plane and its
# database are rolled out and ready, a database backup can be taken and restored, the panel
# answers on its NodePort, the proxy answers a Minecraft status ping, the host timers are
# all there and waiting, and the backup and watchdog runs the installer made succeeded.
# A rerun must also leave the proxy running (it restarts only when what it runs changed)
# and keep every earlier answer.
set -euo pipefail

phase="${1:?usage: e2e_check.sh install|rerun|release|upgrade}"
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
KUBECTL=(/usr/local/bin/k3s kubectl)
PID_FILE=/var/tmp/felis-e2e-velocity.pid
fails=0

pass() { printf 'PASS %s\n' "$*"; }
fail() { printf 'FAIL %s\n' "$*"; fails=$((fails + 1)); }
check() { # label command...
  local label="$1"
  shift
  if "$@"; then pass "$label"; else fail "$label"; fi
}

check "felis version runs" sh -c '/usr/local/bin/felis version | grep -q "^felis "'

deploys=(felis-api felis-operator registry)
# A release may still run the database on the host; the upgrade moves it into felis-postgres.
if [ "$phase" != release ] || "${KUBECTL[@]}" -n felis get deploy/felis-postgres >/dev/null 2>&1; then
  deploys=(felis-postgres "${deploys[@]}")
fi
for d in "${deploys[@]}"; do
  check "deployment ${d} is rolled out" "${KUBECTL[@]}" -n felis rollout status "deploy/${d}" --timeout=180s
done

node_ip="$(ip -4 route get 1.1.1.1 | awk '{for (i = 1; i <= NF; i++) if ($i == "src") { print $(i + 1); exit }}')"
check "the panel serves its page on the NodePort" \
  sh -c "curl -skf --retry 10 --retry-delay 3 --retry-all-errors https://${node_ip}:30443/ | grep -qi '<html'"
# Readiness lives on the internal face only; a Service ClusterIP routes from the node.
internal="$("${KUBECTL[@]}" -n felis get svc felis-api-internal -o jsonpath='{.spec.clusterIP}:{.spec.ports[0].port}')"
check "felis-api is ready (database and cluster reachable)" \
  curl -sf --retry 10 --retry-delay 3 --retry-all-errors -o /dev/null "http://${internal}/readyz"

for unit in k3s felis-velocity; do
  check "${unit} is active" systemctl is-active --quiet "$unit"
done
# pod_psql runs one statement in the database's pod, over its socket, as the felis role on
# the felis database.
pod_psql() {
  "${KUBECTL[@]}" -n felis exec -i deploy/felis-postgres -c postgres -- \
    psql -X -q -At -v ON_ERROR_STOP=1 -U felis -d felis -c "$1"
}

# restore_drill walks troubleshooting.md's "Restore on the same host": refused while the
# control plane is connected; with it scaled to 0 the bundle comes back (a row written after
# it is gone) and the database it replaced is kept; migrate up runs; the control plane serves
# again.
restore_drill() { # dir bundle
  local dir="$1" bundle="$2" out rc
  local sel="app.kubernetes.io/part-of=felis-control-plane,app.kubernetes.io/component in (api,operator)"
  if ! pod_psql "INSERT INTO platform_settings (key, value) VALUES ('e2e_restore_drill', '1')" >/dev/null; then
    fail "write a row after the bundle"
    return
  fi
  rc=0
  out="$(/usr/local/bin/felis db restore -dir "$dir" -yes "$bundle" 2>&1)" || rc=$?
  if [ "$rc" -eq 1 ] && grep -q "other clients are connected to the database" <<<"$out"; then
    pass "felis db restore refuses while the control plane is connected"
  else
    fail "felis db restore refuses while the control plane is connected (exit ${rc}): ${out}"
  fi

  "${KUBECTL[@]}" -n felis scale deployment felis-api felis-operator --replicas=0 >/dev/null
  for _ in $(seq 60); do
    [ -z "$("${KUBECTL[@]}" -n felis get pods -l "$sel" -o name)" ] && break
    sleep 2
  done
  rc=0
  out="$(/usr/local/bin/felis db restore -dir "$dir" -yes "$bundle" 2>&1)" || rc=$?
  if [ "$rc" -eq 0 ]; then
    pass "felis db restore replays the bundle with the control plane scaled to 0"
  else
    fail "felis db restore replays the bundle with the control plane scaled to 0 (exit ${rc}): ${out}"
  fi
  check "the restore dropped the row written after the bundle" \
    test "$(pod_psql "SELECT count(*) FROM platform_settings WHERE key = 'e2e_restore_drill'")" = 0
  check "the restore kept the database it replaced in a pre-restore bundle" \
    sh -c "ls '${dir}' | grep -q -- '-pre-restore\.tar\$'"
  check "felis migrate up runs on the restored database" \
    /usr/local/bin/felis migrate up -config /etc/felis/felis.host.toml
  "${KUBECTL[@]}" -n felis scale deployment felis-api felis-operator --replicas=1 >/dev/null
  for d in felis-api felis-operator; do
    check "deployment ${d} is rolled out again after the restore" "${KUBECTL[@]}" -n felis rollout status "deploy/${d}" --timeout=180s
  done
  check "felis-api is ready on the restored database" \
    curl -sf --retry 10 --retry-delay 3 --retry-all-errors -o /dev/null "http://${internal}/readyz"
}

# The database runs in k3s; a release may still run it on the host, and the upgrade moved
# it. The host has no PostgreSQL client: a bundle that verifies proves felis reaches the
# database's pod through kubectl exec, and that pg_dump there reads every table.
if [ "$phase" != release ]; then
  check "the host's own postgresql is stopped" sh -c '! systemctl is-active --quiet postgresql'
  bundle_dir="$(mktemp -d)"
  if out="$(/usr/local/bin/felis db backup -dir "$bundle_dir" -state-dir "" -no-servers 2>&1)"; then
    bundle="$(printf '%s\n' "$out" | sed -n 's/^felis db backup: wrote //p' | tail -n 1)"
    check "felis db backup writes a bundle that verifies" /usr/local/bin/felis db verify "$bundle"
    # A rerun keeps what the install left; the install and the upgrade restore it.
    [ "$phase" = rerun ] || restore_drill "$bundle_dir" "$bundle"
  else
    fail "felis db backup writes a bundle: ${out}"
  fi
  rm -rf "$bundle_dir"
fi
# The daily reaper is what deletes expired archives. Run it once from its CronJob: the API
# accepts a pod template naming a ServiceAccount that does not exist, and only the Job's
# pod creation fails, so rendering it proves nothing. A release may carry exactly that bug.
if [ "$phase" != release ]; then
  reaper_job="felis-e2e-reaper-${phase}"
  "${KUBECTL[@]}" -n minecraft delete job "$reaper_job" --ignore-not-found >/dev/null
  if "${KUBECTL[@]}" -n minecraft create job --from=cronjob/felis-reaper "$reaper_job" >/dev/null; then
    check "the reaper CronJob runs to completion" \
      "${KUBECTL[@]}" -n minecraft wait --for=condition=complete "job/${reaper_job}" --timeout=180s
    "${KUBECTL[@]}" -n minecraft delete job "$reaper_job" --ignore-not-found >/dev/null
  else
    fail "a Job can be created from the reaper CronJob"
  fi
fi
# A release may predate a timer. What this commit installs has every timer bootstrap.sh
# names, enabled and waiting, and no felis timer it does not name. The off-site copy's
# comes only with an [offsite] bucket, which no e2e host has.
if [ "$phase" != release ]; then
  timers="$(sed -n 's|^[A-Z_]*_TIMER="/etc/systemd/system/\(felis-[a-z-]*\.timer\)"$|\1|p' "$(dirname "$0")/bootstrap.sh")"
  check "bootstrap.sh names the felis timers" test -n "$timers"
  for timer in $timers; do
    if [ "$timer" = felis-offsite.timer ]; then
      check "${timer} is not installed without an [offsite] bucket" test ! -e "/etc/systemd/system/${timer}"
      continue
    fi
    check "${timer} is enabled" systemctl is-enabled --quiet "$timer"
    check "${timer} is waiting" systemctl is-active --quiet "$timer"
  done
  for timer in $(systemctl list-unit-files --no-legend 'felis-*.timer' | awk '{print $1}'); do
    check "${timer} is a timer bootstrap.sh installs" grep -qxF "$timer" <<<"$timers"
  done
  # The installer runs the backup and the watchdog once itself and only warns when that
  # fails: a unit that cannot run (a flag the binary lacks, a path its sandbox hides) shows
  # here. Without an [smtp] relay the watchdog has no mail to fail on.
  for unit in felis-db-backup.service felis-watchdog.service; do
    check "${unit} has run" test "$(systemctl show -p ExecMainStartTimestampMonotonic --value "$unit")" != 0
    check "${unit}'s last run succeeded" test "$(systemctl show -p Result --value "$unit")" = success
  done
  # A failed watchdog run starts the unit its OnFailure= names, which must be installed.
  on_failure="$(systemctl show -p OnFailure --value felis-watchdog.service)"
  check "felis-watchdog.service names an OnFailure= unit" test -n "$on_failure"
  for unit in $on_failure; do
    check "${unit} is installed" test "$(systemctl show -p LoadState --value "$unit")" = loaded
  done
fi

# A status ping is the proxy's own answer (ping passthrough is off), so it proves the JRE,
# Velocity and its config without a login gate or a Mojang account.
ping_proxy() {
  python3 - <<'EOF'
import json, socket, struct, sys

def varint(n):
    out = b""
    n &= 0xFFFFFFFF
    while True:
        b, n = n & 0x7F, n >> 7
        if n:
            out += bytes([b | 0x80])
        else:
            return out + bytes([b])

def read_varint(s):
    n = 0
    for i in range(5):
        b = s.recv(1)
        if not b:
            raise EOFError("connection closed")
        n |= (b[0] & 0x7F) << (7 * i)
        if not b[0] & 0x80:
            return n
    raise ValueError("varint too long")

host, port = "127.0.0.1", 25565
s = socket.create_connection((host, port), timeout=10)
hs = varint(0) + varint(767) + varint(len(host)) + host.encode() + struct.pack(">H", port) + varint(1)
s.sendall(varint(len(hs)) + hs + varint(1) + varint(0))
read_varint(s)
read_varint(s)
size = read_varint(s)
data = b""
while len(data) < size:
    chunk = s.recv(size - len(data))
    if not chunk:
        raise EOFError("short status response")
    data += chunk
print(json.loads(data)["version"]["name"])
EOF
}
# The installer returns once the unit is started; the JVM binds the port a few
# seconds later.
for _ in $(seq 30); do
  if version="$(ping_proxy 2>&1)"; then
    break
  fi
  sleep 2
done
if version="$(ping_proxy 2>&1)"; then
  pass "the proxy answers a status ping (${version})"
else
  fail "the proxy answers a status ping: ${version}"
fi

pid="$(systemctl show -p MainPID --value felis-velocity)"
case "$phase" in
  install | release) printf '%s\n' "$pid" > "$PID_FILE" ;;
  rerun)
    if [ "$pid" = "$(cat "$PID_FILE" 2>/dev/null)" ]; then
      pass "the rerun left the proxy running (pid ${pid})"
    else
      fail "the rerun restarted the proxy (pid $(cat "$PID_FILE" 2>/dev/null || echo '?') -> ${pid}) though nothing it runs changed"
    fi
    ;;
  upgrade) ;; # a new release may well change what the proxy runs
  *) fail "unknown phase ${phase}" ;;
esac

if [ "$fails" -eq 0 ]; then
  echo "ALL PASS (${phase})"
else
  echo "${fails} FAILED (${phase})"
  exit 1
fi
