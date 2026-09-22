#!/bin/sh
# Checks for deploy/bootstrap.sh. Run it as: sh deploy/bootstrap_test.sh
#
# The script it tests cannot be run here — it wants root, a package manager, k3s and the
# network — so each case extracts the block it is about out of bootstrap.sh verbatim and runs
# that with die/log stubbed. Extraction rather than a transcribed copy is the point: a copy
# passes forever after someone edits the original.
set -u

BS="${1:-$(dirname "$0")/bootstrap.sh}"
[ -f "$BS" ] || { echo "no such script: $BS"; exit 1; }
fails=0

expect() { # label needle haystack
  case "$3" in
    *"$2"*) echo "PASS $1" ;;
    *) echo "FAIL $1: expected <$2> in:"; echo "$3"; fails=$((fails + 1)) ;;
  esac
}

# --- the FELIS_VELOCITY_FORK_JAR digest gate -------------------------------------------
# This jar becomes the proxy every player connects through, so the interesting cases are the
# two refusals, not the happy path.

gate="$(awk '/have="\$\(sha256sum <"\$FELIS_VELOCITY_FORK_JAR"/,/^    log "installing the Felis-Legacy/' "$BS")"
[ -n "$gate" ] || { echo "FAIL: no fork-jar digest gate found in $BS"; exit 1; }
# awk runs an unmatched end pattern to EOF, which would quietly pipe the rest of bootstrap.sh
# into the `sh -c` below. The emptiness check above only catches a broken start pattern.
[ "$(printf '%s\n' "$gate" | wc -l)" -lt 40 ] \
  || { echo "FAIL: the extracted block is not the gate -- did its last line move?"; exit 1; }

jar="$(mktemp)"
trap 'rm -f "$jar"' EXIT
printf 'stand-in for a fork build\n' > "$jar"
want="$(sha256sum <"$jar" | cut -d' ' -f1)"

run_gate() { # digest
  FELIS_VELOCITY_FORK_JAR="$jar" FELIS_VELOCITY_FORK_JAR_SHA256="$1" sh -c '
    die() { printf "DIE: %s\n" "$*"; exit 1; }
    log() { printf "LOG: %s\n" "$*"; }
    '"$gate"
}

out="$(run_gate '')"
expect "fork jar without a digest is refused" "DIE: FELIS_VELOCITY_FORK_JAR_SHA256 is required" "$out"
expect "the refusal names the jar's real digest" "$want" "$out"

out="$(run_gate 'deadbeef')"
expect "a mismatched digest is refused" "checksum mismatch: got ${want}, expected deadbeef" "$out"

out="$(run_gate "$want")"
expect "the matching digest installs" "LOG: installing the Felis-Legacy Velocity fork" "$out"
expect "the install line records the digest" "(sha256 ${want})" "$out"

# The build host is usually Windows, where nothing prints the digest the way sha256sum does:
# Get-FileHash returns uppercase and certutil has shipped the bytes space-separated. Feeding
# the gate its own output can never catch that -- these two cases are the operator's paste.
out="$(run_gate "$(printf '%s' "$want" | tr 'a-z' 'A-Z')")"
expect "an uppercase digest is the same digest" "LOG: installing the Felis-Legacy Velocity fork" "$out"

out="$(run_gate "$(printf '%s' "$want" | sed 's/../& /g')")"
expect "a space-separated digest is the same digest" "LOG: installing the Felis-Legacy Velocity fork" "$out"

# --- papermc_latest_jar answers "url sha256" from one response --------------------------
# Fill's download URLs are content-addressed (/v1/objects/<sha256>/<name>.jar), and the
# resolver's contract is to hand both halves back from the same grep — or refuse a URL
# that carries no digest, rather than wave the download through unchecked. Run under
# bash, not sh: bootstrap.sh is bash and the function uses $'\n'.

fn="$(awk '/^papermc_latest_jar\(\)/,/^}/' "$BS")"
[ -n "$fn" ] || { echo "FAIL: no papermc_latest_jar in $BS"; exit 1; }
[ "$(printf '%s\n' "$fn" | wc -l)" -lt 30 ] \
  || { echo "FAIL: the extracted papermc_latest_jar is not just the function -- did its closing brace move?"; exit 1; }

rsha=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef

run_resolver() { # canned-fill-response
  CANNED="$1" bash -c '
    curl() { printf "%s" "$CANNED"; }
    '"$fn"'
    if out="$(papermc_latest_jar velocity 3.5.1)"; then
      printf "RESOLVED %s\n" "$out"
    else
      printf "REFUSED\n"
    fi
  '
}

out="$(run_resolver "{\"url\":\"https://fill-data.papermc.io/v1/objects/${rsha}/velocity-3.5.1-615.jar\"}")"
expect "the resolver pairs the url with its own digest" \
  "RESOLVED https://fill-data.papermc.io/v1/objects/${rsha}/velocity-3.5.1-615.jar ${rsha}" "$out"

out="$(run_resolver '{"url":"https://fill-data.papermc.io/mirror/velocity-3.5.1-615.jar"}')"
expect "a URL that carries no digest is refused" "REFUSED" "$out"

# --- the resolved-Velocity digest gate --------------------------------------------------
# The download must hash to what the content-addressed URL promised, BEFORE
# atomic_install_file — the same refusal the Via plugins and the fork jar already get.

# The end pattern spells ${VELOCITY_DIR} with dots: escaped braces are literal in gawk
# and mawk but undefined in POSIX awk, and CI's awk is whatever ubuntu ships.
vblock="$(awk '/log "resolving the newest Velocity/,/atomic_install_file "\$tmp" "\$.VELOCITY_DIR.\/velocity\.jar"/' "$BS")"
[ -n "$vblock" ] || { echo "FAIL: no resolved-Velocity install block found in $BS"; exit 1; }
[ "$(printf '%s\n' "$vblock" | wc -l)" -lt 30 ] \
  || { echo "FAIL: the extracted block is not the velocity install -- did its last line move?"; exit 1; }

vdir="$(mktemp -d)"
trap 'rm -f "$jar"; rm -rf "$vdir"' EXIT
vwant="$(printf 'stand-in velocity build\n' | sha256sum | cut -d' ' -f1)"

run_velocity_install() { # digest-the-resolver-reports
  WANT="$1" VELOCITY_DIR="$vdir" FELIS_VELOCITY_VERSION=3.5.1 bash -c '
    die() { printf "DIE: %s\n" "$*"; exit 1; }
    log() { printf "LOG: %s\n" "$*"; }
    remember_temp() { :; }
    papermc_latest_jar() {
      printf "%s %s\n" "https://fill-data.papermc.io/v1/objects/${WANT}/velocity-3.5.1-615.jar" "$WANT"
    }
    curl() { while [ "$#" -gt 1 ] && [ "$1" != "-o" ]; do shift; done; printf "stand-in velocity build\n" > "$2"; }
    atomic_install_file() { printf "INSTALL: %s\n" "$2"; }
    '"$vblock"
}

out="$(run_velocity_install deadbeef)"
expect "a download that does not hash to the promised digest is refused" \
  "DIE: Velocity 3.5.1 checksum mismatch: got ${vwant}, expected deadbeef" "$out"
case "$out" in
  *INSTALL:*) echo "FAIL a refused download must not reach atomic_install_file"; fails=$((fails + 1)) ;;
  *) echo "PASS a refused download is not installed" ;;
