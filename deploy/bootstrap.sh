#!/usr/bin/env bash
# Usage: curl -fsSL <raw-url>/deploy/bootstrap.sh | sudo [VAR=value ...] bash

set -Eeuo pipefail

FELIS_REPO_URL="${FELIS_REPO_URL:-https://github.com/FelisMC/Felis.git}"
FELIS_VERSION_BOOTSTRAP="${FELIS_VERSION_BOOTSTRAP:-release}"
# A set ref skips channel resolution and the tag lookup, so the stamp is v0.0.0+g<sha>.
FELIS_REF="${FELIS_REF:-}"
# Recorded before resolve_install_ref overwrites FELIS_REF; a pinned ref forces the source build.
FELIS_REF_PINNED=""
if [ -n "$FELIS_REF" ]; then FELIS_REF_PINNED=1; fi
FELIS_RELEASE="${FELIS_RELEASE:-}"
FELIS_ARTIFACT_DIR="${FELIS_ARTIFACT_DIR:-}"
# Set once a prebuilt felis binary is installed at HOST_BIN (TUI hand-off, release download or FELIS_ARTIFACT_DIR).
HAVE_PREBUILT_BINARY=""
# Control-plane image before this run moved it (deploy_bundle); used for the rollback hint.
PREVIOUS_FELIS_IMAGE=""
# Set by select_release_artifacts: "dir" (FELIS_ARTIFACT_DIR), "release" (ARTIFACT_TAG assets cached
# in ARTIFACT_CACHE) or empty (everything is built here).
ARTIFACT_MODE=""
ARTIFACT_TAG=""
# Root-only and outside /tmp: an image bundle waits here between import and push, so a failed run can resume.
ARTIFACT_CACHE="/var/lib/felis/artifacts"
# SHA256SUMS every artifact is checked against, and the verified local copy artifact_fetch last returned.
ARTIFACT_SUMS=""
ARTIFACT_FILE=""
# Validated image-listing lines ("bundle role name manifest-digest config-digest"); state is "", "ok" or "bad".
RELEASE_LISTING=""
RELEASE_LISTING_STATE=""
# "role bundle name target manifest-digest config-digest" per image taken from a bundle; other images are built.
RELEASE_IMAGES=""
PREBUILT_ROLES=" "
DOCKER_INSTALLED=""
# Release JSON cached per tag: unauthenticated GitHub API calls are limited to 60 an hour.
RELEASE_JSON_TAG=""
RELEASE_JSON=""
# Exported for git's credential helper; never put it in the clone URL, where it would persist in .git/config.
FELIS_GITHUB_TOKEN="${FELIS_GITHUB_TOKEN:-}"
export FELIS_GITHUB_TOKEN
# Set by resolve_install_ref/stamp_version and linked into the binary as main.version.
FELIS_VERSION=""
FELIS_VERSION_BASE=""
# Registry that holds every built image so kubelet can re-pull after image GC; pushes use the loopback
# REGISTRY_PUSH_HOST (docker treats 127.0.0.1 as insecure, so no insecure-registries entry).
REGISTRY_URL="registry.felis.svc:5000"
REGISTRY_PUSH_HOST="127.0.0.1:${REGISTRY_URL##*:}"
# Empty unless set: resolve_felis_image derives the tag from the installed version.
FELIS_IMAGE="${FELIS_IMAGE:-}"
FELIS_EGRESS_MODE="${FELIS_EGRESS_MODE:-nodeport}"
FELIS_PANEL_NODEPORT="${FELIS_PANEL_NODEPORT:-30443}"
# The archive path goes into felis.toml and must equal the Jobs' mount path, or archive refs do not resolve.
FELIS_BACKUP_PVC="${FELIS_BACKUP_PVC:-felis-backups}"
FELIS_ARCHIVE_LOCAL_PATH="${FELIS_ARCHIVE_LOCAL_PATH:-/var/lib/felis/archives}"
# Empty keeps the `felis manifests` defaults.
FELIS_REGISTRY_STORAGE="${FELIS_REGISTRY_STORAGE:-}"
FELIS_UPLOADS_STORAGE="${FELIS_UPLOADS_STORAGE:-}"
FELIS_BACKUP_STORAGE="${FELIS_BACKUP_STORAGE:-}"
FELIS_ARTIFACTS_STORAGE="${FELIS_ARTIFACTS_STORAGE:-}"
# Opt-in because the reaper then deletes worlds after a verified archive; unset renders it retention-only (expired backups).
FELIS_WORLDS_HOST_PATH="${FELIS_WORLDS_HOST_PATH:-}"
# k3s local-path provisioner root; it appears with the first volume, after the reaper's PV is applied.
K3S_STORAGE_ROOT="/var/lib/rancher/k3s/storage"
# Panel-owned settings (written by node-control) become defaults; a variable set for the run wins.
HOST_SETTINGS_FILE="/etc/felis/host-settings.env"
recall_host_settings() {
  [ -r "$HOST_SETTINGS_FILE" ] || return 0
  local key value
  while IFS="=" read -r key value; do
    case "$key" in
      FELIS_DB_BACKUP_TIME|FELIS_DB_BACKUP_KEEP|FELIS_VELOCITY_XMX) ;;
      *) continue ;;
    esac
    [ -z "${!key+x}" ] || continue
    value="${value#\'}"
    value="${value%\'}"
    printf -v "$key" "%s" "$value"
  done < "$HOST_SETTINGS_FILE"
}
recall_host_settings

# Daily db backups and pre-migration snapshots (felis db backup); outside /var/lib/rancher so a k3s reinstall keeps them.
FELIS_DB_BACKUP_DIR="${FELIS_DB_BACKUP_DIR:-/var/lib/felis/db-backups}"
FELIS_DB_BACKUP_KEEP="${FELIS_DB_BACKUP_KEEP:-14}"
FELIS_DB_BACKUP_TIME="${FELIS_DB_BACKUP_TIME:-*-*-* 03:30:00}"
# node-exporter textfile collector target; FelisDBBackupStale (deploy/alerts) reads it.
FELIS_DB_BACKUP_METRICS="${FELIS_DB_BACKUP_METRICS:-/var/lib/node_exporter/textfile_collector/felis_db_backup.prom}"
# 0 skips the pre-migration snapshot; otherwise a failed snapshot stops the upgrade.
FELIS_PRE_MIGRATE_BACKUP="${FELIS_PRE_MIGRATE_BACKUP:-1}"
# Off-site S3 copy (felis offsite); credentials and key go to /etc/felis/offsite.env (0600).
# A rerun without these keeps the [offsite] already in felis.host.toml.
FELIS_OFFSITE_ENDPOINT="${FELIS_OFFSITE_ENDPOINT:-}"
FELIS_OFFSITE_BUCKET="${FELIS_OFFSITE_BUCKET:-}"
RESTORE_FROM_OFFSITE="${FELIS_RESTORE_FROM_OFFSITE:-0}"
RESTORE_BUNDLE="${FELIS_RESTORE_BUNDLE:-}"
RESTORE_TAKE_OVER="${FELIS_RESTORE_TAKE_OVER:-0}"
FELIS_OFFSITE_REGION="${FELIS_OFFSITE_REGION:-}"
FELIS_OFFSITE_PREFIX="${FELIS_OFFSITE_PREFIX:-}"
FELIS_OFFSITE_DB_KEEP="${FELIS_OFFSITE_DB_KEEP:-}"
# Dead man's switch ping URL, kept in /etc/felis/watchdog-heartbeat-url (0600; its path is the secret).
# "off" removes it; a rerun without it keeps it.
FELIS_WATCHDOG_HEARTBEAT_URL="${FELIS_WATCHDOG_HEARTBEAT_URL:-}"
INSTALL_MODE="${FELIS_INSTALL_MODE:-}"
DISTRIBUTED="${FELIS_DISTRIBUTED:-0}"
WORKER_NAME="${FELIS_NODE_NAME:-}"
WORKER_SERVER="${FELIS_SERVER_URL:-}"
WORKER_TOKEN_FILE="${FELIS_BOOTSTRAP_TOKEN_FILE:-}"
WORKER_REGISTRY_IP="${FELIS_REGISTRY_CLUSTER_IP:-}"
NODE_EXTERNAL_IP="${FELIS_NODE_EXTERNAL_IP:-}"
WORKER_PEERS="${FELIS_PEER_CIDRS:-}"
# Peer list last installed by `felis node firewall`; a rerun passing no FELIS_PEER_CIDRS keeps it.
NODE_PEERS_FILE="/etc/felis/node-peers"
# HA persists across reruns; the optional VRRP address requires controllers on one shared network.
CONTROLLER_HA="${FELIS_CONTROLLER_HA:-}"
HA_ENV="/etc/felis/ha.env"
HA_HOME=""
HA_JOIN_SERVER=""
HA_VRRP_PASS="/etc/felis/ha-vrrp-pass"
KEEPALIVED_CONF="/etc/keepalived/keepalived.conf"
FELIS_LONGHORN_VERSION="${FELIS_LONGHORN_VERSION:-v1.8.1}"
LONGHORN_DISK_LABEL="node.longhorn.io/create-default-disk"
REPLICATED_CLASS="felis-replicated"
FELIS_HA_VIP="${FELIS_HA_VIP:-}"
FELIS_HA_VIP_INTERFACE="${FELIS_HA_VIP_INTERFACE:-}"
FELIS_HA_VRID="${FELIS_HA_VRID:-51}"
# Inputs set by felis node join-controller.
SERVER_TOKEN_FILE="${FELIS_SERVER_TOKEN_FILE:-}"
AGENT_TOKEN_FILE="${FELIS_AGENT_TOKEN_FILE:-}"
ETC_BUNDLE="${FELIS_ETC_BUNDLE:-}"
CONTROLLER_PEERS="${FELIS_CONTROLLER_PEERS:-}"
FELIS_PREFLIGHT="${FELIS_PREFLIGHT:-strict}"
# hasJoined is unauthenticated, so a public bind is a free auth relay: loopback is the default.
# Left empty here; resolve_nano_listen applies the default after an installed unit's address.
FELIS_NANO_LISTEN="${FELIS_NANO_LISTEN:-}"
FELIS_NANO_PROXY_CIDR="${FELIS_NANO_PROXY_CIDR:-}"
# Backends that get their identity through the handshake address (see write_velocity_service);
# read at proxy restart, not live from the CRs.
FELIS_LEGACY_FORWARDING_SERVERS="${FELIS_LEGACY_FORWARDING_SERVERS:-legacy18}"
FELIS_VELOCITY_XMX="${FELIS_VELOCITY_XMX:-1G}"
# Pinned go.dev sha256 per architecture; move the version and both digests together.
GO_PINNED_VERSION="1.27.3"
GO_PINNED_SHA256_AMD64="65efbc80fdc1fa2cd2631cad5b481777a68e8725f34e89f50fc6ac714e379b40"
GO_PINNED_SHA256_ARM64="a06e99309dbcbdca1a8ccd2282c3ee146e55930886e26b4f1193269587165f49"
FELIS_GO_VERSION="${FELIS_GO_VERSION:-$GO_PINNED_VERSION}"
FELIS_GO_SHA256="${FELIS_GO_SHA256:-}"
# Pinned release and the sha256 GitHub lists per asset; an installed binary is replaced only under FELIS_UPGRADE_DEPS=1.
CLOUDFLARED_PINNED_VERSION="2026.9.1"
CLOUDFLARED_PINNED_SHA256_AMD64="03f1f25d1cc93b9ad6c60569d44060bc4f17ed97075760ed8cfca4b12dcd68cc"
CLOUDFLARED_PINNED_SHA256_ARM64="3d97437c71848bd8df68041e12436b484a661d95073ea1937f01a845ce88faa3"
CLOUDFLARED_PINNED_SHA256_ARM="093ffa3638ab2b636de63c43a8c68f96a69cf71f9699dd8277a91b160b0f4fc0"
FELIS_CLOUDFLARED_VERSION="${FELIS_CLOUDFLARED_VERSION:-$CLOUDFLARED_PINNED_VERSION}"
FELIS_CLOUDFLARED_SHA256="${FELIS_CLOUDFLARED_SHA256:-}"
CLOUDFLARED_BIN=/usr/local/bin/cloudflared
# Also the tag the k3s install script is read from; that script checks the binary against the release's sha256sum file.
FELIS_K3S_VERSION="${FELIS_K3S_VERSION:-v1.36.4+k3s1}"
FELIS_UPGRADE_DEPS="${FELIS_UPGRADE_DEPS:-0}"
FELIS_MANAGE_TIME_SYNC="${FELIS_MANAGE_TIME_SYNC:-1}"
FELIS_MANAGE_JOURNAL="${FELIS_MANAGE_JOURNAL:-1}"
FELIS_JOURNAL_MAX_USE="${FELIS_JOURNAL_MAX_USE:-1G}"
# Must equal platform.defaultRegistryImage (TestBootstrapPinsTheRegistryImage); containerd caches the same ref.
REGISTRY_IMAGE="docker.io/library/registry:2.8.3@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373"
# Must equal platform.defaultPostgresImage (TestBootstrapPinsThePostgresImage); a new major needs a dump and restore (check_postgres_major).
POSTGRES_IMAGE="docker.io/library/postgres:18.6-trixie@sha256:5a5a84b19854a9ffaa54082c166ff4ec27473a361e496e5ea167f298f2da9722"
PKG_LOCK_TIMEOUT="${PKG_LOCK_TIMEOUT:-${APT_LOCK_TIMEOUT:-900}}"
APT_LOCK_TIMEOUT="${APT_LOCK_TIMEOUT:-$PKG_LOCK_TIMEOUT}"

FELIS_LIMBO_IMAGE="${FELIS_LIMBO_IMAGE:-${REGISTRY_URL}/felis/limbo:demo}"
FELIS_LOBBY_IMAGE="${FELIS_LOBBY_IMAGE:-${REGISTRY_URL}/felis/lobby:demo}"
# Plain Paper base for user servers (seeded in 0019_recommended_paper.sql); needs no secret.
FELIS_PAPER_IMAGE="${FELIS_PAPER_IMAGE:-${REGISTRY_URL}/felis/paper:demo}"
# Resolve builds within the selected minor line to avoid adopting an incompatible unreleased protocol.
FELIS_VELOCITY_VERSION="${FELIS_VELOCITY_VERSION:-}"
VELOCITY_LATEST_MINOR="3.5.1"
FELIS_GAME_STACK="${FELIS_GAME_STACK:-pinned}"
# Opt-in: the fork is unmeasured against real Mojang sessions, which it would carry.
FELIS_VELOCITY_FORK_JAR="${FELIS_VELOCITY_FORK_JAR:-}"
# Required with the jar; case and spaces ignored. Not a signature: it pins the path to one build and is re-checked every run.
FELIS_VELOCITY_FORK_JAR_SHA256="${FELIS_VELOCITY_FORK_JAR_SHA256:-}"
FELIS_JRE_VERSION="${FELIS_JRE_VERSION:-25}"
# Pinned build and Adoptium sha256 per architecture (/v3/assets/latest/25/hotspot?image_type=jre&os=linux); move together.
JRE_PINNED_FEATURE="25"
JRE_PINNED_RELEASE="25.0.4.1+1"
JRE_PINNED_SHA256_X64="1731a34baadec5479258ea0202e4d5d865d2efeee60cb0c7d7eb056fe96ca219"
JRE_PINNED_SHA256_AARCH64="34828cbb93ed31c281c84ecb31ddab655d11a802f263c1fc019d42e9e0230fed"
# The only Minecraft port players reach; backends are ClusterIP-only and verify the modern-forwarding HMAC.
FELIS_GAME_PORT="${FELIS_GAME_PORT:-25565}"
# Must match internal/naming SystemLoginServer/SystemLobbyServer.
LOGIN_SERVER="login"
LOBBY_SERVER="lobby"

CONTROL_NS="felis"
MINECRAFT_NS="minecraft"
BUILD_NS="felis-build"
POD_CIDR="10.42.0.0/16"          # k3s default cluster CIDR
SERVICE_CIDR="10.43.0.0/16"      # k3s default service CIDR
DB_NAME="felis"
DB_USER="felis"

STATE_DIR="/etc/felis"
SECRETS_ENV="${STATE_DIR}/secrets.env"
BOOTSTRAP_DONE="${STATE_DIR}/bootstrap.done"
RESTORE_STATE_DIR="/var/lib/felis/restore-host"
PANEL_TLS_CERT="${STATE_DIR}/panel-tls.crt"
PANEL_TLS_KEY="${STATE_DIR}/panel-tls.key"
SRC_DIR="/opt/felis/src"
HOST_BIN="/usr/local/bin/felis"
# Binary this run replaced (keep_previous_host_binary); once migrations start or nano restarts
# onto the new one, a failed run keeps the new binary.
HOST_BIN_PREV=""
HOST_BIN_KEPT=0
HOST_BIN_IN_USE=0
# Felis's own toolchain, not /usr/local/go: install_go_toolchain replaces whatever is here.
GOROOT_DIR="/opt/felis/go"
NANO_SERVICE="/etc/systemd/system/felis-nano.service"
DB_BACKUP_SERVICE="/etc/systemd/system/felis-db-backup.service"
DB_BACKUP_TIMER="/etc/systemd/system/felis-db-backup.timer"
WATCHDOG_SERVICE="/etc/systemd/system/felis-watchdog.service"
# OnFailure= of felis-watchdog.service: reports a run that failed (felis watchdog -unit-failed).
WATCHDOG_FAILED_SERVICE="/etc/systemd/system/felis-watchdog-failed.service"
WATCHDOG_TIMER="/etc/systemd/system/felis-watchdog.timer"
UPDATE_CHECK_SERVICE="/etc/systemd/system/felis-update-check.service"
UPDATE_CHECK_TIMER="/etc/systemd/system/felis-update-check.timer"
WATCHDOG_STATE="/var/lib/felis/watchdog/state.json"
OFFSITE_ENV="${STATE_DIR}/offsite.env"
# summary_offsite shows a new off-site key on the terminal only; stdout and stderr end up in install logs.
OFFSITE_KEY_TTY=/dev/tty
SETUP_TTY=/dev/tty
SETUP_CONSOLE=0
# Host copies of `felis setup` credentials, one bare value per file, 0600 (cmd/felis/hostcreds.go); applied as Secrets every run.
SMTP_PASSWORD_FILE="${STATE_DIR}/smtp-password"
UPLOADS_S3_ACCESS_KEY_FILE="${STATE_DIR}/uploads-s3-access-key"
UPLOADS_S3_SECRET_KEY_FILE="${STATE_DIR}/uploads-s3-secret-key"
OFFSITE_SERVICE="/etc/systemd/system/felis-offsite.service"
OFFSITE_TIMER="/etc/systemd/system/felis-offsite.timer"
BUILD_TOOLS_SERVICE="/etc/systemd/system/felis-build-tools.service"
BUILD_TOOLS_TIMER="/etc/systemd/system/felis-build-tools.timer"
BUILD_TOOLS_STATUS="/var/lib/felis/build-tools/status.json"
# While this holds a future Unix time felis watchdog mails nothing; cleanup removes it, the time covers a killed installer.
WATCHDOG_QUIET_FILE="/run/felis/watchdog-quiet-until"
WATCHDOG_HEARTBEAT_FILE="${STATE_DIR}/watchdog-heartbeat-url"
VELOCITY_DIR="/opt/felis/velocity"
VELOCITY_USER="felis-velocity"
VELOCITY_SERVICE="/etc/systemd/system/felis-velocity.service"
# Last-started proxy fingerprint and login/lobby images; a rerun restarts only what changed, as each restart disconnects players.
VELOCITY_FINGERPRINT="${STATE_DIR}/velocity.fingerprint"
SYSTEM_SERVER_IMAGES="${STATE_DIR}/system-server-images"
# felis-postgres constants; must equal internal/platform's (TestBootstrapAgreesWithThePostgresConstants).
PG_DEPLOYMENT="felis-postgres"
PG_CONTAINER="postgres"
PG_SECRET="felis-postgres"
PG_SECRET_KEY="superuser-password"
PG_DATA_DIR="/var/lib/felis/postgres"
PG_UID=999
PG_HOST_PORT=15432
PG_SERVICE_ADDR="${PG_DEPLOYMENT}.${CONTROL_NS}.svc:5432"
# Written once a host PostgreSQL database is moved into felis-postgres (migrate_host_postgres); the move never reruns.
PG_MOVED_MARKER="/var/lib/felis/postgres-moved"
PG_LOCKOUT_LINE="# The felis database moved into k3s (felis-postgres); nothing else may reach this copy."
# Host units that write to the database; the move stops them and restarts the ones that were running.
PG_MOVE_TIMERS=(felis-db-backup felis-offsite felis-update-check felis-watchdog)
# Exact per-table row counts in collation "C" byte order, so two servers' answers compare byte for byte
# (internal/pgint runs it; shell tests stub psql).
PG_TABLE_COUNTS="SELECT c.relname || ' ' || (xpath('/row/n/text()', query_to_xml(format('SELECT count(*) AS n FROM public.%I', c.relname), false, true, '')))[1]::text FROM pg_class c JOIN pg_namespace s ON s.oid = c.relnamespace WHERE s.nspname = 'public' AND c.relkind IN ('r', 'p') ORDER BY c.relname"
PG_MOVE_STAGE=""
PG_MOVE_HBA=""
PG_MOVE_UNITS=()
# nftables rules that guarded a host PostgreSQL's port; the move retires them.
PG_FIREWALL_RULES="${STATE_DIR}/postgres-firewall.nft"
PG_FIREWALL_SERVICE="/etc/systemd/system/felis-postgres-firewall.service"
JRE_DIR="/opt/felis/jre"
# Digest-pinned gradle image shared by the lobby and limbo Dockerfiles and build_velocity_plugin.
PLUGIN_BUILD_IMAGE="gradle:9.8.1-jdk25@sha256:e4f9e3d037cc0c7d8fc42cd97edb976dfd19a5b7314212b88b6de3807d13cd47"
K3S_BIN_DIR="${K3S_BIN_DIR:-/usr/local/bin}"
K3S_BIN="${K3S_BIN_DIR}/k3s"
# k3s containerd mirror config (configure_registry_mirror); these paths are variables so bootstrap_test.sh can use scratch files.
K3S_REGISTRIES_FILE="/etc/rancher/k3s/registries.yaml"
# Drop-in written by write_k3s_config; k3s reads it after the operator's config.yaml.
K3S_CONFIG_DROPIN="/etc/rancher/k3s/config.yaml.d/50-felis.yaml"
K3S_UNIT_FILE="/etc/systemd/system/k3s.service"
# Service environment drop-in (write_k3s_service_dropin); k3s's own installer rewrites the unit and .env, not this.
K3S_SERVICE_DROPIN="/etc/systemd/system/k3s.service.d/50-felis.conf"
K3S_KUBECONFIG="/etc/rancher/k3s/k3s.yaml"
K3S_KUBELET_CERT="/var/lib/rancher/k3s/agent/client-kubelet.crt"
# k3s imports image tarballs from here at start (stage_k3s_airgap_images).
K3S_IMAGES_DIR="/var/lib/rancher/k3s/agent/images"
# ensure_persistent_journal drop-in, and the directory journald creates once the journal is persistent.
JOURNALD_DROPIN="/etc/systemd/journald.conf.d/50-felis.conf"
JOURNAL_DIR="/var/log/journal"
# Non-empty after a cut-off dpkg run (finish_interrupted_dpkg).
DPKG_UPDATES_DIR="/var/lib/dpkg/updates"
# Held for the whole run (acquire_run_lock) so a second installer stops at once.
RUN_LOCK_FILE="/run/felis-bootstrap.lock"
APT_LOCK_FILES=(
  /var/lib/dpkg/lock-frontend
  /var/lib/dpkg/lock
  /var/cache/apt/archives/lock
  /var/lib/apt/lists/lock
)
APT_BACKGROUND_TIMERS=(
  apt-daily.timer
  apt-daily-upgrade.timer
)
APT_BACKGROUND_SERVICES=(
  apt-daily.service
  apt-daily-upgrade.service
  unattended-upgrades.service
)
DNF_BACKGROUND_TIMERS=(
  dnf-makecache.timer
  dnf-automatic.timer
)
DNF_BACKGROUND_SERVICES=(
  dnf-makecache.service
  dnf-automatic.service
)
YUM_BACKGROUND_TIMERS=(
  yum-cron.timer
)
YUM_BACKGROUND_SERVICES=(
  yum-cron.service
)
ZYPPER_BACKGROUND_TIMERS=(
  packagekit-background.timer
)
ZYPPER_BACKGROUND_SERVICES=(
  packagekit.service
)

PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
export PATH

# Disable provenance timestamps for deterministic cached image IDs and unchanged system-server Pods.
export BUILDX_NO_DEFAULT_ATTESTATIONS=1

log()  { printf '\033[1;36m[felis]\033[0m %s\n' "$*"; }
ok()   { printf '\033[1;32m[ ok ]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[warn]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[fail]\033[0m %s\n' "$*" >&2; exit 1; }

TEMP_PATHS=()
DOCKER_CONTAINERS=()
REGISTRY_DOCKER_CONFIG=""
PKG_TIMERS_TO_RESTORE=()

on_error() {
  local line="$1" code="$2"
  warn "bootstrap failed near line ${line} (exit ${code})"
}

cleanup() {
  local status=$? id path
  restore_previous_host_binary "$status"
  if [ "$status" -ne 0 ]; then undo_postgres_move; fi
  resume_package_background_timers
  if command -v docker >/dev/null 2>&1; then
    for id in "${DOCKER_CONTAINERS[@]-}"; do
      [ -n "$id" ] && docker rm "$id" >/dev/null 2>&1 || true
    done
  fi
  for path in "${TEMP_PATHS[@]-}"; do
    [ -n "$path" ] && rm -rf -- "$path" || true
  done
  # A failed install leaves something broken the owners should hear about, so the
  # watchdog speaks again the moment the installer exits, however it exits.
  rm -f -- "$WATCHDOG_QUIET_FILE" 2>/dev/null || true
}

# Preserve the previous binary until migration so host timers cannot run against an incompatible schema.
keep_previous_host_binary() {
  [ "$HOST_BIN_KEPT" = 0 ] || return 0
  HOST_BIN_KEPT=1
  [ -x "$HOST_BIN" ] || return 0
  HOST_BIN_PREV="${HOST_BIN}.prev"
  rm -f "$HOST_BIN_PREV"
  # A copy cut short (a full disk) is no copy: the failed run would install it over the
  # binary it was taken from.
  if ! cp "$HOST_BIN" "$HOST_BIN_PREV"; then
    rm -f "$HOST_BIN_PREV"
    return 1
  fi
}

restore_previous_host_binary() { # exit-status
  [ -n "$HOST_BIN_PREV" ] && [ -f "$HOST_BIN_PREV" ] || return 0
  if [ "$1" -ne 0 ] && [ "$HOST_BIN_IN_USE" != 1 ]; then
    # install(1) onto a fresh file, as everywhere else HOST_BIN is written (SELinux label).
    rm -f "$HOST_BIN"
    if install -m 0755 "$HOST_BIN_PREV" "$HOST_BIN"; then
      command -v restorecon >/dev/null 2>&1 && restorecon "$HOST_BIN" >/dev/null 2>&1 || true
      warn "restored the previous felis binary at ${HOST_BIN}; the database was not migrated, so rerunning the installer picks up where this run stopped"
    else
      warn "could not restore the previous felis binary; it is at ${HOST_BIN_PREV}"
      return 0
    fi
  fi
  rm -f -- "$HOST_BIN_PREV"
}

remember_temp() { TEMP_PATHS+=("$1"); }
remember_container() { DOCKER_CONTAINERS+=("$1"); }
# Restore SELinux path labels after installation rather than retaining temporary-file labels.
restore_label() { if command -v restorecon >/dev/null 2>&1; then restorecon "$1" || true; fi; }

trap 'on_error "$LINENO" "$?"' ERR
trap cleanup EXIT

k3s_cmd() { [ -x "$K3S_BIN" ] || die "k3s binary not found at ${K3S_BIN}"; "$K3S_BIN" "$@"; }

# Hold the run lock until exit to prevent concurrent installer mutations.
# The lock goes with the process, however it ends.
acquire_run_lock() {
  if ! command -v flock >/dev/null 2>&1; then
    warn "flock not found; nothing stops a second installer run beside this one"
    return 0
  fi
  exec 9>"$RUN_LOCK_FILE"
  flock -n 9 || die "another installer run is still going on this host; wait for it to finish, then rerun (ps -ef | grep bootstrap)"
}
kube() { k3s_cmd kubectl "$@"; }

# Create or update a single-key Secret without putting the value in kubectl's argv.
# The temporary file is mode 0600 and is also registered with the EXIT cleanup path.
apply_literal_secret() {
  local namespace="$1" name="$2" key="$3" value="$4" tmp
  tmp="$(umask 077; mktemp)"
  remember_temp "$tmp"
  printf '%s' "$value" > "$tmp"
  kube -n "$namespace" create secret generic "$name" \
    --from-file="${key}=${tmp}" \
    --dry-run=client -o yaml | kube apply -f -
  rm -f "$tmp"
}

# Workload Jobs need namespace-local replicas of configuration Secrets.
# Refresh both configuration replicas on every run; stale values could retain obsolete credentials or policy.
apply_felis_config_secrets() {
  kube -n "$CONTROL_NS" create secret generic felis-config \
    --from-file=felis.toml="${STATE_DIR}/felis.pod.toml" \
    --dry-run=client -o yaml | kube apply -f -
  kube -n "$MINECRAFT_NS" create secret generic felis-config \
    --from-file=felis.toml="${STATE_DIR}/felis.pod.toml" \
    --dry-run=client -o yaml | kube apply -f -
}

# Separate registry gate credentials from build-only push credentials in their respective namespaces.
# Values go through 0600 temp files, never kubectl's argv.
apply_registry_secrets() {
  local dir
  dir="$(umask 077; mktemp -d)"
  remember_temp "$dir"
  printf '%s' "$REGISTRY_PLATFORM_TOKEN" > "${dir}/platform"
  printf '%s' "$REGISTRY_BUILD_TOKEN" > "${dir}/build"
  printf '%s' "$REGISTRY_PRUNE_TOKEN" > "${dir}/prune"
  printf '%s' build > "${dir}/username"
  kube -n "$CONTROL_NS" create secret generic felis-registry-auth \
    --from-file=platform="${dir}/platform" \
    --from-file=build="${dir}/build" \
    --from-file=prune="${dir}/prune" \
    --dry-run=client -o yaml | kube apply -f -
  kube -n "$BUILD_NS" create secret generic felis-registry-push \
    --from-file=username="${dir}/username" \
    --from-file=password="${dir}/build" \
    --dry-run=client -o yaml | kube apply -f -
  rm -rf -- "$dir"
}

# Create missing host credential copies with mode 0600; an absent Secret leaves no file.
# The value goes from kubectl into the file, never into argv or the log.
secret_key_to_file() {
  local namespace="$1" name="$2" key="$3" path="$4" encoded tmp
  [ -e "$path" ] && return 0
  encoded="$(kube -n "$namespace" get secret "$name" -o "jsonpath={.data.${key}}" 2>/dev/null)" || return 0
  tmp="$(umask 077; mktemp "${path}.XXXXXX")"
  remember_temp "$tmp"
  printf '%s' "$encoded" | base64 -d > "$tmp"
  mv -f -- "$tmp" "$path"
}

# Stage and sync replacements beside their targets for atomic, durable credential writes.
write_file_atomic() {
  local path="$1" mode="$2" tmp
  tmp="$(mktemp "${path}.XXXXXX")" || die "could not create a temporary file beside ${path}"
  remember_temp "$tmp"
  { cat > "$tmp" && chmod "$mode" "$tmp" && sync -- "$tmp"; } \
    || die "could not write ${path}; it is left as it was"
  mv -f -- "$tmp" "$path" || die "could not replace ${path}; it is left as it was"
  sync -- "$(dirname -- "$path")" 2>/dev/null || true
}

# Prefer host credential copies during recovery; report credentials absent from both host and cluster.
apply_setup_credential_secrets() {
  local ns
  secret_key_to_file "$CONTROL_NS" felis-smtp password "$SMTP_PASSWORD_FILE"
  secret_key_to_file "$CONTROL_NS" felis-uploads-s3 access_key_id "$UPLOADS_S3_ACCESS_KEY_FILE"
  secret_key_to_file "$CONTROL_NS" felis-uploads-s3 secret_access_key "$UPLOADS_S3_SECRET_KEY_FILE"
  if [ -f "$SMTP_PASSWORD_FILE" ]; then
    for ns in "$CONTROL_NS" "$MINECRAFT_NS"; do
      kube -n "$ns" create secret generic felis-smtp \
        --from-file=password="$SMTP_PASSWORD_FILE" \
        --dry-run=client -o yaml | kube apply -f -
    done
  elif persisted_smtp_block | grep -E '^[[:space:]]*username[[:space:]]*=[[:space:]]*"[^"]' >/dev/null; then
    warn "[smtp] signs in with a username, but its password is in neither ${SMTP_PASSWORD_FILE} nor the cluster: sign-in codes and alerts go out without AUTH until it is entered again (sudo felis setup, then e to configure email)"
  fi
  if [ -f "$UPLOADS_S3_ACCESS_KEY_FILE" ] && [ -f "$UPLOADS_S3_SECRET_KEY_FILE" ]; then
    kube -n "$CONTROL_NS" create secret generic felis-uploads-s3 \
      --from-file=access_key_id="$UPLOADS_S3_ACCESS_KEY_FILE" \
      --from-file=secret_access_key="$UPLOADS_S3_SECRET_KEY_FILE" \
      --dry-run=client -o yaml | kube apply -f -
  elif persisted_registry_block | grep -E '^[[:space:]]*user_uploads_context[[:space:]]*=[[:space:]]*"[sS]3://' >/dev/null; then
    warn "uploads go to an S3 bucket, but its keys are in neither ${UPLOADS_S3_ACCESS_KEY_FILE} / ${UPLOADS_S3_SECRET_KEY_FILE} nor the cluster: felis-api refuses modpack uploads until they are entered again (sudo felis setup, then s to change storage)"
  fi
}

# Exclude public node addresses from game egress to block access to host services.
node_global_cidrs() {
  command -v ip >/dev/null 2>&1 || return 0
  ip -o addr show scope global 2>/dev/null | awk '
    $3 == "inet"  { split($4, a, "/"); print a[1] "/32" }
    $3 == "inet6" { split($4, a, "/"); print a[1] "/128" }
  ' | sort -u
}

as_postgres() {
  if command -v runuser >/dev/null 2>&1; then
    runuser -u postgres -- "$@"
  else
    sudo -u postgres "$@"
  fi
}

bootstrap_from_tui() {
  [ "${FELIS_BOOTSTRAP_FROM_TUI:-}" = "1" ]
}

pause_package_background_timers() {
  command -v systemctl >/dev/null 2>&1 || return 0

  local active=0 timers=() services=() unit
  case "${PKG:-}" in
    apt) timers=("${APT_BACKGROUND_TIMERS[@]}"); services=("${APT_BACKGROUND_SERVICES[@]}") ;;
    dnf) timers=("${DNF_BACKGROUND_TIMERS[@]}"); services=("${DNF_BACKGROUND_SERVICES[@]}") ;;
    yum) timers=("${YUM_BACKGROUND_TIMERS[@]}"); services=("${YUM_BACKGROUND_SERVICES[@]}") ;;
    zypper) timers=("${ZYPPER_BACKGROUND_TIMERS[@]}"); services=("${ZYPPER_BACKGROUND_SERVICES[@]}") ;;
    *) return 0 ;;
  esac

  for unit in "${timers[@]}"; do
    if systemctl is-active --quiet "$unit"; then
      PKG_TIMERS_TO_RESTORE+=("$unit")
      active=1
    fi
  done
  if [ "$active" -eq 1 ]; then
    log "pausing package-manager timers during bootstrap: ${PKG_TIMERS_TO_RESTORE[*]}"
    systemctl stop "${PKG_TIMERS_TO_RESTORE[@]}" || warn "could not stop package-manager timers; package operations may need to wait"
  fi
  for unit in "${services[@]}"; do
    if systemctl is-active --quiet "$unit"; then
      log "stopping package-manager background service during bootstrap: ${unit}"
      systemctl stop "$unit" || warn "could not stop ${unit}; package operations may need to wait"
    fi
  done
}

# Starts the timers pause_package_background_timers stopped: from the EXIT cleanup, and
# before the setup console, which stays open as long as the operator likes.
resume_package_background_timers() {
  local unit
  for unit in "${PKG_TIMERS_TO_RESTORE[@]-}"; do
    [ -n "$unit" ] || continue
    systemctl start "$unit" >/dev/null 2>&1 || true
  done
  PKG_TIMERS_TO_RESTORE=()
}

pkg_lock_files() {
  case "${PKG:-}" in
    apt) printf '%s\n' "${APT_LOCK_FILES[@]}" ;;
    dnf|yum)
      printf '%s\n' \
        /var/lib/rpm/.rpm.lock \
        /var/lib/dnf/rpmdb_lock.pid \
        /var/cache/dnf/metadata_lock.pid \
        /run/dnf.pid \
        /var/run/dnf.pid
      ;;
    zypper)
      printf '%s\n' \
        /var/lib/rpm/.rpm.lock \
        /run/zypp.pid \
        /var/run/zypp.pid
      ;;
    pacman) printf '%s\n' /var/lib/pacman/db.lck ;;
  esac
}

pkg_lock_process_names() {
  case "${PKG:-}" in
    dnf) printf '%s\n' dnf dnf5 rpm ;;
    yum) printf '%s\n' yum rpm ;;
    zypper) printf '%s\n' zypper rpm ;;
    pacman) printf '%s\n' pacman ;;
  esac
}

pkg_busy_pids() {
  local file file_count name
  {
    if command -v fuser >/dev/null 2>&1; then
      local files=()
      file_count=0
      while IFS= read -r file; do
        if [ -e "$file" ]; then
          files+=("$file")
          file_count=$((file_count + 1))
        fi
      done < <(pkg_lock_files)
      [ "$file_count" -eq 0 ] || fuser "${files[@]}" 2>/dev/null | tr ' ' '\n'
    fi
    if command -v pgrep >/dev/null 2>&1; then
      while IFS= read -r name; do
        [ -n "$name" ] && pgrep -x "$name" 2>/dev/null || true
      done < <(pkg_lock_process_names)
    fi
  } | awk 'NF && !seen[$1]++'
}

pkg_lock_busy() {
  [ -n "$(pkg_busy_pids)" ]
}

pkg_lock_holders() {
  local pids
  pids="$(pkg_busy_pids | paste -sd, - || true)"
  [ -n "$pids" ] || return 0
  ps -o pid=,comm= -p "$pids" 2>/dev/null | awk '{$1=$1; print}' | paste -sd ';' -
}

wait_for_pkg_locks() {
  local deadline holders next_notice
  deadline=$((SECONDS + PKG_LOCK_TIMEOUT))
  next_notice=0
  while pkg_lock_busy; do
    if [ "$SECONDS" -ge "$next_notice" ]; then
      holders="$(pkg_lock_holders)"
      if [ -n "$holders" ]; then
        log "waiting for ${PKG} package locks to clear (timeout ${PKG_LOCK_TIMEOUT}s; holders: ${holders})"
      else
        log "waiting for ${PKG} package locks to clear (timeout ${PKG_LOCK_TIMEOUT}s)"
      fi
      next_notice=$((SECONDS + 30))
    fi
    [ "$SECONDS" -lt "$deadline" ] || die "${PKG} package manager is still busy after ${PKG_LOCK_TIMEOUT}s; wait for the current package operation to finish, then retry"
    sleep 5
  done
}

