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

mg="$(awk '/^meta_get\(\)/,/^}/' "$BS")"
[ -n "$mg" ] || { echo "FAIL: no meta_get in $BS"; exit 1; }

rsha=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef

run_resolver() { # canned-fill-response
  CANNED="$1" bash -c '
    curl() { printf "%s" "$CANNED"; }
    '"$mg"'
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

# A TLS handshake cut mid-way (curl exit 35) is outside curl's own --retry set, so
# meta_get retries it: two failures, then the answer, still resolves.
tries="$(mktemp)"
out="$(TRIES="$tries" bash -c '
  # curl runs in a command substitution, so the attempt count lives in a file.
  curl() { printf x >> "$TRIES"; [ "$(wc -c < "$TRIES")" -ge 3 ] || return 35; printf "body"; }
  sleep() { :; }
  warn() { :; }
  '"$mg"'
  if out="$(meta_get https://fill.papermc.io/x)"; then printf "GOT %s\n" "$out"; else printf "GAVE UP\n"; fi
')"
rm -f "$tries"
expect "meta_get retries a failed handshake" "GOT body" "$out"

out="$(bash -c '
  curl() { return 35; }
  sleep() { :; }
  warn() { :; }
  '"$mg"'
  if meta_get https://fill.papermc.io/x >/dev/null; then printf "GOT\n"; else printf "GAVE UP\n"; fi
')"
expect "meta_get gives up after three attempts" "GAVE UP" "$out"

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

# --- [smtp] carry-forward does not hoard the auth_source comment block -------------------
# persisted_smtp_block used to print every line between [smtp] and the next section
# header -- which includes the generated Yggdrasil comment block that sits above
# [[auth_source]]. Each re-run re-emitted that hoard plus a fresh template copy, so both
# config files grew by one comment block per run (audit #50). The carry must be the
# section's header and keys only, and must be byte-stable when written back.

sblock="$(awk '/^persisted_smtp_block\(\) \{/,/^}/' "$BS")"
[ -n "$sblock" ] || { echo "FAIL: no persisted_smtp_block found in $BS"; exit 1; }
[ "$(printf '%s\n' "$sblock" | wc -l)" -lt 40 ] \
  || { echo "FAIL: the extracted block is not the function -- did its closing brace move?"; exit 1; }

sfn="$(mktemp)"
printf '%s\n' "$sblock" > "$sfn"
smtp_dir="$(mktemp -d)"
trap 'rm -f "$jar" "$sfn"; rm -rf "$vdir" "$sdir" "$smtp_dir"' EXIT

run_smtp() { # state-dir
  STATE_DIR="$1" SBLOCK_FILE="$sfn" bash -c '. "$SBLOCK_FILE"; persisted_smtp_block'
}

cat > "$smtp_dir/felis.host.toml" <<'TOML'
[server]
listen = "0.0.0.0:8080"

[smtp]
  host = "mail.example"
  port = 587
  from = "felis@example.net"
  username = "relay-user"
  password_ref = "smtp-password"

# Third-party Yggdrasil sources federated by the hasJoined multiplexer. Mojang is
# always the code-owned identity anchor (premium-first), prepended in Go; sources here
# append as namespace-rewritten guests. A fresh install federates LittleSkin. Edit the
# list in /etc/felis/felis.host.toml and rerun the installer; re-runs keep it as it
# is, and with no [[auth_source]] at all the server is Mojang-only.

[[auth_source]]
  tag = "littleskin"
  prefix = "LS"
  url = "https://littleskin.cn/api/yggdrasil/sessionserver/session/minecraft/hasJoined"
TOML

out="$(run_smtp "$smtp_dir")"
expect "a configured [smtp] relay is carried" 'host = "mail.example"' "$out"
expect "its port survives the carry" 'port = 587' "$out"
expect "its credentials reference survives" 'password_ref = "smtp-password"' "$out"
case "$out" in
  *"#"*)
    echo "FAIL: the carry hoards comment lines:"; printf '%s\n' "$out"; fails=$((fails + 1)) ;;
  *) echo "PASS the carry is header and keys only -- no comment hoard" ;;
esac
case "$out" in
  *"[["*)
    echo "FAIL: the carry ran into the next section:"; printf '%s\n' "$out"; fails=$((fails + 1)) ;;
  *) echo "PASS the carry stops at the next section header" ;;
esac

# Write the carry back the way write_felis_toml does (carry + one fresh template block +
# the tables) and extract again: a second re-run must add nothing.
{
  printf '%s\n' "$out"
  printf '\n%s\n' '# Third-party Yggdrasil sources federated by the hasJoined multiplexer. Mojang is'
  printf '%s\n' '[[auth_source]]' '  tag = "littleskin"' '  prefix = "LS"' \
    '  url = "https://littleskin.cn/api/yggdrasil/sessionserver/session/minecraft/hasJoined"'
} > "$smtp_dir/felis.host.toml"
out2="$(run_smtp "$smtp_dir")"
printf '%s\n' "$out" > "$smtp_dir/first"
printf '%s\n' "$out2" > "$smtp_dir/second"
if cmp -s "$smtp_dir/first" "$smtp_dir/second"; then
  echo "PASS a carried-forward [smtp] converges (a second re-run adds nothing)"
else
  echo "FAIL: carrying [smtp] is not idempotent:"; diff "$smtp_dir/first" "$smtp_dir/second" | head
  fails=$((fails + 1))
fi

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

for v in 8081 127.0.0.1 127.0.0.1:0 127.0.0.1:65536 127.0.0.1:x ::1:8081; do
  expect "listen address $v is refused" "DIE: FELIS_NANO_LISTEN" "$(check_listen "$v")"
done
for v in '[::1]:8081' '[::]:8081' :8081 0.0.0.0:8081 127.0.0.1:8081; do
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

run_summary() { # listen [proxy-cidr]
  FELIS_NANO_LISTEN="$1" FELIS_NANO_PROXY_CIDR="${2:-}" NODE_IP=203.0.113.9 STATE_DIR=/etc/felis bash -c '
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

# Loopback is what keeps the firewall shut, and hasJoined takes no token: a default that
# does not classify as loopback turns every fresh nano host into a public auth relay.
run_loopback() { # listen
  FELIS_NANO_LISTEN="$1" bash -c "$kblock"'
    if nano_listen_is_loopback; then echo LOOPBACK; else echo ROUTABLE; fi'
}

for v in 127.0.0.1:8081 127.0.0.5:8081 localhost:8081 '[::1]:8081'; do
  expect "$v is loopback" LOOPBACK "$(run_loopback "$v")"
done
for v in 0.0.0.0:8081 10.0.0.5:8081 '[::]:8081'; do
  expect "$v is not loopback" ROUTABLE "$(run_loopback "$v")"
done
ndefault="$(run_listen '' "$sdir/absent.service")"
ndefault="${ndefault#LISTEN: }"
expect "the default listen address (${ndefault:-empty}) is loopback" LOOPBACK "$(run_loopback "$ndefault")"

# --- firewalld admits the proxy alone ---------------------------------------------------
# hasJoined takes no token, so a routable bind is opened only to FELIS_NANO_PROXY_CIDR, never
# to every source, and a re-run closes the port an earlier installer opened to everyone.

cblock="$(awk '/^validate_cidr\(\) \{/,/^}/' "$BS")"
[ -n "$cblock" ] || { echo "FAIL: no validate_cidr found in $BS"; exit 1; }
[ "$(printf '%s\n' "$cblock" | wc -l)" -lt 15 ] \
  || { echo "FAIL: the extracted block is not the function -- did its closing brace move?"; exit 1; }

check_cidr() { # value
  bash -c 'die() { printf "DIE: %s\n" "$*"; exit 1; }
    '"$cblock"'
    validate_cidr FELIS_NANO_PROXY_CIDR "$1" && echo VALID' _ "$1" 2>&1
}

for v in 10.0.0.7 10.0.0.7/ /32 10.0.0.0/8/9 10.0.0.7/x '10.0.0.7/32 port' '10.0.0.7/32"'; do
  expect "proxy CIDR <$v> is refused" "DIE: FELIS_NANO_PROXY_CIDR" "$(check_cidr "$v")"
done
for v in '' 10.0.0.7/32 192.168.0.0/24 fd00::7/128; do
  expect "proxy CIDR <$v> is accepted" VALID "$(check_cidr "$v")"
done

fblock="$(awk '/^configure_nano_firewall\(\) \{/,/^}/' "$BS")"
[ -n "$fblock" ] || { echo "FAIL: no configure_nano_firewall found in $BS"; exit 1; }
[ "$(printf '%s\n' "$fblock" | wc -l)" -lt 40 ] \
  || { echo "FAIL: the extracted block is not the function -- did its closing brace move?"; exit 1; }

run_fw() { # listen proxy-cidr port-already-open(0|1)
  FELIS_NANO_LISTEN="$1" FELIS_NANO_PROXY_CIDR="$2" OPEN="$3" bash -c '
    ok() { printf "OK: %s\n" "$*"; }
    log() { printf "LOG: %s\n" "$*"; }
    warn() { printf "WARN: %s\n" "$*"; }
    systemctl() { return 0; }
    firewall-cmd() {
      case "$*" in *--query-port=*) [ "$OPEN" = 1 ]; return ;; esac
      printf "FW: %s\n" "$*"
    }
    '"$kblock"'
    '"$fblock"'
    configure_nano_firewall' 2>&1
}

