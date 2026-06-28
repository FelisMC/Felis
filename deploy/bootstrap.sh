#!/usr/bin/env bash
#
# Felis one-line bootstrap installer.
#
#   curl -fsSL <raw-url>/deploy/bootstrap.sh | sudo bash
#
# Brings a fresh single-node Linux host from nothing to a running Felis control
# plane: it installs whatever is missing (picking dnf or apt by OS), provisions a
# swap file on tiny hosts, then configures Docker, k3s and PostgreSQL, builds and
# imports the felis image, runs database migrations and applies the rendered
# install bundle (CRD + namespaces + RBAC + NetworkPolicies + control-plane
# Deployments + in-cluster registry).
#
# By design it stops short of serving the web panel. After it finishes you run
# `felis setup` on the host (a TUI) to create the Owner account and optionally
# configure the Cloudflare edge. See deploy/README.md.
#
# The script is idempotent: re-running it converges rather than duplicating, and
# generated secrets are persisted to /etc/felis/secrets.env so reruns reuse them.
#
# Tunables (export before running to override the demo defaults):
#   FELIS_REPO_URL    git URL to build from   (default: the upstream repo)
#   FELIS_REF         branch/tag/sha          (default: main)
#   FELIS_IMAGE       local image tag         (default: felis:demo  — never :latest)
#   FELIS_ROOT_DOMAIN deployment root domain  (default: <node-ip>.nip.io)
#   FELIS_EGRESS_MODE loadbalancer|nodeport   (default: nodeport — no MetalLB on a demo box)
set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration & constants
# ---------------------------------------------------------------------------
FELIS_REPO_URL="${FELIS_REPO_URL:-https://github.com/MliroLirrorsIngenuity/Felis.git}"
FELIS_REF="${FELIS_REF:-main}"
FELIS_IMAGE="${FELIS_IMAGE:-felis:demo}"
FELIS_EGRESS_MODE="${FELIS_EGRESS_MODE:-nodeport}"

CONTROL_NS="felis"
MINECRAFT_NS="minecraft"
BUILD_NS="felis-build"
POD_CIDR="10.42.0.0/16"          # k3s default cluster CIDR
DB_NAME="felis"
DB_USER="felis"
REGISTRY_URL="registry.felis.svc:5000"

STATE_DIR="/etc/felis"
SECRETS_ENV="${STATE_DIR}/secrets.env"
SRC_DIR="/opt/felis/src"
HOST_BIN="/usr/local/bin/felis"
K3S_BIN_DIR="${K3S_BIN_DIR:-/usr/local/bin}"
K3S_BIN="${K3S_BIN_DIR}/k3s"

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

k3s_cmd() { [ -x "$K3S_BIN" ] || die "k3s binary not found at ${K3S_BIN}"; "$K3S_BIN" "$@"; }
kube() { k3s_cmd kubectl "$@"; }

# ---------------------------------------------------------------------------
# 0. Privilege & host facts
# ---------------------------------------------------------------------------
if [ "$(id -u)" -ne 0 ]; then
  log "re-executing under sudo"
  exec sudo -E bash "$0" "$@"
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
  else
    die "no supported package manager (apt/dnf/yum) found on ${OS_ID} ${OS_VERSION}"
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
    apt) DEBIAN_FRONTEND=noninteractive apt-get install -y "$@" ;;
    dnf) dnf install -y "$@" ;;
    yum) yum install -y "$@" ;;
  esac
}

pkg_refresh_once() {
  [ -n "${_PKG_REFRESHED:-}" ] && return 0
  case "$PKG" in
    apt) DEBIAN_FRONTEND=noninteractive apt-get update -y ;;
    dnf|yum) : ;;   # dnf/yum refresh metadata on demand
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
  pkg_refresh_once
  pkg_install curl ca-certificates git openssl
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

  DEBIAN_FRONTEND=noninteractive apt-get update -y
  pkg_install docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
}

