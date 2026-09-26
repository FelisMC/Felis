#!/usr/bin/env bash
# Removes what deploy/bootstrap.sh installed on this host.
#
#   sudo bash deploy/uninstall.sh                 # remove Felis, keep the data
#   sudo bash deploy/uninstall.sh --purge         # remove the data too
#   curl -fsSL <raw-url>/deploy/uninstall.sh | sudo bash -s -- --yes
#
# Options:
#   --purge       also delete /etc/felis and /var/lib/felis (the database's cluster, the
#                 database bundles, and anything an earlier keep-data run set aside), and
#                 drop the felis database and role from a host PostgreSQL an earlier
#                 release installed. Asks for the word "purge" unless --yes is given.
#   --keep-k3s    leave k3s installed and remove only Felis's namespaces and CRD.
#   --remove-k3s  run k3s's own uninstaller even when other workloads live in the cluster.
#   --no-backup   skip the final database bundle keep-data mode takes first.
#   --yes         do not ask.
#
# Keep-data mode (the default) first takes a database bundle (`felis db backup -label
# manual`) and stops if that fails. It leaves /etc/felis (the secrets, felis.toml,
# offsite.env) and /var/lib/felis in place, and with it the felis database: felis-postgres
# keeps its cluster in /var/lib/felis/postgres, stopped cleanly before k3s goes, and a
# host PostgreSQL an earlier release installed keeps its copy. The world, archive,
# registry and upload volumes live under k3s's storage directory, which k3s's uninstaller
# deletes, so they are moved to /var/lib/felis/retained/k3s-storage-<UTC stamp> first; with
# --keep-k3s their PersistentVolumes are switched to Retain before the namespaces go.
# docs/operations.md walks through reinstalling on top of what is left.
#
# Either mode leaves packages alone (Docker, PostgreSQL, git and the rest), and the swap
# file a low-memory host got: other software may use them. docs/operations.md lists the
# package commands for a bare host.
set -Eeuo pipefail

STATE_DIR="${STATE_DIR:-/etc/felis}"
DATA_DIR="${DATA_DIR:-/var/lib/felis}"
RETAIN_DIR="${DATA_DIR}/retained"
HOST_BIN="${HOST_BIN:-/usr/local/bin/felis}"
OPT_DIR="${OPT_DIR:-/opt/felis}"
UNIT_DIR="${UNIT_DIR:-/etc/systemd/system}"
K3S_BIN_DIR="${K3S_BIN_DIR:-/usr/local/bin}"
K3S_STORAGE="${K3S_STORAGE:-/var/lib/rancher/k3s/storage}"
K3S_REGISTRIES="${K3S_REGISTRIES:-/etc/rancher/k3s/registries.yaml}"
export KUBECONFIG="${KUBECONFIG:-/etc/rancher/k3s/k3s.yaml}"
CLOUDFLARED_BIN="${CLOUDFLARED_BIN:-/usr/local/bin/cloudflared}"
VELOCITY_USER="felis-velocity"
DB_NAME="felis"
DB_USER="felis"
CONTROL_NS="felis"
# The database bootstrap runs in k3s, its cluster on a hostPath under DATA_DIR, and the
# marker bootstrap leaves once it moved a host PostgreSQL's felis database into it.
PG_DEPLOYMENT="felis-postgres"
PG_DATA_DIR="${DATA_DIR}/postgres"
PG_MOVED_MARKER="${DATA_DIR}/postgres-moved"
POD_CIDR="10.42.0.0/16"
SERVICE_CIDR="10.43.0.0/16"
FELIS_PANEL_NODEPORT="${FELIS_PANEL_NODEPORT:-30443}"
CONFIRM_TTY="${CONFIRM_TTY:-/dev/tty}"
FELIS_NAMESPACES=(felis minecraft felis-build)
FELIS_CRD="minecraftservers.felis.lolicon.best"
# Every unit the installer and `felis setup` write. Timers first, so none fires into a
# service that is already gone.
FELIS_UNITS=(
  felis-db-backup.timer felis-watchdog.timer felis-offsite.timer felis-build-tools.timer felis-update-check.timer
  felis-db-backup.service felis-watchdog.service felis-offsite.service felis-build-tools.service felis-update-check.service
  felis-velocity.service felis-nano.service cloudflared-felis.service
  felis-postgres-firewall.service
)