# Suppress needrestart so unrelated package changes do not restart the game proxy.
# The installer restarts what it changes itself.
apt_get() {
  wait_for_pkg_locks
  NEEDRESTART_SUSPEND=1 DEBIAN_FRONTEND=noninteractive apt-get \
    -o DPkg::Lock::Timeout="$PKG_LOCK_TIMEOUT" \
    "$@"
}

validate_timeout() {
  local name="$1" value="$2"
  case "$value" in
    ''|*[!0-9]*) die "${name} must be a non-negative integer (seconds), got: ${value}" ;;
  esac
}

validate_nodeport() {
  local name="$1" value="$2"
  case "$value" in
    ''|*[!0-9]*) die "${name} must be a Kubernetes NodePort integer, got: ${value}" ;;
  esac
  if [ "$value" -lt 30000 ] || [ "$value" -gt 32767 ]; then
    die "${name} must be in Kubernetes NodePort range 30000-32767, got: ${value}"
  fi
}

# Validate a usable listen port before rendering firewall rules, URLs, or the service unit.
validate_listen() {
  local name="$1" value="$2" port="${2##*:}" host="${2%:*}"
  case "$value" in *:*) ;; *) port="" ;; esac
  case "$port" in
    ''|*[!0-9]*) die "${name} must be host:port (for example 127.0.0.1:8081), got: ${value}" ;;
  esac
  [ "$port" -ge 1 ] && [ "$port" -le 65535 ] || die "${name} port must be 1-65535, got: ${value}"
  # Go takes a colon in the host only inside brackets; ::1:8081 would crash-loop the unit.
  case "$host" in
    *:*) case "$host" in "["*"]") ;; *) die "${name} needs an IPv6 host in brackets (for example [::1]:8081), got: ${value}" ;; esac ;;
  esac
}

# The value lands inside a firewalld rich rule, so anything but address characters and one
# prefix length is refused here rather than handed to firewall-cmd.
validate_cidr() {
  case "$2" in
    "") return 0 ;;
    *[!0-9A-Fa-f.:/]*|*/*/*|*/|/*) ;;
    */[0-9]*) return 0 ;;
  esac
  die "$1 must be an address with a prefix length (for example 10.0.0.7/32 or fd00::7/128), got: $2"
}

validate_settings() {
  validate_timeout PKG_LOCK_TIMEOUT "$PKG_LOCK_TIMEOUT"
  validate_timeout APT_LOCK_TIMEOUT "$APT_LOCK_TIMEOUT"
  validate_nodeport FELIS_PANEL_NODEPORT "$FELIS_PANEL_NODEPORT"
  validate_listen FELIS_NANO_LISTEN "$FELIS_NANO_LISTEN"
  validate_cidr FELIS_NANO_PROXY_CIDR "$FELIS_NANO_PROXY_CIDR"
  validate_offsite_settings
  validate_heartbeat_url
  validate_restore_settings
  validate_ha_settings
  case "$FELIS_PREFLIGHT" in
    strict|warn) ;;
    *) die "FELIS_PREFLIGHT must be strict or warn (got '${FELIS_PREFLIGHT}')" ;;
  esac
  case "$FELIS_GAME_STACK" in
    pinned|latest) ;;
    *) die "FELIS_GAME_STACK must be pinned or latest (got '${FELIS_GAME_STACK}')" ;;
  esac
  if [ -n "$FELIS_ARTIFACT_DIR" ]; then
    case "$FELIS_ARTIFACT_DIR" in
      /*) ;;
      *) die "FELIS_ARTIFACT_DIR must be an absolute path (got '${FELIS_ARTIFACT_DIR}')" ;;
    esac
    [ -f "${FELIS_ARTIFACT_DIR}/SHA256SUMS" ] \
      || die "FELIS_ARTIFACT_DIR: ${FELIS_ARTIFACT_DIR}/SHA256SUMS does not exist; point it at the directory deploy/build-release-artifacts.sh wrote, or at a release's downloaded assets"
    # Each names what to install; the directory's binary would silently win.
    [ -z "$FELIS_REF_PINNED" ] || die "FELIS_ARTIFACT_DIR and FELIS_REF both name what to install; set one"
    [ -z "${FELIS_SKIP_FETCH:-}" ] || die "FELIS_ARTIFACT_DIR and FELIS_SKIP_FETCH both name what to install; set one"
  fi
  if [ -n "$FELIS_RELEASE" ]; then
    [[ "$FELIS_RELEASE" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] \
      || die "FELIS_RELEASE must be a release tag like v1.2.3 (got '${FELIS_RELEASE}')"
    # Each of these names what to install another way and would silently win, installing
    # something other than the release asked for.
    [ -z "$FELIS_REF_PINNED" ] || die "FELIS_RELEASE and FELIS_REF both name what to install; set one (FELIS_RELEASE installs a published release's assets, FELIS_REF builds a tree from source)"
    [ -z "$FELIS_ARTIFACT_DIR" ] || die "FELIS_RELEASE and FELIS_ARTIFACT_DIR both name what to install; set one"
    [ -z "${FELIS_SKIP_FETCH:-}" ] || die "FELIS_RELEASE and FELIS_SKIP_FETCH both name what to install; set one"
    [ "$FELIS_VERSION_BOOTSTRAP" = release ] \
      || die "FELIS_RELEASE names a published release, which the ${FELIS_VERSION_BOOTSTRAP} channel does not install; drop FELIS_VERSION_BOOTSTRAP"
    ! bootstrap_from_tui || die "FELIS_RELEASE does not reach felis setup's install, which installs the binary it runs as; run the installer one-liner with FELIS_RELEASE instead"
  fi
  [ "$(heap_megabytes "$FELIS_VELOCITY_XMX")" -ge 256 ] \
    || die "FELIS_VELOCITY_XMX must be a heap size of at least 256M, written <n>M or <n>G (got '${FELIS_VELOCITY_XMX}')"
  case "$FELIS_UPGRADE_DEPS" in
    0|1) ;;
    *) die "FELIS_UPGRADE_DEPS must be 0 or 1 (got '${FELIS_UPGRADE_DEPS}')" ;;
  esac
  case "$FELIS_MANAGE_TIME_SYNC" in
    0|1) ;;
    *) die "FELIS_MANAGE_TIME_SYNC must be 0 or 1 (got '${FELIS_MANAGE_TIME_SYNC}')" ;;
  esac
  case "$FELIS_MANAGE_JOURNAL" in
    0|1) ;;
    *) die "FELIS_MANAGE_JOURNAL must be 0 or 1 (got '${FELIS_MANAGE_JOURNAL}')" ;;
  esac
  # Stripping the unit off a size with none leaves it whole, which the first arm catches.
  local journal_n="${FELIS_JOURNAL_MAX_USE%[KMG]}"
  case "$journal_n" in
    "$FELIS_JOURNAL_MAX_USE"|""|0*|*[!0-9]*)
      die "FELIS_JOURNAL_MAX_USE must be written <n>K, <n>M or <n>G (got '${FELIS_JOURNAL_MAX_USE}')" ;;
  esac
}

# version_newer reports whether version $1 sorts after $2 (a leading v is ignored).
version_newer() {
  local a="${1#v}" b="${2#v}"
  [ "$a" != "$b" ] && [ "$(printf '%s\n%s\n' "$a" "$b" | sort -V | tail -n 1)" = "$a" ]
}

# heap_megabytes prints a JVM heap size written <n>M or <n>G in megabytes, or 0 for any
# other spelling.
heap_megabytes() {
  local n="${1%?}"
  case "$n" in
    ""|*[!0-9]*|0*) echo 0; return ;;
  esac
  # Six digits of gigabytes is far past any host; the cap keeps the arithmetic in range.
  [ "${#n}" -le 6 ] || { echo 0; return; }
  case "$1" in
    *[Mm]) echo "$n" ;;
    *[Gg]) echo "$((n * 1024))" ;;
    *) echo 0 ;;
  esac
}

# Reject quotes, backslashes, and newlines that cannot be safely represented in generated TOML or env files.
validate_offsite_settings() {
  local name value
  for name in FELIS_OFFSITE_ENDPOINT FELIS_OFFSITE_BUCKET FELIS_OFFSITE_REGION FELIS_OFFSITE_PREFIX \
    FELIS_OFFSITE_ACCESS_KEY FELIS_OFFSITE_SECRET_KEY FELIS_OFFSITE_KEY; do
    value="${!name:-}"
    case "$value" in
      *\"* | *\'* | *\\* | *$'\n'* | *$'\r'*) die "${name} must not contain quotes, backslashes or line breaks" ;;
    esac
  done
  if [ -z "$FELIS_OFFSITE_BUCKET" ]; then
    if [ -n "$FELIS_OFFSITE_ENDPOINT$FELIS_OFFSITE_REGION$FELIS_OFFSITE_PREFIX$FELIS_OFFSITE_DB_KEEP" ]; then
      die "FELIS_OFFSITE_* is set without FELIS_OFFSITE_BUCKET; name the bucket too"
    fi
    return 0
  fi
  [ -n "$FELIS_OFFSITE_ENDPOINT" ] || die "FELIS_OFFSITE_BUCKET needs FELIS_OFFSITE_ENDPOINT (https://host[:port] of the S3-compatible store)"
  case "$FELIS_OFFSITE_BUCKET" in
    */* | *' '*) die "FELIS_OFFSITE_BUCKET is a bucket name; put a path inside it in FELIS_OFFSITE_PREFIX" ;;
  esac
  if [ -n "$FELIS_OFFSITE_DB_KEEP" ] && ! [[ "$FELIS_OFFSITE_DB_KEEP" =~ ^[1-9][0-9]*$ ]]; then
    die "FELIS_OFFSITE_DB_KEEP must be a positive number of bundles, got: ${FELIS_OFFSITE_DB_KEEP}"
  fi
}

# Require bucket credentials, encryption key, and a fresh host; resume only from an existing restore progress record.
validate_restore_settings() {
  case "$RESTORE_FROM_OFFSITE" in
    0) return 0 ;;
    1) ;;
    *) die "FELIS_RESTORE_FROM_OFFSITE must be 1 or unset (got '${RESTORE_FROM_OFFSITE}')" ;;
  esac
  local name
  for name in FELIS_OFFSITE_BUCKET FELIS_OFFSITE_ENDPOINT FELIS_OFFSITE_ACCESS_KEY FELIS_OFFSITE_SECRET_KEY FELIS_OFFSITE_KEY; do
    [ -n "${!name:-}" ] || die "FELIS_RESTORE_FROM_OFFSITE=1 needs ${name}: the old install's bucket, its credentials and its encryption key"
  done
  if [ -f "$BOOTSTRAP_DONE" ] && [ ! -f "${RESTORE_STATE_DIR}/state.json" ]; then
    die "FELIS_RESTORE_FROM_OFFSITE=1 rebuilds onto a fresh host, and Felis is already installed here; to replace this install's data anyway, run: sudo felis restore-host -yes -force"
  fi
  case "$RESTORE_BUNDLE" in
    "" | felis-db-*.tar) ;;
    *) die "FELIS_RESTORE_BUNDLE is a bundle name from \`felis offsite list\` (felis-db-<stamp>-<label>.tar), got '${RESTORE_BUNDLE}'" ;;
  esac
}

# Restore offsite state before setup so the wizard finds the recovered owner.
restore_from_offsite() {
  [ "$RESTORE_FROM_OFFSITE" = 1 ] || return 0
  local args=(restore-host -yes)
  [ -z "$RESTORE_BUNDLE" ] || args+=(-bundle "$RESTORE_BUNDLE")
  [ "$RESTORE_TAKE_OVER" != 1 ] || args+=(-take-over)
  log "rebuilding this install from the off-site copy"
  "$HOST_BIN" "${args[@]}" \
    || die "the rebuild stopped before it finished; fix the cause above and run: sudo felis restore-host -yes (it carries on where it stopped)"
  ok "rebuilt from the off-site copy"
}

# Exclude heartbeat URLs from errors because the path contains the monitoring secret.
validate_heartbeat_url() {
  case "$FELIS_WATCHDOG_HEARTBEAT_URL" in
    "" | off) ;;
    *[[:space:]]* | *\"* | *\'* | *\\*) die "FELIS_WATCHDOG_HEARTBEAT_URL must not contain spaces, quotes or backslashes" ;;
    http://[!/]* | https://[!/]*) ;;
    *) die "FELIS_WATCHDOG_HEARTBEAT_URL must be the http:// or https:// ping URL of a monitoring service's check, or off" ;;
  esac
}

# 0. Privilege & host facts
if [ "$(id -u)" -ne 0 ]; then
  if [ -r "$0" ]; then
    log "re-executing under sudo"
    exec sudo -E bash "$0" "$@"
  fi
  die "must run as root (for a piped installer, use: curl -fsSL <url> | sudo bash)"
fi

detect_os() {
  [ -r /etc/os-release ] || die "cannot read /etc/os-release; unsupported host"
  # shellcheck disable=SC1091
  . /etc/os-release
  OS_ID="${ID:-unknown}"
  OS_VERSION="${VERSION_ID:-unknown}"
  OS_ID_LIKE="${ID_LIKE:-}"
  OS_CODENAME="${VERSION_CODENAME:-${UBUNTU_CODENAME:-}}"
  if command -v apt-get >/dev/null 2>&1; then
    PKG="apt"
  elif command -v dnf >/dev/null 2>&1; then
    PKG="dnf"
  elif command -v yum >/dev/null 2>&1; then
    PKG="yum"
  elif command -v zypper >/dev/null 2>&1; then
    PKG="zypper"
  elif command -v pacman >/dev/null 2>&1; then
    PKG="pacman"
  else
    die "no supported package manager (apt/dnf/yum/zypper/pacman) found on ${OS_ID} ${OS_VERSION}"
  fi
  log "host: ${PRETTY_NAME:-$OS_ID $OS_VERSION}  (package manager: ${PKG})"
}

# Read the persisted root domain from generated TOML before choosing defaults.
persisted_root_domain() {
  local f="${STATE_DIR}/felis.host.toml"
  [ -r "$f" ] || return 0
  awk -F'"' '/^[[:space:]]*root_domain[[:space:]]*=/ { print $2; exit }' "$f"
}

detect_node_ip() {
  NODE_IP="$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="src"){print $(i+1); exit}}')"
  if [ -z "${NODE_IP:-}" ]; then
    # Require an IPv4 default route; IPv6 cannot supply nip.io names or /32 policy addresses.
    local a
    for a in $(hostname -I 2>/dev/null); do
      if ipv4_to_int "$a" >/dev/null; then NODE_IP="$a"; break; fi
    done
  fi
  [ -n "${NODE_IP:-}" ] || die "could not determine this host's primary IPv4 address"

  # Preserve installed domains on reruns; use felis domain set to update all dependent surfaces.
  local persisted
  persisted="$(persisted_root_domain)"
  if [ -n "${FELIS_ROOT_DOMAIN:-}" ] && [ -n "$persisted" ] && [ "$FELIS_ROOT_DOMAIN" != "$persisted" ]; then
    die "FELIS_ROOT_DOMAIN (${FELIS_ROOT_DOMAIN}) differs from the installed ${persisted}. The installer keeps the installed domain; to move the install, rerun it without FELIS_ROOT_DOMAIN, then run: sudo felis domain set ${FELIS_ROOT_DOMAIN} (https://docs.felismc.com/en/operations/access#change-domain)"
  fi
  FELIS_ROOT_DOMAIN="${FELIS_ROOT_DOMAIN:-${persisted:-${NODE_IP}.nip.io}}"
  if [ -n "$persisted" ] && [ "$FELIS_ROOT_DOMAIN" = "$persisted" ]; then
    log "node IP: ${NODE_IP}   root domain: ${FELIS_ROOT_DOMAIN} (reusing the installed domain)"
  else
    log "node IP: ${NODE_IP}   root domain: ${FELIS_ROOT_DOMAIN}"
  fi
}

# Warn about DHCP addresses because policies, certificates, and default domains require a stable node IP.
# ip -o addr marks a leased address 'dynamic'.
warn_dynamic_node_ip() {
  if ip -4 -o addr show 2>/dev/null | awk -v ip="$NODE_IP" '
      { split($4, a, "/"); if (a[1] == ip && / dynamic /) found = 1 }
      END { exit !found }'; then
    warn "${NODE_IP} is a DHCP lease, and the install is bound to this address. Give the host a"
    warn "DHCP reservation or a static address before it changes (https://docs.felismc.com/en/guide/requirements#static-address)."
  fi
}

# Collect all preflight failures before mutating the host.
# Warnings print as they are found and never stop the run; FELIS_PREFLIGHT=warn turns problems into warnings too.
PREFLIGHT_PROBLEMS=()
SWAP_FILE=/swapfile
SWAP_SKIP=""
preflight_fail() { PREFLIGHT_PROBLEMS+=("$*"); }

# Allow for kernel-reserved memory when evaluating the minimum full-stack host size.
PREFLIGHT_MIN_RAM_KB=1835008      # 1.75 GiB
PREFLIGHT_RECOMMENDED_RAM_KB=3670016  # 3.5 GiB

# ipv4_to_int prints a dotted quad as a 32-bit integer; anything else fails.
ipv4_to_int() {
  local a b c d n
  IFS=. read -r a b c d <<<"$1"
  for n in "$a" "$b" "$c" "$d"; do
    case "$n" in ""|*[!0-9]*) return 1 ;; esac
    [ "$n" -le 255 ] || return 1
  done
  printf '%s\n' "$(( (a << 24) | (b << 16) | (c << 8) | d ))"
}

# cidr_overlap reports whether two IPv4 prefixes (a bare address is a /32) share an
# address.
cidr_overlap() { # a/n b/m
  local an=32 bn=32 ai bi n mask
  case "$1" in */*) an="${1#*/}" ;; esac
  case "$2" in */*) bn="${2#*/}" ;; esac
  ai="$(ipv4_to_int "${1%/*}")" || return 1
  bi="$(ipv4_to_int "${2%/*}")" || return 1
  n=$(( an < bn ? an : bn ))
  mask=$(( n == 0 ? 0 : (0xFFFFFFFF << (32 - n)) & 0xFFFFFFFF ))
  [ $(( ai & mask )) -eq $(( bi & mask )) ]
}

# cgroup_controllers prints the enabled cgroup controllers (v2, else v1).
cgroup_controllers() {
  if [ -r /sys/fs/cgroup/cgroup.controllers ]; then
    cat /sys/fs/cgroup/cgroup.controllers
  elif [ -r /proc/cgroups ]; then
    awk '$4 == 1 { print $1 }' /proc/cgroups | tr '\n' ' '
  fi
}

systemd_is_init() { [ -d /run/systemd/system ]; }

mem_total_kb() { awk '/^MemTotal:/ { print $2 }' /proc/meminfo; }

# Detect distributions whose k3s installation fetches k3s-selinux from Rancher's RPM repository.
# $1 is a root to read under, for the tests.
k3s_adds_selinux_rpm() {
  local root="${1:-}" f
  [ -d "${root}/usr/share/selinux" ] || return 1
  for f in redhat-release centos-release oracle-release fedora-release system-release; do
    [ -r "${root}/etc/${f}" ] && return 0
  done
  [ "${OS_ID_LIKE%% *}" = suse ]
}

preflight_platform() {
  case "$(uname -m)" in
    x86_64|amd64|aarch64|arm64) ;;
    *) preflight_fail "this host is $(uname -m); Felis publishes its images for amd64 and arm64 only" ;;
  esac
  systemd_is_init \
    || preflight_fail "systemd is not running as init; the installer manages k3s, the proxy and its timers as systemd units"
  # k3s refuses to start without the memory controller. Raspberry Pi OS ships it off.
  local controllers
  controllers="$(cgroup_controllers)"
  case " $controllers " in
    *" memory "*) ;;
    *) preflight_fail "the memory cgroup controller is off, and k3s will not start without it (on a Raspberry Pi add 'cgroup_memory=1 cgroup_enable=memory' to /boot/firmware/cmdline.txt and reboot)" ;;
  esac
}

preflight_memory() {
  local mem_kb
  mem_kb="$(mem_total_kb)"
  [ -n "$mem_kb" ] || return 0
  if [ "$mem_kb" -lt "$PREFLIGHT_MIN_RAM_KB" ]; then
    preflight_fail "this host has $((mem_kb / 1024)) MiB of RAM; the full stack needs at least $((PREFLIGHT_MIN_RAM_KB / 1024)) MiB (a 2 GB host), 4 GB to run players' servers beside it. Felis-nano (FELIS_INSTALL_MODE=nano) fits in far less"
  elif [ "$mem_kb" -lt "$PREFLIGHT_RECOMMENDED_RAM_KB" ]; then
    warn "preflight: $((mem_kb / 1024)) MiB of RAM runs the platform with little room for players' servers; 4 GB is the comfortable size"
  fi
}

# existing_ancestor prints the nearest directory of $1 that exists, for df.
existing_ancestor() {
  local p="$1"
  while [ ! -e "$p" ]; do p="$(dirname "$p")"; done
  printf '%s\n' "$p"
}

# path_populated reports whether directory $1 exists with something in it.
path_populated() { [ -n "$(ls -A "$1" 2>/dev/null)" ]; }

release_assets_expected() {
  [ -z "$FELIS_ARTIFACT_DIR" ] || return 1
  bootstrap_from_tui || use_release_binary
}

# Preflight anticipates local builds, but missing or unusable release images can require source fallback.
host_builds_expected() {
  [ "$FELIS_GAME_STACK" != latest ] || return 0
  [ -z "$FELIS_ARTIFACT_DIR" ] || return 1
  ! release_assets_expected
}

# Check every target filesystem; populated locations need room for changes rather than a full fresh installation.
preflight_disk() {
  local spec path need_empty need_populated need line rows="" mount size used avail
  local felis_spec="/var/lib/felis 2048 1024" docker_spec=""
  release_assets_expected && felis_spec="/var/lib/felis 4096 3072"
  host_builds_expected && docker_spec="/var/lib/containerd 8192 2048"
  for spec in "/var/lib/rancher 10240 3072" "$felis_spec" "/opt/felis 3072 1024" \
    ${docker_spec:+"$docker_spec"}; do
    read -r path need_empty need_populated <<<"$spec"
    need="$need_empty"
    path_populated "$path" && need="$need_populated"
    line="$(df -Pk "$(existing_ancestor "$path")" 2>/dev/null | awk 'NR == 2 { print $6, $2, $3, $4 }')" || continue
    [ -n "$line" ] && rows="${rows}${line} $((need * 1024))"$'\n'
  done
  # One line per filesystem, with what lands on it summed.
  rows="$(printf '%s' "$rows" | awk 'NF == 5 {
      if (!($1 in need)) order[++n] = $1
      size[$1] = $2; used[$1] = $3; avail[$1] = $4; need[$1] += $5
    }
    END { for (i = 1; i <= n; i++) { m = order[i]; print m, size[m], used[m], avail[m], need[m] } }')"
  while read -r mount size used avail need; do
    [ -n "$mount" ] || continue
    if [ "$avail" -lt "$need" ]; then
      preflight_fail "${mount} has $((avail / 1024)) MiB free; this install writes about $((need / 1024)) MiB there (k3s under /var/lib/rancher, the image builds under /var/lib/containerd, the database under /var/lib/felis, sources under /opt/felis)"
      continue
    fi
    # k3s's image GC starts collecting at 85% and the kubelet evicts pods below 5% free.
    if [ "$size" -gt 0 ] && [ $(( (used + need) * 100 / size )) -ge 85 ]; then
      warn "preflight: ${mount} will be over 85% full after the install; k3s starts deleting cached images there and evicts pods near 95%"
    fi
  done <<<"$rows"
  # The swap file is optional: it is left out rather than squeezing what the install writes.
  if [ "$(swap_total_kb)" = 0 ] && [ ! -e "$SWAP_FILE" ]; then
    local swap_kb free_after
    swap_kb=$(($(swap_file_mib) * 1024))
    read -r mount size avail <<<"$(df -Pk / 2>/dev/null | awk 'NR == 2 { print $6, $2, $4 }')"
    need="$(printf '%s\n' "$rows" | awk -v m="$mount" '$1 == m { print $5 }')"
    free_after=$((${avail:-0} - ${need:-0} - swap_kb))
    if [ "$free_after" -lt $((${size:-0} / 10)) ]; then
      SWAP_SKIP="/ has $((${avail:-0} / 1024)) MiB free, too little for a $((swap_kb / 1024)) MiB swap file beside the install"
    fi
  fi
}

# pid_unit prints the systemd service a process runs in, or nothing.
pid_unit() {
  sed -n 's#^.*/\([^/]*\.service\)\(/.*\)\{0,1\}$#\1#p' "/proc/$1/cgroup" 2>/dev/null | tail -n 1
}

# port_listeners prints "comm pid unit" for each process listening on TCP port $1.
port_listeners() {
  local out pair comm pid
  out="$(ss -Hltnp "sport = :$1" 2>/dev/null)" || return 0
  [ -n "$out" ] || return 0
  pair="$(grep -oE '\("[^"]+",pid=[0-9]+' <<<"$out" | sort -u)" || true
  if [ -z "$pair" ]; then
    printf '%s\n' "? ? ?"
    return 0
  fi
  while IFS= read -r pair; do
    comm="${pair#(\"}"; comm="${comm%%\"*}"
    pid="${pair##*pid=}"
    printf '%s %s %s\n' "$comm" "$pid" "$(pid_unit "$pid")"
  done <<<"$pair"
}

# preflight_ports refuses a port another program already holds. Listeners of the units
# this installer runs are what a rerun finds, and pass.
preflight_ports() {
  command -v ss >/dev/null 2>&1 || { warn "preflight: ss not found; ports are not checked"; return 0; }
  local spec port unit label comm pid owner
  for spec in \
    "${FELIS_GAME_PORT} felis-velocity.service the Minecraft proxy (FELIS_GAME_PORT)" \
    "6443 k3s.service the Kubernetes API" "6444 k3s.service k3s's supervisor" \
    "10248 k3s.service the kubelet" "10249 k3s.service kube-proxy" "10250 k3s.service the kubelet" \
    "10256 k3s.service kube-proxy" "10257 k3s.service the controller manager" "10259 k3s.service the scheduler" \
    "${FELIS_PANEL_NODEPORT} k3s.service the panel (FELIS_PANEL_NODEPORT)" \
    "${REGISTRY_URL##*:} k3s.service the image registry's loopback port" \
    "${PG_HOST_PORT} k3s.service the database's loopback port"; do
    read -r port unit label <<<"$spec"
    while read -r comm pid owner; do
      [ -n "$comm" ] || continue
      [ "$owner" = "$unit" ] && continue
      if [ "$comm" = "?" ]; then
        preflight_fail "port ${port} (${label}) is already taken by a process ss cannot name"
      else
        preflight_fail "port ${port} (${label}) is already taken by ${comm} (pid ${pid}${owner:+, ${owner}}); stop it or move it"
      fi
    done < <(port_listeners "$port")
  done
}

# preflight_cluster refuses a host that already runs another Kubernetes, or is a k3s
# agent: k3s would fight it for 6443, 10250 and the iptables chains. A k3s server is
# reused as it is.
preflight_cluster() {
  local unit units
  units="$(systemctl list-units --all --plain --no-legend --type=service 2>/dev/null | awk '{ print $1 }')" || units=""
  for unit in rke2-server.service rke2-agent.service k0scontroller.service k0sworker.service \
    snap.microk8s.daemon-kubelite.service kubelet.service k3s-agent.service; do
    grep -qx "$unit" <<<"$units" || continue
    systemctl is-active --quiet "$unit" 2>/dev/null || continue
    case "$unit" in
      k3s-agent.service) preflight_fail "this host is a k3s agent (k3s-agent.service); Felis installs a single-node k3s server" ;;
      *) preflight_fail "another Kubernetes runs here (${unit}); Felis installs its own k3s, which would fight it for ports 6443 and 10250" ;;
    esac
  done
  if [ -x "$K3S_BIN" ] && [ ! -f "$BOOTSTRAP_DONE" ]; then
    log "preflight: k3s is already installed at ${K3S_BIN}; Felis adds its namespaces to that cluster"
  fi
}

# Reject node or routed networks shadowed by pod/service CIDRs; warn for broader overlapping routes.
preflight_networks() {
  local cidr routes line dst rest dev len
  for cidr in "$POD_CIDR" "$SERVICE_CIDR"; do
    if cidr_overlap "$NODE_IP" "$cidr"; then
      preflight_fail "this host's address ${NODE_IP} is inside k3s's range ${cidr}; the cluster cannot route to its own node"
    fi
  done
  routes="$(ip -4 route show 2>/dev/null)" || return 0
  while read -r dst rest; do
    case "$dst" in
      ""|default) continue ;;
      blackhole|unreachable|prohibit|throw|local|broadcast) read -r dst rest <<<"$rest" ;;
    esac
    line="$dst $rest"
    dev="$(awk '{ for (i = 1; i < NF; i++) if ($i == "dev") { print $(i + 1); exit } }' <<<"$line")"
    case "$dev" in cni0|flannel.1|flannel-wg|kube-ipvs0) continue ;; esac
    len=32
    case "$dst" in */*) len="${dst#*/}" ;; esac
    for cidr in "$POD_CIDR" "$SERVICE_CIDR"; do
      cidr_overlap "$dst" "$cidr" || continue
      if [ "$len" -lt "${cidr#*/}" ]; then
        warn "preflight: the route ${dst}${dev:+ via ${dev}} covers k3s's ${cidr}; hosts in ${cidr} on that network will be unreachable from here"
      else
        preflight_fail "the network ${dst}${dev:+ on ${dev}} lies inside k3s's ${cidr}; pods and that network would be confused (move the Docker network or LAN, or reinstall k3s with other ranges)"
      fi
    done
  done <<<"$routes"
}

# Distinguish required download hosts from optional source fallbacks; package mirrors belong to the package manager.
preflight_hosts() {
  printf '%s\n' "github.com required"
  if [ -z "$FELIS_ARTIFACT_DIR" ] && [ -z "${FELIS_SKIP_FETCH:-}" ] && [ -z "$FELIS_REF_PINNED" ]; then
    if bootstrap_from_tui; then
      printf '%s\n' "api.github.com optional"
    else
      printf '%s\n' "api.github.com required"
    fi
  fi
  if ! [ -x "$K3S_BIN" ]; then
    printf '%s\n' "raw.githubusercontent.com required"
    if k3s_adds_selinux_rpm; then
      printf '%s\n' "rpm.rancher.io required"
    fi
  fi
  [ -n "$FELIS_VELOCITY_FORK_JAR" ] || printf '%s\n' "fill-data.papermc.io required"
  if host_builds_expected; then
    printf '%s\n' "registry-1.docker.io required"
  elif [ -z "$FELIS_ARTIFACT_DIR" ]; then
    printf '%s\n' "registry-1.docker.io optional"
  fi
}

# Retry HTTPS reachability three times; use TCP only without curl and without an HTTP proxy.
host_reachable() {
  local try tls
  for try in 1 2 3; do
    [ "$try" = 1 ] || sleep 2
    if command -v curl >/dev/null 2>&1; then
      # The handshake's time, 0.000000 until one completes, whatever curl then exits with.
      tls="$(curl -s -o /dev/null --connect-timeout 5 --max-time 15 -w '%{time_appconnect}' "https://$1/" 2>/dev/null)" || true
      [ -n "${tls//[0.]/}" ] && return 0
    elif [ -n "${https_proxy:-}${HTTPS_PROXY:-}" ]; then
      return 0
    elif timeout 5 bash -c 'exec 3<>"/dev/tcp/$0/443"' "$1" 2>/dev/null; then
      return 0
    fi
  done
  return 1
}

preflight_outbound() {
  local host need unreachable=() fallback=()
  while read -r host need; do
    [ -n "$host" ] || continue
    host_reachable "$host" && continue
    if [ "$need" = optional ]; then
      fallback+=("$host")
    else
      unreachable+=("$host")
    fi
  done < <(preflight_hosts)
  if [ "${#fallback[@]}" -gt 0 ]; then
    warn "preflight: cannot reach ${fallback[*]} over HTTPS; the install goes on, but cannot build or pull here an image the release turns out not to supply"
  fi
  [ "${#unreachable[@]}" -eq 0 ] && return 0
  preflight_fail "cannot reach ${unreachable[*]} over HTTPS; the install downloads from there (check DNS, the firewall, or set https_proxy)"
}

preflight() {
  log "preflight: checking this host before anything is changed"
  PREFLIGHT_PROBLEMS=()
  preflight_platform
  preflight_memory
  preflight_disk
  preflight_ports
  preflight_cluster
  preflight_networks
  preflight_outbound
  warn_dynamic_node_ip
  local n="${#PREFLIGHT_PROBLEMS[@]}" p
  if [ "$n" -eq 0 ]; then
    ok "preflight passed"
    return 0
  fi
  if [ "$FELIS_PREFLIGHT" = warn ]; then
    for p in "${PREFLIGHT_PROBLEMS[@]}"; do warn "preflight: $p"; done
    warn "preflight: FELIS_PREFLIGHT=warn, going on despite ${n} problem(s)"
    return 0
  fi
  printf '\033[1;31m[fail]\033[0m preflight found %s problem(s); nothing on this host has been changed:\n' "$n" >&2
  for p in "${PREFLIGHT_PROBLEMS[@]}"; do printf '  - %s\n' "$p" >&2; done
  die "fix them and rerun the installer (FELIS_PREFLIGHT=warn installs anyway)"
}

pkg_install() {
  case "$PKG" in
    apt) apt_get install -y "$@" ;;
    dnf) wait_for_pkg_locks; dnf install -y "$@" ;;
    yum) wait_for_pkg_locks; yum install -y "$@" ;;
    zypper) wait_for_pkg_locks; zypper --non-interactive install -y "$@" ;;
    pacman) wait_for_pkg_locks; pacman -S --noconfirm --needed "$@" ;;
  esac
}

# Complete interrupted dpkg configuration before retrying package installation.
finish_interrupted_dpkg() {
  [ -n "$(ls -A "$DPKG_UPDATES_DIR" 2>/dev/null)" ] || [ -n "$(dpkg --audit 2>/dev/null)" ] || return 0
  warn "an earlier dpkg run was cut off; finishing it (dpkg --configure -a)"
  wait_for_pkg_locks
  NEEDRESTART_SUSPEND=1 DEBIAN_FRONTEND=noninteractive \
    dpkg --force-confdef --force-confold --configure -a \
    || die "dpkg --configure -a failed; fix the package it names, then rerun the installer"
}

pkg_refresh_once() {
  [ -n "${_PKG_REFRESHED:-}" ] && return 0
  case "$PKG" in
    apt) finish_interrupted_dpkg; apt_get update -y ;;
    dnf|yum) : ;;   # dnf/yum refresh metadata on demand
    zypper) wait_for_pkg_locks; zypper --non-interactive refresh ;;
    # Use Arch whole-system upgrades while holding PostgreSQL at its existing major to preserve migration and rollback data.
    pacman)
      wait_for_pkg_locks
      if [ -f "$(postgres_data_dir)/PG_VERSION" ]; then
        pacman -Syu --noconfirm --ignore postgresql
      else
        pacman -Syu --noconfirm
      fi
      ;;
  esac
  _PKG_REFRESHED=1
}

swap_total_kb() { awk '/^SwapTotal:/ { print $2 }' /proc/meminfo; }

# swap_file_mib matches memory, with 2 GiB as the floor a small host needs to ride out memory pressure.
swap_file_mib() {
  local mib=$((($(mem_total_kb) + 1023) / 1024))
  [ "$mib" -ge 2048 ] || mib=2048
  echo "$mib"
}

# A hibernating server's memory moves to swap, so a node without swap gets a swap file as large as its memory.
ensure_swap() {
  local mem_kb swap_kb mib
  mem_kb="$(mem_total_kb)"
  swap_kb="$(swap_total_kb)"
  if [ "${swap_kb:-0}" -gt 0 ]; then
    if [ "$swap_kb" -lt "$mem_kb" ]; then
      warn "swap is $((swap_kb / 1024)) MiB, less than the $((mem_kb / 1024)) MiB of memory; an idle server whose memory does not fit in it stops instead of hibernating"
    else
      ok "swap already present ($((swap_kb / 1024)) MiB)"
    fi
    return 0
  fi
  if [ -e "$SWAP_FILE" ]; then
    warn "${SWAP_FILE} exists but is not in use; leaving it alone, so idle servers stop instead of hibernating"
    return 0
  fi
  if [ -n "$SWAP_SKIP" ]; then
    warn "no swap file created: ${SWAP_SKIP}; idle servers stop instead of hibernating"
    return 0
  fi
  mib="$(swap_file_mib)"
  log "creating a ${mib} MiB swap file so idle servers can hibernate"
  if ! make_swap_file "$mib" || ! swapon "$SWAP_FILE" 2>/dev/null; then
    rm -f "$SWAP_FILE"
    warn "this host refused a swap file; idle servers stop instead of hibernating"
    return 0
  fi
  grep -q "^${SWAP_FILE} " /etc/fstab || echo "${SWAP_FILE} none swap sw 0 0" >> /etc/fstab
  ok "${mib} MiB swap active"
}

make_swap_file() { # mib
  if [ "$(stat -f -c %T / 2>/dev/null)" = btrfs ]; then
    btrfs filesystem mkswapfile --size "${1}m" "$SWAP_FILE" >/dev/null 2>&1
    return
  fi
  fallocate -l "${1}M" "$SWAP_FILE" 2>/dev/null || dd if=/dev/zero of="$SWAP_FILE" bs=1M count="$1" status=none || return
  chmod 600 "$SWAP_FILE" && mkswap "$SWAP_FILE" >/dev/null 2>&1
}

# Enable NTP and allow initial synchronization; the watchdog reports clocks still unsynchronized after the wait.
ensure_time_sync() {
  if [ "$FELIS_MANAGE_TIME_SYNC" = 0 ]; then
    log "FELIS_MANAGE_TIME_SYNC=0: leaving time synchronization to the operator"
    return 0
  fi
  if ! command -v timedatectl >/dev/null 2>&1; then
    warn "timedatectl not found; make sure an NTP client keeps this host's clock (https://docs.felismc.com/en/guide/requirements#time-sync)"
    return 0
  fi
  if [ "$(timedatectl show -p NTP --value 2>/dev/null)" != yes ]; then
    # set-ntp fails with "NTP not supported" when no NTP unit is installed at all
    # (Debian's minimal image splits systemd-timesyncd into its own package).
    if ! timedatectl set-ntp true 2>/dev/null; then
      log "no NTP client to enable; installing chrony"
      pkg_install chrony
      if ! timedatectl set-ntp true; then
        warn "could not turn NTP on; set up time synchronization by hand (https://docs.felismc.com/en/operations/troubleshooting#clock)"
        return 0
      fi
    fi
    log "turned NTP time synchronization on"
  fi
  local _
  for _ in $(seq 1 15); do
    if [ "$(timedatectl show -p NTPSynchronized --value 2>/dev/null)" = yes ]; then
      ok "system clock synchronized by NTP"
      return 0
    fi
    sleep 2
  done
  warn "the system clock is not synchronized yet; check 'timedatectl' (https://docs.felismc.com/en/operations/troubleshooting#clock)"
}