no_blanket_port() { # label output
  case "$2" in
    *--add-port*) echo "FAIL $1: the port was opened to every source:"; echo "$2"; fails=$((fails + 1)) ;;
    *) echo "PASS $1" ;;
  esac
}

out="$(run_fw 0.0.0.0:8081 10.0.0.7/32 0)"
expect "a proxy CIDR opens the port to that source alone" \
  'FW: --permanent --add-rich-rule=rule family="ipv4" source address="10.0.0.7/32" port port="8081" protocol="tcp" accept' "$out"
no_blanket_port "a proxy CIDR never opens the port to every source" "$out"
expect "an IPv6 proxy CIDR gets an ipv6 rule" 'rule family="ipv6" source address="fd00::7/128"' \
  "$(run_fw '[::]:8081' fd00::7/128 0)"
out="$(run_fw 0.0.0.0:8081 '' 0)"
expect "no proxy CIDR says the port stays closed" "WARN: no FELIS_NANO_PROXY_CIDR" "$out"
no_blanket_port "no proxy CIDR opens nothing" "$out"
case "$out" in
  *--add-rich-rule*) echo "FAIL no proxy CIDR must add no rule:"; echo "$out"; fails=$((fails + 1)) ;;
  *) echo "PASS no proxy CIDR adds no rule" ;;
esac
expect "a re-run closes the port an earlier install opened to everyone" "FW: --permanent --remove-port=8081/tcp" \
  "$(run_fw 0.0.0.0:8081 10.0.0.7/32 1)"
case "$(run_fw 127.0.0.1:8081 10.0.0.7/32 1)" in
  *FW:*) echo "FAIL a loopback bind must leave firewalld alone"; fails=$((fails + 1)) ;;
  *) echo "PASS a loopback bind leaves firewalld alone" ;;
esac

expect "a routable bind with no proxy CIDR is warned about" "WARNING: bound to 10.0.0.5:8081 with no FELIS_NANO_PROXY_CIDR" \
  "$(run_summary 10.0.0.5:8081)"
expect "a routable bind with a proxy CIDR names it" "admits 8081/tcp only from" \
  "$(run_summary 10.0.0.5:8081 10.0.0.7/32)"

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
# The tarball is unpacked and run as root, so a download that does not hash to the pin is
# refused -- and refused before the working toolchain is removed.

# The function replaces whatever version sits at GOROOT_DIR, so that has to be a directory
# Felis owns, never an operator's /usr/local/go.
case "$(grep '^GOROOT_DIR=' "$BS")" in
  'GOROOT_DIR="/opt/felis/'*) echo "PASS the Go toolchain lives under /opt/felis" ;;
  *) echo "FAIL the Go toolchain must live under /opt/felis, got: $(grep '^GOROOT_DIR=' "$BS")"; fails=$((fails + 1)) ;;
esac

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

# --- default install keeps backups, and retention envs reach the renderer ----------------
# A default install must render the world-archive PVC (without one, backup/restore answer an
# honest 503), and FELIS_WORLDS_HOST_PATH must turn into the reaper's two flags or an
# operator's retention enablement silently renders no CronJob. Extracted, not retyped.

mblock="$(awk '/^  log "rendering \+ applying the control-plane bundle"/,/kube apply -f -/' "$BS")"
[ -n "$mblock" ] || { echo "FAIL: no manifest_args block found in $BS"; exit 1; }
[ "$(printf '%s\n' "$mblock" | wc -l)" -lt 40 ] \
  || { echo "FAIL: the extracted block is not the manifest_args block -- did it move?"; exit 1; }

run_bundle_flags() { # backup-pvc worlds-host-path
  FELIS_IMAGE=reg/felis:test FELIS_PANEL_NODEPORT=30443 NODE_IP=10.0.0.5 \
  FELIS_BACKUP_PVC="$1" FELIS_WORLDS_HOST_PATH="$2" FELIS_ARCHIVE_LOCAL_PATH=/var/lib/felis/archives \
  HOST_BIN=myManifests bash -c '
    log() { :; }
    warn() { printf "WARN: %s\n" "$*"; }
    kube() { cat; }
    myManifests() { printf "%s\n" "$@"; }
    setfacl() { printf "SETFACL %s\n" "$*"; }
    chmod() { printf "CHMOD %s\n" "$*"; }
    node_global_cidrs() { printf "203.0.113.7/32\n2001:db8::7/128\n"; }
    run_bundle() {
    '"$mblock"'
    }
    run_bundle'
}

out="$(run_bundle_flags felis-backups '')"
expect "a default install asks the renderer for the archive PVC" "--backup-pvc
felis-backups" "$out"
case "$out" in
  *--worlds-host-path*) echo "FAIL: no reaper flags may render without FELIS_WORLDS_HOST_PATH"; fails=$((fails + 1)) ;;
esac

out="$(run_bundle_flags '' '')"
expect "an emptied FELIS_BACKUP_PVC is the explicit no-backup shape" "--backup-pvc=" "$out"
# Game server egress excludes every private range already; the node's own public
# addresses must be excluded too or a server can dial the panel NodePort on them.
expect "every global node address is denied to game server egress (v4)" "--server-egress-deny-cidr
203.0.113.7/32" "$out"
expect "every global node address is denied to game server egress (v6)" "--server-egress-deny-cidr
2001:db8::7/128" "$out"

ngblock="$(awk '/^node_global_cidrs\(\) \{/,/^}/' "$BS")"
[ -n "$ngblock" ] || { echo "FAIL: no node_global_cidrs found in $BS"; exit 1; }
out="$(bash -c '
  ip() {
    printf "2: eth0    inet 203.0.113.7/24 brd 203.0.113.255 scope global eth0\\       valid_lft forever\n"
    printf "2: eth0    inet6 2001:db8::7/64 scope global dynamic\\       valid_lft 86000sec\n"
  }
  '"$ngblock"'
  node_global_cidrs')"
expect "a global v4 address becomes a /32" "203.0.113.7/32" "$out"
expect "a global v6 address becomes a /128" "2001:db8::7/128" "$out"
case "$out" in
  */24*|*/64*) echo "FAIL: node_global_cidrs must deny the address, not its whole subnet"; fails=$((fails + 1)) ;;
