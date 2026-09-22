#!/usr/bin/env bash
#
# Felis one-line bootstrap installer.
#
#   curl -fsSL <raw-url>/deploy/bootstrap.sh | sudo bash
#
# Brings a fresh single-node Linux host from nothing to a running Felis control
# plane: it installs whatever is missing (picking apt/dnf/yum/zypper/pacman by OS), provisions a
# swap file on tiny hosts, then configures Docker, k3s and PostgreSQL, builds and
# imports the felis image, runs database migrations and applies the rendered
# install bundle (CRD + namespaces + RBAC + NetworkPolicies + control-plane
# Deployments + in-cluster registry).
#
# The recommended entrypoint is now `sudo felis setup`, which wraps this
# bootstrap in a TUI and then continues to the Owner/edge setup. This script
# remains usable directly for raw host provisioning.
#
# At the start it asks what to install:
#   [1] Felis       — the full control plane described above.
#   [2] Felis-nano  — ONLY the Yggdrasil hasJoined multiplexer (`felis nano`) as a
#                     systemd service: no k3s, no Postgres, no bundle. For a
#                     third-party server operator who just wants multi-Yggdrasil
#                     auth federation. Preselect non-interactively with
#                     FELIS_INSTALL_MODE=nano.
#
# The script is idempotent: re-running it converges rather than duplicating, and
# generated secrets are persisted to /etc/felis/secrets.env so reruns reuse them.
#
# Tunables (export before running to override the demo defaults):
#   FELIS_INSTALL_MODE full|nano — skip the prompt (default: ask on a tty, else full)
#   FELIS_NANO_LISTEN listen addr for `felis nano` (default: 127.0.0.1:8081 — loopback
#                     only; set a private-network IP to serve an off-host proxy)
#   FELIS_LEGACY_FORWARDING_SERVERS comma-separated backends that receive their identity
#                     through the handshake address instead of modern forwarding
#                     (default: legacy18). Read once at Velocity start, so changing it
#                     means re-running this script and restarting the proxy.
#   FELIS_VELOCITY_FORK_JAR path to a Felis-Legacy Velocity fork build to install as the
#                     proxy instead of the stock download (default: unset, stock).
#   FELIS_VELOCITY_FORK_JAR_SHA256 expected sha256 of that jar. REQUIRED whenever the jar
#                     above is set; the install refuses on a mismatch.
#   FELIS_GO_VERSION  Go toolchain used to build the nano binary (default: 1.26.4)
#   FELIS_REPO_URL    git URL to build from   (raw script mode only)
#   FELIS_VERSION_BOOTSTRAP release|dev — which version to install (default: release).
#                     release DOWNLOADS the prebuilt felis binary published for the newest
#                     tag (panel included — it is go:embed'ed into that same binary) and
#                     builds only a thin image around it; dev clones and compiles. If the
#                     asset is missing or this architecture has none, release warns and falls
#                     back to compiling the SAME tag. The game stack is always built here.
#   FELIS_GITHUB_TOKEN GitHub token; REQUIRED while the repo is private
#   FELIS_REF         branch/tag/sha — pins the build, overrides the channel, and forces a
#                     source build (naming a ref asks for that tree, not a published asset)
#   FELIS_IMAGE       local image tag         (default: felis:demo  — never :latest)
#   FELIS_ROOT_DOMAIN deployment root domain  (default: <node-ip>.nip.io)
#   FELIS_PANEL_NODEPORT local HTTPS panel/API NodePort (default: 30443)
#   FELIS_EGRESS_MODE loadbalancer|nodeport   (default: nodeport — no MetalLB on a demo box)
#   PKG_LOCK_TIMEOUT seconds to wait for package-manager locks (default: 900)
#   APT_LOCK_TIMEOUT legacy alias for PKG_LOCK_TIMEOUT
set -Eeuo pipefail

# ---------------------------------------------------------------------------
# Configuration & constants
# ---------------------------------------------------------------------------
FELIS_REPO_URL="${FELIS_REPO_URL:-https://github.com/MliroLirrorsIngenuity/Felis.git}"
# Which version to install. "release" builds the newest published GitHub release;
# "dev" builds the tip of main. Release is the default because an installer that
# tracks a moving branch by default hands every new host a different, untested
# commit — the version an operator reports in a bug is then meaningless.
FELIS_VERSION_BOOTSTRAP="${FELIS_VERSION_BOOTSTRAP:-release}"
# Empty by default and resolved from the channel below. Setting it explicitly pins the
# build to that ref and skips resolution entirely: naming a ref IS asking for a
# development build, so it takes the dev-FORM stamp regardless of the channel. Skipping
# resolution also skips the tag lookup, so the base stays v0.0.0 and the stamp is
# v0.0.0+g<sha> rather than <latest-tag>+g<sha> — deliberate, so pinning a ref costs no
# network call the operator did not ask for.
FELIS_REF="${FELIS_REF:-}"
# Recorded HERE because resolve_install_ref overwrites FELIS_REF on both channels — after it
# runs, "did the operator pin a ref?" is unanswerable. Naming a ref asks for THAT tree to be
# built, so it takes the source path even on the release channel.
FELIS_REF_PINNED=""
if [ -n "$FELIS_REF" ]; then FELIS_REF_PINNED=1; fi
# Set once a prebuilt felis binary is installed at HOST_BIN, by either the TUI hand-off or a
# release download. It is what the image build, the CRD apply and the game stack key off:
# all three only need "is there a binary and no checkout", never "which route got us here".
HAVE_PREBUILT_BINARY=""
# Optional GitHub credential, needed while this repository is private: GitHub answers
# 404 (not 403) for a repo the caller cannot see, so without it both the release lookup
# and the clone fail as "not found". Exported because git's credential helper below runs
# as a child process and reads it from the environment — which is also why it is never
# interpolated into the clone URL. A token in the URL is written verbatim into
# .git/config and survives the install; an environment variable does not.
FELIS_GITHUB_TOKEN="${FELIS_GITHUB_TOKEN:-}"
export FELIS_GITHUB_TOKEN
# Set by resolve_install_ref/stamp_version and linked into the binary as main.version.
FELIS_VERSION=""
FELIS_VERSION_BASE=""
FELIS_IMAGE="${FELIS_IMAGE:-felis:demo}"
FELIS_EGRESS_MODE="${FELIS_EGRESS_MODE:-nodeport}"
FELIS_PANEL_NODEPORT="${FELIS_PANEL_NODEPORT:-30443}"
INSTALL_MODE="${FELIS_INSTALL_MODE:-}"
# Loopback by default: hasJoined is an unauthenticated endpoint by protocol (authlib
# sends no token), so a public bind is a free auth relay — anyone can point their own
# proxy at it and spend YOUR egress IP on Mojang, until Mojang rate-limits you and your
# own players stop getting in. Same-host Velocity reaches 127.0.0.1 fine; a proxy on
# another machine must opt in explicitly with FELIS_NANO_LISTEN=<private-ip>:8081.
FELIS_NANO_LISTEN="${FELIS_NANO_LISTEN:-127.0.0.1:8081}"
# Backends that take their forwarded identity through the handshake address instead of
# proxy-wide modern forwarding. See write_velocity_service for why a protocol-47 backend
# needs this. Overridable because adding a second 1.8 backend otherwise means editing this
# script; it is still a restart-time list, not one that follows the CRs.
FELIS_LEGACY_FORWARDING_SERVERS="${FELIS_LEGACY_FORWARDING_SERVERS:-legacy18}"
FELIS_GO_VERSION="${FELIS_GO_VERSION:-1.26.4}"
PKG_LOCK_TIMEOUT="${PKG_LOCK_TIMEOUT:-${APT_LOCK_TIMEOUT:-900}}"
APT_LOCK_TIMEOUT="${APT_LOCK_TIMEOUT:-$PKG_LOCK_TIMEOUT}"

# --- the game stack: proxy on the host, the two always-on backends in k3s ---
FELIS_LIMBO_IMAGE="${FELIS_LIMBO_IMAGE:-felis-limbo:demo}"
FELIS_LOBBY_IMAGE="${FELIS_LOBBY_IMAGE:-felis-lobby:demo}"
# Plain Paper base recommended for a user's own server (deploy/paper). Not a system
# server — forwarding is applied by the operator's init-forwarding initContainer, so it
# needs no secret. Seeded recommended in 0019_recommended_paper.sql.
FELIS_PAPER_IMAGE="${FELIS_PAPER_IMAGE:-felis-paper:demo}"
# The Velocity MINOR is pinned, not discovered. PaperMC's Fill v3 groups velocity
# builds by version group, and "newest across all groups" today means 4.0.0-SNAPSHOT —
# an UNRELEASED proxy (the 4.0.0 group has zero published builds) that needs a Java 25
# runtime. Crossing a major is a deliberate code change, so we track the newest BUILD of
# a pinned minor and let a human move the pin.
FELIS_VELOCITY_VERSION="${FELIS_VELOCITY_VERSION:-3.5.1}"
# Path to a Felis-Legacy Velocity fork build, installed as the proxy in place of the
# stock download. Unset — the default — changes nothing.
#
# Stock Velocity will not offer the login-plugin-message exchange below 1.13, so a 1.8
# client reaching a modern-forwarding backend today is a side effect of Via replacing
# the channel initializers before that check runs. It works, and nobody designed it.
# The fork registers the login packets on 1.7.2 and drops the gate, which makes the
# same outcome deliberate.
#
# Opt-in because it is unmeasured where it counts: FL-008's probe runs offline-mode
# against a stub, and this jar would carry every real Mojang session on the server.
FELIS_VELOCITY_FORK_JAR="${FELIS_VELOCITY_FORK_JAR:-}"
# Expected sha256 of that jar, REQUIRED whenever it is set. Case and internal spaces are
# ignored, so whatever sha256sum, Get-FileHash or certutil printed can be pasted as-is.
# No digest is hardcoded here:
# the build lives in Felis-Legacy and has never been reproduced on a second machine, so
# any constant this script carried would pin one machine's output rather than the fork.
#
# So this is not a supply-chain signature and does not pretend to be one — an operator
# who can write the jar can write this value too. What it does buy: a path is not an
# identity, and every re-run of this script re-checks it. A truncated copy, a stale build
# left at the same path, or the two-patch jar where the three-patch one was meant all
# change the digest and stop the install. Naming the digest once is what turns "whatever
# is at that path today" into one specific build.
FELIS_VELOCITY_FORK_JAR_SHA256="${FELIS_VELOCITY_FORK_JAR_SHA256:-}"
# Temurin 25: Velocity 3.5 needs 21+, and 25 is also what a future Velocity 4 requires,
# so the runtime does not have to move again when the pin does. Distro JDK packaging is
# a lottery across four package managers — a tarball is one code path everywhere (same
# reasoning as install_go_toolchain).
FELIS_JRE_VERSION="${FELIS_JRE_VERSION:-25}"
# The port the proxy listens on: the ONLY Minecraft port players ever touch. Backends
# are ClusterIP-only, verify the modern-forwarding HMAC, and use NetworkPolicy to limit
# non-node ingress to the declared proxy CIDRs.
FELIS_GAME_PORT="${FELIS_GAME_PORT:-25565}"
# Velocity server names — must match internal/naming (SystemLoginServer/SystemLobbyServer);
# felis-api hands the proxy backends under exactly these names.
LOGIN_SERVER="login"
LOBBY_SERVER="lobby"