# Restart journald when configuration changes or its persistent directory is still missing.
ensure_persistent_journal() {
  if [ "$FELIS_MANAGE_JOURNAL" = 0 ]; then
    log "FELIS_MANAGE_JOURNAL=0: leaving journald as it is"
    return 0
  fi
  local file="$JOURNALD_DROPIN" tmp
  mkdir -p "$(dirname "$file")"
  # Create temporary drop-ins beside the target to inherit its SELinux label; journald ignores non-.conf names.
  tmp="$(mktemp "${file}.XXXXXX")"
  remember_temp "$tmp"
  printf '[Journal]\nStorage=persistent\nSystemMaxUse=%s\n' "$FELIS_JOURNAL_MAX_USE" > "$tmp"
  if [ -f "$file" ] && cmp -s "$tmp" "$file"; then
    rm -f "$tmp"
    if [ -d "$JOURNAL_DIR" ]; then
      ok "system journal already persistent (capped at ${FELIS_JOURNAL_MAX_USE})"
      return 0
    fi
    log "journald has not taken up ${file}; restarting it"
  else
    chmod 0644 "$tmp"
    mv "$tmp" "$file"
  fi
  restore_label "$file"
  systemctl restart systemd-journald
  journalctl --flush >/dev/null 2>&1 || true
  if [ -d "$JOURNAL_DIR" ]; then
    ok "system journal is persistent under ${JOURNAL_DIR} (capped at ${FELIS_JOURNAL_MAX_USE})"
  else
    warn "journald did not create ${JOURNAL_DIR}, so logs still end at a reboot; see: journalctl -u systemd-journald"
  fi
}

# 2. Base packages
install_base() {
  local packages=(ca-certificates openssl)

  pkg_refresh_once
  command -v curl >/dev/null 2>&1 || packages+=(curl)
  command -v tar >/dev/null 2>&1 || packages+=(tar)
  if ! bootstrap_from_tui && ! command -v git >/dev/null 2>&1; then
    packages+=(git)
  fi

  pkg_install "${packages[@]}"
  ok "base tools present"
}

install_cloudflared() {
  local current="" path
  if path="$(command -v cloudflared 2>/dev/null)"; then
    current="$(cloudflared --version 2>/dev/null | awk '{ for (i = 1; i < NF; i++) if ($i == "version") { print $(i + 1); exit } }')"
    if [ "$current" = "$FELIS_CLOUDFLARED_VERSION" ]; then
      ok "cloudflared ${current} already installed"
      return 0
    fi
    if [ "$FELIS_UPGRADE_DEPS" != 1 ]; then
      ok "cloudflared ${current:-(version unreadable)} already installed; this release pins ${FELIS_CLOUDFLARED_VERSION} (FELIS_UPGRADE_DEPS=1 moves it)"
      return 0
    fi
    if [ "$path" != "$CLOUDFLARED_BIN" ]; then
      warn "cloudflared at ${path} was not installed by Felis; upgrade it the way it was installed"
      return 0
    fi
    if [ -n "$current" ] && version_newer "$current" "$FELIS_CLOUDFLARED_VERSION"; then
      ok "cloudflared ${current} is newer than the pinned ${FELIS_CLOUDFLARED_VERSION}; left as it is"
      return 0
    fi
  fi
  local machine arch url tmp want have
  machine="$(uname -m)"
  case "$machine" in
    x86_64|amd64) arch="amd64"; want="$CLOUDFLARED_PINNED_SHA256_AMD64" ;;
    aarch64|arm64) arch="arm64"; want="$CLOUDFLARED_PINNED_SHA256_ARM64" ;;
    armv7l|armv6l) arch="arm"; want="$CLOUDFLARED_PINNED_SHA256_ARM" ;;
    *) die "unsupported architecture for cloudflared: ${machine}" ;;
  esac
  [ "$FELIS_CLOUDFLARED_VERSION" = "$CLOUDFLARED_PINNED_VERSION" ] || want="$FELIS_CLOUDFLARED_SHA256"
  [ -n "$want" ] || die "no pinned sha256 for cloudflared ${FELIS_CLOUDFLARED_VERSION}; set FELIS_CLOUDFLARED_SHA256 to the digest of cloudflared-linux-${arch} on that GitHub release"
  url="https://github.com/cloudflare/cloudflared/releases/download/${FELIS_CLOUDFLARED_VERSION}/cloudflared-linux-${arch}"
  tmp="$(mktemp)"
  remember_temp "$tmp"
  log "installing cloudflared ${FELIS_CLOUDFLARED_VERSION} (${arch})"
  curl -fsSL --retry 5 --retry-delay 2 "$url" -o "$tmp"
  have="$(sha256sum <"$tmp" | cut -d' ' -f1)"
  if [ "$have" != "$(printf '%s' "$want" | tr 'A-Z' 'a-z')" ]; then
    rm -f "$tmp"
    die "cloudflared-linux-${arch} ${FELIS_CLOUDFLARED_VERSION} hashes to ${have}, expected ${want}; refusing to install it"
  fi
  install -m 0755 "$tmp" "$CLOUDFLARED_BIN"
  rm -f "$tmp"
  ok "cloudflared installed ($(cloudflared --version | sed -n 1p))"
  # The running tunnel keeps the old binary mapped until it restarts.
  if [ -n "$current" ] && systemctl is-active --quiet cloudflared-felis 2>/dev/null; then
    systemctl restart cloudflared-felis
    ok "cloudflared-felis restarted onto ${FELIS_CLOUDFLARED_VERSION}"
  fi
}

# 3. Docker (used only to build & export the felis image; k3s uses containerd)
docker_apt_repo_os() {
  case "$OS_ID" in
    debian|ubuntu)
      printf '%s\n' "$OS_ID"
      ;;
    *)
      die "Docker apt repository is not configured for ${OS_ID} ${OS_VERSION}"
      ;;
  esac
}

install_docker_apt() {
  local arch keyring repo_os

  repo_os="$(docker_apt_repo_os)"
  [ -n "$OS_CODENAME" ] || die "cannot determine apt codename for ${OS_ID} ${OS_VERSION}"

  arch="$(dpkg --print-architecture)"
  keyring="/etc/apt/keyrings/docker.asc"

  log "installing docker apt repository"
  install -m 0755 -d /etc/apt/keyrings
  curl -fsSL --retry 5 --retry-delay 2 "https://download.docker.com/linux/${repo_os}/gpg" -o "$keyring" \
    || die "failed to download Docker GPG key for ${repo_os}"
  chmod a+r "$keyring"

  cat > /etc/apt/sources.list.d/docker.list <<EOF
deb [arch=${arch} signed-by=${keyring}] https://download.docker.com/linux/${repo_os} ${OS_CODENAME} stable
EOF

  apt_get update -y
  pkg_install docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
}

docker_rpm_repo_url() {
  case "$OS_ID" in
    fedora)
      printf '%s\n' "https://download.docker.com/linux/fedora/docker-ce.repo"
      ;;
    rhel)
      printf '%s\n' "https://download.docker.com/linux/rhel/docker-ce.repo"
      ;;
    centos|almalinux|rocky)
      printf '%s\n' "https://download.docker.com/linux/centos/docker-ce.repo"
      ;;
    *)
      case " ${OS_ID_LIKE} " in
        *" rhel "*|*" centos "*)
          printf '%s\n' "https://download.docker.com/linux/centos/docker-ce.repo"
          ;;
        *)
          die "Docker rpm repository is not configured for ${OS_ID} ${OS_VERSION}"
          ;;
      esac
      ;;
  esac
}

install_docker_rpm() {
  local repo_file repo_url

  repo_file="/etc/yum.repos.d/docker-ce.repo"
  repo_url="$(docker_rpm_repo_url)"

  log "installing docker rpm repository"
  mkdir -p "$(dirname "$repo_file")"
  curl -fsSL --retry 5 --retry-delay 2 "$repo_url" -o "$repo_file" \
    || die "failed to download Docker repo file: ${repo_url}"
  pkg_install docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
}

install_docker_zypper() {
  log "installing docker from the distribution repositories"
  pkg_install docker
}

install_docker_pacman() {
  log "installing docker from the Arch repositories"
  pkg_install docker docker-buildx
}

start_docker() {
  # NetworkManager can put docker0 back in its default zone after firewalld reloads.
  # Preserve Docker's zone in both the connection profile and firewalld before each start.
  if command -v firewall-cmd >/dev/null 2>&1 && systemctl is-active --quiet firewalld \
    && ip link show docker0 >/dev/null 2>&1 \
    && firewall-cmd --zone=docker --get-target >/dev/null 2>&1; then
    local connection
    if command -v nmcli >/dev/null 2>&1 && systemctl is-active --quiet NetworkManager; then
      connection="$(nmcli -g GENERAL.CONNECTION device show docker0)"
      if [ -n "$connection" ] && [ "$connection" != -- ]; then
        nmcli connection modify "$connection" connection.zone docker
      fi
    fi
    firewall-cmd --permanent --zone=docker --change-interface=docker0
    firewall-cmd --zone=docker --change-interface=docker0
  fi
  systemctl start docker
}

install_docker() {
  if command -v docker >/dev/null 2>&1; then
    ok "docker already installed"
  else
    local had_containerd=""
    command -v containerd >/dev/null 2>&1 && had_containerd=1
    case "$PKG" in
      apt) install_docker_apt ;;
      dnf|yum) install_docker_rpm ;;
      zypper) install_docker_zypper ;;
      pacman) install_docker_pacman ;;
      *) die "Docker installation is not supported with package manager: ${PKG}" ;;
    esac
    # Keep installer-managed Docker and containerd disabled at boot; start them only when building.
    systemctl disable docker.service docker.socket 2>/dev/null || true
    [ -n "$had_containerd" ] || systemctl disable containerd.service 2>/dev/null || true
  fi
  start_docker
  ok "docker running"
}

# Start Docker lazily for source builds; complete release artifacts require no local Docker installation.
ensure_docker() {
  if [ -z "$DOCKER_INSTALLED" ]; then
    host_builds_expected || unplanned_build_room
    install_docker
    DOCKER_INSTALLED=1
  else
    start_docker
  fi
}

# Recheck build space before falling back from release assets to local compilation.
# Stop before installing Docker if fallback builds lack space; warn mode permits continuation.
unplanned_build_room() {
  local path=/var/lib/containerd need=8192 line mount avail problem
  path_populated "$path" && need=2048
  line="$(df -Pk "$(existing_ancestor "$path")" 2>/dev/null | awk 'NR == 2 { print $6, $4 }')" || return 0
  read -r mount avail <<<"$line"
  [ -n "$avail" ] && [ "$avail" -lt $((need * 1024)) ] || return 0
  problem="building here what the release did not supply takes about ${need} MiB under ${path}, and ${mount} has $((avail / 1024)) MiB free"
  if [ "$FELIS_PREFLIGHT" = warn ]; then
    warn "${problem}; FELIS_PREFLIGHT=warn, building anyway"
    return 0
  fi
  die "${problem}; nothing has been built. Rerun the installer once the release's assets download, free space on ${mount}, or set FELIS_PREFLIGHT=warn to build anyway"
}

# Stop only runtimes started by this installer and preserve containerd with non-Docker namespaces.
stop_docker() {
  [ -n "$DOCKER_INSTALLED" ] || return 0
  systemctl stop docker docker.socket 2>/dev/null || true
  local ns
  ns="$(ctr --address /run/containerd/containerd.sock namespaces ls -q 2>/dev/null)" || return 0
  if ! printf '%s\n' "$ns" | grep -vE '^(moby.*)?$' >/dev/null; then
    systemctl stop containerd 2>/dev/null || true
  fi
}

# Keep NetworkPolicy and host firewalls enabled, adding required rules explicitly.

# Use the C locale because ufw translates its status output.
ufw_active() {
  command -v ufw >/dev/null 2>&1 || return 1
  [ "$(LC_ALL=C ufw status 2>/dev/null | sed -n 1p)" = "Status: active" ]
}

# Tag idempotent ufw rules with felis- comments so uninstall can identify them.
configure_extra_ports_firewall() {
  local protocol
  if ufw_active; then
    for protocol in tcp udp; do
      ufw allow "30000:32767/${protocol}" comment felis-k3s-extra-ports >/dev/null
    done
  fi
  command -v firewall-cmd >/dev/null 2>&1 || return 0
  systemctl is-active --quiet firewalld || return 0
  for protocol in tcp udp; do
    firewall-cmd --permanent --add-port="30000-32767/${protocol}"
  done
  firewall-cmd --reload
}

configure_k3s_firewall() {
  configure_extra_ports_firewall
  if ufw_active; then
    log "configuring ufw for k3s"
    ufw allow from "$POD_CIDR" comment felis-k3s-pods >/dev/null
    ufw allow from "$SERVICE_CIDR" comment felis-k3s-services >/dev/null
    ufw allow "${FELIS_PANEL_NODEPORT}/tcp" comment felis-panel >/dev/null
  fi
  command -v firewall-cmd >/dev/null 2>&1 || return 0
  systemctl is-active --quiet firewalld || return 0

  log "configuring firewalld for k3s"
  firewall-cmd --permanent --add-port=6443/tcp
  firewall-cmd --permanent --add-port="${FELIS_PANEL_NODEPORT}/tcp"
  firewall-cmd --permanent --zone=trusted --add-source="$POD_CIDR"
  firewall-cmd --permanent --zone=trusted --add-source="$SERVICE_CIDR"
  firewall-cmd --reload
}

install_k3s() {
  configure_k3s_firewall
  # Before the installer runs: a fresh k3s reads both drop-ins on its first start.
  K3S_RESTART_NEEDED=0
  write_k3s_config
  write_k3s_service_dropin

  local installer_ran=0
  if [ -x "$K3S_BIN" ] && [ ! -f "$K3S_UNIT_FILE" ]; then
    # Rerun the k3s installer if the binary exists without its service, preserving version and cluster data.
    "$K3S_BIN" --version >/dev/null 2>&1 \
      || die "${K3S_BIN} does not run and k3s.service is missing: an earlier k3s install was cut short. Remove ${K3S_BIN} and rerun to install k3s ${FELIS_K3S_VERSION}"
    log "k3s.service is missing beside ${K3S_BIN}: an earlier k3s install was cut short; finishing it around the binary in place"
    run_k3s_installer --keep-binary
    installer_ran=1
  elif [ -x "$K3S_BIN" ]; then
    local current
    current="$("$K3S_BIN" --version 2>/dev/null | awk 'NR == 1 { print $3 }')"
    if [ "$current" = "$FELIS_K3S_VERSION" ]; then
      ok "k3s ${current} already installed at ${K3S_BIN}"
    elif [ "$FELIS_UPGRADE_DEPS" != 1 ]; then
      ok "k3s ${current:-(version unreadable)} already installed at ${K3S_BIN}; this release pins ${FELIS_K3S_VERSION} (FELIS_UPGRADE_DEPS=1 moves it)"
    elif k3s_upgrade_allowed "$current" "$FELIS_K3S_VERSION"; then
      log "upgrading k3s ${current} to ${FELIS_K3S_VERSION}; running pods keep running while it restarts"
      stage_k3s_airgap_images
      run_k3s_installer
      installer_ran=1
    fi
  else
    log "installing k3s ${FELIS_K3S_VERSION} into ${K3S_BIN_DIR} (no traefik/servicelb)"
    stage_k3s_airgap_images
    run_k3s_installer
    installer_ran=1
  fi

  [ -x "$K3S_BIN" ] || die "k3s installation completed but ${K3S_BIN} is missing"
  strip_k3s_kubeconfig_mode_flag
  enable_k3s_metrics_server

  systemctl enable --now k3s
  # The installer restarts k3s itself; otherwise a changed drop-in or unit takes a
  # restart to load. Pods keep running across it (k3s leaves the containers be).
  if [ "$K3S_RESTART_NEEDED" = 1 ] && [ "$installer_ran" = 0 ]; then
    log "restarting k3s to load its new settings (${K3S_CONFIG_DROPIN}, ${K3S_SERVICE_DROPIN})"
    systemctl restart k3s
  fi
  export KUBECONFIG="$K3S_KUBECONFIG"
  log "waiting for the node to become Ready"
  wait_for_node_ready
  # k3s applies write-kubeconfig-mode as it writes the file; this covers a k3s that
  # has not rewritten it since the mode changed.
  chmod 0600 "$K3S_KUBECONFIG"
}

# Keep the node name stable because local-path volumes bind to it.
# Prefer pinned name, then kubelet certificate identity; never guess a name for an existing installation.
k3s_node_name() {
  local name=""
  if [ -f "$K3S_CONFIG_DROPIN" ]; then
    name="$(awk -F'"' '/^node-name:/ { print $2; exit }' "$K3S_CONFIG_DROPIN")"
  fi
  if [ -z "$name" ] && [ -f "$K3S_KUBELET_CERT" ]; then
    # OpenSSL 3 prints "CN=system:node:x", 1.1 "CN = system:node:x", older "/CN=...".
    # An unreadable certificate leaves the name empty (and warned about), not a failed run.
    name="$(openssl x509 -in "$K3S_KUBELET_CERT" -noout -subject 2>/dev/null |
      sed -n 's/.*CN *= *system:node:\([^,/]*\).*/\1/p' || true)"
  elif [ -z "$name" ] && [ ! -x "$K3S_BIN" ]; then
    name="$(uname -n | tr '[:upper:]' '[:lower:]')"
  fi
  printf '%s' "$name"
}

# Preserve distributed addresses and peer lists on reruns unless explicitly overridden.
recall_distributed_settings() {
  [ "${DISTRIBUTED:-0}" = 1 ] || return 0
  if [ -z "$NODE_EXTERNAL_IP" ] && [ -f "$K3S_CONFIG_DROPIN" ]; then
    NODE_EXTERNAL_IP="$(sed -n 's/^node-external-ip: "\(.*\)"$/\1/p' "$K3S_CONFIG_DROPIN" | sed -n 1p)"
  fi
  if [ -z "$WORKER_PEERS" ] && [ -s "$NODE_PEERS_FILE" ]; then
    WORKER_PEERS="$(head -n 1 "$NODE_PEERS_FILE")"
  fi
  return 0
}

# cluster_node_peers prints every node's internal and external address as an exact CIDR,
# comma-separated: the peer list `felis node` computes, read back from a running cluster.
cluster_node_peers() {
  "$K3S_BIN" kubectl --kubeconfig "$K3S_KUBECONFIG" get nodes \
    -o jsonpath='{range .items[*].status.addresses[?(@.type=="InternalIP")]}{.address}{"\n"}{end}{range .items[*].status.addresses[?(@.type=="ExternalIP")]}{.address}{"\n"}{end}' \
    | awk 'NF { print (index($0, ":") ? $0 "/128" : $0 "/32") }' | sort -u | paste -sd, -
}

# Keep kubeconfig root-only and restart k3s only after changed configuration.
write_k3s_config() {
  local file="$K3S_CONFIG_DROPIN" name tmp
  name="$(k3s_node_name)"
  mkdir -p "$(dirname "$file")"
  # Beside its destination for the directory's SELinux label (ensure_persistent_journal
  # has the story); k3s loads only *.yaml and *.yml from the directory.
  tmp="$(mktemp "${file}.XXXXXX")"
  remember_temp "$tmp"
  {
    echo "# Written by the Felis installer (deploy/bootstrap.sh); a rerun rewrites it."
    echo 'write-kubeconfig-mode: "0600"'
    if [ "${DISTRIBUTED:-0}" = 1 ]; then
      [ -n "$NODE_EXTERNAL_IP" ] || NODE_EXTERNAL_IP="$NODE_IP"
      printf 'node-external-ip: "%s"\n' "$NODE_EXTERNAL_IP"
      echo 'flannel-backend: "wireguard-native"'
      echo 'flannel-external-ip: true'
      echo 'agent-token-file: "/etc/rancher/k3s/felis-agent-token"'
      # Append to existing API-server hardening arguments in earlier config files.
      echo 'kube-apiserver-arg+:'
      echo '  - "enable-admission-plugins=NodeRestriction"'
      if [ "${CONTROLLER_HA:-0}" = 1 ]; then
        # The first controller starts etcd (converting k3s's SQLite on the restart); a
        # joining one joins it through an existing controller.
        if [ -n "$HA_JOIN_SERVER" ]; then
          printf 'server: "%s"\n' "$HA_JOIN_SERVER"
          echo 'token-file: "/etc/rancher/k3s/felis-server-token"'
        else
          echo 'cluster-init: true'
        fi
        if [ -n "$FELIS_HA_VIP" ]; then
          echo 'tls-san:'
          printf '  - "%s"\n' "${FELIS_HA_VIP%/*}"
        fi
      fi
      if [ ! -s /etc/rancher/k3s/felis-agent-token ]; then
        (umask 077; openssl rand -hex 32 > /etc/rancher/k3s/felis-agent-token)
      fi
    fi
    if [ -n "$name" ]; then
      printf 'node-name: "%s"\n' "$name"
    fi
  } > "$tmp"
  if [ -z "$name" ]; then
    warn "could not read this node's k3s name, so it is not pinned; a hostname change would orphan every volume"
  fi
  if [ -f "$file" ] && cmp -s "$tmp" "$file"; then
    rm -f "$tmp"
    restore_label "$file"
    ok "k3s settings already current${name:+ (node name ${name})}"
    return 0
  fi
  chmod 0600 "$tmp"
  mv "$tmp" "$file"
  K3S_RESTART_NEEDED=1
  log "wrote ${file}${name:+ (node name pinned to ${name})}"
}

# Use GOGC=50 to reduce memory growth; changed environment requires a k3s restart.
write_k3s_service_dropin() {
  local file="$K3S_SERVICE_DROPIN" tmp
  mkdir -p "$(dirname "$file")"
  # Beside its destination, like write_k3s_config's; systemd reads only *.conf from the
  # directory, so the temp name is never loaded.
  tmp="$(mktemp "${file}.XXXXXX")"
  remember_temp "$tmp"
  {
    echo "# Written by the Felis installer (deploy/bootstrap.sh); a rerun rewrites it."
    echo "[Service]"
    echo "Environment=GOGC=50"
  } > "$tmp"
  if [ -f "$file" ] && cmp -s "$tmp" "$file"; then
    rm -f "$tmp"
    restore_label "$file"
    ok "k3s service environment already current"
    return 0
  fi
  chmod 0644 "$tmp"
  mv "$tmp" "$file"
  systemctl daemon-reload
  K3S_RESTART_NEEDED=1
  log "wrote ${file} (GOGC=50)"
}

# Remove legacy command-line kubeconfig permissions because flags override config files.
strip_k3s_kubeconfig_mode_flag() {
  local unit="$K3S_UNIT_FILE" tmp
  [ -f "$unit" ] && grep -q -- '--write-kubeconfig-mode' "$unit" || return 0
  tmp="$(mktemp)"
  remember_temp "$tmp"
  awk '
    skip { skip = 0; next }
    index($0, "--write-kubeconfig-mode") { if (index($0, "=") == 0) skip = 1; next }
    { print }
  ' "$unit" > "$tmp"
  # Rewritten in place, so the unit keeps its owner, mode and SELinux label.
  cat "$tmp" > "$unit"
  rm -f "$tmp"
  systemctl daemon-reload
  K3S_RESTART_NEEDED=1
  log "dropped --write-kubeconfig-mode from ${unit}; the admin kubeconfig becomes root-only"
}

enable_k3s_metrics_server() {
  local unit="$K3S_UNIT_FILE" tmp
  [ -f "$unit" ] && grep -q -- 'metrics-server' "$unit" || return 0
  tmp="$(mktemp)"
  remember_temp "$tmp"
  awk -v quote="'" '
    function token(line) {
      gsub(quote, "", line)
      gsub(/["\\[:space:]]/, "", line)
      return line
    }
    token($0) == "--disable=metrics-server" { next }
    token($0) == "--disable" {
      flag = $0
      if (getline > 0) {
        if (token($0) != "metrics-server") { print flag; print }
      } else { print flag }
      next
    }
    { print }
  ' "$unit" > "$tmp"
  if cmp -s "$tmp" "$unit"; then rm -f "$tmp"; return 0; fi
  cat "$tmp" > "$unit"
  rm -f "$tmp"
  systemctl daemon-reload
  K3S_RESTART_NEEDED=1
  log "enabled metrics-server in ${unit}"
}

# Use the selected release installer; --keep-binary skips replacement while retaining policy and service setup.
run_k3s_installer() { # [--keep-binary]
  local skip=""
  [ "${1:-}" != --keep-binary ] || skip=binary
  curl -sfL --retry 5 --retry-delay 2 "https://raw.githubusercontent.com/k3s-io/k3s/${FELIS_K3S_VERSION}/install.sh" | \
    INSTALL_K3S_SKIP_DOWNLOAD="$skip" \
    INSTALL_K3S_VERSION="$FELIS_K3S_VERSION" \
    INSTALL_K3S_BIN_DIR="$K3S_BIN_DIR" \
    INSTALL_K3S_EXEC="--disable traefik --disable servicelb" \
    sh -
}

# Verify airgap checksums and stage under non-tarball temporary names so k3s cannot import partial downloads.
stage_k3s_airgap_images() {
  local arch base file sums want have tmp
  arch="$(felis_asset_arch)" || return 0
  base="https://github.com/k3s-io/k3s/releases/download/${FELIS_K3S_VERSION}"
  file="k3s-airgap-images-${arch}.tar.zst"
  sums="$(curl -fsSL --retry 5 --retry-delay 2 "${base}/sha256sum-${arch}.txt")" || sums=""
  want="$(awk -v n="$file" '$2 == n { print $1; exit }' <<<"$sums")"
  if ! [[ "$want" =~ ^[0-9a-f]{64}$ ]]; then
    warn "k3s ${FELIS_K3S_VERSION} lists no sha256 for ${file}; k3s pulls its own images from Docker Hub instead"
    return 0
  fi
  if [ -f "${K3S_IMAGES_DIR}/${file}" ] && [ "$(sha256sum <"${K3S_IMAGES_DIR}/${file}" | cut -d' ' -f1)" = "$want" ]; then
    ok "k3s ${FELIS_K3S_VERSION}'s images already staged"
    return 0
  fi
  mkdir -p "$K3S_IMAGES_DIR"
  tmp="$(mktemp "${K3S_IMAGES_DIR}/.${file}.XXXXXX")"
  remember_temp "$tmp"
  log "downloading k3s ${FELIS_K3S_VERSION}'s own images (${file})"
  if ! curl -fsSL --retry 5 --retry-delay 2 -o "$tmp" "${base}/${file}"; then
    rm -f "$tmp"
    warn "could not download ${file}; k3s pulls its own images from Docker Hub instead"
    return 0
  fi
  have="$(sha256sum <"$tmp" | cut -d' ' -f1)"
  if [ "$have" != "$want" ]; then
    rm -f "$tmp"
    warn "${file} hashes to ${have}, but k3s ${FELIS_K3S_VERSION}'s sha256sum-${arch}.txt says ${want}; k3s pulls its own images from Docker Hub instead"
    return 0
  fi
  chmod 0644 "$tmp"
  mv -f "$tmp" "${K3S_IMAGES_DIR}/${file}"
  ok "staged k3s ${FELIS_K3S_VERSION}'s images for its first start"
}

# Permit k3s upgrades one minor at a time; preserve newer installed versions.
k3s_upgrade_allowed() {
  local current="$1" want="$2" cur_major cur_minor want_major want_minor rest
  IFS=. read -r cur_major cur_minor rest <<<"${current#v}"
  IFS=. read -r want_major want_minor rest <<<"${want#v}"
  case "${cur_major}${cur_minor}${want_major}${want_minor}" in
    ""|*[!0-9]*) die "cannot compare the installed k3s '${current}' with ${want}; upgrade it by hand (https://docs.felismc.com/en/operations/upgrade#component-versions)" ;;
  esac
  if version_newer "$current" "$want"; then
    ok "k3s ${current} is newer than the pinned ${want}; left as it is"
    return 1
  fi
  if [ "$cur_major" != "$want_major" ] || [ "$((want_minor - cur_minor))" -gt 1 ]; then
    die "k3s ${current} -> ${want} skips a minor version, and Kubernetes upgrades one minor at a time. Rerun with FELIS_K3S_VERSION set to the newest v${cur_major}.$((cur_minor + 1)).x+k3sN release first (https://github.com/k3s-io/k3s/releases)"
  fi
  return 0
}

# Wait for node readiness after installation or registry-mirror restarts before applying dependent resources.
wait_for_node_ready() {
  local _
  for _ in $(seq 1 60); do
    if kube get nodes 2>/dev/null | grep ' Ready ' >/dev/null; then
      ok "k3s node Ready"
      return 0
    fi
    sleep 5
  done
  kube get nodes || true
  die "k3s node did not become Ready in time"
}

# Seed the registry image first, then mirror built images so containerd imports are only a first-boot cache.

# Restart k3s only when registry mirror configuration changes.
# K3S_REGISTRIES_FILE is a variable so bootstrap_test.sh can point the function at a scratch file.
configure_registry_mirror() {
  local file="$K3S_REGISTRIES_FILE" tmp endpoints registry_ip=""
  tmp="$(mktemp)"
  remember_temp "$tmp"
  endpoints="      - \"http://${REGISTRY_PUSH_HOST}\""
  # With controller high availability the registry runs beside the control plane, on
  # whichever controller that is: pull through its Service first, then the loopback port.
  if [ "${CONTROLLER_HA:-0}" = 1 ] && [ -f "$K3S_KUBECONFIG" ]; then
    registry_ip="$("$K3S_BIN" kubectl --kubeconfig "$K3S_KUBECONFIG" -n "$CONTROL_NS" get service registry -o jsonpath='{.spec.clusterIP}' 2>/dev/null || true)"
  fi
  if [ -n "$registry_ip" ]; then
    endpoints="      - \"http://${registry_ip}:5000\""$'\n'"${endpoints}"
  fi
  cat > "$tmp" <<EOF
mirrors:
  "${REGISTRY_URL}":
    endpoint:
${endpoints}
EOF
  if [ -f "$file" ] && cmp -s "$tmp" "$file"; then
    rm -f "$tmp"
    ok "registry mirror already configured (${REGISTRY_URL} -> http://${REGISTRY_PUSH_HOST})"
    return 0
  fi
  mkdir -p "$(dirname "$file")"
  mv "$tmp" "$file"
  log "restarting k3s to load the registry mirror (${REGISTRY_URL} -> http://${REGISTRY_PUSH_HOST})"
  systemctl restart k3s
  export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
  wait_for_node_ready
}

# Use CRI pulls for renderer-pinned registry/PostgreSQL images to preserve their digest references.
import_platform_images() {
  local image images
  # Read the list whole before matching: `ctr images ls | grep -q` dies of SIGPIPE under
  # pipefail once grep stops reading a list longer than one pipe buffer.
  images="$(k3s_cmd ctr images ls -q 2>/dev/null || true)"
  for image in "$REGISTRY_IMAGE" "$POSTGRES_IMAGE"; do
    if grep -qxF "$(pinned_image_ref "$image")" <<<"$images"; then
      ok "${image} already in k3s containerd"
      continue
    fi
    log "pulling ${image} into k3s containerd"
    if k3s_cmd crictl pull "$image" >/dev/null; then
      ok "${image} pulled"
    else
      warn "could not pull ${image}: its Deployment starts only if the node can pull it from Docker Hub; on an air-gapped box import it by hand (https://docs.felismc.com/en/operations/troubleshooting#build-pods-not-starting)"
    fi
  done
}

# pinned_image_ref <image> prints the name containerd lists a digest-pinned image under
# once CRI has pulled it: the repository and the digest, with the tag dropped.
pinned_image_ref() {
  printf '%s@%s\n' "${1%%:*}" "${1#*@}"
}

# Pin registry, sidecar, and database images locally to avoid a bootstrap dependency after kubelet GC.
# Use the CRI pinned label, unpinning obsolete versions before upgrades.
pin_platform_images() {
  local ref registry_ref postgres_ref
  registry_ref="$(pinned_image_ref "$REGISTRY_IMAGE")"
  postgres_ref="$(pinned_image_ref "$POSTGRES_IMAGE")"
  while read -r ref; do
    case "$ref" in
      "$FELIS_IMAGE"|"$registry_ref"|"$postgres_ref") ;;
      */felis/felis:*|docker.io/library/registry[:@]*|docker.io/library/postgres[:@]*)
        k3s_cmd ctr images label "$ref" io.cri-containerd.pinned= >/dev/null 2>&1 || true ;;
    esac
  done < <(k3s_cmd ctr images ls -q 2>/dev/null || true)
  for ref in "$FELIS_IMAGE" "$registry_ref" "$postgres_ref"; do
    if k3s_cmd ctr images label "$ref" io.cri-containerd.pinned=pinned >/dev/null 2>&1; then
      ok "pinned ${ref} in containerd (exempt from kubelet image GC)"
    else
      warn "could not pin ${ref} in containerd: if the kubelet's image GC collects it, its pod cannot restart until it is re-imported (https://docs.felismc.com/en/operations/troubleshooting#build-pods-not-starting)"
    fi
  done
}

# Pin user image digests before overwriting tags; otherwise sleeping worlds could upgrade irreversibly.
# A fresh install has no servers to pin; running servers may roll once onto their current build.
pin_user_server_images() {
  kube get crd minecraftservers.felis.lolicon.best >/dev/null 2>&1 || return 0
  # The registry answers the lookups, and a k3s restart above may have left its pod
  # still starting. A registry that never comes up fails the lookups below, loudly.
  kube -n "$CONTROL_NS" rollout status deployment/registry --timeout=180s >/dev/null 2>&1 || true
  if "$HOST_BIN" pin-images --namespace "$MINECRAFT_NS" \
      --registry "$REGISTRY_URL" --endpoint "$REGISTRY_PUSH_HOST"; then
    ok "user servers pinned to the builds they run"
  else
    warn "could not pin every user server listed above to its current build: each one still names a tag this run is about to point at a new build, so its next start may open its world with a newer Minecraft version. Pin it before starting it again: sudo felis pin-images (https://docs.felismc.com/en/operations/troubleshooting#game-images)"
  fi
}

# repo_slug prints the owner/name of FELIS_REPO_URL, for the REST API.
repo_slug() {
  printf '%s\n' "$FELIS_REPO_URL" | sed -e 's#^.*github\.com[:/]##' -e 's#\.git$##'
}

# Show token guidance only for forks because inaccessible private repositories also return 404.
fork_token_hint() {
  [ "$(repo_slug | tr '[:upper:]' '[:lower:]')" != "felismc/felis" ] || return 0
  printf ' If %s is a private fork, set FELIS_GITHUB_TOKEN to a token with read access to it.' "$FELIS_REPO_URL"
}

# Pass GitHub tokens through stdin or credential helpers, never process arguments or URLs.
github_api() {
  local url="https://api.github.com/$1"
  local ua="felis-bootstrap (+${FELIS_REPO_URL})"
  if [ -n "$FELIS_GITHUB_TOKEN" ]; then
    printf 'header = "Authorization: Bearer %s"\n' "$FELIS_GITHUB_TOKEN" \
      | curl -fsSL --retry 5 --retry-delay 2 --config - \
          -A "$ua" -H "Accept: application/vnd.github+json" "$url"
  else
    curl -fsSL --retry 5 --retry-delay 2 \
      -A "$ua" -H "Accept: application/vnd.github+json" "$url"
  fi
}

# Resolve latest through the same stable-release endpoint as felis update.
github_latest_tag() {
  local json tag
  # Fetch first, filter second — the SIGPIPE reason documented on resolve_latest_game_jars.
  json="$(github_api "repos/$(repo_slug)/releases/latest")" || return 1
  tag="$(printf '%s' "$json" | grep -o '"tag_name"[[:space:]]*:[[:space:]]*"[^"]*"' || true)"
  tag="${tag%%$'\n'*}"
  tag="${tag#*:}"        #  "v1.2.3"
  tag="${tag#*\"}"       # v1.2.3"
  tag="${tag%\"}"        # v1.2.3
  [ -n "$tag" ] || return 1
  printf '%s\n' "$tag"
}

# Unsupported prebuilt architectures fall back to compiling the selected tag.
felis_asset_arch() {
  case "$(uname -m)" in
    x86_64|amd64) printf 'amd64\n' ;;
    aarch64|arm64) printf 'arm64\n' ;;
    *) return 1 ;;
  esac
}

# load_release_json keeps release tag $1's JSON in RELEASE_JSON, fetching it only for a tag it
# does not hold yet.
load_release_json() {
  [ "$RELEASE_JSON_TAG" != "$1" ] || return 0
  RELEASE_JSON="$(github_api "repos/$(repo_slug)/releases/tags/$1")" || return 1
  RELEASE_JSON_TAG="$1"
}

# Match exact asset names and their own URLs after flattening JSON; nested uploader IDs are unrelated.
github_asset_id() {
  local tag="$1" name="$2" id
  # Fetch first, filter second — the SIGPIPE reason documented on resolve_latest_game_jars.
  load_release_json "$tag" || return 1
  id="$(printf '%s' "$RELEASE_JSON" | tr -d '\n' | tr '{' '\n' \
    | grep "\"name\":[[:space:]]*\"${name}\"" \
    | grep -o 'releases/assets/[0-9]\{1,\}' | sed -n 1p)" || true
  id="${id##*/}"
  [ -n "$id" ] || return 1
  printf '%s\n' "$id"
}

# Download binary assets without forwarding Authorization across redirects; return failures for same-tag source fallback.
download_release_asset() {
  local tag="$1" name="$2" dest="$3" id ua url rc attempt
  ua="felis-bootstrap (+${FELIS_REPO_URL})"
  # Loaded here, in this shell, so the lookup's subshell below finds it cached.
  load_release_json "$tag" || return 1
  id="$(github_asset_id "$tag" "$name")" || return 1
  url="https://api.github.com/repos/$(repo_slug)/releases/assets/${id}"
  log "downloading ${name} from release ${tag}"
  for attempt in 1 2 3; do
    rc=0
    if [ -n "$FELIS_GITHUB_TOKEN" ]; then
      printf 'header = "Authorization: Bearer %s"\n' "$FELIS_GITHUB_TOKEN" \
        | curl -fsSL --retry 5 --retry-delay 2 --speed-limit 1024 --speed-time 60 --config - \
            -A "$ua" -H "Accept: application/octet-stream" -o "$dest" "$url" || rc=$?
    else
      curl -fsSL --retry 5 --retry-delay 2 --speed-limit 1024 --speed-time 60 \
        -A "$ua" -H "Accept: application/octet-stream" -o "$dest" "$url" || rc=$?
    fi
    [ "$rc" -eq 0 ] && [ -s "$dest" ] && return 0
    # curl -f leaves a PARTIAL file behind when a transfer dies mid-stream, so a failed
    # download must not hand the caller something it could mistake for a complete one.
    rm -f "$dest"
    if [ "$attempt" -lt 3 ]; then
      warn "downloading ${name} failed (attempt ${attempt}/3, curl exit ${rc}); retrying"
      sleep 5
    fi
  done
  return 1
}

