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
    systemctl() {
      echo "SYSTEMCTL $*" >> "$calls"
      case "$*" in "is-active --quiet firewalld") return 1 ;; esac
      return 0
    }
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
        *) echo "PSQL $* $(cat)" >> "$calls" ;;
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
refute "keep-data leaves the database alone" "DROP DATABASE" "$calls"
[ ! -e "$root/h/opt" ] && [ ! -e "$root/h/bin/felis" ] \
  && echo "PASS /opt/felis and the host binary are removed" \
  || { echo "FAIL /opt/felis or the host binary is still there"; fails=$((fails + 1)); }
[ -z "$(ls "$root/h/units")" ] && echo "PASS every Felis unit file is removed" \
  || { echo "FAIL units left: $(ls "$root/h/units")"; fails=$((fails + 1)); }
expect "the timers are disabled" "SYSTEMCTL disable --now felis-db-backup.timer" "$calls"
expect "the velocity user is removed" "USERDEL felis-velocity" "$calls"
expect "the run ends pointing at the reinstall steps" "Reinstall on top of kept data" "$out"

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

if [ "$fails" -eq 0 ]; then
  echo "ALL PASS"
else
  echo "$fails FAILED"
fi
exit "$fails"
