#!/bin/sh
# Checks for deploy/uninstall.sh. Run it as: sh deploy/uninstall_test.sh
#
# The script is sourced with FELIS_UNINSTALL_SOURCED=1, every host path pointed into a
# scratch directory, and the commands that would change the host (systemctl, k3s, nft,
# firewall-cmd, runuser, docker, userdel) replaced by stubs that log their arguments.
set -u

US="${1:-$(dirname "$0")/uninstall.sh}"
[ -f "$US" ] || { echo "no such script: $US"; exit 1; }
fails=0

expect() { # label needle haystack
  case "$3" in
    *"$2"*) echo "PASS $1" ;;
    *) echo "FAIL $1: expected <$2> in:"; echo "$3"; fails=$((fails + 1)) ;;
  esac
}
refute() { # label needle haystack
  case "$3" in
    *"$2"*) echo "FAIL $1: did not expect <$2> in:"; echo "$3"; fails=$((fails + 1)) ;;
    *) echo "PASS $1" ;;
  esac
}

root="$(mktemp -d)"
trap 'rm -rf "$root"' EXIT

# fresh_host lays out what an install leaves: units, state, /opt/felis, the host binary, a
# k3s with its uninstaller and a volume, a tunnel config and its credentials.
fresh_host() {
  rm -rf "$root/h"
  mkdir -p "$root/h/units" "$root/h/etc" "$root/h/data/db-backups" "$root/h/opt/velocity" \
    "$root/h/bin" "$root/h/storage/pvc-1_minecraft_world-a-0" "$root/h/cf"
  for u in felis-db-backup.timer felis-db-backup.service felis-velocity.service felis-postgres-firewall.service; do
    printf '[Unit]\n' > "$root/h/units/$u"
  done
  printf '[Service]\nExecStart=/usr/local/bin/cloudflared --config %s tunnel run\n' "$root/h/etc/cloudflared.yml" \
    > "$root/h/units/cloudflared-felis.service"
  printf 'tunnel: abc\ncredentials-file: %s\n' "$root/h/cf/abc.json" > "$root/h/etc/cloudflared.yml"
  printf '{}\n' > "$root/h/cf/abc.json"
  printf 'bind = "0.0.0.0:25577"\n' > "$root/h/opt/velocity/velocity.toml"
  for f in secrets.env felis.host.toml bootstrap.done system-server-images velocity.fingerprint; do
    printf 'x\n' > "$root/h/etc/$f"
  done
  printf 'world\n' > "$root/h/storage/pvc-1_minecraft_world-a-0/level.dat"
  mkdir -p "$root/h/data/artifacts"
  printf 'bundle\n' > "$root/h/data/artifacts/felis-image-game-linux-amd64.tar.partial"
  mkdir -p "$root/h/data/postgres/18/docker"
  printf '18\n' > "$root/h/data/postgres/18/docker/PG_VERSION"
  for b in felis k3s k3s-killall.sh k3s-uninstall.sh; do
    printf '#!/bin/sh\necho "RUN %s $*" >> "%s"\n' "$b" "$root/calls" > "$root/h/bin/$b"
    chmod +x "$root/h/bin/$b"
  done
  cat > "$root/h/hba.conf" <<'EOF'
# BEGIN FELIS MANAGED HBA
# Felis rules must precede distro defaults such as 127.0.0.1 ident.
host felis felis 127.0.0.1/32 scram-sha-256
# END FELIS MANAGED HBA

local all all peer
host all all 127.0.0.1/32 ident
EOF
  : > "$root/calls"
}