CONTROL_NS="felis"
MINECRAFT_NS="minecraft"
BUILD_NS="felis-build"
POD_CIDR="10.42.0.0/16"          # k3s default cluster CIDR
SERVICE_CIDR="10.43.0.0/16"      # k3s default service CIDR
DB_NAME="felis"
DB_USER="felis"
REGISTRY_URL="registry.felis.svc:5000"

STATE_DIR="/etc/felis"
SECRETS_ENV="${STATE_DIR}/secrets.env"
BOOTSTRAP_DONE="${STATE_DIR}/bootstrap.done"
PANEL_TLS_CERT="${STATE_DIR}/panel-tls.crt"
PANEL_TLS_KEY="${STATE_DIR}/panel-tls.key"
SRC_DIR="/opt/felis/src"
HOST_BIN="/usr/local/bin/felis"
GOROOT_DIR="/usr/local/go"
NANO_SERVICE="/etc/systemd/system/felis-nano.service"
VELOCITY_DIR="/opt/felis/velocity"
VELOCITY_USER="felis-velocity"
VELOCITY_SERVICE="/etc/systemd/system/felis-velocity.service"
JRE_DIR="/opt/felis/jre"
K3S_BIN_DIR="${K3S_BIN_DIR:-/usr/local/bin}"
K3S_BIN="${K3S_BIN_DIR}/k3s"
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

# ---------------------------------------------------------------------------
# Clean PATH (sudo may strip /usr/local/bin)
# ---------------------------------------------------------------------------
PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
export PATH

# ---------------------------------------------------------------------------
# Logging
# ---------------------------------------------------------------------------
log()  { printf '\033[1;36m[felis]\033[0m %s\n' "$*"; }
ok()   { printf '\033[1;32m[ ok ]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[warn]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[fail]\033[0m %s\n' "$*" >&2; exit 1; }

TEMP_PATHS=()
DOCKER_CONTAINERS=()
PKG_TIMERS_TO_RESTORE=()

on_error() {
  local line="$1" code="$2"
  warn "bootstrap failed near line ${line} (exit ${code})"
}

cleanup() {
  local id path unit
  for unit in "${PKG_TIMERS_TO_RESTORE[@]-}"; do
    [ -n "$unit" ] || continue
    systemctl start "$unit" >/dev/null 2>&1 || true
  done
  if command -v docker >/dev/null 2>&1; then
    for id in "${DOCKER_CONTAINERS[@]-}"; do
      [ -n "$id" ] && docker rm "$id" >/dev/null 2>&1 || true
    done
  fi
  for path in "${TEMP_PATHS[@]-}"; do
    [ -n "$path" ] && rm -rf -- "$path" || true
  done
}

remember_temp() { TEMP_PATHS+=("$1"); }
remember_container() { DOCKER_CONTAINERS+=("$1"); }

trap 'on_error "$LINENO" "$?"' ERR
trap cleanup EXIT

k3s_cmd() { [ -x "$K3S_BIN" ] || die "k3s binary not found at ${K3S_BIN}"; "$K3S_BIN" "$@"; }
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

apt_get() {
  wait_for_pkg_locks
  DEBIAN_FRONTEND=noninteractive apt-get \
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

validate_settings() {
  validate_timeout PKG_LOCK_TIMEOUT "$PKG_LOCK_TIMEOUT"
  validate_timeout APT_LOCK_TIMEOUT "$APT_LOCK_TIMEOUT"
  validate_nodeport FELIS_PANEL_NODEPORT "$FELIS_PANEL_NODEPORT"
}

# ---------------------------------------------------------------------------
# 0. Privilege & host facts
# ---------------------------------------------------------------------------
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

# persisted_root_domain echoes the root_domain an earlier run wrote, or nothing. The
# generated toml is the only durable record of it: nothing else on the host stores the
# domain, and it is written on every successful install.
persisted_root_domain() {
  local f="${STATE_DIR}/felis.host.toml"
  [ -r "$f" ] || return 0
  awk -F'"' '/^[[:space:]]*root_domain[[:space:]]*=/ { print $2; exit }' "$f"
}

detect_node_ip() {
  NODE_IP="$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="src"){print $(i+1); exit}}')"
  [ -n "${NODE_IP:-}" ] || NODE_IP="$(hostname -I 2>/dev/null | awk '{print $1}')"
  [ -n "${NODE_IP:-}" ] || die "could not determine this host's primary IPv4 address"

  # Precedence: an explicit FELIS_ROOT_DOMAIN, then whatever the last run persisted, then
  # the nip.io default. The middle step is what makes a re-run idempotent. Without it this
  # installer re-derived the domain from scratch every time and defaulted to nip.io, so
  # re-running it on a live install -- the only way to move felis-api to a newer release,
  # and what `felis update` points operators at -- rewrote root_domain, panel_hostname and
  # admin_hostname to nip.io names. ensure_panel_tls_cert is write-once and kept serving a
  # certificate for the OLD hostnames, so the console stopped matching its own cert, with
  # no re-domain flow to recover through. Secrets never had this problem:
  # load_or_make_secrets has always sourced secrets.env before generating anything.
  local persisted
  persisted="$(persisted_root_domain)"
  if [ -n "${FELIS_ROOT_DOMAIN:-}" ] && [ -n "$persisted" ] && [ "$FELIS_ROOT_DOMAIN" != "$persisted" ]; then
    # Deliberate re-domain. Allowed -- there is no other route to it -- but it is not a
    # thing this script finishes: the panel certificate, the two secrets, the velocity
    # config and the login CR all still carry the old name.
    warn "FELIS_ROOT_DOMAIN (${FELIS_ROOT_DOMAIN}) differs from the installed ${persisted}."
    warn "This re-domains the install. The write-once panel certificate is NOT reissued and"
    warn "will keep the old hostnames; the proxy and login config need the same treatment."
  fi
  FELIS_ROOT_DOMAIN="${FELIS_ROOT_DOMAIN:-${persisted:-${NODE_IP}.nip.io}}"
  if [ -n "$persisted" ] && [ "$FELIS_ROOT_DOMAIN" = "$persisted" ]; then
    log "node IP: ${NODE_IP}   root domain: ${FELIS_ROOT_DOMAIN} (reusing the installed domain)"
  else
    log "node IP: ${NODE_IP}   root domain: ${FELIS_ROOT_DOMAIN}"
  fi
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

pkg_refresh_once() {
  [ -n "${_PKG_REFRESHED:-}" ] && return 0
  case "$PKG" in
    apt) apt_get update -y ;;
    dnf|yum) : ;;   # dnf/yum refresh metadata on demand
    zypper) wait_for_pkg_locks; zypper --non-interactive refresh ;;
    pacman) wait_for_pkg_locks; pacman -Syu --noconfirm ;;
  esac
  _PKG_REFRESHED=1
}

# ---------------------------------------------------------------------------
# 1. Swap — k3s + Postgres + a Go build will OOM on a <2 GiB box without it
# ---------------------------------------------------------------------------
ensure_swap() {
  local mem_kb swap_kb
  mem_kb="$(awk '/^MemTotal:/{print $2}' /proc/meminfo)"
  swap_kb="$(awk '/^SwapTotal:/{print $2}' /proc/meminfo)"
  if [ "${swap_kb:-0}" -gt 0 ]; then
    ok "swap already present ($((swap_kb/1024)) MiB)"
    return 0
  fi
  if [ "${mem_kb:-0}" -ge 2097152 ]; then
    ok "RAM $((mem_kb/1024)) MiB is sufficient; skipping swap"
    return 0
  fi
  log "low RAM ($((mem_kb/1024)) MiB) and no swap — creating a 2 GiB swap file"
  if ! fallocate -l 2G /swapfile 2>/dev/null; then
    dd if=/dev/zero of=/swapfile bs=1M count=2048 status=none
  fi
  chmod 600 /swapfile
  mkswap /swapfile >/dev/null
  swapon /swapfile
  grep -q '^/swapfile ' /etc/fstab || echo '/swapfile none swap sw 0 0' >> /etc/fstab
  ok "2 GiB swap active"
}

# ---------------------------------------------------------------------------
# 2. Base packages
# ---------------------------------------------------------------------------
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
  if command -v cloudflared >/dev/null 2>&1; then
    ok "cloudflared already installed"
    return 0
  fi
  local machine arch url tmp
  machine="$(uname -m)"
  case "$machine" in
    x86_64|amd64) arch="amd64" ;;
    aarch64|arm64) arch="arm64" ;;
    armv7l|armv6l) arch="arm" ;;
    *) die "unsupported architecture for cloudflared: ${machine}" ;;
  esac
  url="https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-${arch}"
  tmp="$(mktemp)"
  remember_temp "$tmp"
  log "installing cloudflared (${arch})"
  curl -fsSL "$url" -o "$tmp"
  install -m 0755 "$tmp" /usr/local/bin/cloudflared
  rm -f "$tmp"
  ok "cloudflared installed ($(cloudflared --version | head -n 1))"
}

# ---------------------------------------------------------------------------
# 3. Docker (used only to build & export the felis image; k3s uses containerd)
# ---------------------------------------------------------------------------
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
  curl -fsSL "https://download.docker.com/linux/${repo_os}/gpg" -o "$keyring" \
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
  curl -fsSL "$repo_url" -o "$repo_file" \
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

install_docker() {
  if command -v docker >/dev/null 2>&1; then
    ok "docker already installed"
  else
    case "$PKG" in
      apt) install_docker_apt ;;
      dnf|yum) install_docker_rpm ;;
      zypper) install_docker_zypper ;;
      pacman) install_docker_pacman ;;
      *) die "Docker installation is not supported with package manager: ${PKG}" ;;
    esac
  fi
  systemctl enable --now docker
  ok "docker running"
}

# ---------------------------------------------------------------------------
# 4. k3s — single node, trimmed for RAM. NetworkPolicy stays ENABLED on purpose:
#    Felis's minecraft fence (default-deny + allow-rcon/allow-game) is a core
#    security claim, so we must NOT pass --disable-network-policy.
#    On SUSE-family hosts firewalld ships active by default; open the required
#    rules rather than disabling the firewall.
# ---------------------------------------------------------------------------
configure_k3s_firewall() {
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

  if [ -x "$K3S_BIN" ]; then
    ok "k3s already installed at ${K3S_BIN}"
  else
    log "installing k3s into ${K3S_BIN_DIR} (no traefik/servicelb/metrics-server)"
    curl -sfL https://get.k3s.io | \
      INSTALL_K3S_BIN_DIR="$K3S_BIN_DIR" \
      INSTALL_K3S_EXEC="--disable traefik --disable servicelb --disable metrics-server --write-kubeconfig-mode 644" \
      sh -
  fi

  [ -x "$K3S_BIN" ] || die "k3s installation completed but ${K3S_BIN} is missing"

  systemctl enable --now k3s
  export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
  log "waiting for the node to become Ready"
  local i
  for i in $(seq 1 60); do
    if kube get nodes 2>/dev/null | grep -q ' Ready '; then
      ok "k3s node Ready"
      return 0
    fi
    sleep 5
  done
  kube get nodes || true
  die "k3s node did not become Ready in time"
}

# ---------------------------------------------------------------------------
# 5. Source/binary + image build + containerd import
# ---------------------------------------------------------------------------
# repo_slug prints the "owner/name" of FELIS_REPO_URL, for the REST API.
repo_slug() {
  printf '%s\n' "$FELIS_REPO_URL" | sed -e 's#^.*github\.com[:/]##' -e 's#\.git$##'
}