esac

out="$(run_bundle_flags felis-backups /var/lib/rancher/k3s/storage)"
expect "enabling retention passes the worlds root" "--worlds-host-path
/var/lib/rancher/k3s/storage" "$out"
expect "enabling retention passes the archive mount that must match felis.toml" "--archive-local-path
/var/lib/felis/archives" "$out"

# The warn fires only when the root is ABSENT (hostPath type Directory would fail);
# the case above passes a path that exists on any host already running k3s, so it
# must not also demand the warning — probing the real /var/lib/rancher path made
# this suite red on exactly the hosts the installer is for. Point the warn case at
# a path guaranteed missing.
missing="/tmp/felis-worlds-root-must-not-exist-$$"
out="$(run_bundle_flags felis-backups "$missing")"
expect "a missing worlds root is warned about, not silently skipped" "WARN: worlds root $missing does not exist yet" "$out"

# The reaper reads the root as root with DAC_OVERRIDE through a static PV, so an existing
# root is left exactly as k3s shipped it: uid 1000 is now the game servers' uid, and a
# traverse grant for it on the node's storage root would serve nothing but them.
wdir="$(mktemp -d)"
out="$(run_bundle_flags felis-backups "$wdir")"
case "$out" in
  *SETFACL*|*CHMOD*|*WARN*) echo "FAIL: an existing worlds root must get no grant and no warning: $out"; fails=$((fails + 1)) ;;
esac

# --- the registry mirror writer -----------------------------------------------------------
# k3s only consults registries.yaml at agent start, so a CONTENT change must restart k3s and
# an identical file (every re-run) must restart nothing. The k3s restart is the expensive,
# disruptive half of the pair -- getting the idempotence wrong bounces the whole cluster on
# every installer re-run, so both halves are pinned here against the extracted function.

cmblock="$(awk '/^configure_registry_mirror\(\) \{/,/^}/' "$BS")"
[ -n "$cmblock" ] || { echo "FAIL: no configure_registry_mirror found in $BS"; exit 1; }

run_mirror() { # scratch-file
  K3S_REGISTRIES_FILE="$1" REGISTRY_URL=registry.felis.svc:5000 REGISTRY_PUSH_HOST=127.0.0.1:5000 \
  bash -c '
    log() { printf "LOG: %s\n" "$*"; }
    ok() { printf "OK: %s\n" "$*"; }
    die() { printf "DIE: %s\n" "$*"; exit 1; }
    warn() { printf "WARN: %s\n" "$*"; }
    remember_temp() { :; }
    systemctl() { printf "SYSTEMCTL %s\n" "$*"; }
    kube() { printf "n Ready \n"; }
    wait_for_node_ready() { kube get nodes | grep -q " Ready " && ok "k3s node Ready"; }
    '"$cmblock"'
    configure_registry_mirror'
}

mfile="$(mktemp -u)"
out="$(run_mirror "$mfile")"
expect "a missing registries.yaml is written" "\"registry.felis.svc:5000\":" "$(cat "$mfile" 2>/dev/null)"
expect "the mirror endpoint is the node loopback push/pull host" "\"http://127.0.0.1:5000\"" "$(cat "$mfile" 2>/dev/null)"
expect "a content change restarts k3s" "SYSTEMCTL restart k3s" "$out"

out="$(run_mirror "$mfile")"
expect "an identical registries.yaml is recognised" "already configured" "$out"
case "$out" in
  *"SYSTEMCTL restart"*) echo "FAIL: a re-run with identical content must not restart k3s"; fails=$((fails + 1)) ;;
esac

printf 'mirrors: {}\n' >"$mfile"
out="$(run_mirror "$mfile")"
expect "changed content restarts k3s again" "SYSTEMCTL restart k3s" "$out"
rm -f "$mfile"

# --- image mirroring ----------------------------------------------------------------------
# The registry keys a repository by the path AFTER the host, so the push must swap the
# registry host for the node's loopback endpoint and nothing else. A ref outside the
# registry must be warned about, not silently pushed somewhere unintended.

pblock="$(awk '/^push_image_to_registry\(\) \{/,/^}/' "$BS")"
[ -n "$pblock" ] || { echo "FAIL: no push_image_to_registry found in $BS"; exit 1; }

run_push() { # ref [docker-push-exit]
  REF="$1" PUSH_EXIT="${2:-0}" \
  REGISTRY_URL=registry.felis.svc:5000 REGISTRY_PUSH_HOST=127.0.0.1:5000 REGISTRY_DOCKER_CONFIG=/cfg \
  bash -c '
    log() { printf "LOG: %s\n" "$*"; }
    warn() { printf "WARN: %s\n" "$*"; }
    die() { printf "DIE: %s\n" "$*"; exit 1; }
    ok() { :; }
    systemctl() { :; }
    docker() {
      printf "DOCKER %s\n" "$*"
      case " $* " in
        *" push "*) return "$PUSH_EXIT" ;;
      esac
    }
    '"$pblock"'
    push_image_to_registry "$REF"'
}

out="$(run_push registry.felis.svc:5000/felis/felis:demo)"
expect "a registry ref is re-tagged onto the node loopback endpoint" \
  "DOCKER tag registry.felis.svc:5000/felis/felis:demo 127.0.0.1:5000/felis/felis:demo" "$out"
expect "and pushed to exactly that endpoint, with the platform login" "DOCKER --config /cfg push 127.0.0.1:5000/felis/felis:demo" "$out"

out="$(run_push registry.felis.svc:50000/felis/felis:demo)"
expect "a ref outside the registry is refused with a warning" "WARN: not mirroring" "$out"
case "$out" in
  *"DOCKER push"*) echo "FAIL: a non-registry ref must not be pushed"; fails=$((fails + 1)) ;;
esac

out="$(run_push registry.felis.svc:5000/felis/felis:demo 1)"
expect "a failed push fails the install loudly" "DIE: could not mirror" "$out"

# docker must be started ONCE for the whole batch: a start/stop pair per image trips
# systemd's start rate limit ("start-limit-hit" — observed live; the 4th image was never
# mirrored because docker.service is socket-triggered and each cycle counts twice).
wiblock="$(awk '/^push_images_to_registry\(\) \{/,/^}/' "$BS")"
[ -n "$wiblock" ] || { echo "FAIL: no push_images_to_registry found in $BS"; exit 1; }
out="$(
  FELIS_IMAGE=a FELIS_LIMBO_IMAGE=b FELIS_LOBBY_IMAGE=c FELIS_PAPER_IMAGE=d bash -c '
    systemctl() { printf "SYSTEMCTL %s\n" "$*"; }
    push_image_to_registry() { printf "PUSH %s\n" "$1"; }
    push_version_tag() { printf "VERSION %s\n" "$1"; }
    registry_docker_login() { printf "LOGIN\n"; }
    '"$wiblock"'
    push_images_to_registry'
)"
expect "the batch logs in to the registry gate before pushing" "SYSTEMCTL start docker
LOGIN
PUSH a" "$out"
starts="$(printf '%s\n' "$out" | grep -c 'SYSTEMCTL start docker')"
stops="$(printf '%s\n' "$out" | grep -c 'SYSTEMCTL stop docker')"
[ "$starts" = 1 ] && [ "$stops" = 1 ] && [ "$(printf '%s\n' "$out" | grep -c '^PUSH')" = 4 ] \
  && echo "PASS the batch wraps all four pushes in ONE docker start/stop" \
  || { echo "FAIL: expected 1 start / 1 stop / 4 pushes, got:"; printf '%s\n' "$out"; fails=$((fails + 1)); }
[ "$(printf '%s\n' "$out" | grep '^VERSION' | tr '\n' ' ')" = "VERSION b VERSION c VERSION d " ] \
  && echo "PASS the three game images, and only they, also get a version tag" \
  || { echo "FAIL: expected version tags for b c d only, got:"; printf '%s\n' "$out"; fails=$((fails + 1)); }

