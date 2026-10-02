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

# --- the Velocity digest gate -----------------------------------------------------------
# The download must hash to what the lock (or the content-addressed URL) promised, BEFORE
# atomic_install_file — the same refusal the Via plugins and the fork jar already get.

vblock="$(awk '/^stage_velocity_jar\(\) \{/,/^}/' "$BS")"
[ -n "$vblock" ] || { echo "FAIL: no stage_velocity_jar found in $BS"; exit 1; }

vdir="$(mktemp -d)"
trap 'rm -f "$jar"; rm -rf "$vdir"' EXIT
vwant="$(printf 'stand-in velocity build\n' | sha256sum | cut -d' ' -f1)"

run_velocity_install() { # expected-digest
  WANT="$1" VELOCITY_DIR="$vdir" bash -c '
    die() { printf "DIE: %s\n" "$*"; exit 1; }
    log() { printf "LOG: %s\n" "$*"; }
    ok() { printf "OK: %s\n" "$*"; }
    remember_temp() { :; }
    curl() { while [ "$#" -gt 1 ] && [ "$1" != "-o" ]; do shift; done; printf "stand-in velocity build\n" > "$2"; }
    atomic_install_file() { printf "INSTALL: %s\n" "$2"; cp "$1" "$2"; }
    '"$vblock"'
    stage_velocity_jar "https://fill-data.papermc.io/v1/objects/${WANT}/velocity-3.5.1-615.jar" "$WANT" 3.5.1'
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
out="$(run_velocity_install "$vwant")"
case "$out" in
  *LOG:*|*INSTALL:*) echo "FAIL a rerun of the staged build downloaded it again"; fails=$((fails + 1)) ;;
  *"Velocity 3.5.1 already staged"*) echo "PASS a rerun of the staged build leaves velocity.jar alone" ;;
  *) echo "FAIL stage_velocity_jar died on a staged build: $out"; fails=$((fails + 1)) ;;
esac

ivblock="$(awk '/^install_velocity\(\) \{/,/^}/' "$BS")"
run_velocity_choice() { # FELIS_GAME_STACK FELIS_VELOCITY_VERSION lock-version
  FELIS_GAME_STACK="$1" FELIS_VELOCITY_VERSION="$2" VELOCITY_VERSION="$3" VELOCITY_LATEST_MINOR=3.5.1 \
  VELOCITY_JAR_URL=https://fill-data.papermc.io/v1/objects/aaa/velocity-3.5.1-615.jar VELOCITY_JAR_SHA256=aaa \
  FELIS_VELOCITY_FORK_JAR='' bash -c '
    set -Eeuo pipefail
    log() { :; }
    die() { printf "DIE: %s\n" "$*"; exit 1; }
    install_jre() { :; }
    prepare_velocity_layout() { :; }
    install_via_plugins() { :; }
    write_velocity_config() { :; }
    install_velocity_service() { :; }
    configure_velocity_firewall() { :; }
    papermc_latest_jar() { printf "https://fill-data.papermc.io/v1/objects/bbb/velocity-%s-700.jar bbb\n" "$2"; }
    stage_velocity_jar() { printf "STAGE %s %s %s\n" "$@"; }
    '"$ivblock"'
    install_velocity'
}
expect "a pinned install stages the lock's Velocity build" \
  "STAGE https://fill-data.papermc.io/v1/objects/aaa/velocity-3.5.1-615.jar aaa 3.5.1" "$(run_velocity_choice pinned '' 3.5.1)"
expect "another FELIS_VELOCITY_VERSION resolves that minor's newest build" \
  "STAGE https://fill-data.papermc.io/v1/objects/bbb/velocity-3.6.0-700.jar bbb 3.6.0" "$(run_velocity_choice pinned 3.6.0 3.5.1)"
expect "FELIS_GAME_STACK=latest resolves the newest build of the default minor" \
  "STAGE https://fill-data.papermc.io/v1/objects/bbb/velocity-3.5.1-700.jar bbb 3.5.1" "$(run_velocity_choice latest '' 3.5.1)"

# --- the game-stack lock ----------------------------------------------------------------
# bootstrap.sh reads deploy/game-stack.lock as data: known keys only, all of them present,
# values limited to URL and version characters, digests shaped like digests.

lblock="$(awk '/^load_game_stack_lock\(\) \{/,/^}/' "$BS")"
[ -n "$lblock" ] || { echo "FAIL: no load_game_stack_lock found in $BS"; exit 1; }
lkeys="$(grep '^GAME_STACK_LOCK_KEYS=' "$BS")"
[ -n "$lkeys" ] || { echo "FAIL: no GAME_STACK_LOCK_KEYS in $BS"; exit 1; }
ldir="$(mktemp -d)"
run_lock() { # lock-file
  LOCK="$1" bash -c '
    set -Eeuo pipefail
    die() { printf "DIE: %s\n" "$*"; exit 1; }
    '"$lkeys"'
    '"$lblock"'
    load_game_stack_lock "$LOCK"
    printf "MC=%s LIMBO=%s VELOCITY=%s\n" "$MC_VERSION" "$LIMBO_JAR_URL" "$VELOCITY_JAR_SHA256"'
}
repo_lock="$(dirname "$BS")/game-stack.lock"
out="$(run_lock "$repo_lock")"
expect "the shipped lock loads" "MC=$(sed -n 's/^MC_VERSION=//p' "$repo_lock") LIMBO=https://ci.loohpjames.com/job/Limbo/" "$out"
{ cat "$repo_lock"; printf 'EVIL=$(touch /tmp/pwned)\n'; } > "$ldir/unknown"
expect "an unknown key is refused" "DIE: $ldir/unknown: unknown key EVIL" "$(run_lock "$ldir/unknown")"
sed 's|^LIMBO_VERSION=.*|LIMBO_VERSION=$(id)|' "$repo_lock" > "$ldir/subst"
expect "a value with shell syntax is refused" "DIE: $ldir/subst: LIMBO_VERSION has an unexpected value" "$(run_lock "$ldir/subst")"
grep -v '^LUCKPERMS_JAR_SHA256=' "$repo_lock" > "$ldir/missing"
expect "a missing key is refused" "DIE: $ldir/missing does not set LUCKPERMS_JAR_SHA256" "$(run_lock "$ldir/missing")"
sed 's|^PAPER_JAR_SHA256=.*|PAPER_JAR_SHA256=ABCDEF|' "$repo_lock" > "$ldir/badsha"
expect "a malformed digest is refused" "DIE: $ldir/badsha: PAPER_JAR_SHA256 is not a lowercase sha256" "$(run_lock "$ldir/badsha")"
expect "no lock file is refused with the way out" "FELIS_GAME_STACK=latest" "$(run_lock "$ldir/none")"
rm -rf "$ldir"

# --- the Temurin JRE pin ----------------------------------------------------------------
jblock="$(awk '/^install_jre\(\) \{/,/^}/' "$BS")"
[ -n "$jblock" ] || { echo "FAIL: no install_jre found in $BS"; exit 1; }
jdir="$(mktemp -d)"
jtar="$(mktemp -d)"
mkdir -p "$jtar/jdk-25.0.9+1-jre/bin"
printf '#!/bin/sh\n' > "$jtar/jdk-25.0.9+1-jre/bin/java"
chmod +x "$jtar/jdk-25.0.9+1-jre/bin/java"
printf 'IMPLEMENTOR="Eclipse Adoptium"\nIMPLEMENTOR_VERSION="Temurin-25.0.9+1"\n' > "$jtar/jdk-25.0.9+1-jre/release"
tar -C "$jtar" -czf "$jtar/jre.tar.gz" "jdk-25.0.9+1-jre"
jsha="$(sha256sum < "$jtar/jre.tar.gz" | cut -d' ' -f1)"
run_jre() { # machine pinned-sha
  MACHINE="$1" SHA="$2" TARBALL="$jtar/jre.tar.gz" JRE_DIR="$jdir/jre" FELIS_JRE_VERSION=25 \
  JRE_PINNED_FEATURE=25 JRE_PINNED_RELEASE=25.0.9+1 JRE_PINNED_SHA256_X64="$2" JRE_PINNED_SHA256_AARCH64="$2" bash -c '
    set -Eeuo pipefail
    die() { printf "DIE: %s\n" "$*"; exit 1; }
    log() { printf "LOG: %s\n" "$*"; }
    ok() { printf "OK: %s\n" "$*"; }
    remember_temp() { :; }
    uname() { printf "%s\n" "$MACHINE"; }
    curl() { local prev=""; while [ "$#" -gt 1 ] && [ "$1" != "-o" ]; do prev="$1"; shift; done; printf "URL %s\n" "$prev" >&2; cp "$TARBALL" "$2"; }
    '"$jblock"'
    install_jre' 2>&1
}
out="$(run_jre x86_64 deadbeef)"
expect "a JRE download with the wrong digest is refused" "hashes to ${jsha}, expected deadbeef" "$out"
[ -e "$jdir/jre" ] && { echo "FAIL a refused JRE was unpacked"; fails=$((fails + 1)); } || echo "PASS a refused JRE is not unpacked"
out="$(run_jre aarch64 "$jsha")"
expect "the pinned JRE is downloaded from its GitHub release" \
  "URL https://github.com/adoptium/temurin25-binaries/releases/download/jdk-25.0.9%2B1/OpenJDK25U-jre_aarch64_linux_hotspot_25.0.9_1.tar.gz" "$out"
expect "the pinned JRE installs" "OK: JRE at $jdir/jre/bin/java (Temurin 25.0.9+1)" "$out"
expect "a rerun of the pinned JRE is a no-op" "OK: Temurin 25.0.9+1 JRE already installed" "$(run_jre x86_64 "$jsha")"
printf 'IMPLEMENTOR="Eclipse Adoptium"\nIMPLEMENTOR_VERSION="Temurin-25.0.1+8"\n' > "$jdir/jre/release"
out="$(run_jre x86_64 "$jsha")"
expect "an older installer-managed JRE moves to the pin" "LOG: moving the proxy's JRE to Temurin 25.0.9+1" "$out"
expect "the moved JRE is the pinned build" 'IMPLEMENTOR_VERSION="Temurin-25.0.9+1"' "$(cat "$jdir/jre/release")"
printf 'IMPLEMENTOR="Azul Systems, Inc."\n' > "$jdir/jre/release"
expect "a JRE someone else installed is left alone" "is not a Temurin build this installer put there" "$(run_jre x86_64 "$jsha")"
expect "an unsupported architecture keeps a pre-staged JRE" "OK: JRE already installed at $jdir/jre" "$(run_jre riscv64 "$jsha")"
rm -rf "$jdir" "$jtar"

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
  require_tls = false

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
expect "an operator's require_tls survives" 'require_tls = false' "$out"
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

# ufw_stub is ufw for a run: UFW=active or inactive, its status line translated outside the
# C locale as ufw translates it; every other call is printed to stderr, which the installer
# leaves alone when it silences ufw's "Rule added".
ufw_stub='
    ufw() {
      case "$*" in
        status)
          if [ "${UFW:-inactive}" != active ]; then echo "Status: inactive"
          elif [ "${LC_ALL:-}" = C ]; then echo "Status: active"
          else echo "状态：激活"; fi ;;
        *) printf "UFW: %s\n" "$*" >&2 ;;
      esac
    }'
ublock="$(awk '/^ufw_active\(\) \{/,/^}/' "$BS")"
[ -n "$ublock" ] || { echo "FAIL: no ufw_active found in $BS"; exit 1; }

run_fw() { # listen proxy-cidr port-already-open(0|1) [ufw-state]
  FELIS_NANO_LISTEN="$1" FELIS_NANO_PROXY_CIDR="$2" OPEN="$3" UFW="${4:-inactive}" bash -c '
    ok() { printf "OK: %s\n" "$*"; }
    log() { printf "LOG: %s\n" "$*"; }
    warn() { printf "WARN: %s\n" "$*"; }
    systemctl() { return 0; }
    firewall-cmd() {
      case "$*" in *--query-port=*) [ "$OPEN" = 1 ]; return ;; esac
      printf "FW: %s\n" "$*"
    }
    '"$ufw_stub"'
    '"$ublock"'
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

# ufw, enabled on many Ubuntu and Debian hosts, drops what no rule admits.
out="$(run_fw 0.0.0.0:8081 10.0.0.7/32 0 active)"
expect "ufw admits the nano port from the proxy CIDR alone" \
  "UFW: allow proto tcp from 10.0.0.7/32 to any port 8081 comment felis-nano" "$out"
out="$(run_fw 0.0.0.0:8081 '' 0 active)"
expect "no proxy CIDR says ufw keeps the port closed" "WARN: no FELIS_NANO_PROXY_CIDR, so ufw keeps 8081/tcp closed" "$out"
case "$out" in
  *"UFW: allow"*) echo "FAIL no proxy CIDR must add no ufw rule:"; echo "$out"; fails=$((fails + 1)) ;;
  *) echo "PASS no proxy CIDR adds no ufw rule" ;;
esac
for args in "127.0.0.1:8081 10.0.0.7/32 0 active" "0.0.0.0:8081 10.0.0.7/32 0 inactive"; do
  # shellcheck disable=SC2086 # the words are the arguments
  case "$(run_fw $args)" in
    *UFW:*) echo "FAIL run_fw $args must leave ufw alone"; fails=$((fails + 1)) ;;
    *) echo "PASS run_fw $args leaves ufw alone" ;;
  esac
done

k3fblock="$(awk '/^configure_k3s_firewall\(\) \{/,/^}/' "$BS")"
[ -n "$k3fblock" ] || { echo "FAIL: no configure_k3s_firewall found in $BS"; exit 1; }
vfblock="$(awk '/^configure_velocity_firewall\(\) \{/,/^}/' "$BS")"
[ -n "$vfblock" ] || { echo "FAIL: no configure_velocity_firewall found in $BS"; exit 1; }
run_host_fw() { # ufw-state firewalld(0|1)
  UFW="$1" FIREWALLD="$2" POD_CIDR=10.42.0.0/16 SERVICE_CIDR=10.43.0.0/16 FELIS_PANEL_NODEPORT=30443 \
  FELIS_GAME_PORT=25565 bash -c '
    log() { printf "LOG: %s\n" "$*"; }
    systemctl() { [ "$FIREWALLD" = 1 ]; }
    firewall-cmd() { printf "FW: %s\n" "$*"; }
    '"$ufw_stub"'
    '"$ublock"'
    '"$k3fblock"'
    '"$vfblock"'
    configure_k3s_firewall
    configure_velocity_firewall' 2>&1
}
out="$(run_host_fw active 0)"
expect "ufw admits the pods" "UFW: allow from 10.42.0.0/16 comment felis-k3s-pods" "$out"
expect "ufw admits the services" "UFW: allow from 10.43.0.0/16 comment felis-k3s-services" "$out"
expect "ufw opens the panel NodePort" "UFW: allow 30443/tcp comment felis-panel" "$out"
expect "ufw opens the game port" "UFW: allow 25565/tcp comment felis-proxy" "$out"
case "$out" in
  *"UFW: allow 6443"*) echo "FAIL ufw must not open the API server to the network:"; echo "$out"; fails=$((fails + 1)) ;;
  *) echo "PASS ufw keeps the API server closed to the network" ;;
esac
case "$out" in
  *FW:*--add*) echo "FAIL an inactive firewalld must be left alone:"; echo "$out"; fails=$((fails + 1)) ;;
  *) echo "PASS ufw alone leaves firewalld alone" ;;
esac
out="$(run_host_fw inactive 1)"
case "$out" in
  *UFW:*) echo "FAIL an inactive ufw must be left alone:"; echo "$out"; fails=$((fails + 1)) ;;
  *) echo "PASS an inactive ufw is left alone" ;;
esac
expect "  while firewalld still gets the pods" "FW: --permanent --zone=trusted --add-source=10.42.0.0/16" "$out"

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

# --- every download retries --------------------------------------------------------------
# curl without --retry gives up on the first 503 or timeout: one blip in the ViaVersion, Go
# or Docker repository download stopped a fresh install halfway.
noretry="$(grep -nE 'curl -(fsSL|sfL)' "$BS" | grep -vE '^[0-9]+:[[:space:]]*#|<(raw-)?url>' | grep -v -- '--retry' || true)"
if [ -z "$noretry" ]; then
  echo "PASS every curl download in the installer retries"
else
  echo "FAIL curl downloads without --retry:"; printf '%s\n' "$noretry"; fails=$((fails + 1))
fi

# --- a release asset's download is tried three times -------------------------------------
# curl's --retry does not cover a connection reset mid-stream, the likely failure on an
# image bundle of several hundred MiB, and what the installer falls back to is a build on
# the host that preflight may not have counted on. A release that does not list the asset
# (still uploading) falls back at once: no download can find it.

dlblock="$(awk '/^download_release_asset\(\) \{/,/^}/' "$BS")"
[ -n "$dlblock" ] || { echo "FAIL: no download_release_asset found in $BS"; exit 1; }
[ "$(printf '%s\n' "$dlblock" | wc -l)" -lt 40 ] \
  || { echo "FAIL: the extracted block is not download_release_asset -- did its closing brace move?"; exit 1; }
dldir="$(mktemp -d)"
run_dl() { # [script]; DL_FAILS curl calls fail (exit ${DL_RC:-56}) after writing part of the file
  rm -f "$dldir/calls" "$dldir/asset"
  DLDIR="$dldir" bash -c '
    set -Eeuo pipefail
    log() { printf "LOG: %s\n" "$*"; }; warn() { printf "WARN: %s\n" "$*"; }
    sleep() { printf "SLEEP: %s\n" "$*"; }
    load_release_json() { :; }
    github_asset_id() { [ -z "${DL_UNLISTED:-}" ] || return 1; echo 4242; }
    repo_slug() { echo FelisMC/Felis; }
    curl() {
      local out="" a stdin=""
      printf "%s\n" "$*" >> "$DLDIR/calls"
      [ "${*: -1}" = https://api.github.com/repos/FelisMC/Felis/releases/assets/4242 ] || { echo "curl: wrong url ${*: -1}" >&2; return 3; }
      for a in "$@"; do [ "$a" != - ] || stdin="$(command cat)"; done
      [ -z "$stdin" ] || printf "%s\n" "$stdin" > "$DLDIR/stdin"
      while [ "$#" -gt 0 ]; do [ "$1" = -o ] && out="$2"; shift; done
      if [ "$(wc -l < "$DLDIR/calls")" -le "${DL_FAILS:-0}" ]; then
        printf "half a bund" > "$out"; return "${DL_RC:-56}"
      fi
      [ -n "${DL_EMPTY:-}" ] && { : > "$out"; return 0; }
      printf "the whole bundle\n" > "$out"
    }
    FELIS_REPO_URL=https://github.com/FelisMC/Felis FELIS_GITHUB_TOKEN="${DL_TOKEN:-}"
    '"$dlblock"'
    if download_release_asset v9.9.9 felis-image-felis-linux-amd64.tar "$DLDIR/asset"; then echo FETCHED; else echo "GAVE UP"; fi' 2>&1
}
calls() { [ -f "$dldir/calls" ] && wc -l < "$dldir/calls" | tr -d ' ' || echo 0; }

out="$(DL_FAILS=2 run_dl)"
expect "a download reset twice mid-stream gets the asset on the third try" "FETCHED" "$out"
expect "the third try's file is the whole asset" "the whole bundle" "$(cat "$dldir/asset" 2>/dev/null)"
expect "each failed try is reported with curl's exit code" "WARN: downloading felis-image-felis-linux-amd64.tar failed (attempt 2/3, curl exit 56); retrying" "$out"
expect "the tries are spaced" "SLEEP: 5" "$out"
expect "a download that works on the third try takes three" 3 "$(calls)"

out="$(DL_FAILS=9 run_dl)"
expect "a download that keeps failing gives up" "GAVE UP" "$out"
expect "it gives up after three tries" 3 "$(calls)"
if [ -e "$dldir/asset" ]; then
  echo "FAIL a failed download left part of the asset behind"; fails=$((fails + 1))
else
  echo "PASS a failed download leaves no part of the asset behind"
fi
out="$(DL_EMPTY=1 run_dl)"
expect "an empty file is a failed download" "GAVE UP" "$out"
expect "an empty file is tried again" 3 "$(calls)"

out="$(DL_UNLISTED=1 DL_FAILS=9 run_dl)"
expect "a release that does not list the asset gives up" "GAVE UP" "$out"
if [ "$(calls) $(printf '%s\n' "$out" | grep -c SLEEP)" = "0 0" ]; then
  echo "PASS an unlisted asset is not downloaded, nor waited for"
else
  echo "FAIL an unlisted asset was tried $(calls) times"; fails=$((fails + 1))
fi

out="$(run_dl)"
expect "a download that works takes one try" 1 "$(calls)"
expect "a stalled transfer counts as a failure curl retries" "--speed-limit 1024 --speed-time 60" "$(cat "$dldir/calls")"
DL_TOKEN=tok-secret run_dl >/dev/null
case "$(cat "$dldir/calls")" in
  *tok-secret*) echo "FAIL the token reached curl's argv"; fails=$((fails + 1)) ;;
  *) echo "PASS the token stays out of curl's argv" ;;
esac
expect "the token goes to curl on stdin" 'header = "Authorization: Bearer tok-secret"' "$(cat "$dldir/stdin" 2>/dev/null)"
expect "a download with a token counts a stalled transfer as a failure too" "--speed-limit 1024 --speed-time 60" "$(cat "$dldir/calls")"
rm -rf "$dldir"

# --- a build preflight did not count on checks its room first ---------------------------
# A release install budgets no Docker; an asset it then cannot use is built here, with
# Docker's images and build cache under /var/lib/containerd. Without the room the install
# must stop before Docker is installed, not fill the disk halfway through a build.

edblock="$(awk '/^ensure_docker\(\) \{/,/^}/' "$BS"; awk '/^unplanned_build_room\(\) \{/,/^}/' "$BS")"
case "$edblock" in *"install_docker"*"FELIS_PREFLIGHT=warn to build anyway"*) ;; *) echo "FAIL: ensure_docker / unplanned_build_room not found in $BS"; exit 1 ;; esac
run_ed() { # [DOCKER_INSTALLED]; ED_FREE_MIB free on the filesystem holding /var/lib/containerd
  DOCKER_INSTALLED="${1:-}" bash -c '
    set -Eeuo pipefail
    ok() { printf "OK: %s\n" "$*"; }; warn() { printf "WARN: %s\n" "$*"; }; die() { printf "DIE: %s\n" "$*"; exit 1; }
    install_docker() { echo INSTALL; }
    systemctl() { echo "SYSTEMCTL $*"; }
    path_populated() { [ -n "${ED_POPULATED:-}" ]; }
    existing_ancestor() { echo /; }
    df() { echo DF >&2; echo "Filesystem 1024-blocks Used Available Capacity Mounted"
      echo "/dev/vda1 41943040 1 $(( ${ED_FREE_MIB:-60000} * 1024 )) 50% /"; }
    bootstrap_from_tui() { return 1; }
    '"$(awk '/^use_release_binary\(\) \{/,/^}/' "$BS")"'
    '"$(awk '/^release_assets_expected\(\) \{/,/^}/' "$BS")"'
    '"$(awk '/^host_builds_expected\(\) \{/,/^}/' "$BS")"'
    FELIS_SKIP_FETCH="" FELIS_REF_PINNED="" FELIS_ARTIFACT_DIR="" FELIS_GAME_STACK=pinned
    FELIS_VERSION_BOOTSTRAP="${ED_CHANNEL:-release}" FELIS_PREFLIGHT="${ED_MODE:-strict}"
    '"$edblock"'
    ensure_docker; echo "installed=$DOCKER_INSTALLED"' 2>&1
}

out="$(ED_FREE_MIB=1000 run_ed)"
expect "a release install without room for an unplanned build stops" \
  "DIE: building here what the release did not supply takes about 8192 MiB under /var/lib/containerd, and / has 1000 MiB free; nothing has been built" "$out"
case "$out" in
  *INSTALL*) echo "FAIL Docker was installed on a host without room to build"; fails=$((fails + 1)) ;;
  *) echo "PASS the stop comes before Docker is installed" ;;
esac
expect "a release install with the room builds" "INSTALL" "$(ED_FREE_MIB=9000 run_ed)"
expect "Docker's own cache needs only the rerun's room" "INSTALL" "$(ED_POPULATED=1 ED_FREE_MIB=3000 run_ed)"
expect "a cache on a nearly full disk still stops" "takes about 2048 MiB" "$(ED_POPULATED=1 ED_FREE_MIB=1500 run_ed)"
out="$(ED_CHANNEL=dev ED_FREE_MIB=1000 run_ed)"
expect "a source build's room is preflight's to check" "installed=1" "$out"
case "$out" in
  *DF*) echo "FAIL a planned build measured the disk again"; fails=$((fails + 1)) ;;
  *) echo "PASS a planned build is not measured again" ;;
esac
out="$(ED_MODE=warn ED_FREE_MIB=1000 run_ed)"
expect "FELIS_PREFLIGHT=warn builds anyway, saying so" "WARN: building here what the release did not supply takes about 8192 MiB" "$out"
expect "FELIS_PREFLIGHT=warn installs Docker" "INSTALL" "$out"
out="$(ED_FREE_MIB=1000 run_ed 1)"
expect "a Docker this run installed is started again" "SYSTEMCTL start docker" "$out"
case "$out" in
  *DF*|*DIE*) echo "FAIL a restart of Docker checked the room again"; fails=$((fails + 1)) ;;
  *) echo "PASS a restart of Docker is not checked again" ;;
esac

# --- Docker holds no memory between builds -------------------------------------------------
# Docker serves the installer's builds and nothing at runtime. The one it installed does not
# start at boot, and once a step is done the daemon stops, and its containerd with it unless
# something besides Docker keeps a namespace there.

sdblock="$(awk '/^stop_docker\(\) \{/,/^}/' "$BS")"
[ -n "$sdblock" ] || { echo "FAIL: no stop_docker found in $BS"; exit 1; }
run_sd() { # DOCKER_INSTALLED namespaces-listed|FAIL
  DOCKER_INSTALLED="$1" NS="$2" bash -c '
    set -Eeuo pipefail
    systemctl() { echo "SYSTEMCTL $*"; }
    # Only the host containerd answers; k3s runs its own on another socket.
    ctr() {
      [ "$*" = "--address /run/containerd/containerd.sock namespaces ls -q" ] || { echo "CTR $*"; return 1; }
      [ "$NS" != FAIL ] || return 1
      printf "%b" "$NS"
    }
    '"$sdblock"'
    stop_docker'
}
expect "a containerd serving Docker alone stops with it" "SYSTEMCTL stop docker docker.socket
SYSTEMCTL stop containerd" "$(run_sd 1 'moby\nmoby_history\n')"
out="$(run_sd 1 'default\nmoby\n')"
case "$out" in
  *"stop containerd"*) echo "FAIL a containerd something else uses was stopped"; fails=$((fails + 1)) ;;
  *"SYSTEMCTL stop docker docker.socket"*) echo "PASS a containerd with another tenant keeps running" ;;
  *) echo "FAIL Docker was not stopped: $out"; fails=$((fails + 1)) ;;
esac
out="$(run_sd 1 FAIL)"
case "$out" in
  *"stop containerd"*) echo "FAIL a containerd that could not be asked was stopped"; fails=$((fails + 1)) ;;
  *"SYSTEMCTL stop docker docker.socket"*) echo "PASS a containerd that cannot be asked is left alone" ;;
  *) echo "FAIL Docker was not stopped: $out"; fails=$((fails + 1)) ;;
esac
out="$(run_sd "" 'moby\n')"
[ -z "$out" ] && echo "PASS a Docker this run never started is left alone" \
  || { echo "FAIL a Docker this run never started was touched: $out"; fails=$((fails + 1)); }

idblock="$(awk '/^install_docker\(\) \{/,/^}/' "$BS")"
[ -n "$idblock" ] || { echo "FAIL: no install_docker found in $BS"; exit 1; }
run_id() { # docker-on-PATH(0|1) containerd-on-PATH(0|1)
  fb="$(mktemp -d)"
  for tool in docker containerd; do
    { [ "$tool" = docker ] && [ "$1" = 1 ]; } || { [ "$tool" = containerd ] && [ "$2" = 1 ]; } || continue
    printf '#!/bin/sh\n' > "$fb/$tool"
    chmod +x "$fb/$tool"
  done
  FB="$fb" bash -c '
    set -Eeuo pipefail
    PATH="$FB"
    ok() { printf "OK: %s\n" "$*"; }; die() { printf "DIE: %s\n" "$*"; exit 1; }
    systemctl() { echo "SYSTEMCTL $*"; }
    install_docker_apt() { echo "INSTALL apt"; }
    PKG=apt
    '"$idblock"'
    install_docker'
  rm -rf "$fb"
}
out="$(run_id 0 0)"
expect "a Docker the installer brings is started without a place at boot" "INSTALL apt
SYSTEMCTL disable docker.service docker.socket
SYSTEMCTL disable containerd.service
SYSTEMCTL start docker" "$out"
case "$out" in *enable*) echo "FAIL the installed Docker was enabled at boot"; fails=$((fails + 1)) ;; *) echo "PASS the installed Docker is not enabled at boot" ;; esac
out="$(run_id 0 1)"
expect "a Docker installed beside an existing containerd leaves that containerd's boot alone" "INSTALL apt
SYSTEMCTL disable docker.service docker.socket
SYSTEMCTL start docker" "$out"
out="$(run_id 1 1)"
expect "a Docker already on the host is only started" "OK: docker already installed
SYSTEMCTL start docker" "$out"
case "$out" in *disable*|*enable*) echo "FAIL the host's own Docker had its boot changed"; fails=$((fails + 1)) ;; *) echo "PASS the host's own Docker keeps its boot setting" ;; esac

# --- a release binary is hashed against SHA256SUMS before anything runs it ---------------
# download_release_binary executes the asset as root to read its version stamp, so the
# checksum has to come first, and every failure has to fall back to the source build.

vblock="$(awk '/^verify_release_checksum\(\) \{/,/^}/' "$BS")"
[ -n "$vblock" ] || { echo "FAIL: no verify_release_checksum found in $BS"; exit 1; }
asset_file="$sdir/felis-linux-amd64"
printf 'stand-in felis binary\n' > "$asset_file"
asum="$(sha256sum <"$asset_file" | cut -d' ' -f1)"

run_verify() { # SHA256SUMS-content ("" = the release has none)
  SUMS="$1" TMPDIR="$sdir" bash -c '
    ok() { printf "OK: %s\n" "$*"; }
    warn() { printf "WARN: %s\n" "$*"; }
    remember_temp() { :; }
    download_release_asset() { [ -n "$SUMS" ] || return 1; printf "%s" "$SUMS" > "$3"; }
    '"$vblock"'
    verify_release_checksum v9.9.9 felis-linux-amd64 '"$asset_file"' && echo VERIFIED'
}
expect "a listed, matching binary is accepted" "VERIFIED" \
  "$(run_verify "$(printf '%s  felis-linux-arm64\n%s  felis-linux-amd64\n' deadbeef "$asum")")"
expect "a binary-mode SHA256SUMS line is read too" "VERIFIED" \
  "$(run_verify "$(printf '%s *felis-linux-amd64\n' "$asum")")"
out="$(run_verify "$(printf '%s  felis-linux-amd64\n' deadbeef)")"
expect "a hash mismatch is refused and names both hashes" "hashes to ${asum}, but release v9.9.9's SHA256SUMS says deadbeef" "$out"
case "$out" in *VERIFIED*) echo "FAIL: a mismatched binary must not verify"; fails=$((fails + 1)) ;; esac
out="$(run_verify "$(printf '%s  felis-linux-amd64.sig\n' "$asum")")"
expect "a SHA256SUMS that does not list the asset is refused" "does not list felis-linux-amd64" "$out"
case "$out" in *VERIFIED*) echo "FAIL: an unlisted binary must not verify"; fails=$((fails + 1)) ;; esac
out="$(run_verify "")"
expect "a release without SHA256SUMS falls back to a source build" "publishes no SHA256SUMS" "$out"
case "$out" in *VERIFIED*) echo "FAIL: a release without SHA256SUMS must not verify"; fails=$((fails + 1)) ;; esac

# download_release_binary itself, with the real verify_release_checksum, run the way the
# installer runs it: under set -Eeuo pipefail, from acquire_felis_binary or
# acquire_nano_binary. The stand-in asset writes to the same log as ok and warn each time it
# runs, so the log shows whether the hash was checked before the file was first executed,
# and that a refused one never runs at all.
dblock="$(awk '/^download_release_binary\(\) \{/,/^}/' "$BS")"
[ -n "$dblock" ] || { echo "FAIL: no download_release_binary found in $BS"; exit 1; }
[ "$(printf '%s\n' "$dblock" | wc -l)" -lt 100 ] \
  || { echo "FAIL: the extracted block is not the function -- did its closing brace move?"; exit 1; }
acqblock=""
for fn in acquire_felis_binary acquire_nano_binary; do
  blk="$(awk "/^${fn}\\(\\) \\{/,/^}/" "$BS")"
  [ -n "$blk" ] || { echo "FAIL: no ${fn} found in $BS"; exit 1; }
  [ "$(printf '%s\n' "$blk" | wc -l)" -lt 40 ] \
    || { echo "FAIL: the extracted block is not ${fn} -- did its closing brace move?"; exit 1; }
  acqblock="${acqblock}${blk}
"
done
ddir="$sdir/download"
mkdir -p "$ddir/bin"
printf '#!/bin/sh\necho "RAN: $*" >> "%s"\necho "felis v9.9.9"\n' "$ddir/log" > "$ddir/asset"
dsum="$(sha256sum <"$ddir/asset" | cut -d' ' -f1)"

# run_download SUMS [entry]: SHA256SUMS-content ("" = the release has none), and the caller
# (acquire_felis_binary by default). INSTALL_FAILS makes install(1) fail, FETCH_FAILS a
# command inside fetch_source. The log lands in $ddir/log; stdout says what the caller did.
run_download() {
  rm -f "$ddir/bin/felis"
  : > "$ddir/log"
  SUMS="$1" ENTRY="${2:-acquire_felis_binary}" ASSET="$ddir/asset" LOG="$ddir/log" FELIS_REF=v9.9.9 \
    HOST_BIN="$ddir/bin/felis" TMPDIR="$sdir" INSTALL_FAILS="${INSTALL_FAILS:-}" FETCH_FAILS="${FETCH_FAILS:-}" bash -c '
    set -Eeuo pipefail
    ok() { printf "OK: %s\n" "$*" >> "$LOG"; }
    warn() { printf "WARN: %s\n" "$*" >> "$LOG"; }
    log() { :; }
    remember_temp() { :; }
    felis_asset_arch() { echo amd64; }
    keep_previous_host_binary() { :; }
    download_release_asset() {
      case "$2" in
        SHA256SUMS) [ -n "$SUMS" ] || return 1; printf "%s" "$SUMS" > "$3" ;;
        felis-linux-amd64) cat "$ASSET" > "$3" ;;
        *) return 1 ;;
      esac
    }
    use_release_binary() { :; }
    bootstrap_from_tui() { return 1; }
    resolve_install_ref() { :; }
    fetch_source() { echo FETCH_SOURCE; [ -z "$FETCH_FAILS" ] || { false; echo "FETCH WENT ON"; }; }
    install_go_toolchain() { :; }
    build_nano_binary() { echo BUILD_NANO; }
    [ -z "$INSTALL_FAILS" ] || install() { echo "install: No space left on device" >&2; return 1; }
    FELIS_ARTIFACT_DIR="" HAVE_PREBUILT_BINARY=""
    '"$vblock"'
    '"$dblock"'
    '"$acqblock"'
    "$ENTRY"
    echo "PREBUILT[$HAVE_PREBUILT_BINARY]"' 2>&1
}
same_log() { # label want
  if [ "$(cat "$ddir/log")" = "$2" ]; then echo "PASS $1"; else printf 'FAIL %s: got\n%s\nwant\n%s\n' "$1" "$(cat "$ddir/log")" "$2"; fails=$((fails + 1)); fi
}
same_out() { # label want got-rc: the caller's exit status and its whole output
  if [ "$3" = 0 ] && [ "$out" = "$2" ]; then echo "PASS $1"; else printf 'FAIL %s: exit %s, got\n%s\nwant\n%s\n' "$1" "$3" "$out" "$2"; fails=$((fails + 1)); fi
}
refused_download() { # label: nothing installed, and the staged download removed
  if [ -e "$ddir/bin/felis" ]; then echo "FAIL $1: a binary was installed"; fails=$((fails + 1)); else echo "PASS $1 installs nothing"; fi
  left="$(ls -A "$ddir/bin")"
  if [ -z "$left" ]; then echo "PASS $1 leaves no staged download"; else echo "FAIL $1 left $left"; fails=$((fails + 1)); fi
}
died_midway() { # label rc: the run stopped at the failure, and the caller went no further
  if [ "$2" -ne 0 ]; then echo "PASS $1 stops the install"; else echo "FAIL $1 must stop the install"; fails=$((fails + 1)); fi
  case "$out" in
    *PREBUILT*|*"WENT ON"*|*BUILD_NANO*) echo "FAIL $1 went on: $out"; fails=$((fails + 1)) ;;
    *) echo "PASS $1 goes no further" ;;
  esac
}

out="$(run_download "$(printf '%s  felis-linux-amd64\n' "$dsum")")"
same_out "a verified release binary installs, with no source build" "PREBUILT[1]" $?
same_log "the release binary is hashed before it is first executed" "$(printf '%s\n' \
  "OK: felis-linux-amd64 matches release v9.9.9's SHA256SUMS" \
  "RAN: version" \
  "OK: installed felis-linux-amd64 v9.9.9 at $ddir/bin/felis")"
if cmp -s "$ddir/asset" "$ddir/bin/felis"; then echo "PASS the installed binary is the download"; else echo "FAIL the installed binary is not the download"; fails=$((fails + 1)); fi

out="$(run_download "$(printf '%s  felis-linux-amd64\n' deadbeef)")"
same_out "a mismatched release binary asks for the source build" "FETCH_SOURCE
PREBUILT[]" $?
same_log "a mismatched release binary is never executed" \
  "WARN: downloaded felis-linux-amd64 hashes to ${dsum}, but release v9.9.9's SHA256SUMS says deadbeef; discarding it and building v9.9.9 from source on this host instead"
refused_download "a mismatched release binary"

out="$(run_download "")"
same_out "a release binary without SHA256SUMS asks for the source build" "FETCH_SOURCE
PREBUILT[]" $?
same_log "a release binary without SHA256SUMS is never executed" \
  "WARN: release v9.9.9 publishes no SHA256SUMS, so felis-linux-amd64 cannot be verified; building v9.9.9 from source on this host instead"