# github_api GETs a REST path and prints the body.
#
# The token goes in through `curl --config -` rather than `-H "Authorization: ..."`
# because argv is world-readable via /proc while this runs. Same reason git_auth uses a
# credential helper instead of a URL: a bootstrap that leaks its own credential to any
# local user has not really installed anything privately.
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

# github_latest_tag prints the tag of the newest published stable release, or fails.
# Same endpoint internal/updater/github.go polls, so `felis update` and the installer
# can never disagree about what "latest" means.
github_latest_tag() {
  local json tag
  # Fetch first, filter second — the SIGPIPE reason documented on resolve_game_jars.
  json="$(github_api "repos/$(repo_slug)/releases/latest")" || return 1
  tag="$(printf '%s' "$json" | grep -o '"tag_name"[[:space:]]*:[[:space:]]*"[^"]*"' || true)"
  tag="${tag%%$'\n'*}"
  tag="${tag#*:}"        #  "v1.2.3"
  tag="${tag#*\"}"       # v1.2.3"
  tag="${tag%\"}"        # v1.2.3
  [ -n "$tag" ] || return 1
  printf '%s\n' "$tag"
}

# felis_asset_arch prints the release-asset suffix for this host, or fails. Unlike the
# cloudflared/JRE/Go mappers just below, this one does NOT die on an unmapped architecture:
# those have no local alternative, whereas a missing prebuilt binary only means we compile
# the same tag on this host — slower, byte-for-byte the same result.
felis_asset_arch() {
  case "$(uname -m)" in
    x86_64|amd64) printf 'amd64\n' ;;
    aarch64|arm64) printf 'arm64\n' ;;
    *) return 1 ;;
  esac
}

# github_asset_id prints the numeric id of the asset named $2 on release tag $1.
#
# Two details here are load-bearing and both are wrong in the obvious version. The newline
# flatten comes FIRST: api.github.com pretty-prints, so "name" and "url" land on different
# lines and splitting on "{" alone matches nothing at all. And the id is read out of the
# asset's own url, never from a bare "id" key — the payload carries a release id, an author
# id, one id per asset AND one per nested uploader, so matching "id" silently resolves the
# wrong file. Anchoring on the CLOSING quote keeps felis-linux-amd64 from also matching a
# sibling like felis-linux-amd64.sha256.
#
# The segment this greps ends at the nested "uploader" object, which is fine because "url"
# precedes it. Anything published after uploader (size, digest, browser_download_url) is NOT
# reachable this way — fetch releases/assets/<id> with the JSON Accept if that is ever needed.
github_asset_id() {
  local tag="$1" name="$2" json id
  # Fetch first, filter second — the SIGPIPE reason documented on resolve_game_jars.
  json="$(github_api "repos/$(repo_slug)/releases/tags/${tag}")" || return 1
  id="$(printf '%s' "$json" | tr -d '\n' | tr '{' '\n' \
    | grep "\"name\":[[:space:]]*\"${name}\"" \
    | grep -o 'releases/assets/[0-9]\{1,\}' | head -1)" || true
  id="${id##*/}"
  [ -n "$id" ] || return 1
  printf '%s\n' "$id"
}

# download_release_asset fetches ONE asset of a published release into $3.
#
# It RETURNS non-zero rather than dying: every caller falls back to building the same tag
# from source, so a tag whose release workflow has not finished uploading yet — a real window,
# since that job runs vet, tests and a full image build first — still installs.
#
# This cannot ride the github_api helper above. That helper sends
# Accept: application/vnd.github+json, and with it this endpoint returns the asset's METADATA
# as JSON under HTTP 200: a "successful" download of a text blob that passes every check and
# only surfaces much later, inside docker build, as an exec format error.
#
# Plain -L, never --location-trusted. GitHub 302s to a DIFFERENT host whose signed URL carries
# its own credentials in the query string; curl drops Authorization across that hop, and the
# drop is REQUIRED — forwarding the token makes the storage backend reject the request with
# 400 while leaking the credential to a third host for nothing.
#
# -f is not cosmetic: without it curl writes GitHub's error JSON into the output file and
# still exits 0.
download_release_asset() {
  local tag="$1" name="$2" dest="$3" id ua url rc=0
  ua="felis-bootstrap (+${FELIS_REPO_URL})"
  id="$(github_asset_id "$tag" "$name")" || return 1
  url="https://api.github.com/repos/$(repo_slug)/releases/assets/${id}"
  log "downloading ${name} from release ${tag}"
  if [ -n "$FELIS_GITHUB_TOKEN" ]; then
    printf 'header = "Authorization: Bearer %s"\n' "$FELIS_GITHUB_TOKEN" \
      | curl -fsSL --retry 5 --retry-delay 2 --config - \
          -A "$ua" -H "Accept: application/octet-stream" -o "$dest" "$url" || rc=$?
  else
    curl -fsSL --retry 5 --retry-delay 2 \
      -A "$ua" -H "Accept: application/octet-stream" -o "$dest" "$url" || rc=$?
  fi
  # curl -f leaves a PARTIAL file behind when a transfer dies mid-stream, so a failed
  # download must not hand the caller something it could mistake for a complete one.
  if [ "$rc" -ne 0 ] || [ ! -s "$dest" ]; then
    rm -f "$dest"
    return 1
  fi
}

# use_release_binary reports whether this run should install a prebuilt binary instead of
# compiling one. Only the plain release channel qualifies: FELIS_SKIP_FETCH means "build
# exactly what I staged" and a pinned FELIS_REF means "build that tree", both of which are
# explicit requests for a source build, and the dev channel has no release to download.
use_release_binary() {
  [ -z "${FELIS_SKIP_FETCH:-}" ] || return 1
  [ -z "$FELIS_REF_PINNED" ] || return 1
  [ "$FELIS_VERSION_BOOTSTRAP" = "release" ]
}

# download_release_binary installs the prebuilt felis binary for $FELIS_REF onto the host.
# It returns non-zero to ask the caller to build that same tag from source instead; it never
# dies, because no reachable failure here is worth aborting an install over.
#
# One asset covers the panel too: internal/panel/panel.go go:embeds internal/panel/static, and
# release.yml builds through the repo Dockerfile so that tree holds the real npm output rather
# than the tracked placeholder. There is nothing else to fetch.
download_release_binary() {
  local arch asset tmp got
  if ! arch="$(felis_asset_arch)"; then
    warn "no prebuilt felis binary for architecture $(uname -m); building ${FELIS_REF} from source on this host instead"
    return 1
  fi
  asset="felis-linux-${arch}"

  # Convergence check, and the cheapest one available: no API call, no download, and it asks
  # the exact question that matters. Reruns are the common case for this installer.
  if [ -x "$HOST_BIN" ] && [ "$("$HOST_BIN" version 2>/dev/null | head -n 1)" = "felis ${FELIS_REF}" ]; then
    HAVE_PREBUILT_BINARY=1
    ok "host binary is already ${FELIS_REF}; skipping the download"
    return 0
  fi

  # Staged next to HOST_BIN rather than in TMPDIR, because the validation below EXECUTES it
  # and /tmp is mounted noexec on CIS-hardened images. There the exec dies 126, the check
  # reads it as a bad asset, and every such host silently falls back to the full on-host
  # compile this path exists to avoid. /usr/local/bin has to be exec for felis to run at
  # all, so validating there tests the binary instead of the mount — and it keeps the
  # private-repo artifact out of a world-readable 1777 directory on the way in.
  mkdir -p "$(dirname "$HOST_BIN")"
  tmp="$(mktemp "$(dirname "$HOST_BIN")/.felis-download.XXXXXX")"
  remember_temp "$tmp"
  if ! download_release_asset "$FELIS_REF" "$asset" "$tmp"; then
    rm -f "$tmp"
    # Deliberately NOT "set FELIS_VERSION_BOOTSTRAP=dev": that channel builds main, a
    # different commit than the tag the operator asked for. Falling back keeps the tag and
    # changes only how it is obtained, so no operator action is needed at all.
    warn "release ${FELIS_REF} publishes no usable ${asset}; building ${FELIS_REF} from source on this host instead"
    return 1
  fi
  chmod 0755 "$tmp"

  # Run it once. This single exec subsumes three checks that would otherwise each need their
  # own code: a truncated download segfaults (curl reports success for an EOF-delimited body,
  # so nothing earlier catches it), a wrong-architecture asset fails to exec, and an unstamped
  # or mis-stamped build reports the wrong string here — rather than silently disabling
  # `felis update` for the entire life of the install.
  got="$("$tmp" version 2>/dev/null | head -n 1 || true)"
  if [ "$got" != "felis ${FELIS_REF}" ]; then
    rm -f "$tmp"
    warn "downloaded ${asset} reports '${got:-nothing}' rather than 'felis ${FELIS_REF}'; building ${FELIS_REF} from source on this host instead"
    return 1
  fi

  # install(1) onto a freshly created destination, matching install_embedded_binary. A rename
  # would carry the source SELinux label instead of type-transitioning to bin_t — see
  # build_nano_binary for the 203/EXEC this shape avoids. Same-directory staging does not
  # change that: install(1) still creates the destination and copies.
  rm -f "$HOST_BIN"
  install -m 0755 "$tmp" "$HOST_BIN"
  rm -f "$tmp"
  command -v restorecon >/dev/null 2>&1 && restorecon "$HOST_BIN" >/dev/null 2>&1 || true

  HAVE_PREBUILT_BINARY=1
  ok "installed ${asset} ${FELIS_REF} at ${HOST_BIN}"
}

# git_auth runs git with the token supplied by an inline credential helper. The helper
# is a shell snippet that READS the exported variable when git asks; the token is never
# in argv and never reaches .git/config, so it does not outlive the process.
#
# The EMPTY credential.helper in front is load-bearing, not a typo. credential.helper is a
# MULTI-valued config: a bare `-c credential.helper=...` APPENDS to whatever the host has
# configured, it does not replace it. On a host with `credential.helper=store` git then runs
# both — ours answers the prompt, and store writes the token to ~/.git-credentials on the
# approve that follows a successful auth, so it outlives the install after all. An empty
# value is git's documented list reset. Verified on Fedora: with store configured the single
# -c form persists the token to disk, the reset form writes nothing.
git_auth() {
  if [ -n "$FELIS_GITHUB_TOKEN" ]; then
    git -c 'credential.helper=' \
        -c 'credential.helper=!f() { printf "username=x-access-token\npassword=%s\n" "$FELIS_GITHUB_TOKEN"; }; f' "$@"
  else
    git "$@"
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
      log "resolving the newest published Felis release"
      FELIS_REF="$(github_latest_tag)" || die "could not resolve the newest Felis release.
  If the repository is private, set FELIS_GITHUB_TOKEN to a token with read access to it.
  If no release has been published yet, set FELIS_VERSION_BOOTSTRAP=dev to build main instead."
      # A release IS its tag, so the stamp is final here and stamp_version leaves it be.
      FELIS_VERSION="$FELIS_REF"
      ok "release channel: ${FELIS_REF}"
      ;;
    dev)
      FELIS_REF="main"
      # Best effort: the tag only names what this build is AHEAD of, and a repo with no
      # release yet is exactly when someone reaches for the dev channel. v0.0.0 keeps the
      # stamp parseable so `felis update` still compares rather than refusing.
      FELIS_VERSION_BASE="$(github_latest_tag 2>/dev/null || true)"
      [ -n "$FELIS_VERSION_BASE" ] || FELIS_VERSION_BASE="v0.0.0"
      ok "dev channel: main (ahead of ${FELIS_VERSION_BASE})"
      ;;
    *)
      die "FELIS_VERSION_BOOTSTRAP must be 'release' or 'dev', got '${FELIS_VERSION_BOOTSTRAP}'"
      ;;
  esac
}