PURGE=0
K3S_MODE=auto
BACKUP=1
ASSUME_YES=0
TUNNEL_CONFIG=""

log()  { printf '\033[1;36m[felis]\033[0m %s\n' "$*"; }
ok()   { printf '\033[1;32m[ ok ]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[warn]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[fail]\033[0m %s\n' "$*" >&2; exit 1; }

parse_args() {
  while [ $# -gt 0 ]; do
    case "$1" in
      --purge) PURGE=1 ;;
      --keep-k3s) K3S_MODE=keep ;;
      --remove-k3s) K3S_MODE=remove ;;
      --no-backup) BACKUP=0 ;;
      --yes|-y) ASSUME_YES=1 ;;
      -h|--help) printf 'usage: uninstall.sh [--purge] [--keep-k3s|--remove-k3s] [--no-backup] [--yes]\n'; exit 0 ;;
      *) die "unknown option: $1 (see --help)" ;;
    esac
    shift
  done
}

kube() { "${K3S_BIN_DIR}/k3s" kubectl "$@"; }
k3s_present() { [ -x "${K3S_BIN_DIR}/k3s" ]; }

# foreign_namespaces prints the namespaces that are neither k3s's own nor Felis's, one per
# line. A cluster with none of them exists for Felis alone, and removing k3s takes nothing
# else with it.
foreign_namespaces() {
  kube get namespaces -o 'jsonpath={range .items[*]}{.metadata.name}{"\n"}{end}' \
    | awk '$0 != "" && $0 != "default" && $0 !~ /^kube-/ && $0 != "felis" && $0 != "minecraft" && $0 != "felis-build"'
}

# decide_k3s turns K3S_MODE=auto into keep or remove.
decide_k3s() {
  k3s_present || { K3S_MODE=absent; return 0; }
  [ "$K3S_MODE" = auto ] || return 0
  local others
  if ! others="$(foreign_namespaces)"; then
    die "k3s does not answer, so this cannot tell whether it runs anything besides Felis; start it (systemctl start k3s) or pass --keep-k3s or --remove-k3s"
  fi
  if [ -z "$others" ]; then
    K3S_MODE=remove
  else
    K3S_MODE=keep
    log "k3s also runs namespaces Felis did not create ($(printf '%s' "$others" | tr '\n' ' ')); leaving k3s installed"
  fi
}

confirm() {
  [ "$ASSUME_YES" = 1 ] && return 0
  local want="yes" answer=""
  [ "$PURGE" = 1 ] && want="purge"
  # A piped script has no stdin to read from; the terminal is asked directly.
  { exec 3<"$CONFIRM_TTY" 4>>"$CONFIRM_TTY"; } 2>/dev/null || die "no terminal to confirm on; re-run with --yes"
  printf 'Type "%s" to continue: ' "$want" >&4
  read -r answer <&3 || true
  exec 3<&- 4>&-
  [ "$answer" = "$want" ] || die "not confirmed; nothing was changed"
}

print_plan() {
  log "this will remove from $(uname -n):"
  log "  the felis-* systemd units, cloudflared-felis.service, the ${VELOCITY_USER} user,"
  log "  ${OPT_DIR}, ${HOST_BIN}, the felis_postgres and felis_edge nftables tables and the firewalld openings"
  case "$K3S_MODE" in
    remove) log "  k3s, with everything in it (${K3S_BIN_DIR}/k3s-uninstall.sh)" ;;
    keep) log "  Felis's namespaces (${FELIS_NAMESPACES[*]}) and the ${FELIS_CRD} CRD; k3s stays" ;;
    absent) ;;
  esac
  if [ "$PURGE" = 1 ]; then
    log "  PURGE: ${STATE_DIR} (secrets), ${DATA_DIR} (the ${DB_NAME} database's cluster, the database"
    log "  bundles and anything set aside before), the ${DB_NAME} database and role in a host PostgreSQL,"
    log "  every world and archive, the Felis images and Docker's build cache"
  else
    [ "$BACKUP" = 1 ] && log "  after a final database bundle into ${DATA_DIR}/db-backups"
    log "  kept: ${STATE_DIR}, ${DATA_DIR} (the ${DB_NAME} database in ${PG_DATA_DIR}); the volumes move to ${RETAIN_DIR}/"
  fi
}