# Each game build is also mirrored under <MC version>-<image id>, a tag no later run
# rewrites, so an admin can still name that exact build after :demo moves on.
vtblock="$(awk '/^push_version_tag\(\) \{/,/^}/' "$BS")"
[ -n "$vtblock" ] || { echo "FAIL: no push_version_tag found in $BS"; exit 1; }
run_version_tag() { # MC_VERSION
  MC_VERSION="$1" bash -c '
    die() { printf "DIE: %s\n" "$*"; exit 1; }
    docker() {
      case "$1" in
        image) printf "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n" ;;
        *) printf "DOCKER %s\n" "$*" ;;
      esac
    }
    push_image_to_registry() { printf "PUSH %s\n" "$1"; }
    '"$vtblock"'
    push_version_tag registry.felis.svc:5000/felis/paper:demo'
}
out="$(run_version_tag 26.2)"
expect "a game build is pushed under its version tag" "PUSH registry.felis.svc:5000/felis/paper:26.2-0123456789ab" "$out"
expect "the version tag is created from the built image" "DOCKER tag registry.felis.svc:5000/felis/paper:demo registry.felis.svc:5000/felis/paper:26.2-0123456789ab" "$out"
case "$(run_version_tag '')" in
  *PUSH*) echo "FAIL: no Minecraft version, no version tag"; fails=$((fails + 1)) ;;
  *) echo "PASS without a resolved Minecraft version no version tag is pushed" ;;
esac

# The registry refuses anonymous writes, and the platform token must never reach
# docker's argv (ps) or root's ~/.docker: stdin into a throwaway --config dir.
lblock="$(awk '/^registry_docker_login\(\) \{/,/^}/' "$BS")"
[ -n "$lblock" ] || { echo "FAIL: no registry_docker_login found in $BS"; exit 1; }
calls="$(mktemp)"
out="$(
  CALLS="$calls" REGISTRY_PLATFORM_TOKEN=s3cret REGISTRY_PUSH_HOST=127.0.0.1:5000 bash -c '
    die() { printf "DIE: %s\n" "$*"; exit 1; }
    remember_temp() { printf "TEMP %s\n" "$1"; }
    docker() { printf "DOCKER %s STDIN=%s\n" "$*" "$(cat)" >>"$CALLS"; }
    '"$lblock"'
    registry_docker_login
    rm -rf "$REGISTRY_DOCKER_CONFIG"'
)$(printf '\n'; cat "$calls")"
rm -f "$calls"
expect "the installer logs in as the platform principal via stdin" "login --username platform --password-stdin 127.0.0.1:5000 STDIN=s3cret" "$out"
case "$(printf '%s\n' "$out" | grep '^DOCKER')" in
  *"DOCKER --config /"*) echo "PASS the login writes a throwaway docker config" ;;
  *) echo "FAIL: registry_docker_login must use a --config temp dir, got: $out"; fails=$((fails + 1)) ;;
esac
case "$out" in
  *"--password s3cret"*|*"-p s3cret"*) echo "FAIL: the registry token reached docker argv"; fails=$((fails + 1)) ;;
esac
expect "the throwaway config is registered for EXIT cleanup" "TEMP /" "$out"

# The registry pod's two images can only come from containerd's own store: pin
# both against kubelet image GC, and unpin a previous felis tag.
pnblock="$(awk '/^pin_registry_images\(\) \{/,/^}/' "$BS")"
[ -n "$pnblock" ] || { echo "FAIL: no pin_registry_images found in $BS"; exit 1; }
calls="$(mktemp)"
out="$(
  CALLS="$calls" FELIS_IMAGE=registry.felis.svc:5000/felis/felis:v2 bash -c '
    ok() { printf "OK: %s\n" "$*"; }
    warn() { printf "WARN: %s\n" "$*"; }
    k3s_cmd() {
      case "$*" in
        "ctr images ls -q") printf "registry.felis.svc:5000/felis/felis:v1\nregistry.felis.svc:5000/felis/felis:v2\ndocker.io/library/registry:2\nregistry.felis.svc:5000/felis/limbo:demo\n" ;;
        *) printf "CTR %s\n" "$*" >>"$CALLS" ;;
      esac
    }
    '"$pnblock"'
    pin_registry_images'
)$(printf '\n'; cat "$calls")"
rm -f "$calls"
expect "the running felis image is pinned" "CTR ctr images label registry.felis.svc:5000/felis/felis:v2 io.cri-containerd.pinned=pinned" "$out"
expect "registry:2 is pinned" "CTR ctr images label docker.io/library/registry:2 io.cri-containerd.pinned=pinned" "$out"
expect "a previous felis tag is unpinned" "CTR ctr images label registry.felis.svc:5000/felis/felis:v1 io.cri-containerd.pinned=" "$out"
case "$out" in
  *"limbo:demo io.cri"*) echo "FAIL: only the registry pod's images may be pinned or unpinned"; fails=$((fails + 1)) ;;
esac

# User servers still on a bare registry tag are pinned to the build it names BEFORE
# this run builds and pushes new ones over it; a fresh install has nothing to pin.
puiblock="$(awk '/^pin_user_server_images\(\) \{/,/^}/' "$BS")"
[ -n "$puiblock" ] || { echo "FAIL: no pin_user_server_images found in $BS"; exit 1; }
run_pin_user() { # crd-present(0/1) pin-exit
  CALLS="$calls" CRD="$1" PIN_EXIT="$2" HOST_BIN=felis CONTROL_NS=felis MINECRAFT_NS=minecraft \
    REGISTRY_URL=registry.felis.svc:5000 REGISTRY_PUSH_HOST=127.0.0.1:5000 bash -c '
    ok() { printf "OK: %s\n" "$*"; }
    warn() { printf "WARN: %s\n" "$*"; }
    kube() {
      case "$*" in
        "get crd"*) [ "$CRD" = 1 ] ;;
        *) printf "KUBE %s\n" "$*" >>"$CALLS" ;;
      esac
    }
    felis() { printf "FELIS %s\n" "$*"; return "$PIN_EXIT"; }
    '"$puiblock"'
    pin_user_server_images'
}
calls="$(mktemp)"
case "$(run_pin_user 0 0)" in
  *FELIS*) echo "FAIL: without the CRD there is nothing to pin"; fails=$((fails + 1)) ;;
  *) echo "PASS a fresh install skips pinning" ;;
esac
out="$(run_pin_user 1 0)$(printf '\n'; cat "$calls")"
expect "pinning reaches the registry through its loopback hostPort" "FELIS pin-images --namespace minecraft --registry registry.felis.svc:5000 --endpoint 127.0.0.1:5000" "$out"
expect "pinning waits for the registry first" "KUBE -n felis rollout status deployment/registry" "$out"
out="$(run_pin_user 1 1)"
expect "a failed pin warns with the consequence" "WARN: could not pin every user server" "$out"
rm -f "$calls"

mainblock="$(awk '/^main\(\) \{/,/^}/' "$BS")"
line_of() { printf '%s\n' "$mainblock" | grep -n "^  $1\$" | head -n 1 | cut -d: -f1; }
p="$(line_of pin_user_server_images)"; b="$(line_of build_game_stack)"; u="$(line_of push_images_to_registry)"
[ -n "$p" ] && [ -n "$b" ] && [ -n "$u" ] && [ "$p" -lt "$b" ] && [ "$b" -lt "$u" ] \
  && echo "PASS user servers are pinned before the game images are rebuilt and pushed" \
  || { echo "FAIL: main must run pin_user_server_images before build_game_stack and push_images_to_registry (lines: $p $b $u)"; fails=$((fails + 1)); }

