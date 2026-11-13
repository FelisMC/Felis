#!/bin/bash
# Checks a Felis-nano install on a CI runner: usage: e2e_nano_check.sh <installer-log> <release-assets-dir>
set -euo pipefail

log="$1"
dist="$2"
fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { echo "PASS: $*"; }

if ! systemctl is-active --quiet felis-nano; then
  journalctl -u felis-nano -n 60 --no-pager || true
  fail "felis-nano is not active"
fi
pass "felis-nano is active"

perm="$(stat -c '%a %G' /etc/felis/felis.toml)"
[ "$perm" = "640 felis-nano" ] || fail "felis.toml is ${perm}, want 640 felis-nano"
grep -qx 'tag = "e2e"' /etc/felis/felis.toml || fail "the earlier nano's auth source left felis.toml"
pass "felis.toml keeps the earlier source and is readable by felis-nano alone"

out="$(curl -fsS http://127.0.0.1:8081/readyz)"
[[ "$out" == *'"ready"'* ]] || fail "readyz answered ${out}"
out="$(curl -fsSk https://127.0.0.1:8443/config.json)"
[[ "$out" == *'"nano":true'* ]] || fail "the panel's config.json is not a nano one: ${out}"
code="$(curl -sk -o /dev/null -w '%{http_code}' https://127.0.0.1:8443/api/v1/servers)"
[ "$code" = 404 ] || fail "the panel serves /api/v1/servers (${code})"
pass "the internal port is ready and the panel is a nano panel without server routes"

# A Yggdrasil source on loopback answers serverId felis-e2e with a fixed native id.
python3 - <<'PY' &
import http.server, json, urllib.parse
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        u = urllib.parse.urlparse(self.path)
        q = urllib.parse.parse_qs(u.query)
        if u.path == "/session/minecraft/hasJoined" and q.get("serverId") == ["felis-e2e"]:
            body = json.dumps({"id": "0123456789abcdef0123456789abcdef", "name": q["username"][0], "properties": []}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        else:
            self.send_response(204)
            self.end_headers()
    def log_message(self, *args):
        pass
http.server.HTTPServer(("127.0.0.1", 9999), H).serve_forever()
PY
stub=$!
trap 'kill "$stub" 2>/dev/null || true' EXIT
sleep 1

# The UUID a source's player gets is uuid3(namespace, tag:native-id); Python computes it independently of Felis.
want="$(python3 -c 'import uuid, sys; print(uuid.uuid3(uuid.UUID("07228eae-77f6-500e-9dc0-436afbc87c27"), "e2e:" + sys.argv[1]).hex)' 0123456789abcdef0123456789abcdef)"
out="$(curl -fsS 'http://127.0.0.1:8081/session/minecraft/hasJoined?username=FelisNanoE2E&serverId=felis-e2e')"
got="$(python3 -c 'import json, sys; print(json.load(sys.stdin)["id"])' <<<"$out")"
[ "$got" = "$want" ] || fail "the source's player joined as ${got}, want ${want}"
pass "a player of the configured source joins with the derived UUID"

token="$(sed -n 's/^FELIS_SERVICE_TOKEN=//p' /etc/felis/nano.env)"
[ -n "$token" ] || fail "nano.env holds no FELIS_SERVICE_TOKEN"
body="{\"mc_uuid\":\"${got}\"}"
code="$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d "$body" http://127.0.0.1:8081/api/v1/internal/account/link/code)"
[ "$code" = 401 ] || fail "a link code without the service token answered ${code}"
out="$(curl -fsS -X POST -H "Authorization: Bearer ${token}" -H 'Content-Type: application/json' -d "$body" http://127.0.0.1:8081/api/v1/internal/account/link/code)"
root="$(sed -n 's/^root_domain = "\(.*\)"$/\1/p' /etc/felis/felis.toml)"
[[ "$out" == *'"code":"'* ]] || fail "no link code: ${out}"
[[ "$out" == *"\"panel_url\":\"https://console.${root}:8443\""* ]] || fail "the link reply does not name the panel with its port: ${out}"
pass "the proxy plugin's link call needs the service token and names the panel's port"

props=/etc/felis/nano-proxy/felis-link.properties
[ "$(stat -c '%a' "$props")" = 600 ] || fail "${props} is not 0600"
grep -qx 'mode=nano' "$props" || fail "${props} has no mode=nano"
grep -qx 'api-base-url=http://127.0.0.1:8081' "$props" || fail "${props} names another api-base-url"
grep -qx "service-token=${token}" "$props" || fail "${props} carries another token than nano.env"
cmp -s /etc/felis/nano-proxy/felis-velocity.jar "${dist}/felis-velocity.jar" || fail "the staged plugin is not the release's"
pass "the proxy bundle carries nano mode, the token and the release's plugin"

grep -q '/setup?token=' "$log" || fail "the installer printed no setup link"
out="$(ufw status)"
[[ "$out" == *'felis-nano-panel'* ]] || fail "ufw does not admit the panel port"
if felis update >/dev/null 2>"${TMPDIR:-/tmp}/nano-update.err"; then fail "felis update ran on a nano host"; fi
grep -q 'Felis-nano' "${TMPDIR:-/tmp}/nano-update.err" || fail "felis update did not point at the installer"
pass "setup link printed, panel port admitted, felis update refused"
