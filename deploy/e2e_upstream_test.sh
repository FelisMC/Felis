#!/bin/bash
# Checks for deploy/e2e_upstream.sh. Run it as: bash deploy/e2e_upstream_test.sh
#
# The trap line in the logs is bootstrap.sh's own on_error message, printed the way its warn
# prints it, so rewording it there fails here rather than turning every refused download
# back into a red e2e run. The curl lines are curl's own wording.
set -u

here="$(dirname "$0")"
EU="${1:-${here}/e2e_upstream.sh}"
BS="${2:-${here}/bootstrap.sh}"
[ -f "$EU" ] || { echo "no such script: $EU"; exit 1; }
[ -f "$BS" ] || { echo "no such script: $BS"; exit 1; }
fails=0

expect() { # label needle haystack
  case "$3" in
    *"$2"*) echo "PASS $1" ;;
    *) echo "FAIL $1: expected <$2> in:"; echo "$3"; fails=$((fails + 1)) ;;
  esac
}
status() { # label want got
  if [ "$2" = "$3" ]; then echo "PASS $1"; else echo "FAIL $1: exit $3, want $2"; fails=$((fails + 1)); fi
}
same() { # label want got
  if [ "$2" = "$3" ]; then echo "PASS $1"; else printf 'FAIL %s: got <%s>, want <%s>\n' "$1" "$3" "$2"; fails=$((fails + 1)); fi
}

root="$(mktemp -d)"
trap 'rm -rf "$root"' EXIT

trap_body="$(grep -E '^[[:space:]]*warn "bootstrap failed near line' "$BS" | head -n 1)"
if [ -z "$trap_body" ]; then
  echo "FAIL bootstrap.sh's on_error prints no 'bootstrap failed near line' warning"
  fails=$((fails + 1))
fi
trap_body="${trap_body#*\"}"
trap_body="${trap_body%\"*}"
trapped() { # line code: on_error's warning as the installer prints it
  # shellcheck disable=SC2034 # read by the eval
  local line="$1" code="$2"
  printf '\033[1;33m[warn]\033[0m %s\n' "$(eval "printf '%s' \"${trap_body}\"")"
}
step() { printf '\033[1;36m[felis]\033[0m %s\n' "$1"; }

run() { # log status: prints what the script says; $root/env is its GITHUB_ENV
  : > "$root/env"
  GITHUB_ENV="$root/env" bash "$EU" "$1" "$2" 2>&1
}

# The upgrade job's v0.2.0 install on 2026-10-02: GitHub refused cloudflared's download.
{
  step "release channel: v0.2.0"
  step "installing cloudflared 2026.9.1 (amd64)"
  echo "curl: (22) The requested URL returned error: 403"
  trapped 1727 22
} > "$root/403.log"
out="$(run "$root/403.log" 22)"
status "a 403 on a download skips" 0 $?
expect "  with a warning that quotes curl" "::warning::the installer stopped on an upstream download (curl: (22) The requested URL returned error: 403)" "$out"
same "  and gates the later steps" "E2E_UPSTREAM_SKIP=1" "$(cat "$root/env")"

for e in "(6) Could not resolve host: github.com" \
    "(7) Failed to connect to github.com port 443 after 130 ms: Couldn't connect to server" \
    "(28) Operation timed out after 300000 milliseconds with 0 out of 0 bytes received" \
    "(35) OpenSSL SSL_connect: SSL_ERROR_SYSCALL in connection to objects.githubusercontent.com:443" \
    "(56) Recv failure: Connection reset by peer" \
    "(22) The requested URL returned error: 429" \
    "(22) The requested URL returned error: 503" \
    "(22) The requested URL returned error: 502 Bad Gateway"; do
  code="${e#(}"
  code="${code%%)*}"
  { step "installing k3s"; echo "curl: $e"; trapped 900 "$code"; } > "$root/e.log"
  out="$(run "$root/e.log" "$code")"
  status "curl: $e skips" 0 $?
done

# What the installer prints after the trap, as it exits, changes nothing.
{
  cat "$root/403.log"
  printf '\033[1;33m[warn]\033[0m %s\n' "restored the previous felis binary at /usr/local/bin/felis; the database was not migrated, so rerunning the installer picks up where this run stopped"
} > "$root/cleanup.log"
out="$(run "$root/cleanup.log" 22)"
status "warnings after the trap still skip" 0 $?

# set -E: the trap again for a function the failure unwound through.
{ cat "$root/403.log"; trapped 1790 22; } > "$root/twice.log"
out="$(run "$root/twice.log" 22)"
status "the trap fired twice still skips" 0 $?

# A 404 is a URL or a version the installer names.
{ step "installing cloudflared 2026.9.1 (amd64)"; echo "curl: (22) The requested URL returned error: 404"; trapped 1727 22; } > "$root/404.log"
out="$(run "$root/404.log" 22)"
status "a 404 fails with the installer's status" 22 $?
expect "  and says so" "the installer failed (exit 22) on something other than a refused upstream download" "$out"
same "  and gates nothing" "" "$(cat "$root/env")"

for e in "401" "400" "410"; do
  { echo "curl: (22) The requested URL returned error: $e"; trapped 1727 22; } > "$root/e.log"
  out="$(run "$root/e.log" 22)"
  status "a $e fails" 22 $?
done

# A refused download the installer got past, then a failure of its own that happens to
# exit with curl's status: only the line before the trap counts.
{
  echo "curl: (22) The requested URL returned error: 403"
  step "building felis from source"
  trapped 4100 22
} > "$root/later.log"
out="$(run "$root/later.log" 22)"
status "a refused download earlier in the log fails" 22 $?

# curl's error on the line before, but the installer stopped with another status.
{ echo "curl: (22) The requested URL returned error: 403"; trapped 1727 1; } > "$root/mismatch.log"
out="$(run "$root/mismatch.log" 1)"
status "a trap for another status fails" 1 $?

# The installer's own die, which the trap never sees.
{ step "installing k3s"; printf '\033[1;31m[fail]\033[0m %s\n' "k3s did not become ready"; } > "$root/die.log"
out="$(run "$root/die.log" 1)"
status "the installer's own failure fails" 1 $?
same "  and gates nothing" "" "$(cat "$root/env")"

: > "$root/empty.log"
out="$(run "$root/empty.log" 141)"
status "an empty log fails with the installer's status" 141 $?

echo
if [ "$fails" -eq 0 ]; then echo "ALL PASS"; else echo "${fails} FAILED"; exit 1; fi