# --- the registry's own image must not be re-pulled on every run --------------------------
iblock="$(awk '/^import_registry_image\(\) \{/,/^}/' "$BS")"
[ -n "$iblock" ] || { echo "FAIL: no import_registry_image found in $BS"; exit 1; }

out="$(
  bash -c '
    log() { printf "LOG: %s\n" "$*"; }
    ok() { printf "OK: %s\n" "$*"; }
    warn() { printf "WARN: %s\n" "$*"; }
    die() { printf "DIE: %s\n" "$*"; exit 1; }
    systemctl() { :; }
    k3s_cmd() { case "$*" in "ctr images ls -q") printf "docker.io/library/registry:2\n" ;; esac; }
    docker() { printf "DOCKER %s\n" "$*"; return 1; }
    '"$iblock"'
    import_registry_image'
)"
expect "an already-imported registry:2 is left alone" "already in k3s containerd" "$out"
case "$out" in
  *DOCKER*) echo "FAIL: a present registry:2 must not trigger a docker pull"; fails=$((fails + 1)) ;;
esac

# --- installer re-runs refresh the workload namespace's felis-config copy ---------------
# The backup/restore/fileedit Jobs and the reaper mount the workload namespace's own
# felis-config (a secretKeyRef is namespace-local). `felis setup` makes that replica
# create-if-absent -- right for credentials, wrong for a rendered config -- so the
# installer must refresh it every run; a stale copy keeps old DB/archive settings.

fcblock="$(awk '/^apply_felis_config_secrets\(\) \{/,/^}/' "$BS")"
[ -n "$fcblock" ] || { echo "FAIL: no apply_felis_config_secrets found in $BS"; exit 1; }
[ "$(printf '%s\n' "$fcblock" | wc -l)" -lt 20 ] \
  || { echo "FAIL: the extracted block is not the function -- did its closing brace move?"; exit 1; }

kubcalls="$(mktemp)"
run_fc() {
  : > "$kubcalls"
  CONTROL_NS=felis MINECRAFT_NS=minecraft STATE_DIR=/tmp/fc KUBCALLS="$kubcalls" bash -c '
    kube() { printf "%s\n" "$*" >> "$KUBCALLS"; }
    '"$fcblock"'
    apply_felis_config_secrets'
  cat "$kubcalls"
}
out="$(run_fc)"
expect "the control plane's felis-config is applied" \
  "-n felis create secret generic felis-config" "$out"
expect "the workload namespace's copy is applied too" \
  "-n minecraft create secret generic felis-config" "$out"
expect "both copies render from the pod config" \
  "felis.toml=/tmp/fc/felis.pod.toml" "$out"
applies="$(printf '%s\n' "$out" | grep -c '^apply -f -$')"
if [ "$applies" -eq 2 ]; then
  echo "PASS both rendered copies are piped to kubectl apply"
else
  echo "FAIL: expected 2 applies, got $applies:"; printf '%s\n' "$out"; fails=$((fails + 1))
fi
rm -f "$kubcalls"

# --- installer re-runs keep the operator's [registry] overrides --------------------------
# §15's upgrade path is re-running the installer, but the build-lane mirrors and the
# uploads backend live in [registry] as hand-written keys (docs/troubleshooting.md §8e or
# the storage wizard) that nothing in this script's inputs derives. A re-run must carry
# them forward — without letting a stale url/build_namespace survive (installer-owned).

wrblock="$(awk '/^write_felis_toml\(\) \{/,/^}/' "$BS")"
prblock="$(awk '/^persisted_registry_block\(\) \{/,/^}/' "$BS")"
pablock="$(awk '/^persisted_archive_block\(\) \{/,/^}/' "$BS")"
poblock="$(awk '/^persisted_offsite_block\(\) \{/,/^}/' "$BS")"
oblock="$(awk '/^offsite_block\(\) \{/,/^}/' "$BS")"
{ [ -n "$wrblock" ] && [ -n "$prblock" ] && [ -n "$pablock" ] && [ -n "$poblock" ] && [ -n "$oblock" ]; } \
  || { echo "FAIL: write_felis_toml / persisted_{registry,archive,offsite}_block / offsite_block not found in $BS"; exit 1; }
# The blocks quote themselves (the awk program uses single quotes), so they are
# sourced from a file instead of being spliced into a single-quoted bash -c.
fnfile="$(mktemp)"
printf '%s\n%s\n%s\n%s\n%s\n' "$prblock" "$pablock" "$poblock" "$oblock" "$wrblock" > "$fnfile"

rdir="$(mktemp -d)"
cat > "$rdir/felis.host.toml" <<'TOML'
[registry]
url = "stale.invalid:5000"
build_namespace = "stale-ns"
kaniko_image = "registry.felis.svc:5000/mirror/kaniko-executor:v1.24.0"
trivy_db_repository = "registry.felis.svc:5000/mirror/trivy-db:2"
trivy_java_db_repository = "registry.felis.svc:5000/mirror/trivy-java-db:1"
build_disk_limit = "20Gi"
build_user_namespaces = "off"
build_runtime_class = "gvisor"

[registry.s3]
endpoint = "https://s3.example"
region = "us-east-1"

[archive]
store = "tarLocal"
local_path = "/stale/path"
retention = "30d"
manual_keep = 3
manual_cooldown = "1h"

[offsite]
endpoint = "https://objects.example"
bucket = "felis-offsite"
TOML

run_write() { # out-file
  STATE_DIR="$rdir" OUT_TOML="$1" FNFILE="$fnfile" bash -c '
    log() { :; }
    persisted_smtp_block() { :; }
    persisted_auth_source_blocks() { :; }
    . "$FNFILE"
    FELIS_ROOT_DOMAIN=r.example.com DB_USER=u DB_PASSWORD=p DB_NAME=d MINECRAFT_NS=minecraft \
    FELIS_EGRESS_MODE=nodeport FELIS_LIMBO_IMAGE=li FELIS_LOBBY_IMAGE=lo \
    REGISTRY_URL=registry.felis.svc:5000 BUILD_NS=felis-build FELIS_ARCHIVE_LOCAL_PATH=/a \
    FELIS_OFFSITE_BUCKET= write_felis_toml "$OUT_TOML" 127.0.0.1'
}

run_write "$rdir/out.toml"
out="$(cat "$rdir/out.toml")"
expect "a re-run carries the build-lane executor mirrors" \
  'kaniko_image = "registry.felis.svc:5000/mirror/kaniko-executor:v1.24.0"' "$out"
expect "a re-run carries the trivy vulnerability-DB mirror" \
  'trivy_db_repository = "registry.felis.svc:5000/mirror/trivy-db:2"' "$out"
expect "a re-run carries the trivy java-DB mirror" \
  'trivy_java_db_repository = "registry.felis.svc:5000/mirror/trivy-java-db:1"' "$out"
expect "a re-run carries the build disk cap" 'build_disk_limit = "20Gi"' "$out"
expect "a re-run carries the build user-namespace mode" 'build_user_namespaces = "off"' "$out"
expect "a re-run carries the build runtime class" 'build_runtime_class = "gvisor"' "$out"
expect "a re-run carries the [registry.s3] uploads subtable" "[registry.s3]" "$out"
expect "the carried subtable keeps its keys" 'endpoint = "https://s3.example"' "$out"
expect "url stays installer-owned" 'url = "registry.felis.svc:5000"' "$out"
expect "a re-run carries the archive retention window" 'retention = "30d"' "$out"
expect "a re-run carries the on-demand backup count" 'manual_keep = 3' "$out"
expect "a re-run carries the on-demand backup cooldown" 'manual_cooldown = "1h"' "$out"
expect "the archive mount stays installer-owned" 'local_path = "/a"' "$out"
expect "a re-run keeps the off-site bucket, set apart from the next section" '[offsite]
endpoint = "https://objects.example"
bucket = "felis-offsite"