refused_download "a release binary without SHA256SUMS"

# errexit reaches inside: run as an if or && condition it did not, and an install(1) that
# failed after HOST_BIN was removed read as an installed binary.
out="$(INSTALL_FAILS=1 run_download "$(printf '%s  felis-linux-amd64\n' "$dsum")")"
died_midway "an install(1) of the release binary that fails" $?
case "$(cat "$ddir/log")" in
  *"OK: installed"*) echo "FAIL a failed install(1) reported the binary installed"; fails=$((fails + 1)) ;;
  *) echo "PASS a failed install(1) reports nothing installed" ;;
esac
out="$(FETCH_FAILS=1 run_download "")"
died_midway "a failure inside the source fetch the download fell back to" $?

out="$(run_download "$(printf '%s  felis-linux-amd64\n' "$dsum")" acquire_nano_binary)"
same_out "nano: a verified release binary installs, with no source build" "PREBUILT[1]" $?
out="$(run_download "" acquire_nano_binary)"
same_out "nano: no usable release binary builds nano from source" "FETCH_SOURCE
BUILD_NANO
PREBUILT[]" $?
out="$(INSTALL_FAILS=1 run_download "$(printf '%s  felis-linux-amd64\n' "$dsum")" acquire_nano_binary)"
died_midway "nano: an install(1) of the release binary that fails" $?

# --- cloudflared is a pinned release, checked before it is installed ---------------------
cfblock="$(awk '/^install_cloudflared\(\) \{/,/^}/' "$BS")"
[ -n "$cfblock" ] || { echo "FAIL: no install_cloudflared found in $BS"; exit 1; }
cfsum="$(printf 'stand-in cloudflared\n' | sha256sum | cut -d' ' -f1)"
run_cf() { # FELIS_CLOUDFLARED_VERSION pinned-amd64-digest [FELIS_CLOUDFLARED_SHA256]
  FELIS_CLOUDFLARED_VERSION="$1" CLOUDFLARED_PINNED_VERSION=2026.9.1 CLOUDFLARED_PINNED_SHA256_AMD64="$2" \
    CLOUDFLARED_PINNED_SHA256_ARM64=unused CLOUDFLARED_PINNED_SHA256_ARM=unused \
    FELIS_CLOUDFLARED_SHA256="${3:-}" TMPDIR="$sdir" CLOUDFLARED_BIN=/usr/local/bin/cloudflared \
    FELIS_UPGRADE_DEPS="${CF_UPGRADE:-0}" CF_PATH="${CF_PATH:-}" CF_HAVE="${CF_HAVE:-test}" \
    CF_ACTIVE="${CF_ACTIVE:-0}" bash -c '
    die() { printf "DIE: %s\n" "$*"; exit 1; }
    log() { printf "LOG: %s\n" "$*"; }
    ok() { printf "OK: %s\n" "$*"; }
    warn() { printf "WARN: %s\n" "$*"; }
    remember_temp() { :; }
    command() { [ -n "$CF_PATH" ] && [ "$1" = -v ] && [ "$2" = cloudflared ] && echo "$CF_PATH"; }
    uname() { echo x86_64; }
    cloudflared() { echo "cloudflared version ${CF_HAVE} (built 2026-01-01-0000 UTC)"; }
    curl() { printf "CURL: %s\n" "$*"; while [ "$#" -gt 1 ] && [ "$1" != "-o" ]; do shift; done
      printf "stand-in cloudflared\n" > "$2"; }
    install() { printf "INSTALL: %s\n" "$*"; }
    systemctl() { case "$1" in is-active) [ "$CF_ACTIVE" = 1 ] ;; *) printf "SYSTEMCTL: %s\n" "$*" ;; esac; }
    '"$(awk '/^version_newer\(\) \{/,/^}/' "$BS")"'
    '"$cfblock"'
    install_cloudflared'
}
out="$(run_cf 2026.9.1 "$cfsum")"
expect "cloudflared comes from the pinned release, never latest" "releases/download/2026.9.1/cloudflared-linux-amd64" "$out"
expect "a matching cloudflared is installed" "INSTALL: -m 0755" "$out"
out="$(run_cf 2026.9.1 deadbeef)"
expect "a cloudflared that does not match the pin is refused" "DIE: cloudflared-linux-amd64 2026.9.1 hashes to ${cfsum}, expected deadbeef" "$out"
case "$out" in *INSTALL:*) echo "FAIL: a refused cloudflared must not be installed"; fails=$((fails + 1)) ;; esac
expect "another cloudflared version needs its own digest" "DIE: no pinned sha256 for cloudflared 2027.1.0" "$(run_cf 2027.1.0 "$cfsum")"
expect "another cloudflared version installs with its digest" "INSTALL: -m 0755" "$(run_cf 2027.1.0 deadbeef "$cfsum")"

# An installed cloudflared moves only under FELIS_UPGRADE_DEPS=1, only when Felis put it
# there, never backwards, and the running tunnel is restarted onto the new binary.
out="$(CF_PATH=/usr/local/bin/cloudflared CF_HAVE=2026.9.1 run_cf 2026.9.1 "$cfsum")"
expect "a cloudflared at the pin is left alone" "OK: cloudflared 2026.9.1 already installed" "$out"
case "$out" in *CURL:*) echo "FAIL: a cloudflared at the pin must not be downloaded again"; fails=$((fails + 1)) ;; esac
out="$(CF_PATH=/usr/local/bin/cloudflared CF_HAVE=2025.8.0 run_cf 2026.9.1 "$cfsum")"
expect "an older cloudflared is reported without the flag" "this release pins 2026.9.1 (FELIS_UPGRADE_DEPS=1 moves it)" "$out"
case "$out" in *CURL:*) echo "FAIL: an installed cloudflared must not move without FELIS_UPGRADE_DEPS=1"; fails=$((fails + 1)) ;; esac
out="$(CF_UPGRADE=1 CF_ACTIVE=1 CF_PATH=/usr/local/bin/cloudflared CF_HAVE=2025.8.0 run_cf 2026.9.1 "$cfsum")"
expect "FELIS_UPGRADE_DEPS=1 installs the pinned cloudflared over an older one" "INSTALL: -m 0755" "$out"
expect "the running tunnel is restarted onto the new cloudflared" "SYSTEMCTL: restart cloudflared-felis" "$out"
out="$(CF_UPGRADE=1 CF_PATH=/usr/local/bin/cloudflared CF_HAVE=2025.8.0 run_cf 2026.9.1 "$cfsum")"
case "$out" in *SYSTEMCTL:*) echo "FAIL: a stopped cloudflared-felis must not be started by an upgrade"; fails=$((fails + 1)) ;; esac
out="$(CF_UPGRADE=1 CF_PATH=/usr/bin/cloudflared CF_HAVE=2025.8.0 run_cf 2026.9.1 "$cfsum")"
expect "a packaged cloudflared is left to its package manager" "WARN: cloudflared at /usr/bin/cloudflared was not installed by Felis" "$out"
case "$out" in *CURL:*) echo "FAIL: a packaged cloudflared must not be overwritten"; fails=$((fails + 1)) ;; esac
out="$(CF_UPGRADE=1 CF_PATH=/usr/local/bin/cloudflared CF_HAVE=2026.10.2 run_cf 2026.9.1 "$cfsum")"
expect "a newer cloudflared is never downgraded" "cloudflared 2026.10.2 is newer than the pinned 2026.9.1" "$out"
case "$out" in *CURL:*) echo "FAIL: a newer cloudflared must not be downgraded"; fails=$((fails + 1)) ;; esac

# k3s: the install script is read from the pinned tag, and told the same version.
kblock="$(awk '/^run_k3s_installer\(\) \{/,/^}/' "$BS")"
expect "k3s's install script comes from the pinned tag" 'raw.githubusercontent.com/k3s-io/k3s/${FELIS_K3S_VERSION}/install.sh' "$kblock"
expect "k3s's install script is told the pinned version" 'INSTALL_K3S_VERSION="$FELIS_K3S_VERSION"' "$kblock"
case "$kblock" in *"https://get.k3s.io"*) echo "FAIL: get.k3s.io serves master's script; read it from the pinned tag"; fails=$((fails + 1)) ;; esac
run_kinst() { # [--keep-binary]
  bash -c '
    curl() { :; }
    sh() { printf "SKIP=[%s] VERSION=%s\n" "$INSTALL_K3S_SKIP_DOWNLOAD" "$INSTALL_K3S_VERSION"; }
    FELIS_K3S_VERSION=v1.36.4+k3s1 K3S_BIN_DIR=/usr/local/bin
    '"$kblock"'
    run_k3s_installer "$@"' _ "$@"
}
expect "finishing a cut-short k3s install keeps the binary in place" "SKIP=[binary] VERSION=v1.36.4+k3s1" "$(run_kinst --keep-binary)"
expect "an install or an upgrade downloads k3s" "SKIP=[] VERSION=v1.36.4+k3s1" "$(run_kinst)"

# An installed k3s moves only under FELIS_UPGRADE_DEPS=1, one minor version at a time and
# never backwards; the refusal names the release to go through first.
kfake="$sdir/k3s"
: > "$sdir/k3s.service"
run_k3s() { # installed-version pinned-version [FELIS_UPGRADE_DEPS]
  printf '#!/bin/sh\necho "k3s version %s (0123abcd)"\necho "go version go1.26"\n' "$1" > "$kfake"
  chmod +x "$kfake"
  K3S_BIN="$kfake" K3S_UNIT_FILE="$sdir/k3s.service" FELIS_K3S_VERSION="$2" FELIS_UPGRADE_DEPS="${3:-0}" bash -c '
    die() { printf "DIE: %s\n" "$*"; exit 1; }
    log() { printf "LOG: %s\n" "$*"; }
    ok() { printf "OK: %s\n" "$*"; }
    configure_k3s_firewall() { :; }
    write_k3s_config() { :; }
    write_k3s_service_dropin() { :; }
    strip_k3s_kubeconfig_mode_flag() { :; }
    run_k3s_installer() { printf "INSTALLER: %s\n" "$FELIS_K3S_VERSION"; }
    stage_k3s_airgap_images() { echo STAGE; }
    systemctl() { :; }
    wait_for_node_ready() { :; }
    chmod() { :; }
    '"$(awk '/^version_newer\(\) \{/,/^}/' "$BS")"'
    '"$(awk '/^k3s_upgrade_allowed\(\) \{/,/^}/' "$BS")"'
    '"$(awk '/^install_k3s\(\) \{/,/^}/' "$BS")"'
    install_k3s'
}
out="$(run_k3s v1.36.4+k3s1 v1.36.4+k3s1 1)"
expect "a k3s at the pin is left alone" "OK: k3s v1.36.4+k3s1 already installed" "$out"
case "$out" in *INSTALLER:*) echo "FAIL: a k3s at the pin must not be reinstalled"; fails=$((fails + 1)) ;; esac
# k3s's image tarball is read as k3s starts, so it is fetched only for a k3s about to start
# on a new version: a rerun downloads nothing.
case "$out" in *STAGE*) echo "FAIL: a k3s left as it is must not have its images downloaded again"; fails=$((fails + 1)) ;; *) echo "PASS a k3s left as it is stages no images" ;; esac
out="$(run_k3s v1.35.2+k3s1 v1.36.4+k3s1)"
expect "an older k3s is reported without the flag" "this release pins v1.36.4+k3s1 (FELIS_UPGRADE_DEPS=1 moves it)" "$out"
case "$out" in *INSTALLER:*) echo "FAIL: an installed k3s must not move without FELIS_UPGRADE_DEPS=1"; fails=$((fails + 1)) ;; esac
expect "FELIS_UPGRADE_DEPS=1 moves k3s up one minor" "INSTALLER: v1.36.4+k3s1" "$(run_k3s v1.35.2+k3s1 v1.36.4+k3s1 1)"
expect "an upgrade stages the new k3s's images before its installer restarts it" "STAGE
INSTALLER: v1.36.4+k3s1" "$(run_k3s v1.35.2+k3s1 v1.36.4+k3s1 1)"
expect "FELIS_UPGRADE_DEPS=1 moves k3s to a newer patch" "INSTALLER: v1.36.4+k3s1" "$(run_k3s v1.36.1+k3s2 v1.36.4+k3s1 1)"
out="$(run_k3s v1.34.6+k3s1 v1.36.4+k3s1 1)"
expect "a k3s upgrade that skips a minor is refused" "DIE: k3s v1.34.6+k3s1 -> v1.36.4+k3s1 skips a minor version" "$out"
expect "the refusal names the minor to go through first" "newest v1.35.x+k3sN release first" "$out"
case "$out" in *INSTALLER:*) echo "FAIL: a skipping k3s upgrade must not run the installer"; fails=$((fails + 1)) ;; esac
out="$(run_k3s v1.37.0+k3s1 v1.36.4+k3s1 1)"
expect "a newer k3s is never downgraded" "OK: k3s v1.37.0+k3s1 is newer than the pinned v1.36.4+k3s1" "$out"
case "$out" in *INSTALLER:*) echo "FAIL: a newer k3s must not be downgraded"; fails=$((fails + 1)) ;; esac
expect "an unreadable k3s version stops the upgrade" "DIE: cannot compare the installed k3s 'dev'" "$(run_k3s dev v1.36.4+k3s1 1)"

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
cblock="$(awk '/^checkout_ref\(\) \{/,/^}/' "$BS")"
hblock="$(awk '/^repo_slug\(\) \{/,/^}/; /^fork_token_hint\(\) \{/,/^}/' "$BS")"
for blk in "$fblock" "$cblock" "$hblock"; do
  [ -n "$blk" ] || { echo "FAIL: fetch_source, checkout_ref or fork_token_hint is missing from $BS"; exit 1; }
  [ "$(printf '%s\n' "$blk" | wc -l)" -lt 40 ] \
    || { echo "FAIL: an extracted block is not the function -- did its closing brace move?"; exit 1; }
done

run_fetch() { # src-dir repo-url
  SRC_DIR="$1" FELIS_REF=main FELIS_REPO_URL="$2" bash -c '
    die() { printf "DIE: %s\n" "$*"; exit 1; }
    log() { :; }
    ok() { :; }
    resolve_install_ref() { :; }
    stamp_version() { :; }
    git_auth() { return 128; }
    git() { :; }
    '"$hblock"'
    '"$cblock"'
    '"$fblock"'
    fetch_source'
}

expect "a failed clone from a fork names the token" "set FELIS_GITHUB_TOKEN" \
  "$(run_fetch "$sdir/src" https://github.com/someone/felis.git)"
out="$(run_fetch "$sdir/src" https://github.com/FelisMC/Felis.git)"
expect "a failed clone from the official repository says what to check" "check that this ref exists" "$out"
case "$out" in
  *private*|*FELIS_GITHUB_TOKEN*) echo "FAIL the official repository is public, so no token is asked for: $out"; fails=$((fails + 1)) ;;
  *) echo "PASS the official repository is public, so no token is asked for" ;;
esac
mkdir -p "$sdir/src/.git"
expect "a failed fetch into an existing checkout of a fork names the token" "set FELIS_GITHUB_TOKEN" \
  "$(run_fetch "$sdir/src" https://github.com/someone/felis.git)"

# --- a rerun takes over whatever sits at SRC_DIR, and an abbreviated commit id ----------------
# Real git against a local repository: git clone refuses a non-empty destination, so a tree
# staged for FELIS_SKIP_FETCH (or left by an interrupted clone) used to stop the rerun, and a
# short sha is no ref a server answers a shallow fetch for.

gdir="$(mktemp -d)"
trap 'rm -f "$jar" "$sfn"; rm -rf "$vdir" "$sdir" "$smtp_dir" "$gdir"' EXIT
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
git init -q -b main "$gdir/up"
for v in one two; do
  echo "$v" >"$gdir/up/a.txt"
  git -C "$gdir/up" add a.txt
  git -C "$gdir/up" -c user.name=t -c user.email=t@example.invalid commit -q -m "$v"
done
first="$(git -C "$gdir/up" rev-parse --short=7 HEAD~1)"
second="$(git -C "$gdir/up" rev-parse --short=7 HEAD)"

run_real_fetch() { # src-dir ref
  SRC_DIR="$1" FELIS_REF="$2" FELIS_REPO_URL="file://$gdir/up" FELIS_GITHUB_TOKEN="" bash -c '
    set -Eeuo pipefail
    die() { printf "DIE: %s\n" "$*"; exit 1; }
    log() { :; }
    ok() { :; }
    resolve_install_ref() { :; }
    stamp_version() { :; }
    '"$gablock"'
    '"$hblock"'
    '"$cblock"'
    '"$fblock"'
    fetch_source
    printf "AT %s %s | %s\n" "$(git -C "$SRC_DIR" rev-parse --short=7 HEAD)" "$(cat "$SRC_DIR/a.txt")" "$(ls -A "$SRC_DIR" | tr "\n" " ")"' 2>&1
}

mkdir -p "$gdir/src"
echo staged >"$gdir/src/staged.txt"
out="$(run_real_fetch "$gdir/src" main)"
expect "a staged tree without .git is replaced by the checkout" "AT ${second} two" "$out"
case "$out" in
  *staged.txt*) echo "FAIL the staged tree's files are gone after the checkout: $out"; fails=$((fails + 1)) ;;
  *) echo "PASS the staged tree's files are gone after the checkout" ;;
esac
expect "a rerun over a shallow checkout takes an abbreviated commit id" "AT ${first} one" \
  "$(run_real_fetch "$gdir/src" "$first")"
# A run killed outright (no EXIT trap) leaves its half-made checkout beside SRC_DIR.
mkdir -p "$gdir/fresh.new.Ab12Cd"
echo killed >"$gdir/fresh.new.Ab12Cd/a.txt"
expect "a fresh clone takes an abbreviated commit id" "AT ${first} one" \
  "$(run_real_fetch "$gdir/fresh" "$first")"
[ ! -e "$gdir/fresh.new.Ab12Cd" ] && echo "PASS   and clears the half-made checkout a killed run left" \
  || { echo "FAIL the half-made checkout a killed run left is still there"; fails=$((fails + 1)); }
# A full clone left a local main behind origin's; with the shallow fetch failing, the whole
# history arm must still land on origin's main.
git clone -q "file://$gdir/up" "$gdir/stale"
git -C "$gdir/stale" reset -q --hard HEAD~1
gablock_noshallow="$gablock
git_auth() { case \" \$* \" in *\" --depth \"*) return 1 ;; esac; GIT_TERMINAL_PROMPT=0 git \"\$@\"; }"
gablock_real="$gablock"; gablock="$gablock_noshallow"
expect "the whole-history arm takes origin's branch over a stale local one" "AT ${second} two" \
  "$(run_real_fetch "$gdir/stale" main)"
gablock="$gablock_real"
mkdir -p "$gdir/kept"
echo staged >"$gdir/kept/staged.txt"
expect "a ref that does not exist stops the install" "DIE: could not check out nope" \
  "$(run_real_fetch "$gdir/kept" nope)"
[ "$(cat "$gdir/kept/staged.txt" 2>/dev/null)" = staged ] \
  && echo "PASS a failed checkout leaves what was there alone" \
  || { echo "FAIL a failed checkout must leave what was there alone"; fails=$((fails + 1)); }
leftover="$(find "$gdir" -maxdepth 1 -name '*.new.*')"
[ -z "$leftover" ] && echo "PASS no half-made checkout is left beside SRC_DIR" \
  || { echo "FAIL a half-made checkout was left behind: $leftover"; fails=$((fails + 1)); }
unset GIT_CONFIG_GLOBAL GIT_CONFIG_NOSYSTEM

# --- default install keeps backups, and retention envs reach the renderer ----------------
# A default install must render the world-archive PVC (without one, backup/restore answer an
# honest 503), and FELIS_WORLDS_HOST_PATH must turn into the reaper's two flags or an
# operator's retention enablement silently renders no CronJob. Extracted, not retyped.

mblock="$(awk '/^  log "rendering \+ applying the control-plane bundle"/,/kube apply -f -/' "$BS")"
[ -n "$mblock" ] || { echo "FAIL: no manifest_args block found in $BS"; exit 1; }
[ "$(printf '%s\n' "$mblock" | wc -l)" -lt 100 ] \
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
    install() { printf "INSTALL %s\n" "$*"; }
    K3S_STORAGE_ROOT="${K3S_STORAGE_ROOT_T:-/var/lib/rancher/k3s/storage}"
    node_global_cidrs() { printf "203.0.113.7/32\n2001:db8::7/128\n"; }
    pvc_size() { case "$2" in registry) printf "20Gi\n" ;; esac; }
    run_bundle() {
    '"$mblock"'
    }
    run_bundle'
}

out="$(run_bundle_flags felis-backups '')"
expect "a default install asks the renderer for the archive PVC" "--backup-pvc
felis-backups" "$out"
# data-durability-17: without a worlds root the reaper still renders, retention-only,
# so backups past their expiry leave the store; it needs the archive mount for that.
expect "a default install passes the archive mount so expired backups are deleted" "--archive-local-path
/var/lib/felis/archives" "$out"
case "$out" in
  *--worlds-host-path*) echo "FAIL: no worlds root may render without FELIS_WORLDS_HOST_PATH"; fails=$((fails + 1)) ;;
esac

expect "a known registry size reaches the renderer" "--registry-storage
20Gi" "$out"
case "$out" in
  *--uploads-storage*|*--backup-storage*) echo "FAIL: an unset size must leave the renderer's default alone"; fails=$((fails + 1)) ;;
esac

out="$(run_bundle_flags '' '')"
expect "an emptied FELIS_BACKUP_PVC is the explicit no-backup shape" "--backup-pvc=" "$out"
case "$out" in
  *--archive-local-path*) echo "FAIL: no archive mount may be passed without an archive PVC"; fails=$((fails + 1)) ;;
esac
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

# On a fresh install the k3s default root does not exist until the provisioner's first
# volume; the installer creates it with k3s's own mode instead of warning about its default.
out="$(K3S_STORAGE_ROOT_T="$missing" run_bundle_flags felis-backups "$missing")"
expect "a missing k3s storage root is created with k3s's mode" "INSTALL -d -m 0700 -o root -g root $missing" "$out"
case "$out" in
  *WARN*) echo "FAIL: the installer's own default root must not be warned about: $out"; fails=$((fails + 1)) ;;
esac

# The reaper reads the root as root with DAC_OVERRIDE through a static PV, so an existing
# root is left exactly as k3s shipped it: uid 1000 is now the game servers' uid, and a
# traverse grant for it on the node's storage root would serve nothing but them.
wdir="$(mktemp -d)"
out="$(run_bundle_flags felis-backups "$wdir")"
case "$out" in
  *SETFACL*|*CHMOD*|*WARN*) echo "FAIL: an existing worlds root must get no grant and no warning: $out"; fails=$((fails + 1)) ;;
esac

# data-durability-18: an older release's traverse grant on the worlds root is taken back on
# every run -- the ACL entry from any root, the other-bits only from k3s's own storage root.
rvblock="$(awk '/^revoke_worlds_root_grant\(\) \{/,/^}/' "$BS")"
[ -n "$rvblock" ] || { echo "FAIL: no revoke_worlds_root_grant found in $BS"; exit 1; }
run_revoke() { # k3s-root worlds-root acl-dir
  K3S_STORAGE_ROOT="$1" FELIS_WORLDS_HOST_PATH="$2" ACL_DIR="$3" bash -c '
    log() { printf "LOG %s\n" "$*"; }
    warn() { printf "WARN %s\n" "$*"; }
    getfacl() { case "$*" in *"$ACL_DIR") printf "user::rwx\nuser:1000:--x\ngroup::---\n" ;; *) printf "user::rwx\ngroup::---\n" ;; esac; }
    setfacl() { printf "SETFACL %s\n" "$*"; }
    '"$rvblock"'
    revoke_worlds_root_grant' 2>&1
}
k3sroot="$(mktemp -d)"; custom="$(mktemp -d)"
command chmod 0701 "$k3sroot"; command chmod 0755 "$custom"
out="$(run_revoke "$k3sroot" "$custom" "$custom")"
expect "the old ACL grant is revoked from a custom worlds root" "SETFACL -x u:1000 $custom" "$out"
expect "the old o+x on k3s's storage root is revoked" "LOG revoked the old world-traversable mode on $k3sroot" "$out"
mode_of() { stat -c %a "$1" 2>/dev/null || stat -f %Lp "$1"; }
if [ "$(mode_of "$k3sroot")" = 700 ]; then echo "PASS k3s's storage root is back to 0700"; else echo "FAIL k3s's storage root is $(mode_of "$k3sroot"), want 700"; fails=$((fails + 1)); fi
if [ "$(mode_of "$custom")" = 755 ]; then echo "PASS a custom worlds root keeps its own mode"; else echo "FAIL a custom worlds root was changed to $(mode_of "$custom")"; fails=$((fails + 1)); fi
out="$(run_revoke "$k3sroot" "" "/nowhere")"
case "$out" in
  *SETFACL*|*LOG*|*WARN*) echo "FAIL: a root with no old grant must be left alone: $out"; fails=$((fails + 1)) ;;
  *) echo "PASS a root with no old grant is left alone" ;;
esac
command rm -rf "$k3sroot" "$custom"

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

pushdir="$(mktemp -d)"
run_push() { # ref [failed-pushes-before-success] [registry-read-only]
  REF="$1" PUSH_FAILS="${2:-0}" READONLY="${3:-0}" COUNT="$pushdir/count" \
  REGISTRY_URL=registry.felis.svc:5000 REGISTRY_PUSH_HOST=127.0.0.1:5000 REGISTRY_DOCKER_CONFIG=/cfg \
  bash -c '
    log() { printf "LOG: %s\n" "$*"; }
    warn() { printf "WARN: %s\n" "$*"; }
    die() { printf "DIE: %s\n" "$*"; exit 1; }
    ok() { :; }
    sleep() { :; }
    systemctl() { :; }
    registry_read_only() { [ "$READONLY" = 1 ]; }
    docker() {
      printf "DOCKER %s\n" "$*"
      case " $* " in
        *" push "*)
          n="$(cat "$COUNT" 2>/dev/null || echo 0)"
          echo $((n + 1)) > "$COUNT"
          [ "$n" -ge "$PUSH_FAILS" ] ;;
      esac
    }
    '"$pblock"'
    push_image_to_registry "$REF"'
  rm -f "$pushdir/count"
}

out="$(run_push registry.felis.svc:5000/felis/felis:demo)"
expect "a registry ref is re-tagged onto the node loopback endpoint" \
  "DOCKER tag registry.felis.svc:5000/felis/felis:demo 127.0.0.1:5000/felis/felis:demo" "$out"
expect "and pushed to exactly that endpoint, with the platform login" "DOCKER --config /cfg push 127.0.0.1:5000/felis/felis:demo" "$out"

out="$(run_push registry.felis.svc:50000/felis/felis:demo)"
expect "a ref outside the registry is refused with a warning" "WARN: not mirroring" "$out"
# The push runs with the platform login, `docker --config <dir> push`, so any docker line
# with a push in it counts.
if printf '%s\n' "$out" | grep -q '^DOCKER .*push '; then
  echo "FAIL a ref outside the registry was pushed: $out"; fails=$((fails + 1))
else
  echo "PASS a ref outside the registry is not pushed"
fi

out="$(run_push registry.felis.svc:5000/felis/felis:demo 1)"
expect "a failed push fails the install loudly" "DIE: could not mirror" "$out"
out="$(run_push registry.felis.svc:5000/felis/felis:demo 2 1)"
expect "a push refused during a GC window is retried" \
  "WARN: the registry is read-only for garbage collection; retrying the push of 127.0.0.1:5000/felis/felis:demo in 30s (2/40)" "$out"
case "$out" in
  *DIE:*) echo "FAIL a push that succeeds after the GC window must not fail the install"; fails=$((fails + 1)) ;;
  *) echo "PASS a push that succeeds after the GC window completes" ;;
esac
expect "a GC window that never ends still fails the install" "DIE: could not mirror" \
  "$(run_push registry.felis.svc:5000/felis/felis:demo 99 1)"
rm -rf "$pushdir"

# docker must be started ONCE for the whole batch: a start/stop pair per image trips
# systemd's start rate limit ("start-limit-hit" — observed live; the 4th image was never
# mirrored because docker.service is socket-triggered and each cycle counts twice). An image
# the release shipped is pushed from its bundle instead, and needs no Docker at all.
wiblock="$(for f in push_images_to_registry role_prebuilt role_image stop_docker; do awk '/^'"$f"'\(\) \{/,/^}/' "$BS"; done)"
case "$wiblock" in *"push_images_to_registry() {"*"role_prebuilt() {"*"role_image() {"*"stop_docker() {"*) ;; *) echo "FAIL: push_images_to_registry or its helpers are missing from $BS"; exit 1 ;; esac
run_batch() { # PREBUILT_ROLES [ARTIFACT_MODE [ARTIFACT_CACHE]]
  FELIS_IMAGE=a FELIS_LIMBO_IMAGE=b FELIS_LOBBY_IMAGE=c FELIS_PAPER_IMAGE=d PREBUILT_ROLES="$1" \
    ARTIFACT_MODE="${2:-}" ARTIFACT_CACHE="${3:-/nonexistent}" bash -c '
    DOCKER_INSTALLED=""
    systemctl() { printf "SYSTEMCTL %s\n" "$*"; }
    ensure_docker() { printf "ENSURE\n"; DOCKER_INSTALLED=1; }
    ctr() { return 1; }
    push_image_to_registry() { printf "PUSH %s\n" "$1"; }
    push_version_tag() { printf "VERSION %s\n" "$1"; }
    push_release_image() { printf "RELEASE %s\n" "$1"; }
    registry_docker_login() { printf "LOGIN\n"; }
    '"$wiblock"'
    push_images_to_registry'
}
out="$(run_batch " ")"
expect "the batch logs in to the registry gate before pushing" "ENSURE
LOGIN
PUSH a" "$out"
starts="$(printf '%s\n' "$out" | grep -c 'ENSURE')"
stops="$(printf '%s\n' "$out" | grep -c 'SYSTEMCTL stop docker')"
[ "$starts" = 1 ] && [ "$stops" = 1 ] && [ "$(printf '%s\n' "$out" | grep -c '^PUSH')" = 4 ] \
  && echo "PASS the batch wraps all four pushes in ONE docker start/stop" \
  || { echo "FAIL: expected 1 start / 1 stop / 4 pushes, got:"; printf '%s\n' "$out"; fails=$((fails + 1)); }
[ "$(printf '%s\n' "$out" | grep '^VERSION' | tr '\n' ' ')" = "VERSION b VERSION c VERSION d " ] \
  && echo "PASS the three game images, and only they, also get a version tag" \
  || { echo "FAIL: expected version tags for b c d only, got:"; printf '%s\n' "$out"; fails=$((fails + 1)); }
out="$(run_batch " felis limbo ")"
expect "release images are pushed from their bundles, the rest with docker" "RELEASE felis
RELEASE limbo
ENSURE
LOGIN
PUSH c
VERSION c
PUSH d
VERSION d
SYSTEMCTL stop docker docker.socket" "$out"
case "$out" in *"PUSH a"*|*"PUSH b"*) echo "FAIL: a release image must not also go through docker push"; fails=$((fails + 1)) ;; *) echo "PASS a release image is pushed once" ;; esac
cachedir="$(mktemp -d)"
out="$(run_batch " felis limbo lobby paper " release "$cachedir")"
[ "$(printf '%s\n' "$out" | grep -c '^RELEASE')" = 4 ] && [ "$(printf '%s\n' "$out" | grep -c '^RELEASE')" = "$(printf '%s\n' "$out" | wc -l | tr -d ' ')" ] \
  && echo "PASS an install from release images pushes without Docker, and never starts or stops it" \
  || { echo "FAIL: an all-release batch touched docker:"; printf '%s\n' "$out"; fails=$((fails + 1)); }
[ ! -e "$cachedir" ] && echo "PASS the downloaded bundles are removed once they are pushed" \
  || { echo "FAIL: ${cachedir} was left behind after the pushes"; fails=$((fails + 1)); rm -rf "$cachedir"; }

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

# The registry pod's two images and the database's can only come from containerd's own
# store: pin all three against kubelet image GC, and unpin previous ones.
pnblock="$(awk '/^pin_platform_images\(\) \{/,/^}/' "$BS")"
[ -n "$pnblock" ] || { echo "FAIL: no pin_platform_images found in $BS"; exit 1; }
rrblock="$(awk '/^pinned_image_ref\(\) \{/,/^}/' "$BS")"
[ -n "$rrblock" ] || { echo "FAIL: no pinned_image_ref found in $BS"; exit 1; }
regimage="$(grep -m1 '^REGISTRY_IMAGE=' "$BS" | cut -d'"' -f2)"
regdigest="${regimage#*@}"
pgimage="$(grep -m1 '^POSTGRES_IMAGE=' "$BS" | cut -d'"' -f2)"
pgdigest="${pgimage#*@}"
[ -n "$pgimage" ] && [ "$pgdigest" != "$pgimage" ] || { echo "FAIL: no digest-pinned POSTGRES_IMAGE in $BS"; exit 1; }
calls="$(mktemp)"
out="$(
  CALLS="$calls" REGISTRY_IMAGE="$regimage" REGDIGEST="$regdigest" POSTGRES_IMAGE="$pgimage" PGDIGEST="$pgdigest" \
    FELIS_IMAGE=registry.felis.svc:5000/felis/felis:v2 bash -c '
    ok() { printf "OK: %s\n" "$*"; }
    warn() { printf "WARN: %s\n" "$*"; }
    k3s_cmd() {
      case "$*" in
        "ctr images ls -q") printf "registry.felis.svc:5000/felis/felis:v1\nregistry.felis.svc:5000/felis/felis:v2\ndocker.io/library/registry:2\ndocker.io/library/registry@%s\nregistry.felis.svc:5000/felis/limbo:demo\ndocker.io/library/postgres@sha256:0000\ndocker.io/library/postgres@%s\n" "$REGDIGEST" "$PGDIGEST" ;;
        *) printf "CTR %s\n" "$*" >>"$CALLS" ;;
      esac
    }
    '"$rrblock"'
    '"$pnblock"'
    pin_platform_images'
)$(printf '\n'; cat "$calls")"
rm -f "$calls"
expect "the running felis image is pinned" "CTR ctr images label registry.felis.svc:5000/felis/felis:v2 io.cri-containerd.pinned=pinned" "$out"
expect "the registry image is pinned by digest" "CTR ctr images label docker.io/library/registry@${regdigest} io.cri-containerd.pinned=pinned" "$out"
expect "the database image is pinned by digest" "CTR ctr images label docker.io/library/postgres@${pgdigest} io.cri-containerd.pinned=pinned" "$out"
expect "a previous felis tag is unpinned" "CTR ctr images label registry.felis.svc:5000/felis/felis:v1 io.cri-containerd.pinned=" "$out"
expect "the old registry:2 tag is unpinned" "CTR ctr images label docker.io/library/registry:2 io.cri-containerd.pinned=" "$out"
expect "a previous database image is unpinned" "CTR ctr images label docker.io/library/postgres@sha256:0000 io.cri-containerd.pinned=" "$out"
# $'\n' is bash; this file runs under dash in CI.
nl='
'
case "$out" in
  *"registry@${regdigest} io.cri-containerd.pinned=${nl}"*) echo "FAIL: the current registry image must not be unpinned"; fails=$((fails + 1)) ;;
esac
case "$out${nl}" in
  *"postgres@${pgdigest} io.cri-containerd.pinned=${nl}"*) echo "FAIL: the current database image must not be unpinned"; fails=$((fails + 1)) ;;
esac
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

# --- the registry's and the database's own images must not be re-pulled on every run -----
iblock="$(awk '/^import_platform_images\(\) \{/,/^}/' "$BS")"
[ -n "$iblock" ] || { echo "FAIL: no import_platform_images found in $BS"; exit 1; }

run_import() { # listed-refs
  LISTED="$1" REGISTRY_IMAGE="$regimage" POSTGRES_IMAGE="$pgimage" bash -c '
    log() { printf "LOG: %s\n" "$*"; }
    ok() { printf "OK: %s\n" "$*"; }
    warn() { printf "WARN: %s\n" "$*"; }
    die() { printf "DIE: %s\n" "$*"; exit 1; }
    k3s_cmd() { case "$*" in "ctr images ls -q") printf "%b" "$LISTED" ;; *) printf "PULL %s\n" "$*" >&2 ;; esac; }
    '"$rrblock"'
    '"$iblock"'
    import_platform_images' 2>&1
}
out="$(run_import "docker.io/library/registry@${regdigest}\ndocker.io/library/postgres@${pgdigest}\n")"
expect "an already-pulled registry image is left alone" "OK: ${regimage} already in k3s containerd" "$out"
expect "an already-pulled database image is left alone" "OK: ${pgimage} already in k3s containerd" "$out"
case "$out" in
  *PULL*) echo "FAIL: a present platform image must not be pulled again"; fails=$((fails + 1)) ;;
esac
out="$(run_import "docker.io/library/registry:2\ndocker.io/library/postgres:18\n")"
expect "only the pinned digest counts as present; the old registry tag is pulled over" "PULL crictl pull ${regimage}" "$out"
expect "only the pinned digest counts as present; the old database tag is pulled over" "PULL crictl pull ${pgimage}" "$out"

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

# --- write_file_atomic leaves the old file whole when a write fails ----------------------
# secrets.env, offsite.env and felis.host.toml were written with a plain `cat >`: a full
# disk or a crash halfway left a truncated file that the next run took as the truth.

wablock="$(awk '/^write_file_atomic\(\) \{/,/^}/' "$BS")"
[ -n "$wablock" ] || { echo "FAIL: no write_file_atomic found in $BS"; exit 1; }
[ "$(printf '%s\n' "$wablock" | wc -l)" -lt 20 ] \
  || { echo "FAIL: the extracted block is not write_file_atomic -- did its closing brace move?"; exit 1; }
wafile="$(mktemp)"
printf '%s\n' "$wablock" > "$wafile"
wadir="$(mktemp -d)"
run_atomic() { # script; under the installer's shell options, its temp files removed on exit as cleanup does
  FNFILE="$wafile" bash -c '
    set -Eeuo pipefail
    die() { printf "DIE: %s\n" "$*"; exit 1; }
    TEMP_PATHS=()
    remember_temp() { TEMP_PATHS+=("$1"); }
    trap '\''for p in "${TEMP_PATHS[@]-}"; do [ -z "$p" ] || rm -f -- "$p"; done'\'' EXIT
    . "$FNFILE"
    '"$1" 2>&1
}
mode_of() { ls -l "$1" | cut -c1-10; }