esac

out="$(run_velocity_install "$vwant")"
expect "the matching download installs" "INSTALL: ${vdir}/velocity.jar" "$out"

# --- [[auth_source]] carry-forward -----------------------------------------------------
# write_felis_toml regenerates felis.toml wholesale on every run; this is what keeps the
# operator's Yggdrasil roots from being reset to the shipped default.

ablock="$(awk '/^persisted_auth_source_blocks\(\) \{/,/^}/' "$BS")"
[ -n "$ablock" ] || { echo "FAIL: no persisted_auth_source_blocks found in $BS"; exit 1; }
[ "$(printf '%s\n' "$ablock" | wc -l)" -lt 20 ] \
  || { echo "FAIL: the extracted block is not the function -- did its closing brace move?"; exit 1; }

sdir="$(mktemp -d)"
trap 'rm -f "$jar"; rm -rf "$vdir" "$sdir"' EXIT

run_carry() {
  STATE_DIR="$sdir" bash -c "$ablock"'
    persisted_auth_source_blocks'
}

out="$(run_carry)"
expect "a first install gets the LittleSkin default" 'tag = "littleskin"' "$out"

printf '%s\n' '[server]' 'listen = "0.0.0.0:8080"' '' '[[auth_source]]' 'tag = "guild"' \
  'prefix = "GD"' 'url = "https://guild.example/hasJoined"' '' '[smtp]' 'host = "mail.example"' \
  > "$sdir/felis.host.toml"