# run_uninstall <namespaces> <args...>: runs main with the stubs; <namespaces> is what
# `kubectl get namespaces` answers, or "down" for a k3s that does not answer.
run_uninstall() {
  ns="$1"; shift
  NS="$ns" ROOT="$root" STATE_DIR="$root/h/etc" DATA_DIR="$root/h/data" HOST_BIN="$root/h/bin/felis" \
  OPT_DIR="$root/h/opt" UNIT_DIR="$root/h/units" K3S_BIN_DIR="$root/h/bin" \
  K3S_STORAGE="$root/h/storage" K3S_REGISTRIES="$root/h/registries.yaml" \
  CLOUDFLARED_BIN="$root/h/no-cloudflared" FELIS_UNINSTALL_SOURCED=1 bash -c '
    set -Eeuo pipefail
    . "$0"
    calls="$ROOT/calls"
    id() { if [ "${1:-}" = -u ]; then echo 0; else echo "ID $*" >> "$calls"; fi; }
    # PG_HOST: the host PostgreSQL is active (the default), stopped, or not installed (none);
    # PG_START=fail for one that will not start.
    systemctl() {
      echo "SYSTEMCTL $*" >> "$calls"
      case "$*" in
        "is-active --quiet firewalld") return 1 ;;
        "is-active --quiet postgresql") [ "${PG_HOST:-active}" = active ] ;;
        "cat postgresql") [ "${PG_HOST:-active}" != none ] ;;
        "start postgresql") [ "${PG_START:-}" != fail ] ;;
        *) return 0 ;;
      esac
    }
    semanage() { echo "SEMANAGE $*" >> "$calls"; }
    kube() {
      echo "KUBE $*" >> "$calls"
      case "$*" in
        "get namespaces"*) [ "$NS" = down ] && return 1; printf "%s\n" $NS ;;
        "get pv"*) printf "pvc-1 minecraft\npvc-9 other\n" ;;
      esac
    }
    nft() { echo "NFT $*" >> "$calls"; return 1; }
    runuser() {
      shift 3
      case "$*" in
        *"SHOW hba_file"*) echo "$ROOT/h/hba.conf" ;;
        *)
          sql="$(cat)"
          case "$sql" in
            # What the felis role still holds: PG_HELD, one line each; PG_CHECK=fail for a
            # server that refuses the query.
            *pg_shdepend*)
              echo "PSQL-CHECK" >> "$calls"
              [ "${PG_CHECK:-}" = fail ] && { echo "psql: error: connection refused" >&2; return 2; }
              [ -z "${PG_HELD:-}" ] || printf "%s\n" "$PG_HELD" ;;
            *) echo "PSQL $* $sql" >> "$calls"; [ "${PG_DROP:-}" != fail ] ;;
          esac ;;
      esac
    }
    # UFW_STATUS: what `ufw status numbered` answers; ufw translates it outside the C locale.
    ufw() {
      case "$*" in
        "status numbered")
          if [ "${LC_ALL:-}" = C ]; then printf "%s\n" "${UFW_STATUS:-Status: inactive}"; else echo "状态：激活"; fi ;;
        *) echo "UFW $*" >> "$calls" ;;
      esac
    }
    docker() { echo "DOCKER $*" >> "$calls"; }
    userdel() { echo "USERDEL $*" >> "$calls"; }
    uname() { echo testhost; }
    main "$@"' "$US" "$@" 2>&1
}

# --- keep-data, a cluster that runs only Felis -------------------------------------------
fresh_host
out="$(run_uninstall "default kube-system felis minecraft felis-build" --yes)"
calls="$(cat "$root/calls")"
expect "keep-data takes a final bundle first" "RUN felis db backup -config $root/h/etc/felis.host.toml -label manual" "$calls"
expect "a Felis-only cluster is removed with k3s's uninstaller" "RUN k3s-uninstall.sh" "$calls"
expect "the pods are stopped before the volumes move" "RUN k3s-killall.sh" "$calls"
kept="$(ls "$root/h/data/retained" 2>/dev/null)"
expect "the volumes are set aside before k3s deletes them" "k3s-storage-" "$kept"
[ -f "$root/h/data/retained/$kept/pvc-1_minecraft_world-a-0/level.dat" ] \
  && echo "PASS a world survives the uninstall" \
  || { echo "FAIL the world did not survive: $(ls -R "$root/h/data")"; fails=$((fails + 1)); }
[ -f "$root/h/etc/secrets.env" ] && [ -f "$root/h/etc/felis.host.toml" ] \
  && echo "PASS the secrets and felis.toml stay" \
  || { echo "FAIL keep-data removed the secrets"; fails=$((fails + 1)); }