# Reject release assets missing from SHA256SUMS or whose checksum differs.
verify_release_checksum() {
  local tag="$1" name="$2" file="$3" sums want have
  sums="$(mktemp)"
  remember_temp "$sums"
  if ! download_release_asset "$tag" SHA256SUMS "$sums"; then
    rm -f "$sums"
    warn "release ${tag} publishes no SHA256SUMS, so ${name} cannot be verified; building ${tag} from source on this host instead"
    return 1
  fi
  # sha256sum's text-mode line is "<hash>  <name>", binary mode "<hash> *<name>".
  want="$(awk -v n="$name" '$2 == n || $2 == "*" n { print $1; exit }' "$sums")"
  rm -f "$sums"
  if [ -z "$want" ]; then
    warn "release ${tag}'s SHA256SUMS does not list ${name}; building ${tag} from source on this host instead"
    return 1
  fi
  # Hash stdin, never the path: the same sha256sum escaping install_via_plugins avoids.
  have="$(sha256sum <"$file" | cut -d' ' -f1)"
  if [ "$have" != "$want" ]; then
    warn "downloaded ${name} hashes to ${have}, but release ${tag}'s SHA256SUMS says ${want}; discarding it and building ${tag} from source on this host instead"
    return 1
  fi
  ok "${name} matches release ${tag}'s SHA256SUMS"
}

# Use prebuilt binaries only for unpinned release installs; explicit refs, staged trees, and dev build from source.
use_release_binary() {
  [ -z "${FELIS_SKIP_FETCH:-}" ] || return 1
  [ -z "$FELIS_REF_PINNED" ] || return 1
  [ "$FELIS_VERSION_BOOTSTRAP" = "release" ]
}

# Call binary installation outside conditionals to preserve errexit; unavailable assets fall back to same-tag compilation.
download_release_binary() {
  local arch asset tmp got
  if ! arch="$(felis_asset_arch)"; then
    warn "no prebuilt felis binary for architecture $(uname -m); building ${FELIS_REF} from source on this host instead"
    return 0
  fi
  asset="felis-linux-${arch}"

  # Convergence check, and the cheapest one available: no API call, no download, and it asks
  # the exact question that matters. Reruns are the common case for this installer.
  if [ "${FELIS_FORCE_UPDATE:-0}" != 1 ] && [ -x "$HOST_BIN" ] && [ "$("$HOST_BIN" version 2>/dev/null | sed -n 1p)" = "felis ${FELIS_REF}" ]; then
    HAVE_PREBUILT_BINARY=1
    ok "host binary is already ${FELIS_REF}; skipping the download"
    return 0
  fi

  # Validate beside HOST_BIN because hardened hosts may mount /tmp noexec.
  # Keep private assets out of world-readable temporary directories.
  mkdir -p "$(dirname "$HOST_BIN")"
  tmp="$(mktemp "$(dirname "$HOST_BIN")/.felis-download.XXXXXX")"
  remember_temp "$tmp"
  if ! download_release_asset "$FELIS_REF" "$asset" "$tmp"; then
    rm -f "$tmp"
    # Fallback must compile the selected tag, never switch to dev/main.
    warn "release ${FELIS_REF} publishes no usable ${asset}; building ${FELIS_REF} from source on this host instead"
    return 0
  fi

  # Verify SHA256SUMS before executing the downloaded binary as root.
  if ! verify_release_checksum "$FELIS_REF" "$asset" "$tmp"; then
    rm -f "$tmp"
    return 0
  fi
  chmod 0755 "$tmp"

  # Execute once to validate architecture, integrity, and build stamp.
  got="$("$tmp" version 2>/dev/null | sed -n 1p || true)"
  if [ "$got" != "felis ${FELIS_REF}" ]; then
    rm -f "$tmp"
    warn "downloaded ${asset} reports '${got:-nothing}' rather than 'felis ${FELIS_REF}'; building ${FELIS_REF} from source on this host instead"
    return 0
  fi

  # Use install rather than rename so SELinux applies the destination executable label.
  keep_previous_host_binary
  rm -f "$HOST_BIN"
  install -m 0755 "$tmp" "$HOST_BIN"
  rm -f "$tmp"
  command -v restorecon >/dev/null 2>&1 && restorecon "$HOST_BIN" >/dev/null 2>&1 || true

  HAVE_PREBUILT_BINARY=1
  ok "installed ${asset} ${FELIS_REF} at ${HOST_BIN}"
}

# Use release images and plugins only after verifying SHA256SUMS.

# load_artifact_sums reads the SHA256SUMS the artifacts are checked against: the directory's,
# or release ARTIFACT_TAG's. It fails when there is none.
load_artifact_sums() {
  local tmp
  ARTIFACT_SUMS=""
  if [ "$ARTIFACT_MODE" = dir ]; then
    ARTIFACT_SUMS="$(cat "${FELIS_ARTIFACT_DIR}/SHA256SUMS" 2>/dev/null)" || ARTIFACT_SUMS=""
  else
    tmp="$(mktemp)"
    remember_temp "$tmp"
    if download_release_asset "$ARTIFACT_TAG" SHA256SUMS "$tmp"; then
      ARTIFACT_SUMS="$(cat "$tmp")"
    fi
    rm -f "$tmp"
  fi
  [ -n "$ARTIFACT_SUMS" ]
}

# Accept SHA256SUMS text/binary formats without regex intervals unsupported by mawk.
artifact_sum() {
  awk -v n="$1" '($2 == n || $2 == "*" n) && length($1) == 64 && $1 !~ /[^0-9a-f]/ { print $1; exit }' <<<"$ARTIFACT_SUMS"
}

# Reuse checksum-matching local assets or cached downloads.
# It warns and fails, leaving nothing half-written, when SHA256SUMS does not list the name, the file is missing, or its bytes differ.
artifact_fetch() {
  local name="$1" want have file partial
  ARTIFACT_FILE=""
  want="$(artifact_sum "$name")"
  if [ -z "$want" ]; then
    warn "SHA256SUMS lists no ${name}"
    return 1
  fi
  if [ "$ARTIFACT_MODE" = dir ]; then
    file="${FELIS_ARTIFACT_DIR}/${name}"
    if [ ! -f "$file" ]; then
      warn "${file} is missing"
      return 1
    fi
  else
    file="${ARTIFACT_CACHE}/${name}"
  fi
  if [ -f "$file" ]; then
    have="$(sha256sum <"$file" | cut -d' ' -f1)"
    if [ "$have" = "$want" ]; then
      ARTIFACT_FILE="$file"
      return 0
    fi
    if [ "$ARTIFACT_MODE" = dir ]; then
      warn "${file} hashes to ${have}, but SHA256SUMS says ${want}"
      return 1
    fi
    rm -f "$file"
  fi
  install -d -m 0700 "$ARTIFACT_CACHE"
  partial="${file}.partial"
  if ! download_release_asset "$ARTIFACT_TAG" "$name" "$partial"; then
    rm -f "$partial"
    warn "could not download ${name} from release ${ARTIFACT_TAG}"
    return 1
  fi
  have="$(sha256sum <"$partial" | cut -d' ' -f1)"
  if [ "$have" != "$want" ]; then
    rm -f "$partial"
    warn "downloaded ${name} hashes to ${have}, but release ${ARTIFACT_TAG}'s SHA256SUMS says ${want}"
    return 1
  fi
  mv -f "$partial" "$file"
  ARTIFACT_FILE="$file"
}

# Explicit offline artifact directories fail on unusable assets; downloaded releases may fall back to local builds.
artifact_unusable() {
  [ "$ARTIFACT_MODE" != dir ] || die "FELIS_ARTIFACT_DIR: $1"
  warn "$1; $2"
}

# Stage and verify explicit artifact binaries beside HOST_BIN; never fall back to compilation.
install_artifact_binary() {
  local arch name tmp got
  ARTIFACT_MODE=dir
  arch="$(felis_asset_arch)" || die "FELIS_ARTIFACT_DIR: Felis publishes no binary for $(uname -m)"
  name="felis-linux-${arch}"
  load_artifact_sums || die "FELIS_ARTIFACT_DIR: ${FELIS_ARTIFACT_DIR} holds no SHA256SUMS"
  [ -n "$(artifact_sum "$name")" ] || die "FELIS_ARTIFACT_DIR: SHA256SUMS lists no ${name}"
  mkdir -p "$(dirname "$HOST_BIN")"
  tmp="$(mktemp "$(dirname "$HOST_BIN")/.felis-download.XXXXXX")"
  remember_temp "$tmp"
  cp "${FELIS_ARTIFACT_DIR}/${name}" "$tmp" || die "FELIS_ARTIFACT_DIR: cannot read ${FELIS_ARTIFACT_DIR}/${name}"
  [ "$(sha256sum <"$tmp" | cut -d' ' -f1)" = "$(artifact_sum "$name")" ] \
    || die "FELIS_ARTIFACT_DIR: ${name} does not match SHA256SUMS"
  chmod 0755 "$tmp"
  got="$("$tmp" version 2>/dev/null | sed -n 1p || true)"
  case "$got" in
    "felis "?*) ;;
    *) die "FELIS_ARTIFACT_DIR: ${name} reports '${got:-nothing}' as its version" ;;
  esac
  FELIS_VERSION="${got#felis }"
  if [ -x "$HOST_BIN" ] && cmp -s "$tmp" "$HOST_BIN"; then
    ok "host binary is already felis ${FELIS_VERSION} from ${FELIS_ARTIFACT_DIR}"
  else
    keep_previous_host_binary
    rm -f "$HOST_BIN"
    install -m 0755 "$tmp" "$HOST_BIN"
    command -v restorecon >/dev/null 2>&1 && restorecon "$HOST_BIN" >/dev/null 2>&1 || true
    ok "installed felis ${FELIS_VERSION} from ${FELIS_ARTIFACT_DIR}"
  fi
  rm -f "$tmp"
  HAVE_PREBUILT_BINARY=1
}

# Select assets for the installed binary's release; source builds or releases without checksums build locally.
select_release_artifacts() {
  local v
  [ "$ARTIFACT_MODE" != dir ] || return 0
  [ -n "$HAVE_PREBUILT_BINARY" ] || return 0
  v="$("$HOST_BIN" version 2>/dev/null | sed -n 1p || true)"
  v="${v#felis }"
  # A release's version is its tag; a dev or pinned build's names no release.
  [[ "$v" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || return 0
  ARTIFACT_MODE=release
  ARTIFACT_TAG="$v"
  if load_artifact_sums; then
    ok "release ${v}'s prebuilt images and Velocity plugin are installed as published"
    return 0
  fi
  ARTIFACT_MODE=""
  ARTIFACT_TAG=""
  warn "release ${v} publishes no SHA256SUMS, so none of its images can be checked; building them on this host instead (this installs Docker and needs about 8 GiB more under /var/lib/containerd)"
}

# Reset host Git helpers and provide tokens through an inline helper; disable prompts and never persist credentials.
git_auth() {
  if [ -n "$FELIS_GITHUB_TOKEN" ]; then
    GIT_TERMINAL_PROMPT=0 git -c 'credential.helper=' \
        -c 'credential.helper=!f() { printf "username=x-access-token\npassword=%s\n" "$FELIS_GITHUB_TOKEN"; }; f' "$@"
  else
    GIT_TERMINAL_PROMPT=0 git "$@"
  fi
}

# resolve_install_ref decides WHICH commit to build and, on the release channel, what to
# stamp it as. Runs before fetch_source because it chooses that fetch's ref.
resolve_install_ref() {
  # Idempotent: main calls it early to fail fast on a missing token, and fetch_source
  # calls it again because the nano path reaches fetch_source by its own route.
  [ -n "${REF_RESOLVED:-}" ] && return 0
  REF_RESOLVED=1
  if [ -n "$FELIS_REF" ]; then
    ok "building the pinned ref ${FELIS_REF}"
    return 0
  fi
  case "$FELIS_VERSION_BOOTSTRAP" in
    release)
      if [ -n "$FELIS_RELEASE" ]; then
        # Require an explicitly requested release to exist and install its matching assets.
        load_release_json "$FELIS_RELEASE" || die "could not find the published Felis release ${FELIS_RELEASE}.
  Check the tag against the repository's releases page.$(fork_token_hint)"
        FELIS_REF="$FELIS_RELEASE"
      else
        log "resolving the newest published Felis release"
        FELIS_REF="$(github_latest_tag)" || die "could not resolve the newest Felis release from api.github.com.$(fork_token_hint)
  If no release has been published yet, set FELIS_VERSION_BOOTSTRAP=dev to build main instead."
      fi
      # A release is its tag, so its stamp is final and stamp_version leaves it alone.
      FELIS_VERSION="$FELIS_REF"
      ok "release channel: ${FELIS_REF}"
      ;;
    dev)
      FELIS_REF="main"
      # Use v0.0.0 when development builds have no release tag so version comparison remains parseable.
      FELIS_VERSION_BASE="$(github_latest_tag 2>/dev/null || true)"
      [ -n "$FELIS_VERSION_BASE" ] || FELIS_VERSION_BASE="v0.0.0"
      ok "dev channel: main (ahead of ${FELIS_VERSION_BASE})"
      ;;
    *)
      die "FELIS_VERSION_BOOTSTRAP must be 'release' or 'dev', got '${FELIS_VERSION_BOOTSTRAP}'"
      ;;
  esac
}

# Stamp checkout SHAs as build metadata; preserve release stamps and avoid shallow-clone git-describe fallbacks.
stamp_version() {
  local sha
  [ -n "$FELIS_VERSION" ] && return 0
  sha="$(git -C "$SRC_DIR" rev-parse --short HEAD 2>/dev/null || true)"
  [ -n "$sha" ] || sha="unknown"
  [ -n "$FELIS_VERSION_BASE" ] || FELIS_VERSION_BASE="v0.0.0"
  FELIS_VERSION="${FELIS_VERSION_BASE}+g${sha}"
  ok "build stamp: ${FELIS_VERSION}"
}

fetch_source() {
  if [ -n "${FELIS_SKIP_FETCH:-}" ]; then
    [ -d "$SRC_DIR" ] || die "FELIS_SKIP_FETCH set but ${SRC_DIR} does not exist"
    ok "skipping fetch; using pre-staged source at ${SRC_DIR}"
    # Whatever is staged is what gets built, so the channel has no say here and no
    # release lookup is made. stamp_version reads the staged checkout's own SHA.
    stamp_version
    return 0
  fi
  resolve_install_ref
  local work failed
  failed="could not check out ${FELIS_REF} from ${FELIS_REPO_URL}: check that this ref exists there and that this host can reach it.$(fork_token_hint)"
  if [ -d "${SRC_DIR}/.git" ]; then
    log "updating source in ${SRC_DIR}"
    checkout_ref "$SRC_DIR" || die "$failed"
  else
    # Replace stale non-Git source directories only after a complete fresh checkout exists.
    log "cloning ${FELIS_REPO_URL} (${FELIS_REF})"
    mkdir -p "$(dirname "$SRC_DIR")"
    # A run killed outright (no EXIT trap) leaves its half-made checkout behind. The run lock means none of these belongs to a run still going.
    rm -rf -- "${SRC_DIR}".new.*
    work="$(mktemp -d "${SRC_DIR}.new.XXXXXX")"
    chmod 755 "$work"
    if ! { git -C "$work" init -q && git -C "$work" remote add origin "$FELIS_REPO_URL" \
           && checkout_ref "$work"; }; then
      rm -rf "$work"
      die "$failed"
    fi
    rm -rf "$SRC_DIR"
    mv "$work" "$SRC_DIR"
  fi
  stamp_version
  ok "source ready at ${SRC_DIR}"
}

# Explicitly check out the requested ref; abbreviated SHAs may require full history.
checkout_ref() {
  local commit
  if git_auth -C "$1" fetch -q --depth 1 origin "$FELIS_REF" 2>/dev/null; then
    git -C "$1" checkout -q -f FETCH_HEAD
    return
  fi
  if [ -f "$1/.git/shallow" ]; then
    git_auth -C "$1" fetch -q --unshallow --tags origin '+refs/heads/*:refs/remotes/origin/*' || return 1
  else
    git_auth -C "$1" fetch -q --tags origin '+refs/heads/*:refs/remotes/origin/*' || return 1
  fi
  commit="$(git -C "$1" rev-parse -q --verify "refs/remotes/origin/${FELIS_REF}^{commit}" \
    || git -C "$1" rev-parse -q --verify "${FELIS_REF}^{commit}")" || return 1
  git -C "$1" checkout -q -f "$commit"
}

install_embedded_binary() {
  local src
  src="${FELIS_BOOTSTRAP_BINARY:-}"
  [ -n "$src" ] || die "FELIS_BOOTSTRAP_BINARY is not set; cannot install the embedded setup binary"
  [ -x "$src" ] || die "FELIS_BOOTSTRAP_BINARY is not executable: ${src}"

  mkdir -p "$(dirname "$HOST_BIN")"
  if [ "$(readlink -f "$src")" != "$(readlink -f "$HOST_BIN" 2>/dev/null || true)" ]; then
    log "installing current felis binary onto the host (${HOST_BIN})"
    keep_previous_host_binary
    install -m 0755 "$src" "$HOST_BIN"
  else
    ok "host binary already installed at ${HOST_BIN}"
  fi
  HAVE_PREBUILT_BINARY=1
}

build_image_from_binary() {
  local tmp
  tmp="$(mktemp -d)"
  remember_temp "$tmp"
  cp "$HOST_BIN" "${tmp}/felis"
  # Wrap the same static binary as CI, including downloaded releases.
  cat > "${tmp}/Dockerfile" <<'EOF'
FROM gcr.io/distroless/static-debian12:nonroot
ENV PATH=/usr/local/bin:/usr/bin:/bin
COPY felis /usr/local/bin/felis
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/felis"]
EOF
  chmod 0755 "${tmp}/felis"

  log "building ${FELIS_IMAGE} from the current felis binary"
  docker build --progress=plain -t "$FELIS_IMAGE" "$tmp"
  rm -rf "$tmp"
}

verify_image_starts() {
  log "verifying ${FELIS_IMAGE} starts"
  docker run --rm --user 1000:1000 --entrypoint /usr/local/bin/felis "$FELIS_IMAGE" help >/dev/null
}

build_image_from_source() {
  log "building ${FELIS_IMAGE} ${FELIS_VERSION:+(${FELIS_VERSION}) }(this compiles the Go binary; first run is slow)"
  # Require a build stamp because an unstamped dev binary cannot participate in update comparison.
  docker build --progress=plain -t "$FELIS_IMAGE" \
    --build-arg FELIS_VERSION="${FELIS_VERSION:-dev}" "$SRC_DIR"

  log "extracting the felis binary onto the host (${HOST_BIN})"
  local cid
  cid="$(docker create "$FELIS_IMAGE")"
  remember_container "$cid"
  keep_previous_host_binary
  docker cp "${cid}:/usr/local/bin/felis" "$HOST_BIN"
  docker rm "$cid" >/dev/null
  chmod 0755 "$HOST_BIN"
}

# Use release-specific image tags so rollout undo can recover the previous build.
# Replace '+' with '-' for image tags; reruns of one version reuse its tag and restart rebuilt Pods.
resolve_felis_image() {
  local v
  [ -z "$FELIS_IMAGE" ] || return 0
  v="$FELIS_VERSION"
  if [ -z "$v" ] && [ -n "$HAVE_PREBUILT_BINARY" ]; then
    v="$("$HOST_BIN" version 2>/dev/null | sed -n 1p || true)"
    v="${v#felis }"
  fi
  FELIS_IMAGE="${REGISTRY_URL}/felis/felis:$(image_tag_for_version "$v")"
  ok "control-plane image: ${FELIS_IMAGE}"
}

# image_tag_for_version turns a felis version into an image tag: every character a tag may not
# hold becomes '-'. An unknown version ("dev" is what an unstamped binary reports) is :demo.
image_tag_for_version() {
  local v
  v="$(printf '%s' "$1" | tr -c 'A-Za-z0-9_.-' '-')"
  case "$v" in
    ""|dev|[!A-Za-z0-9_]*) printf 'demo' ;;
    *) printf '%s' "${v:0:128}" ;;
  esac
}

build_image() {
  if role_prebuilt felis; then
    ok "${FELIS_IMAGE} is the release's own image; nothing to build"
    return 0
  fi
  ensure_docker
  # Use HAVE_PREBUILT_BINARY across TUI and download paths; source fallback clears it.
  if [ -n "$HAVE_PREBUILT_BINARY" ]; then
    build_image_from_binary
  else
    build_image_from_source
  fi
  verify_image_starts

  log "importing ${FELIS_IMAGE} into k3s containerd"
  remove_k3s_image "$FELIS_IMAGE"
  docker save "$FELIS_IMAGE" | k3s_cmd ctr images import -

  stop_docker
  ok "image built, binary on host, image imported"
}

remove_k3s_image() {
  local image="$1"
  k3s_cmd ctr images rm "$image" >/dev/null 2>&1 || true
  case "$image" in
    */*) ;;
    *) k3s_cmd ctr images rm "docker.io/library/${image}" >/dev/null 2>&1 || true ;;
  esac
}

# role_image <role> prints the name this install runs <role>'s image under: the names above for
# the images Felis builds, the name CRI gives a digest-pinned pull for the two it does not.
role_image() {
  case "$1" in
    felis) printf '%s\n' "$FELIS_IMAGE" ;;
    limbo) printf '%s\n' "$FELIS_LIMBO_IMAGE" ;;
    lobby) printf '%s\n' "$FELIS_LOBBY_IMAGE" ;;
    paper) printf '%s\n' "$FELIS_PAPER_IMAGE" ;;
    registry) pinned_image_ref "$REGISTRY_IMAGE" ;;
    postgres) pinned_image_ref "$POSTGRES_IMAGE" ;;
  esac
}

# role_fallback <role> prints what takes the place of a release image this run cannot use.
role_fallback() {
  case "$1" in
    registry|postgres) printf 'k3s pulls it from Docker Hub instead' ;;
    *) printf 'building it on this host instead' ;;
  esac
}

# role_prebuilt <role> reports whether <role>'s image came from the release (import_release_images).
role_prebuilt() {
  case "$PREBUILT_ROLES" in *" $1 "*) return 0 ;; esac
  return 1
}

# image_repo <ref> prints <ref> with its digest and tag dropped.
image_repo() {
  local r="${1%%@*}"
  case "${r##*/}" in *:*) r="${r%:*}" ;; esac
  printf '%s\n' "$r"
}

# Tagged entries must match the expected manifest digest; digest references are already immutable.
image_present() {
  case "$2" in
    *@*) awk -v r="$2" '$1 == r { f = 1 } END { exit !f }' <<<"$1" ;;
    *) awk -v r="$2" -v d="$3" '$1 == r && $3 == d { f = 1 } END { exit !f }' <<<"$1" ;;
  esac
}

# Reject the entire image listing if any role, architecture, digest, or reference is invalid.
load_release_listing() {
  local file="$1" arch="$2" bundle role name digest config rest out="" seen=" "
  while read -r bundle role name digest config rest; do
    [ -n "$bundle" ] || continue
    [ -z "$rest" ] || return 1
    [[ "$bundle" =~ ^felis-image-[a-z]+-linux-${arch}\.tar$ ]] || return 1
    case "$role" in felis|limbo|lobby|paper|registry|postgres) ;; *) return 1 ;; esac
    case "$seen" in *" $role "*) return 1 ;; esac
    seen="${seen}${role} "
    [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]] && [[ "$config" =~ ^sha256:[0-9a-f]{64}$ ]] || return 1
    [[ "$name" =~ ^[A-Za-z0-9._:/@-]+$ ]] || return 1
    out="${out}${bundle} ${role} ${name} ${digest} ${config}"$'\n'
  done <"$file"
  [ -n "$out" ] || return 1
  RELEASE_LISTING="$out"
}

# release_listing reads this architecture's image listing once per run, and fails, after one
# warning (or, from FELIS_ARTIFACT_DIR, the end of the install), when it cannot be used.
release_listing() {
  local arch name
  case "$RELEASE_LISTING_STATE" in
    ok) return 0 ;;
    bad) return 1 ;;
  esac
  RELEASE_LISTING_STATE=bad
  arch="$(felis_asset_arch)" || return 1
  name="felis-images-linux-${arch}.txt"
  if ! artifact_fetch "$name"; then
    artifact_unusable "the release's image listing ${name} cannot be used" "building its images on this host instead"
    return 1
  fi
  if ! load_release_listing "$ARTIFACT_FILE" "$arch"; then
    artifact_unusable "the release's image listing ${name} is malformed" "building its images on this host instead"
    return 1
  fi
  RELEASE_LISTING_STATE=ok
}

# Fetch and import only bundles containing images absent from containerd.
# Retag operator-overridden image names; unsupported release roles fall back independently.
import_release_images() {
  local role line bundle name digest config target images todo="" needed="" b
  [ -n "$ARTIFACT_MODE" ] || return 0
  release_listing || return 0
  # Read the list whole before matching: the SIGPIPE reason on import_platform_images.
  images="$(k3s_cmd ctr images ls 2>/dev/null || true)"
  for role in "$@"; do
    line="$(awk -v r="$role" '$2 == r' <<<"$RELEASE_LISTING")"
    if [ -z "$line" ]; then
      artifact_unusable "the release lists no ${role} image" "$(role_fallback "$role")"
      continue
    fi
    read -r bundle _ name digest config <<<"$line"
    target="$(role_image "$role")"
    case "$role" in
      registry|postgres)
        # Docker Hub's copy is pinned by digest here; the release's must be that one.
        if [ "$name" != "$target" ]; then
          artifact_unusable "the release's ${role} image is ${name}, but this installer runs ${target}" "$(role_fallback "$role")"
          continue
        fi
        ;;
    esac
    todo="${todo}${role} ${bundle} ${name} ${target} ${digest} ${config}"$'\n'
    if ! image_present "$images" "$target" "$digest"; then
      case "${needed} " in *" ${bundle} "*) ;; *) needed="${needed} ${bundle}" ;; esac
    fi
  done
  for b in $needed; do
    # artifact_fetch says why it failed; the images the bundle holds are refused below.
    artifact_fetch "$b" || continue
    log "importing ${b} into k3s containerd"
    k3s_cmd ctr images import "$ARTIFACT_FILE" >/dev/null || warn "k3s containerd could not import ${b}"
  done
  [ -z "$needed" ] || images="$(k3s_cmd ctr images ls 2>/dev/null || true)"
  while read -r role bundle name target digest config; do
    [ -n "$role" ] || continue
    if ! image_present "$images" "$target" "$digest"; then
      if ! image_present "$images" "$name" "$digest" \
          || ! k3s_cmd ctr images tag --force "$name" "$target" >/dev/null \
          || ! k3s_cmd ctr images tag --force "$name" "$(image_repo "$target")@${digest}" >/dev/null; then
        artifact_unusable "k3s containerd holds no ${target} from ${bundle}" "$(role_fallback "$role")"
        continue
      fi
    fi
    PREBUILT_ROLES="${PREBUILT_ROLES}${role} "
    RELEASE_IMAGES="${RELEASE_IMAGES}${role} ${bundle} ${name} ${target} ${digest} ${config}"$'\n'
    # The build each system server runs, for restart_existing_system_servers: the image's
    # config digest, the id docker gives the same image.
    case "$role" in
      limbo) LIMBO_IMAGE_ID="$config" ;;
      lobby) LOBBY_IMAGE_ID="$config" ;;
    esac
    ok "${target} is the release's (${digest:7:12})"
  done <<<"$todo"
}

# Authenticate proxy identities with HMAC; NetworkPolicy cannot block the hosting node and is only defense in depth.

# Prebuilt installs use matching embedded sources instead of possibly stale checkout contents.
game_stack_source() {
  if [ -z "$HAVE_PREBUILT_BINARY" ] && [ -f "${SRC_DIR}/deploy/limbo/Dockerfile" ]; then
    GAME_STACK_DIR="$SRC_DIR"
    ok "game-stack sources: ${SRC_DIR}"
    return 0
  fi
  GAME_STACK_DIR="$(mktemp -d)"
  remember_temp "$GAME_STACK_DIR"
  log "unpacking the embedded game-stack sources (no checkout on this host)"
  "$HOST_BIN" bootstrap-assets game-stack | tar -x -C "$GAME_STACK_DIR" \
    || die "could not unpack the embedded game-stack sources"
  [ -f "${GAME_STACK_DIR}/deploy/limbo/Dockerfile" ] \
    || die "embedded game-stack tar is missing deploy/limbo/Dockerfile"
  ok "game-stack sources unpacked to ${GAME_STACK_DIR}"
}

# Retry all metadata failures externally, including TLS errors unsupported by older curl retry flags.
meta_get() {
  local url="$1" out attempt
  for attempt in 1 2 3; do
    if out="$(curl -fsSL --retry 5 --retry-delay 2 \
      -A "felis-bootstrap (+https://github.com/FelisMC/Felis)" "$url")"; then
      printf '%s' "$out"
      return 0
    fi
    if [ "$attempt" -lt 3 ]; then
      warn "fetching ${url} failed (attempt ${attempt}/3); retrying"
      sleep 5
    fi
  done
  return 1
}

# Pinned stacks verify lock-file checksums and reuse unchanged builds on reruns.
resolve_game_jars() {
  if [ "$FELIS_GAME_STACK" = "latest" ]; then
    resolve_latest_game_jars
    return 0
  fi
  load_game_stack_lock "${GAME_STACK_DIR}/deploy/game-stack.lock"
  ok "Limbo ${LIMBO_VERSION} + Paper on Minecraft ${MC_VERSION}, LuckPerms and Velocity ${VELOCITY_VERSION}: the builds game-stack.lock pins"
}

GAME_STACK_LOCK_KEYS="MC_VERSION LIMBO_VERSION LIMBO_JAR_URL LIMBO_JAR_SHA256 LIMBO_SCHEM_URL LIMBO_SCHEM_SHA256 PAPER_JAR_URL PAPER_JAR_SHA256 LUCKPERMS_JAR_URL LUCKPERMS_JAR_SHA256 VELOCITY_VERSION VELOCITY_JAR_URL VELOCITY_JAR_SHA256"

# Parse only approved lock keys and validated values; never source the lock file.
load_game_stack_lock() {
  local file="$1" line key value
  [ -f "$file" ] || die "no game-stack lock at ${file}; FELIS_GAME_STACK=latest resolves upstream's newest builds instead"
  for key in $GAME_STACK_LOCK_KEYS; do printf -v "$key" '%s' ""; done
  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in ''|'#'*) continue ;; esac
    key="${line%%=*}"
    value="${line#*=}"
    [ "$key" != "$line" ] || die "${file}: not a KEY=value line: ${line}"
    case " ${GAME_STACK_LOCK_KEYS} " in
      *" ${key} "*) ;;
      *) die "${file}: unknown key ${key}" ;;
    esac
    case "$value" in
      ''|*[!A-Za-z0-9._:/+%-]*) die "${file}: ${key} has an unexpected value: ${value}" ;;
    esac
    printf -v "$key" '%s' "$value"
  done < "$file"
  for key in $GAME_STACK_LOCK_KEYS; do
    [ -n "${!key}" ] || die "${file} does not set ${key}"
    case "$key" in
      *_SHA256) [[ "${!key}" =~ ^[0-9a-f]{64}$ ]] || die "${file}: ${key} is not a lowercase sha256" ;;
    esac
  done
}

# url_sha256 prints the sha256 of what $1 serves.
url_sha256() {
  local sum
  sum="$(curl -fsSL --retry 5 --retry-delay 2 -A "felis-bootstrap (+https://github.com/FelisMC/Felis)" "$1" | sha256sum)" || return 1
  printf '%s\n' "${sum%% *}"
}

# Derive the protocol version from Limbo's artifact name and match Paper to it.
# Latest downloads without published digests provide byte consistency, not independent provenance.
resolve_latest_game_jars() {
  local ci="https://ci.loohpjames.com/job/Limbo/lastSuccessfulBuild" meta build file base rest paper
  log "resolving the newest LOOHP/Limbo CI build"
  # Fetch first, filter second: `curl | grep | head` dies of SIGPIPE under `set -o pipefail`
  # the moment head closes the pipe early. Same shape everywhere below.
  meta="$(meta_get "${ci}/api/json")" \
    || die "could not read the LOOHP/Limbo CI build metadata"
  file="$(printf '%s' "$meta" | grep -o 'Limbo-[0-9A-Za-z._-]*\.jar' || true)"
  file="${file%%$'\n'*}"
  [ -n "$file" ] || die "no Limbo jar in the LOOHP/Limbo CI artifact list"
  # The numbered build, so the jar hashed below is the jar the image build downloads even
  # if CI finishes another build in between.
  build="$(printf '%s' "$meta" | grep -o '"number":[0-9]*' || true)"
  build="${build%%$'\n'*}"
  build="${build#*:}"
  [ -n "$build" ] || die "no build number in the LOOHP/Limbo CI metadata"
  ci="https://ci.loohpjames.com/job/Limbo/${build}"

  base="${file%.jar}"          # Limbo-2026.0.2-ALPHA-26.2
  MC_VERSION="${base##*-}"     # 26.2
  rest="${base%-*}"            # Limbo-2026.0.2-ALPHA
  LIMBO_VERSION="${rest#Limbo-}"
  [ -n "$MC_VERSION" ] && [ -n "$LIMBO_VERSION" ] || die "cannot parse Limbo artifact name: ${file}"
  LIMBO_JAR_URL="${ci}/artifact/target/${file}"
  LIMBO_SCHEM_URL="${ci}/artifact/spawn.schem"

  # PaperMC Fill v3. The old api.papermc.io v2 has returned HTTP 410 since 2026-07-01 and
  # is never coming back; Fill wants a descriptive User-Agent.
  log "resolving the newest Paper ${MC_VERSION} build"
  paper="$(papermc_latest_jar paper "$MC_VERSION")" \
    || die "could not resolve a Paper build for Minecraft ${MC_VERSION} (the login gate pins this protocol; the build likely exists — Fill upstream is down, flapping, or no longer content-addressed)"
  PAPER_JAR_URL="${paper% *}"
  PAPER_JAR_SHA256="${paper##* }"
  # LuckPerms is not version-matched to MC_VERSION the way Paper is: it ships one
  # current Bukkit build that supports the whole supported Minecraft range, so there is
  # no per-version endpoint to ask.
  log "resolving the newest LuckPerms build"
  LUCKPERMS_JAR_URL="$(luckperms_latest_jar)" \
    || die "could not resolve a LuckPerms build (metadata.luckperms.net is down or flapping); the lobby needs it for the panel's permission controls"

  log "hashing the Limbo and LuckPerms downloads (their upstreams publish no digest)"
  LIMBO_JAR_SHA256="$(url_sha256 "$LIMBO_JAR_URL")" || die "could not download ${LIMBO_JAR_URL}"
  LIMBO_SCHEM_SHA256="$(url_sha256 "$LIMBO_SCHEM_URL")" || die "could not download ${LIMBO_SCHEM_URL}"
  LUCKPERMS_JAR_SHA256="$(url_sha256 "$LUCKPERMS_JAR_URL")" || die "could not download ${LUCKPERMS_JAR_URL}"
  VELOCITY_VERSION="$VELOCITY_LATEST_MINOR"
  VELOCITY_JAR_URL=""
  VELOCITY_JAR_SHA256=""
  ok "Limbo ${LIMBO_VERSION} (CI build ${build}) + Paper, both on Minecraft ${MC_VERSION}; LuckPerms resolved"
  warn "FELIS_GAME_STACK=latest: these are upstream's builds as of now, not the ones this release pins"
}

# Select LuckPerms' /bukkit/ artifact, excluding legacy and other loaders.
luckperms_latest_jar() {
  local json url
  json="$(meta_get "https://metadata.luckperms.net/data/all")" || return 1
  url="$(printf '%s' "$json" \
    | grep -o 'https://download\.luckperms\.net/[0-9]\{1,\}/bukkit/loader/[^"]*\.jar' || true)"
  url="${url%%$'\n'*}"
  [ -n "$url" ] || return 1
  printf '%s\n' "$url"
}

# Retry transient metadata failures and extract URL plus checksum from the same content-addressed response match.
papermc_latest_jar() {
  local project="$1" version="$2" json urls url sha
  json="$(meta_get "https://fill.papermc.io/v3/projects/${project}/versions/${version}/builds/latest")" || return 1
  urls="$(printf '%s' "$json" | grep -o 'https://fill-data\.papermc\.io/[^"]*\.jar' || true)"
  url="${urls%%$'\n'*}"
  [ -n "$url" ] || return 1
  sha="${url#*/objects/}"
  sha="${sha%%/*}"
  case "$sha" in *[!0-9a-f]*|"") return 1 ;; esac
  [ "${#sha}" -eq 64 ] || return 1
  printf '%s %s\n' "$url" "$sha"
}

build_game_stack() {
  local role docker_used=""
  game_stack_source
  resolve_game_jars
  # A release's game images are built from its game-stack.lock, the pinned stack. The latest
  # The stack is resolved on this host at install time, so it is built here.
  if [ "$FELIS_GAME_STACK" = pinned ]; then
    import_release_images limbo lobby paper
  fi
  for role in limbo lobby paper; do
    role_prebuilt "$role" && continue
    [ -n "$docker_used" ] || ensure_docker
    docker_used=1
    build_game_image "$role"
  done
  install_velocity_plugin
  stop_docker
  ok "login + lobby images imported; felis-velocity.jar staged"
}

# build_game_image <role> builds one game image on this host and imports it into k3s containerd.
build_game_image() {
  local img
  case "$1" in
    limbo)
      img="$FELIS_LIMBO_IMAGE"
      log "building ${img} (LOOHP/Limbo ${LIMBO_VERSION}, Minecraft ${MC_VERSION})"
      docker build --progress=plain -f "${GAME_STACK_DIR}/deploy/limbo/Dockerfile" \
        --build-arg LIMBO_JAR_URL="$LIMBO_JAR_URL" \
        --build-arg LIMBO_JAR_SHA256="$LIMBO_JAR_SHA256" \
        --build-arg LIMBO_SCHEM_URL="$LIMBO_SCHEM_URL" \
        --build-arg LIMBO_SCHEM_SHA256="$LIMBO_SCHEM_SHA256" \
        --build-arg LIMBO_VERSION="$LIMBO_VERSION" \
        -t "$img" "$GAME_STACK_DIR"
      ;;
    lobby)
      img="$FELIS_LOBBY_IMAGE"
      log "building ${img} (Paper ${MC_VERSION} + felis-paper /menu + LuckPerms)"
      docker build --progress=plain -f "${GAME_STACK_DIR}/deploy/lobby/Dockerfile" \
        --build-arg PAPER_JAR_URL="$PAPER_JAR_URL" \
        --build-arg PAPER_JAR_SHA256="$PAPER_JAR_SHA256" \
        --build-arg LUCKPERMS_JAR_URL="$LUCKPERMS_JAR_URL" \
        --build-arg LUCKPERMS_JAR_SHA256="$LUCKPERMS_JAR_SHA256" \
        -t "$img" "$GAME_STACK_DIR"
      ;;
    paper)
      # Plain Paper, same MC_VERSION and PAPER_JAR_URL (no new dependency). Forwarding is the
      # operator initContainer's job, so this image carries no /menu plugin and no secret gate.
      img="$FELIS_PAPER_IMAGE"
      log "building ${img} (plain Paper ${MC_VERSION}, forwarding via the operator initContainer)"
      docker build --progress=plain -f "${GAME_STACK_DIR}/deploy/paper/Dockerfile" \
        --build-arg PAPER_JAR_URL="$PAPER_JAR_URL" \
        --build-arg PAPER_JAR_SHA256="$PAPER_JAR_SHA256" \
        -t "$img" "$GAME_STACK_DIR"
      ;;
  esac

  # Compare deterministic game image IDs so unchanged rebuilds do not restart occupied system servers.
  case "$1" in
    limbo) LIMBO_IMAGE_ID="$(docker image inspect -f '{{.Id}}' "$img")" ;;
    lobby) LOBBY_IMAGE_ID="$(docker image inspect -f '{{.Id}}' "$img")" ;;
  esac

  log "importing ${img} into k3s containerd"
  remove_k3s_image "$img"
  docker save "$img" | k3s_cmd ctr images import -
}