printf 'A=1\nB=2\n' > "$wadir/in.new"
printf 'DB_PASSWORD=new\nSERVICE_TOKEN=new\n' > "$wadir/in.replace"
out="$(run_atomic "umask 000; write_file_atomic '$wadir/new.env' 0600 < '$wadir/in.new'; echo done")"
expect "an atomic write finishes" "done" "$out"
expect "an atomic write holds all of its input" "$(printf 'A=1\nB=2')" "$(cat "$wadir/new.env")"
expect "an atomic write is private under a permissive umask" "-rw-------" "$(mode_of "$wadir/new.env")"
run_atomic "write_file_atomic '$wadir/new.env' 0640 < '$wadir/in.new'" >/dev/null
expect "an atomic write sets the mode it is given" "-rw-r-----" "$(mode_of "$wadir/new.env")"

printf 'DB_PASSWORD=old-and-whole\nSERVICE_TOKEN=kept\n' > "$wadir/old.env"
out="$(run_atomic "cat() { head -c 7; return 1; }; write_file_atomic '$wadir/old.env' 0600 < '$wadir/in.replace'; echo survived")"
expect "a write that fails halfway dies" "DIE: could not write $wadir/old.env; it is left as it was" "$out"
expect "a write that fails halfway leaves the old file whole" \
  "$(printf 'DB_PASSWORD=old-and-whole\nSERVICE_TOKEN=kept')" "$(cat "$wadir/old.env")"
out="$(run_atomic "sync() { return 1; }; write_file_atomic '$wadir/old.env' 0600 < '$wadir/in.replace'")"
expect "content that did not reach the disk does not replace the old file" "DIE: could not write $wadir/old.env" "$out"
expect "the old file survives a failed sync" "DB_PASSWORD=old-and-whole" "$(cat "$wadir/old.env")"
left=""
for p in "$wadir"/old.env.*; do
  if [ -e "$p" ]; then left="$left${left:+ }${p##*/}"; fi
done
if [ -z "$left" ]; then
  echo "PASS a failed write leaves no temp file beside the old one"
else
  echo "FAIL a failed write left $left beside old.env"; fails=$((fails + 1))
fi
rm -rf "$wadir" "$wafile"

# The installer never runs load_or_make_secrets alone (it would mint real secrets), so
# its writer is checked by what it calls.
lsblock="$(awk '/^load_or_make_secrets\(\) \{/,/^}/' "$BS")"
expect "secrets.env is written whole or not at all" 'write_file_atomic "$SECRETS_ENV" 0600 <<EOF' "$lsblock"
case "$lsblock" in
  *'> "$SECRETS_ENV"'*) echo "FAIL load_or_make_secrets still writes secrets.env in place"; fails=$((fails + 1)) ;;
  *) echo "PASS load_or_make_secrets writes secrets.env nowhere else" ;;
esac

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
palblock="$(awk '/^persisted_auth_lines\(\) \{/,/^}/' "$BS")"
ahblock="$(awk '/^auth_hostname\(\) \{/,/^}/' "$BS")"
alblock="$(awk '/^auth_lines\(\) \{/,/^}/' "$BS")"
{ [ -n "$wrblock" ] && [ -n "$prblock" ] && [ -n "$pablock" ] && [ -n "$poblock" ] && [ -n "$oblock" ] &&
  [ -n "$palblock" ] && [ -n "$ahblock" ] && [ -n "$alblock" ]; } \
  || { echo "FAIL: write_felis_toml / persisted_{registry,archive,offsite}_block / offsite_block / the [auth] helpers not found in $BS"; exit 1; }
# The blocks quote themselves (the awk program uses single quotes), so they are
# sourced from a file instead of being spliced into a single-quoted bash -c.
fnfile="$(mktemp)"
printf '%s\n' "$prblock" "$pablock" "$poblock" "$oblock" "$palblock" "$ahblock" "$alblock" "$wablock" "$wrblock" > "$fnfile"

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
scan_fail_on = ["CRITICAL"]
scan_fail_unfixed = true
scan_accept = ["CVE-2021-35515", "CVE-2025-67030"]

[registry.s3]
endpoint = "https://s3.example"
region = "us-east-1"

[archive]
store = "tarLocal"
local_path = "/stale/path"
retention = "30d"
manual_keep = 3
manual_cooldown = "1h"
scheduled_every = "12h"
scheduled_keep = 14
scheduled_retention = "45d"

[offsite]
endpoint = "https://objects.example"
bucket = "felis-offsite"
TOML

run_write() { # out-file [state-dir] [database-deployment]; under the installer's shell options and ERR trap
  STATE_DIR="${2:-$rdir}" OUT_TOML="$1" DEPLOY="${3:-}" FNFILE="$fnfile" bash -c '
    set -Eeuo pipefail
    trap '\''echo "ERR near line $LINENO (exit $?)" >&2'\'' ERR
    log() { :; }
    die() { printf "DIE: %s\n" "$*" >&2; exit 1; }
    remember_temp() { :; }
    [ -z "${CAT_FAILS:-}" ] || cat() { head -c 40; return 1; }
    persisted_smtp_block() { :; }
    persisted_auth_source_blocks() { :; }
    . "$FNFILE"
    FELIS_ROOT_DOMAIN=r.example.com DB_USER=u DB_PASSWORD=p DB_NAME=d MINECRAFT_NS=minecraft \
    FELIS_EGRESS_MODE=nodeport FELIS_LIMBO_IMAGE=li FELIS_LOBBY_IMAGE=lo FELIS_GAME_PORT=25570 \
    REGISTRY_URL=registry.felis.svc:5000 BUILD_NS=felis-build FELIS_ARCHIVE_LOCAL_PATH=/a \
    FELIS_OFFSITE_BUCKET= write_felis_toml "$OUT_TOML" 127.0.0.1:15432 "$DEPLOY"'
}

run_write "$rdir/out.toml"
out="$(cat "$rdir/out.toml")"
expect "the database is reached at the address given" '[database]
url = "postgres://u:p@127.0.0.1:15432/d?sslmode=disable"

[k8s]' "$out"
run_write "$rdir/deploy.toml" "$rdir" felis/felis-postgres
expect "the host copy names the pod felis db runs its tools in" '[database]
url = "postgres://u:p@127.0.0.1:15432/d?sslmode=disable"
# The database'"'"'s pod: felis db runs its client tools there.
deployment = "felis/felis-postgres"

[k8s]' "$(cat "$rdir/deploy.toml")"
expect "a re-run carries the build-lane executor mirrors" \
  'kaniko_image = "registry.felis.svc:5000/mirror/kaniko-executor:v1.24.0"' "$out"
expect "a re-run carries the trivy vulnerability-DB mirror" \
  'trivy_db_repository = "registry.felis.svc:5000/mirror/trivy-db:2"' "$out"
expect "a re-run carries the trivy java-DB mirror" \
  'trivy_java_db_repository = "registry.felis.svc:5000/mirror/trivy-java-db:1"' "$out"
expect "a re-run carries the build disk cap" 'build_disk_limit = "20Gi"' "$out"
expect "a re-run carries the build user-namespace mode" 'build_user_namespaces = "off"' "$out"
expect "a re-run carries the build runtime class" 'build_runtime_class = "gvisor"' "$out"
expect "a re-run carries the scan gate's blocking severities" 'scan_fail_on = ["CRITICAL"]' "$out"
expect "a re-run carries the scan gate's unfixed-vulnerability rule" 'scan_fail_unfixed = true' "$out"
expect "a re-run carries the scan gate's accepted finding ids" 'scan_accept = ["CVE-2021-35515", "CVE-2025-67030"]' "$out"
expect "a re-run carries the [registry.s3] uploads subtable" "[registry.s3]" "$out"
expect "the carried subtable keeps its keys" 'endpoint = "https://s3.example"' "$out"
expect "url stays installer-owned" 'url = "registry.felis.svc:5000"' "$out"
expect "a re-run carries the archive retention window" 'retention = "30d"' "$out"
expect "a re-run carries the on-demand backup count" 'manual_keep = 3' "$out"
expect "a re-run carries the on-demand backup cooldown" 'manual_cooldown = "1h"' "$out"
expect "a re-run carries the scheduled backup period" 'scheduled_every = "12h"' "$out"
expect "a re-run carries the scheduled backup count" 'scheduled_keep = 14' "$out"
expect "a re-run carries the scheduled backup retention" 'scheduled_retention = "45d"' "$out"
expect "the archive mount stays installer-owned" 'local_path = "/a"' "$out"
expect "the panel learns the public game port" 'game_port = 25570' "$out"
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
expect "the config is private: its url holds the database password" "-rw-------" "$(ls -l "$rdir/out.toml" | cut -c1-10)"
# A re-run that cannot finish writing (a full disk) keeps the config it carries from.
out="$(CAT_FAILS=1 run_write "$rdir/felis.host.toml" 2>&1 || true)"
expect "a config write that fails halfway dies" "DIE: could not write $rdir/felis.host.toml; it is left as it was" "$out"
if cmp -s "$rdir/out.toml" "$rdir/felis.host.toml"; then
  echo "PASS a config write that fails halfway leaves the old config whole"
else
  echo "FAIL a failed write cut felis.host.toml short:"; head -3 "$rdir/felis.host.toml"; fails=$((fails + 1))
fi
rm -f "$rdir"/felis.host.toml.*

# --- installer re-runs keep [auth]; a different root domain is refused ------------------
# The Cloudflare edge setup writes access_jwt_aud and client_ip_header into [auth] (the
# header is what the sign-in rate limit keys on), and an operator may serve the admin
# console on a name of their own. A re-run rewrote [auth] from the root domain and dropped
# all of it. The hostnames also feed the proxy's felis-link.properties and the panel
# certificate, which must follow the carried names.

adir="$(mktemp -d)"
cat > "$adir/felis.host.toml" <<'TOML'
[server]
root_domain = "r.example.com"

[auth]
admin_hostname = "ops.example.org"
panel_hostname = "console.r.example.com"
access_jwt_aud = "aud-0123"
client_ip_header = "CF-Connecting-IP"

[smtp]
host = "mail.example"
TOML
err="$(run_write "$adir/out.toml" "$adir" 2>&1)"
if [ -z "$err" ]; then
  echo "PASS writing the config runs nothing (no command substitution in the template)"
else
  echo "FAIL: writing the config printed:"; printf '%s\n' "$err"; fails=$((fails + 1))
fi
out="$(cat "$adir/out.toml")"
expect "a re-run keeps every [auth] key, the edge's and the operator's" '[auth]
admin_hostname = "ops.example.org"
panel_hostname = "console.r.example.com"
access_jwt_aud = "aud-0123"
client_ip_header = "CF-Connecting-IP"

' "$out"
case "$out" in
  *op.console.r.example.com*)
    echo "FAIL: a derived admin hostname was written next to the carried one:"; printf '%s\n' "$out"; fails=$((fails + 1)) ;;
  *mail.example*)
    echo "FAIL: the [auth] carry ran into the next section:"; printf '%s\n' "$out"; fails=$((fails + 1)) ;;
  *) echo "PASS the carried names replace the derived ones and the carry stops at [smtp]" ;;
esac
cp "$adir/out.toml" "$adir/felis.host.toml"
run_write "$adir/out2.toml" "$adir"
if cmp -s "$adir/out.toml" "$adir/out2.toml"; then
  echo "PASS a carried-forward [auth] converges (the second re-run is a no-op)"
else
  echo "FAIL: carrying [auth] is not idempotent"; diff "$adir/out.toml" "$adir/out2.toml" | head
  fails=$((fails + 1))
fi

printf '[server]\nroot_domain = "r.example.com"\n\n[auth]\naccess_jwt_aud = "aud-0123"\n' > "$adir/felis.host.toml"
run_write "$adir/out.toml" "$adir"
expect "an [auth] without the hostnames gets the derived ones and keeps the rest" '[auth]
admin_hostname = "op.console.r.example.com"
panel_hostname = "console.r.example.com"
access_jwt_aud = "aud-0123"' "$(cat "$adir/out.toml")"
rm -f "$adir/felis.host.toml"
run_write "$adir/out.toml" "$adir"
expect "a first install writes the two derived hostnames" '[auth]
admin_hostname = "op.console.r.example.com"
panel_hostname = "console.r.example.com"

' "$(cat "$adir/out.toml")"

# The proxy's link properties and the panel certificate take the carried names.
wvblock="$(awk '/^write_velocity_config\(\) \{/,/^}/' "$BS")"
ptblock="$(awk '/^ensure_panel_tls_cert\(\) \{/,/^}/' "$BS")"
iicblock="$(awk '/^install_if_changed\(\) \{/,/^}/' "$BS")"
{ [ -n "$wvblock" ] && [ -n "$ptblock" ] && [ -n "$iicblock" ]; } \
  || { echo "FAIL: write_velocity_config / ensure_panel_tls_cert / install_if_changed not found in $BS"; exit 1; }
printf '%s\n' "$iicblock" "$wvblock" "$ptblock" >> "$fnfile"
cat > "$adir/felis.host.toml" <<'TOML'
[auth]
admin_hostname = "ops.example.org"
panel_hostname = "play.example.org"
TOML
run_surfaces() { # state-dir out-dir; under the installer's shell options and ERR trap
  STATE_DIR="$1" VOUT="$2" TMPDIR="$2" FNFILE="$fnfile" bash -c '
    set -Eeuo pipefail
    trap '\''echo "ERR near line $LINENO (exit $?)" >&2'\'' ERR
    log() { :; }; ok() { :; }; remember_temp() { :; }
    felis_internal_ip() { printf 10.43.0.1; }
    prepare_velocity_layout() { :; }
    atomic_install_file() { cp "$1" "$VOUT/$(basename "$2")"; }
    openssl() { for a in "$@"; do :; done; cp "$a" "$VOUT/openssl.cnf"; touch "$VOUT/k" "$VOUT/c"; }
    . "$FNFILE"
    FELIS_ROOT_DOMAIN=r.example.com FORWARDING_SECRET=f SERVICE_TOKEN=t LOGIN_SERVER=login \
    LOBBY_SERVER=lobby FELIS_GAME_PORT=25565 VELOCITY_DIR=/v VELOCITY_USER=v NODE_IP=10.0.0.5 \
    PANEL_TLS_CERT="$VOUT/c" PANEL_TLS_KEY="$VOUT/k"
    write_velocity_config
    ensure_panel_tls_cert'
}
mkdir "$adir/v"
run_surfaces "$adir" "$adir/v"
out="$(cat "$adir/v/felis-link.properties" 2>&1)"
expect "the proxy routes the carried panel hostname" "panel-hostname=play.example.org" "$out"
expect "the proxy routes the carried admin hostname" "admin-hostname=ops.example.org" "$out"
expect "the proxy keeps the root domain" "root-domain=r.example.com" "$out"
out="$(cat "$adir/v/openssl.cnf" 2>&1)"
expect "a regenerated panel certificate names the carried hostnames" 'DNS.1 = ops.example.org
DNS.2 = play.example.org' "$out"

# The hostnames on the section's first lines and the section far past one pipe buffer:
# a `printf | grep -q` or `| awk exit` over it has the reader exit while printf is still
# writing, which is certain here and a scheduling race on a real host (offsite_enabled
# lost it, see the off-site section). The config must still carry each name once, and
# the helpers must fail nothing along the way (the installer logs every failed command).
{ printf '[auth]\nadmin_hostname = "ops.example.org"\npanel_hostname = "play.example.org"\n'
  seq 1 200000 | sed 's/.*/k& = "v"/'; } > "$adir/felis.host.toml"
err="$(run_write "$adir/out.toml" "$adir" 2>&1)"
for key in admin_hostname panel_hostname; do
  got="$(grep "^${key} = " "$adir/out.toml")"
  case "$got" in
    *"$nl"*) echo "FAIL: a long carried [auth] wrote ${key} twice:"; printf '%s\n' "$got"; fails=$((fails + 1)) ;;
    "") echo "FAIL: a long carried [auth] lost ${key}"; fails=$((fails + 1)) ;;
    *) echo "PASS a long carried [auth] writes ${key} once: ${got}" ;;
  esac
done
expect "a long carried [auth] keeps the operator's admin name" 'admin_hostname = "ops.example.org"' "$(grep '^admin_hostname' "$adir/out.toml")"
rm -rf "$adir/v"; mkdir "$adir/v"
err="${err}$(run_surfaces "$adir" "$adir/v" 2>&1)"
expect "a long carried [auth] still routes the carried panel hostname" "panel-hostname=play.example.org" "$(cat "$adir/v/felis-link.properties" 2>&1)"
if [ -z "$err" ]; then
  echo "PASS reading a long [auth] fails no command"
else
  echo "FAIL: reading a long [auth] printed:"; printf '%s\n' "$err" | head -5; fails=$((fails + 1))
fi

# A re-run that writes the same felis-link.properties leaves the file alone: felis domain
# check reads a proxy started before the file's mtime as still on the old names. stat answers
# as for the file the first install left, root:v 0640, which the test's user cannot make.
run_link() { # velocity-dir [root-domain]
  VD="$1" RD="${2:-r.example.com}" TMPDIR="$1" FNFILE="$fnfile" bash -c '
    set -Eeuo pipefail
    log() { :; }; ok() { :; }; remember_temp() { :; }
    felis_internal_ip() { printf 10.43.0.1; }
    prepare_velocity_layout() { :; }
    atomic_install_file() { echo "REPLACED $(basename "$2")"; cp "$1" "$2"; }
    chown() { echo "CHOWN $*"; }; chmod() { echo "CHMOD $*"; }
    stat() { echo "root:v 640"; }
    . "$FNFILE"
    STATE_DIR="$VD" FELIS_ROOT_DOMAIN="$RD" FORWARDING_SECRET=f SERVICE_TOKEN=t LOGIN_SERVER=login \
    LOBBY_SERVER=lobby FELIS_GAME_PORT=25565 VELOCITY_DIR="$VD" VELOCITY_USER=v NODE_IP=10.0.0.5
    write_velocity_config' 2>&1
}
ldir2="$(mktemp -d)"
mkdir -p "$ldir2/plugins/felis-link"
lprops="$ldir2/plugins/felis-link/felis-link.properties"
expect "a first write installs felis-link.properties" "REPLACED felis-link.properties" "$(run_link "$ldir2")"
touch -t 202001010000 "$lprops"
before="$(ls -l --time-style=+%s "$lprops" 2>/dev/null || stat -f '%m' "$lprops")"
out="$(run_link "$ldir2")"
case "$out" in
  *"REPLACED felis-link.properties"*) echo "FAIL: a re-run with the same names replaced felis-link.properties"; fails=$((fails + 1)) ;;
  *) echo "PASS a re-run with the same names leaves felis-link.properties in place" ;;
esac
case "$out" in
  *CHOWN*|*CHMOD*) echo "FAIL: the re-run changed felis-link.properties by path:"; printf '%s\n' "$out"; fails=$((fails + 1)) ;;
  *) echo "PASS the re-run changes nothing by path" ;;
esac
expect "the kept file keeps its mtime" "$before" "$(ls -l --time-style=+%s "$lprops" 2>/dev/null || stat -f '%m' "$lprops")"
expect "a re-run on other names replaces felis-link.properties" "REPLACED felis-link.properties" "$(run_link "$ldir2" other.example.net)"
expect "the replaced file has the new root domain" "root-domain=other.example.net" "$(grep '^root-domain=' "$lprops")"
rm -rf "$ldir2"

# A different FELIS_ROOT_DOMAIN on an installed host is refused with the way through.
dnblock="$(awk '/^detect_node_ip\(\) \{/,/^}/' "$BS")"
prdblock="$(awk '/^persisted_root_domain\(\) \{/,/^}/' "$BS")"
{ [ -n "$dnblock" ] && [ -n "$prdblock" ]; } || { echo "FAIL: detect_node_ip / persisted_root_domain not found in $BS"; exit 1; }
printf '%s\n' "$prdblock" "$dnblock" "$(awk '/^ipv4_to_int\(\) \{/,/^}/' "$BS")" > "$fnfile"
run_detect() { # state-dir [FELIS_ROOT_DOMAIN]; IPOUT and HOSTOUT stand in for ip route get and hostname -I
  STATE_DIR="$1" RD="${2:-}" FNFILE="$fnfile" IPOUT="${IPOUT-1.1.1.1 via 10.0.0.1 dev eth0 src 10.0.0.5 uid 0}" HOSTOUT="${HOSTOUT:-}" bash -c '
    die() { printf "DIE: %s\n" "$*"; exit 1; }
    log() { printf "LOG: %s\n" "$*"; }
    warn() { printf "WARN: %s\n" "$*"; }
    ip() { [ -z "$IPOUT" ] || printf "%s\n" "$IPOUT"; }
    hostname() { [ "$1" = -I ] && printf "%s \n" "$HOSTOUT"; }
    . "$FNFILE"
    if [ -n "$RD" ]; then FELIS_ROOT_DOMAIN="$RD"; fi
    detect_node_ip
    echo "ROOT=$FELIS_ROOT_DOMAIN"'
}
printf '[server]\nroot_domain = "r.example.com"\n' > "$adir/felis.host.toml"
out="$(run_detect "$adir" new.example.net)"
expect "a different FELIS_ROOT_DOMAIN is refused" "DIE: FELIS_ROOT_DOMAIN (new.example.net) differs from the installed r.example.com" "$out"
expect "the refusal names the command that moves the install" "sudo felis domain set new.example.net" "$out"
case "$out" in
  *ROOT=*) echo "FAIL: the installer went on after refusing:"; printf '%s\n' "$out"; fails=$((fails + 1)) ;;
esac
expect "the installed domain is reused when named again" "ROOT=r.example.com" "$(run_detect "$adir" r.example.com)"
expect "the installed domain is reused when unset" "ROOT=r.example.com" "$(run_detect "$adir")"
rm -f "$adir/felis.host.toml"
expect "a first install takes FELIS_ROOT_DOMAIN" "ROOT=new.example.net" "$(run_detect "$adir" new.example.net)"
expect "a first install defaults to nip.io" "ROOT=10.0.0.5.nip.io" "$(run_detect "$adir")"
# Without an IPv4 default route, hostname -I is the fallback; it lists IPv6 addresses
# too, and the first IPv4 one is taken.
expect "the fallback skips IPv6 addresses" "ROOT=192.168.1.20.nip.io" \
  "$(IPOUT='' HOSTOUT='fd00::5 2001:db8::20 192.168.1.20 10.42.0.1' run_detect "$adir")"
expect "a host with IPv6 addresses alone is refused" "DIE: could not determine this host's primary IPv4 address" \
  "$(IPOUT='' HOSTOUT='fd00::5 2001:db8::20' run_detect "$adir")"

rm -rf "$adir"
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
    PG_HOST_PORT=15432 CONTROL_NS=felis PG_DEPLOYMENT=felis-postgres HOST_BIN=fakefelis bash -c '
    log() { :; }; ok() { :; }; warn() { printf "WARN: %s\n" "$*"; }
    write_felis_toml() { printf "TOML: %s\n" "$*"; }; ensure_default_config() { :; }
    fakefelis() { printf "RUN: %s\n" "$*"; }
    '"$mblock"'
    run_migrations' 2>&1
}

out="$(run_migrate 1)"
expect "the migrations reach felis-postgres at its loopback port and name its pod" \
  "TOML: /etc/felis/felis.host.toml 127.0.0.1:15432 felis/felis-postgres" "$out"
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
    log() { printf "LOG: %s\n" "$*"; }; die() { printf "DIE: %s\n" "$*"; exit 1; }
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

ublock="$(awk '/^install_update_check_timer\(\) \{/,/^}/' "$BS")"
[ -n "$ublock" ] || { echo "FAIL: no install_update_check_timer found in $BS"; exit 1; }
[ "$(printf '%s\n' "$ublock" | wc -l)" -lt 60 ] \
  || { echo "FAIL: the extracted block is not install_update_check_timer -- did its closing brace move?"; exit 1; }
udir="$(mktemp -d)"
out="$(UPDATE_CHECK_SERVICE="$udir/felis-update-check.service" UPDATE_CHECK_TIMER="$udir/felis-update-check.timer" \
  HOST_BIN=/usr/local/bin/felis STATE_DIR=/etc/felis bash -c '
  ok() { printf "OK: %s\n" "$*"; }; warn() { printf "WARN: %s\n" "$*"; }
  systemctl() { printf "SYSTEMCTL: %s\n" "$*"; }
  '"$ublock"'
  install_update_check_timer' 2>&1)"
unit="$(cat "$udir/felis-update-check.service")"
timer="$(cat "$udir/felis-update-check.timer")"
expect "the version check records its result for the panel" \
  "ExecStart=/usr/local/bin/felis update --record -config /etc/felis/felis.host.toml" "$unit"
expect "the version check is a oneshot" "Type=oneshot" "$unit"
expect "the version check runs daily" "OnCalendar=*-*-* 05:30:00" "$timer"
expect "a missed check catches up at boot" "Persistent=true" "$timer"
expect "the version check timer is enabled" "SYSTEMCTL: enable --now felis-update-check.timer" "$out"
expect "the first check runs without holding up the install" "SYSTEMCTL: start --no-block felis-update-check.service" "$out"
expect "the install says where the result shows" "OK: version check: daily; the panel's Updates page" "$out"
rm -rf "$udir"
order="$(awk '/^main\(\) \{/,/^}/' "$BS" | grep -nE '^[[:space:]]*(install_velocity|install_update_check_timer)$' | tr '\n' ' ')"
case "$order" in
  *install_velocity*install_update_check_timer*) echo "PASS the version check is installed after the proxy it reads" ;;
  *) echo "FAIL the version check must be installed after install_velocity: $order"; fails=$((fails + 1)) ;;
esac

wblock="$(awk '/^install_watchdog_timer\(\) \{/,/^}/' "$BS")"
[ -n "$wblock" ] || { echo "FAIL: no install_watchdog_timer found in $BS"; exit 1; }
[ "$(printf '%s\n' "$wblock" | wc -l)" -lt 90 ] \
  || { echo "FAIL: the extracted block is not install_watchdog_timer -- did its closing brace move?"; exit 1; }
qblock="$(awk '/^quiet_watchdog\(\) \{/,/^}/' "$BS")"
[ -n "$qblock" ] || { echo "FAIL: no quiet_watchdog found in $BS"; exit 1; }
hbblock=""
for fn in write_heartbeat_url heartbeat_host summary_heartbeat validate_heartbeat_url; do
  blk="$(awk "/^${fn}\\(\\) \\{/,/^}/" "$BS")"
  [ -n "$blk" ] || { echo "FAIL: no ${fn} found in $BS"; exit 1; }
  [ "$(printf '%s\n' "$blk" | wc -l)" -lt 30 ] \
    || { echo "FAIL: the extracted block is not ${fn} -- did its closing brace move?"; exit 1; }
  hbblock="${hbblock}${blk}
"
done

tdir="$(mktemp -d)"
run_watchdog_timer() { # $1: exit status of the first run, $2: FELIS_WORLDS_HOST_PATH, $3: NODE_IP
  FIRST="$1" FELIS_WORLDS_HOST_PATH="$2" NODE_IP="${3:-}" WATCHDOG_SERVICE="$tdir/felis-watchdog.service" WATCHDOG_TIMER="$tdir/felis-watchdog.timer" \
    WATCHDOG_FAILED_SERVICE="$tdir/felis-watchdog-failed.service" WATCHDOG_HEARTBEAT_FILE="$tdir/watchdog-heartbeat-url" \
    WATCHDOG_STATE="$tdir/watchdog/state.json" WATCHDOG_QUIET_FILE=/run/felis/watchdog-quiet-until \
    FELIS_DB_BACKUP_DIR=/var/lib/felis/db-backups FELIS_ARCHIVE_LOCAL_PATH=/var/lib/felis/archives FELIS_GAME_PORT=25577 \
    HOST_BIN=/usr/local/bin/felis STATE_DIR=/etc/felis bash -c '
    set -Eeuo pipefail
    ok() { printf "OK: %s\n" "$*"; }; warn() { printf "WARN: %s\n" "$*"; }
    systemctl() { printf "SYSTEMCTL: %s\n" "$*"; [ "$1" != start ] || return "$FIRST"; }
    log() { printf "LOG: %s\n" "$*"; }; die() { printf "DIE: %s\n" "$*"; exit 1; }
    journalctl() { printf "JOURNAL: parse /etc/felis/felis.host.toml\n"; }
    FELIS_WATCHDOG_HEARTBEAT_URL="${FELIS_WATCHDOG_HEARTBEAT_URL:-}"
    '"$hbblock"'
    '"$wblock"'
    eval "${SCRIPT:-install_watchdog_timer}"' 2>&1
}

out="$(run_watchdog_timer 0 "")"
unit="$(cat "$tdir/felis-watchdog.service")"
timer="$(cat "$tdir/felis-watchdog.timer")"
expect "the watchdog runs the host binary against the host config, dialing the proxy's port" \
  "ExecStart=/usr/local/bin/felis watchdog -config /etc/felis/felis.host.toml -state $tdir/watchdog/state.json -quiet-file /run/felis/watchdog-quiet-until -backup-dir /var/lib/felis/db-backups -proxy-addr 127.0.0.1:25577 -disk-paths /,/var/lib/rancher/k3s,/var/lib/felis,/var/lib/felis/archives,/var/lib/felis/db-backups" "$unit"
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

# A run that fails is reported by felis-watchdog-failed.service, and the heartbeat URL is
# kept private and shown by its host only. The units name no heartbeat flag, so a binary
# from before it still runs under them.
failed_unit="$(cat "$tdir/felis-watchdog-failed.service")"
expect "a failed run starts the failure report" "OnFailure=felis-watchdog-failed.service" "$unit"
expect "the failure report runs the host binary against the watchdog's state" \
  "ExecStart=/usr/local/bin/felis watchdog -unit-failed -config /etc/felis/felis.host.toml -state $tdir/watchdog/state.json -quiet-file /run/felis/watchdog-quiet-until
" "$failed_unit"
expect "the failure report is a oneshot" "Type=oneshot" "$failed_unit"
expect "a wedged failure report is killed" "TimeoutStartSec=2min" "$failed_unit"
case "$unit$failed_unit" in
  *-heartbeat*) echo "FAIL the watchdog units must name no heartbeat flag (an older binary refuses it)"; fails=$((fails + 1)) ;;
  *) echo "PASS the watchdog units name no heartbeat flag" ;;
esac
case "$out" in
  *"pings the heartbeat"*) echo "FAIL without a heartbeat URL the install must claim no heartbeat: $out"; fails=$((fails + 1)) ;;
  *) echo "PASS without a heartbeat URL the install claims none" ;;
esac
[ ! -e "$tdir/watchdog-heartbeat-url" ] && echo "PASS no heartbeat URL writes no file" \
  || { echo "FAIL no heartbeat URL must write no file"; fails=$((fails + 1)); }

out="$(FELIS_WATCHDOG_HEARTBEAT_URL=https://hc-ping.com/5f1e0c2a-key run_watchdog_timer 0 "")"
expect "the heartbeat URL is kept where felis watchdog reads it" "https://hc-ping.com/5f1e0c2a-key" "$(cat "$tdir/watchdog-heartbeat-url" 2>&1)"
if [ "$(stat -c %a "$tdir/watchdog-heartbeat-url" 2>/dev/null || stat -f %Lp "$tdir/watchdog-heartbeat-url")" = 600 ]; then
  echo "PASS the heartbeat URL file is private (its path pings the check)"
else
  echo "FAIL the heartbeat URL file must be 0600"; fails=$((fails + 1))
fi
expect "the install says the watchdog pings the heartbeat" \
  "OK: watchdog: checks every 2 minutes and mails the owners' verified addresses and pings the heartbeat at https://hc-ping.com/... (journalctl -u felis-watchdog)" "$out"
case "$out" in
  *5f1e0c2a*) echo "FAIL the install printed the heartbeat URL's key: $out"; fails=$((fails + 1)) ;;
  *) echo "PASS the install shows the heartbeat by its host only" ;;
esac
out="$(run_watchdog_timer 0 "")"
expect "a re-run without the variable keeps the heartbeat" "https://hc-ping.com/5f1e0c2a-key" "$(cat "$tdir/watchdog-heartbeat-url" 2>&1)"
expect "a re-run without the variable still reports the heartbeat" "and pings the heartbeat at https://hc-ping.com/..." "$out"
out="$(SCRIPT=summary_heartbeat run_watchdog_timer 0 "")"
expect "the summary names the heartbeat by its host" "LOG: Heartbeat: every watchdog run pings https://hc-ping.com/...;" "$out"
out="$(FELIS_WATCHDOG_HEARTBEAT_URL=off run_watchdog_timer 0 "")"
[ ! -e "$tdir/watchdog-heartbeat-url" ] && echo "PASS off removes the heartbeat" \
  || { echo "FAIL FELIS_WATCHDOG_HEARTBEAT_URL=off must remove the file"; fails=$((fails + 1)); }
leftover="$(find "$tdir" -maxdepth 1 -name 'watchdog-heartbeat-url.*')"
[ -z "$leftover" ] && echo "PASS writing the heartbeat URL leaves no temporary file" \
  || { echo "FAIL temporary files left: $leftover"; fails=$((fails + 1)); }
out="$(SCRIPT=summary_heartbeat run_watchdog_timer 0 "")"
expect "without a heartbeat the summary says so loudly" "WARN: NO HEARTBEAT: nothing off this machine notices it going down or its watchdog stopping." "$out"
expect "and says how to add one" "then re-run with FELIS_WATCHDOG_HEARTBEAT_URL=<its ping URL> (docs/troubleshooting.md §14)." "$out"
out="$(FELIS_WATCHDOG_HEARTBEAT_URL='https://user:pw@status.example:8443?rid=5f1e0c2a' SCRIPT='write_heartbeat_url; heartbeat_host' run_watchdog_timer 0 "")"
expect "the host shown drops the sign-in and the query" "https://status.example:8443/..." "$out"
rm -f "$tdir/watchdog-heartbeat-url"
for good in "" off https://hc-ping.com/5f1e0c2a http://10.0.0.5:8000/ping/5f1e0c2a; do
  out="$(FELIS_WATCHDOG_HEARTBEAT_URL="$good" SCRIPT='validate_heartbeat_url; echo fine' run_watchdog_timer 0 "")"
  expect "the heartbeat URL <$good> is accepted" "fine" "$out"
done
for bad in hc-ping.com/5f1e0c2a ftp://hc-ping.com/5f1e0c2a https:///5f1e0c2a 'https://hc-ping.com/5f1e 0c2a' 'https://hc-ping.com/5f1e"0c2a' "$(printf 'https://hc-ping.com/5f1e\n0c2a')"; do
  out="$(FELIS_WATCHDOG_HEARTBEAT_URL="$bad" SCRIPT='validate_heartbeat_url; echo fine' run_watchdog_timer 0 "")"
  case "$out" in
    *"DIE: FELIS_WATCHDOG_HEARTBEAT_URL must"*fine*|*5f1e*) echo "FAIL the heartbeat URL <$bad>: $out"; fails=$((fails + 1)) ;;
    *"DIE: FELIS_WATCHDOG_HEARTBEAT_URL must"*) echo "PASS the heartbeat URL <$bad> is refused without being echoed" ;;
    *) echo "FAIL the heartbeat URL <$bad> was accepted: $out"; fails=$((fails + 1)) ;;
  esac
done
case "$(awk '/^validate_settings\(\) \{/,/^}/' "$BS")" in
  *validate_heartbeat_url*) echo "PASS the heartbeat URL is checked before anything is installed" ;;
  *) echo "FAIL validate_settings must call validate_heartbeat_url"; fails=$((fails + 1)) ;;
esac
case "$(awk '/^summary\(\) \{/,/^}/' "$BS")" in
  *summary_offsite*summary_alerts*summary_heartbeat*summary_next*) echo "PASS the install's summary ends on the alerts, the heartbeat, then the next step" ;;
  *) echo "FAIL summary must call summary_alerts, summary_heartbeat, then summary_next"; fails=$((fails + 1)) ;;
esac

# --- the installer's last word: what the operator does next ------------------------------
# An install that leaves no Owner ends on `felis setup`, and starts it when there is a
# terminal for it; one with an Owner ends on where to sign in. The setup console must never
# start into a log (`| tee`, cloud-init, CI), and its own failure must not fail the install.
case "$(awk '/^main\(\) \{/,/^}/' "$BS" | tail -n 3)" in
  "  summary
  start_setup_console
}") echo "PASS the full install ends on the summary, then the setup console" ;;
  *) echo "FAIL main must end with summary, then start_setup_console"; fails=$((fails + 1)) ;;