# stamp_version finalises the dev/pinned stamp once a checkout exists to read the SHA
# from. Release builds are already stamped and return untouched.
#
# The form is "<tag>+g<sha>", NOT `git describe`'s "<tag>-<n>-g<sha>", and the difference
# is load-bearing. updates.Parse reads a "-" tail as a PRERELEASE, which sorts BELOW the
# plain tag: a dev build 14 commits past v1.2.3 would compare as older than v1.2.3, and
# `felis update` would propose "upgrading" onto the release it already contains. After
# "+" the tail is build metadata, ignored for ordering, so the build reads as current
# against v1.2.3 and as behind against v1.3.0 — both correct.
#
# `git describe` is also unreliable here: the primary clone is --depth 1 and carries no
# tags, so describe falls back to a bare SHA, which updates.Parse rejects outright (it
# fails closed on a non-numeric core). fetch_source does retry with a full clone when the
# shallow one fails, which WOULD carry tags — that is exactly the point: the stamp must not
# depend on which arm happened to win. rev-parse needs no history at all.
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
  if [ -d "${SRC_DIR}/.git" ]; then
    log "updating source in ${SRC_DIR}"
    git_auth -C "$SRC_DIR" fetch --depth 1 origin "$FELIS_REF"
    git -C "$SRC_DIR" checkout -f FETCH_HEAD
  else
    log "cloning ${FELIS_REPO_URL} (${FELIS_REF})"
    mkdir -p "$(dirname "$SRC_DIR")"
    # The fallback checks the ref out explicitly. It used to be a bare full clone, which
    # silently landed on the default branch: harmless when FELIS_REF was always "main",
    # but the release channel now asks for a tag, and a build stamped v1.2.3 that
    # actually contains main is worse than a failed install.
    git_auth clone --depth 1 --branch "$FELIS_REF" "$FELIS_REPO_URL" "$SRC_DIR" 2>/dev/null \
      || { git_auth clone "$FELIS_REPO_URL" "$SRC_DIR" \
           && git_auth -C "$SRC_DIR" checkout -f "$FELIS_REF"; } \
      || die "could not check out ${FELIS_REF} from ${FELIS_REPO_URL}"
  fi
  stamp_version
  ok "source ready at ${SRC_DIR}"
}

install_embedded_binary() {
  local src
  src="${FELIS_BOOTSTRAP_BINARY:-}"
  [ -n "$src" ] || die "FELIS_BOOTSTRAP_BINARY is not set; cannot install the embedded setup binary"
  [ -x "$src" ] || die "FELIS_BOOTSTRAP_BINARY is not executable: ${src}"

  mkdir -p "$(dirname "$HOST_BIN")"
  if [ "$(readlink -f "$src")" != "$(readlink -f "$HOST_BIN" 2>/dev/null || true)" ]; then
    log "installing current felis binary onto the host (${HOST_BIN})"
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
  # static-debian12, matching the repo Dockerfile's final stage. Every binary that can reach
  # HOST_BIN traces back to that Dockerfile's CGO_ENABLED=0 build: the downloaded CI asset,
  # the binary the TUI is already running, and the one build_image_from_source docker-cp's
  # out of the image it just built. None of them link glibc, so the larger base-debian12
  # bought nothing and only widened the runtime surface. It mattered little while this was
  # the rare fallback; now that the release channel downloads a binary and wraps it here,
  # this is the image most installs actually run, and it should be the one CI publishes.
  cat > "${tmp}/Dockerfile" <<'EOF'
FROM gcr.io/distroless/static-debian12:nonroot
ENV PATH=/usr/local/bin:/usr/bin:/bin
COPY felis /usr/local/bin/felis
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/felis"]
EOF
  chmod 0755 "${tmp}/felis"

  log "building ${FELIS_IMAGE} from the current felis binary"
  docker build -t "$FELIS_IMAGE" "$tmp"
  rm -rf "$tmp"
}

verify_image_starts() {
  log "verifying ${FELIS_IMAGE} starts"
  docker run --rm --user 1000:1000 --entrypoint /usr/local/bin/felis "$FELIS_IMAGE" help >/dev/null
}

build_image_from_source() {
  log "building ${FELIS_IMAGE} ${FELIS_VERSION:+(${FELIS_VERSION}) }(this compiles the Go binary; first run is slow)"
  # Without the stamp main.version stays "dev", and `felis update` refuses to compare a
  # "dev" build against upstream rather than treating it as 0.0.0. So an unstamped image
  # is not a cosmetic problem: it silently disables update reporting for the install.
  docker build -t "$FELIS_IMAGE" \
    --build-arg FELIS_VERSION="${FELIS_VERSION:-dev}" "$SRC_DIR"

  log "extracting the felis binary onto the host (${HOST_BIN})"
  local cid
  cid="$(docker create "$FELIS_IMAGE")"
  remember_container "$cid"
  docker cp "${cid}:/usr/local/bin/felis" "$HOST_BIN"
  docker rm "$cid" >/dev/null
  chmod 0755 "$HOST_BIN"
}

build_image() {
  systemctl start docker
  # Keyed on the binary, not on the route that produced it: the TUI hand-off and a release
  # download both land on exactly the same state (a felis binary at HOST_BIN, no checkout),
  # and a release download that fell back to source has cleared this so the source build runs.
  if [ -n "$HAVE_PREBUILT_BINARY" ]; then
    build_image_from_binary
  else
    build_image_from_source
  fi
  verify_image_starts

  log "importing ${FELIS_IMAGE} into k3s containerd"
  remove_k3s_image "$FELIS_IMAGE"
  docker save "$FELIS_IMAGE" | k3s_cmd ctr images import -

  # Reclaim the ~150 MiB the docker daemon holds; reruns restart it on demand.
  systemctl stop docker docker.socket 2>/dev/null || true
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

# ---------------------------------------------------------------------------
# 5b. The game stack: the login limbo + lobby images, and the Velocity proxy.
#
#     Without this, `felis setup` asks the Owner to bind by joining Minecraft while
#     no Minecraft server exists — the whole point of installing is a joinable,
#     Mojang-authenticating server, so the installer produces one.
#
#     The trust chain, end to end:
#       player --(Mojang auth)--> Velocity --(modern forwarding, HMAC)--> login limbo
#     Velocity is the ONLY thing that talks to Mojang; the backends run offline-mode and
#     trust the forwarded profile, which is exactly why the forwarding secret is the
#     identity boundary. NetworkPolicy narrows reachability but cannot block the node
#     hosting a pod, so it is defense in depth rather than a substitute for the HMAC.
# ---------------------------------------------------------------------------

# game_stack_source sets GAME_STACK_DIR to a docker build context holding
# deploy/{limbo,lobby} and plugins/. The TUI path pipes this script in over stdin and
# has no checkout on disk, so there the sources come out of the felis binary itself.
#
# Keyed on HAVE_PREBUILT_BINARY, the same flag build_image uses, because the question is
# identical: a prebuilt binary means fetch_source never ran, so SRC_DIR is whatever an
# EARLIER install left behind. Trusting it there would build felis-paper and the Velocity
# plugin from the old commit while the control plane is the freshly downloaded release —
# a silent version skew across the plugin/API boundary. The embedded tar always matches
# the binary it came out of, so it is the correct source on every prebuilt path.
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

# resolve_game_jars pins Limbo and Paper to the SAME Minecraft version. LOOHP/Limbo
# speaks exactly one protocol per build, so the login gate dictates the version and Paper
# follows — a client that can pass the gate must also be able to reach the lobby.
# MC_VERSION is read off Limbo's CI artifact name (Limbo-<limbo-ver>-<mc-ver>.jar), which
# is the only place the pairing is published.
resolve_game_jars() {
  local ci="https://ci.loohpjames.com/job/Limbo/lastSuccessfulBuild" meta file base rest paper
  log "resolving the newest LOOHP/Limbo CI build"
  # Fetch first, filter second: `curl | grep | head` dies of SIGPIPE under `set -o pipefail`
  # the moment head closes the pipe early. Same shape everywhere below.
  meta="$(curl -fsSL --retry 5 --retry-delay 2 "${ci}/api/json")" \
    || die "could not read the LOOHP/Limbo CI build metadata"
  file="$(printf '%s' "$meta" | grep -o 'Limbo-[0-9A-Za-z._-]*\.jar' || true)"
  file="${file%%$'\n'*}"
  [ -n "$file" ] || die "no Limbo jar in the LOOHP/Limbo CI artifact list"

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
  ok "Limbo ${LIMBO_VERSION} + Paper, both on Minecraft ${MC_VERSION}; LuckPerms resolved"
}

# luckperms_latest_jar prints the download URL of the current LuckPerms Bukkit build.
# Bukkit, not bukkit-legacy: legacy targets Minecraft 1.8-1.12, and Paper 26.2 is far
# past that. The same fetch-then-grep shape (and --retry rationale) as
# papermc_latest_jar; the metadata endpoint hands back every platform's URL at once, so
# the grep has to pin the /bukkit/ path segment or it would just as happily return the
# Fabric or Velocity jar, neither of which Paper can load.
luckperms_latest_jar() {
  local json url
  json="$(curl -fsSL --retry 5 --retry-delay 2 \
    -A "felis-bootstrap (+https://github.com/MliroLirrorsIngenuity/Felis)" \
    "https://metadata.luckperms.net/data/all")" || return 1
  url="$(printf '%s' "$json" \
    | grep -o 'https://download\.luckperms\.net/[0-9]\{1,\}/bukkit/loader/[^"]*\.jar' || true)"
  url="${url%%$'\n'*}"
  [ -n "$url" ] || return 1
  printf '%s\n' "$url"
}

