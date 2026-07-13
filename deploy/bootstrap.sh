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
#   FELIS_GO_VERSION  Go toolchain used to build the nano binary (default: 1.26.4)
#   FELIS_REPO_URL    git URL to build from   (raw script mode only)
#   FELIS_REF         branch/tag/sha          (raw script mode only)
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
FELIS_REF="${FELIS_REF:-main}"
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
FELIS_GO_VERSION="${FELIS_GO_VERSION:-1.26.4}"
PKG_LOCK_TIMEOUT="${PKG_LOCK_TIMEOUT:-${APT_LOCK_TIMEOUT:-900}}"
APT_LOCK_TIMEOUT="${APT_LOCK_TIMEOUT:-$PKG_LOCK_TIMEOUT}"

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

detect_node_ip() {
  NODE_IP="$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="src"){print $(i+1); exit}}')"
  [ -n "${NODE_IP:-}" ] || NODE_IP="$(hostname -I 2>/dev/null | awk '{print $1}')"
  [ -n "${NODE_IP:-}" ] || die "could not determine this host's primary IPv4 address"
  FELIS_ROOT_DOMAIN="${FELIS_ROOT_DOMAIN:-${NODE_IP}.nip.io}"
  log "node IP: ${NODE_IP}   root domain: ${FELIS_ROOT_DOMAIN}"
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
fetch_source() {
  if [ -n "${FELIS_SKIP_FETCH:-}" ]; then
    [ -d "$SRC_DIR" ] || die "FELIS_SKIP_FETCH set but ${SRC_DIR} does not exist"
    ok "skipping fetch; using pre-staged source at ${SRC_DIR}"
    return 0
  fi
  if [ -d "${SRC_DIR}/.git" ]; then
    log "updating source in ${SRC_DIR}"
    git -C "$SRC_DIR" fetch --depth 1 origin "$FELIS_REF"
    git -C "$SRC_DIR" checkout -f FETCH_HEAD
  else
    log "cloning ${FELIS_REPO_URL} (${FELIS_REF})"
    mkdir -p "$(dirname "$SRC_DIR")"
    git clone --depth 1 --branch "$FELIS_REF" "$FELIS_REPO_URL" "$SRC_DIR" 2>/dev/null \
      || git clone "$FELIS_REPO_URL" "$SRC_DIR"
  fi
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
}

build_image_from_binary() {
  local tmp
  tmp="$(mktemp -d)"
  remember_temp "$tmp"
  cp "$HOST_BIN" "${tmp}/felis"
  cat > "${tmp}/Dockerfile" <<'EOF'
FROM gcr.io/distroless/base-debian12:nonroot
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
  log "building ${FELIS_IMAGE} (this compiles the Go binary; first run is slow)"
  docker build -t "$FELIS_IMAGE" "$SRC_DIR"

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
  if bootstrap_from_tui; then
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
  (
    umask 077
    cat > "$SECRETS_ENV" <<EOF
DB_PASSWORD=${DB_PASSWORD}
SERVICE_TOKEN=${SERVICE_TOKEN}
SESSION_SECRET=${SESSION_SECRET}
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

write_felis_toml() {
  local target="$1" db_host="$2"
  cat > "$target" <<EOF
# Generated by deploy/bootstrap.sh — do not edit by hand; rerun the installer.
[server]
listen = "0.0.0.0:8080"
root_domain = "${FELIS_ROOT_DOMAIN}"

[database]
url = "postgres://${DB_USER}:${DB_PASSWORD}@${db_host}:5432/${DB_NAME}?sslmode=disable"

[k8s]
namespace = "${MINECRAFT_NS}"
egress_mode = "${FELIS_EGRESS_MODE}"

[registry]
url = "${REGISTRY_URL}"
build_namespace = "${BUILD_NS}"

[archive]
store = "tarLocal"
local_path = "/var/lib/felis/archives"

[auth]
admin_hostname = "op.console.${FELIS_ROOT_DOMAIN}"
panel_hostname = "console.${FELIS_ROOT_DOMAIN}"
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

  log "applying MinecraftServer CRD"
  if bootstrap_from_tui; then
    "$HOST_BIN" bootstrap-assets crd | kube apply -f -
  else
    kube apply -f "${SRC_DIR}/deploy/crd/"
  fi

  log "ensuring namespaces"
  local ns
  for ns in "$CONTROL_NS" "$MINECRAFT_NS" "$BUILD_NS"; do
    kube create namespace "$ns" --dry-run=client -o yaml | kube apply -f -
  done

  log "provisioning felis-config + felis-service-token + panel TLS secrets (out-of-band, never in the bundle)"
  kube -n "$CONTROL_NS" create secret generic felis-config \
    --from-file=felis.toml="${STATE_DIR}/felis.pod.toml" \
    --dry-run=client -o yaml | kube apply -f -
  kube -n "$CONTROL_NS" create secret generic felis-service-token \
    --from-literal=token="${SERVICE_TOKEN}" \
    --dry-run=client -o yaml | kube apply -f -
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
  [ "$had_api" = "1" ] && kube -n "$CONTROL_NS" rollout restart deployment/felis-api
  [ "$had_operator" = "1" ] && kube -n "$CONTROL_NS" rollout restart deployment/felis-operator
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
  log "Panel URL: https://${NODE_IP}:${FELIS_PANEL_NODEPORT}"
  log "DNS alias (if your resolver supports it): https://op.console.${FELIS_ROOT_DOMAIN}:${FELIS_PANEL_NODEPORT}"
  log "The local HTTPS certificate is self-signed; your browser may ask for confirmation on first visit."
  if [ "${FELIS_BOOTSTRAP_FROM_TUI:-}" = "1" ]; then
    log "Returning to the setup console to create the Owner account and verify panel access."
  else
    log "Next: run  'sudo felis setup'  on this host to create the Owner account."
  fi
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

  log "building felis from source (${SRC_DIR})"
  ( cd "$SRC_DIR" \
    && PATH="${GOROOT_DIR}/bin:${PATH}" CGO_ENABLED=0 go build -trimpath -o "$staged" ./cmd/felis ) \
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
  # Raw curl|bash: no binary yet. Build it straight from source with a pinned Go
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
  mkdir -p "$STATE_DIR"
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
# [[auth_source]]
# tag = "littleskin"
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
  log "Point Velocity at it — add to the proxy JVM startup flags:"
  log "    -Dmojang.sessionserver=http://${host}:${port}"
  log "    (base URL only — authlib appends the path itself)"
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
  install_cloudflared
  load_or_make_secrets
  ensure_panel_tls_cert
  install_docker
  install_k3s
  if bootstrap_from_tui; then
    install_embedded_binary
  else
    fetch_source
  fi
  build_image
  install_postgres
  configure_postgres
  run_migrations
  deploy_bundle
  mark_bootstrap_done
  summary
}

main "$@"
