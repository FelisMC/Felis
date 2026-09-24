#!/bin/bash
# Checks a host that deploy/bootstrap.sh just installed (or re-ran on). The e2e workflow
# runs it on a fresh GitHub runner after each of its two installer runs:
#
#   sudo bash deploy/e2e_check.sh install   # after the first run
#   sudo bash deploy/e2e_check.sh rerun     # after the same commit ran again
#   sudo bash deploy/e2e_check.sh upgrade   # after this commit ran over a release
#
# It asks what an operator's first minutes ask: the binary runs, the control plane is
# rolled out and ready, the panel answers on its NodePort, the proxy answers a Minecraft
# status ping, and the host timers are there. A rerun must also leave the proxy running
# (it restarts only when what it runs changed) and keep every earlier answer.
set -euo pipefail

phase="${1:?usage: e2e_check.sh install|rerun|upgrade}"
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

for d in felis-api felis-operator registry; do
  check "deployment ${d} is rolled out" "${KUBECTL[@]}" -n felis rollout status "deploy/${d}" --timeout=180s
done

node_ip="$(ip -4 route get 1.1.1.1 | awk '{for (i = 1; i <= NF; i++) if ($i == "src") { print $(i + 1); exit }}')"
check "the panel serves its page on the NodePort" \
  sh -c "curl -skf --retry 10 --retry-delay 3 --retry-all-errors https://${node_ip}:30443/ | grep -qi '<html'"
# Readiness lives on the internal face only; a Service ClusterIP routes from the node.
internal="$("${KUBECTL[@]}" -n felis get svc felis-api-internal -o jsonpath='{.spec.clusterIP}:{.spec.ports[0].port}')"
check "felis-api is ready (database and cluster reachable)" \
  curl -sf --retry 10 --retry-delay 3 --retry-all-errors -o /dev/null "http://${internal}/readyz"

for unit in k3s postgresql felis-velocity; do
  check "${unit} is active" systemctl is-active --quiet "$unit"
done
for timer in felis-db-backup.timer felis-watchdog.timer; do
  check "${timer} is scheduled" systemctl is-enabled --quiet "$timer"
done

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
if version="$(ping_proxy 2>&1)"; then
  pass "the proxy answers a status ping (${version})"
else
  fail "the proxy answers a status ping: ${version}"
fi

pid="$(systemctl show -p MainPID --value felis-velocity)"
case "$phase" in
  install) printf '%s\n' "$pid" > "$PID_FILE" ;;
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
fi
exit "$fails"