# papermc_latest_jar prints "<url> <sha256>" for the newest build of <project> <version>.
# --retry rides out Fill's transient gateway errors (502/503/504 are in curl's retry
# set): a single blip must not abort the whole bootstrap claiming the build is missing.
# Plain --retry only, deliberately: --retry-connrefused needs curl 7.52+, which the yum
# (el7) path does not have, and it would only add ECONNREFUSED to an already-covered set.
# The digest is not fished out of the JSON separately: Fill's download URLs are
# content-addressed (/v1/objects/<sha256>/<name>.jar), so the path segment names the
# bytes the URL serves and both halves come from the same grep of the same response. A
# URL without that shape fails the resolve rather than waving the download through
# unchecked.
papermc_latest_jar() {
  local project="$1" version="$2" json urls url sha
  json="$(curl -fsSL --retry 5 --retry-delay 2 \
    -A "felis-bootstrap (+https://github.com/MliroLirrorsIngenuity/Felis)" \
    "https://fill.papermc.io/v3/projects/${project}/versions/${version}/builds/latest")" || return 1
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
  systemctl start docker
  game_stack_source
  resolve_game_jars

  log "building ${FELIS_LIMBO_IMAGE} (LOOHP/Limbo ${LIMBO_VERSION}, Minecraft ${MC_VERSION})"
  docker build -f "${GAME_STACK_DIR}/deploy/limbo/Dockerfile" \
    --build-arg LIMBO_JAR_URL="$LIMBO_JAR_URL" \
    --build-arg LIMBO_SCHEM_URL="$LIMBO_SCHEM_URL" \
    --build-arg LIMBO_VERSION="$LIMBO_VERSION" \
    -t "$FELIS_LIMBO_IMAGE" "$GAME_STACK_DIR"

  log "building ${FELIS_LOBBY_IMAGE} (Paper ${MC_VERSION} + felis-paper /menu + LuckPerms)"
  docker build -f "${GAME_STACK_DIR}/deploy/lobby/Dockerfile" \
    --build-arg PAPER_JAR_URL="$PAPER_JAR_URL" \
    --build-arg PAPER_JAR_SHA256="$PAPER_JAR_SHA256" \
    --build-arg LUCKPERMS_JAR_URL="$LUCKPERMS_JAR_URL" \
    -t "$FELIS_LOBBY_IMAGE" "$GAME_STACK_DIR"

  # Plain Paper, same MC_VERSION and PAPER_JAR_URL (no new dependency). Forwarding is the
  # operator initContainer's job, so this image carries no /menu plugin and no secret gate.
  log "building ${FELIS_PAPER_IMAGE} (plain Paper ${MC_VERSION}, forwarding via the operator initContainer)"
  docker build -f "${GAME_STACK_DIR}/deploy/paper/Dockerfile" \
    --build-arg PAPER_JAR_URL="$PAPER_JAR_URL" \
    --build-arg PAPER_JAR_SHA256="$PAPER_JAR_SHA256" \
    -t "$FELIS_PAPER_IMAGE" "$GAME_STACK_DIR"

  local img
  for img in "$FELIS_LIMBO_IMAGE" "$FELIS_LOBBY_IMAGE" "$FELIS_PAPER_IMAGE"; do
    log "importing ${img} into k3s containerd"
    remove_k3s_image "$img"
    docker save "$img" | k3s_cmd ctr images import -
  done

  build_velocity_plugin
  systemctl stop docker docker.socket 2>/dev/null || true
  ok "login + lobby images imported; felis-velocity.jar staged"
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

# Velocity manages its own working directory: on startup it migrates velocity.toml to
# the running config-version, extracts localizations, and creates plugins/bStats — all
# fatal or noisy if it cannot write. So the service owns the tree and velocity.toml. The
# immutable artifacts (velocity.jar, felis-velocity.jar) and the forwarding secret stay
# root-owned and read-only to the proxy; ProtectSystem=strict confines writes to VELOCITY_DIR.
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

# build_velocity_plugin compiles plugins/velocity in the same gradle image the two
# Dockerfiles use, and drops the jar where Velocity will look for it. Docker is the
# toolchain here on purpose: the host needs no JDK and no gradle, only a JRE.
build_velocity_plugin() {
  log "building felis-velocity.jar (gradle in a container; the host gets no JDK)"
  prepare_velocity_layout
  # :z relabels the bind mount for SELinux (Fedora/EL enforce it; elsewhere it is a no-op).
  docker run --rm \
    -v "${GAME_STACK_DIR}:/src:z" \
    -w /src/plugins/velocity \
    gradle:8.14-jdk21 gradle --no-daemon clean build \
    || die "felis-velocity plugin build failed"
  local -a jars=( "${GAME_STACK_DIR}"/plugins/velocity/build/libs/felis-velocity-*.jar )
  [ "${#jars[@]}" -eq 1 ] && [ -f "${jars[0]}" ] \
    || die "felis-velocity build must produce exactly one plugin jar"
  atomic_install_file "${jars[0]}" "${VELOCITY_DIR}/plugins/felis-velocity.jar" 0644 root root
}

# install_via_plugins stages ViaVersion + ViaBackwards + ViaRewind so players on clients
# older than the proxy can still join.
#
# Modern forwarding nominally refuses anything below 1.13: HandshakeSessionHandler#handleLogin
# reads the handshake protocol version and disconnects with
# velocity.error.modern-forwarding-needs-new-client. That gate stops firing once Via is
# present — it logs "Replacing channel initializers" during startup, so the version reaching
# the check is plausibly already the rewritten one. The 1.13 floor is a property of the
# UNASSISTED proxy pipeline, not of the forwarding protocol, so nothing here changes
# player-info-forwarding-mode and no backend is patched or downgraded.
#
# Measured end to end rather than assumed (Felis-Legacy FL-007, cell modern-via121): a
# protocol-47 client joined a stock Paper 1.21.11 backend through a modern-forwarding proxy.
# The proof is the join itself, not the log line — that backend ran velocity.enabled=true with
# a shared secret, and Paper in that state rejects any login not carrying forwarding data
# signed with a matching HMAC. Only protocol 47 was measured; the rest of Via's 1.7-1.12 range
# is its own documented support.
#
# Pinned by hash and not by "latest" on purpose. These three jars sit in front of every packet
# on the proxy, and they are the exact bytes FL-007 measured — a moving tag would quietly make
# this an unmeasured configuration. Bumping a version means bumping its checksum here.
#
# The three versions are a set, not three independent pins. ViaRewind is the component that
# carries 1.8/1.7 support, and 4.1.2 against ViaVersion/ViaBackwards 5.11.0 fails to load
# Protocol1_9To1_8 — the single protocol every 1.8 client needs — with "Invalid version: 1"
# at proxy startup. 4.1.3 is the release that adds 5.11.0 compatibility; a two-arm run of the
# same proxy image logs that error three times on 4.1.2 and not at all on 4.1.3. Read the
# ViaRewind release notes before moving ViaVersion or ViaBackwards.
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
    curl -fsSL "$url" -o "$tmp" || die "failed to download ${name} ${version}: ${url}"
    have="$(sha256sum <"$tmp" | cut -d' ' -f1)"
    [ "$have" = "$want" ] \
      || die "${name} ${version} checksum mismatch: got ${have}, expected ${want}"
    atomic_install_file "$tmp" "$target" 0644 root root
  done <<'EOF'
ViaVersion 5.11.0 18d19e90fc9467d68128c076630ae8700449c901402a3ef421837ce006bc8cae
ViaBackwards 5.11.0 b21983d561e3f92df257683f0133ab6c68ec68175e8acfd82c6231723bf83587
ViaRewind 4.1.3 2d5970d22b4711c9ab2800932326c7b08acdace25ed7c6bbb8f6ea81054962b4
EOF
  pin_via_block_connections
  ok "Via staged; clients from 1.8 up can join under modern forwarding"
}

# pin_via_block_connections turns ViaVersion's serverside block-connection tracking off.
#
# ConnectionData.init() only builds its block-connection provider when Via's lowest supported
# protocol is below 1.13. Under modern forwarding the Velocity injector reports 393 (1.13), so
# init() returns early, blockConnectionProvider stays null, and the first 1.12.2->1.13 chunk
# rewrite dereferences it. A 1.8 client on a protocol-47 backend takes an NPE on the first chunk
# it is sent and never finishes joining. Every call site in protocols/v1_12_2to1_13 is behind
# isServersideBlockConnections(), so switching the option off skips all of them. The cost is
# cosmetic and pre-1.13 only: fences and glass panes stop being drawn connected.
#
# ViaVersion's default is true, so a fresh install ships that NPE unless it is corrected here.
# Seeding a file with this one key is enough: Config#loadConfig parses the bundled
# assets/viaversion/config.yml as the base map and merges the on-disk file over it, so every
# other option still comes from the shipped default and stays current across version bumps.
#
# Do not read the absence of "Loading block connection mappings" from the log as proof this
# worked. init() gates on the protocol version as well, and under modern forwarding that half
# fails on its own — the line is missing either way. The config value is the only evidence.
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
  local arch url
  if [ -x "${JRE_DIR}/bin/java" ]; then
    ok "JRE already installed at ${JRE_DIR}"
    return 0
  fi
  case "$(uname -m)" in
    x86_64|amd64) arch="x64" ;;
    aarch64|arm64) arch="aarch64" ;;
    *) die "no Temurin JRE build for architecture $(uname -m); pre-stage one at ${JRE_DIR}" ;;
  esac
  url="https://api.adoptium.net/v3/binary/latest/${FELIS_JRE_VERSION}/ga/linux/${arch}/jre/hotspot/normal/eclipse"

  log "installing Temurin ${FELIS_JRE_VERSION} JRE (${arch}) to ${JRE_DIR}"
  local tmp
  tmp="$(mktemp -d)"
  remember_temp "$tmp"
  curl -fsSL "$url" -o "${tmp}/jre.tar.gz" || die "failed to download the Temurin JRE: ${url}"
  mkdir -p "$JRE_DIR"
  # The tarball has a single versioned top-level directory (jdk-25+36-jre/); strip it so
  # the path in the systemd unit never carries a build number.
  tar -C "$JRE_DIR" --strip-components=1 -xzf "${tmp}/jre.tar.gz" || die "failed to unpack the JRE"
  [ -x "${JRE_DIR}/bin/java" ] || die "unpacked JRE has no bin/java"
  ok "JRE at ${JRE_DIR}/bin/java"
}

install_velocity() {
  install_jre
  local url tmp have want resolved
  prepare_velocity_layout
  if [ -n "$FELIS_VELOCITY_FORK_JAR" ]; then
    [ -f "$FELIS_VELOCITY_FORK_JAR" ] \
      || die "FELIS_VELOCITY_FORK_JAR is not a readable file: ${FELIS_VELOCITY_FORK_JAR}"
    # Hash stdin, never the path — same reason as install_via_plugins: sha256sum escapes its
    # output line for a filename carrying a backslash or a newline, and the leading "\" that
    # adds would fail every comparison below.
    have="$(sha256sum <"$FELIS_VELOCITY_FORK_JAR" | cut -d' ' -f1)"
    # Refuse rather than warn. This jar is the proxy every player connects through, and a
    # warning in an install log is not a gate. The digest is printed so the first run after
    # a deliberate rebuild is one copy-paste, not an investigation.
    [ -n "$FELIS_VELOCITY_FORK_JAR_SHA256" ] || die \
      "FELIS_VELOCITY_FORK_JAR_SHA256 is required whenever FELIS_VELOCITY_FORK_JAR is set.
   The jar at that path hashes to ${have}.
   Check that against the build you meant to install, then re-run with
   FELIS_VELOCITY_FORK_JAR_SHA256=${have}"
    # Normalise the operator's digest before comparing. sha256sum prints lowercase, but the
    # build host is often Windows, where Get-FileHash prints uppercase and certutil has
    # shipped both with and without spaces between the bytes. All three name the same jar,
    # so comparing raw would refuse two of the three spellings and word it as tampering.
    want="$(printf '%s' "$FELIS_VELOCITY_FORK_JAR_SHA256" | tr -d '[:space:]' | tr 'A-Z' 'a-z')"
    [ "$have" = "$want" ] || die \
      "FELIS_VELOCITY_FORK_JAR checksum mismatch: got ${have}, expected ${want}"
    log "installing the Felis-Legacy Velocity fork from ${FELIS_VELOCITY_FORK_JAR} (sha256 ${have})"
    atomic_install_file "$FELIS_VELOCITY_FORK_JAR" "${VELOCITY_DIR}/velocity.jar" 0644 root root
  else
    log "resolving the newest Velocity ${FELIS_VELOCITY_VERSION} build"
    resolved="$(papermc_latest_jar velocity "$FELIS_VELOCITY_VERSION")" \
      || die "no Velocity build for ${FELIS_VELOCITY_VERSION} (override with FELIS_VELOCITY_VERSION)"
    url="${resolved% *}"
    want="${resolved##* }"
    log "downloading Velocity ${FELIS_VELOCITY_VERSION}"
    tmp="$(mktemp "${VELOCITY_DIR}/.velocity.jar.XXXXXX")"
    remember_temp "$tmp"
    curl -fsSL "$url" -o "$tmp" || die "failed to download Velocity: ${url}"
    # The same gate the Via plugins and the fork jar pass: this jar is the proxy every
    # player connects through, and Fill already promised its digest in the URL — a
    # truncated or tampered download becomes a refusal here, not a proxy that won't boot.
    have="$(sha256sum <"$tmp" | cut -d' ' -f1)"
    [ "$have" = "$want" ] \
      || die "Velocity ${FELIS_VELOCITY_VERSION} checksum mismatch: got ${have}, expected ${want}"
    atomic_install_file "$tmp" "${VELOCITY_DIR}/velocity.jar" 0644 root root
  fi

  install_via_plugins
  write_velocity_config
  install_velocity_service
  configure_velocity_firewall
}