[ ! -e "$root/h/etc/bootstrap.done" ] && [ ! -e "$root/h/etc/velocity.fingerprint" ] \
  && echo "PASS the markers of the removed install go, so a reinstall starts fresh" \
  || { echo "FAIL bootstrap.done or the proxy fingerprint was left"; fails=$((fails + 1)); }
[ ! -e "$root/h/data/artifacts" ] && [ -d "$root/h/data/postgres" ] \
  && echo "PASS keep-data drops the downloaded release assets and keeps the database" \
  || { echo "FAIL keep-data left the release-asset cache or took the database: $(ls "$root/h/data")"; fails=$((fails + 1)); }
refute "keep-data leaves the database alone" "DROP DATABASE" "$calls"
[ ! -e "$root/h/opt" ] && [ ! -e "$root/h/bin/felis" ] \
  && echo "PASS /opt/felis and the host binary are removed" \
  || { echo "FAIL /opt/felis or the host binary is still there"; fails=$((fails + 1)); }
[ -z "$(ls "$root/h/units")" ] && echo "PASS every Felis unit file is removed" \
  || { echo "FAIL units left: $(ls "$root/h/units")"; fails=$((fails + 1)); }
expect "the timers are disabled" "SYSTEMCTL disable --now felis-db-backup.timer" "$calls"
expect "the velocity user is removed" "USERDEL felis-velocity" "$calls"
expect "the run ends pointing at the reinstall steps" "Reinstall on top of kept data" "$out"
[ -f "$root/h/data/postgres/18/docker/PG_VERSION" ] && echo "PASS the database's cluster stays for the reinstall" \
  || { echo "FAIL keep-data removed the database's cluster"; fails=$((fails + 1)); }
stop_at="$(grep -n 'KUBE -n felis scale deployment felis-postgres --replicas=0' "$root/calls" | head -n 1 | cut -d: -f1)"
kill_at="$(grep -n 'RUN k3s-killall.sh' "$root/calls" | head -n 1 | cut -d: -f1)"
[ -n "$stop_at" ] && [ -n "$kill_at" ] && [ "$stop_at" -lt "$kill_at" ] \
  && echo "PASS the database shuts down cleanly before k3s-killall.sh kills what is left" \
  || { echo "FAIL felis-postgres was not stopped before k3s-killall.sh: $calls"; fails=$((fails + 1)); }
expect "and the uninstall waits for it to stop" "KUBE -n felis wait --for=delete pod -l app.kubernetes.io/name=felis,app.kubernetes.io/component=postgres" "$calls"
refute "keep-data keeps the cluster directory's SELinux rule" "SEMANAGE" "$calls"

# --- a failed final bundle stops everything ------------------------------------------------
fresh_host
printf '#!/bin/sh\necho "RUN felis $*" >> "%s"\nexit 1\n' "$root/calls" > "$root/h/bin/felis"
out="$(run_uninstall "default felis minecraft" --yes)"
expect "a failed bundle is fatal" "the final database bundle failed, so nothing was removed" "$out"
[ -d "$root/h/opt" ] && [ -f "$root/h/units/felis-velocity.service" ] \
  && echo "PASS nothing is removed when the bundle fails" \
  || { echo "FAIL the uninstall went on after the bundle failed"; fails=$((fails + 1)); }
run_uninstall "default felis minecraft" --yes --no-backup >/dev/null
[ ! -d "$root/h/opt" ] && echo "PASS --no-backup goes on without one" \
  || { echo "FAIL --no-backup did not remove anything"; fails=$((fails + 1)); }