esac
nextblock=""
for fn in bootstrap_from_tui resume_package_background_timers owner_state setup_terminal summary_next start_setup_console; do
  # A one-line function ends on its own line.
  b="$(awk -v fn="$fn" '$0 ~ "^" fn "\\(\\) \\{" { print; if (/\}$/) exit; on = 1; next } on { print } on && /^}/ { exit }' "$BS")"
  [ -n "$b" ] || { echo "FAIL: no ${fn} found in $BS"; exit 1; }
  [ "$(printf '%s\n' "$b" | wc -l)" -lt 40 ] \
    || { echo "FAIL: the extracted block is not ${fn} -- did its closing brace move?"; exit 1; }
  nextblock="${nextblock}${b}
"
done
next_dir="$(mktemp -d)"
printf 'keys from the terminal\n' > "$next_dir/tty"
cat > "$next_dir/felis" <<'XEOF'
#!/bin/sh
echo "felis $*" >> "$NEXT_JOURNAL"
if (: >&9) 2>/dev/null; then echo "fd 9 open" >> "$NEXT_JOURNAL"; fi
cat > "$NEXT_STDIN"
exit "${SETUP_EXIT:-0}"
XEOF
chmod +x "$next_dir/felis"
# run_next PG_ANSWER: summary_next then start_setup_console under the installer's own shell
# options and ERR trap. PG_ANSWER is what psql prints, or "fail". TERMINAL=1 stands in for
# a terminal on stdout, which a test's captured output never is.
run_next() {
  : > "$next_dir/journal"
  : > "$next_dir/stdin"
  PG_ANSWER="$1" NEXT_JOURNAL="$next_dir/journal" NEXT_STDIN="$next_dir/stdin" NEXT_LOCK="$next_dir/lock" \
    HOST_BIN="$next_dir/felis" SETUP_TTY="$next_dir/tty" bash -c '
    set -Eeuo pipefail
    trap "echo TRAP" ERR
    exec 9>"$NEXT_LOCK" # the run lock acquire_run_lock holds
    log() { printf "LOG: %s\n" "$*"; }; warn() { printf "WARN: %s\n" "$*"; }
    auth_hostname() { printf "%s" "$2"; }
    pg_exec() { echo "pg_exec $*" >> "$NEXT_JOURNAL"; [ "$PG_ANSWER" != fail ] || return 1; echo "$PG_ANSWER"; }
    systemctl() { echo "systemctl $*" >> "$NEXT_JOURNAL"; }
    DB_NAME=felis FELIS_ROOT_DOMAIN=example.net NODE_IP=10.0.0.5 FELIS_PANEL_NODEPORT=30443 FELIS_GAME_PORT=25565
    SETUP_CONSOLE=0
    PKG_TIMERS_TO_RESTORE=(apt-daily.timer)
    '"$nextblock"'
    if [ "${TERMINAL:-}" = 1 ]; then setup_terminal() { return 0; }; fi
    summary_next
    start_setup_console
    echo "exit 0"' 2>&1
}
started() { grep -q '^felis setup$' "$next_dir/journal"; }

out="$(TERMINAL=1 run_next t)"
expect "with an Owner the summary ends on where to sign in" "LOG: Felis is running. Sign in at https://op.console.example.net" "$out"
expect "  and where to change settings" "LOG: Email, edge and storage settings: sudo felis setup" "$out"
if started; then echo "FAIL with an Owner the setup console must not start: $out"; fails=$((fails + 1)); else echo "PASS   and the setup console does not start"; fi
admin_roles="$(grep -o "role IN ('admin', 'owner')" "$(dirname "$BS")/../internal/api/pgrepo.go" | head -n 1)"
expect "the Owner test is felis setup's own (AdminExists)" "${admin_roles:-AdminExists query not found}" "$(cat "$next_dir/journal")"

out="$(TERMINAL=1 run_next f)"
expect "with no Owner on a terminal the summary says the setup console starts" "LOG: Next: create the Owner account. The setup console starts now" "$out"
expect "  and how it binds the Owner" "has you join 10.0.0.5:25565 in Minecraft" "$out"
if started; then echo "PASS   and starts it"; else echo "FAIL the setup console did not start: $out"; fails=$((fails + 1)); fi
expect "  on the terminal's keys" "keys from the terminal" "$(cat "$next_dir/stdin")"
# felis setup can run the installer itself (its host bootstrap step), which would take an
# inherited run lock for a second installer run.
case "$(cat "$next_dir/journal")" in
  *"fd 9 open"*) echo "FAIL the setup console must not inherit the installer's run lock"; fails=$((fails + 1)) ;;
  *) echo "PASS   without the installer's run lock" ;;
esac
case "$(cat "$next_dir/journal")" in
  *"systemctl start apt-daily.timer"*"felis setup"*) echo "PASS   after the package timers are back" ;;
  *) echo "FAIL the package timers must restart before the setup console: $(cat "$next_dir/journal")"; fails=$((fails + 1)) ;;
esac
expect "  and the install ends cleanly" "exit 0" "$out"

out="$(run_next f)"
expect "with no Owner and no terminal the summary names the command" "LOG:     sudo felis setup" "$out"
if started; then echo "FAIL with no terminal the setup console must not start: $out"; fails=$((fails + 1)); else echo "PASS   and the setup console does not start into the log"; fi

out="$(TERMINAL=1 FELIS_NO_SETUP=1 run_next f)"
expect "FELIS_NO_SETUP names the command" "LOG:     sudo felis setup" "$out"
if started; then echo "FAIL FELIS_NO_SETUP must keep the setup console closed: $out"; fails=$((fails + 1)); else echo "PASS   and keeps the setup console closed"; fi

out="$(TERMINAL=1 run_next fail)"
expect "a database that does not answer names the command" "LOG:     sudo felis setup" "$out"
if started; then echo "FAIL an unknown Owner must not start the setup console: $out"; fails=$((fails + 1)); else echo "PASS   and the setup console does not start"; fi

out="$(TERMINAL=1 FELIS_BOOTSTRAP_FROM_TUI=1 run_next f)"
expect "under felis setup the console carries on" "LOG: Returning to the setup console to create the Owner account" "$out"
if started; then echo "FAIL under felis setup a second console must not start: $out"; fails=$((fails + 1)); else echo "PASS   and no second console starts"; fi

out="$(TERMINAL=1 SETUP_EXIT=3 run_next f)"
expect "a setup console that fails says how to come back" "WARN: the setup console exited with status 3; run 'sudo felis setup' to come back to it" "$out"
expect "  and the install still succeeds" "exit 0" "$out"
case "$out" in
  *TRAP*) echo "FAIL the setup console's failure must not reach the ERR trap: $out"; fails=$((fails + 1)) ;;
  *) echo "PASS   without the ERR trap" ;;
esac
rm -rf "$next_dir"

# The watchdog alerts by mail only, through the [smtp] relay. An install without one must
# say that its alerts are only logged; one with a relay must not cry wolf.
sablock="$(awk '/^summary_alerts\(\) \{/,/^}/' "$BS")"
[ -n "$sablock" ] || { echo "FAIL: no summary_alerts found in $BS"; exit 1; }
[ "$(printf '%s\n' "$sablock" | wc -l)" -lt 20 ] \
  || { echo "FAIL: the extracted block is not summary_alerts -- did its closing brace move?"; exit 1; }
run_summary_alerts() { # state-dir
  STATE_DIR="$1" SBLOCK_FILE="$sfn" bash -c '
    log() { printf "LOG: %s\n" "$*"; }; warn() { printf "WARN: %s\n" "$*"; }
    . "$SBLOCK_FILE"
    '"$sablock"'
    summary_alerts' 2>&1
}
out="$(run_summary_alerts "$smtp_dir")"
expect "with a relay the summary says the alerts are mailed" "LOG: Alerts: the watchdog mails the Owner's verified email address through the [smtp] relay." "$out"
case "$out" in
  *WARN*) echo "FAIL a configured relay must not be warned about: $out"; fails=$((fails + 1)) ;;
  *) echo "PASS a configured relay is not warned about" ;;
esac
alerts_dir="$(mktemp -d)"
printf '[smtp]\n  host = "relay.lan"\n  port = 25\n  from = "felis@example.net"\n' > "$alerts_dir/felis.host.toml"
out="$(run_summary_alerts "$alerts_dir")"
expect "a relay that signs in with no username still mails the alerts" "LOG: Alerts: the watchdog mails" "$out"
alerts_case=0
for toml in '' '[server]
listen = "0.0.0.0:8080"' '[smtp]
  host = ""
  port = 587' '[smtp]
  port = 587

[relay]
  host = "mail.example"'; do
  alerts_case=$((alerts_case + 1))
  rm -f "$alerts_dir/felis.host.toml"
  [ -z "$toml" ] || printf '%s\n' "$toml" > "$alerts_dir/felis.host.toml"
  out="$(run_summary_alerts "$alerts_dir")"
  expect "no relay (case $alerts_case): the summary says alerts are only logged" "WARN: NO ALERT MAIL: no email relay is configured, so the watchdog's alerts and the sign-in codes are only written to the journal." "$out"
  expect "no relay (case $alerts_case): the summary says how to add one" "WARN: Configure one in 'sudo felis setup' (e: configure email) and verify the Owner's email in the panel" "$out"
  case "$out" in
    *"LOG: Alerts"*) echo "FAIL no relay must not claim the alerts are mailed: $out"; fails=$((fails + 1)) ;;
  esac
done
rm -rf "$alerts_dir"

out="$(run_watchdog_timer 0 /srv/worlds)"
expect "a custom worlds root is watched for free space" "-disk-paths /,/var/lib/rancher/k3s,/var/lib/felis,/srv/worlds," "$(cat "$tdir/felis-watchdog.service")"
case "$unit" in
  *-node-ip*) echo "FAIL without a node address the watchdog must not check one"; fails=$((fails + 1)) ;;
  *) echo "PASS without a node address the watchdog checks none" ;;
esac
out="$(run_watchdog_timer 0 "" 10.211.55.6)"
expect "the watchdog checks the host still holds the install's address" \
  "-disk-paths /,/var/lib/rancher/k3s,/var/lib/felis,/var/lib/felis/archives,/var/lib/felis/db-backups -node-ip 10.211.55.6
" "$(cat "$tdir/felis-watchdog.service")"

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
for fn in write_file_atomic validate_offsite_settings persisted_offsite_block offsite_block offsite_enabled configure_offsite install_offsite_timer summary_offsite; do
  blk="$(awk "/^${fn}\\(\\) \\{/,/^}/" "$BS")"
  [ -n "$blk" ] || { echo "FAIL: no ${fn} found in $BS"; exit 1; }
  [ "$(printf '%s\n' "$blk" | wc -l)" -lt 120 ] \
    || { echo "FAIL: the extracted block is not ${fn} -- did its closing brace move?"; exit 1; }
  printf '%s\n' "$blk" >> "$ofile"
done

odir="$(mktemp -d)"
run_offsite() { # script; runs with the off-site functions sourced
  STATE_DIR="$odir" OFFSITE_ENV="$odir/offsite.env" OFFSITE_SERVICE="$odir/felis-offsite.service" \
    OFFSITE_TIMER="$odir/felis-offsite.timer" FNFILE="$ofile" HOST_BIN=fakefelis \
    OFFSITE_KEY_TTY="${OFFSITE_KEY_TTY:-$odir/no-terminal/tty}" \
    FELIS_DB_BACKUP_DIR=/var/lib/felis/db-backups FELIS_BACKUP_PVC=felis-backups bash -c '
    set -Eeuo pipefail
    die() { printf "DIE: %s\n" "$*"; exit 1; }
    log() { printf "LOG: %s\n" "$*"; }; ok() { printf "OK: %s\n" "$*"; }; warn() { printf "WARN: %s\n" "$*"; }
    remember_temp() { :; }
    systemctl() { printf "SYSTEMCTL: %s\n" "$*" >&2; }
    fakefelis() { printf "RUN: %s\n" "$*" >&2; [ -z "${CHECK_FAILS:-}" ] || { echo "bucket: access denied" >&2; return 1; }
      [ -z "${KEY_MISMATCH:-}" ] || { echo "the bucket records key id 1111111111111111, this key is 2222222222222222" >&2; return 3; }
      [ "$2" != take-over ] || [ -z "${STANDBY:-}" ] || { echo "felis offsite take-over: host prod-1 (id 3333333333333333) writes the bucket, last at 2026-09-27 07:00:00 (20m ago)"
        echo "This host was built from its backup and copies nothing into the bucket."; return 4; }
      [ "$2" != take-over ] || [ -z "${DISPLACED:-}" ] || { echo "felis offsite take-over: host spare-1 (id 4444444444444444) writes the bucket, last at 2026-09-27 07:10:00 (10m ago)"
        echo "It took the bucket over from this host: this host copies nothing there any more, and its watchdog mails the owners about it."; return 5; }
      [ "$2" != take-over ] || [ -z "${WRITER_FAILS:-}" ] || { echo "felis offsite take-over: offsite: felis-writer in the bucket is not a record Felis wrote" >&2; return 1; }; }
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

# A re-run found a configured bucket "missing" and removed the off-site timer: grep -q
# stopped at the bucket line while offsite_block was still writing, and pipefail turned
# the SIGPIPE into "no bucket". The section far past one pipe buffer with the bucket near
# its top makes the lost race certain.
{ printf '[offsite]\nbucket = "kept"\nendpoint = "https://s3.example"\n'
  seq 1 200000 | sed 's/.*/prefix = "p&"/'; } > "$odir/felis.host.toml"
out="$(run_offsite 'if offsite_enabled; then echo ENABLED; else echo "OFF (exit $?)"; fi')"
expect "a configured bucket is found in a long [offsite] section" "ENABLED" "$out"

# First configured install: the key is generated, the file is private, the key is shown once,
# on the terminal alone. stdout and stderr are what a tee'd install log keeps; warn writes
# to stderr here as in bootstrap.sh.
rm -f "$odir/felis.host.toml"
out="$(FELIS_OFFSITE_ENDPOINT=https://s3.example FELIS_OFFSITE_BUCKET=felis OFFSITE_KEY_TTY="$odir/tty" \
  FELIS_OFFSITE_ACCESS_KEY=AK FELIS_OFFSITE_SECRET_KEY=SK \
  run_offsite 'warn() { printf "WARN: %s\n" "$*" >&2; }; configure_offsite; summary_offsite')"
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
tty_out="$(cat "$odir/tty" 2>/dev/null)"
expect "a new key is shown on the terminal" "WARN:     FELIS_OFFSITE_KEY=${key}" "$tty_out"
expect "the terminal gets the warning to keep it elsewhere" "WARN: The off-site copies are encrypted with this key. Store it NOW somewhere other than
WARN: this machine (a password manager): without it nothing in the bucket can be read." "$tty_out"
case "$out" in
  *"$key"*) echo "FAIL the new key reached the install's output, which a log keeps"; fails=$((fails + 1)) ;;
  *) echo "PASS the new key stays out of the install's output" ;;
esac
expect "the output says where the key shown on the terminal is" "WARN: Off-site copy: the new encryption key was shown on the terminal and is in $odir/offsite.env" "$out"

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
# A re-run that cannot finish writing offsite.env (a full disk) keeps the key that sealed
# the bucket: a file cut short before that line would have a new key minted next run.
out="$(FELIS_OFFSITE_ENDPOINT=https://s3.example FELIS_OFFSITE_BUCKET=felis \
  FELIS_OFFSITE_ACCESS_KEY=AK3 run_offsite 'cat() { head -c 30; return 1; }; configure_offsite')"
expect "a write of offsite.env that fails halfway dies" "DIE: could not write $odir/offsite.env; it is left as it was" "$out"
expect "a failed write keeps the key that sealed the bucket" "FELIS_OFFSITE_KEY='${key}'" "$(cat "$odir/offsite.env")"
expect "a failed write keeps the credentials that worked" "FELIS_OFFSITE_ACCESS_KEY='AK2'" "$(cat "$odir/offsite.env")"
rm -f "$odir"/offsite.env.*

rm -f "$odir/offsite.env"
out="$(FELIS_OFFSITE_ENDPOINT=https://s3.example FELIS_OFFSITE_BUCKET=felis run_offsite configure_offsite)"
expect "a bucket without credentials is refused" "DIE: [offsite] names a bucket but there are no credentials" "$out"

# No terminal (CI, cloud-init, ssh without -t): the key is shown nowhere, and the output
# names the command that reads it.
out="$(FELIS_OFFSITE_ENDPOINT=https://s3.example FELIS_OFFSITE_BUCKET=felis \
  FELIS_OFFSITE_ACCESS_KEY=AK FELIS_OFFSITE_SECRET_KEY=SK run_offsite 'configure_offsite; summary_offsite')"
key2="$(sed -n "s/^FELIS_OFFSITE_KEY='\(.*\)'$/\1/p" "$odir/offsite.env")"
if [ -n "$key2" ]; then
  case "$out" in
    *"$key2"*) echo "FAIL with no terminal the new key reached the install's output"; fails=$((fails + 1)) ;;
    *) echo "PASS with no terminal the new key stays out of the install's output" ;;
  esac
  case "$out" in
    *no-terminal*) echo "FAIL the terminal that could not be opened left an error in the output: $out"; fails=$((fails + 1)) ;;
    *) echo "PASS the terminal that could not be opened leaves no error in the output" ;;
  esac
else
  echo "FAIL the install without a terminal generated no key"; fails=$((fails + 1))
fi
expect "with no terminal the output says the key stays out of it" "WARN: With no terminal to show it on, it stays out of this output; read it with" "$out"
expect "with no terminal the output names the command that reads the key" "WARN:     sudo grep '^FELIS_OFFSITE_KEY=' $odir/offsite.env" "$out"
rm -f "$odir/offsite.env"

out="$(run_offsite 'configure_offsite; install_offsite_timer; summary_offsite; echo "enabled=$OFFSITE_ENABLED"')"
expect "no bucket leaves the off-site copy off" "enabled=0" "$out"
expect "no bucket is a loud warning" "WARN: NO OFF-SITE COPY" "$out"

out="$(OFFSITE_ENABLED=1 run_offsite install_offsite_timer)"
unit="$(cat "$odir/felis-offsite.service")"
timer="$(cat "$odir/felis-offsite.timer")"
expect "the unit loads the secrets" "EnvironmentFile=$odir/offsite.env" "$unit"
expect "the unit syncs from the host config" \
  "ExecStart=fakefelis offsite sync -config $odir/felis.host.toml -env-file $odir/offsite.env -db-dir /var/lib/felis/db-backups -backup-pvc \"felis-backups\"" "$unit"
expect "a pass over a big archive is not cut off at the hour" "TimeoutStartSec=24h" "$unit"
expect "the copy runs hourly" "OnCalendar=hourly" "$timer"
expect "a missed run catches up at boot" "Persistent=true" "$timer"
expect "the timer is enabled" "SYSTEMCTL: enable --now felis-offsite.timer" "$out"
expect "the bucket and its key are checked during the install" "RUN: offsite check-key -config $odir/felis.host.toml -env-file $odir/offsite.env" "$out"
expect "the first copy runs in the background" "SYSTEMCTL: start --no-block felis-offsite.service" "$out"

out="$(OFFSITE_ENABLED=1 CHECK_FAILS=1 run_offsite install_offsite_timer)"
expect "an unreachable bucket shows why" "bucket: access denied" "$out"
expect "an unreachable bucket is a loud warning" "WARN: the [offsite] bucket did not answer" "$out"
case "$out" in
  *"start --no-block"*) echo "FAIL a failed check still started the copy"; fails=$((fails + 1)) ;;
  *) echo "PASS a failed check does not start the copy" ;;
esac

# A reinstall that lost offsite.env generates a fresh key; a bucket sealed with the old one
# must stop the install's copy and say what to do, not read as an unreachable bucket.
out="$(OFFSITE_ENABLED=1 OFFSITE_KEY_NEW=1 FELIS_OFFSITE_KEY=NEWKEY KEY_MISMATCH=1 run_offsite 'install_offsite_timer; summary_offsite')"
expect "a key mismatch shows both ids" "the bucket records key id 1111111111111111, this key is 2222222222222222" "$out"
expect "a key mismatch is a loud warning" "WARN: The [offsite] bucket's objects are sealed with another key than the one in" "$out"
expect "a key mismatch says the key was generated by this run" "WARN: This run generated that key" "$out"
expect "a key mismatch says how to fix it" "WARN: Set FELIS_OFFSITE_KEY in $odir/offsite.env to the key the bucket was written with" "$out"
expect "the summary says the copy stopped" "WARN: OFF-SITE COPY STOPPED" "$out"
expect "the timer stays for the run after the fix" "SYSTEMCTL: enable --now felis-offsite.timer" "$out"
case "$out" in
  *"start --no-block"* | *"did not answer"* | *"Store it NOW"* | *NEWKEY*)
    echo "FAIL a key mismatch started the copy, read as an unreachable bucket or showed the refused key: $out"; fails=$((fails + 1)) ;;
  *) echo "PASS a key mismatch neither starts the copy nor asks to store the refused key" ;;
esac
out="$(OFFSITE_ENABLED=1 KEY_MISMATCH=1 run_offsite install_offsite_timer)"
case "$out" in
  *"This run generated"*) echo "FAIL a kept key was called generated"; fails=$((fails + 1)) ;;
  *) echo "PASS a kept key that mismatches is not called generated" ;;
esac
rm -f "$odir/tty"
out="$(OFFSITE_ENABLED=1 OFFSITE_KEY_NEW=1 FELIS_OFFSITE_KEY=NEWKEY OFFSITE_KEY_TTY="$odir/tty" run_offsite 'install_offsite_timer; summary_offsite')"
expect "a key the bucket takes is shown on the terminal" "WARN:     FELIS_OFFSITE_KEY=NEWKEY" "$(cat "$odir/tty" 2>/dev/null)"

# A rehearsal on a spare machine restores the production host's [offsite] settings: the
# install must find the production host writing the bucket, say this host stands by, and
# still start the first copy so the watchdog learns it.
out="$(OFFSITE_ENABLED=1 run_offsite install_offsite_timer)"
expect "the install asks which host writes the bucket" "RUN: offsite take-over -config $odir/felis.host.toml -env-file $odir/offsite.env" "$out"
case "$out" in
  *"take-over -yes"* | *"-config $odir/felis.host.toml -env-file $odir/offsite.env -yes"*) echo "FAIL the install took the bucket over: $out"; fails=$((fails + 1)) ;;
  *) echo "PASS the install only asks, never takes the bucket over" ;;
esac
out="$(OFFSITE_ENABLED=1 STANDBY=1 run_offsite 'install_offsite_timer; summary_offsite')"
expect "a standby host names the writer" "WARN:   host prod-1 (id 3333333333333333) writes the bucket, last at 2026-09-27 07:00:00 (20m ago)" "$out"
expect "a standby host is a loud warning" "WARN: Another host writes the [offsite] bucket, and this host was built from its backup:" "$out"
expect "a standby host says how to take over" "WARN: this host replaces it for good, run sudo felis offsite take-over -yes, then" "$out"
expect "a standby host still records itself through the first copy" "SYSTEMCTL: start --no-block felis-offsite.service" "$out"
expect "the summary says the copy stands by" "WARN: OFF-SITE COPY ON STANDBY: another host writes the [offsite] bucket (see above)." "$out"
expect "a standby host still says where its key is" "LOG: Off-site copy: the encryption key is in" "$out"
case "$out" in
  *"OK: off-site copy: hourly"* | *"could not tell"* | *"OFF-SITE COPY STOPPED"*) echo "FAIL a standby host read as copying, as an error or as a key mismatch: $out"; fails=$((fails + 1)) ;;
  *) echo "PASS a standby host reads as standing by only" ;;
esac
# A rehearsal machine that took the bucket over by mistake leaves the production host
# displaced: its re-run must say so, with how to take the bucket back, and never call it a
# standby, whose watchdog stays quiet.
out="$(OFFSITE_ENABLED=1 DISPLACED=1 run_offsite 'install_offsite_timer; summary_offsite')"
expect "a displaced host names the host that took over" "WARN:   host spare-1 (id 4444444444444444) writes the bucket, last at 2026-09-27 07:10:00 (10m ago)" "$out"
expect "a displaced host is a loud warning" "WARN: Another host took the [offsite] bucket over from this host:" "$out"
expect "a displaced host says how to take the bucket back" "WARN: sudo felis offsite take-over -yes, then sudo systemctl start felis-offsite.service" "$out"
expect "the summary says the copy stopped" "WARN: OFF-SITE COPY STOPPED: another host took the [offsite] bucket over (see above)." "$out"
expect "a displaced host still says where its key is" "LOG: Off-site copy: the encryption key is in" "$out"
case "$out" in
  *"OK: off-site copy: hourly"* | *"could not tell"* | *"ON STANDBY"* | *"built from its backup:"* | *"sealed with another key"*) echo "FAIL a displaced host read as copying, as an error, as a standby or as a key mismatch: $out"; fails=$((fails + 1)) ;;
  *) echo "PASS a displaced host reads as taken over only" ;;
esac
out="$(OFFSITE_ENABLED=1 WRITER_FAILS=1 run_offsite 'install_offsite_timer; summary_offsite')"
expect "an unreadable writer record shows why" "felis-writer in the bucket is not a record Felis wrote" "$out"
expect "an unreadable writer record is a warning" "WARN: could not tell which host writes the [offsite] bucket (error above)" "$out"
case "$out" in
  *"OK: off-site copy: hourly"* | *"ON STANDBY"*) echo "FAIL an unreadable writer record read as copying or standing by: $out"; fails=$((fails + 1)) ;;
  *) echo "PASS an unreadable writer record reads as neither copying nor standing by" ;;
esac
out="$(OFFSITE_ENABLED=1 run_offsite 'install_offsite_timer; summary_offsite')"
expect "the host that writes the bucket copies" "OK: off-site copy: hourly to the [offsite] bucket, first copy started" "$out"
case "$out" in
  *"ON STANDBY"* | *"Another host writes"*) echo "FAIL the writer was called a standby: $out"; fails=$((fails + 1)) ;;
  *) echo "PASS the writer is not called a standby" ;;
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

# --- the control-plane image is tagged by release, so rollout undo is a rollback --------
# Under one mutable tag `kubectl rollout undo` re-created the pods on the image the upgrade had
# just written over it. The tag must follow the version, and an upgrade must not restart the
# Deployments on top of the apply's own roll (that second revision is what undo would reach).

imgblock="$(awk '/^resolve_felis_image\(\) \{/,/^}/' "$BS"; awk '/^image_tag_for_version\(\) \{/,/^}/' "$BS")"
[ -n "$imgblock" ] || { echo "FAIL: no resolve_felis_image found in $BS"; exit 1; }
[ "$(printf '%s\n' "$imgblock" | wc -l)" -lt 30 ] \
  || { echo "FAIL: the extracted block is not resolve_felis_image -- did it move?"; exit 1; }

run_image() { # FELIS_IMAGE FELIS_VERSION HAVE_PREBUILT_BINARY binary-version
  FELIS_IMAGE="$1" FELIS_VERSION="$2" HAVE_PREBUILT_BINARY="$3" BIN_VERSION="$4" \
  REGISTRY_URL=registry.felis.svc:5000 HOST_BIN=fakeFelis bash -c '
    ok() { :; }
    fakeFelis() { printf "felis %s\n" "$BIN_VERSION"; }
    '"$imgblock"'
    resolve_felis_image
    printf "%s\n" "$FELIS_IMAGE"'
}

expect "a release install is tagged with its release" "registry.felis.svc:5000/felis/felis:v1.2.3" \
  "$(run_image '' v1.2.3 '' '')"
expect "a source build's stamp becomes a legal tag" "registry.felis.svc:5000/felis/felis:v1.2.3-gabc1234" \
  "$(run_image '' 'v1.2.3+gabc1234' '' '')"
expect "the setup console path asks the binary it installed" "registry.felis.svc:5000/felis/felis:v1.4.0" \
  "$(run_image '' '' 1 v1.4.0)"
expect "an unstamped binary keeps the old tag" "registry.felis.svc:5000/felis/felis:demo" \
  "$(run_image '' '' 1 dev)"
expect "an unknown version keeps the old tag" "registry.felis.svc:5000/felis/felis:demo" \
  "$(run_image '' '' '' '')"
expect "an explicit FELIS_IMAGE is used as given" "reg.example/felis:mine" \
  "$(run_image reg.example/felis:mine v1.2.3 '' '')"

rsblock="$(awk '/^restart_existing_control_plane\(\) \{/,/^}/' "$BS")"
[ -n "$rsblock" ] || { echo "FAIL: no restart_existing_control_plane found in $BS"; exit 1; }
run_restart() { # prev-api prev-operator [prev-gate]
  FELIS_IMAGE=reg/felis/felis:v2 CONTROL_NS=felis bash -c '
    set -Eeuo pipefail
    log() { :; }
    kube() { printf "KUBE %s\n" "$*"; }
    '"$rsblock"'
    restart_existing_control_plane "$1" "$2" "$3"
    echo DONE' _ "$1" "$2" "${3:-}"
}

out="$(run_restart reg/felis/felis:v1 reg/felis/felis:v1 reg/felis/felis:v1)"
case "$out" in
  *"rollout restart"*) echo "FAIL an upgrade restarted the control plane on top of the apply's roll"; fails=$((fails + 1)) ;;
  *DONE*) echo "PASS an upgrade leaves the roll to the apply" ;;
  *) echo "FAIL restart_existing_control_plane died on an upgrade: $out"; fails=$((fails + 1)) ;;
esac
out="$(run_restart reg/felis/felis:v2 reg/felis/felis:v2 reg/felis/felis:v2)"
expect "a rerun of the same tag restarts felis-api onto the rebuilt image" "KUBE -n felis rollout restart deployment/felis-api" "$out"
expect "a rerun of the same tag restarts felis-operator too" "KUBE -n felis rollout restart deployment/felis-operator" "$out"
expect "a rerun of the same tag restarts the registry's gate too" "KUBE -n felis rollout restart deployment/registry" "$out"
out="$(run_restart '' '')"
case "$out" in
  *"rollout restart"*) echo "FAIL a first install restarted Deployments that did not exist"; fails=$((fails + 1)) ;;
  *DONE*) echo "PASS a first install restarts nothing" ;;
  *) echo "FAIL restart_existing_control_plane died on a first install: $out"; fails=$((fails + 1)) ;;
esac

# The binary names the release, the release names the images, and each image is looked for in
# the release's bundles before anything is pulled or built.
imorder="$(awk '/^main\(\) \{/,/^}/' "$BS" | grep -nE '^[[:space:]]*(install_k3s|configure_registry_mirror|acquire_felis_binary|select_release_artifacts|resolve_felis_image|import_release_images felis registry postgres|import_platform_images|build_image|pin_platform_images)$' | sed 's/^[0-9]*:[[:space:]]*//' | tr '\n' ' ')"
expect "main takes the binary, then the release's images, before pulling or building any" \
  "install_k3s configure_registry_mirror acquire_felis_binary select_release_artifacts resolve_felis_image import_release_images felis registry postgres import_platform_images build_image pin_platform_images " "$imorder"
if awk '/^main\(\) \{/,/^}/' "$BS" | grep -qE '^[[:space:]]*install_docker$'; then
  echo "FAIL: main installs Docker up front; only a build may (ensure_docker)"; fails=$((fails + 1))
else
  echo "PASS main leaves Docker to the builds that need it"
fi

# --- a rerun restarts only what changed -------------------------------------------------
# The proxy, the login and lobby pods and PostgreSQL each disconnect every player (or cut
# felis-api's transactions) when restarted, so a rerun that changed none of them must leave
# them running, and one that changed a thing must still restart it.

vsblock="$(awk '/^install_velocity_service\(\) \{/,/^}/' "$BS"; awk '/^velocity_fingerprint\(\) \{/,/^}/' "$BS"; awk '/^heap_megabytes\(\) \{/,/^}/' "$BS")"
[ -n "$vsblock" ] || { echo "FAIL: no install_velocity_service found in $BS"; exit 1; }
vdir="$(mktemp -d)"
mkdir -p "$vdir/v/plugins/felis-link" "$vdir/jre"
printf 'jar\n' > "$vdir/v/velocity.jar"
printf 'plugin\n' > "$vdir/v/plugins/felis-velocity.jar"
printf 'JAVA_VERSION="25"\n' > "$vdir/jre/release"
run_velocity_service() { # is-active(0|1) [heap]
  ACTIVE="$1" FELIS_VELOCITY_XMX="${2:-1G}" VELOCITY_SERVICE="$vdir/unit" VELOCITY_DIR="$vdir/v" JRE_DIR="$vdir/jre" \
  VELOCITY_FINGERPRINT="$vdir/fp" VELOCITY_USER=felis-velocity FELIS_GAME_PORT=25565 \
  FELIS_LEGACY_FORWARDING_SERVERS='' bash -c '
    set -Eeuo pipefail
    ok() { printf "OK: %s\n" "$*"; }
    felis_internal_ip() { printf "10.43.0.9"; }
    systemctl() {
      case "$1" in
        is-active) [ "$ACTIVE" = 1 ] ;;
        *) printf "SYSTEMCTL %s\n" "$*" ;;
      esac
    }
    '"$vsblock"'
    install_velocity_service'
}
out="$(run_velocity_service 1)"
expect "a proxy with no recorded start is restarted" "SYSTEMCTL restart felis-velocity" "$out"
expect "the default 1G ceiling runs the serial collector and C1 from a 16M start" \
  "java -Xms16M -Xmx1G -XX:+UseSerialGC -XX:TieredStopAtLevel=1 -Dmojang" "$(cat "$vdir/unit")"
case "$(cat "$vdir/unit")" in
  *AlwaysPreTouch*|*UseG1GC*) echo "FAIL a 1G proxy pre-touches its heap or runs G1: $(grep ExecStart "$vdir/unit")"; fails=$((fails + 1)) ;;
  *) echo "PASS a 1G proxy neither pre-touches its heap nor runs G1" ;;
esac
[ -s "$vdir/fp" ] && echo "PASS the restart records what the proxy runs" \
  || { echo "FAIL no fingerprint was recorded after the restart"; fails=$((fails + 1)); }
out="$(run_velocity_service 1)"
case "$out" in
  *"SYSTEMCTL restart"*) echo "FAIL an unchanged rerun restarted the proxy"; fails=$((fails + 1)) ;;
  *"felis-velocity unchanged; left running"*) echo "PASS an unchanged rerun leaves the proxy running" ;;
  *) echo "FAIL install_velocity_service died on an unchanged rerun: $out"; fails=$((fails + 1)) ;;
esac
printf 'plugin v2\n' > "$vdir/v/plugins/felis-velocity.jar"
expect "a changed plugin jar restarts the proxy" "SYSTEMCTL restart felis-velocity" "$(run_velocity_service 1)"
expect "a stopped proxy is started whatever the fingerprint" "SYSTEMCTL restart felis-velocity" "$(run_velocity_service 0)"
printf 'JAVA_VERSION="25.0.1"\n' > "$vdir/jre/release"
expect "a patched JRE restarts the proxy" "SYSTEMCTL restart felis-velocity" "$(run_velocity_service 1)"
expect "a new heap size restarts the proxy" "SYSTEMCTL restart felis-velocity" "$(run_velocity_service 1 3G)"
expect "a ceiling above 1G runs G1, whose periodic collection hands idle growth back" \
  "java -Xms64M -Xmx3G -XX:+UseG1GC -XX:+ParallelRefProcEnabled -XX:G1PeriodicGCInterval=60000 -Dmojang" "$(cat "$vdir/unit")"
case "$(cat "$vdir/unit")" in
  *AlwaysPreTouch*|*UseSerialGC*|*TieredStopAtLevel*) echo "FAIL a 3G proxy carries a small proxy's flags or pre-touches: $(grep ExecStart "$vdir/unit")"; fails=$((fails + 1)) ;;
  *) echo "PASS a 3G proxy keeps G1 and both compilers" ;;
esac
run_velocity_service 1 1024M >/dev/null
expect "1024M is still a small proxy" "java -Xms16M -Xmx1024M -XX:+UseSerialGC " "$(cat "$vdir/unit")"
run_velocity_service 1 1025M >/dev/null
expect "a megabyte past 1G is a large one" "java -Xms64M -Xmx1025M -XX:+UseG1GC " "$(cat "$vdir/unit")"
run_velocity_service 1 384M >/dev/null
expect "a small ceiling runs the small proxy's flags" \
  "java -Xms16M -Xmx384M -XX:+UseSerialGC -XX:TieredStopAtLevel=1 -Dmojang" "$(cat "$vdir/unit")"
# `felis rotate-token velocity` rewrites service-token, which the plugin re-reads by itself:
# that line alone changing leaves the proxy running, and any other change restarts it.
props="$vdir/v/plugins/felis-link/felis-link.properties"
printf 'api-base-url=http://a\nservice-token=one\n' > "$props"
expect "new proxy properties restart the proxy" "SYSTEMCTL restart felis-velocity" "$(run_velocity_service 1 384M)"
printf 'api-base-url=http://a\nservice-token=two\n' > "$props"
out="$(run_velocity_service 1 384M)"
case "$out" in
  *"SYSTEMCTL restart"*) echo "FAIL a new service-token alone restarted the proxy"; fails=$((fails + 1)) ;;
  *"felis-velocity unchanged; left running"*) echo "PASS a new service-token alone leaves the proxy running" ;;
  *) echo "FAIL install_velocity_service died on a new service-token: $out"; fails=$((fails + 1)) ;;
esac
printf 'api-base-url=http://b\nservice-token=two\n' > "$props"
expect "another change to the proxy properties restarts the proxy" "SYSTEMCTL restart felis-velocity" "$(run_velocity_service 1 384M)"
rm -rf "$vdir"

ssblock="$(awk '/^restart_existing_system_servers\(\) \{/,/^}/' "$BS")"
[ -n "$ssblock" ] || { echo "FAIL: no restart_existing_system_servers found in $BS"; exit 1; }
sdir2="$(mktemp -d)"
run_system_restart() { # limbo-id lobby-id pods(0|1) pin-exit
  LIMBO_IMAGE_ID="$1" LOBBY_IMAGE_ID="$2" PODS="$3" PIN_EXIT="${4:-0}" SYSTEM_SERVER_IMAGES="$sdir2/state" \
  LOGIN_SERVER=login LOBBY_SERVER=lobby MINECRAFT_NS=minecraft HOST_BIN=felis \
  REGISTRY_URL=registry.felis.svc:5000 REGISTRY_PUSH_HOST=127.0.0.1:5000 bash -c '
    set -Eeuo pipefail
    ok() { printf "OK: %s\n" "$*"; }
    warn() { printf "WARN: %s\n" "$*"; }
    log() { :; }
    felis() { printf "FELIS %s\n" "$*"; return "$PIN_EXIT"; }
    kube() {
      case "$*" in
        *"get pod"*) [ "$PODS" = 1 ] && printf "pod/x-0\n" || true ;;
        *delete*) printf "KUBE %s\n" "$*" ;;
      esac
    }
    '"$ssblock"'
    restart_existing_system_servers'
}
out="$(run_system_restart sha256:aaa sha256:bbb 1)"
expect "an unrecorded login build is pinned through the loopback registry" "FELIS pin-images --system login --namespace minecraft --registry registry.felis.svc:5000 --endpoint 127.0.0.1:5000" "$out"
expect "an unrecorded lobby build is pinned" "FELIS pin-images --system lobby " "$out"
case "$out" in
  *delete*) echo "FAIL a successful pin also deleted a pod; the operator rolls it"; fails=$((fails + 1)) ;;
  *) echo "PASS a successful pin leaves the roll to the operator" ;;
esac
out="$(run_system_restart sha256:aaa sha256:bbb 1)"
case "$out" in
  *FELIS*|*delete*) echo "FAIL an unchanged rebuild moved a system server: $out"; fails=$((fails + 1)) ;;
  *"already runs this build"*) echo "PASS an unchanged rebuild leaves the system servers running" ;;
  *) echo "FAIL restart_existing_system_servers died on an unchanged rebuild: $out"; fails=$((fails + 1)) ;;
esac
out="$(run_system_restart sha256:aaa sha256:ccc 1 1)"
expect "a failed pin warns and names the way out" "WARN: could not pin the lobby system server" "$out"
expect "a failed pin falls back to restarting the pod" "KUBE -n minecraft delete pod -l felis.lolicon.best/server=lobby" "$out"
case "$out" in
  *"server=login"*|*"--system login"*) echo "FAIL a new lobby build moved the login gate too"; fails=$((fails + 1)) ;;
  *) echo "PASS a new lobby build leaves the login gate alone" ;;
esac
rm -f "$sdir2/state"
out="$(run_system_restart sha256:aaa sha256:ddd 0 1)"
case "$out" in
  *delete*) echo "FAIL a missing pod was deleted"; fails=$((fails + 1)) ;;
  *) echo "PASS no pod, nothing to restart" ;;