# felis_internal_ip echoes the felis-api-internal Service ClusterIP. Cluster DNS does not
# resolve from the host, but a Service ClusterIP DOES route from the node (kube-proxy programs
# the host netns) — the same trick the on-node break-glass console uses. The internal face is
# deliberately ClusterIP-only: it is service-token authenticated and must never be published on
# a node's external IP. Both the felis-link plugin config and the Velocity sessionserver
# override (install_velocity_service) point at it, so the lookup lives here once.
felis_internal_ip() {
  local ip
  ip="$(kube -n "$CONTROL_NS" get svc felis-api-internal -o jsonpath='{.spec.clusterIP}')" \
    || die "could not resolve the felis-api-internal ClusterIP"
  [ -n "$ip" ] || die "felis-api-internal has no ClusterIP"
  printf '%s' "$ip"
}

write_velocity_config() {
  local api_ip tmp
  api_ip="$(felis_internal_ip)"

  prepare_velocity_layout
  tmp="$(mktemp -d)"
  remember_temp "$tmp"

  # The forwarding key. Velocity refuses to start on an empty one ("The forwarding-secret
  # file must not be empty."), which is the failure mode we want if this ever goes wrong.
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

[query]
enabled = false
EOF

  (umask 077; cat > "${tmp}/felis-link.properties" <<EOF
# Generated by deploy/bootstrap.sh — do not edit by hand; rerun the installer.
api-base-url=http://${api_ip}:8081
service-token=${SERVICE_TOKEN}
root-domain=${FELIS_ROOT_DOMAIN}
panel-hostname=console.${FELIS_ROOT_DOMAIN}
admin-hostname=op.console.${FELIS_ROOT_DOMAIN}
login-server=${LOGIN_SERVER}
lobby-server=${LOBBY_SERVER}
EOF
  )

  atomic_install_file "${tmp}/forwarding.secret" "${VELOCITY_DIR}/forwarding.secret" 0640 root "$VELOCITY_USER"
  atomic_install_file "${tmp}/velocity.toml" "${VELOCITY_DIR}/velocity.toml" 0640 "$VELOCITY_USER" "$VELOCITY_USER"
  atomic_install_file "${tmp}/felis-link.properties" \
    "${VELOCITY_DIR}/plugins/felis-link/felis-link.properties" 0640 root "$VELOCITY_USER"
  ok "velocity.toml + forwarding secret + felis-link.properties written (${VELOCITY_DIR})"
}

install_velocity_service() {
  local api_ip
  # Point Velocity's authlib (mojang.sessionserver) at the felis-api hasJoined multiplexer so a
  # full install federates Mojang + the configured [[auth_source]] set (LittleSkin by default)
  # out of the box — not just the standalone `felis nano`. felis-api enforces the reclaim
  # blacklist on this route; a loopback nano would bypass it.
  api_ip="$(felis_internal_ip)"
  # Servers that receive their forwarded identity through the handshake address (BungeeCord/legacy
  # style) instead of the proxy-wide modern+secret forwarding. A protocol-47 (1.8.x) backend sits
  # behind ViaVersion, which strips modern forwarding's login-plugin-message when it down-translates
  # the proxy->backend pipeline to protocol 47; only the handshake field survives Via. The Felis
  # fork reads this list from -Dfelis.legacy-forwarding.servers and forwards those servers legacy;
  # every other backend keeps modern+secret untouched.
  #
  # The list is a JVM system property, so it is fixed for the life of the proxy process and a
  # change needs a Velocity restart. FELIS_LEGACY_FORWARDING_SERVERS makes that reachable
  # without editing this script, which is as far as a startup property can go. Having it follow
  # the MinecraftServer CRs instead is a larger change: the forwarding decision lives in the
  # fork's patch to Velocity core, not in the Felis plugin, so core would need to read state the
  # plugin owns and refreshes.
  #
  # The -D below is double-quoted in ExecStart on purpose. The fork trims each element, so it
  # accepts "legacy18, legacy112", but systemd splits ExecStart on whitespace before java ever
  # sees it -- unquoted, that spelling would hand java a stray "legacy112" argument and the unit
  # would not start. Quoting keeps the whole property one argv item.
  local legacy_forwarding_servers="${FELIS_LEGACY_FORWARDING_SERVERS}"
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
ExecStart=${JRE_DIR}/bin/java -Xms512M -Xmx1G -XX:+UseG1GC -XX:+ParallelRefProcEnabled -XX:+AlwaysPreTouch -Dmojang.sessionserver=http://${api_ip}:8081/session/minecraft/hasJoined "-Dfelis.legacy-forwarding.servers=${legacy_forwarding_servers}" -jar ${VELOCITY_DIR}/velocity.jar
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
  # restart, not `enable --now`: on a re-run the old proxy is already up and --now would
  # leave it running against the new config.
  systemctl restart felis-velocity
  ok "felis-velocity.service enabled and started (0.0.0.0:${FELIS_GAME_PORT})"
}

configure_velocity_firewall() {
  command -v firewall-cmd >/dev/null 2>&1 || return 0
  systemctl is-active --quiet firewalld || return 0
  log "opening firewalld port ${FELIS_GAME_PORT}/tcp for the Minecraft proxy"
  firewall-cmd --permanent --add-port="${FELIS_GAME_PORT}/tcp"
  firewall-cmd --reload
}

# ---------------------------------------------------------------------------
# 6. PostgreSQL on the host. felis-api pods reach it at <node-ip>:5432;
#    migrations run from the host binary against 127.0.0.1.
# ---------------------------------------------------------------------------
write_pg_hba_block() {
  local hba="$1" tmp tmp_new node_cidr

  node_cidr="${NODE_IP}/32"
  tmp="$(mktemp)"
  tmp_new="${tmp}.new"
  remember_temp "$tmp"
  remember_temp "$tmp_new"

  awk \
    -v db="$DB_NAME" \
    -v user="$DB_USER" \
    -v pod="$POD_CIDR" \
    -v node="$node_cidr" '
      $0 == "# BEGIN FELIS MANAGED HBA" { skip = 1; next }
      $0 == "# END FELIS MANAGED HBA" { skip = 0; next }
      skip { next }

      # Clean up rules appended by older bootstrap versions.
      $1 == "host" && $2 == db && $3 == user && $5 == "scram-sha-256" &&
        ($4 == "127.0.0.1/32" || $4 == pod || $4 == node) { next }

      { print }
    ' "$hba" > "$tmp"

  {
    printf "# BEGIN FELIS MANAGED HBA\n"
    printf "# Felis rules must precede distro defaults such as 127.0.0.1 ident.\n"
    printf "host %s %s 127.0.0.1/32 scram-sha-256\n" "$DB_NAME" "$DB_USER"
    printf "host %s %s %s scram-sha-256\n" "$DB_NAME" "$DB_USER" "$POD_CIDR"
    printf "host %s %s %s scram-sha-256\n" "$DB_NAME" "$DB_USER" "$node_cidr"
    printf "# END FELIS MANAGED HBA\n"
    printf "\n"
    cat "$tmp"
  } > "$tmp_new"

  cat "$tmp_new" > "$hba"
  rm -f "$tmp" "$tmp_new"
}

postgres_data_dir() {
  case "$PKG" in
    pacman) printf '%s\n' /var/lib/postgres/data ;;
    *) printf '%s\n' /var/lib/pgsql/data ;;
  esac
}

init_postgres_data_dir() {
  local data_dir
  data_dir="$(postgres_data_dir)"

  [ "$PKG" = "apt" ] && return 0
  [ -f "${data_dir}/PG_VERSION" ] && return 0

  log "initialising postgresql data directory at ${data_dir}"
  if command -v postgresql-setup >/dev/null 2>&1; then
    postgresql-setup --initdb || /usr/bin/postgresql-setup initdb
  elif command -v initdb >/dev/null 2>&1; then
    install -d -o postgres -g postgres -m 0700 "$data_dir"
    as_postgres initdb -D "$data_dir"
  else
    die "cannot initialise postgresql data directory: postgresql-setup/initdb not found"
  fi
}

install_postgres() {
  if command -v psql >/dev/null 2>&1 && systemctl list-unit-files 2>/dev/null | grep -q '^postgresql'; then
    ok "postgresql already installed"
  else
    log "installing postgresql"
    case "$PKG" in
      apt) pkg_install postgresql ;;
      dnf) pkg_install postgresql-server postgresql ;;
      yum) pkg_install postgresql-server postgresql ;;
      zypper) pkg_install postgresql-server postgresql ;;
      pacman) pkg_install postgresql ;;
    esac
  fi

  init_postgres_data_dir
  systemctl enable --now postgresql
  ok "postgresql running"
}

configure_postgres() {
  local cfg hba
  cfg="$(as_postgres psql -tAc 'SHOW config_file;' 2>/dev/null || true)"
  hba="$(as_postgres psql -tAc 'SHOW hba_file;' 2>/dev/null || true)"
  [ -n "$cfg" ] && [ -n "$hba" ] || die "could not query postgresql config/hba file paths"

  # Listen on all interfaces (applied on restart). ALTER SYSTEM is idempotent.
  as_postgres psql -v ON_ERROR_STOP=1 -c "ALTER SYSTEM SET listen_addresses = '*';" >/dev/null

  # Allow the host loopback, the pod CIDR, and the node IP before broader distro defaults.
  write_pg_hba_block "$hba"

  # Role + database (idempotent), and (re)set the password to our generated one.
  as_postgres psql -v ON_ERROR_STOP=1 <<SQL >/dev/null
SET password_encryption = 'scram-sha-256';
DO \$\$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '${DB_USER}') THEN
    CREATE ROLE ${DB_USER} LOGIN PASSWORD '${DB_PASSWORD}';
  END IF;
END
\$\$;
ALTER ROLE ${DB_USER} WITH LOGIN PASSWORD '${DB_PASSWORD}';
SQL
  if ! as_postgres psql -tAc "SELECT 1 FROM pg_database WHERE datname='${DB_NAME}'" | grep -q 1; then
    as_postgres createdb -O "$DB_USER" "$DB_NAME"
  fi

  systemctl restart postgresql
  ok "postgresql configured (listen=*, role/db '${DB_NAME}', pg_hba opened to pods)"
}