# install_velocity_plugin puts the release's felis-velocity.jar in place, or builds one here
# when the release has none this run can use. The jar is JVM bytecode, one file for every
# architecture.
install_velocity_plugin() {
  if [ -n "$ARTIFACT_MODE" ]; then
    if artifact_fetch felis-velocity.jar; then
      prepare_velocity_layout
      install_if_changed "$ARTIFACT_FILE" "${VELOCITY_DIR}/plugins/felis-velocity.jar" 0644 root root
      ok "felis-velocity.jar is the release's"
      return 0
    fi
    artifact_unusable "the release's felis-velocity.jar cannot be used" "building it on this host instead"
  fi
  ensure_docker
  build_velocity_plugin
}

ensure_velocity_directory() {
  local path="$1" mode="$2" owner="$3" group="$4"
  if [ -L "$path" ]; then
    rm -f -- "$path"
  elif [ -e "$path" ] && [ ! -d "$path" ]; then
    die "velocity path exists but is not a directory: ${path}"
  fi
  install -d -o "$owner" -g "$group" -m "$mode" "$path"
}

# Allow proxy-owned mutable config while keeping JARs and forwarding secrets root-owned and read-only.
prepare_velocity_layout() {
  id -u "$VELOCITY_USER" >/dev/null 2>&1 \
    || useradd --system --home-dir "$VELOCITY_DIR" --shell /usr/sbin/nologin "$VELOCITY_USER"
  ensure_velocity_directory "$VELOCITY_DIR" 0750 "$VELOCITY_USER" "$VELOCITY_USER"
  ensure_velocity_directory "${VELOCITY_DIR}/plugins" 0750 "$VELOCITY_USER" "$VELOCITY_USER"
  ensure_velocity_directory "${VELOCITY_DIR}/plugins/felis-link" 0750 "$VELOCITY_USER" "$VELOCITY_USER"
  ensure_velocity_directory "${VELOCITY_DIR}/logs" 0750 "$VELOCITY_USER" "$VELOCITY_USER"
  ensure_velocity_directory "${VELOCITY_DIR}/crash-reports" 0750 "$VELOCITY_USER" "$VELOCITY_USER"
}

atomic_install_file() {
  local source="$1" target="$2" mode="$3" owner="$4" group="$5"
  local parent base staged
  parent="$(dirname "$target")"
  base="$(basename "$target")"
  [ -d "$parent" ] && [ ! -L "$parent" ] \
    || die "refusing to install through a non-directory/symlink parent: ${parent}"
  if [ -d "$target" ] && [ ! -L "$target" ]; then
    die "refusing to replace directory with file: ${target}"
  fi
  staged="$(mktemp "${parent}/.${base}.XXXXXX")"
  remember_temp "$staged"
  install -o "$owner" -g "$group" -m "$mode" "$source" "$staged"
  mv -fT "$staged" "$target"
}

# Preserve mtime for identical content because domain checks use it to detect unapplied changes.
# Replace files atomically; path-based chmod/chown could follow a proxy-planted symlink.
install_if_changed() {
  local source="$1" target="$2" mode="$3" owner="$4" group="$5"
  if [ -f "$target" ] && [ ! -L "$target" ] && cmp -s "$source" "$target" \
    && [ "$(stat -c '%U:%G %a' "$target")" = "${owner}:${group} ${mode#0}" ]; then
    return 0
  fi
  atomic_install_file "$@"
}

# Build plugins in the verified Gradle image so the host needs only a JRE.
build_velocity_plugin() {
  log "building felis-velocity.jar (gradle in a container; the host gets no JDK)"
  prepare_velocity_layout
  # :z relabels the bind mount for SELinux (Fedora/EL enforce it; elsewhere it is a no-op).
  docker run --rm \
    -v "${GAME_STACK_DIR}:/src:z" \
    -w /src/plugins/velocity \
    "$PLUGIN_BUILD_IMAGE" gradle --no-daemon clean build --console=plain --init-script ../shared/build-progress.gradle \
    || die "felis-velocity plugin build failed"
  local -a jars=( "${GAME_STACK_DIR}"/plugins/velocity/build/libs/felis-velocity-*.jar )
  [ "${#jars[@]}" -eq 1 ] && [ -f "${jars[0]}" ] \
    || die "felis-velocity build must produce exactly one plugin jar"
  atomic_install_file "${jars[0]}" "${VELOCITY_DIR}/plugins/felis-velocity.jar" 0644 root root
}

# Pin Via plugins as a compatible set supporting the locked game protocol.
install_via_plugins() {
  prepare_velocity_layout
  local name version want target url tmp have
  while read -r name version want; do
    [ -n "$name" ] || continue
    target="${VELOCITY_DIR}/plugins/${name}.jar"
    # Hash stdin, never the path: sha256sum escapes its output line when the filename
    # carries a backslash or a newline, and a leading "\" on the digest silently fails
    # every comparison below.
    have=""
    [ -f "$target" ] && have="$(sha256sum <"$target" | cut -d' ' -f1)"
    if [ "$have" = "$want" ]; then
      ok "${name} ${version} already staged"
      continue
    fi
    url="https://github.com/ViaVersion/${name}/releases/download/${version}/${name}-${version}.jar"
    log "downloading ${name} ${version}"
    tmp="$(mktemp "${VELOCITY_DIR}/.${name}.jar.XXXXXX")"
    remember_temp "$tmp"
    curl -fsSL --retry 5 --retry-delay 2 "$url" -o "$tmp" || die "failed to download ${name} ${version}: ${url}"
    have="$(sha256sum <"$tmp" | cut -d' ' -f1)"
    [ "$have" = "$want" ] \
      || die "${name} ${version} checksum mismatch: got ${have}, expected ${want}"
    atomic_install_file "$tmp" "$target" 0644 root root
  done <<'EOF'
ViaVersion 5.12.0 72c40a6a702d67f226fc9a0d8ad82aba1483fdabe2e6159bcdddb2dc070750b0
ViaBackwards 5.12.0 194e9250224632274d7b3c17e411e031a9223c1863c6f5138d53c721f07ab78d
ViaRewind 4.2.0 d6634ba57bb82d5161c68dfb393571cdf40511a0beb1b04b8c7ed794a3532c6a
EOF
  pin_via_block_connections
  ok "Via staged with 26.3 support; verify client versions against your chosen backend images"
}

# Disable Via block tracking to avoid missing-provider failures; old-client block connections may be cosmetic approximations.
pin_via_block_connections() {
  local dir="${VELOCITY_DIR}/plugins/viaversion" config tmp
  config="${dir}/config.yml"
  ensure_velocity_directory "$dir" 0750 "$VELOCITY_USER" "$VELOCITY_USER"
  tmp="$(mktemp "${VELOCITY_DIR}/.viaversion-config.XXXXXX")"
  remember_temp "$tmp"
  if [ ! -f "$config" ]; then
    printf 'serverside-blockconnections: false\n' > "$tmp"
  elif grep -qE '^serverside-blockconnections:' "$config"; then
    sed -E 's/^serverside-blockconnections:.*/serverside-blockconnections: false/' "$config" > "$tmp"
  else
    { cat "$config"; printf 'serverside-blockconnections: false\n'; } > "$tmp"
  fi
  # Via rewrites this file itself on every load, so the proxy user has to own it.
  atomic_install_file "$tmp" "$config" 0640 "$VELOCITY_USER" "$VELOCITY_USER"
}

install_jre() {
  local arch want url release json
  case "$(uname -m)" in
    x86_64|amd64) arch="x64"; want="$JRE_PINNED_SHA256_X64" ;;
    aarch64|arm64) arch="aarch64"; want="$JRE_PINNED_SHA256_AARCH64" ;;
    *)
      if [ -x "${JRE_DIR}/bin/java" ]; then
        ok "JRE already installed at ${JRE_DIR}"
        return 0
      fi
      die "no Temurin JRE build for architecture $(uname -m); pre-stage one at ${JRE_DIR}"
      ;;
  esac

  if [ "$FELIS_JRE_VERSION" = "$JRE_PINNED_FEATURE" ]; then
    release="$JRE_PINNED_RELEASE"
    url="https://github.com/adoptium/temurin${JRE_PINNED_FEATURE}-binaries/releases/download/jdk-${release/+/%2B}/OpenJDK${JRE_PINNED_FEATURE}U-jre_${arch}_linux_hotspot_${release/+/_}.tar.gz"
  else
    # Another feature version is installed once and then left alone; its digest is the
    # one Adoptium's API publishes next to the link.
    if [ -x "${JRE_DIR}/bin/java" ]; then
      ok "JRE already installed at ${JRE_DIR}"
      return 0
    fi
    json="$(meta_get "https://api.adoptium.net/v3/assets/latest/${FELIS_JRE_VERSION}/hotspot?architecture=${arch}&image_type=jre&os=linux&vendor=eclipse")" \
      || die "could not ask the Adoptium API for a Temurin ${FELIS_JRE_VERSION} JRE"
    url="$(printf '%s' "$json" | grep -o '"link": *"[^"]*\.tar\.gz"' || true)"
    url="${url%%$'\n'*}"
    url="${url%\"}"
    url="${url##*\"}"
    want="$(printf '%s' "$json" | grep -o '"checksum": *"[0-9a-f]\{64\}"' || true)"
    want="${want%%$'\n'*}"
    want="${want%\"}"
    want="${want##*\"}"
    release="$(printf '%s' "$json" | grep -o '"release_name": *"jdk-[^"]*"' || true)"
    release="${release%%$'\n'*}"
    release="${release%\"}"
    release="${release##*\"jdk-}"
    [ -n "$url" ] && [ -n "$want" ] && [ -n "$release" ] \
      || die "the Adoptium API lists no Temurin ${FELIS_JRE_VERSION} JRE for linux/${arch}"
  fi

  # Update installer-managed JRE pins while preserving externally supplied runtimes.
  if [ -x "${JRE_DIR}/bin/java" ]; then
    if grep -qxF "IMPLEMENTOR_VERSION=\"Temurin-${release}\"" "${JRE_DIR}/release" 2>/dev/null; then
      chmod 0755 "$JRE_DIR"
      ok "Temurin ${release} JRE already installed at ${JRE_DIR}"
      return 0
    fi
    if ! grep -qxF 'IMPLEMENTOR="Eclipse Adoptium"' "${JRE_DIR}/release" 2>/dev/null; then
      ok "JRE at ${JRE_DIR} is not a Temurin build this installer put there; left as is"
      return 0
    fi
    log "moving the proxy's JRE to Temurin ${release}"
  fi

  log "installing Temurin ${release} JRE (${arch}) to ${JRE_DIR}"
  local tmp have
  tmp="$(mktemp -d)"
  remember_temp "$tmp"
  curl -fsSL --retry 5 --retry-delay 2 "$url" -o "${tmp}/jre.tar.gz" \
    || die "failed to download the Temurin JRE: ${url}"
  have="$(sha256sum <"${tmp}/jre.tar.gz" | cut -d' ' -f1)"
  [ "$have" = "$want" ] \
    || die "Temurin ${release} JRE (${arch}) hashes to ${have}, expected ${want}; refusing to install it"
  # Unpacked beside the live one and swapped in with two renames, so a failed unpack leaves
  # the proxy's runtime untouched. The running proxy keeps the files it has open.
  rm -rf "${JRE_DIR}.new" "${JRE_DIR}.old"
  mkdir -p "${JRE_DIR}.new"
  # The tarball has a single versioned top-level directory (jdk-25+36-jre/); strip it so
  # the path in the systemd unit never carries a build number.
  tar -C "${JRE_DIR}.new" --strip-components=1 -xzf "${tmp}/jre.tar.gz" || die "failed to unpack the JRE"
  chmod 0755 "${JRE_DIR}.new"
  [ -x "${JRE_DIR}.new/bin/java" ] || die "unpacked JRE has no bin/java"
  [ ! -e "$JRE_DIR" ] || mv "$JRE_DIR" "${JRE_DIR}.old"
  mv "${JRE_DIR}.new" "$JRE_DIR"
  rm -rf "${JRE_DIR}.old"
  ok "JRE at ${JRE_DIR}/bin/java (Temurin ${release})"
}

install_velocity() {
  install_jre
  local url tmp have want resolved
  prepare_velocity_layout
  if [ -n "$FELIS_VELOCITY_FORK_JAR" ]; then
    [ -f "$FELIS_VELOCITY_FORK_JAR" ] \
      || die "FELIS_VELOCITY_FORK_JAR is not a readable file: ${FELIS_VELOCITY_FORK_JAR}"
    # Hash stdin so escaped filenames cannot alter sha256sum's output prefix.
    have="$(sha256sum <"$FELIS_VELOCITY_FORK_JAR" | cut -d' ' -f1)"
    # Reject proxy JAR checksum mismatches and print the computed digest for explicit rebuild updates.
    [ -n "$FELIS_VELOCITY_FORK_JAR_SHA256" ] || die \
      "FELIS_VELOCITY_FORK_JAR_SHA256 is required whenever FELIS_VELOCITY_FORK_JAR is set.
   The jar at that path hashes to ${have}.
   Check that against the build you meant to install, then re-run with
   FELIS_VELOCITY_FORK_JAR_SHA256=${have}"
    # Normalize case and spacing in supplied SHA-256 values before comparison.
    want="$(printf '%s' "$FELIS_VELOCITY_FORK_JAR_SHA256" | tr -d '[:space:]' | tr 'A-Z' 'a-z')"
    [ "$have" = "$want" ] || die \
      "FELIS_VELOCITY_FORK_JAR checksum mismatch: got ${have}, expected ${want}"
    log "installing the Felis-Legacy Velocity fork from ${FELIS_VELOCITY_FORK_JAR} (sha256 ${have})"
    atomic_install_file "$FELIS_VELOCITY_FORK_JAR" "${VELOCITY_DIR}/velocity.jar" 0644 root root
  else
    local version="${FELIS_VELOCITY_VERSION:-${VELOCITY_VERSION:-$VELOCITY_LATEST_MINOR}}"
    if [ "$FELIS_GAME_STACK" = "pinned" ] && [ "$version" = "${VELOCITY_VERSION:-}" ]; then
      url="$VELOCITY_JAR_URL"
      want="$VELOCITY_JAR_SHA256"
    else
      log "resolving the newest Velocity ${version} build"
      resolved="$(papermc_latest_jar velocity "$version")" \
        || die "no Velocity build for ${version} (override with FELIS_VELOCITY_VERSION)"
      url="${resolved% *}"
      want="${resolved##* }"
    fi
    stage_velocity_jar "$url" "$want" "$version"
  fi

  install_via_plugins
  write_velocity_config
  install_velocity_service
  configure_velocity_firewall
}

# stage_velocity_jar installs the stock proxy from $1 unless velocity.jar already hashes to
# $2, so a rerun of the same build neither downloads nor touches the file the proxy runs.
stage_velocity_jar() {
  local url="$1" want="$2" version="$3" have="" tmp
  [ -f "${VELOCITY_DIR}/velocity.jar" ] && have="$(sha256sum <"${VELOCITY_DIR}/velocity.jar" | cut -d' ' -f1)"
  if [ "$have" = "$want" ]; then
    ok "Velocity ${version} already staged"
    return 0
  fi
  log "downloading Velocity ${version}"
  tmp="$(mktemp "${VELOCITY_DIR}/.velocity.jar.XXXXXX")"
  remember_temp "$tmp"
  curl -fsSL --retry 5 --retry-delay 2 "$url" -o "$tmp" || die "failed to download Velocity: ${url}"
  # Verify proxy downloads against the lock or content-addressed digest before installation.
  have="$(sha256sum <"$tmp" | cut -d' ' -f1)"
  [ "$have" = "$want" ] \
    || die "Velocity ${version} checksum mismatch: got ${have}, expected ${want}"
  atomic_install_file "$tmp" "${VELOCITY_DIR}/velocity.jar" 0644 root root
}

# Host processes use the internal Service ClusterIP because cluster DNS is unavailable on the node.
# The token-authenticated internal API must remain ClusterIP-only.
felis_internal_ip() {
  local ip
  ip="$(kube -n "$CONTROL_NS" get svc felis-api-internal -o jsonpath='{.spec.clusterIP}')" \
    || die "could not resolve the felis-api-internal ClusterIP"
  [ -n "$ip" ] || die "felis-api-internal has no ClusterIP"
  printf '%s' "$ip"
}

write_velocity_config() {
  local api_ip tmp panel_host admin_host
  api_ip="$(felis_internal_ip)"
  panel_host="$(auth_hostname panel_hostname "console.${FELIS_ROOT_DOMAIN}")"
  admin_host="$(auth_hostname admin_hostname "op.console.${FELIS_ROOT_DOMAIN}")"

  prepare_velocity_layout
  tmp="$(mktemp -d)"
  remember_temp "$tmp"

  # Reject empty forwarding secrets rather than starting an unauthenticated proxy.
  (umask 077; printf '%s' "$FORWARDING_SECRET" > "${tmp}/forwarding.secret")

  cat > "${tmp}/velocity.toml" <<EOF
# Generated by deploy/bootstrap.sh — do not edit by hand; rerun the installer.
config-version = "2.7"
bind = "0.0.0.0:${FELIS_GAME_PORT}"
motd = "A Felis server"
show-max-players = 100

# The crown jewel. THIS is the process that talks to Mojang. Every backend runs
# offline-mode and trusts the profile forwarded from here, so online-mode = false would
# not "relax auth" — it would let anyone join as anyone, the Owner's account included.
online-mode = true
force-key-authentication = true
prevent-client-proxy-connections = false

# modern forwarding hands the Mojang-verified profile to the backend under an HMAC keyed
# by forwarding.secret. The legacy (BungeeCord) mode carries no secret and fails OPEN, so
# it is not an option at any price. Mode is proxy-wide, hence every backend gets the key.
player-info-forwarding-mode = "modern"
forwarding-secret-file = "forwarding.secret"

announce-forge = false
kick-existing-players = false
ping-passthrough = "disabled"
enable-player-address-logging = true

[servers]
# A dead address on purpose. felis-velocity re-registers "${LOGIN_SERVER}" with the real
# backend endpoint it reads from felis-api (ServerRegistry) a few seconds after start, and
# until it does, the proxy must point somewhere that REFUSES rather than somewhere that
# admits. It cannot simply be omitted: Velocity's config validation rejects a \`try\` entry
# that is not in [servers] ("Fallback server ... is not registered in your configuration!").
${LOGIN_SERVER} = "127.0.0.1:1"

# The login gate is the only fallback, and it authenticates. Never add the lobby here:
# a fallback to "${LOBBY_SERVER}" would route an unauthenticated player straight past the gate.
try = ["${LOGIN_SERVER}"]

[forced-hosts]
# Empty on purpose — felis-velocity does host-based routing itself from the subdomains
# felis-api reports (spec §11), so static entries here would only go stale.

[advanced]
haproxy-protocol = false
# Off on purpose. Velocity answers bungeecord:main itself, before any plugin event, so
# with it on EVERY backend — including each user's own server and whatever plugins its
# owner installed — can KickPlayer or ConnectOther anyone on the network, and no plugin
# can restrict that to one server. The login gate releases players over felis:control
# instead, which felis-velocity accepts only from the login server.
bungee-plugin-message-channel = false

[query]
enabled = false
EOF

  (umask 077; cat > "${tmp}/felis-link.properties" <<EOF
# Generated by deploy/bootstrap.sh — do not edit by hand; rerun the installer.
api-base-url=http://${api_ip}:8081
service-token=${SERVICE_TOKEN}
root-domain=${FELIS_ROOT_DOMAIN}
panel-hostname=${panel_host}
admin-hostname=${admin_host}
login-server=${LOGIN_SERVER}
lobby-server=${LOBBY_SERVER}
EOF
  )

  atomic_install_file "${tmp}/forwarding.secret" "${VELOCITY_DIR}/forwarding.secret" 0640 root "$VELOCITY_USER"
  atomic_install_file "${tmp}/velocity.toml" "${VELOCITY_DIR}/velocity.toml" 0640 "$VELOCITY_USER" "$VELOCITY_USER"
  install_if_changed "${tmp}/felis-link.properties" \
    "${VELOCITY_DIR}/plugins/felis-link/felis-link.properties" 0640 root "$VELOCITY_USER"
  ok "velocity.toml + forwarding secret + felis-link.properties written (${VELOCITY_DIR})"
}

install_velocity_service() {
  local api_ip
  # Route hasJoined through felis-api so federation also enforces the reclaim blacklist.
  api_ip="$(felis_internal_ip)"
  # Protocol-47 backends need quoted legacy-server properties; keep other backends on signed modern forwarding.
  local legacy_forwarding_servers="${FELIS_LEGACY_FORWARDING_SERVERS}"
  # Start with a small heap and no pre-touch; use G1 for larger ceilings and require at least 256M.
  local xmx="$FELIS_VELOCITY_XMX" jvm
  if [ "$(heap_megabytes "$xmx")" -le 1024 ]; then
    jvm="-Xms16M -Xmx${xmx} -XX:+UseSerialGC -XX:TieredStopAtLevel=1"
  else
    jvm="-Xms64M -Xmx${xmx} -XX:+UseG1GC -XX:+ParallelRefProcEnabled -XX:G1PeriodicGCInterval=60000"
  fi
  cat > "$VELOCITY_SERVICE" <<EOF
[Unit]
Description=Felis Velocity proxy (Mojang authentication + modern forwarding)
After=network-online.target k3s.service
Wants=network-online.target

[Service]
Type=simple
User=${VELOCITY_USER}
Group=${VELOCITY_USER}
WorkingDirectory=${VELOCITY_DIR}
ExecStart=${JRE_DIR}/bin/java ${jvm} -Dmojang.sessionserver=http://${api_ip}:8081/session/minecraft/hasJoined "-Dfelis.legacy-forwarding.servers=${legacy_forwarding_servers}" -jar ${VELOCITY_DIR}/velocity.jar
Restart=on-failure
RestartSec=5
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
ReadWritePaths=${VELOCITY_DIR}

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable felis-velocity
  # Restart only on changed runtime fingerprints and record the fingerprint only after restart succeeds.
  local fp
  fp="$(velocity_fingerprint)"
  if systemctl is-active --quiet felis-velocity \
      && [ "$fp" = "$(cat "$VELOCITY_FINGERPRINT" 2>/dev/null || true)" ]; then
    ok "felis-velocity unchanged; left running (0.0.0.0:${FELIS_GAME_PORT})"
    return 0
  fi
  systemctl restart felis-velocity
  printf '%s\n' "$fp" > "$VELOCITY_FINGERPRINT"
  ok "felis-velocity.service enabled and started (0.0.0.0:${FELIS_GAME_PORT})"
}

# Fingerprint the unit, JRE, JARs, and installer-owned files; exclude plugin-generated runtime configuration.
# Exclude the hot-reloaded service token from restart fingerprints.
velocity_fingerprint() {
  local f sum
  for f in "$VELOCITY_SERVICE" "${JRE_DIR}/release" "${VELOCITY_DIR}/velocity.jar" \
      "${VELOCITY_DIR}/velocity.toml" "${VELOCITY_DIR}/forwarding.secret" \
      "${VELOCITY_DIR}/plugins/felis-link/felis-link.properties" "${VELOCITY_DIR}"/plugins/*.jar; do
    [ -f "$f" ] || continue
    case "$f" in
      */felis-link.properties) sum="$({ grep -v '^service-token=' "$f" || true; } | sha256sum | cut -d' ' -f1)" ;;
      *) sum="$(sha256sum <"$f" | cut -d' ' -f1)" ;;
    esac
    printf '%s %s\n' "$sum" "$f"
  done | sha256sum | cut -d' ' -f1
}

configure_velocity_firewall() {
  if ufw_active; then
    log "opening ufw port ${FELIS_GAME_PORT}/tcp for the Minecraft proxy"
    ufw allow "${FELIS_GAME_PORT}/tcp" comment felis-proxy >/dev/null
  fi
  command -v firewall-cmd >/dev/null 2>&1 || return 0
  systemctl is-active --quiet firewalld || return 0
  log "opening firewalld port ${FELIS_GAME_PORT}/tcp for the Minecraft proxy"
  firewall-cmd --permanent --add-port="${FELIS_GAME_PORT}/tcp"
  firewall-cmd --reload
}

# Use loopback PostgreSQL for host tools and retain migrated host data for rollback.

# Do not attach stdin to pg_exec during piped installs; pg_sql deliberately receives SQL through stdin.
pg_exec() { kube -n "$CONTROL_NS" exec "deploy/${PG_DEPLOYMENT}" -c "$PG_CONTAINER" -- "$@" </dev/null; }
pg_sql() { # database; SQL on stdin
  kube -n "$CONTROL_NS" exec -i "deploy/${PG_DEPLOYMENT}" -c "$PG_CONTAINER" -- \
    psql -X -q -v ON_ERROR_STOP=1 -U postgres -d "$1"
}

# postgres_image_major prints the major version POSTGRES_IMAGE's tag names (18 for
# postgres:18.6-trixie@sha256:...).
postgres_image_major() {
  local tag="${POSTGRES_IMAGE%%@*}"
  printf '%s\n' "${tag##*:}" | sed -nE 's/^([0-9]+).*/\1/p'
}

# Reject PostgreSQL major mismatches before the image can initialize an empty sibling cluster.
check_postgres_major() {
  local want found
  want="$(postgres_image_major)"
  [ -n "$want" ] || die "cannot read a PostgreSQL major version from ${POSTGRES_IMAGE}"
  [ -f "${PG_DATA_DIR}/${want}/docker/PG_VERSION" ] && return 0
  # A fresh host has no cluster: the glob matches nothing and cat fails the pipeline.
  found="$(cat "$PG_DATA_DIR"/*/docker/PG_VERSION 2>/dev/null | tr -d '[:space:]')" || true
  [ -z "$found" ] && return 0
  die "this release runs PostgreSQL ${want}, but ${PG_DATA_DIR} holds a PostgreSQL ${found} cluster.
  PostgreSQL ${want} would start an empty cluster beside it. A new major version is a dump
  and restore (https://docs.felismc.com/en/operations/upgrade#postgresql-major): on the release you have now, take a bundle with
  sudo felis db backup -label pre-upgrade, then follow that section."
}

# Keep a root-only parent above the PostgreSQL-owned data directory.
prepare_postgres_data_dir() {
  install -d -m 0700 -o root -g root "$(dirname "$PG_DATA_DIR")"
  install -d -m 0700 -o "$PG_UID" -g "$PG_UID" "$PG_DATA_DIR"
  chown "$PG_UID:$PG_UID" "$PG_DATA_DIR"
  chmod 0700 "$PG_DATA_DIR"
  label_postgres_data_dir
}

# Persist SELinux file-context rules for database volumes; use chcon only when semanage is unavailable.
label_postgres_data_dir() {
  command -v selinuxenabled >/dev/null 2>&1 && selinuxenabled || return 0
  if command -v semanage >/dev/null 2>&1; then
    semanage fcontext -a -t container_file_t "${PG_DATA_DIR}(/.*)?" 2>/dev/null \
      || semanage fcontext -m -t container_file_t "${PG_DATA_DIR}(/.*)?" 2>/dev/null || true
    restorecon -R "$PG_DATA_DIR" && return 0
  fi
  chcon -R -t container_file_t "$PG_DATA_DIR" \
    || warn "could not label ${PG_DATA_DIR} container_file_t; if felis-postgres cannot write its cluster, label it by hand"
}

# Create the PostgreSQL superuser secret once because existing clusters do not reread it.
ensure_postgres_superuser_secret() {
  kube -n "$CONTROL_NS" get secret "$PG_SECRET" >/dev/null 2>&1 && return 0
  apply_literal_secret "$CONTROL_NS" "$PG_SECRET" "$PG_SECRET_KEY" "$(openssl rand -hex 32)"
}

# ensure_postgres_role makes the felis role and database, idempotently, and (re)sets the
# role's password to the persisted one.
ensure_postgres_role() {
  # Through a variable: how bash reads \' in a replacement differs between 3.2 and 5.x.
  local q="'" password
  password="${DB_PASSWORD//$q/$q$q}"
  pg_sql postgres >/dev/null <<SQL
SET password_encryption = 'scram-sha-256';
SELECT format('CREATE ROLE %I LOGIN', '${DB_USER}') WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '${DB_USER}')\gexec
ALTER ROLE "${DB_USER}" WITH LOGIN PASSWORD '${password}';
SELECT format('CREATE DATABASE %I OWNER %I', '${DB_NAME}', '${DB_USER}') WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = '${DB_NAME}')\gexec
SQL
}

deploy_postgres() {
  check_postgres_major
  prepare_postgres_data_dir
  kube create namespace "$CONTROL_NS" --dry-run=client -o yaml | kube apply -f - >/dev/null
  ensure_postgres_superuser_secret
  log "deploying ${PG_DEPLOYMENT} (${POSTGRES_IMAGE%%@*})"
  # The HA flags split into words on purpose: a node name and a class name hold no spaces.
  # shellcheck disable=SC2046
  "$HOST_BIN" manifests --only postgres --control-namespace "$CONTROL_NS" --minecraft-namespace "$MINECRAFT_NS" \
    --postgres-image "$POSTGRES_IMAGE" $(postgres_ha_args) | kube apply -f -
  if ! kube -n "$CONTROL_NS" rollout status "deployment/${PG_DEPLOYMENT}" --timeout=600s; then
    diagnose_rollout "deployment/${PG_DEPLOYMENT}"
    die "${PG_DEPLOYMENT} did not become ready (https://docs.felismc.com/en/operations/troubleshooting#database-unavailable)"
  fi
  ensure_postgres_role
  ok "${PG_DEPLOYMENT} ready: pods at ${PG_SERVICE_ADDR}, this host at 127.0.0.1:${PG_HOST_PORT}"
}


# postgres_data_dir is where the distribution keeps a host PostgreSQL's cluster.
postgres_data_dir() {
  case "$PKG" in
    pacman) printf '%s\n' /var/lib/postgres/data ;;
    *) printf '%s\n' /var/lib/pgsql/data ;;
  esac
}

# host_postgres_holds_felis reports whether this host runs the PostgreSQL an earlier release
# installed, with the platform's tables in it.
host_postgres_holds_felis() {
  local n
  systemctl is-active --quiet postgresql 2>/dev/null || return 1
  n="$(as_postgres psql -XtA -d "$DB_NAME" -c "SELECT count(*) FROM pg_tables WHERE schemaname = 'public'" 2>/dev/null)" || return 1
  [ "${n:-0}" -gt 0 ] 2>/dev/null
}

# host_postgres_other_databases prints the databases on the host server besides the
# platform's own: a server that also holds someone else's data keeps running after the move.
host_postgres_other_databases() {
  as_postgres psql -XtA -d postgres -c "SELECT datname FROM pg_database WHERE NOT datistemplate AND datname NOT IN ('postgres', '${DB_NAME}') ORDER BY 1" 2>/dev/null
}

host_pg_counts() { as_postgres psql -XtA -v ON_ERROR_STOP=1 -d "$DB_NAME" -c "$PG_TABLE_COUNTS"; }
pod_pg_counts() { pg_exec psql -XtA -v ON_ERROR_STOP=1 -U postgres -d "$DB_NAME" -c "$PG_TABLE_COUNTS"; }

# Prepend pg_hba rules so migration access overrides older rules while excluding other writers.
write_pg_hba_lockout() {
  local hba="$1" tmp
  tmp="$(mktemp)"
  remember_temp "$tmp"
  {
    printf '# BEGIN FELIS MANAGED HBA\n'
    printf '%s\n' "$PG_LOCKOUT_LINE"
    printf 'host %s %s 127.0.0.1/32 scram-sha-256\n' "$DB_NAME" "$DB_USER"
    printf 'host %s all 0.0.0.0/0 reject\n' "$DB_NAME"
    printf 'host %s all ::/0 reject\n' "$DB_NAME"
    printf '# END FELIS MANAGED HBA\n\n'
    # The blank line the block is written with goes with it, or every run adds one.
    awk -v db="$DB_NAME" -v user="$DB_USER" '
      $0 == "# BEGIN FELIS MANAGED HBA" { skip = 1; next }
      $0 == "# END FELIS MANAGED HBA" { skip = 0; blank = 1; next }
      skip { next }
      blank && $0 == "" { blank = 0; next }
      { blank = 0 }
      $1 == "host" && $2 == db && $3 == user && $5 == "scram-sha-256" { next }
      { print }
    ' "$hba"
  } > "$tmp"
  cat "$tmp" > "$hba"
  rm -f "$tmp"
}

# Stop control-plane replicas and host timers, then block other database clients before migration.
quiesce_host_postgres() {
  local unit hba left
  hba="$(as_postgres psql -XtA -c 'SHOW hba_file' 2>/dev/null)"
  [ -n "$hba" ] && [ -f "$hba" ] || die "could not find the host PostgreSQL's pg_hba.conf"
  PG_MOVE_HBA="$hba"
  # A run killed while the lockout was up left it in place and the copy beside it; the
  # copy is the real file then, and the lockout must not overwrite it.
  if [ ! -f "${hba}.pre-pg-move" ] || ! grep -qxF "$PG_LOCKOUT_LINE" "$hba"; then
    cp -p "$hba" "${hba}.pre-pg-move"
  fi
  PG_MOVE_STAGE=quiesced
  for unit in felis-api felis-operator; do
    kube -n "$CONTROL_NS" scale deployment "$unit" --replicas=0 >/dev/null 2>&1 || true
  done
  for unit in "${PG_MOVE_TIMERS[@]}"; do
    if systemctl is-active --quiet "${unit}.timer" 2>/dev/null; then
      PG_MOVE_UNITS+=("${unit}.timer")
    fi
    systemctl stop "${unit}.timer" "${unit}.service" >/dev/null 2>&1 || true
  done
  write_pg_hba_lockout "$hba"
  as_postgres psql -XtA -v ON_ERROR_STOP=1 -c 'SELECT pg_reload_conf()' >/dev/null
  sleep 2
  as_postgres psql -XtA -c "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '${DB_NAME}' AND pid <> pg_backend_pid()" >/dev/null 2>&1 || true
  sleep 1
  left="$(as_postgres psql -XtA -c "SELECT count(*) FROM pg_stat_activity WHERE datname = '${DB_NAME}' AND pid <> pg_backend_pid()" 2>/dev/null)"
  [ "$left" = 0 ] || die "the host database still has ${left:-unknown} client(s) connected after the lockout; nothing was moved"
}

# compare_pg_counts fails unless both servers hold the same rows in every table.
compare_pg_counts() {
  local host pod
  host="$(host_pg_counts)" || die "could not count the host database's rows"
  pod="$(pod_pg_counts)" || die "could not count ${PG_DEPLOYMENT}'s rows"
  [ -n "$host" ] || die "the host database reported no tables"
  if [ "$host" != "$pod" ]; then
    warn "row counts differ between the host database (<) and ${PG_DEPLOYMENT} (>):"
    diff <(printf '%s\n' "$host") <(printf '%s\n' "$pod") >&2 || true
    die "the copy in ${PG_DEPLOYMENT} does not match the host database; the host database is untouched and stays in use"
  fi
}

# Retain host PostgreSQL data for rollback; keep the server running only if it serves unrelated databases.
retire_host_postgres() {
  local others
  others="$(host_postgres_other_databases)"
  if [ -n "$others" ]; then
    warn "the host PostgreSQL also holds $(printf '%s' "$others" | tr '\n' ' ')— it keeps running; its felis database is a stale copy that only loopback can reach; drop it once you no longer need a rollback"
  else
    systemctl disable --now postgresql
  fi
  if [ -e "$PG_FIREWALL_SERVICE" ]; then
    systemctl disable --now felis-postgres-firewall.service >/dev/null 2>&1 || true
    rm -f "$PG_FIREWALL_SERVICE" "$PG_FIREWALL_RULES"
    systemctl daemon-reload
  fi
}

# Verify every table count after migration; failures before host retirement restore the original access state.
migrate_host_postgres() {
  local legacy fresh out bundle
  [ -e "$PG_MOVED_MARKER" ] && return 0
  host_postgres_holds_felis || return 0
  log "moving the felis database from the host PostgreSQL into ${PG_DEPLOYMENT}"
  quiesce_host_postgres
  legacy="$(mktemp)"
  fresh="$(mktemp)"
  remember_temp "$legacy"
  remember_temp "$fresh"
  write_felis_toml "$legacy" "127.0.0.1:5432"
  write_felis_toml "$fresh" "127.0.0.1:${PG_HOST_PORT}" "${CONTROL_NS}/${PG_DEPLOYMENT}"
  # The move carries the database alone; the MinecraftServer objects stay in the
  # cluster, so a cluster slow to answer their export must not stop it.
  out="$("$HOST_BIN" db backup -config "$legacy" -dir "$FELIS_DB_BACKUP_DIR" -label pre-pg-move -keep 0 -no-servers)" \
    || die "could not take a bundle of the host database; nothing was moved"
  bundle="$(printf '%s\n' "$out" | sed -n 's/^felis db backup: wrote //p' | tail -n 1)"
  [ -n "$bundle" ] && [ -f "$bundle" ] || die "felis db backup reported no bundle; nothing was moved"
  "$HOST_BIN" db restore -config "$fresh" -dir "$FELIS_DB_BACKUP_DIR" -yes -no-safety-backup "$bundle" >/dev/null \
    || die "could not restore ${bundle} into ${PG_DEPLOYMENT}; the host database is untouched and stays in use"
  compare_pg_counts
  retire_host_postgres
  printf '%s moved from the host PostgreSQL; bundle %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$bundle" > "$PG_MOVED_MARKER"
  PG_MOVE_STAGE=moved
  ok "the felis database now lives in ${PG_DEPLOYMENT} (bundle ${bundle}); the host PostgreSQL is stopped and kept for a rollback"
}

# Rollback reopens the host database only before retirement; afterward reruns finish the pod migration.
undo_postgres_move() {
  local unit
  case "${PG_MOVE_STAGE:-}" in
    quiesced)
      if [ -f "${PG_MOVE_HBA}.pre-pg-move" ]; then
        cat "${PG_MOVE_HBA}.pre-pg-move" > "$PG_MOVE_HBA" \
          || warn "could not put ${PG_MOVE_HBA} back; its copy is ${PG_MOVE_HBA}.pre-pg-move"
        as_postgres psql -XtA -c 'SELECT pg_reload_conf()' >/dev/null 2>&1 || true
      fi
      for unit in felis-api felis-operator; do
        kube -n "$CONTROL_NS" scale deployment "$unit" --replicas=1 >/dev/null 2>&1 || true
      done
      warn "the database move was undone: the platform runs on the host PostgreSQL as before"
      ;;
    moved)
      warn "the felis database already lives in ${PG_DEPLOYMENT}; rerun the installer to point the platform at it"
      ;;
    *) return 0 ;;
  esac
  for unit in "${PG_MOVE_UNITS[@]-}"; do
    [ -n "$unit" ] || continue
    systemctl start "$unit" >/dev/null 2>&1 || true
  done
}