esac
expect "the builds are recorded even before the pods exist" "lobby sha256:ddd" "$(cat "$sdir2/state")"
rm -rf "$sdir2"

rfblock="$(awk '/^pkg_refresh_once\(\) \{/,/^}/' "$BS")"
pmdir="$(mktemp -d)"
run_refresh() {
  PKG=pacman DATA="$pmdir" bash -c '
    wait_for_pkg_locks() { :; }
    postgres_data_dir() { printf "%s\n" "$DATA"; }
    pacman() { printf "PACMAN %s\n" "$*"; }
    '"$rfblock"'
    pkg_refresh_once'
}
case "$(run_refresh)" in
  *--ignore*) echo "FAIL a host with no cluster yet held PostgreSQL back"; fails=$((fails + 1)) ;;
  *"PACMAN -Syu"*) echo "PASS a fresh Arch host upgrades normally" ;;
  *) echo "FAIL pkg_refresh_once did not refresh pacman"; fails=$((fails + 1)) ;;
esac
printf '16\n' > "$pmdir/PG_VERSION"
expect "an Arch rerun holds PostgreSQL at the cluster's version" "PACMAN -Syu --noconfirm --ignore postgresql" "$(run_refresh)"
rm -rf "$pmdir"
expect "an apt refresh first finishes a dpkg run that was cut off" "FINISH
APT_GET update -y" "$(PKG=apt bash -c '
  finish_interrupted_dpkg() { echo FINISH; }
  apt_get() { echo "APT_GET $*"; }
  '"$rfblock"'
  pkg_refresh_once')"

# An install killed halfway through a package leaves dpkg's journal behind, and apt-get then
# refuses everything until `dpkg --configure -a` runs: the rerun runs it.
fdblock="$(awk '/^finish_interrupted_dpkg\(\) \{/,/^}/' "$BS")"
[ -n "$fdblock" ] || { echo "FAIL: no finish_interrupted_dpkg found in $BS"; exit 1; }
[ "$(printf '%s\n' "$fdblock" | wc -l)" -lt 15 ] \
  || { echo "FAIL: the extracted block is not finish_interrupted_dpkg -- did its closing brace move?"; exit 1; }
dpdir="$(mktemp -d)"
run_finish_dpkg() { # $1: what dpkg --audit prints, $2: dpkg --configure's exit status
  AUDIT="$1" DPKG_RC="${2:-0}" DPKG_UPDATES_DIR="$dpdir/updates" bash -c '
    set -Eeuo pipefail
    warn() { printf "WARN: %s\n" "$*"; }; die() { printf "DIE: %s\n" "$*"; exit 1; }
    wait_for_pkg_locks() { echo LOCKS; }
    dpkg() {
      if [ "$1" = --audit ]; then printf "%s" "$AUDIT"; return 0; fi
      printf "DPKG: %s (frontend=%s)\n" "$*" "${DEBIAN_FRONTEND:-}"
      return "$DPKG_RC"
    }
    '"$fdblock"'
    finish_interrupted_dpkg
    echo DONE' 2>&1
}
out="$(run_finish_dpkg "")"
expect "a host without dpkg's journal carries on" "DONE" "$out"
case "$out" in *DPKG:*) echo "FAIL a host without dpkg's journal must not run dpkg --configure: $out"; fails=$((fails + 1)) ;; *) echo "PASS   without running dpkg --configure" ;; esac
mkdir -p "$dpdir/updates"
case "$(run_finish_dpkg "")" in *DPKG:*) echo "FAIL an empty dpkg journal must not run dpkg --configure"; fails=$((fails + 1)) ;; *) echo "PASS an empty dpkg journal runs nothing" ;; esac
: > "$dpdir/updates/0007"
out="$(run_finish_dpkg "")"
expect "a dpkg run cut off mid-package is finished first" "LOCKS
DPKG: --force-confdef --force-confold --configure -a (frontend=noninteractive)" "$out"
expect "  and the install carries on" "DONE" "$out"
out="$(run_finish_dpkg "" 1)"
expect "a dpkg run that cannot be finished stops the install" "DIE: dpkg --configure -a failed" "$out"
case "$out" in *DONE*) echo "FAIL a dpkg run that cannot be finished must stop the install"; fails=$((fails + 1)) ;; esac
rm -f "$dpdir/updates/0007"
expect "a package dpkg --audit names as half configured is finished too" "DPKG: --force-confdef --force-confold --configure -a" \
  "$(run_finish_dpkg "The following packages are only half configured")"
rm -rf "$dpdir"

# A second installer started while one still runs (in tmux after the SSH session dropped)
# stops at once.
alblock="$(awk '/^acquire_run_lock\(\) \{/,/^}/' "$BS")"
[ -n "$alblock" ] || { echo "FAIL: no acquire_run_lock found in $BS"; exit 1; }
[ "$(printf '%s\n' "$alblock" | wc -l)" -lt 15 ] \
  || { echo "FAIL: the extracted block is not acquire_run_lock -- did its closing brace move?"; exit 1; }
case "$(awk '/^main\(\) \{/,/^}/' "$BS" | sed -n 2p)" in
  "  acquire_run_lock") echo "PASS the installer takes its run lock before anything else" ;;
  *) echo "FAIL main must start with acquire_run_lock"; fails=$((fails + 1)) ;;
esac
lkdir="$(mktemp -d)"
run_lock() { # $1: flock's exit status ("" = no flock on the host)
  FLOCK_RC="$1" RUN_LOCK_FILE="$lkdir/run.lock" NOPATH="$lkdir/nopath" bash -c '
    set -Eeuo pipefail
    warn() { printf "WARN: %s\n" "$*"; }; die() { printf "DIE: %s\n" "$*"; exit 1; }
    if [ -n "$FLOCK_RC" ]; then
      flock() { printf "FLOCK: %s\n" "$*"; if (: >&9) 2>/dev/null; then echo "FD9 OPEN"; fi; return "$FLOCK_RC"; }
    else
      PATH="$NOPATH"
    fi
    '"$alblock"'
    acquire_run_lock
    echo CONTINUES' 2>&1
}
out="$(run_lock 0)"
expect "the installer takes its run lock on the lock file" "FLOCK: -n 9
FD9 OPEN
CONTINUES" "$out"
[ -e "$lkdir/run.lock" ] && echo "PASS   which it creates" || { echo "FAIL the run lock file was not created"; fails=$((fails + 1)); }
out="$(run_lock 1)"
expect "a run lock held by another run stops the install" "DIE: another installer run is still going on this host" "$out"
case "$out" in *CONTINUES*) echo "FAIL a second installer run must stop: $out"; fails=$((fails + 1)) ;; esac
expect "a host without flock warns and carries on" "WARN: flock not found" "$(run_lock "")"
if command -v flock >/dev/null 2>&1; then
  out="$(flock "$lkdir/held.lock" env RUN_LOCK_FILE="$lkdir/held.lock" bash -c '
    die() { printf "DIE: %s\n" "$*"; exit 1; }; warn() { :; }
    '"$alblock"'
    acquire_run_lock
    echo CONTINUES' 2>&1)"
  expect "a lock another process holds stops the install (real flock)" "DIE: another installer run" "$out"
else
  echo "SKIP a lock another process holds (real flock): flock is not installed here"
fi
rm -rf "$lkdir"



# --- the proxy heap -----------------------------------------------------------------------
hblock="$(awk '/^heap_megabytes\(\) \{/,/^}/' "$BS")"
[ -n "$hblock" ] || { echo "FAIL: no heap_megabytes found in $BS"; exit 1; }
heap() { bash -c "$hblock"'
heap_megabytes "$1"' _ "$1"; }
expect "a heap in gigabytes converts to megabytes" "2048" "$(heap 2G)"
expect "a heap in megabytes is kept" "768" "$(heap 768m)"
for bad in 1 1K 0G 01G G -1G 1.5G 9999999G; do
  expect "the heap spelling '$bad' is refused" "0" "$(heap "$bad")"
done
# The unit written for each ceiling is checked with the rerun tests above.

# --- reproducible image ids ---------------------------------------------------------------
# restart_existing_system_servers compares image ids across runs; a default BuildKit
# provenance attestation (it carries a timestamp) would make every rebuild look new.
attest_line="$(grep -n '^export BUILDX_NO_DEFAULT_ATTESTATIONS=1$' "$BS" | cut -d: -f1 | head -1)"
build_line="$(grep -n '^  docker build ' "$BS" | cut -d: -f1 | head -1)"
if [ -n "$attest_line" ] && [ -n "$build_line" ] && [ "$attest_line" -lt "$build_line" ]; then
  echo "PASS default build attestations are off before the first docker build"
else
  echo "FAIL BUILDX_NO_DEFAULT_ATTESTATIONS=1 must be exported before the first docker build"; fails=$((fails + 1))
fi

# --- a failed run puts the previous host binary back until the new one is in use ----------
hbdir="$(mktemp -d)"
run_host_bin() { # exit-status in-use [no-previous]
  rm -f "$hbdir"/felis*
  [ -n "${3:-}" ] || { printf 'old\n' > "$hbdir/felis"; chmod 0755 "$hbdir/felis"; }
  HOST_BIN="$hbdir/felis" bash -c '
    set -e
    warn() { printf "WARN: %s\n" "$*"; }
    HOST_BIN_PREV=""; HOST_BIN_KEPT=0; HOST_BIN_IN_USE='"$2"'
    '"$(awk '/^keep_previous_host_binary\(\) \{/,/^}/' "$BS")"'
    '"$(awk '/^restore_previous_host_binary\(\) \{/,/^}/' "$BS")"'
    keep_previous_host_binary
    rm -f "$HOST_BIN"; printf "new\n" > "$HOST_BIN"; chmod 0755 "$HOST_BIN"
    keep_previous_host_binary # a second replacement keeps the first original
    restore_previous_host_binary '"$1"'
    printf "BIN: %s\n" "$(cat "$HOST_BIN")"
    [ -e "$HOST_BIN.prev" ] && echo "PREV LEFT" || true'
}
out="$(run_host_bin 1 0)"
expect "a run that fails before the new binary is used restores the old one" "BIN: old" "$out"
expect "the restore says so" "WARN: restored the previous felis binary" "$out"
out="$(run_host_bin 1 1)"
expect "a run that fails after migrations keeps the new binary" "BIN: new" "$out"
out="$(run_host_bin 0 0)"
expect "a successful run keeps the new binary" "BIN: new" "$out"
case "$out" in *"PREV LEFT"*) echo "FAIL: the previous binary copy must be removed"; fails=$((fails + 1)) ;; esac
out="$(run_host_bin 1 0 fresh)"
expect "a first install has nothing to restore" "BIN: new" "$out"
case "$out" in *WARN:*) echo "FAIL: a first install must not restore the binary it just installed"; fails=$((fails + 1)) ;; esac
# A copy cut short (a full disk) stops the run, and the exit's restore must not put it over
# the binary it was taken from.
rm -f "$hbdir"/felis*
printf 'old\n' > "$hbdir/felis"; chmod 0755 "$hbdir/felis"
out="$(HOST_BIN="$hbdir/felis" bash -c '
  set -e
  warn() { printf "WARN: %s\n" "$*"; }
  HOST_BIN_PREV=""; HOST_BIN_KEPT=0; HOST_BIN_IN_USE=0
  '"$(awk '/^keep_previous_host_binary\(\) \{/,/^}/' "$BS")"'
  '"$(awk '/^restore_previous_host_binary\(\) \{/,/^}/' "$BS")"'
  cp() { printf "ol" > "$2"; return 1; }
  on_exit() { restore_previous_host_binary "$?"; printf "BIN: %s\n" "$(cat "$HOST_BIN")"; [ ! -e "$HOST_BIN.prev" ] || echo "PREV LEFT"; }
  trap on_exit EXIT
  keep_previous_host_binary
  echo KEPT' 2>&1)"
expect "a copy of the binary cut short leaves the binary as it was" "BIN: old" "$out"
case "$out" in
  *KEPT*|*"PREV LEFT"*) echo "FAIL a copy of the binary cut short went on or was left behind: $out"; fails=$((fails + 1)) ;;
  *) echo "PASS a copy of the binary cut short stops the run and is removed" ;;
esac
rm -rf "$hbdir"


# --- a rerun reads the host right and keeps the proxy up --------------------------------
# Both checks run under bootstrap.sh's own `set -Eeuo pipefail`. The lists are far past one
# pipe buffer with the match on the first line, so a `cmd | grep -q` has grep exit while cmd
# is still writing: cmd dies of SIGPIPE, pipefail fails the pipeline, and the install took
# the "missing" branch on a host that had it (CI caught the PostgreSQL case reinstalling a
# package on a rerun). apt then ran needrestart, which restarted felis-velocity.

for f in import_platform_images apt_get; do
  [ -n "$(awk '/^'"$f"'\(\) \{/,/^}/' "$BS")" ] || { echo "FAIL: no ${f} in $BS"; exit 1; }
  [ "$(awk '/^'"$f"'\(\) \{/,/^}/' "$BS" | wc -l)" -lt 20 ] \
    || { echo "FAIL: the extracted ${f} is not just the function -- did its closing brace move?"; exit 1; }
done

rrdir="$(mktemp -d)"
cat > "$rrdir/apt-get" <<'EOF'
#!/bin/sh
echo "NEEDRESTART_SUSPEND=${NEEDRESTART_SUSPEND:-unset} $*"
EOF
chmod +x "$rrdir/apt-get"

run_reg() {
  bash -c '
    set -Eeuo pipefail
    ok() { echo "OK: $*"; }; log() { echo "LOG: $*"; }; warn() { echo "WARN: $*"; }
    REGISTRY_IMAGE=registry:2@sha256:aa
    POSTGRES_IMAGE=postgres:18@sha256:bb
    k3s_cmd() {
      if [ "$1" = ctr ]; then
        echo registry@sha256:aa
        echo postgres@sha256:bb
        seq 1 200000 | sed "s/.*/example.test\/img-&:1/"
      else
        echo "PULLED $*"
      fi
    }
    '"$(awk '/^pinned_image_ref\(\) \{/,/^}/' "$BS")"'
    '"$(awk '/^import_platform_images\(\) \{/,/^}/' "$BS")"'
    import_platform_images'
}
out="$(run_reg)"
expect "an imported registry image is found in a long image list" "OK: registry:2@sha256:aa already in k3s containerd" "$out"
expect "an imported database image is found in a long image list" "OK: postgres:18@sha256:bb already in k3s containerd" "$out"
case "$out" in *PULLED*) echo "FAIL: a platform image was pulled again"; fails=$((fails + 1)) ;; esac

out="$(PATH="$rrdir:$PATH" bash -c '
  set -Eeuo pipefail
  wait_for_pkg_locks() { :; }
  PKG_LOCK_TIMEOUT=5
  '"$(awk '/^apt_get\(\) \{/,/^}/' "$BS")"'
  apt_get install -y postgresql')"
expect "the install's apt runs keep needrestart from restarting services" \
  "NEEDRESTART_SUSPEND=1 -o DPkg::Lock::Timeout=5 install -y postgresql" "$out"
rm -rf "$rrdir"

# --- host hardening: k3s settings, clock, journal, the node address ----------------------

kcblock="$(awk '/^k3s_node_name\(\) \{/,/^}/' "$BS")
$(awk '/^write_k3s_config\(\) \{/,/^}/' "$BS")"
case "$kcblock" in
  *'k3s_node_name() {'*'write_k3s_config() {'*) ;;
  *) echo "FAIL: k3s_node_name / write_k3s_config not found in $BS"; exit 1 ;;
esac
[ "$(printf '%s\n' "$kcblock" | wc -l)" -lt 60 ] \
  || { echo "FAIL: the extracted k3s settings blocks ran past their closing braces"; exit 1; }

kdir="$(mktemp -d)"
# The kubelet client certificate a k3s agent holds: k3s issues it with exactly this subject.
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 1 \
  -keyout "$kdir/kubelet.key" -out "$kdir/kubelet.crt" \
  -subj "/O=system:nodes/CN=system:node:localhost.localdomain" >/dev/null 2>&1
printf '#!/bin/sh\n' > "$kdir/k3s"; chmod +x "$kdir/k3s"

run_k3s_config() { # $1: kubelet cert path, $2: k3s binary path, $3: hostname, $4: openssl subject stub ("" = real openssl)
  CERT="$1" BIN="$2" HOSTNAME_STUB="$3" SUBJECT="${4:-}" DROPIN="$kdir/config.yaml.d/50-felis.yaml" bash -c '
    set -Eeuo pipefail
    ok() { echo "OK: $*"; }; log() { echo "LOG: $*"; }; warn() { echo "WARN: $*"; }
    remember_temp() { :; }
    restore_label() { echo "RELABEL: $*"; }
    mktemp() { local p; p="$(command mktemp "$@")"; echo "MKTEMP: $(dirname "$p")" >&2; echo "$p"; }
    uname() { echo "$HOSTNAME_STUB"; }
    if [ -n "$SUBJECT" ]; then openssl() { echo "$SUBJECT"; }; fi
    K3S_CONFIG_DROPIN="$DROPIN" K3S_KUBELET_CERT="$CERT" K3S_BIN="$BIN"
    '"$kcblock"'
    K3S_RESTART_NEEDED=0
    write_k3s_config
    echo "RESTART=$K3S_RESTART_NEEDED"' 2>&1
}
dropin="$kdir/config.yaml.d/50-felis.yaml"

out="$(run_k3s_config "$kdir/none.crt" "$kdir/none" Rocky-Box)"
expect "a fresh host pins the hostname k3s would take, lowercased" 'node-name: "rocky-box"' "$(cat "$dropin")"
expect "the admin kubeconfig is root-only" 'write-kubeconfig-mode: "0600"' "$(cat "$dropin")"
expect "a new drop-in asks for a k3s restart" "RESTART=1" "$out"
if [ "$(printf '%s\n' "$out" | sed -n 's/^MKTEMP: //p' | sort -u)" = "$kdir/config.yaml.d" ]; then
  echo "PASS the k3s drop-in is made beside itself, never under /tmp"
else
  echo "FAIL temporary files for the k3s drop-in were made in: $(printf '%s\n' "$out" | sed -n 's/^MKTEMP: //p')"; fails=$((fails + 1))
fi
if [ "$(stat -c %a "$dropin" 2>/dev/null || stat -f %Lp "$dropin")" = 600 ]; then
  echo "PASS the k3s drop-in is root-only"
else
  echo "FAIL the k3s drop-in must be 0600"; fails=$((fails + 1))
fi

out="$(run_k3s_config "$kdir/none.crt" "$kdir/none" Rocky-Box)"
expect "an unchanged drop-in is left alone" "RESTART=0" "$out"
expect "an unchanged drop-in says so" 'OK: k3s settings already current (node name rocky-box)' "$out"
expect "an unchanged drop-in is still relabelled (one an earlier installer moved in from /tmp)" "RELABEL: $dropin" "$out"
if [ "$(ls "$kdir/config.yaml.d")" = "50-felis.yaml" ]; then
  echo "PASS no temporary file is left beside the k3s drop-in"
else
  echo "FAIL config.yaml.d holds: $(ls "$kdir/config.yaml.d")"; fails=$((fails + 1))
fi

rm -f "$dropin"
out="$(run_k3s_config "$kdir/kubelet.crt" "$kdir/k3s" renamed-host)"
expect "an installed node keeps the name it registered under, whatever the hostname says now" \
  'node-name: "localhost.localdomain"' "$(cat "$dropin")"

out="$(run_k3s_config "$kdir/kubelet.crt" "$kdir/k3s" renamed-host)"
expect "the certificate's name, once pinned, restarts nothing on a rerun" "RESTART=0" "$out"

printf '# old\nwrite-kubeconfig-mode: "0600"\nnode-name: "first-name"\n' > "$dropin"
out="$(run_k3s_config "$kdir/kubelet.crt" "$kdir/k3s" renamed-host)"
expect "a pinned name outranks the certificate" 'node-name: "first-name"' "$(cat "$dropin")"

# The three spellings of -subject: OpenSSL 1.1, and the slash form of 1.0 and LibreSSL.
rm -f "$dropin"
run_k3s_config "$kdir/kubelet.crt" "$kdir/k3s" x 'subject=O = system:nodes, CN = system:node:node-a' >/dev/null
expect "OpenSSL 1.1's subject is read" 'node-name: "node-a"' "$(cat "$dropin")"
rm -f "$dropin"
run_k3s_config "$kdir/kubelet.crt" "$kdir/k3s" x 'subject= /O=system:nodes/CN=system:node:node-b' >/dev/null
expect "the slash-form subject is read" 'node-name: "node-b"' "$(cat "$dropin")"

rm -f "$dropin"
out="$(run_k3s_config "$kdir/none.crt" "$kdir/k3s" renamed-host)"
case "$(cat "$dropin")" in
  *node-name*) echo "FAIL an installed k3s whose name cannot be read must not be pinned to the hostname"; fails=$((fails + 1)) ;;
  *) echo "PASS an installed k3s whose name cannot be read is not pinned to a guess" ;;
esac
expect "an unpinned name is a warning" "WARN: could not read this node's k3s name" "$out"
rm -f "$dropin"
printf 'half-written\n' > "$kdir/broken.crt"
out="$(run_k3s_config "$kdir/broken.crt" "$kdir/k3s" renamed-host)"
expect "a certificate openssl cannot parse is a warning, not a failed install" "WARN: could not read this node's k3s name" "$out"
expect "and the drop-in is still written" 'write-kubeconfig-mode: "0600"' "$(cat "$dropin")"

# k3s's Go collector runs at GOGC=50 through a systemd drop-in, written beside itself and
# loaded (with a k3s restart) only when it changed.
svblock="$(awk '/^write_k3s_service_dropin\(\) \{/,/^}/' "$BS")"
[ -n "$svblock" ] || { echo "FAIL: no write_k3s_service_dropin found in $BS"; exit 1; }
svdropin="$kdir/k3s.service.d/50-felis.conf"
run_k3s_service_dropin() {
  DROPIN="$svdropin" bash -c '
    set -Eeuo pipefail
    ok() { echo "OK: $*"; }; log() { echo "LOG: $*"; }
    remember_temp() { :; }
    restore_label() { echo "RELABEL: $*"; }
    systemctl() { echo "SYSTEMCTL: $*"; }
    mktemp() { local p; p="$(command mktemp "$@")"; echo "MKTEMP: $(dirname "$p")" >&2; echo "$p"; }
    K3S_SERVICE_DROPIN="$DROPIN"
    '"$svblock"'
    K3S_RESTART_NEEDED=0
    write_k3s_service_dropin
    echo "RESTART=$K3S_RESTART_NEEDED"' 2>&1
}
out="$(run_k3s_service_dropin)"
if [ "$(cat "$svdropin")" = '# Written by the Felis installer (deploy/bootstrap.sh); a rerun rewrites it.
[Service]
Environment=GOGC=50' ]; then
  echo "PASS k3s runs its Go collector at GOGC=50"
else
  echo "FAIL the k3s service drop-in is:"; cat "$svdropin"; fails=$((fails + 1))
fi
expect "a new service drop-in is loaded" "SYSTEMCTL: daemon-reload" "$out"
expect "a new service drop-in asks for a k3s restart" "RESTART=1" "$out"
if [ "$(printf '%s\n' "$out" | sed -n 's/^MKTEMP: //p' | sort -u)" = "$kdir/k3s.service.d" ]; then
  echo "PASS the service drop-in is made beside itself, never under /tmp"
else
  echo "FAIL temporary files for the service drop-in were made in: $(printf '%s\n' "$out" | sed -n 's/^MKTEMP: //p')"; fails=$((fails + 1))
fi
if [ "$(stat -c %a "$svdropin" 2>/dev/null || stat -f %Lp "$svdropin")" = 644 ]; then
  echo "PASS the service drop-in is readable like the unit it extends"
else
  echo "FAIL the service drop-in must be 0644"; fails=$((fails + 1))
fi
out="$(run_k3s_service_dropin)"
expect "an unchanged service drop-in restarts nothing" "RESTART=0" "$out"
expect "an unchanged service drop-in says so" "OK: k3s service environment already current" "$out"
case "$out" in
  *daemon-reload*) echo "FAIL an unchanged service drop-in must not reload systemd"; fails=$((fails + 1)) ;;
  *) echo "PASS an unchanged service drop-in reloads nothing" ;;
esac
expect "an unchanged service drop-in is still relabelled" "RELABEL: $svdropin" "$out"
if [ "$(ls "$kdir/k3s.service.d")" = "50-felis.conf" ]; then
  echo "PASS no temporary file is left beside the service drop-in"
else
  echo "FAIL k3s.service.d holds: $(ls "$kdir/k3s.service.d")"; fails=$((fails + 1))
fi
printf '[Service]\nEnvironment=GOGC=100\n' > "$svdropin"
out="$(run_k3s_service_dropin)"
expect "an edited service drop-in is put back" "Environment=GOGC=50" "$(cat "$svdropin")"
expect "and k3s is restarted onto it" "RESTART=1" "$out"

sblock="$(awk '/^strip_k3s_kubeconfig_mode_flag\(\) \{/,/^}/' "$BS")"
[ -n "$sblock" ] || { echo "FAIL: no strip_k3s_kubeconfig_mode_flag found in $BS"; exit 1; }
# ExecStart as k3s's installer writes it (copied off an install made with the old flag).
tab="$(printf '\t')"
cat > "$kdir/k3s.service" <<EOF
ExecStartPre=-/sbin/modprobe overlay
ExecStart=/usr/local/bin/k3s \\
    server \\
${tab}'--disable' \\
${tab}'metrics-server' \\
${tab}'--write-kubeconfig-mode' \\
${tab}'644' \\

EOF
run_strip() {
  UNIT="$kdir/k3s.service" bash -c '
    set -Eeuo pipefail
    log() { echo "LOG: $*"; }
    remember_temp() { :; }
    systemctl() { echo "SYSTEMCTL: $*"; }
    K3S_UNIT_FILE="$UNIT"
    '"$sblock"'
    K3S_RESTART_NEEDED=0
    strip_k3s_kubeconfig_mode_flag
    echo "RESTART=$K3S_RESTART_NEEDED"' 2>&1
}
out="$(run_strip)"
want="ExecStartPre=-/sbin/modprobe overlay
ExecStart=/usr/local/bin/k3s \\
    server \\
${tab}'--disable' \\
${tab}'metrics-server' \\
"
if [ "$(cat "$kdir/k3s.service"; echo x)" = "${want}
x" ]; then
  echo "PASS the old kubeconfig-mode flag and its value leave the k3s unit, the rest stays"
else
  echo "FAIL the stripped unit is:"; cat "$kdir/k3s.service"; fails=$((fails + 1))
fi
expect "a rewritten unit is reloaded" "SYSTEMCTL: daemon-reload" "$out"
expect "a rewritten unit asks for a k3s restart" "RESTART=1" "$out"
out="$(run_strip)"
expect "a unit without the flag is left alone" "RESTART=0" "$out"
case "$out" in *daemon-reload*) echo "FAIL a unit without the flag must not be reloaded"; fails=$((fails + 1)) ;; esac
printf "ExecStart=/usr/local/bin/k3s \\\\\n    server \\\\\n\t'--write-kubeconfig-mode=644' \\\\\n\t'--disable' \\\\\n\t'traefik' \\\\\n" > "$kdir/k3s.service"
run_strip >/dev/null
expect "the one-word spelling goes too, and only that word" "$(printf "    server \\\\\n\t'--disable' \\\\\n\t'traefik' \\\\")" "$(cat "$kdir/k3s.service")"
case "$(cat "$kdir/k3s.service")" in *kubeconfig-mode*) echo "FAIL the one-word flag is still in the unit"; fails=$((fails + 1)) ;; esac

case "$(awk '/^run_k3s_installer\(\) \{/,/^}/' "$BS")" in
  *write-kubeconfig-mode*) echo "FAIL the k3s installer must not be told a kubeconfig mode; the drop-in holds it"; fails=$((fails + 1)) ;;
  *INSTALL_K3S_EXEC=*) echo "PASS the k3s installer leaves the kubeconfig mode to the drop-in" ;;
  *) echo "FAIL: no run_k3s_installer found in $BS"; fails=$((fails + 1)) ;;
esac

iblock="$(awk '/^install_k3s\(\) \{/,/^}/' "$BS")"
[ -n "$iblock" ] || { echo "FAIL: no install_k3s found in $BS"; exit 1; }
iblock="$iblock
$(awk '/^version_newer\(\) \{/,/^}/' "$BS")
$(awk '/^k3s_upgrade_allowed\(\) \{/,/^}/' "$BS")"
run_install_k3s() { # $1: installed version ("" = none, "broken" = a binary that does not run), $2: config drop-in changed (0|1), $3: FELIS_UPGRADE_DEPS, $4: service drop-in changed (0|1), $5: k3s.service present (1|0)
  INSTALLED="$1" CHANGED="$2" UPGRADE="${3:-0}" SVC_CHANGED="${4:-0}" UNIT="${5:-1}" KDIR="$kdir" bash -c '
    set -Eeuo pipefail
    ok() { echo "OK: $*"; }; log() { echo "LOG: $*"; }; warn() { echo "WARN: $*"; }
    die() { echo "DIE: $*"; exit 1; }
    FELIS_K3S_VERSION=v1.36.4+k3s1 FELIS_UPGRADE_DEPS="$UPGRADE" K3S_BIN_DIR="$KDIR" K3S_BIN="$KDIR/k3s-under-test"
    K3S_CONFIG_DROPIN=/etc/rancher/k3s/config.yaml.d/50-felis.yaml K3S_KUBECONFIG=/etc/rancher/k3s/k3s.yaml
    K3S_SERVICE_DROPIN=/etc/systemd/system/k3s.service.d/50-felis.conf K3S_UNIT_FILE="$KDIR/k3s.service-under-test"
    rm -f "$K3S_BIN" "$K3S_UNIT_FILE"
    [ "$UNIT" = 0 ] || : > "$K3S_UNIT_FILE"
    case "$INSTALLED" in
      "") ;;
      broken) printf "#!/bin/sh\nexit 1\n" > "$K3S_BIN"; chmod +x "$K3S_BIN" ;;
      *) printf "#!/bin/sh\necho \"k3s version %s (abc)\"\n" "$INSTALLED" > "$K3S_BIN"; chmod +x "$K3S_BIN" ;;
    esac
    configure_k3s_firewall() { :; }
    write_k3s_config() { [ "$CHANGED" = 0 ] || K3S_RESTART_NEEDED=1; }
    write_k3s_service_dropin() { echo "SERVICE-DROPIN"; [ "$SVC_CHANGED" = 0 ] || K3S_RESTART_NEEDED=1; }
    strip_k3s_kubeconfig_mode_flag() { :; }
    run_k3s_installer() {
      echo "INSTALLER${1:+ $1}"
      [ "${1:-}" = --keep-binary ] || { printf "#!/bin/sh\n" > "$K3S_BIN"; command chmod +x "$K3S_BIN"; }
    }
    stage_k3s_airgap_images() { echo "STAGE"; }
    systemctl() { echo "SYSTEMCTL: $*"; }
    wait_for_node_ready() { echo "READY"; }
    chmod() { echo "CHMOD: $*"; }
    '"$iblock"'
    install_k3s' 2>&1
}
out="$(run_install_k3s v1.36.4+k3s1 1)"
expect "new k3s settings on a running k3s restart it" "SYSTEMCTL: restart k3s" "$out"
expect "the admin kubeconfig is made root-only once the node is up" "READY
CHMOD: 0600 /etc/rancher/k3s/k3s.yaml" "$out"
out="$(run_install_k3s v1.36.4+k3s1 0)"
case "$out" in *"restart k3s"*) echo "FAIL unchanged k3s settings must not restart k3s"; fails=$((fails + 1)) ;; *) echo "PASS unchanged k3s settings restart nothing" ;; esac
expect "a new service environment alone restarts a running k3s" "SYSTEMCTL: restart k3s" "$(run_install_k3s v1.36.4+k3s1 0 0 1)"
out="$(run_install_k3s "" 1)"
expect "a fresh host runs the k3s installer and waits for the node" "INSTALLER
SYSTEMCTL: enable --now k3s" "$out"
expect "a fresh k3s has its service environment before its first start" "SERVICE-DROPIN INSTALLER " \
  "$(printf '%s\n' "$out" | grep -E '^(SERVICE-DROPIN|INSTALLER)$' | tr '\n' ' ')"
expect "a fresh host stages k3s's images before k3s first starts" "STAGE
INSTALLER" "$out"
expect "a fresh host's kubeconfig is made root-only too" "CHMOD: 0600 /etc/rancher/k3s/k3s.yaml" "$out"
case "$out" in *"restart k3s"*) echo "FAIL the k3s installer already started k3s on the new settings; no second restart"; fails=$((fails + 1)) ;; *) echo "PASS a fresh k3s is not restarted a second time" ;; esac
out="$(run_install_k3s v1.35.2+k3s1 1 1)"
expect "an upgrade runs the k3s installer" "INSTALLER" "$out"
case "$out" in *"restart k3s"*) echo "FAIL the upgrade already restarted k3s on the new settings; no second restart"; fails=$((fails + 1)) ;; *) echo "PASS an upgraded k3s is not restarted a second time" ;; esac
# k3s's installer puts the binary in place before it writes k3s.service: a run cut short
# between the two is finished around that binary, at its own version.
out="$(run_install_k3s v1.36.4+k3s1 1 0 0 0)"
expect "a k3s binary without k3s.service has its installer finish around it" "INSTALLER --keep-binary
SYSTEMCTL: enable --now k3s" "$out"
case "$out" in *"restart k3s"*) echo "FAIL the finishing installer already started k3s; no second restart"; fails=$((fails + 1)) ;; *) echo "PASS   and k3s is not restarted a second time" ;; esac
expect "an older binary without k3s.service is finished at its own version" "INSTALLER --keep-binary" \
  "$(run_install_k3s v1.35.2+k3s1 0 1 0 0)"
out="$(run_install_k3s broken 0 0 0 0)"
expect "a k3s binary that does not run, without k3s.service, stops the install" \
  "does not run and k3s.service is missing: an earlier k3s install was cut short" "$out"
case "$out" in *INSTALLER*) echo "FAIL a k3s binary that does not run must not be installed around"; fails=$((fails + 1)) ;; *) echo "PASS   and the installer is not run around it" ;; esac
rm -rf "$kdir"

jblock="$(awk '/^ensure_persistent_journal\(\) \{/,/^}/' "$BS")"
[ -n "$jblock" ] || { echo "FAIL: no ensure_persistent_journal found in $BS"; exit 1; }
jdir="$(mktemp -d)"
jcalls="$(mktemp)"
run_journal() { # $1: FELIS_JOURNAL_MAX_USE, $2: FELIS_MANAGE_JOURNAL, $3: a journald restart creates the journal directory (1|0)
  MAXUSE="$1" MANAGE="${2:-1}" MAKES="${3:-1}" DROPIN="$jdir/journald.conf.d/50-felis.conf" JDIR="$jdir/journal" \
    JCALLS="$jcalls" bash -c '
    set -Eeuo pipefail
    ok() { echo "OK: $*"; }; log() { echo "LOG: $*"; }; warn() { echo "WARN: $*"; }
    remember_temp() { :; }
    # Where each temporary file lands, on stderr: stdout is the path the caller captures.
    mktemp() { local p; p="$(command mktemp "$@")"; echo "MKTEMP: $(dirname "$p")" >&2; echo "$p"; }
    restore_label() { echo "RELABEL: $*"; }
    # journald creates the directory as it starts with Storage=persistent.
    systemctl() { echo "SYSTEMCTL: $*"; [ "$MAKES" = 0 ] || mkdir -p "$JDIR"; }
    journalctl() { echo "JOURNALCTL: $*" >> "$JCALLS"; }
    FELIS_JOURNAL_MAX_USE="$MAXUSE" FELIS_MANAGE_JOURNAL="$MANAGE" JOURNALD_DROPIN="$DROPIN" JOURNAL_DIR="$JDIR"
    '"$jblock"'
    ensure_persistent_journal' 2>&1
}
out="$(run_journal 1G)"
if [ "$(cat "$jdir/journald.conf.d/50-felis.conf")" = "[Journal]
Storage=persistent
SystemMaxUse=1G" ]; then
  echo "PASS the journal is made persistent and capped"
else
  echo "FAIL journald drop-in:"; echo "$out"; fails=$((fails + 1))
fi
expect "a new journald drop-in restarts journald" "SYSTEMCTL: restart systemd-journald" "$out"
jmode="$(stat -c %a "$jdir/journald.conf.d/50-felis.conf" 2>/dev/null || stat -f %Lp "$jdir/journald.conf.d/50-felis.conf")"
if [ "$jmode" = 644 ]; then
  echo "PASS the journald drop-in is readable like the rest of /etc/systemd (systemd-analyze cat-config)"
else
  echo "FAIL the journald drop-in is mode $jmode, want 644"; fails=$((fails + 1))
fi
expect "the drop-in gets its directory's SELinux label before journald reads it" "RELABEL: $jdir/journald.conf.d/50-felis.conf
SYSTEMCTL: restart systemd-journald" "$out"
expect "the runtime journal is flushed to disk" "JOURNALCTL: --flush" "$(cat "$jcalls")"
# A file made under /tmp and moved into place keeps user_tmp_t, which journald is denied.
if [ "$(printf '%s\n' "$out" | sed -n 's/^MKTEMP: //p' | sort -u)" = "$jdir/journald.conf.d" ]; then
  echo "PASS the journald drop-in is made beside itself, never under /tmp"
else
  echo "FAIL temporary files for the journald drop-in were made in: $(printf '%s\n' "$out" | sed -n 's/^MKTEMP: //p')"; fails=$((fails + 1))
fi
expect "the journal directory journald made is the proof" "OK: system journal is persistent under $jdir/journal" "$out"
if [ "$(ls "$jdir/journald.conf.d")" = "50-felis.conf" ]; then
  echo "PASS no temporary file is left beside the journald drop-in"
else
  echo "FAIL journald.conf.d holds: $(ls "$jdir/journald.conf.d")"; fails=$((fails + 1))
fi
out="$(run_journal 1G)"
case "$out" in *restart*) echo "FAIL a current drop-in journald has taken up must not restart it"; fails=$((fails + 1)) ;; *) echo "PASS a current, working drop-in restarts nothing" ;; esac
rmdir "$jdir/journal"
out="$(run_journal 1G)"
expect "a current drop-in journald never took up (no journal directory) restarts it" "LOG: journald has not taken up" "$out"
expect "and that restart happens" "SYSTEMCTL: restart systemd-journald" "$out"
rmdir "$jdir/journal"
out="$(run_journal 1G 1 0)"
expect "a journald that still stores nothing on disk is a warning" "WARN: journald did not create $jdir/journal" "$out"
out="$(run_journal 4G)"
expect "a new cap is written" "SystemMaxUse=4G" "$(cat "$jdir/journald.conf.d/50-felis.conf")"
expect "a new cap restarts journald" "SYSTEMCTL: restart systemd-journald" "$out"
rm -rf "$jdir"
out="$(run_journal 1G 0)"
if [ -e "$jdir/journald.conf.d/50-felis.conf" ]; then
  echo "FAIL FELIS_MANAGE_JOURNAL=0 must leave journald alone"; fails=$((fails + 1))