docker_rpm_repo_url() {
  case "$OS_ID" in
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

install_docker() {
  if command -v docker >/dev/null 2>&1; then
    ok "docker already installed"
  else
    case "$PKG" in
      apt) install_docker_apt ;;
      dnf|yum) install_docker_rpm ;;
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
# ---------------------------------------------------------------------------
install_k3s() {
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
# 5. Source + image build + host binary + containerd import
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

build_image() {
  systemctl start docker
  log "building ${FELIS_IMAGE} (this compiles the Go binary; first run is slow)"
  docker build -t "$FELIS_IMAGE" "$SRC_DIR"

  log "extracting the felis binary onto the host (${HOST_BIN})"
  local cid
  cid="$(docker create "$FELIS_IMAGE")"
  docker cp "${cid}:/usr/local/bin/felis" "$HOST_BIN"
  docker rm "$cid" >/dev/null
  chmod 0755 "$HOST_BIN"

  log "importing ${FELIS_IMAGE} into k3s containerd"
  docker save "$FELIS_IMAGE" | k3s_cmd ctr images import -

  # Reclaim the ~150 MiB the docker daemon holds; reruns restart it on demand.
  systemctl stop docker docker.socket 2>/dev/null || true
  ok "image built, binary on host, image imported"
}

# ---------------------------------------------------------------------------
# 6. PostgreSQL on the host (apt/dnf). felis-api pods reach it at <node-ip>:5432;
#    migrations run from the host binary against 127.0.0.1.
# ---------------------------------------------------------------------------
write_pg_hba_block() {
  local hba="$1" tmp node_cidr

  node_cidr="${NODE_IP}/32"
  tmp="$(mktemp)"

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
  } > "${tmp}.new"

  cat "${tmp}.new" > "$hba"
  rm -f "$tmp" "${tmp}.new"
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
    esac
  fi
  # RHEL-family ships an uninitialised data dir.
  if [ "$PKG" != "apt" ] && [ ! -f /var/lib/pgsql/data/PG_VERSION ]; then
    log "initialising postgresql data directory"
    if command -v postgresql-setup >/dev/null 2>&1; then
      postgresql-setup --initdb || /usr/bin/postgresql-setup initdb
    fi
  fi
  systemctl enable --now postgresql
  ok "postgresql running"
}

configure_postgres() {
  local cfg hba
  cfg="$(sudo -u postgres psql -tAc 'SHOW config_file;' 2>/dev/null || true)"
  hba="$(sudo -u postgres psql -tAc 'SHOW hba_file;' 2>/dev/null || true)"
  [ -n "$cfg" ] && [ -n "$hba" ] || die "could not query postgresql config/hba file paths"

  # Listen on all interfaces (applied on restart). ALTER SYSTEM is idempotent.
  sudo -u postgres psql -v ON_ERROR_STOP=1 -c "ALTER SYSTEM SET listen_addresses = '*';" >/dev/null

  # Allow the host loopback, the pod CIDR, and the node IP before broader distro defaults.
  write_pg_hba_block "$hba"

  # Role + database (idempotent), and (re)set the password to our generated one.
  sudo -u postgres psql -v ON_ERROR_STOP=1 <<SQL >/dev/null
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
  if ! sudo -u postgres psql -tAc "SELECT 1 FROM pg_database WHERE datname='${DB_NAME}'" | grep -q 1; then
    sudo -u postgres createdb -O "$DB_USER" "$DB_NAME"
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
  umask 077
  cat > "$SECRETS_ENV" <<EOF
DB_PASSWORD=${DB_PASSWORD}
SERVICE_TOKEN=${SERVICE_TOKEN}
SESSION_SECRET=${SESSION_SECRET}
EOF
  chmod 0600 "$SECRETS_ENV"
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
  local target="${STATE_DIR}/felis.toml"
  if [ -L "$target" ] && [ "$(readlink "$target")" = "${STATE_DIR}/felis.host.toml" ]; then
    ok "default host config already points at ${STATE_DIR}/felis.host.toml"
    return 0
  fi
  if [ -e "$target" ] || [ -L "$target" ]; then
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
  export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
  write_felis_toml "${STATE_DIR}/felis.pod.toml" "${NODE_IP}"

  log "applying MinecraftServer CRD"
  kube apply -f "${SRC_DIR}/deploy/crd/"

  log "ensuring namespaces"
  local ns
  for ns in "$CONTROL_NS" "$MINECRAFT_NS" "$BUILD_NS"; do
    kube create namespace "$ns" --dry-run=client -o yaml | kube apply -f -
  done

  log "provisioning felis-config + felis-service-token secrets (out-of-band, never in the bundle)"
  kube -n "$CONTROL_NS" create secret generic felis-config \
    --from-file=felis.toml="${STATE_DIR}/felis.pod.toml" \
    --dry-run=client -o yaml | kube apply -f -
  kube -n "$CONTROL_NS" create secret generic felis-service-token \
    --from-literal=token="${SERVICE_TOKEN}" \
    --dry-run=client -o yaml | kube apply -f -

  log "rendering + applying the control-plane bundle"
  "$HOST_BIN" manifests \
    --felis-image "$FELIS_IMAGE" \
    --velocity-cidr "${NODE_IP}/32" \
    | kube apply -f -

  log "waiting for control-plane rollouts"
  local d
  for d in $(kube -n "$CONTROL_NS" get deploy -o name); do
    kube -n "$CONTROL_NS" rollout status "$d" --timeout=180s || warn "rollout not complete: $d"
  done
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
  log "Web is intentionally NOT enabled yet."
  log "Next: run  'sudo felis setup'  on this host to create the Owner account and configure the web edge."
  log "Use 'sudo felis breakGlass' only for emergency local Owner recovery/reset."
  echo
}

main() {
  detect_os
  detect_node_ip
  ensure_swap
  install_base
  install_cloudflared
  load_or_make_secrets
  install_docker
  install_k3s
  fetch_source
  build_image
  install_postgres
  configure_postgres
  run_migrations
  deploy_bundle
  summary
}

main "$@"