out="$(run_carry)"
expect "an operator's root is carried forward" 'tag = "guild"' "$out"
case "$out" in
  *littleskin*|*"[smtp]"*) echo "FAIL the carried list must be exactly the operator's tables:"; echo "$out"; fails=$((fails + 1)) ;;
  *) echo "PASS the carried list stops at the next section and adds no default" ;;
esac

printf '%s\n' '[server]' 'listen = "0.0.0.0:8080"' > "$sdir/felis.host.toml"
out="$(run_carry)"
if [ -z "$out" ]; then
  echo "PASS a config with no sources stays Mojang-only"
else
  echo "FAIL a config with no sources must not get the default back:"; echo "$out"; fails=$((fails + 1))
fi

# The felis setup TUI re-encodes the whole file, which indents keys under each table.
rm -f "$sdir/felis.host.toml"
printf '%s\n' '[[auth_source]]' '  tag = "littleskin"' '  prefix = "LS"' '  url = "https://a.example"' \
  '' '[[auth_source]]' '  tag = "guild"' '  prefix = "GD"' '  url = "https://b.example"' \
  > "$sdir/felis.pod.toml"
out="$(run_carry)"
expect "both encoder-written tables are carried (first)" '  tag = "littleskin"' "$out"
expect "both encoder-written tables are carried (second)" '  tag = "guild"' "$out"

# TOML allows spaces inside the brackets and a quoted key. Each is still the operator's table.
for hdr in '[[ auth_source ]]' '[["auth_source"]]' "[['auth_source']]"; do
  printf '%s\n' "$hdr" 'tag = "guild"' 'prefix = "GD"' 'url = "https://b.example"' '' \
    '[smtp]' 'host = "mail.example"' > "$sdir/felis.host.toml"
  out="$(run_carry)"
  expect "a $hdr header is carried" "$hdr" "$out"
  expect "a $hdr table keeps its keys" 'tag = "guild"' "$out"
  case "$out" in
    *"[smtp]"*) echo "FAIL a $hdr table must stop at the next section:"; echo "$out"; fails=$((fails + 1)) ;;
    *) echo "PASS a $hdr table stops at the next section" ;;
  esac
done

# --- write_nano_config leaves the unit able to read its config ---------------------------
# felis-nano runs as a DynamicUser, so the directory must be searchable by others under a
# hardened umask too, including one an older installer left at 0750 -- but the full
# install's, locked to 0700 for its secrets, must not be widened.

wblock="$(awk '/^write_nano_config\(\) \{/,/^}/' "$BS")"
[ -n "$wblock" ] || { echo "FAIL: no write_nano_config found in $BS"; exit 1; }
[ "$(printf '%s\n' "$wblock" | wc -l)" -lt 50 ] \
  || { echo "FAIL: the extracted block is not the function -- did its closing brace move?"; exit 1; }

run_nano_config() { # state-dir
  STATE_DIR="$1" SECRETS_ENV="$1/secrets.env" BOOTSTRAP_DONE="$1/bootstrap.done" bash -c 'umask 027
    ok() { printf "OK: %s\n" "$*"; }
    '"$wblock"'
    write_nano_config'
}