# ---------------------------------------------------------------------------
# 7. Secrets + felis.toml (pod variant reaches Postgres at the node IP; host
#    variant at 127.0.0.1 for migrations)
# ---------------------------------------------------------------------------
load_or_make_secrets() {
  mkdir -p "$STATE_DIR"
  chmod 0700 "$STATE_DIR"
  if [ -f "$SECRETS_ENV" ]; then
    # shellcheck disable=SC1090
    . "$SECRETS_ENV"
    ok "reusing persisted secrets from ${SECRETS_ENV}"
  fi
  DB_PASSWORD="${DB_PASSWORD:-$(openssl rand -hex 24)}"
  SERVICE_TOKEN="${SERVICE_TOKEN:-$(openssl rand -hex 32)}"
  SESSION_SECRET="${SESSION_SECRET:-$(openssl rand -hex 32)}"
  # The Velocity modern-forwarding key. It is what makes a backend's UUID trustworthy:
  # the proxy does the Mojang handshake and HMACs the resulting profile with this key,
  # and a backend that cannot verify it would fall back to an offline UUID derived from
  # the username — i.e. anyone could join as anyone, the Owner included. Same value on
  # the proxy (forwarding.secret) and in every backend pod (felis-forwarding-secret).
  FORWARDING_SECRET="${FORWARDING_SECRET:-$(openssl rand -hex 32)}"
  (
    umask 077
    cat > "$SECRETS_ENV" <<EOF
DB_PASSWORD=${DB_PASSWORD}
SERVICE_TOKEN=${SERVICE_TOKEN}
SESSION_SECRET=${SESSION_SECRET}
FORWARDING_SECRET=${FORWARDING_SECRET}
EOF
  )
  chmod 0600 "$SECRETS_ENV"
}

ensure_panel_tls_cert() {
  mkdir -p "$STATE_DIR"
  chmod 0700 "$STATE_DIR"
  if [ -s "$PANEL_TLS_CERT" ] && [ -s "$PANEL_TLS_KEY" ]; then
    ok "panel TLS certificate already present"
    return 0
  fi

  local cn conf
  cn="op.console.${FELIS_ROOT_DOMAIN}"
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
DNS.1 = op.console.${FELIS_ROOT_DOMAIN}
DNS.2 = console.${FELIS_ROOT_DOMAIN}
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

# persisted_smtp_block echoes the [smtp] section an earlier run left behind, or
# nothing. Unlike every other value in the generated toml, [smtp] is not derived
# from this script's inputs -- `felis setup`'s SMTP screen writes it, after
# proving the relay works. A wholesale `cat >` therefore erases it on every
# re-run, and since re-running the installer is the documented way to update
# felis-api, an operator who updates loses mail: OTP delivery silently reverts
# to the no-Mailer path and every code is logged instead of sent. Same defect
# family as the root_domain loss fixed in ecbeb20 -- generated file, hand-set
# value, no carry-forward.
#
# Cached on first call because write_felis_toml clobbers felis.host.toml before
# it is called again for felis.pod.toml: by then the file this would read from
# no longer has the block. The pod toml is the fallback for exactly that window.
persisted_smtp_block() {
  if [ -z "${SMTP_BLOCK_CACHED:-}" ]; then
    SMTP_BLOCK_CACHED=1
    SMTP_BLOCK=""
    local f
    for f in "${STATE_DIR}/felis.host.toml" "${STATE_DIR}/felis.pod.toml"; do
      [ -r "$f" ] || continue
      # Print from [smtp] up to (not including) the next section header.
      SMTP_BLOCK="$(awk '/^[[:space:]]*\[smtp\]/ { f=1 }
                         f && /^[[:space:]]*\[/ && !/^[[:space:]]*\[smtp\]/ { exit }
                         f { print }' "$f")"
      [ -n "$SMTP_BLOCK" ] && break
    done
  fi
  printf '%s' "$SMTP_BLOCK"
}

# persisted_auth_source_blocks echoes the [[auth_source]] tables an earlier run left
# behind, or the LittleSkin default when there is no earlier felis.toml at all. The
# list is the operator's: it is the only way to add or drop a Yggdrasil root on a full
# install, and nothing in this script's inputs derives it. Without the carry-forward a
# re-run would put LittleSkin back after the operator removed it and silently drop any
# root they added. An earlier file with no tables stays that way — that is a Mojang-only
# server, not a missing value. Same first-readable-file rule as persisted_smtp_block.
persisted_auth_source_blocks() {
  local f
  for f in "${STATE_DIR}/felis.host.toml" "${STATE_DIR}/felis.pod.toml"; do
    [ -r "$f" ] || continue
    # Every [[auth_source]] table, up to (not including) the next other section header.
    awk '/^[[:space:]]*\[\[auth_source\]\]/ { f=1 }
         f && /^[[:space:]]*\[/ && !/^[[:space:]]*\[\[auth_source\]\]/ { f=0 }
         f { print }' "$f"
    return 0
  done
  printf '%s\n' '[[auth_source]]' 'tag = "littleskin"' 'prefix = "LS"' \
    'url = "https://littleskin.cn/api/yggdrasil/sessionserver/session/minecraft/hasJoined"'
}

write_felis_toml() {
  local target="$1" db_host="$2" smtp_block auth_source_blocks
  smtp_block="$(persisted_smtp_block)"
  if [ -n "$smtp_block" ]; then
    log "carrying forward the configured [smtp] relay"
    smtp_block="${smtp_block}"$'\n' # keep a blank line before the next section
  fi
  auth_source_blocks="$(persisted_auth_source_blocks)"
  cat > "$target" <<EOF
# Generated by deploy/bootstrap.sh; rerun the installer to regenerate. Hand edits are
# overwritten, except [smtp] and [[auth_source]], which carry forward.
[server]
listen = "0.0.0.0:8080"
root_domain = "${FELIS_ROOT_DOMAIN}"

[database]
url = "postgres://${DB_USER}:${DB_PASSWORD}@${db_host}:5432/${DB_NAME}?sslmode=disable"

[k8s]
namespace = "${MINECRAFT_NS}"
egress_mode = "${FELIS_EGRESS_MODE}"

[velocity]
# The two always-on system servers that felis setup provisions. They are built and imported
# into k3s by build_game_stack below, so setup never has to be told "build these first".
login_image = "${FELIS_LIMBO_IMAGE}"
lobby_image = "${FELIS_LOBBY_IMAGE}"

[registry]
url = "${REGISTRY_URL}"
build_namespace = "${BUILD_NS}"

[archive]
store = "tarLocal"
local_path = "/var/lib/felis/archives"

[auth]
admin_hostname = "op.console.${FELIS_ROOT_DOMAIN}"
panel_hostname = "console.${FELIS_ROOT_DOMAIN}"

${smtp_block}
# Third-party Yggdrasil sources federated by the hasJoined multiplexer. Mojang is
# always the code-owned identity anchor (premium-first), prepended in Go; sources here
# append as namespace-rewritten guests. A fresh install federates LittleSkin. Edit the
# list in ${STATE_DIR}/felis.host.toml and rerun the installer; re-runs keep it as it
# is, and with no [[auth_source]] at all the server is Mojang-only.
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

# ---------------------------------------------------------------------------
# 8. Migrate + deploy bundle
# ---------------------------------------------------------------------------
run_migrations() {
  write_felis_toml "${STATE_DIR}/felis.host.toml" "127.0.0.1"
  ensure_default_config
  log "running database migrations (host binary -> 127.0.0.1)"
  "$HOST_BIN" migrate up -config "${STATE_DIR}/felis.host.toml"
  ok "migrations applied"
}

deploy_bundle() {
  local had_api=0 had_operator=0
  export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
  write_felis_toml "${STATE_DIR}/felis.pod.toml" "${NODE_IP}"

  kube -n "$CONTROL_NS" get deployment felis-api >/dev/null 2>&1 && had_api=1
  kube -n "$CONTROL_NS" get deployment felis-operator >/dev/null 2>&1 && had_operator=1

  # Always the embedded copy. It is byte-identical to deploy/crd/ (bootstrap_asset.go embeds
  # that very file), it needs no checkout — which the release-download path does not have —
  # and one source beats a branch whose two arms have to be kept in agreement by hand.
  log "applying MinecraftServer CRD"
  "$HOST_BIN" bootstrap-assets crd | kube apply -f -

  log "ensuring namespaces"
  local ns
  for ns in "$CONTROL_NS" "$MINECRAFT_NS" "$BUILD_NS"; do
    kube create namespace "$ns" --dry-run=client -o yaml | kube apply -f -
  done

  log "provisioning felis-config + felis-service-token + felis-forwarding-secret + panel TLS secrets (out-of-band, never in the bundle)"
  kube -n "$CONTROL_NS" create secret generic felis-config \
    --from-file=felis.toml="${STATE_DIR}/felis.pod.toml" \
    --dry-run=client -o yaml | kube apply -f -
  apply_literal_secret "$CONTROL_NS" felis-service-token token "$SERVICE_TOKEN"
  # The forwarding key every backend verifies the proxy's handshake with. `felis setup`
  # replicates it into the minecraft namespace (ensureSecretReplica) before it creates
  # the pods that mount it; the operator injects it into EVERY backend, because Velocity's
  # forwarding mode is one proxy-wide setting — a backend that does not speak it is not
  # "less secure", it is unjoinable.
  apply_literal_secret "$CONTROL_NS" felis-forwarding-secret secret "$FORWARDING_SECRET"
  kube -n "$CONTROL_NS" create secret tls felis-api-tls \
    --cert="$PANEL_TLS_CERT" \
    --key="$PANEL_TLS_KEY" \
    --dry-run=client -o yaml | kube apply -f -

  log "rendering + applying the control-plane bundle"
  "$HOST_BIN" manifests \
    --felis-image "$FELIS_IMAGE" \
    --panel-node-port "$FELIS_PANEL_NODEPORT" \
    --velocity-cidr "${NODE_IP}/32" \
    | kube apply -f -
  restart_existing_control_plane "$had_api" "$had_operator"

  log "waiting for control-plane rollouts"
  local d
  for d in $(kube -n "$CONTROL_NS" get deploy -o name); do
    if ! kube -n "$CONTROL_NS" rollout status "$d" --timeout=180s; then
      diagnose_rollout "$d"
      die "control-plane rollout did not complete: ${d}"
    fi
  done
}

restart_existing_control_plane() {
  local had_api="$1" had_operator="$2"
  [ "$had_api$had_operator" != "00" ] || return 0

  log "restarting existing control-plane deployments to pick up ${FELIS_IMAGE}"
  # `if`, not `[ test ] && cmd`: as the LAST command of the function the and-list returns 1
  # when the test is false, which becomes the function's exit status and kills the whole
  # install under `set -Eeuo pipefail` — right after the bundle is applied and before the
  # rollout wait. Fires on any host carrying felis-api without felis-operator.
  if [ "$had_api" = "1" ]; then kube -n "$CONTROL_NS" rollout restart deployment/felis-api; fi
  if [ "$had_operator" = "1" ]; then kube -n "$CONTROL_NS" rollout restart deployment/felis-operator; fi
}

# The login/lobby images use local mutable tags. Importing a replacement updates
# containerd, but an existing StatefulSet template is byte-for-byte unchanged and
# Kubernetes will not roll it. Recreate only the two always-on system pods so a
# convergent bootstrap actually starts the images it just imported.
restart_existing_system_servers() {
  local name pods
  for name in "$LOGIN_SERVER" "$LOBBY_SERVER"; do
    pods="$(kube -n "$MINECRAFT_NS" get pod \
      -l "felis.lolicon.best/server=${name}" -o name 2>/dev/null || true)"
    [ -n "$pods" ] || continue
    log "restarting existing ${name} system server to pick up its imported image"
    kube -n "$MINECRAFT_NS" delete pod \
      -l "felis.lolicon.best/server=${name}" --wait=false
  done
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

# ---------------------------------------------------------------------------
# 9. Summary
# ---------------------------------------------------------------------------
summary() {
  export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
  echo
  ok "Felis control plane deployed."
  echo
  kube -n "$CONTROL_NS" get pods -o wide || true
  echo
  systemctl --no-pager --full status felis-velocity 2>/dev/null | head -n 4 || true
  echo
  log "Player panel: https://console.${FELIS_ROOT_DOMAIN} — served on 443 once your edge/Cloudflare Tunnel routes it here."
  log "Operator console (Op/Admin/Owner): https://op.console.${FELIS_ROOT_DOMAIN} — the Owner runs 'felis setup' and onboards here."
  log "Before the edge is ready: direct + self-signed at https://${NODE_IP}:${FELIS_PANEL_NODEPORT} (browser will warn on first visit)."
  log "Minecraft address: ${NODE_IP}:${FELIS_GAME_PORT} (point mc.${FELIS_ROOT_DOMAIN} here)"
  log "The proxy authenticates against Mojang and forwards the verified profile to the"
  log "login gate; the backends are reachable in-cluster only. Follow it with:"
  log "    sudo journalctl -u felis-velocity -f"
  if [ "${FELIS_BOOTSTRAP_FROM_TUI:-}" = "1" ]; then
    log "Returning to the setup console to create the Owner account and verify panel access."
  else
    log "Next: run  'sudo felis setup'  on this host to create the Owner account."
  fi
  log "setup provisions the login/lobby servers, then asks the Owner to bind by joining"
  log "the proxy in Minecraft — that is what makes the Owner's admin identity a real"
  log "Mojang account rather than a password."
  log "Use 'sudo felis breakGlass' only for emergency local Owner recovery/reset."
  echo
}

# ---------------------------------------------------------------------------
# 10. Felis-nano install path — the auth multiplexer only: felis binary + a
#     minimal [[auth_source]] config + a systemd unit running `felis nano`.
#     No k3s, no Postgres, no control-plane bundle. Chosen at the top-of-run
#     prompt (or FELIS_INSTALL_MODE=nano).
# ---------------------------------------------------------------------------
prompt_install_mode() {
  case "$INSTALL_MODE" in
    full|nano) log "install mode: ${INSTALL_MODE} (from FELIS_INSTALL_MODE)"; return 0 ;;
    "") ;;
    *) die "FELIS_INSTALL_MODE must be 'full' or 'nano', got: ${INSTALL_MODE}" ;;
  esac

  # No override: ask on the controlling terminal. Under `curl | sudo bash` stdin
  # is the script, so we must read /dev/tty, not stdin. No tty (CI/cloud-init) →
  # default to a full install.
  if [ ! -r /dev/tty ]; then
    INSTALL_MODE="full"
    log "no terminal for a prompt; defaulting to a full Felis install (set FELIS_INSTALL_MODE=nano to override)"
    return 0
  fi

  printf '\n'
  printf 'What do you want to install on this host?\n'
  printf '  [1] Felis       — full control plane (k3s + Postgres + panel; orchestrates Minecraft servers)\n'
  printf '  [2] Felis-nano  — auth multiplexer only (federates Mojang + third-party Yggdrasil; no k3s/DB)\n'
  local reply
  while :; do
    printf 'Choose [1/2] (default 1): '
    IFS= read -r reply </dev/tty || reply=""
    case "$reply" in
      ""|1|full|Felis|felis) INSTALL_MODE="full"; break ;;
      2|nano|felis-nano|Felis-nano) INSTALL_MODE="nano"; break ;;
      *) printf 'Please enter 1 or 2.\n' ;;
    esac
  done
  log "install mode: ${INSTALL_MODE}"
}