# --- a shared cluster --------------------------------------------------------------------
fresh_host
out="$(run_uninstall "default kube-system felis minecraft felis-build shop" --yes)"
calls="$(cat "$root/calls")"
refute "a cluster that runs something else keeps k3s" "RUN k3s-uninstall.sh" "$calls"
expect "the reason names the other namespace" "shop" "$out"
expect "Felis's volumes are retained before their claims go" 'KUBE patch pv pvc-1 -p {"spec":{"persistentVolumeReclaimPolicy":"Retain"}}' "$calls"
refute "a volume of another namespace is not touched" "patch pv pvc-9" "$calls"
expect "Felis's namespaces are deleted" "KUBE delete namespace felis minecraft felis-build" "$calls"
expect "the CRD is deleted" "KUBE delete crd minecraftservers.felis.lolicon.best" "$calls"

out="$(fresh_host; run_uninstall down --yes)"
expect "a k3s that does not answer stops the run" "k3s does not answer" "$out"
fresh_host
run_uninstall down --yes --keep-k3s >/dev/null
calls="$(cat "$root/calls")"
refute "--keep-k3s never runs k3s's uninstaller" "RUN k3s-uninstall.sh" "$calls"

# --- purge -------------------------------------------------------------------------------
fresh_host
out="$(run_uninstall "default felis minecraft" --purge --yes)"
calls="$(cat "$root/calls")"
refute "purge takes no bundle" "db backup" "$calls"
expect "purge asks what the role holds first" "PSQL-CHECK" "$calls"
expect "purge drops the database" "DROP DATABASE IF EXISTS felis;" "$calls"
expect "purge drops the role" "DROP ROLE IF EXISTS felis;" "$calls"
expect "purge puts listen_addresses back" "ALTER SYSTEM RESET listen_addresses;" "$calls"
[ ! -e "$root/h/etc" ] && [ ! -e "$root/h/data" ] && echo "PASS purge removes /etc/felis and /var/lib/felis" \
  || { echo "FAIL purge left state behind"; fails=$((fails + 1)); }
[ ! -e "$root/h/cf/abc.json" ] && echo "PASS purge removes the tunnel's credentials file" \
  || { echo "FAIL the tunnel credentials are still there"; fails=$((fails + 1)); }
[ ! -e "$root/h/data/retained" ] && echo "PASS purge does not set the volumes aside" \
  || { echo "FAIL purge kept the volumes"; fails=$((fails + 1)); }
hba="$(cat "$root/h/hba.conf")"
refute "the Felis block leaves pg_hba.conf" "FELIS MANAGED" "$hba"
expect "the distro's own rules stay" "host all all 127.0.0.1/32 ident" "$hba"
case "$hba" in
  "local all all peer"*) echo "PASS no blank line is left where the block was" ;;
  *) echo "FAIL pg_hba.conf starts with: $(printf '%s' "$hba" | head -n 1)"; fails=$((fails + 1)) ;;
esac
expect "purge cleans Docker's build cache" "DOCKER builder prune -af" "$calls"
expect "purge drops the cluster directory's SELinux rule" "SEMANAGE fcontext -d $root/h/data/postgres(/.*)?" "$calls"
refute "purge leaves k3s's pods to k3s-uninstall.sh" "scale deployment felis-postgres" "$calls"

# --- purge after the move into felis-postgres ---------------------------------------------
# The move stopped the host server with the felis database left in it for a rollback.
moved_host() { fresh_host; printf 'moved\n' > "$root/h/data/postgres-moved"; printf 'saved\n' > "$root/h/hba.conf.pre-pg-move"; }
moved_host
out="$(PG_HOST=stopped run_uninstall "default felis minecraft" --purge --yes)"
calls="$(cat "$root/calls")"
expect "purge starts the stopped host server to drop the pre-move copy" "SYSTEMCTL start postgresql" "$calls"
expect "and drops it" "DROP DATABASE IF EXISTS felis;" "$calls"
expect "the server stays stopped afterwards" "SYSTEMCTL stop postgresql" "$calls"
refute "and is not restarted" "SYSTEMCTL restart postgresql" "$calls"
refute "the lockout leaves pg_hba.conf" "FELIS MANAGED" "$(cat "$root/h/hba.conf")"
[ ! -e "$root/h/hba.conf.pre-pg-move" ] && echo "PASS the pg_hba.conf the move saved goes with the copy" \
  || { echo "FAIL purge left pg_hba.conf.pre-pg-move"; fails=$((fails + 1)); }