mkdir "$sdir/probe" && chmod 0700 "$sdir/probe"
if [ "$(stat -c %a "$sdir/probe")" = 700 ]; then
  run_nano_config "$sdir/nano" >/dev/null
  expect "a fresh config dir is searchable under umask 027" 755 "$(stat -c %a "$sdir/nano")"
  mkdir "$sdir/old" && chmod 0750 "$sdir/old"
  run_nano_config "$sdir/old" >/dev/null
  expect "a nano-only 0750 dir is opened up" 755 "$(stat -c %a "$sdir/old")"
  : > "$sdir/probe/secrets.env"
  run_nano_config "$sdir/probe" >/dev/null
  expect "a dir holding the full install's secrets is not widened" 700 "$(stat -c %a "$sdir/probe")"
  mkdir "$sdir/done" && chmod 0700 "$sdir/done" && : > "$sdir/done/bootstrap.done"
  run_nano_config "$sdir/done" >/dev/null
  expect "a dir marked as a full install is not widened" 700 "$(stat -c %a "$sdir/done")"
else
  echo "SKIP directory modes: this filesystem ignores chmod"
fi

# --- install_nano_service reports a unit that dies at once ------------------------------
# A config the new binary rejects leaves the unit in auto-restart; the install must say so
# instead of printing "started" over a proxy whose every login now fails.

iblock="$(awk '/^install_nano_service\(\) \{/,/^}/' "$BS")"
[ -n "$iblock" ] || { echo "FAIL: no install_nano_service found in $BS"; exit 1; }
[ "$(printf '%s\n' "$iblock" | wc -l)" -lt 50 ] \
  || { echo "FAIL: the extracted block is not the function -- did its closing brace move?"; exit 1; }

run_nano_service() { # exit status systemctl is-active reports
  ACTIVE="$1" NANO_SERVICE="$sdir/felis-nano.service" HOST_BIN=/usr/local/bin/felis \
    STATE_DIR=/etc/felis FELIS_NANO_LISTEN=127.0.0.1:25580 bash -c '
    die() { printf "DIE: %s\n" "$*"; exit 1; }
    ok() { printf "OK: %s\n" "$*"; }
    sleep() { :; }
    systemctl() { if [ "$1" = is-active ]; then return "$ACTIVE"; fi; }
    journalctl() { printf "JOURNAL: config: needs prefix\n"; }
    '"$iblock"'
    install_nano_service'
}

out="$(run_nano_service 3)"
expect "a unit that dies at once fails the install" "DIE: felis-nano did not stay up" "$out"
expect "the failure shows the unit's own log" "JOURNAL: config: needs prefix" "$out"
case "$out" in
  *"OK: felis-nano.service"*) echo "FAIL a dead unit must not be reported as started"; fails=$((fails + 1)) ;;
  *) echo "PASS a dead unit is not reported as started" ;;
esac

out="$(run_nano_service 0)"
expect "a unit that stays up is reported as started" "OK: felis-nano.service enabled and started" "$out"

# --- a re-run on a nano host keeps what that host is --------------------------------------
# Re-running the installer is how a nano host updates. It must not move the endpoint an
# off-host proxy points at, nor default a nano-only host to the full control plane. The unit
# read back here is the one install_nano_service wrote above.

rblock="$(awk '/^resolve_nano_listen\(\) \{/,/^}/' "$BS")"
[ -n "$rblock" ] || { echo "FAIL: no resolve_nano_listen found in $BS"; exit 1; }
[ "$(printf '%s\n' "$rblock" | wc -l)" -lt 20 ] \
  || { echo "FAIL: the extracted block is not the function -- did its closing brace move?"; exit 1; }

run_listen() { # env-value unit-path
  FELIS_NANO_LISTEN="$1" NANO_SERVICE="$2" bash -c "$rblock"'
    resolve_nano_listen
    printf "LISTEN: %s\n" "$FELIS_NANO_LISTEN"'
}

expect "a re-run keeps the unit's listen address" "LISTEN: 127.0.0.1:25580" \
  "$(run_listen '' "$sdir/felis-nano.service")"
expect "the operator's address beats the unit's" "LISTEN: 10.0.0.5:8081" \
  "$(run_listen 10.0.0.5:8081 "$sdir/felis-nano.service")"
expect "a first install listens on loopback" "LISTEN: 127.0.0.1:8081" \
  "$(run_listen '' "$sdir/absent.service")"