[auth]' "$out"
case "$out" in
  *stale.invalid* | *stale-ns* | *stale/path*)
    echo "FAIL: stale installer-owned values survived the re-run"; fails=$((fails + 1)) ;;
esac

cp "$rdir/out.toml" "$rdir/felis.host.toml"
run_write "$rdir/out2.toml"
if cmp -s "$rdir/out.toml" "$rdir/out2.toml"; then
  echo "PASS a carried-forward config converges (the second re-run is a no-op)"
else
  echo "FAIL: carrying [registry] overrides is not idempotent"
  diff "$rdir/out.toml" "$rdir/out2.toml" | head
  fails=$((fails + 1))
fi

rm -f "$fnfile"

# --- database backups: the pre-migration snapshot and the daily timer --------------------
# Migrations only roll forward, so an upgrade must hand `migrate up` the snapshot directory,
# and only an explicit FELIS_PRE_MIGRATE_BACKUP=0 may take that away.

mblock="$(awk '/^run_migrations\(\) \{/,/^}/' "$BS")"
[ -n "$mblock" ] || { echo "FAIL: no run_migrations found in $BS"; exit 1; }
[ "$(printf '%s\n' "$mblock" | wc -l)" -lt 30 ] \
  || { echo "FAIL: the extracted block is not run_migrations -- did its closing brace move?"; exit 1; }

run_migrate() { # FELIS_PRE_MIGRATE_BACKUP
  FELIS_PRE_MIGRATE_BACKUP="$1" FELIS_DB_BACKUP_DIR=/var/lib/felis/db-backups STATE_DIR=/etc/felis \
    HOST_BIN=fakefelis bash -c '
    log() { :; }; ok() { :; }; warn() { printf "WARN: %s\n" "$*"; }
    write_felis_toml() { :; }; ensure_default_config() { :; }
    fakefelis() { printf "RUN: %s\n" "$*"; }
    '"$mblock"'
    run_migrations' 2>&1
}

out="$(run_migrate 1)"
expect "an upgrade snapshots into the backup dir" "RUN: migrate up -config /etc/felis/felis.host.toml -backup-dir /var/lib/felis/db-backups" "$out"
out="$(run_migrate 0)"
expect "FELIS_PRE_MIGRATE_BACKUP=0 opts out explicitly" "RUN: migrate up -config /etc/felis/felis.host.toml -no-backup" "$out"
expect "the opt-out is loud" "WARN: FELIS_PRE_MIGRATE_BACKUP=0" "$out"

tblock="$(awk '/^install_db_backup_timer\(\) \{/,/^}/' "$BS")"
[ -n "$tblock" ] || { echo "FAIL: no install_db_backup_timer found in $BS"; exit 1; }
[ "$(printf '%s\n' "$tblock" | wc -l)" -lt 60 ] \
  || { echo "FAIL: the extracted block is not install_db_backup_timer -- did its closing brace move?"; exit 1; }

tdir="$(mktemp -d)"
run_timer() { # exit status of the first backup
  FIRST="$1" DB_BACKUP_SERVICE="$tdir/felis-db-backup.service" DB_BACKUP_TIMER="$tdir/felis-db-backup.timer" \
    FELIS_DB_BACKUP_DIR="$tdir/db-backups" FELIS_DB_BACKUP_KEEP=7 FELIS_DB_BACKUP_TIME='*-*-* 04:00:00' \
    FELIS_DB_BACKUP_METRICS=/var/lib/node_exporter/textfile_collector/felis_db_backup.prom \
    HOST_BIN=/usr/local/bin/felis STATE_DIR=/etc/felis bash -c '
    ok() { printf "OK: %s\n" "$*"; }; warn() { printf "WARN: %s\n" "$*"; }
    systemctl() { printf "SYSTEMCTL: %s\n" "$*"; [ "$1" != start ] || return "$FIRST"; }
    journalctl() { printf "JOURNAL: pg_dump: connection refused\n"; }
    '"$tblock"'
    install_db_backup_timer' 2>&1
}

out="$(run_timer 0)"
unit="$(cat "$tdir/felis-db-backup.service")"
timer="$(cat "$tdir/felis-db-backup.timer")"
expect "the unit runs a daily-labelled backup with the configured retention" \
  "ExecStart=/usr/local/bin/felis db backup -config /etc/felis/felis.host.toml -dir $tdir/db-backups -label daily -keep 7 -metrics-file /var/lib/node_exporter/textfile_collector/felis_db_backup.prom" "$unit"
expect "the timer fires at the configured time" "OnCalendar=*-*-* 04:00:00" "$timer"
expect "a missed run (host off at 03:30) catches up at boot" "Persistent=true" "$timer"
expect "the timer is enabled" "SYSTEMCTL: enable --now felis-db-backup.timer" "$out"
expect "the first backup runs during the install" "SYSTEMCTL: start felis-db-backup.service" "$out"
expect "a working first backup is reported" "OK: database backups: daily" "$out"
if [ "$(stat -c %a "$tdir/db-backups" 2>/dev/null || stat -f %Lp "$tdir/db-backups")" = 700 ]; then
  echo "PASS the backup directory is private"
else
  echo "FAIL the backup directory must be 0700"; fails=$((fails + 1))
fi

out="$(run_timer 1)"
expect "a failed first backup shows its log" "JOURNAL: pg_dump: connection refused" "$out"
expect "a failed first backup is a loud warning" "WARN: the first database backup failed" "$out"
rm -rf "$tdir"

wblock="$(awk '/^install_watchdog_timer\(\) \{/,/^}/' "$BS")"
[ -n "$wblock" ] || { echo "FAIL: no install_watchdog_timer found in $BS"; exit 1; }
[ "$(printf '%s\n' "$wblock" | wc -l)" -lt 60 ] \
  || { echo "FAIL: the extracted block is not install_watchdog_timer -- did its closing brace move?"; exit 1; }
qblock="$(awk '/^quiet_watchdog\(\) \{/,/^}/' "$BS")"
[ -n "$qblock" ] || { echo "FAIL: no quiet_watchdog found in $BS"; exit 1; }

tdir="$(mktemp -d)"
run_watchdog_timer() { # $1: exit status of the first run, $2: FELIS_WORLDS_HOST_PATH
  FIRST="$1" FELIS_WORLDS_HOST_PATH="$2" WATCHDOG_SERVICE="$tdir/felis-watchdog.service" WATCHDOG_TIMER="$tdir/felis-watchdog.timer" \
    WATCHDOG_STATE="$tdir/watchdog/state.json" WATCHDOG_QUIET_FILE=/run/felis/watchdog-quiet-until \
    FELIS_DB_BACKUP_DIR=/var/lib/felis/db-backups FELIS_ARCHIVE_LOCAL_PATH=/var/lib/felis/archives FELIS_GAME_PORT=25577 \
    HOST_BIN=/usr/local/bin/felis STATE_DIR=/etc/felis bash -c '
    set -Eeuo pipefail
    ok() { printf "OK: %s\n" "$*"; }; warn() { printf "WARN: %s\n" "$*"; }
    systemctl() { printf "SYSTEMCTL: %s\n" "$*"; [ "$1" != start ] || return "$FIRST"; }
    journalctl() { printf "JOURNAL: parse /etc/felis/felis.host.toml\n"; }
    '"$wblock"'
    install_watchdog_timer' 2>&1
}

out="$(run_watchdog_timer 0 "")"
unit="$(cat "$tdir/felis-watchdog.service")"
timer="$(cat "$tdir/felis-watchdog.timer")"
expect "the watchdog runs the host binary against the host config, dialing the proxy's port" \
  "ExecStart=/usr/local/bin/felis watchdog -config /etc/felis/felis.host.toml -state $tdir/watchdog/state.json -quiet-file /run/felis/watchdog-quiet-until -backup-dir /var/lib/felis/db-backups -proxy-addr 127.0.0.1:25577 -disk-paths /,/var/lib/rancher/k3s,/var/lib/postgresql,/var/lib/felis,/var/lib/felis/archives,/var/lib/felis/db-backups" "$unit"