# Secrets and felis.toml. The pod copy reaches felis-postgres at its Service; the host copy uses its loopback hostPort, and names the pod that felis db runs its tools in.
load_or_make_secrets() {
  mkdir -p "$STATE_DIR"
  chmod 0700 "$STATE_DIR"
  if [ -f "$SECRETS_ENV" ]; then
    # shellcheck disable=SC1090
    . "$SECRETS_ENV"
    ok "reusing persisted secrets from ${SECRETS_ENV}"
  fi
  DB_PASSWORD="${DB_PASSWORD:-$(openssl rand -hex 24)}"
  # Use distinct internal tokens per caller to contain credential leaks.
  SERVICE_TOKEN="${SERVICE_TOKEN:-$(openssl rand -hex 32)}"
  LIMBO_TOKEN="${LIMBO_TOKEN:-$(openssl rand -hex 32)}"
  BUILD_TOKEN="${BUILD_TOKEN:-$(openssl rand -hex 32)}"
  OPS_TOKEN="${OPS_TOKEN:-$(openssl rand -hex 32)}"
  # Share the modern-forwarding HMAC key between proxy and backends to authenticate player UUIDs.
  FORWARDING_SECRET="${FORWARDING_SECRET:-$(openssl rand -hex 32)}"
  # Separate registry write credentials by platform, build, and prune role; reads stay anonymous.
  REGISTRY_PLATFORM_TOKEN="${REGISTRY_PLATFORM_TOKEN:-$(openssl rand -hex 32)}"
  REGISTRY_BUILD_TOKEN="${REGISTRY_BUILD_TOKEN:-$(openssl rand -hex 32)}"
  REGISTRY_PRUNE_TOKEN="${REGISTRY_PRUNE_TOKEN:-$(openssl rand -hex 32)}"
  write_file_atomic "$SECRETS_ENV" 0600 <<EOF
DB_PASSWORD=${DB_PASSWORD}
SERVICE_TOKEN=${SERVICE_TOKEN}
LIMBO_TOKEN=${LIMBO_TOKEN}
BUILD_TOKEN=${BUILD_TOKEN}
OPS_TOKEN=${OPS_TOKEN}
FORWARDING_SECRET=${FORWARDING_SECRET}
REGISTRY_PLATFORM_TOKEN=${REGISTRY_PLATFORM_TOKEN}
REGISTRY_BUILD_TOKEN=${REGISTRY_BUILD_TOKEN}
REGISTRY_PRUNE_TOKEN=${REGISTRY_PRUNE_TOKEN}
EOF
}

ensure_panel_tls_cert() {
  mkdir -p "$STATE_DIR"
  chmod 0700 "$STATE_DIR"
  if [ -s "$PANEL_TLS_CERT" ] && [ -s "$PANEL_TLS_KEY" ]; then
    ok "panel TLS certificate already present"
    return 0
  fi

  local cn panel_host conf
  cn="$(auth_hostname admin_hostname "op.console.${FELIS_ROOT_DOMAIN}")"
  panel_host="$(auth_hostname panel_hostname "console.${FELIS_ROOT_DOMAIN}")"
  conf="$(mktemp)"
  remember_temp "$conf"
  cat > "$conf" <<EOF
[req]
default_bits = 2048
distinguished_name = dn
x509_extensions = v3_req
prompt = no

[dn]
CN = ${cn}

[v3_req]
subjectAltName = @alt_names

[alt_names]
DNS.1 = ${cn}
DNS.2 = ${panel_host}
DNS.3 = localhost
IP.1 = 127.0.0.1
IP.2 = ${NODE_IP}
EOF

  log "generating self-signed panel TLS certificate"
  openssl req -x509 -newkey rsa:2048 -sha256 -days 825 -nodes \
    -keyout "$PANEL_TLS_KEY" \
    -out "$PANEL_TLS_CERT" \
    -subj "/CN=${cn}" \
    -config "$conf" >/dev/null 2>&1
  chmod 0600 "$PANEL_TLS_KEY"
  chmod 0644 "$PANEL_TLS_CERT"
  ok "panel TLS certificate ready (${PANEL_TLS_CERT})"
}

# Preserve SMTP headers and keys without adjacent generated comments; use Pod settings if host settings are absent.
persisted_smtp_block() {
  local f out
  for f in "${STATE_DIR}/felis.host.toml" "${STATE_DIR}/felis.pod.toml"; do
    [ -r "$f" ] || continue
    out="$(awk '
      /^[[:space:]]*\[/ {
        if (insmtp) exit
        insmtp = ($0 ~ /^[[:space:]]*\[smtp\][[:space:]]*$/)
        if (insmtp) print
        next
      }
      insmtp && /^[[:space:]]*("[A-Za-z_][A-Za-z0-9_]*"|[A-Za-z_][A-Za-z0-9_]*)[[:space:]]*=/ { print }
    ' "$f")"
    [ -n "$out" ] || continue
    printf '%s' "$out"
    return 0
  done
}

# Preserve setup-owned Access/IP headers and operator-owned auth hostnames on reruns.
# Header and keys only, like persisted_smtp_block, under the same first-readable-file rule.
persisted_auth_lines() {
  local f out
  for f in "${STATE_DIR}/felis.host.toml" "${STATE_DIR}/felis.pod.toml"; do
    [ -r "$f" ] || continue
    out="$(awk '
      /^[[:space:]]*\[/ {
        if (inauth) exit
        inauth = ($0 ~ /^[[:space:]]*\[auth\][[:space:]]*$/)
        next
      }
      inauth && /^[[:space:]]*[A-Za-z_][A-Za-z0-9_]*[[:space:]]*=/ { print }
    ' "$f")"
    [ -n "$out" ] || continue
    printf '%s\n' "$out"
    return 0
  done
}

# Read configured auth hostnames before deriving defaults from the root domain.
auth_hostname() {
  local lines v
  lines="$(persisted_auth_lines)"
  v="$(awk -F'"' -v k="$1" '$1 ~ "^[[:space:]]*" k "[[:space:]]*=[[:space:]]*$" { print $2; exit }' <<<"$lines")"
  printf '%s' "${v:-$2}"
}

# Preserve carried auth keys and derive only absent hostnames; avoid early-closing pipelines under pipefail.
auth_lines() {
  local carried
  carried="$(persisted_auth_lines)"
  grep -Eq '^[[:space:]]*admin_hostname[[:space:]]*=' <<<"$carried" ||
    printf 'admin_hostname = "op.console.%s"\n' "$FELIS_ROOT_DOMAIN"
  grep -Eq '^[[:space:]]*panel_hostname[[:space:]]*=' <<<"$carried" ||
    printf 'panel_hostname = "console.%s"\n' "$FELIS_ROOT_DOMAIN"
  if [ -n "$carried" ]; then printf '%s\n' "$carried"; fi
}

# Preserve explicit auth-source tables, including a deliberately empty Mojang-only configuration.
persisted_auth_source_blocks() {
  local f
  for f in "${STATE_DIR}/felis.host.toml" "${STATE_DIR}/felis.pod.toml"; do
    [ -r "$f" ] || continue
    # Accept whitespace and quoted TOML table headers without including the next unrelated section.
    awk '/^[[:space:]]*\[/ { f = /^[[:space:]]*\[\[[[:space:]]*["\047]?auth_source["\047]?[[:space:]]*\]\]/ }
         f { print }' "$f"
    return 0
  done
  printf '%s\n' '[[auth_source]]' 'tag = "littleskin"' 'prefix = "LS"' \
    'url = "https://littleskin.cn/api/yggdrasil/sessionserver/session/minecraft/hasJoined"'
}

# Preserve operator retention settings; installer-owned store/path stay derived and warn_before must be single-line.
persisted_archive_block() {
  local f out
  for f in "${STATE_DIR}/felis.host.toml" "${STATE_DIR}/felis.pod.toml"; do
    [ -r "$f" ] || continue
    out="$(awk '
      /^[[:space:]]*\[/ { sect = $0; next }
      sect ~ /^[[:space:]]*\[archive\][[:space:]]*$/ &&
        /^[[:space:]]*(retention|warn_before|max_local_bytes|manual_retention|manual_keep|manual_cooldown|scheduled_every|scheduled_keep|scheduled_retention)[[:space:]]*=/ { print }
    ' "$f")"
    [ -n "$out" ] || continue
    printf '%s\n' "$out"
    return 0
  done
}

# Preserve registry overrides and S3 settings, excluding installer-owned url and build_namespace.
persisted_registry_block() {
  local f out
  for f in "${STATE_DIR}/felis.host.toml" "${STATE_DIR}/felis.pod.toml"; do
    [ -r "$f" ] || continue
    out="$(awk '
      /^[[:space:]]*\[/ { sect = $0; next }
      sect ~ /^[[:space:]]*\[registry\][[:space:]]*$/ &&
        /^[[:space:]]*(kaniko_image|trivy_image|trivy_db_repository|trivy_java_db_repository|build_cpu_limit|build_mem_limit|build_disk_limit|build_user_namespaces|build_runtime_class|max_concurrent_builds|scan_fail_on|scan_fail_unfixed|scan_accept|user_uploads_context|user_uploads_max_bytes|context_max_bytes)[[:space:]]*=/ { print }
      sect ~ /^[[:space:]]*\[registry\.s3\][[:space:]]*$/ && /^[[:space:]]*[A-Za-z_]+[[:space:]]*=/ {
        if (!s3hdr) { printf "[registry.s3]\n"; s3hdr = 1 }
        print
      }
    ' "$f")"
    [ -n "$out" ] || continue
    printf '%s\n' "$out"
    return 0
  done
}

# Preserve the first readable offsite block; deleting it explicitly disables offsite storage.
persisted_offsite_block() {
  local f out
  for f in "${STATE_DIR}/felis.host.toml" "${STATE_DIR}/felis.pod.toml"; do
    [ -r "$f" ] || continue
    out="$(awk '
      /^[[:space:]]*\[/ {
        if (inoff) exit
        inoff = ($0 ~ /^[[:space:]]*\[offsite\][[:space:]]*$/)
        if (inoff) print
        next
      }
      inoff && /^[[:space:]]*(endpoint|region|bucket|prefix|access_key_ref|secret_key_ref|key_ref|db_keep)[[:space:]]*=/ { print }
    ' "$f")"
    [ -n "$out" ] || continue
    printf '%s\n' "$out"
    return 0
  done
}

# offsite_block is the [offsite] section this run writes: from FELIS_OFFSITE_* when the
# bucket is given, else the one an earlier run left.
offsite_block() {
  if [ -z "$FELIS_OFFSITE_BUCKET" ]; then
    persisted_offsite_block
    return 0
  fi
  printf '[offsite]\n'
  printf 'endpoint = "%s"\n' "$FELIS_OFFSITE_ENDPOINT"
  printf 'bucket = "%s"\n' "$FELIS_OFFSITE_BUCKET"
  if [ -n "$FELIS_OFFSITE_REGION" ]; then printf 'region = "%s"\n' "$FELIS_OFFSITE_REGION"; fi
  if [ -n "$FELIS_OFFSITE_PREFIX" ]; then printf 'prefix = "%s"\n' "$FELIS_OFFSITE_PREFIX"; fi
  if [ -n "$FELIS_OFFSITE_DB_KEEP" ]; then printf 'db_keep = %s\n' "$FELIS_OFFSITE_DB_KEEP"; fi
}

# Capture output before grep -q so pipefail cannot mistake SIGPIPE for disabled offsite storage.
offsite_enabled() {
  local block
  block="$(offsite_block)"
  grep -Eq '^[[:space:]]*bucket[[:space:]]*=[[:space:]]*"[^"]+"' <<<"$block"
}

# Only host config names the database Deployment; mode 0600 protects the URL password.
write_felis_toml() {
  local target="$1" db_addr="$2" deployment="${3:-}" deployment_line="" smtp_block auth_body auth_source_blocks registry_block archive_block offsite_section
  local config_keys game_version_line=""
  # main's installer also installs older releases, whose strict TOML parser rejects
  # newer optional keys. Ask the installed binary rather than guessing from its stamp.
  config_keys="$("$HOST_BIN" bootstrap-assets config-keys 2>/dev/null || true)"
  if grep -Fxq 'velocity.game_version' <<<"$config_keys"; then
    game_version_line="game_version = \"${MC_VERSION:-}\""
  fi
  if [ -n "$deployment" ]; then
    # Starts with the newline that ends the url line, so the pod copy has no blank line there.
    deployment_line="
# The database's pod: felis db runs its client tools there.
deployment = \"${deployment}\""
  fi
  smtp_block="$(persisted_smtp_block)"
  if [ -n "$smtp_block" ]; then
    log "carrying forward the configured [smtp] relay"
    smtp_block="${smtp_block}"$'\n' # keep a blank line before the next section
  fi
  if [ -n "$(persisted_auth_lines)" ]; then
    log "carrying forward the configured [auth] keys"
  fi
  auth_body="$(auth_lines)"
  auth_source_blocks="$(persisted_auth_source_blocks)"
  registry_block="$(persisted_registry_block)"
  if [ -n "$registry_block" ]; then
    log "carrying forward the configured [registry] overrides"
    registry_block="${registry_block}"$'\n' # keep a blank line before the next section
  fi
  archive_block="$(persisted_archive_block)"
  if [ -n "$archive_block" ]; then
    log "carrying forward the configured [archive] overrides"
    archive_block="${archive_block}"$'\n' # keep a blank line before the next section
  fi
  # The pod copy needs it as much as the host one: the reaper reads [offsite] from the
  # config Secret to know it must wait for each archive's off-site copy.
  offsite_section="$(offsite_block)"
  if [ -n "$offsite_section" ]; then
    offsite_section="${offsite_section}"$'\n\n' # keep a blank line before the next section
  fi
  write_file_atomic "$target" 0600 <<EOF
# Generated by deploy/bootstrap.sh; rerun the installer to regenerate. Hand edits are
# overwritten, except [auth], [smtp], [[auth_source]], [offsite], and the operator-owned
# [registry] / [archive] overrides, which carry forward. Move the install to another
# root domain with: sudo felis domain set <new-root-domain>
[server]
listen = "0.0.0.0:8080"
root_domain = "${FELIS_ROOT_DOMAIN}"

[database]
url = "postgres://${DB_USER}:${DB_PASSWORD}@${db_addr}/${DB_NAME}?sslmode=disable"${deployment_line}

[k8s]
namespace = "${MINECRAFT_NS}"
egress_mode = "${FELIS_EGRESS_MODE}"

[velocity]
# The two always-on system servers that felis setup provisions. They are built and imported
# into k3s by build_game_stack below, so setup never has to be told "build these first".
login_image = "${FELIS_LIMBO_IMAGE}"
lobby_image = "${FELIS_LOBBY_IMAGE}"
# The public port players connect on; the panel shows it in server addresses.
game_port = ${FELIS_GAME_PORT}
${game_version_line}

[registry]
url = "${REGISTRY_URL}"
build_namespace = "${BUILD_NS}"
${registry_block}
[archive]
store = "tarLocal"
local_path = "${FELIS_ARCHIVE_LOCAL_PATH}"
${archive_block}
${offsite_section}[auth]
${auth_body}

${smtp_block}
# Third-party Yggdrasil sources federated by the hasJoined multiplexer. Mojang is
# always the code-owned identity anchor (premium-first), prepended in Go; sources here
# append as namespace-rewritten guests. A fresh install federates LittleSkin. Edit the
# list in ${STATE_DIR}/felis.host.toml and rerun the installer; re-runs keep it as it
# is, and with no [[auth_source]] at all the server is Mojang-only.
# Order is trust: the first source that answers 200 wins, so list the most trusted roots
# first, and remove a compromised root rather than just moving it down.
# A tag is permanent: it is hashed into every player UUID of its source, so changing it
# (even its case) gives all of them new UUIDs and orphans their data, links and bans.
${auth_source_blocks}
EOF
}

ensure_default_config() {
  local target="${STATE_DIR}/felis.toml" backup
  if [ -L "$target" ] && [ "$(readlink "$target")" = "${STATE_DIR}/felis.host.toml" ]; then
    ok "default host config already points at ${STATE_DIR}/felis.host.toml"
    return 0
  fi
  if [ -e "$target" ] || [ -L "$target" ]; then
    if bootstrap_from_tui; then
      backup="${target}.bak.$(date -u +%Y%m%d%H%M%S).$$"
      warn "replacing existing ${target}; backup saved at ${backup}"
      mv "$target" "$backup"
      ln -s "${STATE_DIR}/felis.host.toml" "$target"
      ok "default host config: ${target} -> ${STATE_DIR}/felis.host.toml"
      return 0
    fi
    warn "leaving existing ${target}; setup can use -config ${STATE_DIR}/felis.host.toml if needed"
    return 0
  fi
  ln -s "${STATE_DIR}/felis.host.toml" "$target"
  ok "default host config: ${target} -> ${STATE_DIR}/felis.host.toml"
}

# 8. Migrate + deploy bundle
run_migrations() {
  local backup_flags=(-backup-dir "$FELIS_DB_BACKUP_DIR")
  write_felis_toml "${STATE_DIR}/felis.host.toml" "127.0.0.1:${PG_HOST_PORT}" "${CONTROL_NS}/${PG_DEPLOYMENT}"
  ensure_default_config
  if [ "$FELIS_PRE_MIGRATE_BACKUP" = 0 ]; then
    warn "FELIS_PRE_MIGRATE_BACKUP=0: pending migrations run without a database snapshot"
    backup_flags=(-no-backup)
  fi
  # Back up initialized databases before pending migrations; refuse migration if the backup fails.
  log "running database migrations (host binary -> ${PG_DEPLOYMENT} at 127.0.0.1:${PG_HOST_PORT})"
  # From here the database may move forward, and the binary that moved it stays.
  HOST_BIN_IN_USE=1
  "$HOST_BIN" migrate up -config "${STATE_DIR}/felis.host.toml" "${backup_flags[@]}"
  ok "migrations applied"
}

# Silence felis watchdog's mail for the rest of this install (see WATCHDOG_QUIET_FILE).
# Two hours covers a slow source build; cleanup lifts it as soon as the installer exits.
quiet_watchdog() {
  install -d -m 0755 "$(dirname "$WATCHDOG_QUIET_FILE")"
  printf '%s\n' "$(( $(date +%s) + 7200 ))" > "$WATCHDOG_QUIET_FILE"
}

# Validate offsite credentials and writer state before syncing; no configured bucket removes the timer.
install_offsite_timer() {
  if [ "${OFFSITE_ENABLED:-0}" != 1 ]; then
    if [ -e "$OFFSITE_TIMER" ] || [ -e "$OFFSITE_SERVICE" ]; then
      systemctl disable --now felis-offsite.timer >/dev/null 2>&1 || true
      rm -f "$OFFSITE_TIMER" "$OFFSITE_SERVICE"
      systemctl daemon-reload
      warn "no [offsite] bucket is configured any more; the off-site copy timer was removed"
    fi
    return 0
  fi
  cat > "$OFFSITE_SERVICE" <<EOF
[Unit]
Description=Felis off-site copy (world archives, database bundles, user registry images and submission uploads, encrypted, to the [offsite] bucket)
After=network-online.target k3s.service felis-db-backup.service
Wants=network-online.target

[Service]
Type=oneshot
EnvironmentFile=${OFFSITE_ENV}
$(ha_exec_condition)
ExecStart=${HOST_BIN} offsite sync -config ${STATE_DIR}/felis.host.toml -env-file ${OFFSITE_ENV} -db-dir ${FELIS_DB_BACKUP_DIR} -backup-pvc "${FELIS_BACKUP_PVC}"
TimeoutStartSec=24h
Nice=10
IOSchedulingClass=idle
PrivateTmp=yes
NoNewPrivileges=yes
EOF
  cat > "$OFFSITE_TIMER" <<EOF
[Unit]
Description=Hourly Felis off-site copy

[Timer]
OnCalendar=hourly
RandomizedDelaySec=10min
Persistent=true

[Install]
WantedBy=timers.target
EOF
  systemctl daemon-reload
  systemctl enable --now felis-offsite.timer
  local rc=0
  "$HOST_BIN" offsite check-key -config "${STATE_DIR}/felis.host.toml" -env-file "$OFFSITE_ENV" >/dev/null || rc=$?
  case "$rc" in
    0)
      # Standby hosts neither upload nor notify another writer's owners; record that state for the watchdog.
      local who wrc=0
      who="$("$HOST_BIN" offsite take-over -config "${STATE_DIR}/felis.host.toml" -env-file "$OFFSITE_ENV")" || wrc=$?
      systemctl start --no-block felis-offsite.service
      case "$wrc" in
        0) ok "off-site copy: hourly to the [offsite] bucket, first copy started (sudo felis offsite status; journalctl -u felis-offsite)" ;;
        4)
          OFFSITE_STANDBY=1
          warn "================================================================================"
          warn "Another host writes the [offsite] bucket, and this host was built from its backup:"
          warn "  $(printf '%s\n' "$who" | sed -n 1p | sed 's/^felis offsite take-over: //')"
          warn "This host copies nothing into the bucket and, while that host keeps writing it,"
          warn "mails no watchdog alert: a rehearsal leaves that host and its owners alone. When"
          warn "this host replaces it for good, run sudo felis offsite take-over -yes, then"
          warn "sudo systemctl start felis-offsite.service (https://docs.felismc.com/en/operations/backup#take-over)."
          warn "================================================================================"
          ;;
        5)
          OFFSITE_DISPLACED=1
          warn "================================================================================"
          warn "Another host took the [offsite] bucket over from this host:"
          warn "  $(printf '%s\n' "$who" | sed -n 1p | sed 's/^felis offsite take-over: //')"
          warn "This host copies nothing into the bucket any more, and its watchdog mails the"
          warn "owners about it. If that host is a rehearsal machine, take the bucket back:"
          warn "sudo felis offsite take-over -yes, then sudo systemctl start felis-offsite.service"
          warn "(https://docs.felismc.com/en/operations/backup#take-over)."
          warn "================================================================================"
          ;;
        *) warn "could not tell which host writes the [offsite] bucket (error above); the first copy started and says what it found: journalctl -u felis-offsite" ;;
      esac
      ;;
    3)
      # The timer stays: every run is refused (and reported by the watchdog) until the
      # key is fixed, and the first run after that needs no re-run of the installer.
      OFFSITE_KEY_MISMATCH=1
      warn "================================================================================"
      warn "The [offsite] bucket's objects are sealed with another key than the one in"
      warn "${OFFSITE_ENV} (felis offsite check-key, above). Nothing is copied off this"
      warn "machine, and nothing in the bucket is written or pruned, until the keys match."
      if [ "${OFFSITE_KEY_NEW:-0}" = 1 ]; then
        warn "This run generated that key: neither FELIS_OFFSITE_KEY nor ${OFFSITE_ENV} had one."
      fi
      warn "Set FELIS_OFFSITE_KEY in ${OFFSITE_ENV} to the key the bucket was written with and run"
      warn "sudo systemctl start felis-offsite.service, or give [offsite] an empty bucket or prefix"
      warn "and re-run the installer (https://docs.felismc.com/en/operations/backup#offsite-key)."
      warn "================================================================================"
      ;;
    *)
      warn "the [offsite] bucket did not answer (error above); nothing is copied off this machine until it does: fix ${OFFSITE_ENV} or [offsite] in ${STATE_DIR}/felis.host.toml, then sudo systemctl start felis-offsite.service"
      ;;
  esac
}

# summary_offsite is the installer's last word on where the backups live.
summary_offsite() {
  if [ "${OFFSITE_ENABLED:-0}" != 1 ]; then
    warn "NO OFF-SITE COPY: every world archive and database backup is on this machine only."
    warn "Losing its disk loses them all. Set FELIS_OFFSITE_BUCKET, FELIS_OFFSITE_ENDPOINT,"
    warn "FELIS_OFFSITE_ACCESS_KEY and FELIS_OFFSITE_SECRET_KEY and re-run (https://docs.felismc.com/en/operations/backup#offsite-configure)."
    return 0
  fi
  if [ "${OFFSITE_DISPLACED:-0}" = 1 ]; then
    warn "OFF-SITE COPY STOPPED: another host took the [offsite] bucket over (see above)."
    warn "This host's backups stay on this machine until: sudo felis offsite take-over -yes"
  fi
  if [ "${OFFSITE_STANDBY:-0}" = 1 ]; then
    warn "OFF-SITE COPY ON STANDBY: another host writes the [offsite] bucket (see above)."
    warn "This host's backups stay on this machine until: sudo felis offsite take-over -yes"
  fi
  if [ "${OFFSITE_KEY_MISMATCH:-0}" = 1 ]; then
    # A key the bucket refuses is no key to store; this run's is in OFFSITE_ENV if the
    # operator moves to an empty bucket instead.
    warn "OFF-SITE COPY STOPPED: the bucket's objects are sealed with another key (see above)."
    warn "Every backup is on this machine only until ${OFFSITE_ENV} holds that key (sudo felis offsite check-key)."
    return 0
  fi
  if [ "${OFFSITE_KEY_NEW:-0}" = 1 ]; then
    # The key itself goes to the terminal alone: whatever reads stdout and stderr (a tee'd
    # log, cloud-init, a CI artifact) keeps them on disk. The output says where the key is.
    if { {
      echo
      warn "================================================================================"
      warn "The off-site copies are encrypted with this key. Store it NOW somewhere other than"
      warn "this machine (a password manager): without it nothing in the bucket can be read."
      warn ""
      warn "    FELIS_OFFSITE_KEY=${FELIS_OFFSITE_KEY}"
      warn ""
      warn "It is also in ${OFFSITE_ENV}, which is lost with this machine."
      warn "================================================================================"
    } >"$OFFSITE_KEY_TTY" 2>&1; } 2>/dev/null; then
      warn "Off-site copy: the new encryption key was shown on the terminal and is in ${OFFSITE_ENV}; keep a copy of it off this machine."
    else
      echo
      warn "================================================================================"
      warn "The off-site copies are encrypted with a new key. Store it NOW somewhere other than"
      warn "this machine (a password manager): without it nothing in the bucket can be read."
      warn "With no terminal to show it on, it stays out of this output; read it with"
      warn ""
      warn "    sudo grep '^FELIS_OFFSITE_KEY=' ${OFFSITE_ENV}"
      warn ""
      warn "That file is lost with this machine."
      warn "================================================================================"
    fi
  else
    log "Off-site copy: the encryption key is in ${OFFSITE_ENV}; keep a copy of it off this machine."
  fi
}

# Record host update availability for the panel; installation never applies updates automatically.
install_update_check_timer() {
  cat > "$UPDATE_CHECK_SERVICE" <<EOF
[Unit]
Description=Felis component version check (felis update --record)
After=network-online.target k3s.service
Wants=network-online.target

[Service]
Type=oneshot
$(ha_exec_condition)
ExecStart=${HOST_BIN} update --record -config ${STATE_DIR}/felis.host.toml
TimeoutStartSec=5min
Nice=10
PrivateTmp=yes
NoNewPrivileges=yes
ProtectSystem=full
EOF
  cat > "$UPDATE_CHECK_TIMER" <<EOF
[Unit]
Description=Daily Felis component version check

[Timer]
OnCalendar=*-*-* 05:30:00
RandomizedDelaySec=30min
Persistent=true

[Install]
WantedBy=timers.target
EOF
  systemctl daemon-reload
  systemctl enable --now felis-update-check.timer
  systemctl start --no-block felis-update-check.service
  ok "version check: daily; the panel's Updates page shows what has a newer release (journalctl -u felis-update-check)"
}

# Run host watchdog probes with cached failure notifications; omit heartbeat flags for binary compatibility.
install_watchdog_timer() {
  local disks="/,/var/lib/rancher/k3s,/var/lib/felis" path
  for path in "$FELIS_WORLDS_HOST_PATH" "$FELIS_ARCHIVE_LOCAL_PATH" "$FELIS_DB_BACKUP_DIR"; do
    if [ -n "$path" ]; then disks="${disks},${path}"; fi
  done
  write_heartbeat_url
  install -d -m 0700 "$(dirname "$WATCHDOG_STATE")"
  cat > "$WATCHDOG_SERVICE" <<EOF
[Unit]
Description=Felis platform watchdog (health checks, owner alert mail)
After=network-online.target k3s.service
OnFailure=felis-watchdog-failed.service

[Service]
Type=oneshot
$(ha_exec_condition -or-unreachable)
ExecStart=${HOST_BIN} watchdog -config ${STATE_DIR}/felis.host.toml -state ${WATCHDOG_STATE} -quiet-file ${WATCHDOG_QUIET_FILE} -backup-dir ${FELIS_DB_BACKUP_DIR} -proxy-addr 127.0.0.1:${FELIS_GAME_PORT} -disk-paths ${disks}${NODE_IP:+ -node-ip ${NODE_IP}}
TimeoutStartSec=3min
Nice=5
PrivateTmp=yes
NoNewPrivileges=yes
ProtectSystem=full
EOF
  cat > "$WATCHDOG_FAILED_SERVICE" <<EOF
[Unit]
Description=Felis platform watchdog failure report (owner alert mail, heartbeat failure ping)

[Service]
Type=oneshot
ExecStart=${HOST_BIN} watchdog -unit-failed -config ${STATE_DIR}/felis.host.toml -state ${WATCHDOG_STATE} -quiet-file ${WATCHDOG_QUIET_FILE}
TimeoutStartSec=2min
Nice=5
PrivateTmp=yes
NoNewPrivileges=yes
ProtectSystem=full
EOF
  cat > "$WATCHDOG_TIMER" <<EOF
[Unit]
Description=Felis platform watchdog, every two minutes

[Timer]
OnBootSec=3min
OnUnitActiveSec=2min
AccuracySec=15s

[Install]
WantedBy=timers.target
EOF
  systemctl daemon-reload
  systemctl enable --now felis-watchdog.timer
  local reach="mails the owners' verified addresses"
  if [ -f "$WATCHDOG_HEARTBEAT_FILE" ]; then
    reach="${reach} and pings the heartbeat at $(heartbeat_host)"
  fi
  if systemctl start felis-watchdog.service; then
    ok "watchdog: checks every 2 minutes and ${reach} (journalctl -u felis-watchdog)"
  else
    journalctl -u felis-watchdog.service -n 20 --no-pager >&2 || true
    warn "the first watchdog run failed (log above); nothing will be mailed until it runs: sudo systemctl start felis-watchdog.service"
  fi
}

# write_heartbeat_url keeps FELIS_WATCHDOG_HEARTBEAT_URL in WATCHDOG_HEARTBEAT_FILE, mode
# 0600 and replaced whole; off removes the file, and no value keeps it as it is.
write_heartbeat_url() {
  local tmp
  case "$FELIS_WATCHDOG_HEARTBEAT_URL" in
    "") return 0 ;;
    off)
      rm -f -- "$WATCHDOG_HEARTBEAT_FILE"
      return 0
      ;;
  esac
  # mktemp creates the file 0600, before the key is in it.
  tmp="$(mktemp "${WATCHDOG_HEARTBEAT_FILE}.XXXXXX")"
  printf '%s\n' "$FELIS_WATCHDOG_HEARTBEAT_URL" > "$tmp"
  mv -f -- "$tmp" "$WATCHDOG_HEARTBEAT_FILE"
}

# heartbeat_host is the heartbeat URL as the install shows it: its scheme and host.
heartbeat_host() {
  local url rest host
  url="$(head -n 1 "$WATCHDOG_HEARTBEAT_FILE")"
  rest="${url#*://}"
  host="${rest%%/*}"
  host="${host%%\?*}"
  host="${host##*@}"
  printf '%s://%s/...' "${url%%://*}" "$host"
}

# Report the actual owner alert route, including missing SMTP delivery.
summary_alerts() {
  if panel_setting relay; then
    log "Alerts: the watchdog mails the Owner's verified email address through the relay saved in the panel."
    return 0
  fi
  if persisted_smtp_block | grep -E '^[[:space:]]*host[[:space:]]*=[[:space:]]*"[^"]' >/dev/null; then
    log "Alerts: the watchdog mails the Owner's verified email address through the [smtp] relay."
    return 0
  fi
  warn "NO ALERT MAIL: no email relay is configured, so the watchdog's alerts and the sign-in codes are only written to the journal."
  warn "Configure one in the panel (Platform settings → Mail delivery) or in 'sudo felis setup' (e: configure email), and verify the Owner's email in the panel; alerts go to that address."
}

# Panel relay/heartbeat settings override host values; unavailable database reads cannot confirm those overrides.
panel_setting() {
  "${HOST_BIN:-}" panel-settings -config "${STATE_DIR}/felis.host.toml" 2>/dev/null | grep -x "$1=yes" >/dev/null
}

# summary_heartbeat closes the install on the heartbeat: without one, nothing off this
# machine notices it going down.
summary_heartbeat() {
  if panel_setting heartbeat; then
    log "Heartbeat: every watchdog run pings the URL saved in the panel; that service mails you when the pings stop."
    return 0
  fi
  if [ -f "$WATCHDOG_HEARTBEAT_FILE" ]; then
    log "Heartbeat: every watchdog run pings $(heartbeat_host); that service mails you when the pings stop."
    return 0
  fi
  warn "NO HEARTBEAT: nothing off this machine notices it going down or its watchdog stopping."
  warn "Create a check at a monitoring service (Healthchecks.io or alike; period 2 min, grace 10 min),"
  warn "then save its ping URL in the panel (Platform settings → Monitoring heartbeat), or re-run with FELIS_WATCHDOG_HEARTBEAT_URL=<its ping URL> (https://docs.felismc.com/en/operations/monitoring#heartbeat)."
}

# Mirror build tools and vulnerability databases because build Jobs have no internet egress.
install_build_tools_timer() {
  install -d -m 0755 "$(dirname "$BUILD_TOOLS_STATUS")"
  cat > "$BUILD_TOOLS_SERVICE" <<EOF
[Unit]
Description=Copy the Felis build tools and Trivy's vulnerability DBs into the registry
After=network-online.target k3s.service
Wants=network-online.target

[Service]
Type=oneshot
$(ha_exec_condition)
ExecStart=${HOST_BIN} mirror-build-tools -endpoint ${REGISTRY_PUSH_HOST} -status ${BUILD_TOOLS_STATUS} -secrets-env ${SECRETS_ENV}
TimeoutStartSec=1h
Nice=10
PrivateTmp=yes
NoNewPrivileges=yes
ProtectSystem=full
EOF
  cat > "$BUILD_TOOLS_TIMER" <<EOF
[Unit]
Description=Refresh the Felis build tools and Trivy DBs twice a day

[Timer]
OnCalendar=*-*-* 04,16:00:00
RandomizedDelaySec=1h
Persistent=true

[Install]
WantedBy=timers.target
EOF
  systemctl daemon-reload
  systemctl enable --now felis-build-tools.timer
  systemctl start --no-block felis-build-tools.service
  ok "build tools: kaniko, trivy and the Trivy DBs are being copied into the registry (journalctl -u felis-build-tools); refreshed twice a day"
}

# Run the first database backup immediately to expose pipeline failures during installation.
# The API mounts only this Unix socket, never the host kubeconfig or SSH keys.
install_node_control_service() {
  local selinux_environment=""
  if command -v selinuxenabled >/dev/null 2>&1 && selinuxenabled; then
    command -v semodule >/dev/null 2>&1 || die "node control on SELinux requires semodule"
    cat > "${STATE_DIR}/felis-node-control.cil" <<'EOF_NODE_SELINUX'
(type felis_node_control_socket_t)
(typeattributeset file_type (felis_node_control_socket_t))
(typeattributeset non_auth_file_type (felis_node_control_socket_t))
(allow container_t felis_node_control_socket_t (dir (search getattr open read)))
(allow container_t felis_node_control_socket_t (sock_file (write open read getattr)))
(allow container_t unconfined_service_t (unix_stream_socket (connectto)))
EOF_NODE_SELINUX
    semodule -i "${STATE_DIR}/felis-node-control.cil" || die "could not install the node-control SELinux policy"
    selinux_environment="Environment=FELIS_NODE_CONTROL_SELINUX=1"
  fi
  install -d -o root -g 65532 -m 0750 /run/felis-node-control
  cat > /etc/tmpfiles.d/felis-node-control.conf <<'EOF_NODE_TMP'
d /run/felis-node-control 0750 root 65532 -
EOF_NODE_TMP
  if [ -n "$selinux_environment" ] && command -v semanage >/dev/null 2>&1; then
    # /run is an SELinux equivalence alias for /var/run. Register the canonical path.
    semanage fcontext -a -t felis_node_control_socket_t '/var/run/felis-node-control(/.*)?' 2>/dev/null \
      || semanage fcontext -m -t felis_node_control_socket_t '/var/run/felis-node-control(/.*)?'
  fi
  if [ -n "$selinux_environment" ] && command -v chcon >/dev/null 2>&1; then
    chcon -R -t felis_node_control_socket_t /run/felis-node-control 2>/dev/null || true
  fi
  cat > /etc/systemd/system/felis-node-control.service <<EOF_NODE_UNIT
[Unit]
Description=Felis host node operations
After=network-online.target k3s.service
Wants=network-online.target
[Service]
Type=simple
ExecStart=${HOST_BIN} node-control --config ${STATE_DIR}/felis.host.toml --state /var/lib/felis/node-control --namespace ${MINECRAFT_NS} --control-namespace ${CONTROL_NS}
${selinux_environment}
Restart=on-failure
RestartSec=5
UMask=0077
[Install]
WantedBy=multi-user.target
EOF_NODE_UNIT
  systemctl daemon-reload
  systemctl enable felis-node-control.service
  # A conversion task runs inside this service; restarting it would terminate the installer.
  if [ "${FELIS_NODE_CONTROL_TASK:-0}" != 1 ]; then
    systemctl restart felis-node-control.service
  fi
}

install_db_backup_timer() {
  install -d -m 0700 "$FELIS_DB_BACKUP_DIR"
  cat > "$DB_BACKUP_SERVICE" <<EOF
[Unit]
Description=Felis control-plane database backup (pg_dump + /etc/felis state)
After=k3s.service

[Service]
Type=oneshot
$(ha_exec_condition)
ExecStart=${HOST_BIN} db backup -config ${STATE_DIR}/felis.host.toml -dir ${FELIS_DB_BACKUP_DIR} -label daily -keep ${FELIS_DB_BACKUP_KEEP} -metrics-file ${FELIS_DB_BACKUP_METRICS}
Nice=10
IOSchedulingClass=idle
PrivateTmp=yes
NoNewPrivileges=yes
EOF
  cat > "$DB_BACKUP_TIMER" <<EOF
[Unit]
Description=Daily Felis control-plane database backup

[Timer]
OnCalendar=${FELIS_DB_BACKUP_TIME}
RandomizedDelaySec=15min
Persistent=true

[Install]
WantedBy=timers.target
EOF
  systemctl daemon-reload
  systemctl enable --now felis-db-backup.timer
  if systemctl start felis-db-backup.service; then
    ok "database backups: daily at ${FELIS_DB_BACKUP_TIME}, newest ${FELIS_DB_BACKUP_KEEP} kept in ${FELIS_DB_BACKUP_DIR} (first one taken now)"
  else
    journalctl -u felis-db-backup.service -n 20 --no-pager >&2 || true
    warn "the first database backup failed (log above); fix it before relying on the daily timer: sudo systemctl start felis-db-backup.service"
  fi
}