moved_host
out="$(PG_HOST=stopped PG_START=fail run_uninstall "default felis minecraft" --purge --yes)"
calls="$(cat "$root/calls")"
expect "a host server that will not start is named" "could not start the host PostgreSQL" "$out"
refute "so nothing is dropped" "DROP DATABASE" "$calls"
[ ! -e "$root/h/etc" ] && [ ! -e "$root/h/data" ] && echo "PASS and the purge goes on" \
  || { echo "FAIL the purge stopped at the host server"; fails=$((fails + 1)); }
moved_host
out="$(PG_HOST=stopped PG_DROP=fail run_uninstall "default felis minecraft" --purge --yes)"
calls="$(cat "$root/calls")"
expect "a drop that fails says how to finish by hand" "sudo -u postgres dropdb felis" "$out"
expect "and stops the server it started" "SYSTEMCTL stop postgresql" "$calls"
[ ! -e "$root/h/data" ] && echo "PASS a failed drop does not stop the purge halfway" \
  || { echo "FAIL the purge stopped at a failed drop"; fails=$((fails + 1)); }
fresh_host
(PG_HOST=stopped run_uninstall "default felis minecraft" --purge --yes >/dev/null)  # a subshell: sh keeps a prefix assignment to a function
refute "a stopped host server the move never touched is left alone" "SYSTEMCTL start postgresql" "$(cat "$root/calls")"
moved_host
(PG_HOST=none run_uninstall "default felis minecraft" --purge --yes >/dev/null)  # a subshell: sh keeps a prefix assignment to a function
refute "a host server removed since the move is not started" "SYSTEMCTL start postgresql" "$(cat "$root/calls")"

# --- a purge DROP ROLE would refuse -------------------------------------------------------
# The VM drill: the PG contract tests' felis_pgint was owned by felis, the purge removed the
# units and k3s, then stopped at DROP ROLE with half the host gone.
untouched() { # label
  [ -d "$root/h/opt" ] && [ -f "$root/h/units/felis-velocity.service" ] && [ -d "$root/h/etc" ] \
    && ! grep -q "k3s-uninstall.sh\|DROP DATABASE\|SYSTEMCTL disable" "$root/calls" \
    && echo "PASS $1" \
    || { echo "FAIL $1: $(cat "$root/calls")"; fails=$((fails + 1)); }
}
fresh_host
out="$(PG_HELD="database felis_pgint (owned)
objects in database shop (privileges)" run_uninstall "default felis minecraft" --purge --yes)"
expect "a purge the role cannot survive is refused" "the felis role still holds database felis_pgint (owned); objects in database shop (privileges)" "$out"
expect "with the way to hand the database over" "ALTER DATABASE <name> OWNER TO postgres" "$out"
untouched "nothing is removed when DROP ROLE would fail"
fresh_host
out="$(PG_HELD="database felis_pgint (owned)" CONFIRM_TTY="$root/no-tty/x" run_uninstall "default felis minecraft" --purge)"
expect "the check comes before the plan and the prompt" "the felis role still holds database felis_pgint" "$out"
refute "so nobody confirms a purge that cannot finish" "this will remove" "$out"
fresh_host
out="$(PG_CHECK=fail run_uninstall "default felis minecraft" --purge --yes)"
expect "a server that cannot be asked stops the purge" "could not ask PostgreSQL what the felis role still holds, so nothing was removed: psql: error: connection refused" "$out"
untouched "nothing is removed when the check cannot run"
fresh_host
PG_HELD="database felis_pgint (owned)" run_uninstall "default felis minecraft" --yes >/dev/null
calls="$(cat "$root/calls")"
refute "keep-data drops no role, so it asks nothing" "PSQL-CHECK" "$calls"
expect "and goes on" "RUN k3s-uninstall.sh" "$calls"

# --- the pieces read before they are removed ---------------------------------------------
fresh_host
lib() { STATE_DIR="$root/h/etc" OPT_DIR="$root/h/opt" UNIT_DIR="$root/h/units" FELIS_UNINSTALL_SOURCED=1 \
  bash -c '. "$0"; '"$1" "$US"; }