final_backup() {
  [ "$PURGE" = 0 ] && [ "$BACKUP" = 1 ] || return 0
  [ -x "$HOST_BIN" ] && [ -r "${STATE_DIR}/felis.host.toml" ] || {
    warn "no ${HOST_BIN} or ${STATE_DIR}/felis.host.toml; skipping the final database bundle"
    return 0
  }
  log "taking a final database bundle"
  "$HOST_BIN" db backup -config "${STATE_DIR}/felis.host.toml" -label manual \
    || die "the final database bundle failed, so nothing was removed. Fix the database (sudo felis db check), or pass --no-backup to go on without one"
  ok "database bundle written to ${DATA_DIR}/db-backups"
}

# game_port reads the proxy's port from velocity.toml before /opt/felis goes.
game_port() {
  local toml="${OPT_DIR}/velocity/velocity.toml" port=""
  [ -r "$toml" ] && port="$(sed -n 's/^bind *= *"[^"]*:\([0-9][0-9]*\)".*/\1/p' "$toml" | head -n 1)"
  printf '%s\n' "${FELIS_GAME_PORT:-${port:-25565}}"
}

# tunnel_config reads the cloudflared config felis setup pointed its unit at (by default
# /etc/felis/cloudflared.yml) before the unit goes.
tunnel_config() {
  local unit="${UNIT_DIR}/cloudflared-felis.service" path=""
  [ -r "$unit" ] && path="$(sed -n 's/^ExecStart=.* --config \([^ ]*\) tunnel run$/\1/p' "$unit" | head -n 1)"
  printf '%s\n' "${path:-${STATE_DIR}/cloudflared.yml}"
}

# nano_port reads felis-nano's port from its unit before the unit goes.
nano_port() {
  local unit="${UNIT_DIR}/felis-nano.service"
  [ -r "$unit" ] || return 0
  sed -n 's/^ExecStart=.* -listen [^ ]*:\([0-9][0-9]*\).*$/\1/p' "$unit" | head -n 1
}

remove_units() {
  local unit removed=0
  for unit in "${FELIS_UNITS[@]}"; do
    [ -f "${UNIT_DIR}/${unit}" ] || continue
    systemctl disable --now "$unit" >/dev/null 2>&1 || systemctl stop "$unit" >/dev/null 2>&1 || true
    rm -f "${UNIT_DIR}/${unit}"
    removed=$((removed + 1))
  done
  systemctl daemon-reload
  systemctl reset-failed >/dev/null 2>&1 || true
  ok "${removed} systemd unit(s) removed"
}

remove_nft_tables() {
  command -v nft >/dev/null 2>&1 || return 0
  local t
  for t in felis_postgres felis_edge; do
    if nft list table inet "$t" >/dev/null 2>&1; then
      nft delete table inet "$t"
      ok "nftables table inet ${t} removed"
    fi
  done
}

# remove_firewalld_rules takes back what configure_k3s_firewall, configure_velocity_firewall
# and the nano setup opened. The k3s ones stay when k3s does.
remove_firewalld_rules() { # game-port nano-port
  command -v firewall-cmd >/dev/null 2>&1 || return 0
  systemctl is-active --quiet firewalld || return 0
  local game="$1" nano="$2" port rule changed=0
  local ports=("${game}/tcp" "${FELIS_PANEL_NODEPORT}/tcp")
  [ -n "$nano" ] && ports+=("${nano}/tcp")
  [ "$K3S_MODE" = remove ] && ports+=("6443/tcp")
  for port in "${ports[@]}"; do
    if firewall-cmd --permanent --query-port="$port" >/dev/null 2>&1; then
      firewall-cmd --permanent --remove-port="$port" >/dev/null
      changed=1
    fi
  done
  if [ -n "$nano" ]; then
    while IFS= read -r rule; do
      case "$rule" in
        *"port=\"${nano}\""*) firewall-cmd --permanent --remove-rich-rule="$rule" >/dev/null; changed=1 ;;
      esac
    done < <(firewall-cmd --permanent --list-rich-rules 2>/dev/null)
  fi
  if [ "$K3S_MODE" = remove ]; then
    local cidr
    for cidr in "$POD_CIDR" "$SERVICE_CIDR"; do
      if firewall-cmd --permanent --zone=trusted --query-source="$cidr" >/dev/null 2>&1; then
        firewall-cmd --permanent --zone=trusted --remove-source="$cidr" >/dev/null
        changed=1
      fi
    done
  fi
  if [ "$changed" = 1 ]; then
    firewall-cmd --reload >/dev/null
    ok "firewalld openings removed"
  fi
}