# Environment credentials override host copies; generate the encryption key only if neither supplies one.
# Reject changes to the persisted encryption key because existing bucket objects require it.
configure_offsite() {
  OFFSITE_ENABLED=0
  OFFSITE_KEY_NEW=0
  OFFSITE_KEY_MISMATCH=0
  OFFSITE_STANDBY=0
  OFFSITE_DISPLACED=0
  offsite_enabled || return 0
  OFFSITE_ENABLED=1
  local env_ak="${FELIS_OFFSITE_ACCESS_KEY:-}" env_sk="${FELIS_OFFSITE_SECRET_KEY:-}" env_key="${FELIS_OFFSITE_KEY:-}"
  local file_key=""
  FELIS_OFFSITE_ACCESS_KEY="" FELIS_OFFSITE_SECRET_KEY="" FELIS_OFFSITE_KEY=""
  if [ -f "$OFFSITE_ENV" ]; then
    # shellcheck disable=SC1090
    . "$OFFSITE_ENV"
    file_key="$FELIS_OFFSITE_KEY"
  fi
  FELIS_OFFSITE_ACCESS_KEY="${env_ak:-$FELIS_OFFSITE_ACCESS_KEY}"
  FELIS_OFFSITE_SECRET_KEY="${env_sk:-$FELIS_OFFSITE_SECRET_KEY}"
  if [ -n "$env_key" ] && [ -n "$file_key" ] && [ "$env_key" != "$file_key" ]; then
    die "FELIS_OFFSITE_KEY differs from the key in ${OFFSITE_ENV}, which sealed what this host copied to the bucket. Unset FELIS_OFFSITE_KEY to keep it; when felis offsite check-key says the bucket's objects are sealed with another key, set FELIS_OFFSITE_KEY in ${OFFSITE_ENV} by hand instead (https://docs.felismc.com/en/operations/backup#offsite-key)"
  fi
  FELIS_OFFSITE_KEY="${env_key:-$file_key}"
  if [ -z "$FELIS_OFFSITE_ACCESS_KEY" ] || [ -z "$FELIS_OFFSITE_SECRET_KEY" ]; then
    die "[offsite] names a bucket but there are no credentials for it: set FELIS_OFFSITE_ACCESS_KEY and FELIS_OFFSITE_SECRET_KEY (they are kept in ${OFFSITE_ENV})"
  fi
  if [ -z "$FELIS_OFFSITE_KEY" ]; then
    FELIS_OFFSITE_KEY="$(openssl rand -base64 32)"
    OFFSITE_KEY_NEW=1
  fi
  write_file_atomic "$OFFSITE_ENV" 0600 <<EOF
# The off-site copy's secrets (felis offsite, https://docs.felismc.com/en/operations/backup#offsite-key). Keep a copy of
# FELIS_OFFSITE_KEY somewhere other than this machine: without it the copies in the
# bucket cannot be read, and this file goes with the machine.
FELIS_OFFSITE_ACCESS_KEY='${FELIS_OFFSITE_ACCESS_KEY}'
FELIS_OFFSITE_SECRET_KEY='${FELIS_OFFSITE_SECRET_KEY}'
FELIS_OFFSITE_KEY='${FELIS_OFFSITE_KEY}'
EOF
  ok "off-site copy: secrets in ${OFFSITE_ENV}"
}

# Remove legacy traversal grants from the world root to prevent cross-world access.
# Remove other traversal bits only from k3s's root; preserve custom-root modes.
revoke_worlds_root_grant() {
  local dir
  for dir in "$K3S_STORAGE_ROOT" "$FELIS_WORLDS_HOST_PATH"; do
    [ -n "$dir" ] && [ -d "$dir" ] || continue
    if command -v getfacl >/dev/null 2>&1 && getfacl -cpn "$dir" 2>/dev/null | grep '^user:1000:' >/dev/null; then
      if setfacl -x u:1000 "$dir"; then
        log "revoked the old uid-1000 traverse grant on ${dir}"
      else
        warn "could not revoke the old uid-1000 traverse grant on ${dir}; remove it with: setfacl -x u:1000 ${dir}"
      fi
    fi
  done
  if [ -d "$K3S_STORAGE_ROOT" ] && [ -n "$(find "$K3S_STORAGE_ROOT" -maxdepth 0 -perm -o=x)" ]; then
    if chmod o-rwx "$K3S_STORAGE_ROOT"; then
      log "revoked the old world-traversable mode on ${K3S_STORAGE_ROOT} (back to k3s's 0700)"
    else
      warn "could not restore ${K3S_STORAGE_ROOT} to 0700; any local account can reach the world volumes below it: chmod o-rwx ${K3S_STORAGE_ROOT}"
    fi
  fi
}

deploy_bundle() {
  local prev_api prev_operator prev_gate
  export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
  write_felis_toml "${STATE_DIR}/felis.pod.toml" "$PG_SERVICE_ADDR"

  prev_api="$(deployment_image felis-api api)"
  prev_operator="$(deployment_image felis-operator operator)"
  prev_gate="$(deployment_image registry registry-gate)"
  if [ -n "$prev_api" ] && [ "$prev_api" != "$FELIS_IMAGE" ]; then
    PREVIOUS_FELIS_IMAGE="$prev_api"
    printf '%s\n' "$prev_api" > "${STATE_DIR}/previous-felis-image"
  fi

  # Use the embedded CRD so checkout availability cannot change installation behavior.
  log "applying MinecraftServer CRD"
  "$HOST_BIN" bootstrap-assets crd | kube apply -f -

  log "ensuring namespaces"
  local ns
  for ns in "$CONTROL_NS" "$MINECRAFT_NS" "$BUILD_NS"; do
    kube create namespace "$ns" --dry-run=client -o yaml | kube apply -f -
  done

  log "provisioning felis-config + internal caller tokens + felis-forwarding-secret + registry credentials + mail relay and uploads bucket credentials + panel TLS secrets (out-of-band, never in the bundle)"
  apply_felis_config_secrets
  # Replicate scoped caller tokens before upgraded workloads begin referencing them.
  apply_literal_secret "$CONTROL_NS" felis-service-token token "$SERVICE_TOKEN"
  apply_literal_secret "$CONTROL_NS" felis-limbo-token token "$LIMBO_TOKEN"
  apply_literal_secret "$MINECRAFT_NS" felis-limbo-token token "$LIMBO_TOKEN"
  apply_literal_secret "$CONTROL_NS" felis-build-token token "$BUILD_TOKEN"
  apply_literal_secret "$BUILD_NS" felis-build-token token "$BUILD_TOKEN"
  apply_literal_secret "$CONTROL_NS" felis-ops-token token "$OPS_TOKEN"
  # Replicate the forwarding key before creating backends; modern forwarding requires every backend to verify it.
  apply_literal_secret "$CONTROL_NS" felis-forwarding-secret secret "$FORWARDING_SECRET"
  apply_registry_secrets
  apply_setup_credential_secrets
  kube -n "$CONTROL_NS" create secret tls felis-api-tls \
    --cert="$PANEL_TLS_CERT" \
    --key="$PANEL_TLS_KEY" \
    --dry-run=client -o yaml | kube apply -f -

  revoke_worlds_root_grant

  if [ "${DISTRIBUTED:-0}" = 1 ]; then
    local controller archive_key_file="${STATE_DIR}/archive-transfer.key"
    controller="$(k3s_node_name)"
    [ -n "$controller" ] || die "distributed deployment needs a stable controller node name"
    kube label node "$controller" "felis.node-restriction.kubernetes.io/role=controller" "felis.node-restriction.kubernetes.io/identity=$controller" --overwrite
    # With HA the controller worlds without a node of their own live on is the home one
    # ha.env names, whichever controller runs this; cluster add-ons run on any controller.
    local addon_selector="{\"felis.node-restriction.kubernetes.io/identity\":\"$controller\"}"
    if [ "${CONTROLLER_HA:-0}" = 1 ]; then
      controller="$HA_HOME"
      addon_selector='{"felis.node-restriction.kubernetes.io/role":"controller"}'
    fi
    local system_deployment
    for system_deployment in coredns local-path-provisioner; do
      if kube -n kube-system get deployment "$system_deployment" >/dev/null 2>&1; then
        kube -n kube-system patch deployment "$system_deployment" --type merge \
          -p "{\"spec\":{\"template\":{\"spec\":{\"nodeSelector\":${addon_selector}}}}}"
      fi
    done
    if [ ! -s "$archive_key_file" ]; then (umask 077; openssl rand -hex 32 > "$archive_key_file"); fi
    local archive_key
    archive_key="$(cat "$archive_key_file")"
    apply_literal_secret "$CONTROL_NS" felis-archive-key key "$archive_key"
    apply_literal_secret "$MINECRAFT_NS" felis-archive-key key "$archive_key"
  fi
  log "rendering + applying the control-plane bundle"
  local -a manifest_args=(
    --felis-image "$FELIS_IMAGE"
    --postgres-image "$POSTGRES_IMAGE"
    --panel-node-port "$FELIS_PANEL_NODEPORT"
    --velocity-cidr "${NODE_IP}/32"
    --node-control-socket /run/felis-node-control/control.sock --node-control-node "$(k3s_node_name)"
  )
  if [ "${DISTRIBUTED:-0}" = 1 ]; then
    manifest_args+=(--distributed --controller-node "$controller" --egress-probe "felis-api.${CONTROL_NS}.svc:443")
  if [ "${CONTROLLER_HA:-0}" = 1 ]; then
    manifest_args+=(--controller-ha --replicated-class "$REPLICATED_CLASS")
    # Velocity runs on every controller: each one's address may reach the game servers.
    local proxy
    for proxy in $(controller_cidrs | tr , ' '); do
      [ "$proxy" = "${NODE_IP}/32" ] || manifest_args+=(--velocity-cidr "$proxy")
    done
  fi
    # Every node address, including global addresses, must be excluded from game egress.
    while read -r cidr; do
      [ -z "$cidr" ] || manifest_args+=(--server-egress-deny-cidr "$cidr")
    done < <(kube get nodes -o jsonpath='{range .items[*]}{range .status.addresses[*]}{.address}{"\n"}{end}{end}' | awk '/^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$/ {print $0"/32"}')
  fi
  local cidr
  while read -r cidr; do
    [ -n "$cidr" ] && manifest_args+=(--server-egress-deny-cidr "$cidr")
  done < <(node_global_cidrs)
  # An empty backup-PVC value explicitly disables archives; forward custom names to every renderer.
  if [ -n "$FELIS_BACKUP_PVC" ]; then
    manifest_args+=(--backup-pvc "$FELIS_BACKUP_PVC")
  else
    manifest_args+=(--backup-pvc=)
  fi
  # Archive retention always needs the reaper; world deletion additionally requires an explicit worlds root.
  if [ -n "$FELIS_BACKUP_PVC" ]; then
    manifest_args+=(--archive-local-path "$FELIS_ARCHIVE_LOCAL_PATH")
    if [ -z "$FELIS_WORLDS_HOST_PATH" ]; then
      log "idle-world retention is off: the daily reaper deletes expired backups and keeps every world (set FELIS_WORLDS_HOST_PATH=${K3S_STORAGE_ROOT} to reap worlds idle for 15 days)"
    fi
  fi
  if [ -n "$FELIS_WORLDS_HOST_PATH" ]; then
    log "retention enabled: the daily reaper will read worlds from ${FELIS_WORLDS_HOST_PATH}"
    # Create the hostPath directory with root-only access; the root reaper requires no extra traversal grants.
    if [ ! -d "$FELIS_WORLDS_HOST_PATH" ]; then
      if [ "$FELIS_WORLDS_HOST_PATH" = "$K3S_STORAGE_ROOT" ]; then
        # The provisioner would create it moments later with this same 0700 root:root;
        # creating it now keeps a fresh install from warning about its own default.
        install -d -m 0700 -o root -g root "$FELIS_WORLDS_HOST_PATH"
      else
        warn "worlds root ${FELIS_WORLDS_HOST_PATH} does not exist yet; the reaper CronJob cannot start until it does (hostPath type Directory)"
      fi
    fi
    manifest_args+=(--worlds-host-path "$FELIS_WORLDS_HOST_PATH")
  fi
  local size
  size="$(pvc_size "$CONTROL_NS" registry "$FELIS_REGISTRY_STORAGE" FELIS_REGISTRY_STORAGE)"
  if [ -n "$size" ]; then manifest_args+=(--registry-storage "$size"); fi
  size="$(pvc_size "$CONTROL_NS" felis-uploads "$FELIS_UPLOADS_STORAGE" FELIS_UPLOADS_STORAGE)"
  if [ -n "$size" ]; then manifest_args+=(--uploads-storage "$size"); fi
  size="$(pvc_size "$CONTROL_NS" felis-artifacts "$FELIS_ARTIFACTS_STORAGE" FELIS_ARTIFACTS_STORAGE)"
  if [ -n "$size" ]; then manifest_args+=(--artifacts-storage "$size"); fi
  if [ -n "$FELIS_BACKUP_PVC" ]; then
    size="$(pvc_size "$MINECRAFT_NS" "$FELIS_BACKUP_PVC" "$FELIS_BACKUP_STORAGE" FELIS_BACKUP_STORAGE)"
    if [ -n "$size" ]; then manifest_args+=(--backup-storage "$size"); fi
  fi
  "$HOST_BIN" manifests "${manifest_args[@]}" | kube apply -f -
  # Explicitly restore replica counts because unchanged client-side apply preserves a previous zero scale.
  kube -n "$CONTROL_NS" scale deployment felis-api felis-operator --replicas=1
  restart_existing_control_plane "$prev_api" "$prev_operator" "$prev_gate"

  log "waiting for control-plane rollouts"
  local d
  for d in $(kube -n "$CONTROL_NS" get deploy -o name); do
    if ! kube -n "$CONTROL_NS" rollout status "$d" --timeout=180s; then
      diagnose_rollout "$d"
      die "control-plane rollout did not complete: ${d}"
    fi
  done
  # Remove obsolete proxy-token replicas from workload namespaces that now use scoped credentials.
  kube -n "$MINECRAFT_NS" delete secret felis-service-token --ignore-not-found
  kube -n "$BUILD_NS" delete secret felis-service-token --ignore-not-found
}

# Preserve existing PVC capacity because storage classes may reject resize; report mismatches for manual action.
pvc_size() {
  local ns="$1" claim="$2" want="$3" env="$4" have
  have="$(kube -n "$ns" get pvc "$claim" -o jsonpath='{.spec.resources.requests.storage}' 2>/dev/null || true)"
  if [ -z "$have" ]; then
    printf '%s' "$want"
    return 0
  fi
  if [ -n "$want" ] && [ "$want" != "$have" ]; then
    warn "PVC ${ns}/${claim} already requests ${have}; keeping it (${env}=${want} applies to a new claim; grow this one with kubectl patch where its storage class allows expansion)"
  fi
  printf '%s' "$have"
}

# deployment_image <deployment> <container> prints the image that container of a
# control-plane Deployment runs now, or nothing when the Deployment does not exist yet.
deployment_image() {
  kube -n "$CONTROL_NS" get deployment "$1" \
    -o "jsonpath={.spec.template.spec.containers[?(@.name==\"$2\")].image}" 2>/dev/null || true
}

# Restart API, operator, and registry only for rebuilt unchanged tags, preserving rollout-undo history.
restart_existing_control_plane() {
  local prev_api="$1" prev_operator="$2" prev_gate="${3:-}"
  # Use if so an unmatched final condition does not return failure under errexit.
  if [ "$prev_api" = "$FELIS_IMAGE" ]; then
    log "restarting felis-api onto the rebuilt ${FELIS_IMAGE}"
    kube -n "$CONTROL_NS" rollout restart deployment/felis-api
  fi
  if [ "$prev_operator" = "$FELIS_IMAGE" ]; then
    log "restarting felis-operator onto the rebuilt ${FELIS_IMAGE}"
    kube -n "$CONTROL_NS" rollout restart deployment/felis-operator
  fi
  if [ "$prev_gate" = "$FELIS_IMAGE" ]; then
    log "restarting the registry's gate onto the rebuilt ${FELIS_IMAGE}"
    kube -n "$CONTROL_NS" rollout restart deployment/registry
  fi
}

# Push local repositories through loopback; external image references remain outside this mirror.
push_image_to_registry() {
  local ref="$1" push_ref
  case "$ref" in
    "${REGISTRY_URL}/"*)
      push_ref="${REGISTRY_PUSH_HOST}/${ref#"${REGISTRY_URL}/"}"
      ;;
    *)
      warn "not mirroring ${ref} into the internal registry: it is not under ${REGISTRY_URL}; once the image GC collects that tag, nothing can re-pull it"
      return 0
      ;;
  esac
  log "mirroring ${ref} into the internal registry"
  docker tag "$ref" "$push_ref" || die "could not tag ${ref} as ${push_ref} — is docker healthy?"
  local attempt=1
  until docker --config "$REGISTRY_DOCKER_CONFIG" push "$push_ref"; do
    # The gate answers writes 503 while the registry-gc sidecar sweeps; wait
    # that out, and fail at once on anything else.
    if [ "$attempt" -ge 40 ] || ! registry_read_only; then
      die "could not mirror ${ref} into the internal registry — check the registry Deployment/pod (the registry, registry-gate and registry-gc containers) and its PVC"
    fi
    warn "the registry is read-only for garbage collection; retrying the push of ${push_ref} in 30s (${attempt}/40)"
    attempt=$((attempt + 1))
    sleep 30
  done
  docker rmi "$push_ref" >/dev/null 2>&1 || true
}

# registry_read_only asks the gate, over its pod-loopback maintenance listener,
# whether a garbage-collection window is open.
registry_read_only() {
  kubectl -n "$CONTROL_NS" exec deploy/registry -c registry-gc -- \
    wget -q -O /dev/null "http://127.0.0.1:$((${REGISTRY_URL##*:} + 2))/readonly" >/dev/null 2>&1
}

# Use a temporary mode-0700 Docker config for platform credentials and remove it on exit.
registry_docker_login() {
  REGISTRY_DOCKER_CONFIG="$(umask 077; mktemp -d)"
  remember_temp "$REGISTRY_DOCKER_CONFIG"
  printf '%s' "$REGISTRY_PLATFORM_TOKEN" | docker --config "$REGISTRY_DOCKER_CONFIG" \
    login --username platform --password-stdin "$REGISTRY_PUSH_HOST" >/dev/null \
    || die "could not log in to the internal registry at ${REGISTRY_PUSH_HOST} as platform — check the registry-gate container's log and the felis-registry-auth Secret"
}

# Deploy the registry before publishing; push release bundles directly and start Docker once for locally built images.
push_images_to_registry() {
  local role img docker_used=""
  for role in felis limbo lobby paper; do
    role_prebuilt "$role" || continue
    push_release_image "$role"
  done
  for role in felis limbo lobby paper; do
    role_prebuilt "$role" && continue
    img="$(role_image "$role")"
    [ -n "$img" ] || continue
    if [ -z "$docker_used" ]; then
      ensure_docker
      registry_docker_login
      docker_used=1
    fi
    push_image_to_registry "$img"
    [ "$role" = felis ] || push_version_tag "$img"
  done
  [ -z "$docker_used" ] || prune_build_images
  stop_docker
  # Every bundle is in containerd and the registry now; a rerun that needs one fetches it again.
  if [ "$ARTIFACT_MODE" = release ]; then
    rm -rf -- "$ARTIFACT_CACHE"
  fi
}

# After publishing, retain only current Docker images and remove obsolete installer-owned tags.
# Prune unused build cache older than a week; retain images used by containers.
prune_build_images() {
  local keep=" " role repo tag digest id ref
  local gradle="${PLUGIN_BUILD_IMAGE%%[:@]*}" pinned="${PLUGIN_BUILD_IMAGE##*@}"
  for role in felis limbo lobby paper; do keep+="$(role_image "$role") "; done
  while IFS=$'\t' read -r repo tag digest id; do
    ref=""
    case "$repo" in
      "${REGISTRY_URL}/felis/"*|"${REGISTRY_PUSH_HOST}/felis/"*)
        if [ "$tag" != "<none>" ]; then ref="${repo}:${tag}"; fi ;;
      "$gradle")
        if [ "$tag" = "<none>" ] && [ "$digest" != "$pinned" ]; then ref="${repo}@${digest}"; fi ;;
    esac
    [ -n "$ref" ] || continue
    case "$keep" in *" ${ref} "*) continue ;; esac
    docker rmi "$ref" >/dev/null 2>&1 || true
  done < <(docker images --digests --format '{{.Repository}}{{"\t"}}{{.Tag}}{{"\t"}}{{.Digest}}{{"\t"}}{{.ID}}' 2>/dev/null)
  docker image prune -f >/dev/null 2>&1 || true
  docker builder prune -f --filter until=168h >/dev/null 2>&1 || true
}

# registry_manifest_digest <host/repository:tag> prints the digest the platform registry holds
# under that tag, or nothing. Reads are anonymous at the gate.
registry_manifest_digest() {
  local ref="$1" repo tag
  repo="${ref#*/}"
  tag="${repo##*:}"
  repo="${repo%:*}"
  curl -fsSI --max-time 30 \
    -H 'Accept: application/vnd.oci.image.manifest.v1+json' \
    -H 'Accept: application/vnd.docker.distribution.manifest.v2+json' \
    -H 'Accept: application/vnd.oci.image.index.v1+json' \
    -H 'Accept: application/vnd.docker.distribution.manifest.list.v2+json' \
    "http://${ref%%/*}/v2/${repo}/manifests/${tag}" 2>/dev/null \
    | tr -d '\r' | awk 'tolower($1) == "docker-content-digest:" { print $2 }' || true
}

# Skip unchanged release image digests; also publish stable game-version tags.
push_release_image() {
  local role="$1" bundle name target digest config refs ref push_ref
  read -r _ bundle name target digest config <<<"$(awk -v r="$role" '$1 == r' <<<"$RELEASE_IMAGES")"
  case "$target" in
    "${REGISTRY_URL}/"*) ;;
    *)
      warn "not mirroring ${target} into the internal registry: it is not under ${REGISTRY_URL}; once the image GC collects that tag, nothing can re-pull it"
      return 0
      ;;
  esac
  refs="$target"
  if [ "$role" != felis ] && [ -n "${MC_VERSION:-}" ]; then
    refs="${refs} ${target%:*}:${MC_VERSION}-${config:7:12}"
  fi
  for ref in $refs; do
    push_ref="${REGISTRY_PUSH_HOST}/${ref#"${REGISTRY_URL}/"}"
    if [ "$(registry_manifest_digest "$push_ref")" = "$digest" ]; then
      ok "${ref} is already in the internal registry"
      continue
    fi
    artifact_fetch "$bundle" || die "could not mirror ${ref} into the internal registry: ${bundle} (above) is gone"
    log "mirroring ${ref} into the internal registry"
    FELIS_REGISTRY_USERNAME=platform FELIS_REGISTRY_PASSWORD="$REGISTRY_PLATFORM_TOKEN" \
      "$HOST_BIN" push-image --tar "$ARTIFACT_FILE" --image "$name" --ref "$push_ref" >/dev/null \
      || die "could not mirror ${ref} into the internal registry — check the registry Deployment/pod (the registry, registry-gate and registry-gc containers) and its PVC"
  done
}

# Add immutable <Minecraft version>-<12 hex image ID> tags alongside mutable :demo.
push_version_tag() {
  local ref="$1" id versioned
  [ -n "${MC_VERSION:-}" ] || return 0
  id="$(docker image inspect -f '{{.Id}}' "$ref" 2>/dev/null)" || return 0
  id="${id#sha256:}"
  versioned="${ref%:*}:${MC_VERSION}-${id:0:12}"
  docker tag "$ref" "$versioned" || die "could not tag ${ref} as ${versioned} — is docker healthy?"
  push_image_to_registry "$versioned"
  docker rmi "$versioned" >/dev/null 2>&1 || true
}

# Record changed system-image IDs only after repinning/restart so interrupted runs retry safely.
restart_existing_system_servers() {
  local name id pods next=""
  while read -r name id; do
    [ -n "$name" ] || continue
    next="${next}${name} ${id}"$'\n'
    if [ -n "$id" ] && grep -qxF "${name} ${id}" "$SYSTEM_SERVER_IMAGES" 2>/dev/null; then
      ok "${name} system server already runs this build; left running"
      continue
    fi
    if "$HOST_BIN" pin-images --system "$name" --namespace "$MINECRAFT_NS" \
        --registry "$REGISTRY_URL" --endpoint "$REGISTRY_PUSH_HOST"; then
      continue
    fi
    pods="$(kube -n "$MINECRAFT_NS" get pod \
      -l "felis.lolicon.best/server=${name}" -o name 2>/dev/null || true)"
    [ -n "$pods" ] || continue
    warn "could not pin the ${name} system server to its new build (above); restarting its pod, which starts the new build only if its spec.image still names the bare tag. Rerun the installer once the registry answers."
    kube -n "$MINECRAFT_NS" delete pod \
      -l "felis.lolicon.best/server=${name}" --wait=false
  done <<EOF
${LOGIN_SERVER} ${LIMBO_IMAGE_ID:-}
${LOBBY_SERVER} ${LOBBY_IMAGE_ID:-}
EOF
  printf '%s' "$next" > "$SYSTEM_SERVER_IMAGES"
}

diagnose_rollout() {
  local deploy="$1" name selector pod
  name="${deploy##*/}"
  warn "rollout not complete: ${deploy}"
  kube -n "$CONTROL_NS" describe "$deploy" || true

  case "$name" in
    felis-api) selector='app.kubernetes.io/name=felis,app.kubernetes.io/component=api' ;;
    felis-operator) selector='app.kubernetes.io/name=felis,app.kubernetes.io/component=operator' ;;
    registry) selector='app.kubernetes.io/name=felis,app.kubernetes.io/component=registry' ;;
    felis-postgres) selector='app.kubernetes.io/name=felis,app.kubernetes.io/component=postgres' ;;
    *) selector='' ;;
  esac
  [ -n "$selector" ] || return 0

  kube -n "$CONTROL_NS" get pods -l "$selector" -o wide || true
  for pod in $(kube -n "$CONTROL_NS" get pods -l "$selector" -o name 2>/dev/null); do
    warn "pod detail: ${pod}"
    kube -n "$CONTROL_NS" describe "$pod" || true
    warn "recent logs: ${pod}"
    kube -n "$CONTROL_NS" logs "$pod" --all-containers --tail=120 || true
    kube -n "$CONTROL_NS" logs "$pod" --all-containers --previous --tail=120 || true
  done
}

mark_bootstrap_done() {
  date -u +%Y-%m-%dT%H:%M:%SZ > "$BOOTSTRAP_DONE"
  chmod 0644 "$BOOTSTRAP_DONE"
}

# 9. Summary
summary() {
  export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
  echo
  ok "Felis control plane deployed."
  echo
  kube -n "$CONTROL_NS" get pods -o wide || true
  echo
  systemctl --no-pager --full status felis-velocity 2>/dev/null | sed -n 1,4p || true
  echo
  log "Player panel: https://$(auth_hostname panel_hostname "console.${FELIS_ROOT_DOMAIN}") — served on 443 once your edge/Cloudflare Tunnel routes it here."
  log "Operator console (Op/Admin/Owner): https://$(auth_hostname admin_hostname "op.console.${FELIS_ROOT_DOMAIN}") — the Owner runs 'felis setup' and onboards here."
  log "Before the edge is ready: direct + self-signed at https://${NODE_IP}:${FELIS_PANEL_NODEPORT} (browser will warn on first visit)."
  log "Minecraft address: ${NODE_IP}:${FELIS_GAME_PORT} (point mc.${FELIS_ROOT_DOMAIN} here)"
  log "The proxy authenticates against Mojang and forwards the verified profile to the"
  log "login gate; the backends are reachable in-cluster only. Follow it with:"
  log "    sudo journalctl -u felis-velocity -f"
  log "Use 'sudo felis breakGlass' only for emergency local Owner recovery/reset."
  if [ -n "${PREVIOUS_FELIS_IMAGE:-}" ]; then
    echo
    log "The control plane moved from ${PREVIOUS_FELIS_IMAGE} to ${FELIS_IMAGE}"
    log "(also recorded in ${STATE_DIR}/previous-felis-image). To go back to it:"
    log "    kubectl -n ${CONTROL_NS} rollout undo deployment/felis-api deployment/felis-operator"
    log "The database stays migrated; https://docs.felismc.com/en/operations/upgrade#rollback has the full rollback."
  fi
  echo
  summary_offsite
  summary_alerts
  summary_heartbeat
  echo
  summary_next
  echo
}

# Staff existence determines setup versus status mode; database failure leaves owner state unknown.
owner_state() {
  local out
  out="$(pg_exec psql -XtA -U postgres -d "$DB_NAME" -c "SELECT EXISTS (SELECT 1 FROM users WHERE role IN ('admin', 'owner'))" 2>/dev/null || true)"
  case "$out" in
    t) echo yes ;;
    f) echo no ;;
    *) echo unknown ;;
  esac
}

# Open the controlling terminal explicitly; piped stdin contains the installer and mode 0666 does not prove a TTY exists.
setup_terminal() { [ -t 1 ] && (: <"$SETUP_TTY") 2>/dev/null; }

summary_next() {
  local owner rule="================================================================================"
  if bootstrap_from_tui; then
    log "Returning to the setup console to create the Owner account and verify panel access."
    return 0
  fi
  owner="$(owner_state)"
  log "$rule"
  if [ "$owner" = yes ]; then
    log "Felis is running. Sign in at https://$(auth_hostname admin_hostname "op.console.${FELIS_ROOT_DOMAIN}")"
    log "Local access: https://$(auth_hostname admin_hostname "op.console.${FELIS_ROOT_DOMAIN}"):${FELIS_PANEL_NODEPORT}"
    log "Passkey requires the configured hostname; direct IP access cannot use Passkey."
    log "Email, edge and storage settings: sudo felis setup"
    log "$rule"
    return 0
  fi
  if [ "$owner" = no ] && [ -z "${FELIS_NO_SETUP:-}" ] && setup_terminal; then
    SETUP_CONSOLE=1
    log "Next: create the Owner account. The setup console starts now; if you leave it,"
    log "run  sudo felis setup  to come back to it."
  else
    log "Next: create the Owner account. Run on this host:"
    log "    sudo felis setup"
  fi
  log "It starts the login and lobby servers, has you join ${NODE_IP}:${FELIS_GAME_PORT} in Minecraft to"
  log "bind your Mojang account as the Owner, then sets up how the panel is reached."
  log "$rule"
}

# Setup-console failure must not trigger installer rollback after installation has already succeeded.
start_setup_console() {
  [ "$SETUP_CONSOLE" = 1 ] || return 0
  resume_package_background_timers
  # The install is done, and felis setup can start the installer itself (its host
  # bootstrap step): an inherited run lock would stop that run as a second one.
  exec 9>&-
  "$HOST_BIN" setup <"$SETUP_TTY" \
    || warn "the setup console exited with status $?; run 'sudo felis setup' to come back to it"
}

prompt_install_mode() {
  if [ "$INSTALL_MODE" = reconfigure ]; then log "install mode: reconfigure ${FELIS_RECONFIGURE:-}"; return; fi
  if [ "$INSTALL_MODE" = worker ]; then log "install mode: worker";return; fi
  if [ "$INSTALL_MODE" = worker-upgrade ]; then log "install mode: worker update";return; fi
  if [ "$INSTALL_MODE" = controller-join ]; then log "install mode: controller join";return; fi
  # felis setup carries on to the Owner and edge setup, which needs the control plane, so
  # a nano install under it could only end in a setup error.
  if bootstrap_from_tui; then
    [ "$INSTALL_MODE" != nano ] || die "felis setup installs the full control plane; for Felis-nano run deploy/bootstrap.sh with FELIS_INSTALL_MODE=nano"
    INSTALL_MODE="full"
    log "install mode: full (felis setup)"
    return 0
  fi
  case "$INSTALL_MODE" in
    full|nano|worker) log "install mode: ${INSTALL_MODE} (from FELIS_INSTALL_MODE)"; return 0 ;;
    "") ;;
    *) die "FELIS_INSTALL_MODE must be 'full', 'nano' or 'worker', got: ${INSTALL_MODE}" ;;
  esac

  # A felis-nano unit with no full install beside it makes this re-run a nano update;
  # defaulting to full there would put k3s and Postgres on a host that asked for neither.
  local def=full n=1 reply
  if [ -e "$NANO_SERVICE" ] && [ ! -e "$BOOTSTRAP_DONE" ]; then def=nano n=2; fi

  # Prompt through /dev/tty, never piped script stdin; absent controlling terminals use defaults.
  if ! (: </dev/tty) 2>/dev/null; then
    INSTALL_MODE="$def"
    log "no terminal for a prompt; defaulting to a ${def} install (set FELIS_INSTALL_MODE=full or nano to override)"
    return 0
  fi

  printf '\n'
  printf 'What do you want to install on this host?\n'
  printf '  [1] Felis       — full control plane (k3s + Postgres + panel; orchestrates Minecraft servers)\n'
  printf '  [2] Felis-nano  — auth multiplexer only (federates Mojang + third-party Yggdrasil; no k3s/DB)\n'
  while :; do
    printf 'Choose [1/2] (default %s): ' "$n"
    IFS= read -r reply </dev/tty || reply=""
    case "$reply" in
      "") INSTALL_MODE="$def"; break ;;
      1|full|Felis|felis) INSTALL_MODE="full"; break ;;
      2|nano|felis-nano|Felis-nano) INSTALL_MODE="nano"; break ;;
      *) printf 'Please enter 1 or 2.\n' ;;
    esac
  done
  log "install mode: ${INSTALL_MODE}"
}

install_go_toolchain() {
  local arch tarball url tmp want have
  if [ -x "${GOROOT_DIR}/bin/go" ] && "${GOROOT_DIR}/bin/go" version | grep "go${FELIS_GO_VERSION} " >/dev/null; then
    ok "go ${FELIS_GO_VERSION} already installed at ${GOROOT_DIR}"
    return 0
  fi

  case "$(uname -m)" in
    x86_64|amd64) arch="amd64"; want="$GO_PINNED_SHA256_AMD64" ;;
    aarch64|arm64) arch="arm64"; want="$GO_PINNED_SHA256_ARM64" ;;
    *) die "no Go toolchain build for architecture $(uname -m); set FELIS_GO_VERSION or pre-stage ${GOROOT_DIR}" ;;
  esac
  [ "$FELIS_GO_VERSION" = "$GO_PINNED_VERSION" ] || want="$FELIS_GO_SHA256"
  [ -n "$want" ] || die "no pinned sha256 for Go ${FELIS_GO_VERSION}; set FELIS_GO_SHA256 to the linux-${arch} digest https://go.dev/dl/ lists for it, or pre-stage ${GOROOT_DIR}"

  tarball="go${FELIS_GO_VERSION}.linux-${arch}.tar.gz"
  url="https://go.dev/dl/${tarball}"
  log "installing Go ${FELIS_GO_VERSION} (${arch}) to ${GOROOT_DIR}"
  # A private directory, not a fixed /tmp name another local user could have planted first.
  tmp="$(mktemp -d)"
  remember_temp "$tmp"
  curl -fsSL --retry 5 --retry-delay 2 "$url" -o "${tmp}/${tarball}" || die "failed to download the Go toolchain: ${url}"
  # Checked before the old toolchain is removed, so a refusal leaves the host as it was.
  # Hash stdin, never the path — same reason as install_via_plugins.
  have="$(sha256sum <"${tmp}/${tarball}" | cut -d' ' -f1)"
  [ "$have" = "$want" ] || die "Go ${FELIS_GO_VERSION} (${arch}) checksum mismatch: got ${have}, expected ${want}"
  rm -rf "$GOROOT_DIR"
  mkdir -p "$(dirname "$GOROOT_DIR")"
  tar -C "$(dirname "$GOROOT_DIR")" -xzf "${tmp}/${tarball}" || die "failed to unpack ${tarball}"
  ok "go toolchain at ${GOROOT_DIR}/bin/go"
}

build_nano_binary() {
  local staged="${SRC_DIR}/.felis-nano-build"

  log "building felis from source (${SRC_DIR})${FELIS_VERSION:+ — ${FELIS_VERSION}}"
  # Stamped the same way as the image build: a nano host runs felis version and felis update too.
  ( cd "$SRC_DIR" \
    && PATH="${GOROOT_DIR}/bin:${PATH}" CGO_ENABLED=0 go build -trimpath \
        -ldflags "-X main.version=${FELIS_VERSION:-dev}" -o "$staged" ./cmd/felis ) \
    || die "go build ./cmd/felis failed"

  # The whole point of this path is the nano subcommand; a binary without it would
  # only surface as a crash-looping felis-nano.service, so fail loudly here instead.
  "$staged" -h 2>&1 | grep -w nano >/dev/null || die "built binary has no 'nano' subcommand"

  # Install a fresh executable at the destination so SELinux assigns bin_t instead of retaining a temporary label.
  mkdir -p "$(dirname "$HOST_BIN")"
  keep_previous_host_binary
  rm -f "$HOST_BIN"
  install -m 0755 "$staged" "$HOST_BIN"
  rm -f "$staged"
  command -v restorecon >/dev/null 2>&1 && restorecon "$HOST_BIN" >/dev/null 2>&1 || true

  ok "felis binary on host at ${HOST_BIN}"
}

# Neither the download nor fetch_source is an if condition or a command before && or ||, since errexit is off inside those.
acquire_felis_binary() {
  if [ -n "$FELIS_ARTIFACT_DIR" ]; then
    install_artifact_binary
  elif bootstrap_from_tui; then
    install_embedded_binary
  else
    if use_release_binary; then
      download_release_binary
    fi
    [ -n "$HAVE_PREBUILT_BINARY" ] || fetch_source
  fi
}

acquire_nano_binary() {
  # Reuse release binaries for nano to avoid requiring a local Go toolchain or checkout.
  if use_release_binary; then
    resolve_install_ref
    download_release_binary
    [ -z "$HAVE_PREBUILT_BINARY" ] || return 0
  fi
  # Build nano directly with the pinned Go toolchain; it needs no Docker daemon.
  fetch_source
  install_go_toolchain
  build_nano_binary
}

write_nano_config() {
  local target="${STATE_DIR}/felis.toml"
  # Nano's DynamicUser needs directory traversal; do not widen full-install directories containing secrets.
  if [ ! -d "$STATE_DIR" ]; then
    mkdir -p "$STATE_DIR"
    chmod 0755 "$STATE_DIR"
  elif [ ! -e "$SECRETS_ENV" ] && [ ! -e "$BOOTSTRAP_DONE" ]; then
    chmod 0755 "$STATE_DIR"
  fi
  if [ -e "$target" ]; then
    ok "config already present at ${target}; leaving it (edit it to add [[auth_source]] roots)"
    return 0
  fi
  cat > "$target" <<'EOF'
# Felis-nano — Yggdrasil hasJoined multiplexer.
# Mojang is always the first (identity) source, added in code — do NOT list it here.
# Add each third-party Yggdrasil root below (priority = order). url is the FULL
# hasJoined endpoint. After editing:  sudo systemctl restart felis-nano
#
# Order is trust: the first source that answers 200 wins, so list the most trusted roots
# first, and remove a compromised root rather than just moving it down.
# tag is permanent: it is hashed into every player UUID of its source, so changing it
# (even its case) gives all of them new UUIDs and orphans their data, links and bans.
#
# prefix is required, 1-4 letters/digits, unique per source. A player of this source
# whose name belongs to a Mojang account joins as PREFIX_name (LS_steve) instead —
# otherwise the proxy, which keys its player list on the NAME, refuses to have both
# online at once ("You are already connected to this proxy!"). Everyone else keeps
# their own name.
#
# [[auth_source]]
# tag = "littleskin"
# prefix = "LS"
# url = "https://littleskin.cn/api/yggdrasil/sessionserver/session/minecraft/hasJoined"
EOF
  chmod 0644 "$target"
  ok "wrote nano config template ${target} (edit it to add your Yggdrasil sources)"
}

