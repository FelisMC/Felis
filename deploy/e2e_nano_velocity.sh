#!/bin/bash
# Starts the locked Velocity with the staged Felis-nano bundle and checks that the plugin comes up in nano mode.
set -euo pipefail

lock="$(cd "$(dirname "$0")" && pwd)/game-stack.lock"
url="$(sed -n 's/^VELOCITY_JAR_URL=//p' "$lock")"
sum="$(sed -n 's/^VELOCITY_JAR_SHA256=//p' "$lock")"
java="${JAVA_HOME_21_X64:?the runner provides Java 21}/bin/java"
dir="$(mktemp -d)"
proxy=""
trap '[ -z "$proxy" ] || kill "$proxy" 2>/dev/null || true; rm -rf "$dir"' EXIT

curl -fsSL --retry 5 --retry-delay 2 -o "$dir/velocity.jar" "$url"
printf '%s  %s\n' "$sum" "$dir/velocity.jar" | sha256sum -c -
mkdir -p "$dir/plugins/felis-link"
sudo install -o "$(id -u)" -g "$(id -g)" -m 0644 /etc/felis/nano-proxy/felis-velocity.jar "$dir/plugins/felis-velocity.jar"
sudo install -o "$(id -u)" -g "$(id -g)" -m 0600 /etc/felis/nano-proxy/felis-link.properties "$dir/plugins/felis-link/felis-link.properties"

cd "$dir"
"$java" -Xmx256M -Dmojang.sessionserver=http://127.0.0.1:8081/session/minecraft/hasJoined -jar velocity.jar \
  < /dev/null > velocity.log 2>&1 &
proxy=$!
for _ in $(seq 1 90); do
  grep -q 'Felis-nano: sign-in only' velocity.log && break
  kill -0 "$proxy" 2>/dev/null || break
  sleep 1
done
if ! grep -q 'Felis-nano: sign-in only' velocity.log; then
  cat velocity.log
  echo "FAIL: the plugin did not start in nano mode"
  exit 1
fi
if grep -E 'Felis disabled|Felis routing DISABLED' velocity.log; then
  echo "FAIL: the plugin started outside nano mode"
  exit 1
fi
echo "PASS: Velocity $(sed -n 's/^VELOCITY_VERSION=//p' "$lock") runs the plugin in nano mode"