expect "a first install takes the operator's address" "LISTEN: 10.0.0.5:8081" \
  "$(run_listen 10.0.0.5:8081 "$sdir/absent.service")"

# Whatever the address came from, it reaches the firewall, the summary and the unit as-is.
lblock="$(awk '/^validate_listen\(\) \{/,/^}/' "$BS")"
[ -n "$lblock" ] || { echo "FAIL: no validate_listen found in $BS"; exit 1; }
[ "$(printf '%s\n' "$lblock" | wc -l)" -lt 20 ] \
  || { echo "FAIL: the extracted block is not the function -- did its closing brace move?"; exit 1; }

check_listen() { # value
  bash -c 'die() { printf "DIE: %s\n" "$*"; exit 1; }
    '"$lblock"'
    validate_listen FELIS_NANO_LISTEN "$1" && echo VALID' _ "$1" 2>&1
}

for v in 8081 127.0.0.1 127.0.0.1:0 127.0.0.1:65536 127.0.0.1:x; do
  expect "listen address $v is refused" "DIE: FELIS_NANO_LISTEN" "$(check_listen "$v")"
done
for v in '[::1]:8081' 0.0.0.0:8081 127.0.0.1:8081; do
  expect "listen address $v is accepted" VALID "$(check_listen "$v")"
done

# The summary hands the operator the URL to paste into the proxy's JVM flags, so it must
# name the address nano actually binds, and the node's only for a wildcard bind.
sblock="$(awk '/^summary_nano\(\) \{/,/^}/' "$BS")"
[ -n "$sblock" ] || { echo "FAIL: no summary_nano found in $BS"; exit 1; }
[ "$(printf '%s\n' "$sblock" | wc -l)" -lt 40 ] \
  || { echo "FAIL: the extracted block is not the function -- did its closing brace move?"; exit 1; }
kblock="$(awk '/^nano_listen_is_loopback\(\) \{/,/^}/' "$BS")"
[ -n "$kblock" ] || { echo "FAIL: no nano_listen_is_loopback found in $BS"; exit 1; }
[ "$(printf '%s\n' "$kblock" | wc -l)" -lt 10 ] \
  || { echo "FAIL: the extracted block is not the function -- did its closing brace move?"; exit 1; }

run_summary() { # listen
  FELIS_NANO_LISTEN="$1" NODE_IP=203.0.113.9 STATE_DIR=/etc/felis bash -c '
    ok() { printf "OK: %s\n" "$*"; }
    log() { printf "LOG: %s\n" "$*"; }
    systemctl() { :; }
    '"$kblock"'
    '"$sblock"'
    summary_nano'
}

expect "a private bind is the address printed" "http://10.0.0.5:8081/session/minecraft/hasJoined" \
  "$(run_summary 10.0.0.5:8081)"
expect "an IPv6 loopback bind is printed as bound" "http://[::1]:8081/session/minecraft/hasJoined" \
  "$(run_summary '[::1]:8081')"
out="$(run_summary 127.0.0.1:8081)"
expect "a loopback bind is printed as bound" "http://127.0.0.1:8081/session/minecraft/hasJoined" "$out"
expect "a loopback bind keeps its loopback note" "Bound to loopback" "$out"
for v in 0.0.0.0:8081 '[::]:8081' :8081; do
  expect "a wildcard $v bind prints the node's address" "http://203.0.113.9:8081/session/minecraft/hasJoined" \
    "$(run_summary "$v")"
done

pblock="$(awk '/^prompt_install_mode\(\) \{/,/^}/' "$BS")"
[ -n "$pblock" ] || { echo "FAIL: no prompt_install_mode found in $BS"; exit 1; }
[ "$(printf '%s\n' "$pblock" | wc -l)" -lt 60 ] \
  || { echo "FAIL: the extracted block is not the function -- did its closing brace move?"; exit 1; }
tblock="$(awk '/^bootstrap_from_tui\(\) \{/,/^}/' "$BS")"
[ -n "$tblock" ] || { echo "FAIL: no bootstrap_from_tui found in $BS"; exit 1; }
[ "$(printf '%s\n' "$tblock" | wc -l)" -lt 5 ] \
  || { echo "FAIL: the extracted block is not the function -- did its closing brace move?"; exit 1; }