expect "the game port comes from velocity.toml" "25577" "$(lib game_port)"
expect "FELIS_GAME_PORT overrides it" "25599" "$(FELIS_GAME_PORT=25599 lib game_port)"
rm "$root/h/opt/velocity/velocity.toml"
expect "without velocity.toml the port is the default" "25565" "$(lib game_port)"
printf '[Service]\nExecStart=/usr/local/bin/felis nano -listen 10.0.0.5:8082 -config x\n' > "$root/h/units/felis-nano.service"
expect "the nano port comes from its unit" "8082" "$(lib nano_port)"
expect "the tunnel config comes from the cloudflared unit" "$root/h/etc/cloudflared.yml" "$(lib tunnel_config)"
rm "$root/h/units/cloudflared-felis.service"
expect "without the unit the tunnel config is the default path" "$root/h/etc/cloudflared.yml" "$(lib tunnel_config)"

out="$(FELIS_UNINSTALL_SOURCED=1 bash -c '. "$0"; parse_args --bogus' "$US" 2>&1)"
expect "an unknown option is refused" "unknown option: --bogus" "$out"
printf 'nope\n' > "$root/tty"
out="$(CONFIRM_TTY="$root/tty" FELIS_UNINSTALL_SOURCED=1 bash -c '. "$0"; confirm; echo WENT ON' "$US" 2>&1)"
expect "any answer but the word stops it" "not confirmed; nothing was changed" "$out"
printf 'yes\n' > "$root/tty"
out="$(CONFIRM_TTY="$root/tty" FELIS_UNINSTALL_SOURCED=1 bash -c '. "$0"; confirm; echo WENT ON' "$US" 2>&1)"
expect "yes goes on" "WENT ON" "$out"
out="$(CONFIRM_TTY="$root/tty" FELIS_UNINSTALL_SOURCED=1 bash -c '. "$0"; PURGE=1; confirm; echo WENT ON' "$US" 2>&1)"
expect "a purge wants the word purge" "not confirmed" "$out"
out="$(CONFIRM_TTY="$root/no-tty/x" FELIS_UNINSTALL_SOURCED=1 bash -c '. "$0"; confirm; echo WENT ON' "$US" 2>&1)"
expect "with no terminal it asks for --yes" "no terminal to confirm on; re-run with --yes" "$out"

# Every unit the installer writes is one the uninstaller removes: a timer left behind
# keeps firing a felis binary that is gone.
units="$(awk '/^FELIS_UNITS=\(/ { f = 1; next } f && /^\)/ { f = 0 } f' "$US")"
for u in $(sed -n 's|^[A-Z_]*="/etc/systemd/system/\([^"]*\)"$|\1|p' "$(dirname "$US")/bootstrap.sh"); do
  expect "the uninstaller removes $u" " $u" " $(printf '%s' "$units" | tr '\n' ' ')"
done

# --- ufw: the rules bootstrap added, by the comments bootstrap gives them ------------------
# The listing is what `ufw status numbered` prints once bootstrap has run: a rule of the
# operator's own first (commented, as an operator may), then each felis- rule bootstrap.sh can
# add and its IPv6 twin.
tags="$(grep -o 'comment felis-[a-z0-9-]*' "$(dirname "$US")/bootstrap.sh" | awk '{ print $2 }' | sort -u)"
case " $(printf '%s ' $tags)" in
  *" felis-k3s-pods "*" felis-proxy "*) echo "PASS bootstrap tags its ufw rules" ;;
  *) echo "FAIL bootstrap.sh adds no felis-k3s-pods and felis-proxy ufw rules: <$tags>"; fails=$((fails + 1)) ;;
esac
listing="Status: active

     To                         Action      From
     --                         ------      ----
[ 1] 22/tcp                     ALLOW IN    Anywhere                    # ssh"
n=1
for twin in "" " (v6)"; do
  for t in $tags; do
    n=$((n + 1))
    listing="${listing}