else
  echo "PASS FELIS_MANAGE_JOURNAL=0 leaves journald alone"
fi
rm -f "$jcalls"

tblock="$(awk '/^ensure_time_sync\(\) \{/,/^}/' "$BS")"
[ -n "$tblock" ] || { echo "FAIL: no ensure_time_sync found in $BS"; exit 1; }
run_time() { # $1: NTP now, $2: set-ntp works before chrony (0|1), $3: synchronizes (0|1), $4: FELIS_MANAGE_TIME_SYNC
  NTP="$1" SETWORKS="$2" SYNCS="$3" MANAGE="${4:-1}" bash -c '
    set -Eeuo pipefail
    ok() { echo "OK: $*"; }; log() { echo "LOG: $*"; }; warn() { echo "WARN: $*"; }
    sleep() { :; }
    pkg_install() { echo "PKG: $*"; SETWORKS=1; }
    timedatectl() {
      case "$*" in
        "show -p NTP --value") echo "$NTP" ;;
        "show -p NTPSynchronized --value") [ "$SYNCS" = 1 ] && echo yes || echo no ;;
        "set-ntp true") echo "TIMEDATECTL: set-ntp true"; [ "$SETWORKS" = 1 ] || { echo "Failed to set ntp: NTP not supported" >&2; return 1; } ;;
        *) echo "TIMEDATECTL?: $*" ;;
      esac
    }
    FELIS_MANAGE_TIME_SYNC="$MANAGE"
    '"$tblock"'
    ensure_time_sync' 2>&1
}
out="$(run_time no 1 1)"
expect "NTP is turned on where it is off" "TIMEDATECTL: set-ntp true" "$out"
expect "a synchronized clock is reported" "OK: system clock synchronized by NTP" "$out"
case "$out" in *PKG:*) echo "FAIL chrony must not be installed where timedatectl has a client to enable"; fails=$((fails + 1)) ;; esac
case "$out" in *WARN:*) echo "FAIL a synchronized clock must not warn"; fails=$((fails + 1)) ;; esac
out="$(run_time no 0 1)"
expect "with no NTP client to enable, chrony is installed" "PKG: chrony" "$out"
expect "and NTP is turned on after it" "PKG: chrony
TIMEDATECTL: set-ntp true" "$out"
out="$(run_time yes 1 1)"
case "$out" in *set-ntp*) echo "FAIL NTP already on must not be set again"; fails=$((fails + 1)) ;; *) echo "PASS NTP already on is left as it is" ;; esac
out="$(run_time yes 1 0)"
expect "a clock that does not synchronize is a warning, not a stop" "WARN: the system clock is not synchronized yet" "$out"
out="$(run_time no 1 1 0)"
case "$out" in *TIMEDATECTL*) echo "FAIL FELIS_MANAGE_TIME_SYNC=0 must leave the clock alone"; fails=$((fails + 1)) ;; *) echo "PASS FELIS_MANAGE_TIME_SYNC=0 leaves the clock alone" ;; esac

dblock="$(awk '/^warn_dynamic_node_ip\(\) \{/,/^}/' "$BS")"
[ -n "$dblock" ] || { echo "FAIL: no warn_dynamic_node_ip found in $BS"; exit 1; }
run_dyn() { # $1: NODE_IP, $2: `ip -4 -o addr show` output
  NODE_IP="$1" ADDRS="$2" bash -c '
    set -Eeuo pipefail
    warn() { echo "WARN: $*"; }
    ip() { printf "%s\n" "$ADDRS"; }
    '"$dblock"'
    warn_dynamic_node_ip; echo done' 2>&1
}
# `ip -4 -o addr show` lines as iproute2 prints them (the leased one is off the test host).
lo='1: lo    inet 127.0.0.1/8 scope host lo\       valid_lft forever preferred_lft forever'
leased='2: enp0s5    inet 10.211.55.6/24 brd 10.211.55.255 scope global dynamic noprefixroute enp0s5\       valid_lft 1459sec preferred_lft 1459sec'
static='2: enp0s5    inet 10.211.55.6/24 brd 10.211.55.255 scope global noprefixroute enp0s5\       valid_lft forever preferred_lft forever'
other='3: wlan0    inet 192.168.1.20/24 brd 192.168.1.255 scope global dynamic wlan0\       valid_lft 3000sec preferred_lft 3000sec'
out="$(run_dyn 10.211.55.6 "$lo
$leased")"
expect "a leased node address is a warning" "WARN: 10.211.55.6 is a DHCP lease" "$out"
out="$(run_dyn 10.211.55.6 "$lo
$static
$other")"
case "$out" in *WARN*) echo "FAIL a static node address must not warn because another interface is leased"; fails=$((fails + 1)) ;; *) echo "PASS a static node address does not warn" ;; esac
out="$(run_dyn 10.211.55.60 "$leased")"
case "$out" in *WARN*) echo "FAIL a different address with the same prefix is not the node's"; fails=$((fails + 1)) ;; *) echo "PASS only the node's own address is judged" ;; esac

vsnip="$(awk '/local journal_n=/,/^  esac/' "$BS")"
[ -n "$vsnip" ] || { echo "FAIL: no FELIS_JOURNAL_MAX_USE check found in $BS"; exit 1; }
check_max_use() {
  FELIS_JOURNAL_MAX_USE="$1" bash -c '
    die() { echo "DIE: $*"; exit 1; }
    f() {
    '"$vsnip"'
    }
    f; echo accepted' 2>&1
}
for v in 1G 512M 900K; do
  expect "journal cap $v is accepted" "accepted" "$(check_max_use "$v")"
done
for v in 1g G 01G 1.5G 1GB 2T 1024 ""; do
  expect "journal cap '$v' is refused" "DIE: FELIS_JOURNAL_MAX_USE must be written" "$(check_max_use "$v")"
done

order="$(awk '/^main\(\) \{/,/^}/' "$BS" | grep -nE '^[[:space:]]*(detect_node_ip|install_base|ensure_time_sync|ensure_persistent_journal|install_k3s)$' | sed 's/^[0-9]*:[[:space:]]*//' | tr '\n' ' ')"
expect "main checks the address, then turns on NTP and the journal once packages install, before k3s" \
  "detect_node_ip install_base ensure_time_sync ensure_persistent_journal install_k3s " "$order"
expect "preflight is what warns about a leased address" "warn_dynamic_node_ip" "$(awk '/^preflight\(\) \{/,/^}/' "$BS")"

# --- preflight ---------------------------------------------------------------------------
# The whole preflight section runs against a stubbed host: every fact it reads (RAM,
# df, ss, systemctl, routes, curl) is answered from variables, so each case below is one
# changed fact and the verdict it must change.
pfblock="$(awk '/^PREFLIGHT_PROBLEMS=\(\)$/,/^preflight\(\) \{/' "$BS"; awk '/^preflight\(\) \{/,/^}/' "$BS" | tail -n +2)"
case "$pfblock" in *"preflight_outbound"*"FELIS_PREFLIGHT=warn installs anyway"*) ;; *) echo "FAIL: preflight section not found in $BS"; exit 1 ;; esac
wdblock="$(awk '/^warn_dynamic_node_ip\(\) \{/,/^}/' "$BS")"
pfroot="$(mktemp -d)"
# run_pf runs preflight with the stubbed facts in the environment; each defaults to a
# healthy host: 8 GiB RAM, one 100 GiB filesystem with 60 GiB free, no listeners, no
# other cluster, a LAN route, every download host answering. The install defaults to a
# source build (the dev channel), the one that writes the most and downloads from the most.
run_pf() {
  PF_ROOT="$pfroot" bash -c '
    set -Eeuo pipefail
    log() { echo "LOG: $*"; }
    ok() { echo "OK: $*"; }
    warn() { echo "WARN: $*"; }
    die() { echo "DIE: $*"; exit 1; }
    uname() { echo "${PF_ARCH:-x86_64}"; }
    df() { # -Pk path: the longest mount in PF_DF that prefixes path
      local row
      row="$(printf "%s\n" "${PF_DF:-/ 104857600 41943040 62914560}" | awk -v p="$2" "
        { if (index(p, \$1) == 1 && length(\$1) > best) { best = length(\$1); r = \$0 } } END { print r }")"
      echo "Filesystem 1024-blocks Used Available Capacity Mounted"
      set -- $row
      echo "/dev/x $2 $3 $4 50% $1"
    }
    ss() { # -Hltnp "sport = :PORT"
      local port="${2##*:}"
      printf "%s\n" "${PF_SS:-}" | grep -F ":${port} " || true
    }
    systemctl() {
      case "$1" in
        list-units) printf "%s\n" ${PF_ACTIVE:-} ${PF_STOPPED:-} ;;
        is-active) case " ${PF_ACTIVE:-} " in *" ${3:-} "*) return 0 ;; esac; return 1 ;;
      esac
    }
    ip() {
      case "$*" in
        "-4 route show") printf "%s\n" "${PF_ROUTES:-default via 192.168.1.1 dev eth0
192.168.1.0/24 dev eth0 proto kernel scope link src 192.168.1.20}" ;;
        *) printf "%s\n" "${PF_ADDRS:-}" ;;
      esac
    }
    # curl -w "%{time_appconnect}": a host in PF_DOWN never answers, one in PF_FLAKY drops
    # its first probe, one in PF_STALL completes TLS and then times out. Each probe of those
    # is counted in $PF_ROOT/tries.<host>.
    curl() {
      local host="${!#}"
      host="${host#https://}"; host="${host%/}"
      case " ${PF_DOWN:-} ${PF_FLAKY:-} ${PF_STALL:-} " in *" ${host} "*) printf x >> "$PF_ROOT/tries.${host}" ;; esac
      case " ${PF_FLAKY:-} " in *" ${host} "*)
        [ "$(cat "$PF_ROOT/tries.${host}")" = x ] && printf 0.000000 && return 7
        printf 0.300000; return ;;
      esac
      case " ${PF_STALL:-} " in *" ${host} "*) printf 0.481676; return 28 ;; esac
      case " ${PF_DOWN:-} " in *" ${host} "*) printf 0.000000; return 7 ;; *) printf 0.300000 ;; esac
    }
    sleep() { echo "SLEEP: $*"; }
    bootstrap_from_tui() { [ -n "${PF_TUI:-}" ]; }
    '"$(awk '/^use_release_binary\(\) \{/,/^}/' "$BS")"'
    FELIS_GAME_PORT=25565 FELIS_PANEL_NODEPORT=30443 REGISTRY_URL=registry.felis.svc:5000 PG_HOST_PORT=15432
    POD_CIDR=10.42.0.0/16 SERVICE_CIDR=10.43.0.0/16 NODE_IP="${PF_NODE_IP:-192.168.1.20}"
    K3S_BIN="$PF_ROOT/k3s" BOOTSTRAP_DONE="$PF_ROOT/bootstrap.done"
    FELIS_REF_PINNED="" FELIS_VELOCITY_FORK_JAR="" FELIS_PREFLIGHT="${PF_MODE:-strict}"
    FELIS_VERSION_BOOTSTRAP="${PF_CHANNEL:-dev}" FELIS_ARTIFACT_DIR="${PF_ARTIFACT_DIR:-}"
    FELIS_GAME_STACK="${PF_GAME_STACK:-pinned}"
    '"$wdblock"'
    '"$pfblock"'
    # The host readers the section defines, answered from the same facts.
    systemd_is_init() { [ "${PF_SYSTEMD:-1}" = 1 ]; }
    cgroup_controllers() { echo "${PF_CGROUPS:-cpuset cpu io memory pids}"; }
    mem_total_kb() { echo "${PF_MEM_KB:-8000000}"; }
    pid_unit() { printf "%s\n" "${PF_UNITS:-}" | awk -v p="$1" "\$1 == p { print \$2 }"; }
    existing_ancestor() { printf "%s\n" "$1"; }
    path_populated() { case " ${PF_POPULATED:-} " in *" $1 "*) return 0 ;; esac; return 1; }
    k3s_adds_selinux_rpm() { [ -n "${PF_SELINUX_RPM:-}" ]; }
    if [ -n "${PF_HOSTS:-}" ]; then preflight_hosts; else preflight; echo "WENT ON"; fi' 2>&1
}
out="$(run_pf)"
expect "a healthy host passes preflight" "OK: preflight passed" "$out"
expect "and the install goes on" "WENT ON" "$out"

out="$(PF_MEM_KB=1000000 run_pf)"
expect "a 1 GB host is refused" "976 MiB of RAM; the full stack needs at least 1792 MiB" "$out"
expect "and nano is named as what fits it" "FELIS_INSTALL_MODE=nano" "$out"
out="$(PF_MEM_KB=1950000 run_pf)"
expect "a 2 GB host installs" "WENT ON" "$out"
expect "with a warning about room for servers" "WARN: preflight: 1904 MiB of RAM runs the platform" "$out"

# 20 GiB free on the root filesystem: every path lands there and a first install writes
# 10 + 2 + 3 + 8 GiB, so only the sum refuses it.
out="$(PF_DF="/ 104857600 83886080 20971520" run_pf)"
expect "a first install that fits each budget but not their sum is refused" "/ has 20480 MiB free; this install writes about 23552 MiB there" "$out"
out="$(PF_DF="/ 104857600 83886080 20971520
/var/lib/rancher 52428800 1048576 51380224" run_pf)"
expect "the same host with k3s on its own disk passes" "OK: preflight passed" "$out"
out="$(PF_POPULATED="/var/lib/rancher /var/lib/felis /opt/felis /var/lib/containerd" PF_DF="/ 104857600 96468992 8388608" run_pf)"
expect "a rerun needs only room for new images" "WENT ON" "$out"
expect "and warns when that crosses k3s's image-GC line" "WARN: preflight: / will be over 85% full" "$out"
# The VM after a failed purge: Docker's cache stayed, k3s and /opt/felis went.
out="$(PF_POPULATED="/var/lib/felis /var/lib/containerd" PF_DF="/ 39755776 21827584 17928192" run_pf)"
expect "Docker's cache left by an earlier install is counted as there" "WENT ON" "$out"
out="$(PF_DF="/ 39755776 21827584 17928192" run_pf)"
expect "the same free space is too little for a bare host" "/ has 17508 MiB free; this install writes about 23552 MiB there" "$out"

ss_line() { printf 'LISTEN 0 4096 0.0.0.0:%s 0.0.0.0:* users:(("%s",pid=%s,fd=7))' "$1" "$2" "$3"; }
out="$(PF_SS="$(ss_line 25565 java 900)
$(ss_line 6443 k3s-server 812)" PF_UNITS="900 felis-velocity.service
812 k3s.service" run_pf)"
expect "the installer's own proxy and k3s pass on a rerun" "OK: preflight passed" "$out"
out="$(PF_SS="$(ss_line 25565 java 901)" PF_UNITS="901 minecraft.service" run_pf)"
expect "someone else's server on the game port is refused" "port 25565 (the Minecraft proxy (FELIS_GAME_PORT)) is already taken by java (pid 901, minecraft.service)" "$out"
out="$(PF_SS="$(ss_line 5000 python3 77)" run_pf)"
expect "a program on the registry's loopback port is refused" "port 5000 (the image registry's loopback port) is already taken by python3 (pid 77)" "$out"
out="$(PF_SS="$(ss_line 15432 socat 78)" run_pf)"
expect "a program on the database's loopback port is refused" "port 15432 (the database's loopback port) is already taken by socat (pid 78)" "$out"

out="$(PF_ACTIVE="kubelet.service" run_pf)"
expect "a kubeadm node is refused" "another Kubernetes runs here (kubelet.service)" "$out"
out="$(PF_STOPPED="kubelet.service" run_pf)"
expect "a kubelet left installed but stopped is no obstacle" "OK: preflight passed" "$out"
out="$(PF_ACTIVE="k3s-agent.service" run_pf)"
expect "a k3s agent is refused" "this host is a k3s agent" "$out"
: > "$pfroot/k3s"; chmod +x "$pfroot/k3s"
out="$(run_pf)"
expect "an existing k3s server is reused" "LOG: preflight: k3s is already installed" "$out"
rm -f "$pfroot/k3s"

out="$(PF_NODE_IP=10.42.7.9 run_pf)"
expect "a node address inside the pod range is refused" "10.42.7.9 is inside k3s's range 10.42.0.0/16" "$out"
out="$(PF_ROUTES="default via 192.168.1.1 dev eth0
10.42.0.0/24 dev cni0 proto kernel scope link src 10.42.0.1
10.43.0.0/16 dev docker0 proto kernel scope link src 10.43.0.1 linkdown" run_pf)"
expect "a Docker network inside the service range is refused" "the network 10.43.0.0/16 on docker0 lies inside k3s's 10.43.0.0/16" "$out"
case "$out" in *"10.42.0.0/24"*) echo "FAIL k3s's own cni0 route must not count against it"; fails=$((fails + 1)) ;; *) echo "PASS k3s's own cni0 route is its own" ;; esac
out="$(PF_ROUTES="default via 192.168.1.1 dev eth0
10.0.0.0/8 via 172.20.0.1 dev tun0" run_pf)"
expect "a VPN route around both ranges only warns" "WARN: preflight: the route 10.0.0.0/8 via tun0 covers k3s's 10.42.0.0/16" "$out"
expect "and the install goes on" "WENT ON" "$out"

out="$(PF_DOWN="github.com fill-data.papermc.io" run_pf)"
expect "unreachable download hosts are named together" "cannot reach github.com fill-data.papermc.io over HTTPS" "$out"
# The VM's rerun after an upgrade: one probe of fill-data.papermc.io timed out, a second a
# minute later answered in under a second, and preflight had refused the install.
rm -f "$pfroot"/tries.*
out="$(PF_FLAKY=fill-data.papermc.io run_pf)"
expect "a download host that drops one probe is tried again" "WENT ON" "$out"
expect "two seconds later" "SLEEP: 2" "$out"
[ "$(cat "$pfroot/tries.fill-data.papermc.io")" = xx ] && echo "PASS and not again once it answered" \
  || { echo "FAIL: a host that answered its second probe was probed $(wc -c < "$pfroot/tries.fill-data.papermc.io") times"; fails=$((fails + 1)); }
out="$(PF_STALL=fill-data.papermc.io run_pf)"
expect "a host that completes TLS and then sits on the request is reachable" "WENT ON" "$out"
rm -f "$pfroot"/tries.*
out="$(PF_DOWN=fill-data.papermc.io run_pf)"
expect "a host that never answers is refused" "cannot reach fill-data.papermc.io over HTTPS" "$out"
[ "$(cat "$pfroot/tries.fill-data.papermc.io")" = xxx ] && echo "PASS after three probes" \
  || { echo "FAIL: a host that never answered was probed $(wc -c < "$pfroot/tries.fill-data.papermc.io") times"; fails=$((fails + 1)); }

# An install from a release's assets builds nothing: no Docker cache under /var/lib/containerd,
# and Docker Hub only as the fallback for an image the release cannot supply. The downloaded
# bundles wait under /var/lib/felis until they are pushed, so that budget grows.
out="$(PF_CHANNEL=release PF_DF="/ 104857600 83886080 20971520" run_pf)"
expect "a release install fits where a source build does not" "OK: preflight passed" "$out"
out="$(PF_CHANNEL=release PF_DF="/ 104857600 88080384 16777216" run_pf)"
expect "a release install still needs room for its bundles" "/ has 16384 MiB free; this install writes about 17408 MiB there" "$out"
out="$(PF_ARTIFACT_DIR=/srv/felis-release PF_DF="/ 104857600 88080384 16777216" PF_DOWN="api.github.com registry-1.docker.io" run_pf)"
expect "an install from FELIS_ARTIFACT_DIR downloads no bundles and asks neither GitHub's API nor Docker Hub" "OK: preflight passed" "$out"
case "$out" in *"cannot reach"*) echo "FAIL: FELIS_ARTIFACT_DIR probed a host it never uses"; fails=$((fails + 1)) ;; *) echo "PASS FELIS_ARTIFACT_DIR probes only what it uses" ;; esac
out="$(PF_CHANNEL=release PF_DOWN=registry-1.docker.io run_pf)"
expect "a release install without Docker Hub is warned about" "WARN: preflight: cannot reach registry-1.docker.io over HTTPS; the install goes on" "$out"
expect "and goes on" "OK: preflight passed" "$out"
out="$(PF_DOWN=registry-1.docker.io run_pf)"
expect "a source build without Docker Hub is refused" "cannot reach registry-1.docker.io over HTTPS; the install downloads from there" "$out"
out="$(PF_CHANNEL=release PF_GAME_STACK=latest PF_DOWN=registry-1.docker.io run_pf)"
expect "FELIS_GAME_STACK=latest builds its images here, so it needs Docker Hub" "cannot reach registry-1.docker.io over HTTPS; the install downloads from there" "$out"
out="$(PF_CHANNEL=release PF_GAME_STACK=latest PF_DF="/ 104857600 83886080 20971520" run_pf)"
expect "and room for the builds" "/ has 20480 MiB free; this install writes about 25600 MiB there" "$out"
out="$(PF_CHANNEL=release PF_DOWN=api.github.com run_pf)"
expect "the release channel cannot install without GitHub's API" "cannot reach api.github.com over HTTPS; the install downloads from there" "$out"
out="$(PF_TUI=1 PF_DOWN=api.github.com run_pf)"
expect "the setup console only warns without it: its binary is already here" "WARN: preflight: cannot reach api.github.com" "$out"
expect "and goes on" "OK: preflight passed" "$out"

# k3s's install.sh installs k3s-selinux from Rancher's RPM repository on an SELinux host of
# the Red Hat or SUSE family (the CentOS Stream VM has its rancher-k3s-common.repo), and the
# k3s install fails when dnf cannot reach it.
out="$(PF_SELINUX_RPM=1 PF_DOWN=rpm.rancher.io run_pf)"
expect "k3s's SELinux package repository is required where its installer adds it" "cannot reach rpm.rancher.io over HTTPS" "$out"
out="$(PF_DOWN=rpm.rancher.io run_pf)"
expect "and not probed where it does not" "OK: preflight passed" "$out"
: > "$pfroot/k3s"; chmod +x "$pfroot/k3s"
out="$(PF_SELINUX_RPM=1 PF_DOWN=rpm.rancher.io run_pf)"
expect "nor once k3s is installed" "OK: preflight passed" "$out"
rm -f "$pfroot/k3s"
# The same test install.sh makes, against a root laid out as each family's host is.
selroot="$(mktemp -d)"
adds_rpm() { OS_ID_LIKE="$1" bash -c "$(awk '/^k3s_adds_selinux_rpm\(\) \{/,/^}/' "$BS")"'
  if k3s_adds_selinux_rpm "$0"; then echo yes; else echo no; fi' "$selroot"; }
mkdir -p "$selroot/etc"
: > "$selroot/etc/centos-release"
expect "a Red Hat-like host without an SELinux policy directory gets no k3s-selinux" no "$(adds_rpm "rhel fedora")"
mkdir -p "$selroot/usr/share/selinux"
rm "$selroot/etc/centos-release"
for f in redhat-release centos-release oracle-release fedora-release system-release; do
  : > "$selroot/etc/$f"
  expect "a host with /etc/$f and an SELinux policy directory gets k3s-selinux" yes "$(adds_rpm "")"
  rm "$selroot/etc/$f"
done
expect "so does one whose ID_LIKE names suse first (openSUSE Leap)" yes "$(adds_rpm "suse opensuse")"
expect "install.sh adds no repository where suse comes second (Tumbleweed)" no "$(adds_rpm "opensuse suse")"
expect "nor on Debian with SELinux policies installed" no "$(adds_rpm debian)"
rm -rf "$selroot"

# docs/operations.md lists the hosts an install from FELIS_ARTIFACT_DIR still downloads from,
# for a network that admits only those: every host preflight probes there must be in it.
ops="$(dirname "$BS")/../docs/operations.md"
artdoc="$(awk '/^`FELIS_ARTIFACT_DIR=<absolute path>` installs/,/^### /' "$ops" 2>/dev/null)"
hosts="$(PF_HOSTS=1 PF_ARTIFACT_DIR=/srv/felis-release PF_SELINUX_RPM=1 run_pf)"
expect "an install from FELIS_ARTIFACT_DIR probes GitHub" "github.com required" "$hosts"
for h in $(printf '%s\n' "$hosts" | awk '{ print $1 }'); do
  expect "operations.md names ${h} for an install from FELIS_ARTIFACT_DIR" "| \`${h}\`" "$artdoc"
done

# Three problems, one report, nothing done.
out="$(PF_MEM_KB=1000000 PF_ARCH=armv7l PF_DOWN=github.com run_pf)"
expect "every problem is in the one report" "preflight found 3 problem(s); nothing on this host has been changed" "$out"
expect "the architecture is one of them" "this host is armv7l" "$out"
case "$out" in *"WENT ON"*) echo "FAIL a failed preflight must stop the install"; fails=$((fails + 1)) ;; *) echo "PASS a failed preflight stops the install" ;; esac
out="$(PF_MEM_KB=1000000 PF_MODE=warn run_pf)"
expect "FELIS_PREFLIGHT=warn reports the problem" "WARN: preflight: this host has 976 MiB" "$out"
expect "and goes on" "WENT ON" "$out"
out="$(PF_CGROUPS="cpuset cpu io pids" run_pf)"
expect "a host without the memory controller is refused" "the memory cgroup controller is off" "$out"
rm -rf "$pfroot"

ciblock="$(awk '/^ipv4_to_int\(\) \{/,/^}/' "$BS"; awk '/^cidr_overlap\(\) \{/,/^}/' "$BS")"
overlap() { bash -c "$ciblock"'
  if cidr_overlap "$0" "$1"; then echo yes; else echo no; fi' "$1" "$2"; }
expect "a /24 inside the pod range overlaps" yes "$(overlap 10.42.5.0/24 10.42.0.0/16)"
expect "the next /16 does not" no "$(overlap 10.44.0.0/16 10.42.0.0/16)"
expect "a /8 around the service range overlaps" yes "$(overlap 10.0.0.0/8 10.43.0.0/16)"
expect "a bare address is a /32" no "$(overlap 10.41.255.255 10.42.0.0/16)"
expect "and is inside its own range" yes "$(overlap 10.42.0.1 10.42.0.0/16)"

pforder="$(awk '/^main\(\) \{/,/^}/' "$BS" | grep -nE '^[[:space:]]*(detect_node_ip|preflight|quiet_watchdog|pause_package_background_timers|ensure_swap|install_base)$' | sed 's/^[0-9]*:[[:space:]]*//' | tr '\n' ' ')"
expect "preflight runs once the node address is known and before the first change" \
  "detect_node_ip preflight quiet_watchdog pause_package_background_timers ensure_swap install_base " "$pforder"

# --- the control-plane database: felis-postgres in k3s -------------------------------------
bsfn() { awk '/^'"$1"'\(\) \{/,/^}/' "$BS"; }
for f in postgres_image_major check_postgres_major prepare_postgres_data_dir label_postgres_data_dir \
  ensure_postgres_superuser_secret ensure_postgres_role deploy_postgres host_postgres_holds_felis \
  write_pg_hba_lockout quiesce_host_postgres compare_pg_counts retire_host_postgres \
  migrate_host_postgres undo_postgres_move ensure_k3s_on_path; do
  [ -n "$(bsfn "$f")" ] || { echo "FAIL: no ${f} in $BS"; exit 1; }
  [ "$(bsfn "$f" | wc -l)" -lt 60 ] \
    || { echo "FAIL: the extracted ${f} is not just the function -- did its closing brace move?"; exit 1; }
done
# before <label> <first> <second> <text>: both lines are present, the first one earlier.
before() {
  _a="$(printf '%s\n' "$4" | grep -nF -- "$2" | head -n 1 | cut -d: -f1)"
  _b="$(printf '%s\n' "$4" | grep -nF -- "$3" | head -n 1 | cut -d: -f1)"
  if [ -n "$_a" ] && [ -n "$_b" ] && [ "$_a" -lt "$_b" ]; then
    echo "PASS $1"
  else
    echo "FAIL $1: expected <$2> before <$3> in:"; echo "$4"; fails=$((fails + 1))
  fi
}
lockline="$(grep -m1 '^PG_LOCKOUT_LINE=' "$BS" | cut -d'"' -f2)"
[ -n "$lockline" ] || { echo "FAIL: no PG_LOCKOUT_LINE in $BS"; exit 1; }

major() { POSTGRES_IMAGE="$1" bash -c "$(bsfn postgres_image_major)"'
postgres_image_major'; }
expect "the pinned image's major version is read off its tag" "18" "$(major "docker.io/library/postgres:18.6-trixie@sha256:5a5a")"
expect "a one-digit major is read whole" "9" "$(major "postgres:9.6-alpine@sha256:00")"
expect "a bare major tag is read" "17" "$(major "example.test:5000/pg:17@sha256:00")"

# A new major must stop at the old cluster: the image would initialise an empty one beside it.
run_major() { # data-dir
  PG_DATA_DIR="$1" POSTGRES_IMAGE="postgres:18.6-trixie@sha256:00" bash -c '
    set -Eeuo pipefail
    die() { printf "DIE: %s\n" "$*"; exit 1; }
    '"$(bsfn postgres_image_major)"'
    '"$(bsfn check_postgres_major)"'
    # A plain statement, as main calls it: on the left of && errexit is off inside the function.
    check_postgres_major
    echo STARTS' 2>&1
}
pmdir="$(mktemp -d)"
expect "an empty data directory starts" "STARTS" "$(run_major "$pmdir")"
mkdir -p "$pmdir/17/docker"; echo 17 > "$pmdir/17/docker/PG_VERSION"
out="$(run_major "$pmdir")"
expect "an older major's cluster stops the install" "DIE: this release runs PostgreSQL 18, but $pmdir holds a PostgreSQL 17 cluster" "$out"
expect "the refusal names the way forward" "docs/operations.md §4" "$out"
mkdir -p "$pmdir/18/docker"; echo 18 > "$pmdir/18/docker/PG_VERSION"
expect "the image's own major version starts beside an old one" "STARTS" "$(run_major "$pmdir")"
rm -rf "$pmdir"

out="$(PG_DATA_DIR=/var/lib/felis/postgres PG_UID=999 bash -c '
  set -Eeuo pipefail
  install() { printf "INSTALL %s\n" "$*"; }
  chown() { printf "CHOWN %s\n" "$*"; }
  chmod() { printf "CHMOD %s\n" "$*"; }
  label_postgres_data_dir() { echo LABEL; }
  '"$(bsfn prepare_postgres_data_dir)"'
  prepare_postgres_data_dir')"
expect "the data directory's parent is root-only" "INSTALL -d -m 0700 -o root -g root /var/lib/felis" "$out"
expect "the cluster directory belongs to the image's postgres account" "INSTALL -d -m 0700 -o 999 -g 999 /var/lib/felis/postgres" "$out"
expect "an existing cluster directory is handed back to that account" "CHOWN 999:999 /var/lib/felis/postgres" "$out"
expect "an existing cluster directory is closed to 0700 again" "CHMOD 0700 /var/lib/felis/postgres" "$out"
expect "the cluster directory is labelled for containers" "LABEL" "$out"

run_label() { # tools-present selinux-enabled(0/1) chcon-exit
  TOOLS="$1" SEL="$2" CHCON="${3:-0}" PG_DATA_DIR=/var/lib/felis/postgres bash -c '
    set -Eeuo pipefail
    warn() { printf "WARN: %s\n" "$*"; }
    command() { [ "$1" = -v ] || return 1; case " $TOOLS " in *" $2 "*) return 0 ;; esac; return 1; }
    selinuxenabled() { [ "$SEL" = 1 ]; }
    semanage() { printf "SEMANAGE %s\n" "$*"; }
    restorecon() { printf "RESTORECON %s\n" "$*"; }
    chcon() { printf "CHCON %s\n" "$*"; return "$CHCON"; }
    '"$(bsfn label_postgres_data_dir)"'
    label_postgres_data_dir' 2>&1
}
[ -z "$(run_label "" 1)" ] && echo "PASS a host without SELinux labels nothing" \
  || { echo "FAIL: a host without SELinux was relabelled"; fails=$((fails + 1)); }
[ -z "$(run_label "selinuxenabled semanage" 0)" ] && echo "PASS SELinux disabled labels nothing" \
  || { echo "FAIL: a host with SELinux disabled was relabelled"; fails=$((fails + 1)); }
out="$(run_label "selinuxenabled semanage" 1)"
expect "the label survives a relabel" "SEMANAGE fcontext -a -t container_file_t /var/lib/felis/postgres(/.*)?" "$out"
expect "the label is applied now" "RESTORECON -R /var/lib/felis/postgres" "$out"
case "$out" in *CHCON*) echo "FAIL: chcon ran although semanage labelled the directory"; fails=$((fails + 1)) ;; esac
expect "without semanage the directory is labelled directly" "CHCON -R -t container_file_t /var/lib/felis/postgres" "$(run_label "selinuxenabled" 1)"
expect "a label that cannot be set is a warning" "WARN: could not label /var/lib/felis/postgres" "$(run_label "selinuxenabled" 1 1)"

run_pgsecret() { # secret-exists(0/1)
  HAVE="$1" CONTROL_NS=felis PG_SECRET=felis-postgres PG_SECRET_KEY=superuser-password bash -c '
    set -Eeuo pipefail
    kube() { case "$*" in "-n felis get secret felis-postgres") [ "$HAVE" = 1 ] ;; *) printf "KUBE %s\n" "$*" ;; esac; }
    apply_literal_secret() { printf "APPLY %s %s %s len=%s\n" "$1" "$2" "$3" "${#4}"; }
    '"$(bsfn ensure_postgres_superuser_secret)"'
    ensure_postgres_superuser_secret'
}
expect "a missing superuser password is generated" "APPLY felis felis-postgres superuser-password len=64" "$(run_pgsecret 0)"
case "$(run_pgsecret 1)" in
  *APPLY*) echo "FAIL: an existing superuser password was replaced"; fails=$((fails + 1)) ;;
  *) echo "PASS an existing superuser password is kept" ;;
esac

rcalls="$(mktemp)"
run_role() { # password
  : > "$rcalls"
  CALLS="$rcalls" DB_USER=felis DB_NAME=felis DB_PASSWORD="$1" bash -c '
    set -Eeuo pipefail
    pg_sql() { printf "ARGV %s\n" "$*" >>"$CALLS"; cat >>"$CALLS"; }
    '"$(bsfn ensure_postgres_role)"'
    ensure_postgres_role'
  cat "$rcalls"
}
out="$(run_role s3cretpw)"
expect "the role is made from the maintenance database" "ARGV postgres" "$out"
case "$(printf '%s\n' "$out" | grep '^ARGV')" in
  *s3cretpw*) echo "FAIL: the role's password reached kubectl's argv"; fails=$((fails + 1)) ;;
  *) echo "PASS the role's password stays out of argv" ;;
esac
expect "the role's password is (re)set on every run" "ALTER ROLE \"felis\" WITH LOGIN PASSWORD 's3cretpw';" "$out"
expect "a quote in the password cannot end the literal" "PASSWORD 'it''s';" "$(run_role "it's")"
rm -f "$rcalls"

dpcalls="$(mktemp)"
run_deploy_pg() { # rollout-exit
  : > "$dpcalls"
  ROLL="$1" CALLS="$dpcalls" CONTROL_NS=felis MINECRAFT_NS=minecraft PG_DEPLOYMENT=felis-postgres PG_SERVICE_ADDR=felis-postgres.felis.svc:5432 \
    PG_HOST_PORT=15432 POSTGRES_IMAGE="postgres:18.6@sha256:00" HOST_BIN=fakefelis bash -c '
    set -Eeuo pipefail
    log() { :; }; ok() { printf "OK: %s\n" "$*"; }
    die() { printf "DIE: %s\n" "$*"; exit 1; }
    note() { printf "%s\n" "$*" >>"$CALLS"; }
    check_postgres_major() { note MAJOR; }
    prepare_postgres_data_dir() { note PREPARE; }
    ensure_postgres_superuser_secret() { note SECRET; }
    ensure_postgres_role() { note ROLE; }
    diagnose_rollout() { note "DIAGNOSE $*"; }
    fakefelis() { printf "RENDER %s\n" "$*"; }
    kube() {
      case "$*" in
        *"rollout status"*) note "ROLLOUT $*"; return "$ROLL" ;;
        "apply -f -") note "APPLY $(cat)" ;;
        *) printf "KUBE %s\n" "$*" ;;
      esac
    }
    '"$(bsfn deploy_postgres)"'
    deploy_postgres' 2>&1
  cat "$dpcalls"
}
out="$(run_deploy_pg 0)"
before "an old major is caught before anything is written" "MAJOR" "PREPARE" "$out"
before "the hostPath exists before the Deployment" "PREPARE" "APPLY RENDER manifests --only postgres" "$out"
before "the namespace exists before the Secret" "APPLY KUBE create namespace felis" "SECRET" "$out"
before "the superuser password exists before the first start" "SECRET" "APPLY RENDER manifests --only postgres" "$out"
expect "the database is rendered from the pinned image" \
  "APPLY RENDER manifests --only postgres --control-namespace felis --minecraft-namespace minecraft --postgres-image postgres:18.6@sha256:00" "$out"
before "the role waits for the database to be ready" "ROLLOUT -n felis rollout status deployment/felis-postgres" "ROLE" "$out"
out="$(run_deploy_pg 1)"
expect "a database that never gets ready is diagnosed" "DIAGNOSE deployment/felis-postgres" "$out"
expect "a database that never gets ready stops the install" "DIE: felis-postgres did not become ready" "$out"
case "$out" in *ROLE*) echo "FAIL: the role was made on a database that is not ready"; fails=$((fails + 1)) ;; esac
rm -f "$dpcalls"