# Only the no-terminal path can run unattended, and with a terminal attached the prompt
# would sit waiting on it. setsid drops the controlling terminal, as cloud-init and CI have.
notty=""
if (: </dev/tty) 2>/dev/null; then
  if command -v setsid >/dev/null 2>&1; then notty=setsid; else notty=skip; fi
fi

run_mode() { # unit-path done-marker-path [FELIS_INSTALL_MODE [FELIS_BOOTSTRAP_FROM_TUI]]
  INSTALL_MODE="${3:-}" FELIS_BOOTSTRAP_FROM_TUI="${4:-}" NANO_SERVICE="$1" BOOTSTRAP_DONE="$2" \
    $notty bash -c '
    die() { printf "DIE: %s\n" "$*"; exit 1; }
    log() { printf "LOG: %s\n" "$*"; }
    '"$tblock"'
    '"$pblock"'
    prompt_install_mode </dev/null
    printf "MODE: %s\n" "$INSTALL_MODE"' 2>&1
}

if [ "$notty" = skip ]; then
  echo "SKIP install-mode default: a terminal is attached and there is no setsid to drop it"
else
  : > "$sdir/bootstrap.done"
  expect "a nano-only host re-runs as nano" "MODE: nano" \
    "$(run_mode "$sdir/felis-nano.service" "$sdir/absent.done")"
  expect "a host with the full install re-runs as full" "MODE: full" \
    "$(run_mode "$sdir/felis-nano.service" "$sdir/bootstrap.done")"
  out="$(run_mode "$sdir/absent.service" "$sdir/absent.done")"
  expect "a fresh host defaults to full" "MODE: full" "$out"
  expect "no controlling terminal takes the no-prompt path" "LOG: no terminal for a prompt" "$out"
  # felis setup goes on to need the control plane, so under it nano is refused, and the
  # nano-only default above must not apply either.
  expect "felis setup refuses FELIS_INSTALL_MODE=nano" "DIE: felis setup installs the full control plane" \
    "$(run_mode "$sdir/absent.service" "$sdir/absent.done" nano 1)"
  expect "felis setup installs full on a nano-only host" "MODE: full" \
    "$(run_mode "$sdir/felis-nano.service" "$sdir/absent.done" "" 1)"
fi

# --- install_go_toolchain checks the tarball before it replaces anything ----------------
# The tarball is unpacked into /usr/local and run as root, so a download that does not hash
# to the pin is refused -- and refused before the working toolchain is removed.

gblock="$(awk '/^install_go_toolchain\(\) \{/,/^}/' "$BS")"
[ -n "$gblock" ] || { echo "FAIL: no install_go_toolchain found in $BS"; exit 1; }
[ "$(printf '%s\n' "$gblock" | wc -l)" -lt 50 ] \
  || { echo "FAIL: the extracted block is not the function -- did its closing brace move?"; exit 1; }

gsum="$(printf 'stand-in go toolchain\n' | sha256sum | cut -d' ' -f1)"
groot="$sdir/go"

run_go() { # FELIS_GO_VERSION pinned-amd64-digest [FELIS_GO_SHA256]
  FELIS_GO_VERSION="$1" GO_PINNED_VERSION=1.26.4 GO_PINNED_SHA256_AMD64="$2" \
    GO_PINNED_SHA256_ARM64=unused FELIS_GO_SHA256="${3:-}" GOROOT_DIR="$groot" TMPDIR="$sdir" bash -c '
    die() { printf "DIE: %s\n" "$*"; exit 1; }
    log() { printf "LOG: %s\n" "$*"; }
    ok() { printf "OK: %s\n" "$*"; }
    remember_temp() { printf "TEMP: %s\n" "$1"; }
    uname() { echo x86_64; }
    curl() { while [ "$#" -gt 1 ] && [ "$1" != "-o" ]; do shift; done
      printf "stand-in go toolchain\n" > "$2"; printf "CURL: %s\n" "$2"; }
    tar() { printf "TAR: %s\n" "$*"; }
    '"$gblock"'
    install_go_toolchain'
}