install_go_toolchain() {
  local arch tarball url
  if [ -x "${GOROOT_DIR}/bin/go" ] && "${GOROOT_DIR}/bin/go" version | grep -q "go${FELIS_GO_VERSION} "; then
    ok "go ${FELIS_GO_VERSION} already installed at ${GOROOT_DIR}"
    return 0
  fi

  case "$(uname -m)" in
    x86_64|amd64) arch="amd64" ;;
    aarch64|arm64) arch="arm64" ;;
    *) die "no Go toolchain build for architecture $(uname -m); set FELIS_GO_VERSION or pre-stage ${GOROOT_DIR}" ;;
  esac

  tarball="go${FELIS_GO_VERSION}.linux-${arch}.tar.gz"
  url="https://go.dev/dl/${tarball}"
  log "installing Go ${FELIS_GO_VERSION} (${arch}) to ${GOROOT_DIR}"
  curl -fsSL "$url" -o "/tmp/${tarball}" || die "failed to download the Go toolchain: ${url}"
  rm -rf "$GOROOT_DIR"
  tar -C "$(dirname "$GOROOT_DIR")" -xzf "/tmp/${tarball}" || die "failed to unpack ${tarball}"
  rm -f "/tmp/${tarball}"
  ok "go toolchain at ${GOROOT_DIR}/bin/go"
}

build_nano_binary() {
  local staged="${SRC_DIR}/.felis-nano-build"

  log "building felis from source (${SRC_DIR})${FELIS_VERSION:+ — ${FELIS_VERSION}}"
  # Stamped for the same reason the image build is: a nano host runs `felis version` and
  # `felis update` too, and this path used to pass no ldflags at all.
  ( cd "$SRC_DIR" \
    && PATH="${GOROOT_DIR}/bin:${PATH}" CGO_ENABLED=0 go build -trimpath \
        -ldflags "-X main.version=${FELIS_VERSION:-dev}" -o "$staged" ./cmd/felis ) \
    || die "go build ./cmd/felis failed"

  # The whole point of this path is the nano subcommand; a binary without it would
  # only surface as a crash-looping felis-nano.service, so fail loudly here instead.
  "$staged" -h 2>&1 | grep -qw nano || die "built binary has no 'nano' subcommand"

  # Stage-then-install, never `go build -o ${HOST_BIN}` directly: the Go linker renames
  # its output out of $TMPDIR, and a same-filesystem rename CARRIES THE SOURCE SELinux
  # label — the binary lands in /usr/local/bin still labelled user_tmp_t. Root (being
  # unconfined) can still run it, so it looks fine by hand, but the DynamicUser service
  # cannot exec it and felis-nano dies with 203/EXEC. Creating the file fresh at the
  # destination lets the policy's type transition label it bin_t; restorecon is the belt.
  mkdir -p "$(dirname "$HOST_BIN")"
  rm -f "$HOST_BIN"
  install -m 0755 "$staged" "$HOST_BIN"
  rm -f "$staged"
  command -v restorecon >/dev/null 2>&1 && restorecon "$HOST_BIN" >/dev/null 2>&1 || true

  ok "felis binary on host at ${HOST_BIN}"
}

acquire_nano_binary() {
  if bootstrap_from_tui; then
    install_embedded_binary
    return 0
  fi
  # The release channel takes the same prebuilt binary the control plane does. This is the
  # biggest win on this path: a host that only wants the auth multiplexer stops needing a Go
  # toolchain and a checkout at all.
  if use_release_binary; then
    resolve_install_ref
    if download_release_binary; then
      return 0
    fi
  fi
  # Raw curl|bash with no usable release: build it straight from source with a pinned Go
  # toolchain — nano needs one static binary, not an image, so dragging in docker
  # (as the full control-plane path does) buys nothing and costs a daemon that must
  # start. It does not start on EL10: the docker-ce el10 rpms install but dockerd
  # fails, which used to kill the whole nano install at `systemctl enable --now docker`.
  fetch_source
  install_go_toolchain
  build_nano_binary
}

write_nano_config() {
  local target="${STATE_DIR}/felis.toml"
  # The unit is a DynamicUser, so it can read felis.toml only if it can search this
  # directory. The mode is explicit because a hardened root umask (027) would leave it 0750.
  # An existing directory keeps its mode: the full install locks it to 0700 for its secrets,
  # and install_nano_service reports that lockout rather than this widening it.
  [ -d "$STATE_DIR" ] || mkdir -p -m 0755 "$STATE_DIR"
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

nano_listen_is_loopback() {
  case "${FELIS_NANO_LISTEN%:*}" in
    127.*|localhost|::1|"[::1]") return 0 ;;
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
  command -v firewall-cmd >/dev/null 2>&1 || return 0
  systemctl is-active --quiet firewalld || return 0
  local port="${FELIS_NANO_LISTEN##*:}"
  log "opening firewalld port ${port}/tcp for felis-nano"
  firewall-cmd --permanent --add-port="${port}/tcp"
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
  local port="${FELIS_NANO_LISTEN##*:}" host
  if nano_listen_is_loopback; then host="127.0.0.1"; else host="${NODE_IP}"; fi
  echo
  ok "Felis-nano deployed."
  echo
  systemctl --no-pager --full status felis-nano 2>/dev/null | head -n 6 || true
  echo
  log "hasJoined endpoint: http://${host}:${port}/session/minecraft/hasJoined"
  log "Point Velocity at it — add to the proxy JVM startup flags (between java and -jar):"
  log "    -Dmojang.sessionserver=http://${host}:${port}/session/minecraft/hasJoined"
  log "    (the FULL endpoint URL, path included — Velocity's default for this property is"
  log "     the full https://sessionserver.mojang.com/session/minecraft/hasJoined)"
  if nano_listen_is_loopback; then
    log "Bound to loopback: reachable from Velocity on THIS host, and from nowhere else."
    log "Proxy on another machine? Re-run with FELIS_NANO_LISTEN=<private-ip>:${port} and"
    log "allow ${port}/tcp ONLY from that proxy — hasJoined takes no auth token, so an"
    log "internet-facing one is a free auth relay burning your Mojang egress IP."
  else
    log "WARNING: bound to ${FELIS_NANO_LISTEN} — hasJoined takes no auth token, so restrict"
    log "${port}/tcp to your proxy's source IP or anyone can relay their logins through you."
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

main() {
  validate_settings
  detect_os
  prompt_install_mode
  if [ "$INSTALL_MODE" = "nano" ]; then
    main_nano
    return
  fi
  pause_package_background_timers
  detect_node_ip
  ensure_swap
  install_base
  # Right after install_base because it is the first point curl exists, and well before
  # docker and k3s: a missing FELIS_GITHUB_TOKEN or an unpublished release should cost
  # the operator seconds, not a k3s install they then have to unwind. This is purely
  # fail-fast — fetch_source resolves again itself — so it must skip on exactly the paths
  # that never consume the result, or it invents a network dependency and a version they
  # do not have: the TUI rebuilds the binary it is already running, and FELIS_SKIP_FETCH
  # builds whatever is staged, which stamp_version reads the SHA off. Resolving anyway
  # would set FELIS_VERSION to the newest tag and stamp a staged tree as that release.
  bootstrap_from_tui || [ -n "${FELIS_SKIP_FETCH:-}" ] || resolve_install_ref
  install_cloudflared
  load_or_make_secrets
  ensure_panel_tls_cert
  install_docker
  install_k3s
  # Three ways to end up with a felis binary, in preference order. The release download is
  # the only one that skips compiling: it is the CI artifact for this exact tag, panel
  # included. Both other arms leave HAVE_PREBUILT_BINARY unset where a source build is what
  # actually happens, which is what routes build_image below.
  if bootstrap_from_tui; then
    install_embedded_binary
  elif use_release_binary && download_release_binary; then
    :
  else
    fetch_source
  fi
  build_image
  build_game_stack
  install_postgres
  configure_postgres
  run_migrations
  deploy_bundle
  restart_existing_system_servers
  # After deploy_bundle: the proxy dials felis-api's internal ClusterIP, which does not
  # exist until the bundle is applied.
  install_velocity
  mark_bootstrap_done
  summary
}

main "$@"