expect "a wedged run is killed before the next one is due twice over" "TimeoutStartSec=3min" "$unit"
expect "the watchdog runs every two minutes" "OnUnitActiveSec=2min" "$timer"
expect "the watchdog starts soon after boot" "OnBootSec=3min" "$timer"
expect "the watchdog timer is enabled" "SYSTEMCTL: enable --now felis-watchdog.timer" "$out"
expect "the first watchdog run happens during the install" "SYSTEMCTL: start felis-watchdog.service" "$out"
expect "a working first run is reported" "OK: watchdog: checks every 2 minutes" "$out"
if [ "$(stat -c %a "$tdir/watchdog" 2>/dev/null || stat -f %Lp "$tdir/watchdog")" = 700 ]; then
  echo "PASS the watchdog state directory is private (it caches the relay password)"
else
  echo "FAIL the watchdog state directory must be 0700"; fails=$((fails + 1))
fi

out="$(run_watchdog_timer 0 /srv/worlds)"
expect "a custom worlds root is watched for free space" "-disk-paths /,/var/lib/rancher/k3s,/var/lib/postgresql,/var/lib/felis,/srv/worlds," "$(cat "$tdir/felis-watchdog.service")"

out="$(run_watchdog_timer 1 "")"
expect "a failed first watchdog run shows its log" "JOURNAL: parse /etc/felis/felis.host.toml" "$out"
expect "a failed first watchdog run is a loud warning" "WARN: the first watchdog run failed" "$out"

out="$(WATCHDOG_QUIET_FILE="$tdir/run/quiet" bash -c '
  set -Eeuo pipefail
  '"$qblock"'
  quiet_watchdog; cat "$WATCHDOG_QUIET_FILE"; date +%s' 2>&1)"
until_ts="$(printf '%s\n' "$out" | sed -n 1p)"; now_ts="$(printf '%s\n' "$out" | sed -n 2p)"
if [ -n "$until_ts" ] && [ "$((until_ts - now_ts))" -ge 3600 ] && [ "$((until_ts - now_ts))" -le 7200 ]; then
  echo "PASS the install quiets the watchdog for a bounded while"
else
  echo "FAIL quiet_watchdog wrote '$until_ts' at $now_ts, want now+1h..2h"; fails=$((fails + 1))
fi
rm -rf "$tdir"

cblock="$(awk '/^cleanup\(\) \{/,/^}/' "$BS")"
case "$cblock" in
  *'rm -f -- "$WATCHDOG_QUIET_FILE"'*) echo "PASS the installer lifts the watchdog's quiet period on exit" ;;
  *) echo "FAIL cleanup must remove WATCHDOG_QUIET_FILE, or a failed install stays silent"; fails=$((fails + 1)) ;;
esac
main_block="$(awk '/^main\(\) \{/,/^}/' "$BS")"
case "$main_block" in
  *quiet_watchdog*pause_package_background_timers*install_db_backup_timer*install_watchdog_timer*mark_bootstrap_done*)
    echo "PASS main quiets the watchdog first and installs it last" ;;
  *) echo "FAIL main must call quiet_watchdog before any restart and install_watchdog_timer after the backup timer"; fails=$((fails + 1)) ;;
esac
case "$main_block" in
  *load_or_make_secrets*configure_offsite*run_migrations*install_db_backup_timer*install_offsite_timer*install_watchdog_timer*)
    echo "PASS main configures the off-site copy before the toml is written and starts it after the first backup" ;;
  *) echo "FAIL main must call configure_offsite before run_migrations and install_offsite_timer between the backup and watchdog timers"; fails=$((fails + 1)) ;;
esac

# --- the off-site copy: [offsite], its secrets file, the hourly timer ---------------------
# Without it every backup is on one disk. A re-run must keep the bucket, the encryption key
# must never change under objects sealed with the old one, and an install without a bucket
# must say so loudly.

ofile="$(mktemp)"
for fn in validate_offsite_settings persisted_offsite_block offsite_block offsite_enabled configure_offsite install_offsite_timer summary_offsite; do
  blk="$(awk "/^${fn}\\(\\) \\{/,/^}/" "$BS")"
  [ -n "$blk" ] || { echo "FAIL: no ${fn} found in $BS"; exit 1; }
  [ "$(printf '%s\n' "$blk" | wc -l)" -lt 90 ] \
    || { echo "FAIL: the extracted block is not ${fn} -- did its closing brace move?"; exit 1; }
  printf '%s\n' "$blk" >> "$ofile"
done

odir="$(mktemp -d)"
run_offsite() { # script; runs with the off-site functions sourced
  STATE_DIR="$odir" OFFSITE_ENV="$odir/offsite.env" OFFSITE_SERVICE="$odir/felis-offsite.service" \
    OFFSITE_TIMER="$odir/felis-offsite.timer" FNFILE="$ofile" HOST_BIN=fakefelis \
    FELIS_DB_BACKUP_DIR=/var/lib/felis/db-backups FELIS_BACKUP_PVC=felis-backups bash -c '
    set -Eeuo pipefail
    die() { printf "DIE: %s\n" "$*"; exit 1; }
    log() { printf "LOG: %s\n" "$*"; }; ok() { printf "OK: %s\n" "$*"; }; warn() { printf "WARN: %s\n" "$*"; }
    systemctl() { printf "SYSTEMCTL: %s\n" "$*" >&2; }
    fakefelis() { printf "RUN: %s\n" "$*" >&2; [ -z "${CHECK_FAILS:-}" ] || { echo "bucket: access denied" >&2; return 1; }; }
    FELIS_OFFSITE_ENDPOINT="${FELIS_OFFSITE_ENDPOINT:-}" FELIS_OFFSITE_BUCKET="${FELIS_OFFSITE_BUCKET:-}"
    FELIS_OFFSITE_REGION="${FELIS_OFFSITE_REGION:-}" FELIS_OFFSITE_PREFIX="${FELIS_OFFSITE_PREFIX:-}"
    FELIS_OFFSITE_DB_KEEP="${FELIS_OFFSITE_DB_KEEP:-}"
    . "$FNFILE"
    '"$1" 2>&1
}

out="$(FELIS_OFFSITE_BUCKET=b run_offsite validate_offsite_settings)"
expect "a bucket without an endpoint is refused" "DIE: FELIS_OFFSITE_BUCKET needs FELIS_OFFSITE_ENDPOINT" "$out"
out="$(FELIS_OFFSITE_ENDPOINT=https://s3.example FELIS_OFFSITE_BUCKET='b"x' run_offsite validate_offsite_settings)"
expect "a quote cannot reach the generated toml" "DIE: FELIS_OFFSITE_BUCKET must not contain quotes" "$out"
out="$(FELIS_OFFSITE_ENDPOINT=https://s3.example FELIS_OFFSITE_BUCKET=b FELIS_OFFSITE_SECRET_KEY="se'cret" run_offsite validate_offsite_settings)"
expect "a quote cannot reach the secrets file" "DIE: FELIS_OFFSITE_SECRET_KEY must not contain quotes" "$out"
out="$(FELIS_OFFSITE_ENDPOINT=https://s3.example FELIS_OFFSITE_BUCKET=b/sub run_offsite validate_offsite_settings)"
expect "a path in the bucket name is refused" "DIE: FELIS_OFFSITE_BUCKET is a bucket name" "$out"
out="$(FELIS_OFFSITE_ENDPOINT=https://s3.example run_offsite validate_offsite_settings)"
expect "an endpoint without a bucket is refused" "DIE: FELIS_OFFSITE_* is set without FELIS_OFFSITE_BUCKET" "$out"
out="$(FELIS_OFFSITE_ENDPOINT=https://s3.example FELIS_OFFSITE_BUCKET=b FELIS_OFFSITE_DB_KEEP=0 run_offsite validate_offsite_settings)"
expect "db_keep must be positive" "DIE: FELIS_OFFSITE_DB_KEEP must be a positive number" "$out"
out="$(run_offsite 'validate_offsite_settings; echo fine')"
expect "no off-site settings is a valid install" "fine" "$out"