mkdir -p "$groot" && : > "$groot/KEEP"
out="$(run_go 1.26.4 deadbeef)"
expect "a Go download that does not match the pin is refused" \
  "DIE: Go 1.26.4 (amd64) checksum mismatch: got ${gsum}, expected deadbeef" "$out"
case "$out" in
  *TAR:*) echo "FAIL a refused Go download must not be unpacked"; fails=$((fails + 1)) ;;
  *) echo "PASS a refused Go download is not unpacked" ;;
esac
if [ -e "$groot/KEEP" ]; then
  echo "PASS a refused Go download leaves the old toolchain in place"
else
  echo "FAIL a refused Go download must not remove the old toolchain"; fails=$((fails + 1))
fi

expect "an unpinned FELIS_GO_VERSION without a digest is refused" "DIE: no pinned sha256 for Go 1.99.0" \
  "$(run_go 1.99.0 "$gsum")"
expect "an unpinned FELIS_GO_VERSION installs with its own FELIS_GO_SHA256" "TAR: " \
  "$(run_go 1.99.0 deadbeef "$gsum")"

out="$(run_go 1.26.4 "$gsum")"
expect "a Go download matching the pin is unpacked" "TAR: " "$out"
gtmp="$(printf '%s\n' "$out" | sed -n 's/^TEMP: //p')"
expect "the Go download is staged in a directory the cleanup removes" \
  "CURL: ${gtmp:-<none>}/go1.26.4.linux-amd64.tar.gz" "$out"

# --- a private repo without a token fails with the hint instead of prompting -------------
# git asks for credentials on /dev/tty, where a piped install would sit waiting. Every
# network git call goes through git_auth, so the switch belongs there.

gablock="$(awk '/^git_auth\(\) \{/,/^}/' "$BS")"
[ -n "$gablock" ] || { echo "FAIL: no git_auth found in $BS"; exit 1; }
[ "$(printf '%s\n' "$gablock" | wc -l)" -lt 15 ] \
  || { echo "FAIL: the extracted block is not the function -- did its closing brace move?"; exit 1; }

run_git_auth() { # token
  FELIS_GITHUB_TOKEN="$1" bash -c '
    unset GIT_TERMINAL_PROMPT # whatever runs this harness may have set it already
    git() { printf "GIT: prompt=%s\n" "${GIT_TERMINAL_PROMPT:-<unset>}"; }
    '"$gablock"'
    git_auth clone https://example.invalid/felis.git'
}

expect "git never prompts without a token" "GIT: prompt=0" "$(run_git_auth '')"
expect "git never prompts with a token" "GIT: prompt=0" "$(run_git_auth ghp_example)"

fblock="$(awk '/^fetch_source\(\) \{/,/^}/' "$BS")"
[ -n "$fblock" ] || { echo "FAIL: no fetch_source found in $BS"; exit 1; }
[ "$(printf '%s\n' "$fblock" | wc -l)" -lt 40 ] \
  || { echo "FAIL: the extracted block is not the function -- did its closing brace move?"; exit 1; }

run_fetch() { # src-dir
  SRC_DIR="$1" FELIS_REF=main FELIS_REPO_URL=https://example.invalid/felis.git bash -c '
    die() { printf "DIE: %s\n" "$*"; exit 1; }
    log() { :; }
    ok() { :; }
    resolve_install_ref() { :; }
    stamp_version() { :; }
    git_auth() { return 128; }
    git() { :; }
    '"$fblock"'
    fetch_source'
}

expect "a failed clone names the token" "set FELIS_GITHUB_TOKEN" "$(run_fetch "$sdir/src")"
mkdir -p "$sdir/src/.git"
expect "a failed fetch into an existing checkout names the token" "set FELIS_GITHUB_TOKEN" \
  "$(run_fetch "$sdir/src")"

# ---------------------------------------------------------------------------------------
if [ "$fails" -eq 0 ]; then
  echo "ALL PASS"
else
  echo "$fails FAILED"
fi
exit "$fails"