$(printf '[%2d] Rule%-22s ALLOW IN    Anywhere%-19s # %s' "$n" "$twin" "$twin" "$t")"
  done
done
listing="${listing}
$(printf '[%2d] 22/tcp (v6)                ALLOW IN    Anywhere (v6)' "$((n + 1))")"
# numbers <listing> <regex>: the rule numbers whose line matches, highest first.
numbers() { printf '%s\n' "$1" | grep -E "$2" | sed 's/^\[ *\([0-9]*\)\].*/\1/' | sort -rn | paste -sd ' ' -; }
deletes() { grep '^UFW --force delete' "$root/calls" | awk '{ print $4 }' | paste -sd ' ' -; }

fresh_host
out="$(UFW_STATUS="$listing" run_uninstall "default felis minecraft" --yes)"
want="$(numbers "$listing" '# felis-')"
[ -n "$want" ] && [ "$(deletes)" = "$want" ] \
  && echo "PASS a Felis-only cluster's uninstall deletes every felis- ufw rule, highest first" \
  || { echo "FAIL a Felis-only cluster: deleted <$(deletes)>, want <$want>"; fails=$((fails + 1)); }
case " $(deletes) " in
  *" 1 "* | *" $((n + 1)) "*) echo "FAIL the operator's own ufw rules were deleted: $(deletes)"; fails=$((fails + 1)) ;;
  *) echo "PASS the operator's own ufw rules stay" ;;
esac
expect "  and says so" "ufw rules removed" "$out"

fresh_host
UFW_STATUS="$listing" run_uninstall "default kube-system felis minecraft felis-build shop" --yes >/dev/null
[ "$(deletes)" = "$(numbers "$listing" '# felis-' | tr ' ' '\n' | grep -vxE "$(numbers "$listing" '# felis-k3s-' | tr ' ' '|')" | paste -sd ' ' -)" ] \
  && echo "PASS a k3s that stays keeps its pod and service ranges in ufw" \
  || { echo "FAIL a kept k3s: deleted <$(deletes)>, listing:"; echo "$listing"; fails=$((fails + 1)); }

fresh_host
UFW_STATUS="Status: inactive" run_uninstall "default felis minecraft" --yes >/dev/null
[ -z "$(deletes)" ] && echo "PASS an inactive ufw is left alone" \
  || { echo "FAIL an inactive ufw: deleted <$(deletes)>"; fails=$((fails + 1)); }

# The database's cluster and the move's marker are where bootstrap put them, or keep-data
# and purge act on a directory that is not there.
paths="$(FELIS_UNINSTALL_SOURCED=1 bash -c '. "$0"; printf "%s %s\n" "$PG_DATA_DIR" "$PG_MOVED_MARKER"' "$US")"
bspaths="$(sed -n 's/^PG_DATA_DIR="\(.*\)"$/\1/p; s/^PG_MOVED_MARKER="\(.*\)"$/\1/p' "$(dirname "$US")/bootstrap.sh" | paste -sd ' ' -)"
[ -n "$bspaths" ] && [ "$paths" = "$bspaths" ] && echo "PASS the database paths agree with bootstrap" \
  || { echo "FAIL uninstall's database paths <$paths> differ from bootstrap's <$bspaths>"; fails=$((fails + 1)); }
bscache="$(sed -n 's/^ARTIFACT_CACHE="\(.*\)"$/\1/p' "$(dirname "$US")/bootstrap.sh")"
uscache="$(FELIS_UNINSTALL_SOURCED=1 bash -c '. "$0"; printf "%s/artifacts\n" "$DATA_DIR"' "$US")"
[ -n "$bscache" ] && [ "$uscache" = "$bscache" ] && echo "PASS the release-asset cache is where bootstrap keeps it" \
  || { echo "FAIL uninstall removes <$uscache>, bootstrap caches release assets in <$bscache>"; fails=$((fails + 1)); }

if [ "$fails" -eq 0 ]; then
  echo "ALL PASS"
else
  echo "$fails FAILED"
fi
exit "$fails"