# Preserve the installed nano listen address unless an explicit override is supplied.
resolve_nano_listen() {
  if [ -z "$FELIS_NANO_LISTEN" ] && [ -r "$NANO_SERVICE" ]; then
    FELIS_NANO_LISTEN="$(sed -n 's/^ExecStart=.* -listen \([^ ]*\).*$/\1/p' "$NANO_SERVICE")"
  fi
  FELIS_NANO_LISTEN="${FELIS_NANO_LISTEN:-127.0.0.1:8081}"
}

nano_listen_is_loopback() {
  case "${FELIS_NANO_LISTEN%:*}" in
    127.*|localhost|"[::1]") return 0 ;;
    *) return 1 ;;
  esac
}

configure_nano_firewall() {
  # A loopback bind is unreachable from off-host by construction, so opening the port
  # would advertise a hole nothing answers on. Only punch it for a routable bind.
  if nano_listen_is_loopback; then
    ok "nano listens on ${FELIS_NANO_LISTEN} (loopback); no firewall port opened"
    return 0
  fi
  local port="${FELIS_NANO_LISTEN##*:}" family=ipv4
  if ufw_active; then
    if [ -n "$FELIS_NANO_PROXY_CIDR" ]; then
      log "opening ufw port ${port}/tcp to ${FELIS_NANO_PROXY_CIDR} only"
      ufw allow proto tcp from "$FELIS_NANO_PROXY_CIDR" to any port "$port" comment felis-nano >/dev/null
    else
      warn "no FELIS_NANO_PROXY_CIDR, so ufw keeps ${port}/tcp closed; the summary shows how to admit your proxy"
    fi
  fi
  command -v firewall-cmd >/dev/null 2>&1 || return 0
  systemctl is-active --quiet firewalld || return 0
  # Restrict unauthenticated nano access to the proxy; obsolete proxy-CIDR rules require manual removal.
  if firewall-cmd --permanent --query-port="${port}/tcp" >/dev/null 2>&1; then
    log "closing firewalld port ${port}/tcp, which an earlier install opened to every source"
    firewall-cmd --permanent --remove-port="${port}/tcp"
  fi
  if [ -n "$FELIS_NANO_PROXY_CIDR" ]; then
    case "$FELIS_NANO_PROXY_CIDR" in *:*) family=ipv6 ;; esac
    log "opening firewalld port ${port}/tcp to ${FELIS_NANO_PROXY_CIDR} only"
    firewall-cmd --permanent --add-rich-rule="rule family=\"${family}\" source address=\"${FELIS_NANO_PROXY_CIDR}\" port port=\"${port}\" protocol=\"tcp\" accept"
  else
    warn "no FELIS_NANO_PROXY_CIDR, so firewalld keeps ${port}/tcp closed; the summary shows how to admit your proxy"
  fi
  firewall-cmd --reload
}

install_nano_service() {
  # DynamicUser: no static account, ephemeral UID. nano writes nothing (logs go to
  # journald) and only reads the world-readable felis.toml, so strict sandboxing fits.
  cat > "$NANO_SERVICE" <<EOF
[Unit]
Description=Felis-nano Yggdrasil hasJoined multiplexer
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=${HOST_BIN} nano -config ${STATE_DIR}/felis.toml -listen ${FELIS_NANO_LISTEN}
Restart=on-failure
RestartSec=5
DynamicUser=yes
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable felis-nano
  # restart, not `enable --now`: on a re-run the service is already active and --now would
  # leave the OLD binary running against the NEW unit. Converge means converge.
  HOST_BIN_IN_USE=1
  systemctl restart felis-nano
  # restart returns as soon as the process is forked. A config the new binary rejects, or a
  # file it cannot open, only shows once it has exited and the unit sits in auto-restart.
  sleep 2
  if ! systemctl is-active --quiet felis-nano; then
    journalctl -u felis-nano -n 20 --no-pager || true
    die "felis-nano did not stay up; its last log lines are above"
  fi
  ok "felis-nano.service enabled and started (listen ${FELIS_NANO_LISTEN})"
}

summary_nano() {
  local port="${FELIS_NANO_LISTEN##*:}" host="${FELIS_NANO_LISTEN%:*}"
  # A wildcard bind names no address a proxy could dial; the node's own is the useful one.
  case "$host" in ""|0.0.0.0|"[::]") host="${NODE_IP}" ;; esac
  echo
  ok "Felis-nano deployed."
  echo
  systemctl --no-pager --full status felis-nano 2>/dev/null | sed -n 1,6p || true
  echo
  log "hasJoined endpoint: http://${host}:${port}/session/minecraft/hasJoined"
  log "Point Velocity at it — add to the proxy JVM startup flags (between java and -jar):"
  log "    -Dmojang.sessionserver=http://${host}:${port}/session/minecraft/hasJoined"
  log "    (the FULL endpoint URL, path included — Velocity's default for this property is"
  log "     the full https://sessionserver.mojang.com/session/minecraft/hasJoined)"
  if nano_listen_is_loopback; then
    log "Bound to loopback: reachable from Velocity on THIS host, and from nowhere else."
    log "Proxy on another machine? Re-run with the address on the sudo line (sudo drops"
    log "exported variables):"
    log "    curl -fsSL <raw-url>/deploy/bootstrap.sh | sudo FELIS_NANO_LISTEN=<private-ip>:${port} FELIS_NANO_PROXY_CIDR=<proxy-ip>/32 bash"
    log "firewalld or ufw then admits ${port}/tcp ONLY from that proxy — hasJoined takes no auth token,"
    log "so an internet-facing one is a free auth relay burning your Mojang egress IP."
  elif [ -n "$FELIS_NANO_PROXY_CIDR" ]; then
    log "Bound to ${FELIS_NANO_LISTEN}. firewalld or ufw, where it runs, admits ${port}/tcp only from"
    log "${FELIS_NANO_PROXY_CIDR}; any other firewall in front of this host must do the same."
  else
    log "WARNING: bound to ${FELIS_NANO_LISTEN} with no FELIS_NANO_PROXY_CIDR. hasJoined takes no auth"
    log "token, so admit ${port}/tcp from your proxy alone, or anyone can relay their logins"
    log "through you. firewalld or ufw, where it runs, keeps the port closed until you add one of:"
    log "    firewall-cmd --permanent --add-rich-rule='rule family=\"ipv4\" source address=\"<proxy-ip>/32\" port port=\"${port}\" protocol=\"tcp\" accept' && firewall-cmd --reload"
    log "    ufw allow proto tcp from <proxy-ip>/32 to any port ${port} comment felis-nano"
  fi
  log "Then edit ${STATE_DIR}/felis.toml to add your [[auth_source]] roots and run:"
  log "    sudo systemctl restart felis-nano"
  log "Follow live login traffic with:  sudo journalctl -u felis-nano -f"
  echo
}

main_nano() {
  detect_node_ip
  install_base
  acquire_nano_binary
  write_nano_config
  install_nano_service
  configure_nano_firewall
  summary_nano
}

# felis runs k3s from PATH (felis db's tools in the database pod, the MinecraftServer
# export in every bundle), and sudo's secure_path on EL leaves out /usr/local/bin, where
# k3s lives.
ensure_k3s_on_path() {
  case ":${PATH}:" in
    *":${K3S_BIN_DIR}:"*) ;;
    *) PATH="${K3S_BIN_DIR}:${PATH}"; export PATH ;;
  esac
}

# Workers install no controller credentials; ha.env preserves controller mode, home, and floating address.

validate_ha_settings() {
  case "${CONTROLLER_HA:-}" in
    "" | 0 | 1) ;;
    *) die "FELIS_CONTROLLER_HA must be 1 or unset (got '${CONTROLLER_HA}')" ;;
  esac
  if [ -n "$FELIS_HA_VIP" ] && ! [[ "$FELIS_HA_VIP" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+/[0-9]+$ ]]; then
    die "FELIS_HA_VIP must be an IPv4 address with its prefix, like 192.0.2.50/24 (got '${FELIS_HA_VIP}')"
  fi
  if ! [[ "$FELIS_HA_VRID" =~ ^[0-9]+$ ]] || [ "$FELIS_HA_VRID" -lt 1 ] || [ "$FELIS_HA_VRID" -gt 255 ]; then
    die "FELIS_HA_VRID must be 1 to 255 (got '${FELIS_HA_VRID}')"
  fi
  [[ "$FELIS_LONGHORN_VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "FELIS_LONGHORN_VERSION must be a release tag like v1.8.1 (got '${FELIS_LONGHORN_VERSION}')"
}

# recall_ha_settings reads ha.env: what the environment does not set comes from there.
recall_ha_settings() {
  if [ -f "$HA_ENV" ]; then
    local key value
    while IFS='=' read -r key value; do
      case "$key" in
        HA) [ -n "$CONTROLLER_HA" ] || CONTROLLER_HA="$value" ;;
        HOME) HA_HOME="$value" ;;
        VIP) [ -n "$FELIS_HA_VIP" ] || FELIS_HA_VIP="$value" ;;
        VIP_INTERFACE) [ -n "$FELIS_HA_VIP_INTERFACE" ] || FELIS_HA_VIP_INTERFACE="$value" ;;
        VRID) [ -z "$value" ] || FELIS_HA_VRID="$value" ;;
      esac
    done < "$HA_ENV"
  fi
  if [ "${CONTROLLER_HA:-0}" != 1 ]; then
    CONTROLLER_HA=0
    return 0
  fi
  [ "${DISTRIBUTED:-0}" = 1 ] || die "controller high availability (FELIS_CONTROLLER_HA=1) builds on distributed mode; set FELIS_DISTRIBUTED=1 too"
  validate_ha_settings
}

# write_ha_env records the HA settings, the home controller first named when HA was
# turned on. It travels to every controller that joins (in /etc/felis).
write_ha_env() {
  [ "${CONTROLLER_HA:-0}" = 1 ] || return 0
  [ -n "$HA_HOME" ] || HA_HOME="$(k3s_node_name)"
  [ -n "$HA_HOME" ] || die "controller high availability needs this controller's node name"
  mkdir -p "$(dirname "$HA_ENV")"
  (
    umask 077
    {
      echo "# Written by the Felis installer (deploy/bootstrap.sh): controller high availability."
      echo "HA=1"
      echo "HOME=${HA_HOME}"
      echo "VIP=${FELIS_HA_VIP}"
      echo "VIP_INTERFACE=${FELIS_HA_VIP_INTERFACE}"
      echo "VRID=${FELIS_HA_VRID}"
    } > "$HA_ENV"
  )
}

# Run cluster-wide timers only on the host leader; watchdog may use the unreachable exception.
ha_exec_condition() {
  [ "${CONTROLLER_HA:-0}" = 1 ] || return 0
  printf 'ExecCondition=%s ha leader -q%s\n' "$HOST_BIN" "${1:+ $1}"
}

# controller_cidrs prints every controller's addresses as exact CIDRs, comma-separated.
controller_cidrs() {
  "$K3S_BIN" kubectl --kubeconfig "$K3S_KUBECONFIG" get nodes -l felis.node-restriction.kubernetes.io/role=controller \
    -o jsonpath='{range .items[*].status.addresses[?(@.type=="InternalIP")]}{.address}{"\n"}{end}{range .items[*].status.addresses[?(@.type=="ExternalIP")]}{.address}{"\n"}{end}' \
    | awk 'NF { print (index($0, ":") ? $0 "/128" : $0 "/32") }' | sort -u | paste -sd, -
}

# Admit etcd, supervisor, kubelet, and WireGuard traffic between controllers without disabling host firewalls.
configure_ha_host_firewall() {
  [ "${CONTROLLER_HA:-0}" = 1 ] || return 0
  local peers="$1" peer
  [ -n "$peers" ] || return 0
  if ufw_active; then
    for peer in ${peers//,/ }; do
      ufw allow from "$peer" to any port 2379:2380 proto tcp comment felis-ha-etcd >/dev/null
      ufw allow from "$peer" to any port 6443:6444 proto tcp comment felis-ha-supervisor >/dev/null
      ufw allow from "$peer" to any port 10250 proto tcp comment felis-ha-kubelet >/dev/null
      ufw allow from "$peer" to any port 51820:51821 proto udp comment felis-ha-wireguard >/dev/null
    done
  fi
  if command -v firewall-cmd >/dev/null 2>&1 && systemctl is-active --quiet firewalld; then
    for peer in ${peers//,/ }; do
      firewall-cmd --permanent --zone=trusted --add-source="$peer" >/dev/null
    done
    firewall-cmd --reload >/dev/null
  fi
}

# install_iscsi installs the iSCSI initiator Longhorn attaches volumes through.
install_iscsi() {
  case "$PKG" in
    apt | pacman | zypper) pkg_install open-iscsi ;;
    dnf | yum) pkg_install iscsi-initiator-utils ;;
  esac
  systemctl enable --now iscsid >/dev/null 2>&1 || systemctl enable --now iscsid.socket >/dev/null 2>&1 \
    || die "the iSCSI initiator (iscsid) does not start; Longhorn needs it"
}

# Constrain Longhorn replicas and components to controllers and release Pods after node loss.
longhorn_default_settings() {
  local manifest="$1" tmp
  tmp="$(mktemp)"
  remember_temp "$tmp"
  awk '
    { print }
    /default-setting\.yaml: *\|-? *$/ && !done {
      match($0, /^ */)
      pad = substr($0, 1, RLENGTH) "  "
      print pad "create-default-disk-labeled-nodes: true"
      print pad "node-down-pod-deletion-policy: delete-both-statefulset-and-deployment-pod"
      print pad "system-managed-components-node-selector: \"felis.node-restriction.kubernetes.io/role:controller\""
      done = 1
    }
    END { if (!done) exit 3 }
  ' "$manifest" > "$tmp" || die "the Longhorn ${FELIS_LONGHORN_VERSION} manifest has no default-setting.yaml block to configure"
  cat "$tmp" > "$manifest"
}

# install_longhorn installs Longhorn on a controller of an HA cluster, labels this node a
# home for replicas, and waits until its CSI driver can provision volumes.
install_longhorn() {
  [ "${CONTROLLER_HA:-0}" = 1 ] || return 0
  install_iscsi
  local self manifest selector i
  self="$(k3s_node_name)"
  kube label node "$self" "${LONGHORN_DISK_LABEL}=true" --overwrite >/dev/null
  if kube get csidriver driver.longhorn.io >/dev/null 2>&1; then
    ok "Longhorn already installed"
  else
    log "installing Longhorn ${FELIS_LONGHORN_VERSION} for the control plane's replicated volumes"
    manifest="$(mktemp)"
    remember_temp "$manifest"
    curl -fsSL --retry 5 --retry-delay 2 "https://raw.githubusercontent.com/longhorn/longhorn/${FELIS_LONGHORN_VERSION}/deploy/longhorn.yaml" -o "$manifest" \
      || die "could not fetch Longhorn ${FELIS_LONGHORN_VERSION}"
    longhorn_default_settings "$manifest"
    kube apply -f "$manifest" >/dev/null
  fi
  selector='{"spec":{"template":{"spec":{"nodeSelector":{"felis.node-restriction.kubernetes.io/role":"controller"}}}}}'
  kube -n longhorn-system patch daemonset longhorn-manager --type merge -p "$selector" >/dev/null
  for i in longhorn-driver-deployer longhorn-ui; do
    kube -n longhorn-system patch deployment "$i" --type merge -p "$selector" >/dev/null 2>&1 || true
  done
  kube -n longhorn-system rollout status daemonset/longhorn-manager --timeout=600s \
    || die "Longhorn did not start (kubectl -n longhorn-system get pods)"
  for i in $(seq 1 120); do
    kube get csidriver driver.longhorn.io >/dev/null 2>&1 && break
    sleep 5
  done
  kube get csidriver driver.longhorn.io >/dev/null 2>&1 || die "Longhorn's CSI driver did not register (kubectl -n longhorn-system get pods)"
  "$HOST_BIN" manifests --only replicated-class --replicated-class "$REPLICATED_CLASS" | kube apply -f - >/dev/null
  ok "Longhorn ready; control-plane volumes use the ${REPLICATED_CLASS} class"
}

# Copy control-plane volumes to Longhorn once, retaining originals for recovery.
ha_migrate_storage() {
  [ "${CONTROLLER_HA:-0}" = 1 ] || return 0
  HOST_BIN_IN_USE=1
  "$HOST_BIN" ha migrate-storage -node "$(k3s_node_name)" -image "$FELIS_IMAGE" -class "$REPLICATED_CLASS" \
    -control-namespace "$CONTROL_NS" -minecraft-namespace "$MINECRAFT_NS" -backup-pvc "$FELIS_BACKUP_PVC" \
    || die "the control plane's volumes could not be moved to Longhorn; nothing was deleted, and a rerun carries on"
}

# ha_vip_interface is the interface the floating address goes on: the one named, else
# the one carrying NODE_IP.
ha_vip_interface() {
  if [ -n "$FELIS_HA_VIP_INTERFACE" ]; then
    printf '%s\n' "$FELIS_HA_VIP_INTERFACE"
    return 0
  fi
  ip -o -4 addr show | awk -v ip="$NODE_IP" '{ split($4, a, "/"); if (a[1] == ip) { print $2; exit } }'
}

# Float the shared-network VRRP address toward the host leader running the control plane.
install_keepalived() {
  [ "${CONTROLLER_HA:-0}" = 1 ] && [ -n "$FELIS_HA_VIP" ] || return 0
  local iface pass
  iface="$(ha_vip_interface)"
  [ -n "$iface" ] || die "no interface carries ${NODE_IP}; set FELIS_HA_VIP_INTERFACE"
  command -v keepalived >/dev/null 2>&1 || pkg_install keepalived
  # One VRRP password for all the controllers: written once here, it travels in
  # /etc/felis to every controller that joins.
  if [ ! -s "$HA_VRRP_PASS" ]; then
    (umask 077; openssl rand -hex 4 > "$HA_VRRP_PASS")
  fi
  pass="$(cat "$HA_VRRP_PASS")"
  mkdir -p /etc/keepalived
  (
    umask 077
    printf '%s\n' \
      "# Written by the Felis installer (deploy/bootstrap.sh): the controllers' floating address." \
      "global_defs {" \
      "  enable_script_security" \
      "  script_user root" \
      "}" \
      "vrrp_script felis_leader {" \
      "  script \"${HOST_BIN} ha leader -q\"" \
      "  interval 5" \
      "  timeout 15" \
      "  weight 20" \
      "}" \
      "vrrp_instance FELIS {" \
      "  state BACKUP" \
      "  interface ${iface}" \
      "  virtual_router_id ${FELIS_HA_VRID}" \
      "  priority 100" \
      "  advert_int 1" \
      "  authentication {" \
      "    auth_type PASS" \
      "    auth_pass ${pass}" \
      "  }" \
      "  virtual_ipaddress {" \
      "    ${FELIS_HA_VIP} dev ${iface}" \
      "  }" \
      "  track_script {" \
      "    felis_leader" \
      "  }" \
      "}" > "$KEEPALIVED_CONF"
  )
  unset pass
  systemctl enable keepalived >/dev/null 2>&1
  systemctl restart keepalived
  ok "floating address ${FELIS_HA_VIP} on ${iface} (VRRP router ${FELIS_HA_VRID})"
}

# HA database rendering selects replicated storage and permits scheduling on any controller.
postgres_ha_args() {
  [ "${CONTROLLER_HA:-0}" = 1 ] || return 0
  printf -- '--distributed --controller-node %s --controller-ha --replicated-class %s\n' "$HA_HOME" "$REPLICATED_CLASS"
}


main_controller_join() {
  [ -n "$WORKER_NAME" ] && [[ "$WORKER_NAME" =~ ^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$ ]] || die "FELIS_NODE_NAME is required and must be a DNS node name"
  [[ "$WORKER_SERVER" =~ ^https://([a-zA-Z0-9.:-]+|\[[0-9a-fA-F:]+\]):6443$ ]] || die "FELIS_SERVER_URL must be an HTTPS k3s endpoint on port 6443"
  [[ "$WORKER_REGISTRY_IP" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "FELIS_REGISTRY_CLUSTER_IP is required"
  [ -n "$WORKER_PEERS" ] || die "FELIS_PEER_CIDRS must list exact cluster peer addresses"
  [ -n "$CONTROLLER_PEERS" ] || die "FELIS_CONTROLLER_PEERS must list every controller's exact address"
  local f
  for f in "$SERVER_TOKEN_FILE" "$AGENT_TOKEN_FILE" "$ETC_BUNDLE"; do
    [ -s "$f" ] && [ -f "$f" ] && [ ! -L "$f" ] || die "controller join needs FELIS_SERVER_TOKEN_FILE, FELIS_AGENT_TOKEN_FILE and FELIS_ETC_BUNDLE as regular files"
    (( (8#$(stat -c %a "$f") & 077) == 0 )) || die "$f must not be readable by group or others (use chmod 600)"
  done
  [ ! -e /var/lib/rancher/k3s/server ] && [ ! -e /var/lib/rancher/k3s/agent ] || die "k3s is already installed here; a controller joins only onto a fresh host"
  bootstrap_from_tui || die "a controller joins with the existing controller's felis binary (FELIS_BOOTSTRAP_BINARY)"
  detect_node_ip
  [ -n "$NODE_EXTERNAL_IP" ] || NODE_EXTERNAL_IP="$NODE_IP"
  PREFLIGHT_PROBLEMS=()
  preflight_platform; preflight_memory; preflight_disk; preflight_networks; preflight_outbound
  [ "${#PREFLIGHT_PROBLEMS[@]}" = 0 ] || die "controller preflight failed: ${PREFLIGHT_PROBLEMS[*]}"
  ensure_swap
  install_base
  ensure_time_sync
  ensure_persistent_journal
  acquire_felis_binary
  HOST_BIN_IN_USE=1
  # The cluster's state: this controller's configuration and secrets are the first one's.
  mkdir -p /etc/rancher/k3s/config.yaml.d
  tar -xf "$ETC_BUNDLE" -C /etc --no-same-owner
  chmod 0700 /etc/felis
  install -m 0600 "$SERVER_TOKEN_FILE" /etc/rancher/k3s/felis-server-token
  install -m 0600 "$AGENT_TOKEN_FILE" /etc/rancher/k3s/felis-agent-token
  DISTRIBUTED=1
  CONTROLLER_HA=1
  HA_JOIN_SERVER="$WORKER_SERVER"
  recall_ha_settings
  "$HOST_BIN" node firewall --controller --controller-ip "$NODE_EXTERNAL_IP" --peers "$WORKER_PEERS" --controller-peers "$CONTROLLER_PEERS" \
    --pod-cidr "$POD_CIDR" --node-port "$FELIS_PANEL_NODEPORT" --control-namespace "$CONTROL_NS" --namespace "$MINECRAFT_NS"
  configure_ha_host_firewall "$CONTROLLER_PEERS"
  write_k3s_service_dropin
  K3S_RESTART_NEEDED=0
  write_k3s_config
  # The registry runs beside the control plane, on whichever controller that is: pull
  # through its Service.
  cat > "$K3S_REGISTRIES_FILE" <<EOF_MIRROR
mirrors:
  "$REGISTRY_URL":
    endpoint:
      - "http://${WORKER_REGISTRY_IP}:5000"
EOF_MIRROR
  chmod 0600 "$K3S_REGISTRIES_FILE"
  stage_k3s_airgap_images
  run_k3s_installer
  systemctl enable --now k3s
  export KUBECONFIG="$K3S_KUBECONFIG"
  wait_for_node_ready
  chmod 0600 "$K3S_KUBECONFIG"
  install_iscsi
  load_or_make_secrets
  install_node_control_service
  install_velocity
  install_db_backup_timer
  install_offsite_timer
  install_update_check_timer
  install_watchdog_timer
  install_keepalived
  ok "controller ${WORKER_NAME} joined; the existing controller labels it once it is ready"
}


main_worker() {
  [ -n "$WORKER_NAME" ] && [[ "$WORKER_NAME" =~ ^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$ ]] || die "FELIS_NODE_NAME is required and must be a DNS node name"
  [[ "$WORKER_SERVER" =~ ^https://([a-zA-Z0-9.:-]+|\[[0-9a-fA-F:]+\]):6443$ ]] || die "FELIS_SERVER_URL must be an HTTPS k3s endpoint on port 6443"
  [[ "$WORKER_REGISTRY_IP" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "FELIS_REGISTRY_CLUSTER_IP is required"
  [ -n "$WORKER_PEERS" ] || die "FELIS_PEER_CIDRS must list exact cluster peer addresses"
  [ -s "$WORKER_TOKEN_FILE" ] || die "FELIS_BOOTSTRAP_TOKEN_FILE must name a secure limited bootstrap token file"
  local token saved mode
  [ -f "$WORKER_TOKEN_FILE" ] && [ ! -L "$WORKER_TOKEN_FILE" ] || die "bootstrap token must be a regular file"
  mode="$(stat -c %a "$WORKER_TOKEN_FILE")"
  (( (8#$mode & 077) == 0 )) || die "bootstrap token must not be readable by group or others (use chmod 600)"
  token="$(cat "$WORKER_TOKEN_FILE")"
  [[ "$token" =~ ^K10[0-9a-f]{64}::[a-z0-9]{6}\.[a-z0-9]{16}$ ]] || die "worker accepts only CA-pinned bootstrap tokens, never server or static agent tokens"
  [ ! -e /var/lib/rancher/k3s/server ] || die "this host has a k3s server; refusing to turn a controller into a worker"
  if [ -d /var/lib/rancher/k3s/agent ]; then
    saved="$(k3s_node_name)"
    [ -n "$saved" ] && [ "$saved" = "$WORKER_NAME" ] || die "cannot change or guess an installed worker identity"
  fi
  if [ -f "$K3S_CONFIG_DROPIN" ]; then
    saved="$(awk -F'"' '/^node-name:/ {print $2;exit}' "$K3S_CONFIG_DROPIN")"
    [ -z "$saved" ] || [ "$saved" = "$WORKER_NAME" ] || die "existing node identity is $saved; refusing to rename it"
    saved="$(awk -F'"' '/^server:/ {print $2;exit}' "$K3S_CONFIG_DROPIN")"
    [ -z "$saved" ] || [ "$saved" = "$WORKER_SERVER" ] || die "existing worker belongs to another controller"
  fi
  detect_node_ip
  [ -n "$NODE_EXTERNAL_IP" ] || NODE_EXTERNAL_IP="$NODE_IP"
  PREFLIGHT_PROBLEMS=()
  preflight_platform; preflight_memory; preflight_disk; preflight_networks; preflight_outbound
  local unit
  for unit in rke2-server rke2-agent k0scontroller k0sworker snap.microk8s.daemon-kubelite kubelet k3s; do
    if systemctl is-active --quiet "$unit.service"; then preflight_fail "conflicting Kubernetes service: $unit"; fi
  done
  [ "${#PREFLIGHT_PROBLEMS[@]}" = 0 ] || die "worker preflight failed: ${PREFLIGHT_PROBLEMS[*]}"
  ensure_swap
  install_base
  ensure_time_sync
  ensure_persistent_journal
  # Reuse the release binary path for the local admission checks, without importing game/platform images.
  bootstrap_from_tui || [ -n "$FELIS_ARTIFACT_DIR" ] || [ -n "${FELIS_SKIP_FETCH:-}" ] || resolve_install_ref
  acquire_felis_binary
  if [ -z "$HAVE_PREBUILT_BINARY" ]; then install_go_toolchain; build_nano_binary; fi
  mkdir -p /etc/rancher/k3s/config.yaml.d
  (umask 077; printf '%s\n' "$token" > /etc/rancher/k3s/felis-bootstrap-token)
  unset token
  cat > "$K3S_CONFIG_DROPIN" <<EOF_WORKER
server: "$WORKER_SERVER"
node-name: "$WORKER_NAME"
node-external-ip: "$NODE_EXTERNAL_IP"
token-file: "/etc/rancher/k3s/felis-bootstrap-token"
disable-default-registry-endpoint: true
node-taint:
  - "felis.lolicon.best/unapproved=true:NoSchedule"
EOF_WORKER
  chmod 0600 "$K3S_CONFIG_DROPIN"
  cat > "$K3S_REGISTRIES_FILE" <<EOF_MIRROR
mirrors:
  "$REGISTRY_URL":
    endpoint:
      - "http://${WORKER_REGISTRY_IP}:5000"
EOF_MIRROR
  chmod 0600 "$K3S_REGISTRIES_FILE"
  HOST_BIN_IN_USE=1
  "$HOST_BIN" node firewall --peers "$WORKER_PEERS" --controller-ip "${WORKER_SERVER#https://}" --pod-cidr "$POD_CIDR" --node-port "$FELIS_PANEL_NODEPORT"
  configure_extra_ports_firewall
  if [ ! -x "$K3S_BIN" ]; then
    stage_k3s_airgap_images
    curl -sfL --retry 5 --retry-delay 2 "https://raw.githubusercontent.com/k3s-io/k3s/${FELIS_K3S_VERSION}/install.sh" | \
      INSTALL_K3S_VERSION="$FELIS_K3S_VERSION" INSTALL_K3S_BIN_DIR="$K3S_BIN_DIR" INSTALL_K3S_EXEC=agent sh -
  else
    [ "$("$K3S_BIN" --version | awk 'NR==1 {print $3}')" = "$FELIS_K3S_VERSION" ] || die "worker k3s version differs from pinned controller version; upgrade in a maintenance window"
    systemctl enable --now k3s-agent
    systemctl restart k3s-agent
  fi
  ok "worker $WORKER_NAME joined under quarantine; run felis node approve on A"
}

# Worker upgrades preserve node and controller identity and require stopped servers.
main_worker_upgrade() {
  [ -n "$WORKER_NAME" ] && [[ "$WORKER_NAME" =~ ^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$ ]] || die "FELIS_NODE_NAME is required and must be a DNS node name"
  [[ "$WORKER_SERVER" =~ ^https://([a-zA-Z0-9.:-]+|\[[0-9a-fA-F:]+\]):6443$ ]] || die "FELIS_SERVER_URL must be an HTTPS k3s endpoint on port 6443"
  [ -n "$WORKER_PEERS" ] || die "FELIS_PEER_CIDRS must list exact cluster peer addresses"
  [ -d /var/lib/rancher/k3s/agent ] && [ ! -e /var/lib/rancher/k3s/server ] || die "this host is not a Felis worker; join it from the controller first"
  [ -f "$K3S_CONFIG_DROPIN" ] || die "the worker's k3s settings are missing ($K3S_CONFIG_DROPIN)"
  local saved
  saved="$(awk -F'"' '/^node-name:/ {print $2;exit}' "$K3S_CONFIG_DROPIN")"
  [ "$saved" = "$WORKER_NAME" ] || die "this worker is ${saved:-unnamed}, not $WORKER_NAME"
  saved="$(awk -F'"' '/^server:/ {print $2;exit}' "$K3S_CONFIG_DROPIN")"
  [ "$saved" = "$WORKER_SERVER" ] || die "this worker belongs to another controller (${saved:-none})"
  detect_node_ip
  bootstrap_from_tui || die "a worker update takes the controller's felis binary (FELIS_BOOTSTRAP_BINARY)"
  acquire_felis_binary
  HOST_BIN_IN_USE=1
  "$HOST_BIN" node firewall --peers "$WORKER_PEERS" --controller-ip "${WORKER_SERVER#https://}" --pod-cidr "$POD_CIDR" --node-port "$FELIS_PANEL_NODEPORT"
  configure_extra_ports_firewall
  if [ "$("$K3S_BIN" --version 2>/dev/null | awk 'NR==1 {print $3}')" = "$FELIS_K3S_VERSION" ]; then
    ok "worker $WORKER_NAME already runs k3s $FELIS_K3S_VERSION"
  else
    stage_k3s_airgap_images
    curl -sfL --retry 5 --retry-delay 2 "https://raw.githubusercontent.com/k3s-io/k3s/${FELIS_K3S_VERSION}/install.sh" | \
      INSTALL_K3S_VERSION="$FELIS_K3S_VERSION" INSTALL_K3S_BIN_DIR="$K3S_BIN_DIR" INSTALL_K3S_EXEC=agent sh - \
      || die "could not move k3s-agent to $FELIS_K3S_VERSION"
    ok "worker $WORKER_NAME moved to k3s $FELIS_K3S_VERSION"
  fi
  ok "worker $WORKER_NAME updated to the controller's felis build"
}

# The settings the panel saved come in through recall_host_settings.
main_reconfigure() {
  [ -e "$BOOTSTRAP_DONE" ] || die "reconfigure changes an installed host, and this one has no full install"
  export KUBECONFIG="$K3S_KUBECONFIG"
  if [ -z "${FELIS_DISTRIBUTED+x}" ] && [ -f "$K3S_CONFIG_DROPIN" ] && grep -q 'flannel-backend: "wireguard-native"' "$K3S_CONFIG_DROPIN"; then DISTRIBUTED=1; fi
  recall_distributed_settings
  recall_ha_settings
  local target
  local -a targets=()
  IFS="," read -ra targets <<<"${FELIS_RECONFIGURE:-}"
  [ "${#targets[@]}" -gt 0 ] || die "FELIS_RECONFIGURE names nothing to reconfigure"
  for target in "${targets[@]}"; do
    case "$target" in
      db-backup) install_db_backup_timer ;;
      velocity) install_velocity_service ;;
      offsite) configure_offsite; install_offsite_timer ;;
      *) die "FELIS_RECONFIGURE: unknown target '${target}' (db-backup, velocity or offsite)" ;;
    esac
  done
  ok "reconfigured: ${FELIS_RECONFIGURE}"
}

main() {
  acquire_run_lock
  ensure_k3s_on_path
  resolve_nano_listen
  validate_settings
  detect_os
  prompt_install_mode
  if [ "$INSTALL_MODE" = "nano" ]; then
    main_nano
    return
  fi
  if [ "$INSTALL_MODE" = reconfigure ]; then main_reconfigure; return; fi
  if [ "$INSTALL_MODE" = worker ]; then main_worker; return; fi
  if [ "$INSTALL_MODE" = worker-upgrade ]; then main_worker_upgrade; return; fi
  if [ "$INSTALL_MODE" = controller-join ]; then main_controller_join; return; fi
  detect_node_ip
  # Before the first change to the host: a problem found here costs a rerun, one found
  # halfway through costs an install to unwind.
  preflight
  check_postgres_major
  if [ "$FELIS_UPGRADE_DEPS" = 1 ] && [ -x "$K3S_BIN" ]; then
    k3s_upgrade_allowed "$("$K3S_BIN" --version 2>/dev/null | awk 'NR == 1 { print $3 }')" "$FELIS_K3S_VERSION" || true
  fi
  quiet_watchdog
  pause_package_background_timers
  ensure_swap
  install_base
  ensure_time_sync
  ensure_persistent_journal
  # Resolve releases early except when staged, explicit artifact, or running-binary inputs already determine the version.
  bootstrap_from_tui || [ -n "${FELIS_SKIP_FETCH:-}" ] || [ -n "$FELIS_ARTIFACT_DIR" ] || resolve_install_ref
  install_cloudflared
  load_or_make_secrets
  configure_offsite
  ensure_panel_tls_cert
  # No install_docker here: Docker comes in only for an image this run has to build
  # (ensure_docker), and an install from a release's assets builds none.
  if [ -z "${FELIS_DISTRIBUTED+x}" ] && [ -f "$K3S_CONFIG_DROPIN" ] && grep -q 'flannel-backend: "wireguard-native"' "$K3S_CONFIG_DROPIN"; then DISTRIBUTED=1; fi
  recall_distributed_settings
  recall_ha_settings
  write_ha_env
  install_k3s
  # The registry mirror must exist before the bundle's pods start pulling (and
  # before any re-run's rollouts).
  configure_registry_mirror
  acquire_felis_binary
  select_release_artifacts
  resolve_felis_image
  # The registry's and the database's own images must be in containerd before their
  # Deployments can start at all: from the release's bundle when it has them, else pulled.
  import_release_images felis registry postgres
  import_platform_images
  build_image
  # Source builds install the new HOST_BIN here; an earlier call may execute the
  # old release's binary, which has no distributed node commands.
  if [ "${DISTRIBUTED:-0}" = 1 ]; then
    [ -n "$WORKER_PEERS" ] || WORKER_PEERS="$(cluster_node_peers || true)"
    [ -n "$WORKER_PEERS" ] || WORKER_PEERS="${NODE_EXTERNAL_IP:-$NODE_IP}/32"
    # With HA every controller reaches this one's etcd, supervisor and kubelet.
    local ha_peers=""
    if [ "${CONTROLLER_HA:-0}" = 1 ]; then
      ha_peers="$(controller_cidrs || true)"
      [ -n "$ha_peers" ] || ha_peers="${NODE_EXTERNAL_IP:-$NODE_IP}/32"
    fi
    "$HOST_BIN" node firewall --controller --controller-ip "${NODE_EXTERNAL_IP:-$NODE_IP}" --peers "$WORKER_PEERS" ${ha_peers:+--controller-peers "$ha_peers"} --pod-cidr "$POD_CIDR" --node-port "$FELIS_PANEL_NODEPORT" --control-namespace "$CONTROL_NS" --namespace "$MINECRAFT_NS"
    configure_ha_host_firewall "$ha_peers"
  fi
  # After build_image imported the felis image: the registry pod's gate runs it.
  pin_platform_images
  # Before build_game_stack: the builds user servers run must be read off the
  # registry's tags before new ones replace them.
  pin_user_server_images
  build_game_stack
  # HA: Longhorn first, then the control plane's volumes onto it, before the database
  # starts on its replicated one.
  install_longhorn
  ha_migrate_storage
  deploy_postgres
  # Before run_migrations writes the host config that points at felis-postgres: until the
  # move, the database is the host PostgreSQL an earlier release installed.
  migrate_host_postgres
  run_migrations
  install_node_control_service
  deploy_bundle
  # After deploy_bundle: the registry the built images are mirrored into is part
  # of that bundle.
  push_images_to_registry
  # After deploy_bundle, like the pushes: it writes into the registry.
  install_build_tools_timer
  restart_existing_system_servers
  # After deploy_bundle: the proxy dials felis-api's internal ClusterIP, which does not
  # exist until the bundle is applied.
  install_velocity
  install_keepalived
  # After deploy_bundle: the bundle's MinecraftServer export reads the cluster.
  install_db_backup_timer
  # After the backup timer: its first bundle is part of the first copy.
  install_offsite_timer
  # After install_velocity: the check reads the installed proxy jar's version.
  install_update_check_timer
  # Last: its first run should see the platform as this install leaves it.
  install_watchdog_timer
  mark_bootstrap_done
  restore_from_offsite
  summary
  start_setup_console
}

main "$@"