# retain_volumes_in_cluster keeps every volume Felis's claims are bound to when the
# namespaces go: local-path deletes a Delete-policy volume's directory with its claim.
retain_volumes_in_cluster() {
  local pv
  while IFS= read -r pv; do
    [ -n "$pv" ] || continue
    kube patch pv "$pv" -p '{"spec":{"persistentVolumeReclaimPolicy":"Retain"}}' >/dev/null
  done < <(kube get pv -o 'jsonpath={range .items[*]}{.metadata.name} {.spec.claimRef.namespace}{"\n"}{end}' \
    | awk '$2 == "felis" || $2 == "minecraft" || $2 == "felis-build" { print $1 }')
  ok "Felis's volumes set to Retain; their directories stay under ${K3S_STORAGE}"
}

remove_from_cluster() {
  [ "$PURGE" = 1 ] || retain_volumes_in_cluster
  log "deleting Felis's namespaces and CRD"
  # The operator is part of what goes, so nothing would clear a MinecraftServer finalizer
  # and the minecraft namespace would stay Terminating. Drop them first.
  local s
  while IFS= read -r s; do
    [ -n "$s" ] || continue
    kube -n minecraft patch minecraftserver "$s" --type=merge -p '{"metadata":{"finalizers":null}}' >/dev/null 2>&1 || true
  done < <(kube -n minecraft get minecraftservers -o 'jsonpath={range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null || true)
  kube delete namespace "${FELIS_NAMESPACES[@]}" --ignore-not-found --wait=true --timeout=300s >/dev/null \
    || warn "a namespace is still terminating; check: k3s kubectl get namespaces"
  kube delete crd "$FELIS_CRD" --ignore-not-found >/dev/null || true
  if [ "$PURGE" = 1 ]; then
    local pv
    while IFS= read -r pv; do
      [ -n "$pv" ] && kube delete pv "$pv" --ignore-not-found >/dev/null
    done < <(kube get pv -o 'jsonpath={range .items[*]}{.metadata.name} {.spec.claimRef.namespace}{"\n"}{end}' \
      | awk '$2 == "felis" || $2 == "minecraft" || $2 == "felis-build" { print $1 }')
  fi
  if [ -f "$K3S_REGISTRIES" ] && grep -q 'registry\.felis\.svc' "$K3S_REGISTRIES"; then
    rm -f "$K3S_REGISTRIES"
    systemctl restart k3s
  fi
  ok "Felis removed from the cluster; k3s stays"
}

# stop_database_pod shuts felis-postgres down cleanly: k3s-killall.sh SIGKILLs every
# container, and the cluster it leaves in PG_DATA_DIR is what a reinstall starts from.
stop_database_pod() {
  kube -n "$CONTROL_NS" scale deployment "$PG_DEPLOYMENT" --replicas=0 >/dev/null 2>&1 || return 0
  kube -n "$CONTROL_NS" wait --for=delete pod -l app.kubernetes.io/name=felis,app.kubernetes.io/component=postgres \
    --timeout=120s >/dev/null 2>&1 \
    || warn "${PG_DEPLOYMENT} did not stop within 2 minutes; its cluster recovers from its WAL on the next start"
}

remove_k3s() {
  local stamp
  [ "$PURGE" = 1 ] || stop_database_pod
  if [ "$PURGE" = 0 ] && [ -d "$K3S_STORAGE" ]; then
    # k3s-killall.sh stops every pod and unmounts their volumes, so nothing is writing a
    # world while it moves.
    "${K3S_BIN_DIR}/k3s-killall.sh" >/dev/null 2>&1 || systemctl stop k3s
    stamp="$(date -u +%Y%m%dT%H%M%SZ)"
    install -d -m 0700 "$RETAIN_DIR"
    mv "$K3S_STORAGE" "${RETAIN_DIR}/k3s-storage-${stamp}"
    ok "volumes moved to ${RETAIN_DIR}/k3s-storage-${stamp}"
  fi
  if [ -x "${K3S_BIN_DIR}/k3s-uninstall.sh" ]; then
    log "running k3s-uninstall.sh"
    "${K3S_BIN_DIR}/k3s-uninstall.sh" >/dev/null 2>&1 || warn "k3s-uninstall.sh reported an error; check /var/lib/rancher and /etc/rancher"
    ok "k3s removed"
  else
    warn "k3s is at ${K3S_BIN_DIR}/k3s but ${K3S_BIN_DIR}/k3s-uninstall.sh is missing; remove k3s by hand"
  fi
}

# remove_cloudflared_binary deletes the binary the installer put in /usr/local/bin, unless
# a unit other than Felis's still runs it.
remove_cloudflared_binary() {
  [ -x "$CLOUDFLARED_BIN" ] || return 0
  local others
  others="$(grep -ls "$CLOUDFLARED_BIN" "${UNIT_DIR}"/*.service /lib/systemd/system/*.service /usr/lib/systemd/system/*.service 2>/dev/null || true)"
  if [ -n "$others" ]; then
    log "leaving ${CLOUDFLARED_BIN}: $(printf '%s' "$others" | tr '\n' ' ')uses it"
    return 0
  fi
  rm -f "$CLOUDFLARED_BIN"
  ok "${CLOUDFLARED_BIN} removed"
}

remove_host_files() {
  if id "$VELOCITY_USER" >/dev/null 2>&1; then
    userdel "$VELOCITY_USER" >/dev/null 2>&1 || warn "could not remove the ${VELOCITY_USER} user"
  fi
  rm -rf "$OPT_DIR"
  rm -f "$HOST_BIN" "${HOST_BIN}.new" "${HOST_BIN}.prev"
  remove_cloudflared_binary
  if [ "$PURGE" = 0 ]; then
    # What describes the removed install goes; what a reinstall reuses stays. Without
    # bootstrap.done the next run takes the first-install path.
    rm -f "${STATE_DIR}/bootstrap.done" "${STATE_DIR}/system-server-images" \
      "${STATE_DIR}/velocity.fingerprint" "${STATE_DIR}/previous-felis-image"
  fi
  ok "${OPT_DIR} and ${HOST_BIN} removed"
}

as_postgres() { (cd / && runuser -u postgres -- "$@"); }

# remove_hba_block drops the block bootstrap heads pg_hba.conf with (the rules of an
# install on the host server, or the lockout the move into k3s left), and nothing else.
remove_hba_block() { # file
  local tmp
  tmp="$(mktemp)"
  awk '
    $0 == "# BEGIN FELIS MANAGED HBA" { skip = 1; next }
    $0 == "# END FELIS MANAGED HBA" { skip = 0; blank = 1; next }
    blank && $0 == "" { blank = 0; next }
    { blank = 0 }
    !skip { print }
  ' "$1" > "$tmp"
  cat "$tmp" > "$1"
  rm -f "$tmp"
}

# check_database_purge runs before anything is removed. DROP ROLE refuses a role that
# still owns a database or holds anything in one besides felis (the felis_pgint database
# CONTRIBUTING.md has developers make for the PG contract tests, a grant made by hand),
# and by the time purge_database runs the units and k3s are already gone. It lists what
# holds the role instead, so the purge either runs to the end or not at all.
check_database_purge() {
  [ "$PURGE" = 1 ] || return 0
  systemctl is-active --quiet postgresql 2>/dev/null || return 0
  local sql held err
  read -r -d '' sql <<SQL || true
WITH r AS (SELECT oid FROM pg_roles WHERE rolname = '${DB_USER}'),
     f AS (SELECT oid FROM pg_database WHERE datname = '${DB_NAME}')
SELECT DISTINCT CASE
    WHEN s.classid = 'pg_database'::regclass THEN 'database ' || (SELECT datname FROM pg_database WHERE oid = s.objid)
    WHEN s.classid = 'pg_tablespace'::regclass THEN 'tablespace ' || (SELECT spcname FROM pg_tablespace WHERE oid = s.objid)
    ELSE 'objects in database ' || (SELECT datname FROM pg_database WHERE oid = s.dbid)
  END || CASE s.deptype WHEN 'o' THEN ' (owned)' ELSE ' (privileges)' END
FROM pg_shdepend s
WHERE s.refclassid = 'pg_authid'::regclass AND s.refobjid = (SELECT oid FROM r)
  AND s.dbid IS DISTINCT FROM (SELECT oid FROM f)
  AND NOT (s.classid = 'pg_database'::regclass AND s.objid IS NOT DISTINCT FROM (SELECT oid FROM f))
ORDER BY 1;
SQL
  err="$(mktemp)"
  if ! held="$(as_postgres psql -v ON_ERROR_STOP=1 -tAq 2>"$err" <<<"$sql")"; then
    held="$(cat "$err")"
    rm -f "$err"
    die "could not ask PostgreSQL what the ${DB_USER} role still holds, so nothing was removed: ${held}"
  fi
  rm -f "$err"
  [ -n "$held" ] || return 0
  die "the ${DB_USER} role still holds $(printf '%s' "$held" | paste -sd ';' - | sed 's/;/; /g'), so DROP ROLE would fail halfway through the purge; nothing was removed. Hand them to postgres first (sudo -u postgres psql -c 'ALTER DATABASE <name> OWNER TO postgres', or REASSIGN OWNED BY ${DB_USER} TO postgres; DROP OWNED BY ${DB_USER}; inside that database), or rerun without --purge"
}

# purge_database drops the felis database and role from a host PostgreSQL an earlier
# release installed; the cluster felis-postgres runs goes with DATA_DIR (purge_state). That
# host server either still serves the platform, or the move into felis-postgres stopped it
# with the pre-move copy left in it for a rollback: a purge takes that copy too and leaves
# the server stopped. The units and k3s are gone by now, so a failure here is a warning
# with the commands to finish by hand, and the purge goes on.
purge_database() {
  [ "$PURGE" = 1 ] || return 0
  local hba started=0
  if ! systemctl is-active --quiet postgresql 2>/dev/null; then
    [ -e "$PG_MOVED_MARKER" ] && systemctl cat postgresql >/dev/null 2>&1 || return 0
    if ! systemctl start postgresql >/dev/null 2>&1; then
      warn "could not start the host PostgreSQL, so its copy of the ${DB_NAME} database from before the move into k3s stays in it; drop it once it runs: sudo -u postgres dropdb ${DB_NAME}; sudo -u postgres dropuser ${DB_USER}"
      return 0
    fi
    started=1
  fi
  hba="$(as_postgres psql -tAc 'SHOW hba_file;' 2>/dev/null || true)"
  if ! as_postgres psql -v ON_ERROR_STOP=1 -q <<SQL
SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '${DB_NAME}' AND pid <> pg_backend_pid();
DROP DATABASE IF EXISTS ${DB_NAME};
DROP ROLE IF EXISTS ${DB_USER};
ALTER SYSTEM RESET listen_addresses;
SQL
  then
    [ "$started" = 0 ] || systemctl stop postgresql >/dev/null 2>&1 || true
    warn "could not drop the ${DB_NAME} database and role from the host PostgreSQL; drop them by hand: sudo -u postgres dropdb ${DB_NAME}; sudo -u postgres dropuser ${DB_USER}"
    return 0
  fi
  if [ -n "$hba" ] && [ -f "$hba" ]; then
    remove_hba_block "$hba"
    rm -f "${hba}.pre-pg-move"
  fi
  if [ "$started" = 1 ]; then
    systemctl stop postgresql
    ok "the host PostgreSQL's copy of '${DB_NAME}' from before the move into k3s dropped; the server stays stopped"
  else
    systemctl restart postgresql
    ok "database and role '${DB_NAME}' dropped; PostgreSQL listens on its default address again"
  fi
}

purge_images() {
  [ "$PURGE" = 1 ] || return 0
  command -v docker >/dev/null 2>&1 || return 0
  # The installer stops Docker after its builds; start it just long enough to clean up.
  local was_active=1 refs
  systemctl is-active --quiet docker || { was_active=0; systemctl start docker >/dev/null 2>&1 || return 0; }
  refs="$(docker image ls --format '{{.Repository}}:{{.Tag}}' | grep -E '^(registry\.felis\.svc:5000/felis/|felis/)' || true)"
  if [ -n "$refs" ]; then
    # shellcheck disable=SC2086 # one ref per word
    docker image rm -f $refs >/dev/null 2>&1 || true
  fi
  docker builder prune -af >/dev/null 2>&1 || true
  [ "$was_active" = 1 ] || systemctl stop docker docker.socket >/dev/null 2>&1 || true
  ok "Felis images and Docker's build cache removed"
}

purge_state() {
  [ "$PURGE" = 1 ] || return 0
  # felis setup's tunnel credentials sit beside cloudflared's login (cert.pem, which stays:
  # it is the Cloudflare account's, not Felis's). The tunnel itself lives on in the account
  # until it is deleted there (docs/operations.md).
  local cred=""
  [ -r "$TUNNEL_CONFIG" ] \
    && cred="$(sed -n 's/^credentials-file: *"\{0,1\}\([^"]*\)"\{0,1\} *$/\1/p' "$TUNNEL_CONFIG" | head -n 1)"
  if [ -n "$cred" ]; then rm -f "$cred"; fi
  rm -f "$TUNNEL_CONFIG"
  # The file-context rule bootstrap gave the database's cluster directory.
  if command -v semanage >/dev/null 2>&1; then
    semanage fcontext -d "${PG_DATA_DIR}(/.*)?" >/dev/null 2>&1 || true
  fi
  rm -rf "$STATE_DIR" "$DATA_DIR"
  ok "${STATE_DIR} and ${DATA_DIR} removed"
}

main() {
  parse_args "$@"
  [ "$(id -u)" = 0 ] || die "run as root: sudo bash $0"
  [ -e "$STATE_DIR" ] || [ -e "$HOST_BIN" ] || [ -e "$OPT_DIR" ] \
    || die "no Felis install here (${STATE_DIR}, ${HOST_BIN} and ${OPT_DIR} are all absent)"
  decide_k3s
  check_database_purge
  print_plan
  confirm
  final_backup

  local game nano
  game="$(game_port)"
  nano="$(nano_port)"
  TUNNEL_CONFIG="$(tunnel_config)"
  remove_units
  case "$K3S_MODE" in
    remove) remove_k3s ;;
    keep) remove_from_cluster ;;
  esac
  remove_nft_tables
  remove_firewalld_rules "$game" "$nano"
  remove_host_files
  purge_database
  purge_images
  purge_state

  if [ "$PURGE" = 1 ]; then
    ok "Felis is gone from this host"
  else
    ok "Felis is removed; the data stays in ${STATE_DIR} and ${DATA_DIR}, the ${DB_NAME} database in ${PG_DATA_DIR}"
    log "reinstalling reuses it: see docs/operations.md, \"Reinstall on top of kept data\""
  fi
}

if [ "${FELIS_UNINSTALL_SOURCED:-0}" != 1 ]; then
  main "$@"
fi