run_holds() { # active tables psql-exit
  ACTIVE="$1" TABLES="$2" PSQL="${3:-0}" DB_NAME=felis bash -c '
    systemctl() { [ "$*" = "is-active --quiet postgresql" ] && [ "$ACTIVE" = 1 ]; }
    as_postgres() { case "$*" in *"-d felis "*) printf "%s\n" "$TABLES"; return "$PSQL" ;; *) echo 99 ;; esac; }
    '"$(bsfn host_postgres_holds_felis)"'
    if host_postgres_holds_felis; then echo HOLDS; else echo EMPTY; fi' 2>/dev/null
}
expect "a running host PostgreSQL with the platform's tables is moved" "HOLDS" "$(run_holds 1 12)"
expect "a stopped host PostgreSQL is left alone" "EMPTY" "$(run_holds 0 12)"
expect "a host PostgreSQL without the platform's tables is left alone" "EMPTY" "$(run_holds 1 0)"
expect "a host PostgreSQL that cannot be asked is left alone" "EMPTY" "$(run_holds 1 12 2)"

# The lockout heads pg_hba.conf, drops the rules earlier installers wrote, and keeps the rest.
lkdir="$(mktemp -d)"
cat > "$lkdir/pg_hba.conf" <<'HBA'
# BEGIN FELIS MANAGED HBA
# Felis rules must precede distro defaults such as 127.0.0.1 ident.
host felis felis 127.0.0.1/32 scram-sha-256
host felis felis 10.42.0.0/16 scram-sha-256
host felis felis 10.0.0.5/32 scram-sha-256
# END FELIS MANAGED HBA

local   all             all                                     peer
host    all             all             127.0.0.1/32            ident
host felis felis 10.42.0.0/16 scram-sha-256
HBA
run_lockout() {
  PG_LOCKOUT_LINE="$lockline" DB_NAME=felis DB_USER=felis HBA="$lkdir/pg_hba.conf" bash -c '
    set -Eeuo pipefail
    remember_temp() { :; }
    '"$(bsfn write_pg_hba_lockout)"'
    write_pg_hba_lockout "$HBA"'
}
run_lockout
want="# BEGIN FELIS MANAGED HBA
${lockline}
host felis felis 127.0.0.1/32 scram-sha-256
host felis all 0.0.0.0/0 reject
host felis all ::/0 reject
# END FELIS MANAGED HBA

local   all             all                                     peer
host    all             all             127.0.0.1/32            ident"
if [ "$(cat "$lkdir/pg_hba.conf")" = "$want" ]; then
  echo "PASS the lockout heads pg_hba.conf and removes every earlier felis rule"
else
  echo "FAIL: the locked-out pg_hba.conf is:"; cat "$lkdir/pg_hba.conf"; fails=$((fails + 1))
fi
run_lockout
[ "$(cat "$lkdir/pg_hba.conf")" = "$want" ] && echo "PASS a second lockout changes nothing" \
  || { echo "FAIL: a second lockout changed pg_hba.conf:"; cat "$lkdir/pg_hba.conf"; fails=$((fails + 1)); }
rm -rf "$lkdir"

qdir="$(mktemp -d)"
run_quiesce() { # clients-left
  : > "$qdir/calls"
  LEFT="$1" HBA="$qdir/pg_hba.conf" CALLS="$qdir/calls" CONTROL_NS=felis DB_NAME=felis PG_LOCKOUT_LINE="$lockline" bash -c '
    set -Eeuo pipefail
    '"$(grep '^PG_MOVE_TIMERS=' "$BS")"'
    PG_MOVE_UNITS=()
    die() { printf "DIE: %s\n" "$*"; exit 1; }
    sleep() { :; }
    as_postgres() {
      case "$*" in
        *"SHOW hba_file"*) echo "$HBA" ;;
        *pg_reload_conf*) echo RELOAD >>"$CALLS" ;;
        *pg_terminate_backend*) echo TERMINATE >>"$CALLS" ;;
        *"SELECT count(*) FROM pg_stat_activity"*) echo "$LEFT" ;;
      esac
    }
    kube() { printf "KUBE %s\n" "$*" >>"$CALLS"; }
    systemctl() {
      case "$*" in
        "is-active --quiet felis-db-backup.timer"|"is-active --quiet felis-watchdog.timer") return 0 ;;
        is-active*) return 1 ;;
        *) printf "SYSTEMCTL %s\n" "$*" >>"$CALLS" ;;
      esac
    }
    write_pg_hba_lockout() { echo LOCKOUT >>"$CALLS"; printf "%s\n" "$PG_LOCKOUT_LINE" > "$1"; }
    '"$(bsfn quiesce_host_postgres)"'
    quiesce_host_postgres
    echo "STAGE=$PG_MOVE_STAGE UNITS=${PG_MOVE_UNITS[*]}"' 2>&1
}
printf 'local all all peer\n' > "$qdir/pg_hba.conf"
out="$(run_quiesce 0)"
calls="$(cat "$qdir/calls")"
expect "felis-api stops writing" "KUBE -n felis scale deployment felis-api --replicas=0" "$calls"
expect "felis-operator stops writing" "KUBE -n felis scale deployment felis-operator --replicas=0" "$calls"
for unit in felis-db-backup felis-offsite felis-update-check felis-watchdog; do
  expect "the host's ${unit} stops writing" "SYSTEMCTL stop ${unit}.timer ${unit}.service" "$calls"
done
expect "only the timers that ran are remembered for a restart" "STAGE=quiesced UNITS=felis-db-backup.timer felis-watchdog.timer" "$out"
[ "$(cat "$qdir/pg_hba.conf.pre-pg-move")" = "local all all peer" ] && echo "PASS pg_hba.conf is kept for the undo" \
  || { echo "FAIL: pg_hba.conf.pre-pg-move is not the original"; fails=$((fails + 1)); }
before "the lockout is loaded" "LOCKOUT" "RELOAD" "$calls"
before "clients are cut off once the lockout is live" "RELOAD" "TERMINATE" "$calls"
before "the control plane is stopped before the lockout" "KUBE -n felis scale deployment felis-api --replicas=0" "LOCKOUT" "$calls"
out="$(run_quiesce 0)"
[ "$(cat "$qdir/pg_hba.conf.pre-pg-move")" = "local all all peer" ] && echo "PASS a lockout left behind never overwrites the saved pg_hba.conf" \
  || { echo "FAIL: the second quiesce saved the lockout over the original pg_hba.conf"; fails=$((fails + 1)); }
expect "a client that outlives the lockout stops the move" "DIE: the host database still has 2 client(s)" "$(run_quiesce 2)"
rm -rf "$qdir"

run_compare() { # host pod [pod-exit]
  H="$1" P="$2" PX="${3:-0}" PG_DEPLOYMENT=felis-postgres bash -c '
    set -Eeuo pipefail
    warn() { printf "WARN: %s\n" "$*"; }
    die() { printf "DIE: %s\n" "$*"; exit 1; }
    host_pg_counts() { printf "%s\n" "$H"; }
    pod_pg_counts() { printf "%s\n" "$P"; return "$PX"; }
    '"$(bsfn compare_pg_counts)"'
    compare_pg_counts
    echo SAME' 2>&1
}
counts="audit_logs 505${nl}servers 3${nl}users 17"
expect "the same rows in every table pass" "SAME" "$(run_compare "$counts" "$counts")"
out="$(run_compare "$counts" "audit_logs 505${nl}servers 2${nl}users 17")"
expect "a table short of rows stops the move" "DIE: the copy in felis-postgres does not match" "$out"
expect "the differing table is shown" "> servers 2" "$out"
expect "a host that reports no tables stops the move" "DIE: the host database reported no tables" "$(run_compare "" "")"
expect "a copy that cannot be counted stops the move" "DIE: could not count felis-postgres's rows" "$(run_compare "$counts" "$counts" 1)"

rtdir="$(mktemp -d)"
run_retire() { # other-databases
  : > "$rtdir/calls"
  OTHERS="$1" CALLS="$rtdir/calls" PG_FIREWALL_SERVICE="$rtdir/fw.service" PG_FIREWALL_RULES="$rtdir/fw.nft" bash -c '
    set -Eeuo pipefail
    warn() { printf "WARN: %s\n" "$*"; }
    host_postgres_other_databases() { [ -z "$OTHERS" ] || printf "%s\n" $OTHERS; }
    systemctl() { printf "SYSTEMCTL %s\n" "$*" >>"$CALLS"; }
    '"$(bsfn retire_host_postgres)"'
    retire_host_postgres' 2>&1
  cat "$rtdir/calls"
}
expect "the host PostgreSQL is stopped and kept off at boot" "SYSTEMCTL disable --now postgresql" "$(run_retire "")"
out="$(run_retire "gitea nextcloud")"
case "$out" in *"disable --now postgresql"*) echo "FAIL: a host PostgreSQL serving other databases was stopped"; fails=$((fails + 1)) ;; esac
expect "a host PostgreSQL serving other databases says which" "gitea nextcloud" "$out"
: > "$rtdir/fw.service"; : > "$rtdir/fw.nft"
expect "the old port firewall goes with the server" "SYSTEMCTL disable --now felis-postgres-firewall.service" "$(run_retire "")"
[ ! -e "$rtdir/fw.service" ] && [ ! -e "$rtdir/fw.nft" ] && echo "PASS the old port firewall's files are removed" \
  || { echo "FAIL: the old port firewall's files are still there"; fails=$((fails + 1)); }
rm -rf "$rtdir"

mgdir="$(mktemp -d)"
run_move() { # holds(0/1) backup(ok|silent|fail) restore-exit compare-exit
  : > "$mgdir/calls"
  HOLDS="$1" BACKUP="$2" RESTORE="${3:-0}" COMPARE="${4:-0}" MARKER="$mgdir/moved" CALLS="$mgdir/calls" MGDIR="$mgdir" \
    CONTROL_NS=felis PG_DEPLOYMENT=felis-postgres PG_HOST_PORT=15432 FELIS_DB_BACKUP_DIR="$mgdir/db" HOST_BIN=fakefelis bash -c '
    set -Eeuo pipefail
    PG_MOVED_MARKER="$MARKER"; PG_MOVE_STAGE=""
    log() { :; }; ok() { printf "OK: %s\n" "$*"; }
    die() { printf "DIE: %s\n" "$*"; exit 1; }
    remember_temp() { :; }
    note() { printf "%s\n" "$*" >>"$CALLS"; }
    host_postgres_holds_felis() { [ "$HOLDS" = 1 ]; }
    quiesce_host_postgres() { note QUIESCE; PG_MOVE_STAGE=quiesced; }
    write_felis_toml() { printf "%s %s\n" "$2" "${3:-no-deployment}" > "$1"; }
    compare_pg_counts() { note COMPARE; [ "$COMPARE" = 0 ] || die "counts differ"; }
    retire_host_postgres() { note RETIRE; }
    fakefelis() {
      note "FELIS $1 $2 [$(cat "$4")] ${*:5}"
      case "$2" in
        backup)
          case "$BACKUP" in
            ok) mkdir -p "$MGDIR/db"; : > "$MGDIR/db/b.tar"; echo "felis db backup: wrote $MGDIR/db/b.tar" ;;
            silent) : ;;
            fail) return 1 ;;
          esac ;;
        restore) return "$RESTORE" ;;
      esac
    }
    '"$(bsfn migrate_host_postgres)"'
    migrate_host_postgres
    echo "STAGE=$PG_MOVE_STAGE"' 2>&1
}
out="$(run_move 1 ok)"
calls="$(cat "$mgdir/calls")"
expect "the bundle is taken from the host server, never pruned" \
  "FELIS db backup [127.0.0.1:5432 no-deployment] -dir $mgdir/db -label pre-pg-move -keep 0 -no-servers" "$calls"
expect "the bundle is restored into the pod, which served nobody yet" \
  "FELIS db restore [127.0.0.1:15432 felis/felis-postgres] -dir $mgdir/db -yes -no-safety-backup $mgdir/db/b.tar" "$calls"
before "writers are stopped before the bundle" "QUIESCE" "FELIS db backup" "$calls"
before "the copy is counted after the restore" "FELIS db restore" "COMPARE" "$calls"
before "the host server is retired only after the counts agree" "COMPARE" "RETIRE" "$calls"
expect "the move is recorded with its bundle" "bundle $mgdir/db/b.tar" "$(cat "$mgdir/moved" 2>&1)"
expect "the move ends in the moved stage" "STAGE=moved" "$out"
out="$(run_move 1 ok)"
[ -z "$(cat "$mgdir/calls")" ] && echo "PASS a recorded move never runs again" \
  || { echo "FAIL: the move ran again after it was recorded:"; cat "$mgdir/calls"; fails=$((fails + 1)); }
rm -f "$mgdir/moved"
out="$(run_move 0 ok)"
[ -z "$(cat "$mgdir/calls")" ] && echo "PASS a host with nothing to move is left alone" \
  || { echo "FAIL: a host without the platform's database was moved"; fails=$((fails + 1)); }
for case_ in "silent 0 0" "fail 0 0" "ok 1 0" "ok 0 1"; do
  # shellcheck disable=SC2086
  set -- $case_
  rm -f "$mgdir/moved"
  out="$(run_move 1 "$1" "$2" "$3")"
  calls="$(cat "$mgdir/calls")"
  case "$out" in
    *DIE:*) ;;
    *) echo "FAIL: a failed move (backup=$1 restore=$2 compare=$3) did not stop the install"; fails=$((fails + 1)) ;;
  esac
  case "$calls" in
    *RETIRE*) echo "FAIL: a failed move (backup=$1 restore=$2 compare=$3) stopped the host server"; fails=$((fails + 1)) ;;
    *) echo "PASS a failed move (backup=$1 restore=$2 compare=$3) keeps the host server" ;;
  esac
  [ ! -e "$mgdir/moved" ] || { echo "FAIL: a failed move was recorded as done"; fails=$((fails + 1)); }
done
case "$(run_move 1 silent; cat "$mgdir/calls")" in
  *"db restore"*) echo "FAIL: a backup that named no bundle was restored anyway"; fails=$((fails + 1)) ;;
esac
rm -rf "$mgdir"

uddir="$(mktemp -d)"
run_undo() { # stage
  : > "$uddir/calls"
  printf '%s\n' "$lockline" > "$uddir/pg_hba.conf"
  printf 'local all all peer\n' > "$uddir/pg_hba.conf.pre-pg-move"
  STAGE="$1" HBA="$uddir/pg_hba.conf" CALLS="$uddir/calls" CONTROL_NS=felis PG_DEPLOYMENT=felis-postgres bash -c '
    set -Eeuo pipefail
    warn() { printf "WARN: %s\n" "$*"; }
    PG_MOVE_STAGE="$STAGE"; PG_MOVE_HBA="$HBA"; PG_MOVE_UNITS=(felis-db-backup.timer felis-watchdog.timer)
    as_postgres() { printf "PSQL %s\n" "$*" >>"$CALLS"; }
    kube() { printf "KUBE %s\n" "$*" >>"$CALLS"; }
    systemctl() { printf "SYSTEMCTL %s\n" "$*" >>"$CALLS"; }
    '"$(bsfn undo_postgres_move)"'
    undo_postgres_move' 2>&1
}
out="$(run_undo quiesced)"
calls="$(cat "$uddir/calls")"
[ "$(cat "$uddir/pg_hba.conf")" = "local all all peer" ] && echo "PASS an undone move puts pg_hba.conf back" \
  || { echo "FAIL: an undone move left the lockout in pg_hba.conf"; fails=$((fails + 1)); }
expect "an undone move reloads pg_hba.conf" "PSQL psql -XtA -c SELECT pg_reload_conf()" "$calls"
expect "an undone move brings felis-api back" "KUBE -n felis scale deployment felis-api --replicas=1" "$calls"
expect "an undone move brings felis-operator back" "KUBE -n felis scale deployment felis-operator --replicas=1" "$calls"
expect "an undone move restarts the timers it stopped" "SYSTEMCTL start felis-watchdog.timer" "$calls"
expect "an undone move says so" "WARN: the database move was undone" "$out"
out="$(run_undo moved)"
calls="$(cat "$uddir/calls")"
[ "$(cat "$uddir/pg_hba.conf")" = "$lockline" ] && echo "PASS a finished move keeps the host copy locked out" \
  || { echo "FAIL: a finished move reopened the host database"; fails=$((fails + 1)); }
case "$calls" in *"--replicas=1"*) echo "FAIL: a finished move pointed the control plane back at the host"; fails=$((fails + 1)) ;; esac
expect "a finished move restarts the timers it stopped" "SYSTEMCTL start felis-db-backup.timer" "$calls"
expect "a finished move says a rerun completes it" "rerun the installer" "$out"
out="$(run_undo "")"
[ -z "$out$(cat "$uddir/calls")" ] && echo "PASS a run that moved nothing undoes nothing" \
  || { echo "FAIL: undo acted without a move: $out"; fails=$((fails + 1)); }
rm -rf "$uddir"

run_cleanup() { # exit status
  bash -c '
    restore_previous_host_binary() { :; }
    undo_postgres_move() { echo UNDO; }
    WATCHDOG_QUIET_FILE=/nonexistent/felis-quiet
    TEMP_PATHS=(); DOCKER_CONTAINERS=(); PKG_TIMERS_TO_RESTORE=()
    '"$(bsfn cleanup)"'
    trap cleanup EXIT
    exit '"$1" 2>&1
}
expect "a failed install undoes the database move" "UNDO" "$(run_cleanup 1)"
case "$(run_cleanup 0)" in *UNDO*) echo "FAIL: a finished install undid the database move"; fails=$((fails + 1)) ;; *) echo "PASS a finished install keeps the database move" ;; esac

kp() { PATH_IN="$1" K3S_BIN_DIR=/usr/local/bin bash -c "$(bsfn ensure_k3s_on_path)"'
PATH="$PATH_IN"; ensure_k3s_on_path; printf "%s\n" "$PATH"'; }
expect "k3s joins a PATH sudo stripped of it" "/usr/local/bin:/usr/sbin:/usr/bin" "$(kp /usr/sbin:/usr/bin)"
[ "$(kp /usr/bin:/usr/local/bin)" = "/usr/bin:/usr/local/bin" ] && echo "PASS a PATH that has k3s is left alone" \
  || { echo "FAIL: ensure_k3s_on_path changed a PATH that had k3s: $(kp /usr/bin:/usr/local/bin)"; fails=$((fails + 1)); }

# The host server is gone once the database moved: no unit may still wait for it.
if grep -n 'postgresql\.service' "$BS"; then
  echo "FAIL: a unit bootstrap writes still orders itself after postgresql.service"; fails=$((fails + 1))
else
  echo "PASS no unit waits for the host PostgreSQL"
fi
pgorder="$(awk '/^main\(\) \{/,/^}/' "$BS" | grep -nE '^[[:space:]]*(ensure_k3s_on_path|install_k3s|deploy_postgres|migrate_host_postgres|run_migrations|deploy_bundle)$' | sed 's/^[0-9]*:[[:space:]]*//' | tr '\n' ' ')"
expect "the database runs in k3s, takes the host's data, then is migrated and joined by the bundle" \
  "ensure_k3s_on_path install_k3s deploy_postgres migrate_host_postgres run_migrations deploy_bundle " "$pgorder"
dbblock="$(bsfn deploy_bundle)"
before "the control plane is scaled back up after the bundle is applied" \
  'manifests "${manifest_args[@]}" | kube apply -f -' 'kube -n "$CONTROL_NS" scale deployment felis-api felis-operator --replicas=1' "$dbblock"
before "the control plane is scaled back up before the rollouts are awaited" \
  'kube -n "$CONTROL_NS" scale deployment felis-api felis-operator --replicas=1' 'rollout status "$d"' "$dbblock"
expect "the pods reach the database at its Service" 'write_felis_toml "${STATE_DIR}/felis.pod.toml" "$PG_SERVICE_ADDR"' "$dbblock"

# --- a release's prebuilt assets ----------------------------------------------------------
# Every file is used only once its sha256 is the one SHA256SUMS lists; the interesting cases
# are the refusals, and that a refused file is never handed on.
for f in load_artifact_sums artifact_sum artifact_fetch artifact_unusable install_artifact_binary \
  select_release_artifacts load_release_listing release_listing import_release_images \
  push_release_image registry_manifest_digest stage_k3s_airgap_images install_velocity_plugin \
  build_game_stack load_release_json; do
  [ -n "$(bsfn "$f")" ] || { echo "FAIL: no ${f} in $BS"; exit 1; }
  [ "$(bsfn "$f" | wc -l)" -lt 90 ] \
    || { echo "FAIL: the extracted ${f} is not just the function -- did its closing brace move?"; exit 1; }
done
adir="$(mktemp -d)"
sha() { sha256sum <"$1" | cut -d' ' -f1; }
afblock="$(bsfn artifact_sum; bsfn artifact_fetch)"
# run_fetch_artifact <mode> <name> [sums]: the directory is $adir/dir, the cache $adir/cache,
# and the "release" serves $adir/release/<name>.
run_fetch_artifact() {
  MODE="$1" NAME="$2" SUMS="${3:-$(cat "$adir/SUMS")}" A="$adir" bash -c '
    set -Eeuo pipefail
    warn() { echo "WARN: $*"; }
    download_release_asset() { echo "DOWNLOAD $2" >&2; [ -f "$A/release/$2" ] || return 1; cp "$A/release/$2" "$3"; }
    ARTIFACT_MODE="$MODE" ARTIFACT_TAG=v9.9.9 ARTIFACT_SUMS="$SUMS" ARTIFACT_CACHE="$A/cache"
    FELIS_ARTIFACT_DIR="$A/dir"
    '"$afblock"'
    if artifact_fetch "$NAME"; then echo "FILE $ARTIFACT_FILE"; cat "$ARTIFACT_FILE"; else echo REFUSED; fi' 2>&1
}
mkdir -p "$adir/dir" "$adir/release"
printf 'the game images\n' > "$adir/dir/felis-image-game-linux-amd64.tar"
cp "$adir/dir/felis-image-game-linux-amd64.tar" "$adir/release/"
printf '%s  felis-image-game-linux-amd64.tar\n%s *felis-velocity.jar\n' \
  "$(sha "$adir/dir/felis-image-game-linux-amd64.tar")" "$(printf 'the plugin\n' | sha256sum | cut -d' ' -f1)" > "$adir/SUMS"
out="$(run_fetch_artifact dir felis-image-game-linux-amd64.tar)"
expect "a directory's file that matches SHA256SUMS is used where it is" "FILE $adir/dir/felis-image-game-linux-amd64.tar" "$out"
out="$(run_fetch_artifact dir felis-velocity.jar)"
expect "a file the directory lacks is refused" "WARN: $adir/dir/felis-velocity.jar is missing" "$out"
case "$out" in *DOWNLOAD*) echo "FAIL: FELIS_ARTIFACT_DIR went to the release for a file it lacks"; fails=$((fails + 1)) ;; *) echo "PASS FELIS_ARTIFACT_DIR never downloads" ;; esac
printf 'not the plugin\n' > "$adir/dir/felis-velocity.jar"
out="$(run_fetch_artifact dir felis-velocity.jar)"
expect "a directory's file that does not match is refused, naming both hashes" "hashes to $(sha "$adir/dir/felis-velocity.jar"), but SHA256SUMS says" "$out"
case "$out" in *FILE*) echo "FAIL: a mismatched file was handed on"; fails=$((fails + 1)) ;; *) echo "PASS a mismatched file is never handed on" ;; esac
out="$(run_fetch_artifact dir felis-linux-amd64)"
expect "a name SHA256SUMS does not list is refused" "WARN: SHA256SUMS lists no felis-linux-amd64" "$out"
out="$(run_fetch_artifact release felis-linux-amd64)"
case "$out" in *DOWNLOAD*|*FILE*) echo "FAIL: a file SHA256SUMS does not list was fetched: $out"; fails=$((fails + 1)) ;; *) echo "PASS a file SHA256SUMS does not list is never downloaded" ;; esac
out="$(run_fetch_artifact dir felis-image-game-linux-amd64.tar "$(printf 'deadbeef  felis-image-game-linux-amd64.tar\n')")"
expect "a SHA256SUMS line without a whole sha256 lists nothing" "WARN: SHA256SUMS lists no felis-image-game-linux-amd64.tar" "$out"

out="$(run_fetch_artifact release felis-image-game-linux-amd64.tar)"
expect "a release file is downloaded into the cache once it matches" "FILE $adir/cache/felis-image-game-linux-amd64.tar" "$out"
[ ! -e "$adir/cache/felis-image-game-linux-amd64.tar.partial" ] && echo "PASS the download's partial file is gone" \
  || { echo "FAIL: the partial download was left behind"; fails=$((fails + 1)); }
[ "$(stat -c %a "$adir/cache" 2>/dev/null || stat -f %Lp "$adir/cache")" = 700 ] && echo "PASS the cache is root's alone" \
  || { echo "FAIL: the artifact cache is not 0700"; fails=$((fails + 1)); }
out="$(run_fetch_artifact release felis-image-game-linux-amd64.tar)"
case "$out" in *DOWNLOAD*) echo "FAIL: a cached file that still matches was downloaded again"; fails=$((fails + 1)) ;; *) echo "PASS a cached file that still matches is reused" ;; esac
printf 'tampered\n' > "$adir/cache/felis-image-game-linux-amd64.tar"
out="$(run_fetch_artifact release felis-image-game-linux-amd64.tar)"
expect "a cached file that no longer matches is fetched again" "DOWNLOAD felis-image-game-linux-amd64.tar" "$out"
expect "and the fresh copy is used" "the game images" "$out"
printf 'a swapped asset\n' > "$adir/release/felis-velocity.jar"
out="$(run_fetch_artifact release felis-velocity.jar)"
expect "a download that does not match is refused" "downloaded felis-velocity.jar hashes to $(sha "$adir/release/felis-velocity.jar"), but release v9.9.9's SHA256SUMS says" "$out"
[ ! -e "$adir/cache/felis-velocity.jar" ] && [ ! -e "$adir/cache/felis-velocity.jar.partial" ] \
  && echo "PASS a refused download leaves nothing in the cache" \
  || { echo "FAIL: a refused download was kept"; fails=$((fails + 1)); }
rm -f "$adir/release/felis-velocity.jar"
out="$(run_fetch_artifact release felis-velocity.jar)"
expect "a release without the asset is refused" "could not download felis-velocity.jar from release v9.9.9" "$out"
[ "$(printf '%s\n' "$out" | grep -c '^WARN')" = 1 ] && echo "PASS a failed download is reported once, as a failed download" \
  || { echo "FAIL: a failed download went on to be checked: $out"; fails=$((fails + 1)); }

# From FELIS_ARTIFACT_DIR nothing is ever built, so an unusable asset stops the install; from
# a release it falls back, with a warning.
unblock="$(bsfn artifact_unusable)"
out="$(ARTIFACT_MODE=dir bash -c 'die() { echo "DIE: $*"; exit 1; }; warn() { echo "WARN: $*"; }; '"$unblock"'; artifact_unusable "no plugin" "building it"; echo GOES ON')"
expect "FELIS_ARTIFACT_DIR stops on an unusable asset" "DIE: FELIS_ARTIFACT_DIR: no plugin" "$out"
case "$out" in *"GOES ON"*) echo "FAIL: FELIS_ARTIFACT_DIR went on without an asset"; fails=$((fails + 1)) ;; esac
out="$(ARTIFACT_MODE=release bash -c 'die() { echo "DIE: $*"; exit 1; }; warn() { echo "WARN: $*"; }; '"$unblock"'; artifact_unusable "no plugin" "building it"; echo GOES ON')"
expect "a release's unusable asset falls back" "WARN: no plugin; building it" "$out"
expect "and the install goes on" "GOES ON" "$out"

# --- the image listing ----------------------------------------------------------------------
llblock="$(bsfn load_release_listing)"
d1="sha256:$(printf 'm' | sha256sum | cut -d' ' -f1)"
d2="sha256:$(printf 'c' | sha256sum | cut -d' ' -f1)"
good="felis-image-game-linux-amd64.tar limbo registry.felis.svc:5000/felis/limbo:demo $d1 $d2
felis-image-base-linux-amd64.tar registry docker.io/library/registry@$d1 $d1 $d2"
run_listing() { # content
  printf '%s\n' "$1" > "$adir/listing"
  bash -c "$llblock"'
    if load_release_listing "$0" amd64; then printf "%s" "$RELEASE_LISTING"; echo LOADED; else echo REFUSED; fi' "$adir/listing"
}
expect "a well-formed listing is loaded" "LOADED" "$(run_listing "$good")"
expect "a listing for another architecture is refused" "REFUSED" "$(run_listing "$(printf '%s\n' "$good" | sed 's/amd64/arm64/')")"
expect "a line with a sixth field is refused" "REFUSED" "$(run_listing "$good extra")"
expect "a line missing its config digest is refused" "REFUSED" "$(run_listing "felis-image-game-linux-amd64.tar limbo registry.felis.svc:5000/felis/limbo:demo $d1")"
expect "a role Felis does not know is refused" "REFUSED" "$(run_listing "felis-image-game-linux-amd64.tar miner x/y:z $d1 $d2")"
expect "a role listed twice is refused" "REFUSED" "$(run_listing "$good
felis-image-game-linux-amd64.tar limbo other/limbo:demo $d1 $d2")"
expect "a short digest is refused" "REFUSED" "$(run_listing "felis-image-game-linux-amd64.tar limbo x/limbo:demo sha256:abc $d2")"
expect "a name with shell characters is refused" "REFUSED" "$(run_listing "felis-image-game-linux-amd64.tar limbo x/limbo:\$(id) $d1 $d2")"
expect "a bundle outside the felis-image-* names is refused" "REFUSED" "$(run_listing "../../etc/shadow limbo x/limbo:demo $d1 $d2")"
expect "an empty listing is refused" "REFUSED" "$(run_listing "")"

# --- importing a release's images into containerd --------------------------------------------
# ctr is a fake containerd: `images ls` prints the refs in $adir/ctr (with the header row the
# real one has), `images import` adds what the imported bundle's .names file holds, and
# `images tag` adds a name.
ilblock="$(bsfn artifact_unusable; bsfn role_image; bsfn role_fallback; bsfn role_prebuilt; bsfn image_repo; bsfn image_present; bsfn pinned_image_ref; bsfn import_release_images)"
fdig="sha256:$(printf 'felis' | sha256sum | cut -d' ' -f1)"
gdig="sha256:$(printf 'limbo' | sha256sum | cut -d' ' -f1)"
rdig="sha256:$(printf 'registry' | sha256sum | cut -d' ' -f1)"
cdig="sha256:0123456789ab$(printf 'cfg' | sha256sum | cut -c13-64)"
ridx="sha256:$(printf 'index' | sha256sum | cut -d' ' -f1)"
listing="felis-image-felis-linux-amd64.tar felis registry.felis.svc:5000/felis/felis:v9.9.9 $fdig $cdig
felis-image-game-linux-amd64.tar limbo registry.felis.svc:5000/felis/limbo:demo $gdig $cdig
felis-image-base-linux-amd64.tar registry docker.io/library/registry@$ridx $rdig $cdig"
run_import_release() { # mode ctr-refs FELIS_IMAGE-or-empty roles...
  ir_mode="$1" ir_refs="$2" ir_fimg="${3:-registry.felis.svc:5000/felis/felis:v9.9.9}"
  shift 3
  MODE="$ir_mode" REFS="$ir_refs" FIMG="$ir_fimg" A="$adir" LISTING="$listing" \
    FD="$fdig" GD="$gdig" RD="$rdig" RIDX="$ridx" bash -c '
    set -Eeuo pipefail
    die() { echo "DIE: $*"; exit 1; }
    warn() { echo "WARN: $*"; }
    log() { :; }
    ok() { echo "OK: $*"; }
    printf "%s\n" "$REFS" > "$A/ctr"
    k3s_cmd() {
      shift
      case "$2" in
        ls) echo "REF TYPE DIGEST SIZE PLATFORMS LABELS"; awk "NF { print \$1, \"application/vnd.oci.image.manifest.v1+json\", \$2, \"25.3 MiB\", \"linux/amd64\", \"-\" }" "$A/ctr" ;;
        import) echo "IMPORT ${3##*/}" >&2; cat "$3.names" >> "$A/ctr" ;;
        tag) echo "TAG $4 $5" >&2; awk -v s="$4" -v t="$5" "\$1 == s { print t, \$2 }" "$A/ctr" >> "$A/ctr" ;;
      esac
    }
    artifact_fetch() { echo "FETCH $1" >&2; ARTIFACT_FILE="$A/bundles/$1"; [ -f "$ARTIFACT_FILE" ]; }
    release_listing() { RELEASE_LISTING="$LISTING"; }
    ARTIFACT_MODE="$MODE" PREBUILT_ROLES=" " RELEASE_IMAGES="" LIMBO_IMAGE_ID="" LOBBY_IMAGE_ID=""
    FELIS_IMAGE="$FIMG" FELIS_LIMBO_IMAGE=registry.felis.svc:5000/felis/limbo:demo
    FELIS_LOBBY_IMAGE=registry.felis.svc:5000/felis/lobby:demo FELIS_PAPER_IMAGE=registry.felis.svc:5000/felis/paper:demo
    REGISTRY_IMAGE="docker.io/library/registry:2.8.3@$RIDX" POSTGRES_IMAGE="docker.io/library/postgres:18.6-trixie@$RIDX"
    '"$ilblock"'
    import_release_images "$@"
    echo "PREBUILT[$PREBUILT_ROLES] LIMBO_ID[$LIMBO_IMAGE_ID]"
    printf "%s" "$RELEASE_IMAGES"' x "$@" 2>&1
}
mkdir -p "$adir/bundles"
: > "$adir/bundles/felis-image-felis-linux-amd64.tar"
printf '%s %s\n' "registry.felis.svc:5000/felis/felis:v9.9.9" "$fdig" "registry.felis.svc:5000/felis/felis@$fdig" "$fdig" \
  > "$adir/bundles/felis-image-felis-linux-amd64.tar.names"
: > "$adir/bundles/felis-image-game-linux-amd64.tar"
printf '%s %s\n' "registry.felis.svc:5000/felis/limbo:demo" "$gdig" > "$adir/bundles/felis-image-game-linux-amd64.tar.names"
: > "$adir/bundles/felis-image-base-linux-amd64.tar"
printf '%s %s\n' "docker.io/library/registry@$ridx" "$rdig" > "$adir/bundles/felis-image-base-linux-amd64.tar.names"

out="$(run_import_release release "" "" felis registry)"
expect "a fresh node imports the bundles holding the images it lacks" "IMPORT felis-image-felis-linux-amd64.tar" "$out"
expect "the base bundle too" "IMPORT felis-image-base-linux-amd64.tar" "$out"
case "$out" in *"felis-image-game"*) echo "FAIL: a bundle no asked-for image is in was fetched"; fails=$((fails + 1)) ;; *) echo "PASS only the bundles holding the asked-for images are fetched" ;; esac
expect "each imported image is recorded as the release's" "PREBUILT[ felis registry ]" "$out"
expect "with the line push_release_image reads" "felis felis-image-felis-linux-amd64.tar registry.felis.svc:5000/felis/felis:v9.9.9 registry.felis.svc:5000/felis/felis:v9.9.9 $fdig $cdig" "$out"

out="$(run_import_release release "registry.felis.svc:5000/felis/felis:v9.9.9 $fdig
docker.io/library/registry@$ridx $ridx" "" felis registry)"
case "$out" in *FETCH*|*IMPORT*) echo "FAIL: images containerd already holds were fetched again"; fails=$((fails + 1)) ;; *) echo "PASS a rerun fetches nothing containerd already holds" ;; esac
expect "a digest name CRI pulled counts as the image" "PREBUILT[ felis registry ]" "$out"

out="$(run_import_release release "registry.felis.svc:5000/felis/felis:v9.9.9 sha256:$(printf old | sha256sum | cut -d' ' -f1)" "" felis)"
expect "a tag naming another build is imported over" "IMPORT felis-image-felis-linux-amd64.tar" "$out"

out="$(run_import_release release "" "registry.felis.svc:5000/felis/felis:custom" felis)"
expect "a FELIS_IMAGE set to another name gets that name on the release's image" "TAG registry.felis.svc:5000/felis/felis:v9.9.9 registry.felis.svc:5000/felis/felis:custom" "$out"
expect "and its digest name, for a pinned pull" "TAG registry.felis.svc:5000/felis/felis:v9.9.9 registry.felis.svc:5000/felis/felis@$fdig" "$out"
expect "and counts as the release's" "PREBUILT[ felis ]" "$out"

out="$(run_import_release release "" "" limbo)"
expect "a game image's config digest is the build the login server runs" "LIMBO_ID[$cdig]" "$out"

out="$(run_import_release release "" "" paper)"
expect "a role the release does not list falls back alone" "WARN: the release lists no paper image; building it on this host instead" "$out"
expect "and is not recorded as the release's" "PREBUILT[ ]" "$out"
[ "$(printf '%s\n' "$out" | grep -c '^WARN')" = 1 ] && echo "PASS a role the release does not list is reported once" \
  || { echo "FAIL: a role the release does not list went on to be looked for in containerd: $out"; fails=$((fails + 1)); }
out="$(run_import_release dir "" "" paper)"
expect "from FELIS_ARTIFACT_DIR a missing role stops the install" "DIE: FELIS_ARTIFACT_DIR: the release lists no paper image" "$out"

out="$(run_import_release release "" "" postgres)"
expect "a base image the release does not list falls back to Docker Hub" "WARN: the release lists no postgres image; k3s pulls it from Docker Hub instead" "$out"
listing="$(printf '%s\n' "$listing" | sed "s|docker.io/library/registry@$ridx|docker.io/library/registry@sha256:$(printf other | sha256sum | cut -d' ' -f1)|")"
out="$(run_import_release release "" "" registry)"
expect "the release's registry must be the one this installer pins" "the release's registry image is docker.io/library/registry@sha256:$(printf other | sha256sum | cut -d' ' -f1), but this installer runs docker.io/library/registry@$ridx" "$out"
expect "and falls back to Docker Hub" "k3s pulls it from Docker Hub instead" "$out"
case "$out" in *IMPORT*) echo "FAIL: a refused base image was imported"; fails=$((fails + 1)) ;; *) echo "PASS a refused base image is not imported" ;; esac

rm -f "$adir/bundles/felis-image-game-linux-amd64.tar.names"
: > "$adir/bundles/felis-image-game-linux-amd64.tar.names"
out="$(run_import_release release "" "" limbo)"
expect "a bundle that does not hold the image it is listed for falls back" "WARN: k3s containerd holds no registry.felis.svc:5000/felis/limbo:demo from felis-image-game-linux-amd64.tar; building it on this host instead" "$out"
expect "and records nothing" "PREBUILT[ ]" "$out"

out="$(run_import_release "" "" "" felis limbo)"
case "$out" in *FETCH*|*IMPORT*|*WARN*) echo "FAIL: a source build looked at release bundles"; fails=$((fails + 1)) ;; *) echo "PASS a source build imports nothing from a release" ;; esac