out="$(FELIS_OFFSITE_ENDPOINT=https://s3.example FELIS_OFFSITE_BUCKET=felis FELIS_OFFSITE_PREFIX=host1 run_offsite offsite_block)"
expect "the environment writes [offsite]" '[offsite]
endpoint = "https://s3.example"
bucket = "felis"
prefix = "host1"' "$out"

cat > "$odir/felis.host.toml" <<'TOML'
[archive]
store = "tarLocal"

[offsite]
endpoint = "https://s3.example"
bucket = "kept"
key_ref = "MY_KEY"

[auth]
admin_hostname = "x"
TOML
out="$(run_offsite offsite_block)"
expect "a re-run keeps the configured bucket" 'bucket = "kept"' "$out"
expect "a re-run keeps the key reference" 'key_ref = "MY_KEY"' "$out"
case "$out" in
  *admin_hostname*) echo "FAIL the carried [offsite] swallowed the next section"; fails=$((fails + 1)) ;;
  *) echo "PASS the carried [offsite] stops at the next section" ;;
esac

# First configured install: the key is generated, the file is private, the key is shown once.
rm -f "$odir/felis.host.toml"
out="$(FELIS_OFFSITE_ENDPOINT=https://s3.example FELIS_OFFSITE_BUCKET=felis \
  FELIS_OFFSITE_ACCESS_KEY=AK FELIS_OFFSITE_SECRET_KEY=SK run_offsite 'configure_offsite; summary_offsite')"
envf="$(cat "$odir/offsite.env" 2>/dev/null)"
expect "the access key is kept for the unit" "FELIS_OFFSITE_ACCESS_KEY='AK'" "$envf"
expect "an encryption key is generated" "FELIS_OFFSITE_KEY='" "$envf"
key="$(sed -n "s/^FELIS_OFFSITE_KEY='\(.*\)'$/\1/p" "$odir/offsite.env")"
if [ "$(printf '%s' "$key" | base64 -d 2>/dev/null | wc -c | tr -d ' ')" = 32 ]; then
  echo "PASS the generated key is 32 random bytes in base64"
else
  echo "FAIL the generated key '$key' is not 32 bytes of base64"; fails=$((fails + 1))
fi
if [ "$(stat -c %a "$odir/offsite.env" 2>/dev/null || stat -f %Lp "$odir/offsite.env")" = 600 ]; then
  echo "PASS the off-site secrets file is private"
else
  echo "FAIL offsite.env must be 0600"; fails=$((fails + 1))
fi
expect "a new key is shown once, with the warning to keep it elsewhere" "WARN:     FELIS_OFFSITE_KEY=${key}" "$out"

# A re-run with new credentials keeps the key; a different key is refused.
out="$(FELIS_OFFSITE_ENDPOINT=https://s3.example FELIS_OFFSITE_BUCKET=felis \
  FELIS_OFFSITE_ACCESS_KEY=AK2 run_offsite 'configure_offsite; summary_offsite')"
expect "rotated credentials replace the old ones" "FELIS_OFFSITE_ACCESS_KEY='AK2'" "$(cat "$odir/offsite.env")"
expect "the secret key the re-run did not give is kept" "FELIS_OFFSITE_SECRET_KEY='SK'" "$(cat "$odir/offsite.env")"
expect "the key survives a re-run" "FELIS_OFFSITE_KEY='${key}'" "$(cat "$odir/offsite.env")"
case "$out" in
  *"FELIS_OFFSITE_KEY="*) echo "FAIL a re-run printed the key again"; fails=$((fails + 1)) ;;
  *) echo "PASS a re-run does not print the key again" ;;
esac
out="$(FELIS_OFFSITE_ENDPOINT=https://s3.example FELIS_OFFSITE_BUCKET=felis \
  FELIS_OFFSITE_KEY=c29tZXRoaW5nIGVsc2UgZW50aXJlbHkgZGlmZmVyZW50IQ== run_offsite configure_offsite)"
expect "a different key is refused" "DIE: FELIS_OFFSITE_KEY differs from the key in" "$out"
expect "the refusal leaves the key alone" "FELIS_OFFSITE_KEY='${key}'" "$(cat "$odir/offsite.env")"

rm -f "$odir/offsite.env"
out="$(FELIS_OFFSITE_ENDPOINT=https://s3.example FELIS_OFFSITE_BUCKET=felis run_offsite configure_offsite)"
expect "a bucket without credentials is refused" "DIE: [offsite] names a bucket but there are no credentials" "$out"

out="$(run_offsite 'configure_offsite; install_offsite_timer; summary_offsite; echo "enabled=$OFFSITE_ENABLED"')"
expect "no bucket leaves the off-site copy off" "enabled=0" "$out"
expect "no bucket is a loud warning" "WARN: NO OFF-SITE COPY" "$out"

out="$(OFFSITE_ENABLED=1 run_offsite install_offsite_timer)"
unit="$(cat "$odir/felis-offsite.service")"
timer="$(cat "$odir/felis-offsite.timer")"
expect "the unit loads the secrets" "EnvironmentFile=$odir/offsite.env" "$unit"
expect "the unit syncs from the host config" \
  "ExecStart=fakefelis offsite sync -config $odir/felis.host.toml -env-file $odir/offsite.env -db-dir /var/lib/felis/db-backups -backup-pvc \"felis-backups\"" "$unit"
expect "a run ends before the next hour's" "TimeoutStartSec=55min" "$unit"
expect "the copy runs hourly" "OnCalendar=hourly" "$timer"
expect "a missed run catches up at boot" "Persistent=true" "$timer"
expect "the timer is enabled" "SYSTEMCTL: enable --now felis-offsite.timer" "$out"
expect "the bucket is checked during the install" "RUN: offsite list -config $odir/felis.host.toml -env-file $odir/offsite.env" "$out"
expect "the first copy runs in the background" "SYSTEMCTL: start --no-block felis-offsite.service" "$out"

out="$(OFFSITE_ENABLED=1 CHECK_FAILS=1 run_offsite install_offsite_timer)"
expect "an unreachable bucket shows why" "bucket: access denied" "$out"
expect "an unreachable bucket is a loud warning" "WARN: the [offsite] bucket did not answer" "$out"
case "$out" in
  *"start --no-block"*) echo "FAIL a failed check still started the copy"; fails=$((fails + 1)) ;;
  *) echo "PASS a failed check does not start the copy" ;;
esac

out="$(OFFSITE_ENABLED=0 run_offsite 'systemctl() { printf "SYSTEMCTL: %s\n" "$*" >> "$STATE_DIR/systemctl.log"; }; install_offsite_timer')"
expect "removing [offsite] disables the timer" "SYSTEMCTL: disable --now felis-offsite.timer" "$(cat "$odir/systemctl.log")"
expect "removing [offsite] says so" "WARN: no [offsite] bucket is configured any more" "$out"
if [ -e "$odir/felis-offsite.service" ] || [ -e "$odir/felis-offsite.timer" ]; then
  echo "FAIL removing [offsite] left the units behind"; fails=$((fails + 1))
else
  echo "PASS removing [offsite] removes the units"
fi
rm -rf "$odir" "$ofile"


# ---------------------------------------------------------------------------------------
if [ "$fails" -eq 0 ]; then
  echo "ALL PASS"
else
  echo "$fails FAILED"
fi
exit "$fails"