# The listing is read once per run, and a bad one is reported once.
rlblock="$(bsfn release_listing)"
run_rl() { # listing-content
  printf '%s\n' "$1" > "$adir/rl.txt"
  A="$adir" bash -c '
    warn() { echo "WARN: $*"; }
    die() { echo "DIE: $*"; exit 1; }
    felis_asset_arch() { echo amd64; }
    artifact_fetch() { echo "FETCH $1"; ARTIFACT_FILE="$A/rl.txt"; }
    '"$unblock"'
    '"$llblock"'
    '"$rlblock"'
    ARTIFACT_MODE=release RELEASE_LISTING="" RELEASE_LISTING_STATE=""
    release_listing && echo FIRST-OK
    release_listing && echo SECOND-OK
    true'
}
out="$(run_rl "$good")"
[ "$(printf '%s\n' "$out" | grep -c FETCH)" = 1 ] && expect "a good listing serves every lookup" "FIRST-OK
SECOND-OK" "$out" \
  || { echo "FAIL: the listing was fetched more than once: $out"; fails=$((fails + 1)); }
out="$(run_rl "junk")"
[ "$(printf '%s\n' "$out" | grep -c 'WARN:')" = 1 ] && [ "$(printf '%s\n' "$out" | grep -c FETCH)" = 1 ] \
  && expect "a malformed listing is reported once and trusted nowhere" "is malformed; building its images on this host instead" "$out" \
  || { echo "FAIL: a malformed listing was reported or fetched more than once: $out"; fails=$((fails + 1)); }
case "$out" in *-OK*) echo "FAIL: a malformed listing was used"; fails=$((fails + 1)) ;; esac

# --- pushing a release image into the platform registry ----------------------------------------
prblock="$(bsfn push_release_image)"
run_push_release() { # role registry-digest-for-tag registry-digest-for-version-tag [MC_VERSION]
  REG_TAG="$2" REG_VER="$3" MCV="${4-26.2}" LINE="$1 felis-image-game-linux-amd64.tar registry.felis.svc:5000/felis/$1:demo registry.felis.svc:5000/felis/$1:demo $gdig $cdig" bash -c '
    die() { echo "DIE: $*"; exit 1; }
    warn() { echo "WARN: $*"; }
    log() { :; }
    ok() { echo "OK: $*"; }
    registry_manifest_digest() { case "$1" in *:demo) echo "$REG_TAG" ;; *) echo "$REG_VER" ;; esac; }
    artifact_fetch() { ARTIFACT_FILE="/cache/$1"; }
    felis() { echo "PUSH-IMAGE user=$FELIS_REGISTRY_USERNAME pass=$FELIS_REGISTRY_PASSWORD $*" >&2; echo "$gdig"; }
    HOST_BIN=felis REGISTRY_URL=registry.felis.svc:5000 REGISTRY_PUSH_HOST=127.0.0.1:5000
    REGISTRY_PLATFORM_TOKEN=tok MC_VERSION="$MCV" RELEASE_IMAGES="$LINE"
    '"$prblock"'
    push_release_image "${LINE%% *}"' 2>&1
}
out="$(run_push_release limbo "" "")"
expect "a release image is pushed from its bundle by name, as the platform principal" \
  "PUSH-IMAGE user=platform pass=tok push-image --tar /cache/felis-image-game-linux-amd64.tar --image registry.felis.svc:5000/felis/limbo:demo --ref 127.0.0.1:5000/felis/limbo:demo" "$out"
expect "and under its Minecraft version and config digest" "--ref 127.0.0.1:5000/felis/limbo:26.2-0123456789ab" "$out"
out="$(run_push_release limbo "$gdig" "")"
case "$out" in *"--ref 127.0.0.1:5000/felis/limbo:demo"*) echo "FAIL: a tag already naming the image was pushed again"; fails=$((fails + 1)) ;; *) echo "PASS a tag already naming the image is left as it is" ;; esac
expect "while a missing version tag is still pushed" "--ref 127.0.0.1:5000/felis/limbo:26.2-0123456789ab" "$out"
out="$(run_push_release limbo "sha256:$(printf other | sha256sum | cut -d' ' -f1)" "$gdig")"
expect "a tag naming another build is pushed over" "--ref 127.0.0.1:5000/felis/limbo:demo" "$out"
out="$(run_push_release felis "" "")"
case "$out" in *"26.2-"*) echo "FAIL: the control-plane image got a Minecraft version tag"; fails=$((fails + 1)) ;; *) echo "PASS only game images get a version tag" ;; esac
out="$(run_push_release limbo "" "" "")"
case "$out" in *"-0123456789ab"*) echo "FAIL: a version tag was made without a Minecraft version"; fails=$((fails + 1)) ;; *) echo "PASS no Minecraft version, no version tag" ;; esac

rmblock="$(bsfn registry_manifest_digest)"
curlargs="$(mktemp)"
out="$(CA="$curlargs" bash -c 'curl() { printf "%s\n" "$*" > "$CA"; printf "HTTP/1.1 200 OK\r\nContent-Type: x\r\nDocker-Content-Digest: sha256:abc\r\n\r\n"; }
'"$rmblock"'
registry_manifest_digest 127.0.0.1:5000/felis/limbo:26.2-0123')"
expect "the registry's digest for a tag is read off a HEAD" "-fsSI --max-time 30" "$(cat "$curlargs")"
expect "from the tag's manifest URL" "http://127.0.0.1:5000/v2/felis/limbo/manifests/26.2-0123" "$(cat "$curlargs")"
expect "asking for every manifest kind a registry may hold" "application/vnd.docker.distribution.manifest.list.v2+json" "$(cat "$curlargs")"
rm -f "$curlargs"
[ "$out" = "sha256:abc" ] && echo "PASS the digest comes back without its carriage return" \
  || { echo "FAIL: registry_manifest_digest printed: $out"; fails=$((fails + 1)); }
[ -z "$(bash -c 'curl() { return 22; }; '"$rmblock"'; registry_manifest_digest 127.0.0.1:5000/felis/x:demo')" ] \
  && echo "PASS a tag the registry lacks has no digest" || { echo "FAIL: a missing tag printed a digest"; fails=$((fails + 1)); }

# --- the binary from FELIS_ARTIFACT_DIR -----------------------------------------------------
iabblock="$(bsfn load_artifact_sums; bsfn artifact_sum; bsfn install_artifact_binary)"
mkdir -p "$adir/abin/dir" "$adir/abin/bin"
run_artifact_binary() { # binary-script
  printf '%s\n' "$1" > "$adir/abin/dir/felis-linux-amd64"
  printf '%s  felis-linux-amd64\n' "$(sha "$adir/abin/dir/felis-linux-amd64")" > "$adir/abin/dir/SHA256SUMS"
  [ -z "${TAMPER:-}" ] || printf 'x\n' >> "$adir/abin/dir/felis-linux-amd64"
  A="$adir/abin" bash -c '
    set -Eeuo pipefail
    die() { echo "DIE: $*"; exit 1; }
    ok() { echo "OK: $*"; }
    remember_temp() { :; }
    keep_previous_host_binary() { echo KEEP; }
    felis_asset_arch() { echo amd64; }
    FELIS_ARTIFACT_DIR="$A/dir" HOST_BIN="$A/bin/felis" ARTIFACT_MODE="" ARTIFACT_SUMS="" HAVE_PREBUILT_BINARY="" FELIS_VERSION=""
    '"$iabblock"'
    install_artifact_binary
    echo "VERSION[$FELIS_VERSION] PREBUILT[$HAVE_PREBUILT_BINARY] MODE[$ARTIFACT_MODE]"' 2>&1
}
out="$(run_artifact_binary '#!/bin/sh
echo "felis v9.9.9"')"
expect "the directory's binary is installed and names the version" "VERSION[v9.9.9] PREBUILT[1] MODE[dir]" "$out"
[ -x "$adir/abin/bin/felis" ] && echo "PASS the binary lands at HOST_BIN" || { echo "FAIL: no binary at HOST_BIN"; fails=$((fails + 1)); }
out="$(run_artifact_binary '#!/bin/sh
echo "felis v9.9.9"')"
case "$out" in *KEEP*) echo "FAIL: the same binary was replaced (and its previous copy overwritten)"; fails=$((fails + 1)) ;; *) echo "PASS the same binary is left in place" ;; esac
out="$(TAMPER=1 run_artifact_binary '#!/bin/sh
echo "felis v9.9.9"')"
expect "a binary that does not match SHA256SUMS stops the install" "DIE: FELIS_ARTIFACT_DIR: felis-linux-amd64 does not match SHA256SUMS" "$out"
out="$(run_artifact_binary '#!/bin/sh
exit 1')"
expect "a binary that cannot say its version stops the install" "DIE: FELIS_ARTIFACT_DIR: felis-linux-amd64 reports 'nothing' as its version" "$out"
rm -f "$adir/abin/dir/SHA256SUMS"
out="$(A="$adir/abin" bash -c 'die() { echo "DIE: $*"; exit 1; }; remember_temp() { :; }; felis_asset_arch() { echo amd64; }
FELIS_ARTIFACT_DIR="$A/dir" ARTIFACT_MODE="" ARTIFACT_SUMS=""
'"$iabblock"'
install_artifact_binary' 2>&1)"
expect "a directory without SHA256SUMS stops the install" "DIE: FELIS_ARTIFACT_DIR: $adir/abin/dir holds no SHA256SUMS" "$out"

# --- which release the images come from -------------------------------------------------------
srblock="$(bsfn select_release_artifacts)"
run_select() { # binary-version sums-available(0|1) [ARTIFACT_MODE] [HAVE_PREBUILT_BINARY]
  printf '#!/bin/sh\necho "felis %s"\n' "$1" > "$adir/selbin"; chmod +x "$adir/selbin"
  SUMS_OK="$2" MODE="${3:-}" PRE="${4-1}" B="$adir/selbin" bash -c '
    ok() { echo "OK: $*"; }
    warn() { echo "WARN: $*"; }
    load_artifact_sums() { echo "SUMS $ARTIFACT_TAG"; [ "$SUMS_OK" = 1 ]; }
    ARTIFACT_MODE="$MODE" ARTIFACT_TAG="" HAVE_PREBUILT_BINARY="$PRE" HOST_BIN="$B"
    '"$srblock"'
    select_release_artifacts
    echo "MODE[$ARTIFACT_MODE] TAG[$ARTIFACT_TAG]"'
}
expect "a release binary takes its images from that release" "MODE[release] TAG[v9.9.9]" "$(run_select v9.9.9 1)"
expect "a prerelease tag too" "MODE[release] TAG[v9.9.9-rc.1]" "$(run_select v9.9.9-rc.1 1)"
out="$(run_select v9.9.9 0)"
expect "a release without SHA256SUMS builds its images here" "MODE[] TAG[]" "$out"
expect "and says what that costs" "this installs Docker and needs about 8 GiB more" "$out"
out="$(run_select v0.0.0+gabc1234 1)"
expect "a dev build names no release" "MODE[] TAG[]" "$out"
case "$out" in *SUMS*) echo "FAIL: a dev build looked up a release"; fails=$((fails + 1)) ;; *) echo "PASS a dev build looks up no release" ;; esac
case "$(run_select dev 1)" in *SUMS*) echo "FAIL: an unstamped build looked up a release"; fails=$((fails + 1)) ;; *) echo "PASS an unstamped build looks up no release" ;; esac
expect "a source build takes nothing from a release" "MODE[] TAG[]" "$(run_select v9.9.9 1 "" "")"
expect "FELIS_ARTIFACT_DIR stays the source" "MODE[dir] TAG[]" "$(run_select v9.9.9 1 dir)"

# --- the game stack and the Velocity plugin --------------------------------------------------
gsblock="$(bsfn build_game_stack; bsfn role_prebuilt; bsfn stop_docker)"
run_game_stack() { # PREBUILT_ROLES-after-import FELIS_GAME_STACK [ARTIFACT_MODE]
  AFTER="$1" STACK="$2" MODE="${3-release}" bash -c '
    ok() { :; }
    game_stack_source() { :; }
    resolve_game_jars() { :; }
    import_release_images() { echo "IMPORT $*"; PREBUILT_ROLES="$AFTER"; }
    ctr() { return 1; }
    ensure_docker() { echo ENSURE; DOCKER_INSTALLED=1; }
    build_game_image() { echo "BUILD $1"; }
    install_velocity_plugin() { echo PLUGIN; }
    systemctl() { echo "SYSTEMCTL $*"; }
    PREBUILT_ROLES=" " DOCKER_INSTALLED="" FELIS_GAME_STACK="$STACK" ARTIFACT_MODE="$MODE"
    '"$gsblock"'
    build_game_stack'
}
out="$(run_game_stack " limbo lobby paper " pinned)"
expect "the pinned stack comes from the release" "IMPORT limbo lobby paper" "$out"
case "$out" in *BUILD*|*ENSURE*|*SYSTEMCTL*) echo "FAIL: a release game stack built or touched docker: $out"; fails=$((fails + 1)) ;; *) echo "PASS a release game stack needs no docker" ;; esac
out="$(run_game_stack " limbo " pinned)"
expect "only what the release did not supply is built" "ENSURE
BUILD lobby
BUILD paper
PLUGIN
SYSTEMCTL stop docker docker.socket" "$out"
out="$(run_game_stack " limbo lobby paper " latest)"
case "$out" in *IMPORT*) echo "FAIL: FELIS_GAME_STACK=latest took the release's pinned images"; fails=$((fails + 1)) ;; *) echo "PASS FELIS_GAME_STACK=latest takes nothing from the release" ;; esac
expect "and builds all three" "BUILD limbo
BUILD lobby
BUILD paper" "$out"

vpblock="$(bsfn install_velocity_plugin; bsfn artifact_unusable)"
run_plugin() { # ARTIFACT_MODE fetch-ok(0|1)
  MODE="$1" FOK="$2" bash -c '
    die() { echo "DIE: $*"; exit 1; }
    warn() { echo "WARN: $*"; }
    ok() { echo "OK: $*"; }
    artifact_fetch() { ARTIFACT_FILE=/cache/felis-velocity.jar; [ "$FOK" = 1 ]; }
    prepare_velocity_layout() { echo LAYOUT; }
    install_if_changed() { echo "INSTALL $*"; }
    ensure_docker() { echo ENSURE; }
    build_velocity_plugin() { echo BUILD; }
    ARTIFACT_MODE="$MODE" VELOCITY_DIR=/opt/felis/velocity
    '"$vpblock"'
    install_velocity_plugin'
}
out="$(run_plugin release 1)"
expect "the release's plugin is put in place" "LAYOUT
INSTALL /cache/felis-velocity.jar /opt/felis/velocity/plugins/felis-velocity.jar 0644 root root" "$out"
case "$out" in *BUILD*|*ENSURE*) echo "FAIL: a release plugin was built too"; fails=$((fails + 1)) ;; *) echo "PASS a release plugin is not built" ;; esac
out="$(run_plugin release 0)"
expect "an unusable release plugin is built here" "WARN: the release's felis-velocity.jar cannot be used; building it on this host instead
ENSURE
BUILD" "$out"
expect "from FELIS_ARTIFACT_DIR it stops the install" "DIE: FELIS_ARTIFACT_DIR: the release's felis-velocity.jar cannot be used" "$(run_plugin dir 0)"
expect "a source build builds the plugin" "ENSURE
BUILD" "$(run_plugin "" 0)"

# --- install_if_changed never fixes a target in place -----------------------------------------
# The proxy's account owns the directories these files land in and can swap one for a symlink
# after the checks, so a chown or chmod by path would land on whatever the link names. The cmp
# stub makes that swap right after the content check; chown and chmod report every call.
iicblock="$(bsfn install_if_changed)"
[ -n "$iicblock" ] || { echo "FAIL: no install_if_changed in $BS"; exit 1; }
mkdir "$adir/iic"
printf 'plugin\n' > "$adir/iic/src"
printf 'not the plugin\n' > "$adir/iic/decoy"
chmod 600 "$adir/iic/decoy"
mine="$(stat -c '%U:%G' "$adir/iic/src")"
run_iic() { # target's mode, the owner:group asked for, then "edited" or "swapped"
  rm -f "$adir/iic/dst"
  if [ "${3-}" = edited ]; then printf 'an older plugin\n' > "$adir/iic/dst"; else cp "$adir/iic/src" "$adir/iic/dst"; fi
  chmod "$1" "$adir/iic/dst"
  D="$adir/iic" OG="$2" HOW="${3-}" bash -c '
    atomic_install_file() { echo "ATOMIC $2"; }
    chown() { echo "CHOWN $*"; command chown "$@"; }
    chmod() { echo "CHMOD $*"; command chmod "$@"; }
    cmp() { command cmp "$@" || return; [ "$HOW" != swapped ] || ln -sfn "$D/decoy" "$3"; }
    '"$iicblock"'
    install_if_changed "$D/src" "$D/dst" 0644 "${OG%%:*}" "${OG#*:}"' 2>&1
}
iic_is() { # label want got
  [ "$3" = "$2" ] && echo "PASS $1" || { printf 'FAIL %s: got\n%s\nwant\n%s\n' "$1" "$3" "$2"; fails=$((fails + 1)); }
}
iic_is "the same bytes, owner and mode are left alone" "" "$(run_iic 644 "$mine")"
iic_is "the same bytes with the wrong mode are reinstalled" "ATOMIC $adir/iic/dst" "$(run_iic 600 "$mine")"
iic_is "the same bytes with the wrong owner are reinstalled" "ATOMIC $adir/iic/dst" "$(run_iic 644 "felis-nobody:${mine#*:}")"
iic_is "new bytes are installed" "ATOMIC $adir/iic/dst" "$(run_iic 644 "$mine" edited)"
iic_is "a target swapped for a symlink after the checks is reinstalled" "ATOMIC $adir/iic/dst" "$(run_iic 644 "$mine" swapped)"
iic_is "and the file the link named keeps its mode" 600 "$(stat -c %a "$adir/iic/decoy")"

# --- k3s's own images from its GitHub release ----------------------------------------------------
kablock="$(bsfn stage_k3s_airgap_images)"
mkdir -p "$adir/k3simg" "$adir/k3srel"
printf 'k3s images\n' > "$adir/k3srel/k3s-airgap-images-amd64.tar.zst"
run_airgap() { # sums-file-content
  printf '%s\n' "$1" > "$adir/k3srel/sha256sum-amd64.txt"
  A="$adir" bash -c '
    set -Eeuo pipefail
    warn() { echo "WARN: $*"; }
    ok() { echo "OK: $*"; }
    log() { :; }
    remember_temp() { :; }
    felis_asset_arch() { echo amd64; }
    curl() {
      local out="" url
      while [ "$#" -gt 0 ]; do case "$1" in -o) out="$2"; shift 2 ;; *) url="$1"; shift ;; esac; done
      echo "CURL ${url##*/}" >&2
      [ -f "$A/k3srel/${url##*/}" ] || return 22
      if [ -n "$out" ]; then cp "$A/k3srel/${url##*/}" "$out"; else cat "$A/k3srel/${url##*/}"; fi
    }
    FELIS_K3S_VERSION=v1.36.4+k3s1 K3S_IMAGES_DIR="$A/k3simg"
    '"$kablock"'
    stage_k3s_airgap_images
    echo STAGED-OK' 2>&1
}
ksum="$(sha "$adir/k3srel/k3s-airgap-images-amd64.tar.zst")"
out="$(run_airgap "$ksum  k3s-airgap-images-amd64.tar.zst")"
expect "k3s's images are downloaded where k3s imports them" "OK: staged k3s v1.36.4+k3s1's images" "$out"
cmp -s "$adir/k3simg/k3s-airgap-images-amd64.tar.zst" "$adir/k3srel/k3s-airgap-images-amd64.tar.zst" \
  && echo "PASS the staged tarball is the release's" || { echo "FAIL: the tarball was not staged"; fails=$((fails + 1)); }
[ "$(ls -A "$adir/k3simg")" = k3s-airgap-images-amd64.tar.zst ] && echo "PASS no temp file is left where k3s reads" \
  || { echo "FAIL: a temp file was left in the images directory: $(ls -A "$adir/k3simg")"; fails=$((fails + 1)); }
out="$(run_airgap "$ksum  k3s-airgap-images-amd64.tar.zst")"
case "$out" in *"CURL k3s-airgap-images-amd64.tar.zst"*) echo "FAIL: staged images were downloaded again"; fails=$((fails + 1)) ;; *) echo "PASS staged images are not downloaded again" ;; esac
rm -f "$adir/k3simg/k3s-airgap-images-amd64.tar.zst"
out="$(run_airgap "$(printf '%064d' 0)  k3s-airgap-images-amd64.tar.zst")"
expect "a tarball that does not match k3s's sums is refused" "WARN: k3s-airgap-images-amd64.tar.zst hashes to $ksum" "$out"
expect "and the install goes on without it" "STAGED-OK" "$out"
[ -z "$(ls -A "$adir/k3simg")" ] && echo "PASS a refused tarball leaves nothing for k3s to import" \
  || { echo "FAIL: a refused tarball was left: $(ls -A "$adir/k3simg")"; fails=$((fails + 1)); }
out="$(run_airgap "$ksum  k3s-airgap-images-arm64.tar.zst")"
expect "a sums file that does not list the tarball stages nothing" "WARN: k3s v1.36.4+k3s1 lists no sha256 for k3s-airgap-images-amd64.tar.zst" "$out"
case "$out" in *"CURL k3s-airgap-images-amd64.tar.zst"*) echo "FAIL: an unlisted tarball was downloaded"; fails=$((fails + 1)) ;; *) echo "PASS an unlisted tarball is not downloaded" ;; esac

# --- one release lookup per tag ------------------------------------------------------------------
rjblock="$(bsfn load_release_json; bsfn github_asset_id)"
out="$(bash -c '
  github_api() { echo "API $1" >&2; printf "{\"assets\":[{\"url\":\"https://api.github.com/repos/o/r/releases/assets/11\",\"name\":\"SHA256SUMS\"},{\"url\":\"https://api.github.com/repos/o/r/releases/assets/12\",\"name\":\"felis-velocity.jar\"}]}"; }
  repo_slug() { echo o/r; }
  RELEASE_JSON_TAG="" RELEASE_JSON=""
  '"$rjblock"'
  load_release_json v9.9.9
  github_asset_id v9.9.9 SHA256SUMS; github_asset_id v9.9.9 felis-velocity.jar' 2>&1)"
[ "$(printf '%s\n' "$out" | grep -c '^API')" = 1 ] && echo "PASS one release is looked up once" \
  || { echo "FAIL: the release was looked up more than once: $out"; fails=$((fails + 1)); }
expect "and every asset is found in it" "11
12" "$out"

# --- FELIS_ARTIFACT_DIR is checked before anything happens ------------------------------------
vsblock="$(awk '/^  if \[ -n "\$FELIS_ARTIFACT_DIR" \]; then$/ { f = 1 } f { print } f && /^  fi$/ { exit }' "$BS")"
case "$vsblock" in *"FELIS_SKIP_FETCH both"*) ;; *) echo "FAIL: the FELIS_ARTIFACT_DIR checks in validate_settings moved"; exit 1 ;; esac
run_vs() { # FELIS_ARTIFACT_DIR [FELIS_REF_PINNED]
  FELIS_ARTIFACT_DIR="$1" FELIS_REF_PINNED="${2:-}" bash -c 'die() { echo "DIE: $*"; exit 1; }
'"$vsblock"'
echo VALID'
}
expect "a relative FELIS_ARTIFACT_DIR is refused" "DIE: FELIS_ARTIFACT_DIR must be an absolute path" "$(run_vs release-assets)"
expect "a FELIS_ARTIFACT_DIR without SHA256SUMS is refused up front" "SHA256SUMS does not exist" "$(run_vs "$adir/nowhere")"
printf 'x  y\n' > "$adir/SHA256SUMS"
expect "a FELIS_ARTIFACT_DIR with SHA256SUMS is accepted" "VALID" "$(run_vs "$adir")"
expect "FELIS_ARTIFACT_DIR and FELIS_REF together are refused" "DIE: FELIS_ARTIFACT_DIR and FELIS_REF both name what to install" "$(run_vs "$adir" 1)"
rm -rf "$adir"

# --- FELIS_RELEASE installs one published release, from its assets --------------------------
# The way back to an earlier release. Without it a rerun resolves the newest release (and
# migrates again), and FELIS_REF builds from source; anything else that names what to install
# would silently win over it.
vrblock="$(awk '/^  if \[ -n "\$FELIS_RELEASE" \]; then$/ { f = 1 } f { print } f && /^  fi$/ { exit }' "$BS")"
case "$vrblock" in *"FELIS_SKIP_FETCH both"*) ;; *) echo "FAIL: the FELIS_RELEASE checks in validate_settings moved"; exit 1 ;; esac
run_vr() { # FELIS_RELEASE, with the other settings from the environment
  FELIS_RELEASE="$1" bash -c 'die() { echo "DIE: $*"; exit 1; }
bootstrap_from_tui() { [ "${FELIS_BOOTSTRAP_FROM_TUI:-}" = 1 ]; }
FELIS_REF_PINNED="${FELIS_REF_PINNED:-}" FELIS_ARTIFACT_DIR="${FELIS_ARTIFACT_DIR:-}"
FELIS_VERSION_BOOTSTRAP="${FELIS_VERSION_BOOTSTRAP:-release}"
'"$vrblock"'
echo VALID' 2>&1
}
expect "a release tag is accepted" "VALID" "$(run_vr v1.2.3)"
for bad in 1.2.3 v1.2 main v1.2.3-rc1 'v1.2.3;id'; do
  expect "FELIS_RELEASE=$bad is refused" "DIE: FELIS_RELEASE must be a release tag like v1.2.3 (got '$bad')" "$(run_vr "$bad")"
done
expect "FELIS_RELEASE and FELIS_REF together are refused" "DIE: FELIS_RELEASE and FELIS_REF both name what to install" "$(FELIS_REF_PINNED=1 run_vr v1.2.3)"
expect "FELIS_RELEASE and FELIS_ARTIFACT_DIR together are refused" "DIE: FELIS_RELEASE and FELIS_ARTIFACT_DIR both name what to install" "$(FELIS_ARTIFACT_DIR=/srv/assets run_vr v1.2.3)"
expect "FELIS_RELEASE and FELIS_SKIP_FETCH together are refused" "DIE: FELIS_RELEASE and FELIS_SKIP_FETCH both name what to install" "$(FELIS_SKIP_FETCH=1 run_vr v1.2.3)"
expect "FELIS_RELEASE on the dev channel is refused" "DIE: FELIS_RELEASE names a published release, which the dev channel does not install" "$(FELIS_VERSION_BOOTSTRAP=dev run_vr v1.2.3)"
expect "FELIS_RELEASE under felis setup is refused" "DIE: FELIS_RELEASE does not reach felis setup's install" "$(FELIS_BOOTSTRAP_FROM_TUI=1 run_vr v1.2.3)"

riblock="$(bsfn resolve_install_ref)"
[ -n "$riblock" ] && [ "$(printf '%s\n' "$riblock" | wc -l)" -lt 60 ] \
  || { echo "FAIL: no resolve_install_ref in $BS, or its closing brace moved"; exit 1; }
urblock="$(bsfn use_release_binary)"
run_ri() { # FELIS_RELEASE [published tags]
  FELIS_RELEASE="$1" PUBLISHED="${2:-v1.2.3 v1.4.0}" bash -c 'die() { echo "DIE: $*"; exit 1; }
log() { :; }
ok() { echo "OK: $*"; }
github_latest_tag() { echo "LOOKED UP THE NEWEST" >&2; echo v1.4.0; }
load_release_json() { case " $PUBLISHED " in *" $1 "*) ;; *) return 1 ;; esac; }
FELIS_REF="" FELIS_REF_PINNED="" FELIS_VERSION_BOOTSTRAP=release
'"$riblock"'
'"$urblock"'
resolve_install_ref
use_release_binary && echo "FROM ASSETS" || echo "FROM SOURCE"
echo "ref=$FELIS_REF version=$FELIS_VERSION"' 2>&1
}
out="$(run_ri v1.2.3)"
expect "a named release is what gets installed" "ref=v1.2.3 version=v1.2.3" "$out"
expect "from its published assets" "FROM ASSETS" "$out"
case "$out" in
  *"LOOKED UP THE NEWEST"*) echo "FAIL a named release still looked up the newest"; fails=$((fails + 1)) ;;
  *) echo "PASS a named release does not look up the newest" ;;
esac
expect "an unpublished release stops the install" "DIE: could not find the published Felis release v1.3.9" "$(run_ri v1.3.9)"
expect "without one the newest release is installed" "ref=v1.4.0 version=v1.4.0" "$(run_ri "")"

# --- the setup screens' credentials come back from /etc/felis ---------------------------
# `felis setup` keeps the relay password and the uploads bucket's keys in /etc/felis as
# well as in their Secrets. A reinstall, or a host rebuilt from a bundle's state/, starts
# k3s empty, so every run applies the Secrets from those files; an install from before
# them moves the Secrets' values into the files first. No value may reach kubectl's argv.
for f in secret_key_to_file apply_setup_credential_secrets; do
  [ -n "$(bsfn "$f")" ] || { echo "FAIL: no ${f} in $BS"; exit 1; }
  [ "$(bsfn "$f" | wc -l)" -lt 45 ] \
    || { echo "FAIL: the extracted ${f} is not the function -- did its closing brace move?"; exit 1; }
done
grep -q '^  apply_setup_credential_secrets$' "$BS" \
  && echo "PASS every run applies the setup screens' Secrets" \
  || { echo "FAIL: the install no longer calls apply_setup_credential_secrets"; fails=$((fails + 1)); }
credir="$(mktemp -d)"
credcalls="$(mktemp)"
run_creds() { # secrets-in-cluster(0/1) smtp-username uploads-context
  : > "$credcalls"
  HAVE="$1" SMTPUSER="$2" UPCTX="$3" CALLS="$credcalls" STATE_DIR="$credir" CONTROL_NS=felis MINECRAFT_NS=minecraft bash -c '
    set -Eeuo pipefail
    SMTP_PASSWORD_FILE="${STATE_DIR}/smtp-password"
    UPLOADS_S3_ACCESS_KEY_FILE="${STATE_DIR}/uploads-s3-access-key"
    UPLOADS_S3_SECRET_KEY_FILE="${STATE_DIR}/uploads-s3-secret-key"
    remember_temp() { :; }
    warn() { printf "WARN: %s\n" "$*"; }
    persisted_smtp_block() { printf "[smtp]\nhost = \"mail.example.com\"\nusername = \"%s\"\n" "$SMTPUSER"; }
    persisted_registry_block() { printf "user_uploads_context = \"%s\"\n" "$UPCTX"; }
    kube() {
      case "$*" in
        *" get secret "*)
          printf "GET %s\n" "$*" >>"$CALLS"
          [ "$HAVE" = 1 ] || return 1
          case "$*" in
            *felis-smtp*) printf "cmVsYXkgcHcvMSt4" ;;          # relay pw/1+x
            *access_key_id*) printf "QUtJQU9MRA==" ;;           # AKIAOLD
            *secret_access_key*) printf "b2xkL3NlY3JldCtrZXk=" ;; # old/secret+key
          esac ;;
        "apply -f -") cat >/dev/null; printf "APPLY\n" >>"$CALLS" ;;
        *) printf "KUBE %s\n" "$*" >>"$CALLS" ;;
      esac
    }
    '"$(bsfn secret_key_to_file)"'
    '"$(bsfn apply_setup_credential_secrets)"'
    apply_setup_credential_secrets' 2>&1
  cat "$credcalls"
}
applies() { printf '%s\n' "$1" | grep -c '^APPLY$'; }

out="$(run_creds 1 felis s3://uploads)"
[ "$(cat "$credir/smtp-password")" = "relay pw/1+x" ] \
  && [ "$(cat "$credir/uploads-s3-access-key")" = "AKIAOLD" ] \
  && [ "$(cat "$credir/uploads-s3-secret-key")" = "old/secret+key" ] \
  && echo "PASS an install from before the host copies moves the Secrets' values onto the host" \
  || { echo "FAIL: the Secrets' values did not land in ${credir}:"; printf '%s\n' "$out"; fails=$((fails + 1)); }
for f in smtp-password uploads-s3-access-key uploads-s3-secret-key; do
  case "$(ls -l "$credir/$f" | cut -c1-10)" in
    -rw-------) echo "PASS ${f} is root's alone" ;;
    *) echo "FAIL: ${f} is $(ls -l "$credir/$f" | cut -c1-10), want -rw-------"; fails=$((fails + 1)) ;;
  esac
done
[ -z "$(find "$credir" -name '*.??????' | head -1)" ] && echo "PASS the move leaves no temporary file in /etc/felis" \
  || { echo "FAIL: a temporary file stayed behind: $(ls -a "$credir")"; fails=$((fails + 1)); }
expect "the relay password is applied to the control plane" \
  "KUBE -n felis create secret generic felis-smtp --from-file=password=${credir}/smtp-password --dry-run=client -o yaml" "$out"
expect "and to the workload namespace the reaper mails from" \
  "KUBE -n minecraft create secret generic felis-smtp --from-file=password=${credir}/smtp-password --dry-run=client -o yaml" "$out"
expect "the bucket keys are applied to the control plane" \
  "KUBE -n felis create secret generic felis-uploads-s3 --from-file=access_key_id=${credir}/uploads-s3-access-key --from-file=secret_access_key=${credir}/uploads-s3-secret-key --dry-run=client -o yaml" "$out"
[ "$(applies "$out")" = 3 ] && echo "PASS all three Secrets are piped to kubectl apply" \
  || { echo "FAIL: expected 3 applies:"; printf '%s\n' "$out"; fails=$((fails + 1)); }
case "$out" in
  *"relay pw"*|*AKIAOLD*|*"old/secret"*|*WARN*) echo "FAIL: a credential reached argv or the log, or a warning fired:"; printf '%s\n' "$out"; fails=$((fails + 1)) ;;
  *) echo "PASS no credential reaches kubectl's argv or the log" ;;
esac

printf 'new pw' > "$credir/smtp-password"
out="$(run_creds 1 felis s3://uploads)"
[ "$(cat "$credir/smtp-password")" = "new pw" ] && ! printf '%s\n' "$out" | grep -q '^GET' \
  && echo "PASS the host copy wins over the cluster's" \
  || { echo "FAIL: the cluster's value replaced the host copy:"; printf '%s\n' "$out"; fails=$((fails + 1)); }
[ "$(applies "$out")" = 3 ] && echo "PASS a re-run applies the Secrets from the host copies" \
  || { echo "FAIL: expected 3 applies on the re-run:"; printf '%s\n' "$out"; fails=$((fails + 1)); }

rm -f "$credir"/*
out="$(run_creds 0 felis s3://uploads)"
expect "a relay that signs in with no password anywhere is reported" \
  "WARN: [smtp] signs in with a username, but its password is in neither ${credir}/smtp-password nor the cluster" "$out"
expect "a bucket with no keys anywhere is reported" \
  "WARN: uploads go to an S3 bucket, but its keys are in neither" "$out"
[ "$(applies "$out")" = 0 ] && [ -z "$(ls -A "$credir")" ] \
  && echo "PASS nothing is applied or written without a value" \
  || { echo "FAIL: something was applied or written without a value:"; printf '%s\n' "$out"; ls -A "$credir"; fails=$((fails + 1)); }

out="$(run_creds 0 "" /var/lib/felis/uploads)"
case "$out" in
  *WARN*|*APPLY*) echo "FAIL: a relay without AUTH and local uploads were reported or applied:"; printf '%s\n' "$out"; fails=$((fails + 1)) ;;
  *) echo "PASS a relay without AUTH and local uploads need no credentials" ;;
esac

printf 'AKIAONLY' > "$credir/uploads-s3-access-key"
out="$(run_creds 0 "" s3://uploads)"
case "$out" in
  *"create secret generic felis-uploads-s3"*) echo "FAIL: a bucket Secret was applied from one key alone:"; printf '%s\n' "$out"; fails=$((fails + 1)) ;;
  *) expect "one bucket key alone is reported as missing keys" "WARN: uploads go to an S3 bucket" "$out" ;;
esac
rm -rf "$credir" "$credcalls"

# Worker admission reuses the installer but must never enter host control-plane setup.
before "distributed host firewall runs the newly built binary" \
  '  build_image' '"$HOST_BIN" node firewall --controller' "$(bsfn main)"
before "distributed host firewall is installed before platform deployment" \
  '"$HOST_BIN" node firewall --controller' '  deploy_postgres' "$(bsfn main)"
expect "distributed admission preserves existing API-server arguments" \
  "echo 'kube-apiserver-arg+:'" "$(bsfn write_k3s_config)"
worker="$(bsfn main_worker)"
expect "source worker install builds the requested binary even when an older binary exists" \
  'if [ -z "$HAVE_PREBUILT_BINARY" ]; then install_go_toolchain; build_nano_binary; fi' "$worker"
before "worker identity is checked before the agent config is written" \
  'refusing to rename it' 'cat > "$K3S_CONFIG_DROPIN"' "$worker"
expect "worker rejects server tokens and verifies the CA-pinned bootstrap shape" \
  'K10[0-9a-f]{64}::[a-z0-9]{6}\.[a-z0-9]{16}' "$worker"
expect "worker cannot replace a controller" 'refusing to turn a controller into a worker' "$worker"
expect "worker is quarantined" 'felis.lolicon.best/unapproved=true:NoSchedule' "$worker"
expect "worker mirror preserves logical references and points at the cluster service" \
  'http://${WORKER_REGISTRY_IP}:5000' "$worker"
expect "worker disables registry endpoint fallback" 'disable-default-registry-endpoint: true' "$worker"
for forbidden in deploy_bundle install_cloudflared install_velocity run_migrations load_or_make_secrets; do
  case "$worker" in
    *"$forbidden"*) echo "FAIL worker invokes $forbidden"; fails=$((fails + 1));;
    *) echo "PASS worker does not invoke $forbidden";;
  esac
done
badtoken="$(mktemp)"
printf 'server-token-not-bootstrap' > "$badtoken"
out="$(WORKER_TOKEN_FILE="$badtoken" bash -c '
  die() { printf "DIE: %s\n" "$*"; exit 1; }
  WORKER_NAME=b WORKER_SERVER=https://192.0.2.1:6443 WORKER_REGISTRY_IP=10.43.0.10 WORKER_PEERS=192.0.2.1/32
  '"$worker"'
  main_worker
')"
expect "worker refuses a copied server token before changing the machine" 'worker accepts only CA-pinned bootstrap tokens' "$out"
rm -f "$badtoken"

# ---------------------------------------------------------------------------------------
if [ "$fails" -eq 0 ]; then
  echo "ALL PASS"
else
  echo "$fails FAILED"
  exit 1
fi
