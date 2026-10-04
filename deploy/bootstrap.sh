#!/usr/bin/env bash
#
# Felis one-line bootstrap installer.
#
#   curl -fsSL <raw-url>/deploy/bootstrap.sh | sudo bash
#
# Brings a fresh single-node Linux host from nothing to a running Felis control
# plane: it installs whatever is missing (picking apt/dnf/yum/zypper/pacman by OS), provisions a
# swap file on tiny hosts, then configures Docker and k3s, starts PostgreSQL in k3s,
# builds and imports the felis image, runs database migrations and applies the rendered
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
# Tunables override the demo defaults. sudo resets the environment, so a variable exported
# before `curl ... | sudo bash` never arrives. Name it on the sudo line, or keep it with -E:
#   curl -fsSL <raw-url>/deploy/bootstrap.sh | sudo FELIS_INSTALL_MODE=nano FELIS_NANO_LISTEN=10.0.0.5:8081 bash
#   export FELIS_INSTALL_MODE=nano; curl -fsSL <raw-url>/deploy/bootstrap.sh | sudo -E bash
#   FELIS_INSTALL_MODE full|nano — skip the prompt (default: ask on a tty, else full; nano
#                     instead on a host that runs felis-nano and no full install)
#   FELIS_NO_SETUP    1 ends a full install at its summary. By default an install that
#                     leaves no Owner account goes on into `felis setup` when it runs on
#                     a terminal
#   FELIS_NANO_LISTEN listen addr for `felis nano` (default: the address an installed
#                     felis-nano already uses, else 127.0.0.1:8081 — loopback only; set a
#                     private-network IP to serve an off-host proxy)
#   FELIS_NANO_PROXY_CIDR the proxy allowed to reach a non-loopback nano bind, as an address
#                     with a prefix length (for example 10.0.0.7/32). firewalld opens the
#                     port to that source only; unset, it opens nothing
#   FELIS_LEGACY_FORWARDING_SERVERS comma-separated backends that always receive their
#                     identity through the handshake address instead of modern forwarding
#                     (default: legacy18). A floor: any server whose MinecraftServer CR is
#                     labelled felis.lolicon.best/forwarding=legacy joins it while the
#                     proxy runs (docs/operations.md), so a new 1.8 backend needs a label,
#                     not a re-run. Changing the floor itself means re-running this script.
#   FELIS_VELOCITY_XMX maximum heap of the Velocity proxy, as <n>M or <n>G (default: 1G;
#                     at least 256M). docs/operations.md sizes it by player count.
#   FELIS_VELOCITY_FORK_JAR path to a Felis-Legacy Velocity fork build to install as the
#                     proxy instead of the stock download (default: unset, stock).
#   FELIS_VELOCITY_FORK_JAR_SHA256 expected sha256 of that jar. REQUIRED whenever the jar
#                     above is set; the install refuses on a mismatch.
#   FELIS_GAME_STACK  pinned|latest — which Limbo, Paper, LuckPerms and Velocity builds to
#                     install (default: pinned, the builds deploy/game-stack.lock names,
#                     each checked against its sha256). latest resolves upstream's newest
#                     builds on every run; the Minecraft version then follows Limbo's CI.
#   FELIS_JRE_VERSION Temurin feature version for the proxy (default: 25, whose build and
#                     digests are pinned; a rerun moves an installer-managed JRE to the
#                     pinned build). Another feature version is checked against the
#                     digest Adoptium's API publishes for it.
#   FELIS_GO_VERSION  Go toolchain used to build the nano binary (default: 1.26.8)
#   FELIS_GO_SHA256   sha256 of that version's linux tarball for this host's architecture.
#                     REQUIRED for a non-default FELIS_GO_VERSION; the default's is pinned.
#   FELIS_K3S_VERSION k3s release a fresh install gets (default: v1.36.4+k3s1). An
#                     installed k3s is left alone unless FELIS_UPGRADE_DEPS=1.
#   FELIS_CLOUDFLARED_VERSION / FELIS_CLOUDFLARED_SHA256 cloudflared release installed
#                     when none is present (default: 2026.9.1, digests pinned); the
#                     sha256 is REQUIRED for any other version
#   FELIS_UPGRADE_DEPS 1 moves an installed k3s and cloudflared to the versions above
#                     (k3s one minor version at a time; neither is ever downgraded) and
#                     restarts cloudflared-felis onto the new binary (default: 0)
#   FELIS_REPO_URL    git URL to build from   (raw script mode only)
#   FELIS_VERSION_BOOTSTRAP release|dev — which version to install (default: release).
#                     release DOWNLOADS what the newest tag publishes: the felis binary
#                     (panel included — it is go:embed'ed into that same binary), every
#                     image and the Velocity plugin, each checked against the release's
#                     SHA256SUMS before it is used, so the host needs neither Docker nor
#                     Docker Hub; dev clones and compiles. If an asset is missing or this
#                     architecture has none, release warns and builds that piece of the
#                     SAME tag here instead (with Docker); a release without SHA256SUMS
#                     is compiled from source whole.
#   FELIS_ARTIFACT_DIR a directory holding a release's assets as release.yml publishes them
#                     (felis-linux-<arch>, felis-image-*-linux-<arch>.tar, their listing
#                     felis-images-linux-<arch>.txt, felis-velocity.jar, SHA256SUMS; see
#                     deploy/build-release-artifacts.sh): the binary, images and plugin are
#                     installed from there and nothing is fetched from api.github.com or
#                     Docker Hub, nor built (FELIS_GAME_STACK=latest aside: no release
#                     ships that stack, so its game images are built here). A file missing
#                     from it or not matching its SHA256SUMS stops the install.
#   FELIS_GITHUB_TOKEN GitHub token; needed only when installing from a private fork
#   FELIS_REF         branch/tag/sha — pins the build, overrides the channel, and forces a
#                     source build (naming a ref asks for that tree, not a published asset)
#   FELIS_RELEASE     a published release tag (v1.2.3) the release channel installs, from
#                     its assets, instead of the newest: the way back to an earlier release.
#                     Read this script at the same tag. Installers older than this variable
#                     ignore it and install the newest; for those, FELIS_REF=<tag> builds
#                     that tag from source
#   FELIS_IMAGE       control-plane image ref (default:
#                     registry.felis.svc:5000/felis/felis:<the felis version>, so each
#                     release has its own tag and `kubectl rollout undo` returns to the
#                     previous one; :demo when the version is unknown — never :latest;
#                     anything not under the registry is used as-is but is NOT
#                     mirrored into it, so it has no pull source after an image GC)
#   FELIS_ROOT_DOMAIN deployment root domain  (default: <node-ip>.nip.io; first install
#                     only -- a rerun keeps the installed one, and sudo felis domain set
#                     moves it)
#   FELIS_PANEL_NODEPORT local HTTPS panel/API NodePort (default: 30443)
#   FELIS_EGRESS_MODE loadbalancer|nodeport   (default: nodeport — no MetalLB on a demo box)
#   FELIS_BACKUP_PVC  world-archive PVC the installer renders and felis-api hands to its
#                     backup/restore Jobs (default: felis-backups; empty string disables
#                     backups — the endpoints answer 503)
#   FELIS_ARCHIVE_LOCAL_PATH path that PVC is mounted at inside those Jobs; written into
#                     felis.toml [archive] local_path (default: /var/lib/felis/archives)
#   FELIS_WORLDS_HOST_PATH node directory holding the world volumes (on the k3s this
#                     installer provisions: /var/lib/rancher/k3s/storage). Setting it
#                     lets the daily reaper also archive and then delete worlds idle
#                     for 15 days (without it the reaper only deletes backups past
#                     their expiry and leaves every world); the reaper reads that
#                     root as root, so it keeps k3s's own 0700 root:root
#                     (default: unset = worlds are never reaped)
#   FELIS_REGISTRY_STORAGE / FELIS_UPLOADS_STORAGE / FELIS_BACKUP_STORAGE capacity the
#                     registry, uploads and world-archive PVCs request on first install
#                     (defaults: 10Gi, 5Gi, 10Gi). An existing claim keeps its size; on
#                     k3s local-path the number is not enforced, see troubleshooting §9
#   FELIS_MANAGE_TIME_SYNC 0 leaves the host's clock alone; by default the installer
#                     turns NTP on (installing chrony when nothing can) and waits for it
#                     to synchronize (default: 1)
#   FELIS_MANAGE_JOURNAL 0 leaves journald alone; by default the installer makes the
#                     system journal persistent so logs survive a reboot (default: 1)
#   FELIS_JOURNAL_MAX_USE the persistent journal's size cap, journald's SystemMaxUse
#                     written <n>K|M|G (default: 1G)
#   FELIS_PREFLIGHT   strict|warn — before touching the host the installer checks RAM,
#                     disk, ports, other Kubernetes, the pod network ranges and the hosts it
#                     downloads from, and stops on any problem with the full list (default:
#                     strict). warn reports them and installs anyway.
#   PKG_LOCK_TIMEOUT seconds to wait for package-manager locks (default: 900)
#   APT_LOCK_TIMEOUT legacy alias for PKG_LOCK_TIMEOUT
set -Eeuo pipefail

# ---------------------------------------------------------------------------
# Configuration & constants
# ---------------------------------------------------------------------------
FELIS_REPO_URL="${FELIS_REPO_URL:-https://github.com/FelisMC/Felis.git}"
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
# A published release tag the release channel installs instead of the newest (header).
FELIS_RELEASE="${FELIS_RELEASE:-}"
# The directory of release assets to install from (header); empty installs as the channel says.
FELIS_ARTIFACT_DIR="${FELIS_ARTIFACT_DIR:-}"
# Set once a prebuilt felis binary is installed at HOST_BIN, by the TUI hand-off, a release
# download or FELIS_ARTIFACT_DIR. It is what the image build, the CRD apply and the game stack
# key off: all three only need "is there a binary and no checkout", never "which route got us
# here".
HAVE_PREBUILT_BINARY=""
# The image the control plane ran before this run moved it (deploy_bundle), for the rollback
# hint in summary.
PREVIOUS_FELIS_IMAGE=""
# Where the images and the Velocity plugin come from, decided by select_release_artifacts once
# the binary is on the host: "dir" (FELIS_ARTIFACT_DIR), "release" (the assets of release
# ARTIFACT_TAG, downloaded into ARTIFACT_CACHE), or empty, when everything is built here.
ARTIFACT_MODE=""
ARTIFACT_TAG=""
# Root-only, and outside /tmp: an image bundle waits here from its import to its push, and a
# run that failed in between finds it again. push_images_to_registry empties it.
ARTIFACT_CACHE="/var/lib/felis/artifacts"
# The SHA256SUMS every artifact is checked against (load_artifact_sums), and the verified local
# copy artifact_fetch last pointed at.
ARTIFACT_SUMS=""
ARTIFACT_FILE=""
# The validated lines of the image listing ("bundle role name manifest-digest config-digest"),
# and whether it was read yet: "", "ok", or "bad" (the listing could not be used).
RELEASE_LISTING=""
RELEASE_LISTING_STATE=""
# One "role bundle name target manifest-digest config-digest" line per image this run took from
# a bundle (import_release_images), and those roles; every other image is built here.
RELEASE_IMAGES=""
PREBUILT_ROLES=" "
# Docker is installed and started only for what has to be built on this host (ensure_docker).
DOCKER_INSTALLED=""
# The release JSON every asset lookup reads, fetched once per tag (load_release_json): an install
# downloads up to a dozen assets, and GitHub allows an address without a token 60 API calls an
# hour.
RELEASE_JSON_TAG=""
RELEASE_JSON=""
# Optional GitHub credential, needed only for a private fork: GitHub answers
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
# Where the installer parks every image it builds, so kubelet can re-pull one the
# image GC has collected (the disk-pressure drill's dead end: ImagePullBackOff with
# nothing to pull from). The node pulls through the loopback hostPort the registry
# Deployment binds (configure_registry_mirror below); pushes go through
# REGISTRY_PUSH_HOST — docker treats 127.0.0.1 as insecure by default, so the
# daemon needs no insecure-registries entry for it.
REGISTRY_URL="registry.felis.svc:5000"
REGISTRY_PUSH_HOST="127.0.0.1:${REGISTRY_URL##*:}"
# Empty unless the operator names one: resolve_felis_image derives the tag from the version
# this run installs, which is only known once the binary or the checkout is.
FELIS_IMAGE="${FELIS_IMAGE:-}"
FELIS_EGRESS_MODE="${FELIS_EGRESS_MODE:-nodeport}"
FELIS_PANEL_NODEPORT="${FELIS_PANEL_NODEPORT:-30443}"
# World-archive storage. The installer renders this PVC (minecraft namespace) and felis-api
# advertises it to its backup/restore Jobs; emptying it disables backups (503). The archive
# path is written into felis.toml so the Jobs' mount and [archive] local_path agree by
# construction — a mismatch would leave tarLocal's absolute archive refs unresolvable.
FELIS_BACKUP_PVC="${FELIS_BACKUP_PVC:-felis-backups}"
FELIS_ARCHIVE_LOCAL_PATH="${FELIS_ARCHIVE_LOCAL_PATH:-/var/lib/felis/archives}"
# PVC capacities; empty keeps `felis manifests`' defaults.
FELIS_REGISTRY_STORAGE="${FELIS_REGISTRY_STORAGE:-}"
FELIS_UPLOADS_STORAGE="${FELIS_UPLOADS_STORAGE:-}"
FELIS_BACKUP_STORAGE="${FELIS_BACKUP_STORAGE:-}"
# Retention is opt-in because it DELETES worlds (after a verified archive): point this at the
# node directory the world volumes live under. On the k3s this installer provisions that is
# /var/lib/rancher/k3s/storage — the reaper resolves each PVC's local-path directory exactly
# from its volumeName. Left unset, the reaper CronJob renders retention-only: backups past
# their expiry are still deleted daily, and no world is ever archived or deleted.
FELIS_WORLDS_HOST_PATH="${FELIS_WORLDS_HOST_PATH:-}"
# k3s's local-path provisioner root. It appears with the first volume the provisioner
# creates, which on a fresh install is after the reaper's PV has been applied.
K3S_STORAGE_ROOT="/var/lib/rancher/k3s/storage"
# Control-plane database backups (felis db backup): a daily timer bundles pg_dump with the
# /etc/felis state a rebuild needs, and every upgrade that has migrations to apply snapshots
# the database first (felis migrate up). The directory sits outside /var/lib/rancher on
# purpose: reinstalling k3s must not take the database backups with it. Copy it off the
# host for anything beyond "undo a bad upgrade or a mistaken delete" (troubleshooting §16).
FELIS_DB_BACKUP_DIR="${FELIS_DB_BACKUP_DIR:-/var/lib/felis/db-backups}"
FELIS_DB_BACKUP_KEEP="${FELIS_DB_BACKUP_KEEP:-14}"
FELIS_DB_BACKUP_TIME="${FELIS_DB_BACKUP_TIME:-*-*-* 03:30:00}"
# node-exporter textfile collector target; FelisDBBackupStale (deploy/alerts) reads it.
FELIS_DB_BACKUP_METRICS="${FELIS_DB_BACKUP_METRICS:-/var/lib/node_exporter/textfile_collector/felis_db_backup.prom}"
# 0 migrates without the pre-migration snapshot. The upgrade stops if the snapshot fails
# and this is not set.
FELIS_PRE_MIGRATE_BACKUP="${FELIS_PRE_MIGRATE_BACKUP:-1}"
# The off-site copy (felis offsite, troubleshooting §16). Every hour the host encrypts each
# world archive and the newest database bundles and copies them to an S3-compatible bucket,
# and the reaper deletes an idle world only once its archive is there. Without a bucket
# every backup lives on this one machine, and losing its disk loses them all. Set the
# bucket and its endpoint to turn it on; FELIS_OFFSITE_ACCESS_KEY / FELIS_OFFSITE_SECRET_KEY
# (and optionally a FELIS_OFFSITE_KEY from `felis offsite keygen`) go into
# /etc/felis/offsite.env, mode 0600, with the encryption key generated when there is none.
# A re-run without these keeps the [offsite] felis.host.toml already has.
FELIS_OFFSITE_ENDPOINT="${FELIS_OFFSITE_ENDPOINT:-}"
FELIS_OFFSITE_BUCKET="${FELIS_OFFSITE_BUCKET:-}"
FELIS_OFFSITE_REGION="${FELIS_OFFSITE_REGION:-}"
FELIS_OFFSITE_PREFIX="${FELIS_OFFSITE_PREFIX:-}"
FELIS_OFFSITE_DB_KEEP="${FELIS_OFFSITE_DB_KEEP:-}"
# The watchdog's heartbeat (troubleshooting §14): the ping URL of a check at a monitoring
# service such as Healthchecks.io, a dead man's switch. Every watchdog run pings it, and
# the service mails its own users when the pings stop or report failure: the host down,
# the watchdog broken, alerts that reach no one. Nothing on this host can report its own
# death. The URL is kept in /etc/felis/watchdog-heartbeat-url, mode 0600, as its path is
# the key that pings the check; off removes it, and a re-run without it keeps it.
FELIS_WATCHDOG_HEARTBEAT_URL="${FELIS_WATCHDOG_HEARTBEAT_URL:-}"
INSTALL_MODE="${FELIS_INSTALL_MODE:-}"
DISTRIBUTED="${FELIS_DISTRIBUTED:-0}"
WORKER_NAME="${FELIS_NODE_NAME:-}"
WORKER_SERVER="${FELIS_SERVER_URL:-}"
WORKER_TOKEN_FILE="${FELIS_BOOTSTRAP_TOKEN_FILE:-}"
WORKER_REGISTRY_IP="${FELIS_REGISTRY_CLUSTER_IP:-}"
NODE_EXTERNAL_IP="${FELIS_NODE_EXTERNAL_IP:-}"
WORKER_PEERS="${FELIS_PEER_CIDRS:-}"
# strict stops the install on any preflight problem (preflight below); warn reports them
# and goes on, for a host the checks misjudge.
FELIS_PREFLIGHT="${FELIS_PREFLIGHT:-strict}"
# Loopback by default: hasJoined is an unauthenticated endpoint by protocol (Velocity
# sends no token), so a public bind is a free auth relay — anyone can point their own
# proxy at it and spend YOUR egress IP on Mojang, until Mojang rate-limits you and your
# own players stop getting in. Same-host Velocity reaches 127.0.0.1 fine; a proxy on
# another machine must opt in explicitly with FELIS_NANO_LISTEN=<private-ip>:8081.
# Left empty here: resolve_nano_listen applies that default only after an existing unit's
# address has had its say.
FELIS_NANO_LISTEN="${FELIS_NANO_LISTEN:-}"
FELIS_NANO_PROXY_CIDR="${FELIS_NANO_PROXY_CIDR:-}"
# Backends that take their forwarded identity through the handshake address instead of
# proxy-wide modern forwarding. See write_velocity_service for why a protocol-47 backend
# needs this. Overridable because adding a second 1.8 backend otherwise means editing this
# script; it is still a restart-time list, not one that follows the CRs.
FELIS_LEGACY_FORWARDING_SERVERS="${FELIS_LEGACY_FORWARDING_SERVERS:-legacy18}"
FELIS_VELOCITY_XMX="${FELIS_VELOCITY_XMX:-1G}"
# The Go tarball is unpacked and run as root, so the default version is pinned by the sha256
# go.dev/dl publishes for each architecture install_go_toolchain handles. Move all three
# together; any other FELIS_GO_VERSION has to bring its own FELIS_GO_SHA256.
GO_PINNED_VERSION="1.26.8"
GO_PINNED_SHA256_AMD64="d0f743b33e8d8945e6b1f432edd15785c70507121d6e2a723b21285eddf8b57b"
GO_PINNED_SHA256_ARM64="211ffced9dcb9633a55eac6364816ec0ddd951389a740e88fa8b3337971bdda0"
FELIS_GO_VERSION="${FELIS_GO_VERSION:-$GO_PINNED_VERSION}"
FELIS_GO_SHA256="${FELIS_GO_SHA256:-}"
# cloudflared runs as root on the edge, so it gets the same treatment: a pinned release and
# the sha256 GitHub lists for each asset. A different FELIS_CLOUDFLARED_VERSION has to bring
# its own FELIS_CLOUDFLARED_SHA256. An installed binary is replaced only under
# FELIS_UPGRADE_DEPS=1; `felis update --cloudflared` reports when that would change it.
CLOUDFLARED_PINNED_VERSION="2026.9.1"
CLOUDFLARED_PINNED_SHA256_AMD64="03f1f25d1cc93b9ad6c60569d44060bc4f17ed97075760ed8cfca4b12dcd68cc"
CLOUDFLARED_PINNED_SHA256_ARM64="3d97437c71848bd8df68041e12436b484a661d95073ea1937f01a845ce88faa3"
CLOUDFLARED_PINNED_SHA256_ARM="093ffa3638ab2b636de63c43a8c68f96a69cf71f9699dd8277a91b160b0f4fc0"
FELIS_CLOUDFLARED_VERSION="${FELIS_CLOUDFLARED_VERSION:-$CLOUDFLARED_PINNED_VERSION}"
FELIS_CLOUDFLARED_SHA256="${FELIS_CLOUDFLARED_SHA256:-}"
CLOUDFLARED_BIN=/usr/local/bin/cloudflared
# The k3s release a fresh install gets, and the tag its install script is read from. The
# script checks the k3s binary against that release's sha256sum file, so pinning the tag
# pins both. An installed k3s moves only under FELIS_UPGRADE_DEPS=1.
FELIS_K3S_VERSION="${FELIS_K3S_VERSION:-v1.36.4+k3s1}"
FELIS_UPGRADE_DEPS="${FELIS_UPGRADE_DEPS:-0}"
FELIS_MANAGE_TIME_SYNC="${FELIS_MANAGE_TIME_SYNC:-1}"
FELIS_MANAGE_JOURNAL="${FELIS_MANAGE_JOURNAL:-1}"
FELIS_JOURNAL_MAX_USE="${FELIS_JOURNAL_MAX_USE:-1G}"
# The in-cluster registry's image, by digest. It must equal platform.defaultRegistryImage
# (internal/platform/identities.go, TestBootstrapPinsTheRegistryImage): the renderer puts
# that ref in the Deployment, and this script caches and pins the same ref in containerd.
REGISTRY_IMAGE="docker.io/library/registry:2.8.3@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373"
# The control-plane database's image, by digest; it must equal platform.defaultPostgresImage
# (TestBootstrapPinsThePostgresImage). Its major version names the cluster directory
# under PG_DATA_DIR, so moving to a new major is a dump and restore (check_postgres_major).
POSTGRES_IMAGE="docker.io/library/postgres:18.6-trixie@sha256:5a5a84b19854a9ffaa54082c166ff4ec27473a361e496e5ea167f298f2da9722"
PKG_LOCK_TIMEOUT="${PKG_LOCK_TIMEOUT:-${APT_LOCK_TIMEOUT:-900}}"
APT_LOCK_TIMEOUT="${APT_LOCK_TIMEOUT:-$PKG_LOCK_TIMEOUT}"

# --- the game stack: proxy on the host, the two always-on backends in k3s ---
FELIS_LIMBO_IMAGE="${FELIS_LIMBO_IMAGE:-${REGISTRY_URL}/felis/limbo:demo}"
FELIS_LOBBY_IMAGE="${FELIS_LOBBY_IMAGE:-${REGISTRY_URL}/felis/lobby:demo}"
# Plain Paper base recommended for a user's own server (deploy/paper). Not a system
# server — forwarding is applied by the operator's init-forwarding initContainer, so it
# needs no secret. Seeded recommended in 0019_recommended_paper.sql.
FELIS_PAPER_IMAGE="${FELIS_PAPER_IMAGE:-${REGISTRY_URL}/felis/paper:demo}"
# The Velocity build comes from deploy/game-stack.lock (VELOCITY_VERSION and its jar digest).
# Setting FELIS_VELOCITY_VERSION to another version, or FELIS_GAME_STACK=latest, installs
# the newest BUILD of that minor instead. The minor itself is never discovered: PaperMC's
# Fill v3 groups velocity builds by version group, and "newest across all groups" can mean
# an unreleased 4.x SNAPSHOT that needs another runtime. Crossing a major is a deliberate
# code change.
FELIS_VELOCITY_VERSION="${FELIS_VELOCITY_VERSION:-}"
VELOCITY_LATEST_MINOR="3.5.1"
# pinned installs the builds deploy/game-stack.lock names (resolve_game_jars); latest asks
# upstream for its newest ones, which is how that lock file gets refreshed.
FELIS_GAME_STACK="${FELIS_GAME_STACK:-pinned}"
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
# The JRE the proxy runs on is unpacked as root and carries every player session, so the
# default feature version is pinned to one build and the sha256 Adoptium publishes for each
# architecture. Move the three together (the Adoptium API lists them:
# /v3/assets/latest/25/hotspot?image_type=jre&os=linux).
JRE_PINNED_FEATURE="25"
JRE_PINNED_RELEASE="25.0.4.1+1"
JRE_PINNED_SHA256_X64="1731a34baadec5479258ea0202e4d5d865d2efeee60cb0c7d7eb056fe96ca219"
JRE_PINNED_SHA256_AARCH64="34828cbb93ed31c281c84ecb31ddab655d11a802f263c1fc019d42e9e0230fed"
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

STATE_DIR="/etc/felis"
SECRETS_ENV="${STATE_DIR}/secrets.env"
BOOTSTRAP_DONE="${STATE_DIR}/bootstrap.done"
PANEL_TLS_CERT="${STATE_DIR}/panel-tls.crt"
PANEL_TLS_KEY="${STATE_DIR}/panel-tls.key"
SRC_DIR="/opt/felis/src"
HOST_BIN="/usr/local/bin/felis"
# The binary this run replaced (keep_previous_host_binary), and whether the new one has
# been put to use: once migrations start, or the nano service restarts onto it, the old
# one no longer matches what is running and a failed run keeps the new one.
HOST_BIN_PREV=""
HOST_BIN_KEPT=0
HOST_BIN_IN_USE=0
# Felis's own build toolchain, not /usr/local/go: install_go_toolchain replaces whatever
# version sits here, and an operator's Go at the conventional path is not ours to swap.
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
# Where summary_offsite shows a newly generated off-site key: the operator's terminal alone.
# stdout and stderr are what `2>&1 | tee install.log`, cloud-init and CI keep on disk.
OFFSITE_KEY_TTY=/dev/tty
# Where the setup console the installer starts at the end reads its keys (setup_terminal).
SETUP_TTY=/dev/tty
# Set by summary_next when the installer goes on into the setup console.
SETUP_CONSOLE=0
# Host copies of the credentials `felis setup` takes at the keyboard, one bare value per
# file, mode 0600 (cmd/felis/hostcreds.go); apply_setup_credential_secrets applies their
# Secrets from them on every run.
SMTP_PASSWORD_FILE="${STATE_DIR}/smtp-password"
UPLOADS_S3_ACCESS_KEY_FILE="${STATE_DIR}/uploads-s3-access-key"
UPLOADS_S3_SECRET_KEY_FILE="${STATE_DIR}/uploads-s3-secret-key"
OFFSITE_SERVICE="/etc/systemd/system/felis-offsite.service"
OFFSITE_TIMER="/etc/systemd/system/felis-offsite.timer"
BUILD_TOOLS_SERVICE="/etc/systemd/system/felis-build-tools.service"
BUILD_TOOLS_TIMER="/etc/systemd/system/felis-build-tools.timer"
BUILD_TOOLS_STATUS="/var/lib/felis/build-tools/status.json"
# While this marker holds a future Unix time, felis watchdog mails nothing: an install
# restarts the control plane and the system servers on purpose. cleanup removes it; the
# time in it is the backstop for an installer killed before its EXIT trap runs.
WATCHDOG_QUIET_FILE="/run/felis/watchdog-quiet-until"
# FELIS_WATCHDOG_HEARTBEAT_URL, where felis watchdog reads it by default.
WATCHDOG_HEARTBEAT_FILE="${STATE_DIR}/watchdog-heartbeat-url"
VELOCITY_DIR="/opt/felis/velocity"
VELOCITY_USER="felis-velocity"
VELOCITY_SERVICE="/etc/systemd/system/felis-velocity.service"
# What the running proxy was last started from (velocity_fingerprint), and the builds the
# login and lobby pods were last started on: a rerun restarts only what actually changed,
# since each of those restarts disconnects every player online.
VELOCITY_FINGERPRINT="${STATE_DIR}/velocity.fingerprint"
SYSTEM_SERVER_IMAGES="${STATE_DIR}/system-server-images"
# The control-plane database, felis-postgres (internal/platform/postgres.go); these must
# equal the platform's constants (TestBootstrapAgreesWithThePostgresConstants).
PG_DEPLOYMENT="felis-postgres"
PG_CONTAINER="postgres"
PG_SECRET="felis-postgres"
PG_SECRET_KEY="superuser-password"
PG_DATA_DIR="/var/lib/felis/postgres"
PG_UID=999
PG_HOST_PORT=15432
PG_SERVICE_ADDR="${PG_DEPLOYMENT}.${CONTROL_NS}.svc:5432"
# Written once a host PostgreSQL's database has been moved into felis-postgres
# (migrate_host_postgres); the move never runs again after it.
PG_MOVED_MARKER="/var/lib/felis/postgres-moved"
PG_LOCKOUT_LINE="# The felis database moved into k3s (felis-postgres); nothing else may reach this copy."
# The units that write to the database from the host; the move stops them and starts
# again the ones that were running.
PG_MOVE_TIMERS=(felis-db-backup felis-offsite felis-update-check felis-watchdog)
# Each public table with its exact row count, one per line, in byte order of the table names
# whatever the servers' collations (relname is a name, which sorts in collation "C" since
# PostgreSQL 12), so the two servers' answers compare byte for byte. internal/pgint runs it
# on the oldest and newest server the installer meets; the shell tests stub psql.
PG_TABLE_COUNTS="SELECT c.relname || ' ' || (xpath('/row/n/text()', query_to_xml(format('SELECT count(*) AS n FROM public.%I', c.relname), false, true, '')))[1]::text FROM pg_class c JOIN pg_namespace s ON s.oid = c.relnamespace WHERE s.nspname = 'public' AND c.relkind IN ('r', 'p') ORDER BY c.relname"
PG_MOVE_STAGE=""
PG_MOVE_HBA=""
PG_MOVE_UNITS=()
# The nftables rules an earlier release kept a host PostgreSQL's port behind; the move
# retires them with the server.
PG_FIREWALL_RULES="${STATE_DIR}/postgres-firewall.nft"
PG_FIREWALL_SERVICE="/etc/systemd/system/felis-postgres-firewall.service"
JRE_DIR="/opt/felis/jre"
# The gradle image the lobby and limbo Dockerfiles build their plugins in, digest
# included; build_velocity_plugin runs the same one.
PLUGIN_BUILD_IMAGE="gradle:9.8.0-jdk25@sha256:2b2fc1b1dfc3604a2acc916839f36eb5ee48fd7f232427fc5faca224c73bcb01"
K3S_BIN_DIR="${K3S_BIN_DIR:-/usr/local/bin}"
K3S_BIN="${K3S_BIN_DIR}/k3s"
# k3s's containerd mirror config, written by configure_registry_mirror. A variable
# (not just the literal path) so bootstrap_test.sh can point the writer at a
# scratch file.
K3S_REGISTRIES_FILE="/etc/rancher/k3s/registries.yaml"
# The installer's k3s settings (write_k3s_config), a drop-in k3s reads after any
# config.yaml the operator keeps. The unit, the admin kubeconfig and the kubelet's
# client certificate are variables for the same reason as the file above.
K3S_CONFIG_DROPIN="/etc/rancher/k3s/config.yaml.d/50-felis.yaml"
K3S_UNIT_FILE="/etc/systemd/system/k3s.service"
# The installer's environment for the k3s service (write_k3s_service_dropin); k3s's own
# installer rewrites the unit and its .env file, never this.
K3S_SERVICE_DROPIN="/etc/systemd/system/k3s.service.d/50-felis.conf"
K3S_KUBECONFIG="/etc/rancher/k3s/k3s.yaml"
K3S_KUBELET_CERT="/var/lib/rancher/k3s/agent/client-kubelet.crt"
# Where k3s imports image tarballs from as it starts (stage_k3s_airgap_images).
K3S_IMAGES_DIR="/var/lib/rancher/k3s/agent/images"
# ensure_persistent_journal's drop-in, and the directory journald creates once it
# stores the journal persistently.
JOURNALD_DROPIN="/etc/systemd/journald.conf.d/50-felis.conf"
JOURNAL_DIR="/var/log/journal"
# dpkg's journal of a run in progress: not empty after a dpkg run was cut off
# (finish_interrupted_dpkg).
DPKG_UPDATES_DIR="/var/lib/dpkg/updates"
# Held for the whole run (acquire_run_lock), so a second installer started while one is
# still going stops at once instead of working the same files and cluster beside it.
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

# ---------------------------------------------------------------------------
# Clean PATH (sudo may strip /usr/local/bin)
# ---------------------------------------------------------------------------
PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
export PATH

# BuildKit attaches a provenance attestation to every build by default, and it records the
# build's start time. On Docker's containerd image store the image id is the digest of the
# index that carries it, so an unchanged rebuild would get a new id each run: the login and
# lobby pods would restart on every rerun, and each run would push another versioned tag.
# Without it the id is the manifest digest, which an all-cached build reproduces.
export BUILDX_NO_DEFAULT_ATTESTATIONS=1

# ---------------------------------------------------------------------------
# Logging
# ---------------------------------------------------------------------------
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

# keep_previous_host_binary copies the felis binary this run is about to replace, once. A
# run that fails before the new binary is in use puts it back (restore_previous_host_binary):
# until then the old binary, the old cluster and the unmigrated database still agree, while
# a new binary left behind runs the host timers against a schema it was not built for, and
# `felis setup` with it would migrate the database under the old control plane.
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
# Gives a file the SELinux label its path calls for, on hosts that have SELinux. It
# repairs files earlier installers wrote under /tmp and moved into place, which kept
# user_tmp_t (a confined daemon is then denied them).
restore_label() { if command -v restorecon >/dev/null 2>&1; then restorecon "$1" || true; fi; }

trap 'on_error "$LINENO" "$?"' ERR
trap cleanup EXIT

k3s_cmd() { [ -x "$K3S_BIN" ] || die "k3s binary not found at ${K3S_BIN}"; "$K3S_BIN" "$@"; }

# acquire_run_lock takes RUN_LOCK_FILE for the rest of the run. A rerun started while an
# earlier one still runs (in tmux after the SSH session dropped, in a second terminal)
# would otherwise install the same packages, rewrite the same files and apply the same
# cluster objects beside it. The lock goes with the process, however it ends.
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

# The control plane mounts felis-config from its own namespace; the workload
# namespace's backup/restore/fileedit Jobs and the reaper mount a local copy (a
# secretKeyRef is namespace-local). The installer owns the rendered config, so both
# copies are (re)applied on every run — unlike the create-if-absent credential
# replicas `felis setup` makes, because a stale config copy keeps an old database URL
# or archive policy after an upgrade or a credential rotation.
apply_felis_config_secrets() {
  kube -n "$CONTROL_NS" create secret generic felis-config \
    --from-file=felis.toml="${STATE_DIR}/felis.pod.toml" \
    --dry-run=client -o yaml | kube apply -f -
  kube -n "$MINECRAFT_NS" create secret generic felis-config \
    --from-file=felis.toml="${STATE_DIR}/felis.pod.toml" \
    --dry-run=client -o yaml | kube apply -f -
}

# The registry gate reads one token file per principal from felis-registry-auth
# (registry namespace = control namespace); a build Job's push container reads the
# build principal's username/password from felis-registry-push in the build
# namespace. Values go through 0600 temp files, never kubectl's argv.
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

# secret_key_to_file keeps one key of a Secret in path, mode 0600, when path does not
# exist yet. An install from before the host copies had the setup screens' credentials
# only in the cluster; this is the run that moves them onto the host. A missing Secret
# leaves path missing. The value goes from kubectl into the file, never into argv or
# the log.
secret_key_to_file() {
  local namespace="$1" name="$2" key="$3" path="$4" encoded tmp
  [ -e "$path" ] && return 0
  encoded="$(kube -n "$namespace" get secret "$name" -o "jsonpath={.data.${key}}" 2>/dev/null)" || return 0
  tmp="$(umask 077; mktemp "${path}.XXXXXX")"
  remember_temp "$tmp"
  printf '%s' "$encoded" | base64 -d > "$tmp"
  mv -f -- "$tmp" "$path"
}

# write_file_atomic path mode < content: path ends up holding all of content, or is
# left as it was. A crash or a full disk halfway through a plain `cat >` leaves a
# truncated file that the next run takes as the truth: a secrets.env cut short mints
# passwords the database does not know, an offsite.env cut short a key that cannot
# read the bucket. The temp file sits beside path, so mv is a rename on one
# filesystem; mktemp makes it 0600 before anything is in it, it is on the disk
# before the rename, and the directory is synced after it, so the new name survives
# a power cut too.
write_file_atomic() {
  local path="$1" mode="$2" tmp
  tmp="$(mktemp "${path}.XXXXXX")" || die "could not create a temporary file beside ${path}"
  remember_temp "$tmp"
  { cat > "$tmp" && chmod "$mode" "$tmp" && sync -- "$tmp"; } \
    || die "could not write ${path}; it is left as it was"
  mv -f -- "$tmp" "$path" || die "could not replace ${path}; it is left as it was"
  sync -- "$(dirname -- "$path")" 2>/dev/null || true
}

# apply_setup_credential_secrets applies the Secrets behind `felis setup`'s email and
# uploads-bucket screens from their host copies: felis-smtp in the control namespace and
# in the workload one (the reaper's warning mails read that copy), felis-uploads-s3 in the
# control namespace. A reinstall that kept /etc/felis, or a host rebuilt from a bundle's
# state/, starts k3s with no Secrets at all; without this, every sign-in code and alert
# would go out without AUTH and the relay would turn it away. The host copy wins over the
# cluster's: `felis setup` writes both, so they differ only after a hand edit of the
# Secret. A relay or bucket whose credentials are on neither side is reported, so the
# operator enters them again before the first code fails.
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
  elif persisted_smtp_block | grep -Eq '^[[:space:]]*username[[:space:]]*=[[:space:]]*"[^"]'; then
    warn "[smtp] signs in with a username, but its password is in neither ${SMTP_PASSWORD_FILE} nor the cluster: sign-in codes and alerts go out without AUTH until it is entered again (sudo felis setup, then e to configure email)"
  fi
  if [ -f "$UPLOADS_S3_ACCESS_KEY_FILE" ] && [ -f "$UPLOADS_S3_SECRET_KEY_FILE" ]; then
    kube -n "$CONTROL_NS" create secret generic felis-uploads-s3 \
      --from-file=access_key_id="$UPLOADS_S3_ACCESS_KEY_FILE" \
      --from-file=secret_access_key="$UPLOADS_S3_SECRET_KEY_FILE" \
      --dry-run=client -o yaml | kube apply -f -
  elif persisted_registry_block | grep -Eq '^[[:space:]]*user_uploads_context[[:space:]]*=[[:space:]]*"[sS]3://'; then
    warn "uploads go to an S3 bucket, but its keys are in neither ${UPLOADS_S3_ACCESS_KEY_FILE} / ${UPLOADS_S3_SECRET_KEY_FILE} nor the cluster: felis-api refuses modpack uploads until they are entered again (sudo felis setup, then s to change storage)"
  fi
}

# node_global_cidrs prints one host-length CIDR per global address on this node.
# Game server egress already excludes every private range; this adds the node's
# public addresses, which would otherwise let a server dial the panel NodePort,
# SSH, or anything else the host serves on them.
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

# NEEDRESTART_SUSPEND keeps Ubuntu's needrestart hook from restarting services after
# the install's own apt runs. It would restart felis-velocity whenever a rerun
# happens to install or upgrade a package (the running JVM maps files the rerun
# replaces), dropping every player on a rerun that changed nothing the proxy runs.
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

# A value without a usable port would reach the firewall and the summary as-is: 8081 opens
# port 8081 while nano binds nothing, 127.0.0.1 prints http://127.0.0.1:127.0.0.1/..., and
# the unit crash-loops either way.
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

# validate_offsite_settings checks the FELIS_OFFSITE_* inputs before anything is
# installed. They are written into felis.toml as TOML strings and the secrets into a
# single-quoted env file, so a quote, a backslash or a line break is refused outright.
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

# validate_heartbeat_url checks FELIS_WATCHDOG_HEARTBEAT_URL before anything is
# installed: a check's http(s) ping URL, or off. The messages leave the URL out: its path
# is the key that pings the check.
validate_heartbeat_url() {
  case "$FELIS_WATCHDOG_HEARTBEAT_URL" in
    "" | off) ;;
    *[[:space:]]* | *\"* | *\'* | *\\*) die "FELIS_WATCHDOG_HEARTBEAT_URL must not contain spaces, quotes or backslashes" ;;
    http://[!/]* | https://[!/]*) ;;
    *) die "FELIS_WATCHDOG_HEARTBEAT_URL must be the http:// or https:// ping URL of a monitoring service's check, or off" ;;
  esac
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
  if [ -z "${NODE_IP:-}" ]; then
    # No IPv4 default route. hostname -I lists the IPv6 addresses too, in interface
    # order, and an IPv6 one would end up in the nip.io name, the /32 policies and the
    # certificate's IP entry: take the first IPv4 one.
    local a
    for a in $(hostname -I 2>/dev/null); do
      if ipv4_to_int "$a" >/dev/null; then NODE_IP="$a"; break; fi
    done
  fi
  [ -n "${NODE_IP:-}" ] || die "could not determine this host's primary IPv4 address"

  # Precedence: an explicit FELIS_ROOT_DOMAIN, then whatever the last run persisted, then
  # the nip.io default. The middle step is what makes a re-run idempotent. Without it this
  # installer re-derived the domain from scratch every time and defaulted to nip.io, so
  # re-running it on a live install -- the only way to move felis-api to a newer release,
  # and what `felis update` points operators at -- rewrote root_domain, panel_hostname and
  # admin_hostname to nip.io names while the write-once panel certificate kept the old
  # ones. Secrets never had this problem: load_or_make_secrets has always sourced
  # secrets.env before generating anything.
  #
  # A different FELIS_ROOT_DOMAIN on an installed host is refused. The name is on more
  # surfaces than this script rewrites -- the panel certificate, the felis-api-tls and
  # felis-config Secrets, the proxy's felis-link.properties, the login gate's CR env, the
  # Cloudflare tunnel -- and a half-moved install serves a certificate for the old names.
  # `felis domain set` moves all of them and `felis domain check` proves each one.
  local persisted
  persisted="$(persisted_root_domain)"
  if [ -n "${FELIS_ROOT_DOMAIN:-}" ] && [ -n "$persisted" ] && [ "$FELIS_ROOT_DOMAIN" != "$persisted" ]; then
    die "FELIS_ROOT_DOMAIN (${FELIS_ROOT_DOMAIN}) differs from the installed ${persisted}. The installer keeps the installed domain; to move the install, rerun it without FELIS_ROOT_DOMAIN, then run: sudo felis domain set ${FELIS_ROOT_DOMAIN} (docs/operations.md, Changing the root domain)"
  fi
  FELIS_ROOT_DOMAIN="${FELIS_ROOT_DOMAIN:-${persisted:-${NODE_IP}.nip.io}}"
  if [ -n "$persisted" ] && [ "$FELIS_ROOT_DOMAIN" = "$persisted" ]; then
    log "node IP: ${NODE_IP}   root domain: ${FELIS_ROOT_DOMAIN} (reusing the installed domain)"
  else
    log "node IP: ${NODE_IP}   root domain: ${FELIS_ROOT_DOMAIN}"
  fi
}

# warn_dynamic_node_ip warns when NODE_IP is a DHCP lease. The address is the k3s node's
# and is written into the network policies, the panel certificate and the default nip.io
# domain, and nothing re-addresses a live install, so a lease that later comes back
# different takes the platform down (the watchdog then reports host-address). `ip -o addr` marks a leased address "dynamic".
warn_dynamic_node_ip() {
  if ip -4 -o addr show 2>/dev/null | awk -v ip="$NODE_IP" '
      { split($4, a, "/"); if (a[1] == ip && / dynamic /) found = 1 }
      END { exit !found }'; then
    warn "${NODE_IP} is a DHCP lease, and the install is bound to this address. Give the host a"
    warn "DHCP reservation or a static address before it changes (docs/operations.md §1)."
  fi
}

# ---------------------------------------------------------------------------
# Preflight: what would stop the install halfway, checked before the host is touched.
# Every problem is collected and reported together, so a host that needs three fixes
# costs one rerun rather than three; warnings print as they are found and never stop
# the run. FELIS_PREFLIGHT=warn turns the problems into warnings too.
# ---------------------------------------------------------------------------
PREFLIGHT_PROBLEMS=()
preflight_fail() { PREFLIGHT_PROBLEMS+=("$*"); }

# The smallest host the full stack runs on: k3s, PostgreSQL, the control plane, the
# registry, the proxy (1G heap by default) and the login and lobby servers. A "2 GB"
# VPS reports ~1.9 GiB once the kernel has taken its share; ensure_swap adds swap below
# 2 GiB. Below the recommendation it runs, with little room for players' own servers.
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

# k3s_adds_selinux_rpm reports whether k3s's install.sh will install k3s-selinux from
# Rancher's RPM repository (rpm.rancher.io), mirroring its setup_selinux and
# install_selinux_rpm: on a host with an SELinux policy directory that is Red Hat-like (one
# of these release files) or names suse first in ID_LIKE. dnf or zypper failing to reach the
# repository fails the k3s install. $1 is a root to read under, for the tests.
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

# release_assets_expected reports whether this run expects to download a release's images and
# Velocity plugin (select_release_artifacts): on the release channel, and from the setup
# console, whose binary is a release's.
release_assets_expected() {
  [ -z "$FELIS_ARTIFACT_DIR" ] || return 1
  bootstrap_from_tui || use_release_binary
}

# host_builds_expected reports whether this run expects to build images here, and so to install
# Docker: a source build does, an install from a release's assets does not, and the game images
# under FELIS_GAME_STACK=latest are always built here, since no release ships that stack. What
# preflight cannot see coming is a release that turns out to carry no usable images (one cut
# before they were published, or still uploading); that one is built here after all.
host_builds_expected() {
  [ "$FELIS_GAME_STACK" != latest ] || return 0
  [ -z "$FELIS_ARTIFACT_DIR" ] || return 1
  ! release_assets_expected
}

# preflight_disk checks each filesystem the install writes to against what it will
# write there, in MiB: k3s's images and volumes, the database and its bundles under
# /var/lib/felis (and a release's image bundles on their way in), the sources, toolchains
# and proxy under /opt/felis, and Docker's image builds (operations.md §2 has the measured
# sizes). A directory that already holds something (a rerun, a reused k3s, Docker's cache
# from an earlier install) needs only the room for what changes.
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

# preflight_networks refuses addresses k3s's pod and service ranges would shadow: the
# node's own address, or a network (a Docker bridge, a LAN, a VPN) routed inside them.
# A wider route that merely contains them, a 10.0.0.0/8 VPN say, keeps working for
# everything outside the two ranges, so it is a warning.
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

# preflight_hosts prints the hosts this run downloads from, one "host required|optional" line
# each. An optional host is one the install only falls back to: Docker Hub when the images are
# expected from a release, and, from the setup console, the release lookup (without it every
# image is built here). Package mirrors are left out: the package manager names its own.
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

# host_reachable reports whether an HTTPS connection to $1 can be made: a TLS handshake that
# completes, which any answer (a 404 included) comes after, and which a server that then sits
# on the request has made too. It tries three times, two seconds apart, as the downloads it
# stands for retry: one dropped probe must not stop an install. Without curl (a minimal
# image, before install_base) a bare TCP connect stands in, unless a proxy is configured,
# which only curl would use.
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

# finish_interrupted_dpkg completes a dpkg run that was cut off: an earlier install killed
# halfway through a package, a reboot during unattended-upgrades. Until then apt-get refuses
# everything with "dpkg was interrupted, you must manually run 'dpkg --configure -a'", so
# each rerun failed exactly as the run before it.
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
    # Arch supports only whole-system upgrades (-Sy alone leaves a partial upgrade), so the
    # refresh stays -Syu, with a host PostgreSQL held back once it has a cluster: a new major
    # version cannot open the old data directory. That cluster is the database an earlier
    # release ran on, which migrate_host_postgres reads to move it into felis-postgres, and
    # afterwards the copy a rollback of that move starts again (docs/operations.md §4).
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

# ensure_time_sync turns NTP on. A drifting clock breaks things far from their cause:
# sign-in codes and sessions expire early or late, S3 refuses off-site uploads signed
# more than 15 minutes off, and certificate checks fail. Rocky's minimal image ships
# chronyd disabled (the test host reported NTP=no), so the installer enables whatever
# timedatectl manages and installs chrony only when there is nothing to enable. It
# waits half a minute for the first synchronization and then carries on: the watchdog
# keeps reporting an unsynchronized clock.
ensure_time_sync() {
  if [ "$FELIS_MANAGE_TIME_SYNC" = 0 ]; then
    log "FELIS_MANAGE_TIME_SYNC=0: leaving time synchronization to the operator"
    return 0
  fi
  if ! command -v timedatectl >/dev/null 2>&1; then
    warn "timedatectl not found; make sure an NTP client keeps this host's clock (docs/operations.md §1)"
    return 0
  fi
  if [ "$(timedatectl show -p NTP --value 2>/dev/null)" != yes ]; then
    # set-ntp fails with "NTP not supported" when no NTP unit is installed at all
    # (Debian's minimal image splits systemd-timesyncd into its own package).
    if ! timedatectl set-ntp true 2>/dev/null; then
      log "no NTP client to enable; installing chrony"
      pkg_install chrony
      if ! timedatectl set-ntp true; then
        warn "could not turn NTP on; set up time synchronization by hand (docs/troubleshooting.md §13c)"
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
  warn "the system clock is not synchronized yet; check 'timedatectl' (docs/troubleshooting.md §13c)"
}

# ensure_persistent_journal keeps the system journal across reboots. Rocky's journald
# stores it under /run unless /var/log/journal exists, and its minimal image does not
# create that directory, so a reboot (the moment an operator most needs to know what
# came before it) erased every log. journald creates JOURNAL_DIR itself once it runs
# with Storage=persistent, so the directory is the proof the drop-in took: journald is
# restarted when the drop-in changed, or when it is current and the directory is still
# missing.
ensure_persistent_journal() {
  if [ "$FELIS_MANAGE_JOURNAL" = 0 ]; then
    log "FELIS_MANAGE_JOURNAL=0: leaving journald as it is"
    return 0
  fi
  local file="$JOURNALD_DROPIN" tmp
  mkdir -p "$(dirname "$file")"
  # Made beside its destination so the file is born with that directory's SELinux
  # label. A file made under /tmp keeps user_tmp_t through the mv, and journald was
  # denied it on the test host ("Failed to open configuration file ... Permission
  # denied"). journald reads only *.conf, so the temporary name is never loaded.
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
  ok "cloudflared installed ($(cloudflared --version | head -n 1))"
  # The running tunnel keeps the old binary mapped until it restarts.
  if [ -n "$current" ] && systemctl is-active --quiet cloudflared-felis 2>/dev/null; then
    systemctl restart cloudflared-felis
    ok "cloudflared-felis restarted onto ${FELIS_CLOUDFLARED_VERSION}"
  fi
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
    # Docker serves this installer's builds and nothing at runtime, so the one it installed
    # does not come up at boot to hold ~200 MiB until the next run; a run that builds starts
    # it (ensure_docker). Some packages enable it, and its containerd, as they install.
    systemctl disable docker.service docker.socket 2>/dev/null || true
    [ -n "$had_containerd" ] || systemctl disable containerd.service 2>/dev/null || true
  fi
  systemctl start docker
  ok "docker running"
}

# ensure_docker installs and starts Docker the first time this run has something to build, and
# starts it again after an earlier step stopped it. Only what a release did not ship prebuilt
# is built here, so an install from a release's assets never installs Docker at all.
ensure_docker() {
  if [ -z "$DOCKER_INSTALLED" ]; then
    host_builds_expected || unplanned_build_room
    install_docker
    DOCKER_INSTALLED=1
  else
    systemctl start docker
  fi
}

# unplanned_build_room runs before the first build of a run whose preflight counted on none:
# a release asset that could not be downloaded or checked (the warnings before it name
# which) is built here instead, with Docker's images and build cache under
# /var/lib/containerd. When that filesystem lacks preflight_disk's room for them the install
# stops here, before Docker is installed, instead of filling the disk halfway through a
# build; FELIS_PREFLIGHT=warn goes on, as it does past preflight's own problems.
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

# stop_docker hands back what Docker holds once a step is done with it, ~150 MiB for the daemon
# and ~45 MiB for its containerd; the next step that builds starts both again. A Docker this run
# never started is left alone, and so is a containerd holding any namespace besides Docker's own
# (moby, moby_history): something else on the host runs on it. k3s's containerd listens on a
# socket of its own and is never the one asked here.
stop_docker() {
  [ -n "$DOCKER_INSTALLED" ] || return 0
  systemctl stop docker docker.socket 2>/dev/null || true
  local ns
  ns="$(ctr --address /run/containerd/containerd.sock namespaces ls -q 2>/dev/null)" || return 0
  if ! printf '%s\n' "$ns" | grep -qvE '^(moby.*)?$'; then
    systemctl stop containerd 2>/dev/null || true
  fi
}

# ---------------------------------------------------------------------------
# 4. k3s — single node, trimmed for RAM. NetworkPolicy stays ENABLED on purpose:
#    Felis's minecraft fence (default-deny + allow-rcon/allow-game) is a core
#    security claim, so we must NOT pass --disable-network-policy.
#    On SUSE-family hosts firewalld ships active by default, and Ubuntu and Debian
#    hosts often enable ufw; open the required rules rather than disabling either.
# ---------------------------------------------------------------------------

# ufw_active reports whether ufw is enabled. Enabled, it drops every inbound packet no rule
# admits, the pods' traffic to the API server among them, so an install that left it alone
# waited out its first rollout and died with "did not complete". ufw translates its status
# line, hence the C locale.
ufw_active() {
  command -v ufw >/dev/null 2>&1 || return 1
  [ "$(LC_ALL=C ufw status 2>/dev/null | head -n 1)" = "Status: active" ]
}

# configure_k3s_firewall opens what k3s needs. For ufw that is what k3s's documentation asks:
# the pod and service ranges, plus the panel's NodePort as firewalld gets it. The API
# server's 6443 stays closed to the network, since pods reach it from POD_CIDR. Each ufw rule
# carries a felis- comment, which is how deploy/uninstall.sh finds it again; `ufw allow`
# skips a rule it already has, so a rerun adds nothing.
configure_k3s_firewall() {
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
    # k3s's installer moves the binary into place before it writes k3s.service, so a run
    # cut short between the two left a k3s nothing starts, and every rerun stopped at
    # `systemctl enable`. Its installer run again around the binary in place finishes the
    # job: no download, no version change, the cluster's data untouched.
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
    log "installing k3s ${FELIS_K3S_VERSION} into ${K3S_BIN_DIR} (no traefik/servicelb/metrics-server)"
    stage_k3s_airgap_images
    run_k3s_installer
    installer_ran=1
  fi

  [ -x "$K3S_BIN" ] || die "k3s installation completed but ${K3S_BIN} is missing"
  strip_k3s_kubeconfig_mode_flag

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

# k3s_node_name prints the name this node must keep. Every local-path volume (worlds,
# registry, uploads, backups) is bound to its node by name, and k3s takes the name from
# the hostname on every start, so a renamed host came back as a second, empty node with
# every volume stuck Pending on the old one. Precedence: the name already pinned; else
# the name the node registered under, which its kubelet client certificate carries as
# system:node:<name> and which is readable with k3s stopped; else, where k3s has never
# run, the lowercased hostname k3s itself would pick. It prints nothing when k3s has run
# but neither source can be read: pinning a guess there would rename the node.
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

# write_k3s_config writes the installer's k3s settings to K3S_CONFIG_DROPIN and sets
# K3S_RESTART_NEEDED when they changed, since k3s reads the file only as it starts.
# write-kubeconfig-mode keeps the admin kubeconfig root-only: it is cluster-admin, and
# installs before this passed 644, which let every local account read it, felis-velocity
# (the account the internet-facing proxy runs as) included.
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

# write_k3s_service_dropin runs k3s, and the containerd it starts with its own environment,
# with the Go collector at half the default heap growth (GOGC=50). An idle k3s holds about
# 150 MiB live and by default lets its heap reach twice that before collecting; at 50 it
# collects at one and a half times. Measured on the verification host: k3s 430 -> 370 MiB and
# its containerd 114 -> 104 MiB, for about 2% of one core more while idle. It sets
# K3S_RESTART_NEEDED when the file changed, since k3s reads its environment only as it starts.
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

# A command-line flag outranks every config file, and k3s's installer writes
# INSTALL_K3S_EXEC into the unit's ExecStart one quoted word per line, so installs from
# before the drop-in keep "'--write-kubeconfig-mode' \" followed by "'644' \" there.
# This drops both lines (or the single --write-kubeconfig-mode=<mode> spelling). An
# upgrade through run_k3s_installer rewrites the unit without them anyway.
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

# The script from the release's own tag rather than get.k3s.io, which serves whatever
# master holds today. '+' is literal in a URL path, so the tag needs no escaping. On an
# installed k3s the same script replaces the binary in place and restarts the service.
# --keep-binary keeps the k3s already at K3S_BIN (INSTALL_K3S_SKIP_DOWNLOAD=binary) and
# still does everything after it, the SELinux policy included.
run_k3s_installer() { # [--keep-binary]
  local skip=""
  [ "${1:-}" != --keep-binary ] || skip=binary
  curl -sfL --retry 5 --retry-delay 2 "https://raw.githubusercontent.com/k3s-io/k3s/${FELIS_K3S_VERSION}/install.sh" | \
    INSTALL_K3S_SKIP_DOWNLOAD="$skip" \
    INSTALL_K3S_VERSION="$FELIS_K3S_VERSION" \
    INSTALL_K3S_BIN_DIR="$K3S_BIN_DIR" \
    INSTALL_K3S_EXEC="--disable traefik --disable servicelb --disable metrics-server" \
    sh -
}

# stage_k3s_airgap_images puts the image tarball of the k3s release about to be installed where
# k3s imports images from as it starts, so k3s's own images (pause, CoreDNS, the local-path
# provisioner) come from that GitHub release, checked against its sha256sum file, instead of
# from Docker Hub, whose anonymous pull limit (10 an hour per address) a shared VPS address may
# have spent already. Best-effort: without the file k3s pulls them as it always has. k3s only
# reads names ending in a tarball extension, so the download's dotted temp name is never read.
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

# k3s_upgrade_allowed decides whether an installed k3s ($1) may move to $2. Kubernetes
# supports upgrading one minor version at a time, so a larger jump stops the install
# before anything changed; a newer installed k3s is left as it is.
k3s_upgrade_allowed() {
  local current="$1" want="$2" cur_major cur_minor want_major want_minor rest
  IFS=. read -r cur_major cur_minor rest <<<"${current#v}"
  IFS=. read -r want_major want_minor rest <<<"${want#v}"
  case "${cur_major}${cur_minor}${want_major}${want_minor}" in
    ""|*[!0-9]*) die "cannot compare the installed k3s '${current}' with ${want}; upgrade it by hand (docs/operations.md §4)" ;;
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

# Waits for the (single) node to report Ready. Shared by the k3s install and the
# registry-mirror restart below: both restart the agent, and a bootstrap that
# proceeds early fails later with a misleading "not found"/timeout instead.
wait_for_node_ready() {
  local _
  for _ in $(seq 1 60); do
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
# 4b. The in-cluster registry: the node-side pull path, the registry's own
#     image, and the hosting of every image this installer builds.
#
#     Kubelet's image GC collects an unused image under disk pressure (drilled:
#     the game images were collected and ImagePullBackOff had nothing to pull
#     from). The fix is a pull source that is always there — the registry the
#     bundle already renders. Two node-level facts make that work:
#       * kubelet cannot reach the registry Service VIP (the live stack answered
#         "Empty reply"), so containerd is told to go through the loopback
#         hostPort the registry Deployment binds (the Deployment renders it) —
#         that is configure_registry_mirror below;
#       * the registry's own image (REGISTRY_IMAGE) must already be in containerd
#         before the registry Deployment can start at all —
#         import_platform_images below caches it.
#     After deploy_bundle, push_images_to_registry mirrors the built images into
#     the registry, so containerd's imported copies are a first-boot cache
#     rather than the only copy.
# ---------------------------------------------------------------------------

# The node's containerd cannot dial the registry Service VIP, so pulls arrive
# over the loopback hostPort the registry Deployment binds. k3s reads this file
# when the agent starts and regenerates containerd's certs.d from it — no
# restart, no effect — so a CONTENT change restarts k3s; an identical file
# (every re-run) restarts nothing. K3S_REGISTRIES_FILE is a variable so
# bootstrap_test.sh can point the function at a scratch file.
configure_registry_mirror() {
  local file="$K3S_REGISTRIES_FILE" tmp
  tmp="$(mktemp)"
  remember_temp "$tmp"
  cat > "$tmp" <<EOF
mirrors:
  "${REGISTRY_URL}":
    endpoint:
      - "http://${REGISTRY_PUSH_HOST}"
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

# Two platform images come from Docker Hub by digest, never from the platform's own
# registry: REGISTRY_IMAGE, which is that registry, and POSTGRES_IMAGE, the database
# everything else waits for. They must equal the renderer's defaults
# (platform.defaultRegistryImage / defaultPostgresImage). On a box that cannot reach
# Docker Hub neither Deployment can ever start without a local copy, so the installer
# caches one whenever it can. Best-effort by design: if a pull fails the rollout still
# fails loudly, with the regular diagnostics, but for every box that CAN pull, each
# image is fetched exactly once, here, instead of at first pod start.
#
# crictl pulls through CRI, the same call kubelet makes, so containerd records the
# digest ref the Deployment names and kubelet finds it. A docker save/import round
# trip rewrites the manifest and would leave a copy the digest ref never matches.
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
      warn "could not pull ${image}: its Deployment starts only if the node can pull it from Docker Hub; on an air-gapped box import it by hand (docs/troubleshooting.md §8e)"
    fi
  done
}

# pinned_image_ref <image> prints the name containerd lists a digest-pinned image under
# once CRI has pulled it: the repository and the digest, with the tag dropped.
pinned_image_ref() {
  printf '%s@%s\n' "${1%%:*}" "${1#*@}"
}

# The registry pod runs REGISTRY_IMAGE and, as its registry-gate sidecar, the felis
# image — neither of which can be pulled from the registry they make up — and the
# database pod runs POSTGRES_IMAGE. A kubelet image GC that collected any of them
# would leave that pod dead until someone re-imported by hand. containerd reports an
# image labelled io.cri-containerd.pinned=pinned as pinned over CRI, and kubelet's
# image GC never removes a pinned image. Older felis/felis tags and older registry and
# postgres images are unpinned first, so upgrades do not pile up pinned images forever.
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
      warn "could not pin ${ref} in containerd: if the kubelet's image GC collects it, its pod cannot restart until it is re-imported (docs/troubleshooting.md §8e)"
    fi
  done
}

# pin_user_server_images fixes every user server still naming a tag in the platform
# registry (felis/paper:demo) to the digest that tag names now. It has to run before
# build_game_stack and push_images_to_registry put new builds under those tags: a
# server left on the bare tag would boot the new build on its next wake and open its
# world with a newer Minecraft version, and chunk upgrades cannot be undone. felis-api
# pins every server it creates; this catches the ones created before it did. A fresh
# install has no CRD, so nothing to pin; a running server restarts once onto the
# build it already runs.
pin_user_server_images() {
  kube get crd minecraftservers.felis.lolicon.best >/dev/null 2>&1 || return 0
  # The registry answers the lookups, and a k3s restart above may have left its pod
  # still starting. A registry that never comes up fails the lookups below, loudly.
  kube -n "$CONTROL_NS" rollout status deployment/registry --timeout=180s >/dev/null 2>&1 || true
  if "$HOST_BIN" pin-images --namespace "$MINECRAFT_NS" \
      --registry "$REGISTRY_URL" --endpoint "$REGISTRY_PUSH_HOST"; then
    ok "user servers pinned to the builds they run"
  else
    warn "could not pin every user server listed above to its current build: each one still names a tag this run is about to point at a new build, so its next start may open its world with a newer Minecraft version. Pin it before starting it again: sudo felis pin-images (docs/troubleshooting.md §15b)"
  fi
}

# ---------------------------------------------------------------------------
# 5. Source/binary + image build + containerd import
# ---------------------------------------------------------------------------
# repo_slug prints the "owner/name" of FELIS_REPO_URL, for the REST API.
repo_slug() {
  printf '%s\n' "$FELIS_REPO_URL" | sed -e 's#^.*github\.com[:/]##' -e 's#\.git$##'
}

# fork_token_hint is what a failed GitHub fetch says about FELIS_GITHUB_TOKEN. The official
# repository is public and needs none, so the sentence appears only for a fork, where GitHub
# answers a private repository the caller cannot see with 404, as it does a missing one.
fork_token_hint() {
  [ "$(repo_slug | tr '[:upper:]' '[:lower:]')" != "felismc/felis" ] || return 0
  printf ' If %s is a private fork, set FELIS_GITHUB_TOKEN to a token with read access to it.' "$FELIS_REPO_URL"
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

# load_release_json keeps release tag $1's JSON in RELEASE_JSON, fetching it only for a tag it
# does not hold yet.
load_release_json() {
  [ "$RELEASE_JSON_TAG" != "$1" ] || return 0
  RELEASE_JSON="$(github_api "repos/$(repo_slug)/releases/tags/$1")" || return 1
  RELEASE_JSON_TAG="$1"
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
  local tag="$1" name="$2" id
  # Fetch first, filter second — the SIGPIPE reason documented on resolve_latest_game_jars.
  load_release_json "$tag" || return 1
  id="$(printf '%s' "$RELEASE_JSON" | tr -d '\n' | tr '{' '\n' \
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
#
# curl's --retry covers transient HTTP statuses and timeouts, and --speed-limit makes a
# transfer stalled under 1 KiB/s for a minute one of those. A connection reset mid-stream
# is outside its retry set, and on an image bundle of several hundred MiB it is the likely
# failure, so the outer loop tries the whole download twice more (meta_get's reasoning):
# what the caller falls back to is a build on this host that preflight may not have
# counted on (ensure_docker checks the room for it).
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

# verify_release_checksum checks the downloaded asset $2 (at $3) against the SHA256SUMS
# file release.yml publishes next to it on tag $1. It returns non-zero, with a warning,
# when the release has no SHA256SUMS (a tag cut before release.yml wrote one, or a release
# still uploading), when the file does not list the asset, or when the hash differs.
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

# use_release_binary reports whether this run should install a prebuilt binary instead of
# compiling one. Only the plain release channel qualifies: FELIS_SKIP_FETCH means "build
# exactly what I staged" and a pinned FELIS_REF means "build that tree", both of which are
# explicit requests for a source build, and the dev channel has no release to download.
use_release_binary() {
  [ -z "${FELIS_SKIP_FETCH:-}" ] || return 1
  [ -z "$FELIS_REF_PINNED" ] || return 1
  [ "$FELIS_VERSION_BOOTSTRAP" = "release" ]
}

# download_release_binary installs the prebuilt felis binary for $FELIS_REF onto the host and
# sets HAVE_PREBUILT_BINARY. It leaves HAVE_PREBUILT_BINARY empty, and returns 0, to ask the
# caller to build that same tag from source instead: a release without a usable asset is no
# reason to abort an install. Callers run it as a plain command, so errexit covers the steps
# that replace HOST_BIN. Bash turns errexit off inside a function run as an if or && condition,
# and there an install(1) that failed after the rm below left no binary at all while the
# install went on as if it had one.
#
# One asset covers the panel too: internal/panel/panel.go go:embeds internal/panel/static, and
# release.yml builds through the repo Dockerfile so that tree holds the real npm output rather
# than the tracked placeholder. There is nothing else to fetch.
download_release_binary() {
  local arch asset tmp got
  if ! arch="$(felis_asset_arch)"; then
    warn "no prebuilt felis binary for architecture $(uname -m); building ${FELIS_REF} from source on this host instead"
    return 0
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
    return 0
  fi

  # The hash comes BEFORE the exec below: until the file matches the release's SHA256SUMS it
  # is unverified bytes, and running it as root to ask its version would hand root to
  # whoever could swap the asset or sit on the download path. Missing or unmatched both
  # fall back to the source build of the same tag, which trusts only the git fetch.
  if ! verify_release_checksum "$FELIS_REF" "$asset" "$tmp"; then
    rm -f "$tmp"
    return 0
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
    return 0
  fi

  # install(1) onto a freshly created destination, matching install_embedded_binary. A rename
  # would carry the source SELinux label instead of type-transitioning to bin_t — see
  # build_nano_binary for the 203/EXEC this shape avoids. Same-directory staging does not
  # change that: install(1) still creates the destination and copies.
  keep_previous_host_binary
  rm -f "$HOST_BIN"
  install -m 0755 "$tmp" "$HOST_BIN"
  rm -f "$tmp"
  command -v restorecon >/dev/null 2>&1 && restorecon "$HOST_BIN" >/dev/null 2>&1 || true

  HAVE_PREBUILT_BINARY=1
  ok "installed ${asset} ${FELIS_REF} at ${HOST_BIN}"
}

# ---------------------------------------------------------------------------
# 5a. A release's prebuilt assets: every image and the Velocity plugin, next to the binary
#     (deploy/build-release-artifacts.sh writes them, release.yml publishes them). Each file
#     is used only once its sha256 matches the release's SHA256SUMS.
# ---------------------------------------------------------------------------

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

# artifact_sum <name> prints the sha256 SHA256SUMS lists for <name>, or nothing. sha256sum's
# text-mode line is "<hash>  <name>", binary mode "<hash> *<name>". No regex interval here:
# Debian's mawk does not take one.
artifact_sum() {
  awk -v n="$1" '($2 == n || $2 == "*" n) && length($1) == 64 && $1 !~ /[^0-9a-f]/ { print $1; exit }' <<<"$ARTIFACT_SUMS"
}

# artifact_fetch <name> points ARTIFACT_FILE at a local copy of artifact <name> whose sha256 is
# the one SHA256SUMS lists: the directory's own file, or a download kept in ARTIFACT_CACHE (a
# copy already there that still matches is used as it is, so a rerun after a failure fetches
# nothing twice). It warns and fails, leaving nothing half-written behind, when SHA256SUMS
# does not list the name, the file is missing, or its bytes differ.
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

# artifact_unusable <problem> <fallback> handles an asset this run cannot use. With
# FELIS_ARTIFACT_DIR it stops the install: the operator named the directory so that nothing
# would be built or pulled here. For a downloaded release it warns, and <fallback> (that piece
# built on this host, or pulled) takes its place.
artifact_unusable() {
  [ "$ARTIFACT_MODE" != dir ] || die "FELIS_ARTIFACT_DIR: $1"
  warn "$1; $2"
}

# install_artifact_binary installs FELIS_ARTIFACT_DIR's felis binary, and never falls back to a
# build: a missing or mismatched file is for the operator to fix. The copy is staged next to
# HOST_BIN for download_release_binary's reason (the directory, like /tmp, may be mounted
# noexec), and hashed there, after the copy, so the bytes checked are the bytes run. Its
# version names the control-plane image and the release the rest of the directory must be.
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
  got="$("$tmp" version 2>/dev/null | head -n 1 || true)"
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

# select_release_artifacts decides, once the felis binary is on the host, where this run's
# images and Velocity plugin come from: FELIS_ARTIFACT_DIR, else the assets of the release the
# binary is (the one just downloaded, or the one the setup console runs). A source build
# builds them too, and so does a release without SHA256SUMS (cut before release.yml wrote
# one, or still uploading), since nothing of it could be checked.
select_release_artifacts() {
  local v
  [ "$ARTIFACT_MODE" != dir ] || return 0
  [ -n "$HAVE_PREBUILT_BINARY" ] || return 0
  v="$("$HOST_BIN" version 2>/dev/null | head -n 1 || true)"
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
#
# GIT_TERMINAL_PROMPT=0 on both arms: GitHub answers a private repo with no or a bad token
# by asking for credentials, and git would put that prompt on /dev/tty, where a piped
# install sits waiting instead of failing with the FELIS_GITHUB_TOKEN hint.
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
        # A named release installs from its own assets, as the newest would: the way back
        # to an earlier release (docs/troubleshooting.md §16). validate_settings checked
        # the tag's form; this checks it was published.
        load_release_json "$FELIS_RELEASE" || die "could not find the published Felis release ${FELIS_RELEASE}.
  Check the tag against the repository's releases page.$(fork_token_hint)"
        FELIS_REF="$FELIS_RELEASE"
      else
        log "resolving the newest published Felis release"
        FELIS_REF="$(github_latest_tag)" || die "could not resolve the newest Felis release from api.github.com.$(fork_token_hint)
  If no release has been published yet, set FELIS_VERSION_BOOTSTRAP=dev to build main instead."
      fi
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
# `git describe` is also unreliable here: the primary fetch is --depth 1 and carries no
# tags, so describe falls back to a bare SHA, which updates.Parse rejects outright (it
# fails closed on a non-numeric core). checkout_ref does fall back to the whole history
# when the shallow fetch fails, which WOULD carry tags — that is exactly the point: the
# stamp must not depend on which arm happened to win. rev-parse needs no history at all.
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
    # Whatever sits at SRC_DIR without a .git (a tree staged for FELIS_SKIP_FETCH, the
    # remains of an interrupted clone) is replaced, and only once the new checkout is
    # whole: git clone refuses a non-empty destination, which stopped the rerun outright.
    log "cloning ${FELIS_REPO_URL} (${FELIS_REF})"
    mkdir -p "$(dirname "$SRC_DIR")"
    # A run killed outright (no EXIT trap) leaves its half-made checkout; the run lock
    # means none of these belongs to a run still going.
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

# checkout_ref checks FELIS_REF out in the repository at $1, whose origin is FELIS_REPO_URL.
# A branch, a tag or a full commit id comes down at depth 1. An abbreviated commit id is no
# ref a server answers for, so it falls back to the whole history and is resolved there,
# with origin's branch ahead of a local one an earlier clone left behind. The ref is always
# checked out explicitly: a build stamped v1.2.3 that actually holds main is worse than a
# failed install.
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
  keep_previous_host_binary
  docker cp "${cid}:/usr/local/bin/felis" "$HOST_BIN"
  docker rm "$cid" >/dev/null
  chmod 0755 "$HOST_BIN"
}

# resolve_felis_image names the control-plane image after the release it carries
# (registry.felis.svc:5000/felis/felis:v1.2.3) unless FELIS_IMAGE was given. One tag per
# release is what makes `kubectl rollout undo` a rollback: the previous ReplicaSet names the
# previous tag, and the registry keeps the newest five of them (its pruner, §9). Under one
# mutable tag the undo re-created the pods on the image the upgrade had just written over it.
#
# The version is the release tag on the download path, the stamp on a source build
# (v1.2.3+gabc1234 becomes the tag v1.2.3-gabc1234: '+' is not allowed in a tag), and the
# binary's own report on the setup-console path, which skips both. A rerun of the same version
# reuses its tag; deploy_bundle restarts the pods onto the rebuilt image then.
resolve_felis_image() {
  local v
  [ -z "$FELIS_IMAGE" ] || return 0
  v="$FELIS_VERSION"
  if [ -z "$v" ] && [ -n "$HAVE_PREBUILT_BINARY" ]; then
    v="$("$HOST_BIN" version 2>/dev/null | head -n 1 || true)"
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

# image_present <ctr-images-ls> <ref> <manifest-digest> reports whether the listing holds <ref>
# (its header row starts with REF, which no image is named). A tag has to name
# <manifest-digest> too, since a tag moves; a digest name is the content it names, whether a
# bundle imported the platform manifest under it or CRI pulled the whole index.
image_present() {
  case "$2" in
    *@*) awk -v r="$2" '$1 == r { f = 1 } END { exit !f }' <<<"$1" ;;
    *) awk -v r="$2" -v d="$3" '$1 == r && $3 == d { f = 1 } END { exit !f }' <<<"$1" ;;
  esac
}

# load_release_listing <file> <arch> keeps the image listing's lines in RELEASE_LISTING once every
# line is one bundle of this architecture, a role Felis knows once, two sha256 digests and a
# name made of image-reference characters. A single line that is not is reason enough to trust
# none: the listing is what decides which tar each image is read from.
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

# import_release_images <role>... puts each role's image into k3s containerd from the release's
# bundles, and records it as prebuilt: build_image and build_game_stack skip it, and
# push_images_to_registry pushes it from its bundle. Only the bundles holding an image
# containerd lacks are fetched and imported, so a rerun, or an upgrade that changed only the
# control plane, downloads just that. A bundle names each image by the name the installer
# runs it under by default; a FELIS_*_IMAGE set to another name gets that name as a second tag
# on the same image. A role the release cannot supply falls back to role_fallback, one image
# at a time: the rest still install as published.
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

# meta_get prints a small metadata document. curl's --retry covers transient HTTP
# statuses and timeouts; a TLS handshake cut mid-way (exit 35, seen against Fill over
# a flaky IPv6 path) is outside its retry set, so the outer loop retries every failure
# twice more. --retry-all-errors would say the same but needs curl 7.71+.
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

# resolve_game_jars sets the artifacts the three game images and the proxy are built from:
# the builds deploy/game-stack.lock names (FELIS_GAME_STACK=pinned, the default), or
# upstream's newest ones (latest). Pinned is what makes an install reproducible: every host
# installing one release gets the same login gate, lobby and proxy, each download is
# checked against the lock's sha256, and a rerun's docker builds hit their cache, so
# restart_existing_system_servers leaves the login and lobby pods running.
resolve_game_jars() {
  if [ "$FELIS_GAME_STACK" = "latest" ]; then
    resolve_latest_game_jars
    return 0
  fi
  load_game_stack_lock "${GAME_STACK_DIR}/deploy/game-stack.lock"
  ok "Limbo ${LIMBO_VERSION} + Paper on Minecraft ${MC_VERSION}, LuckPerms and Velocity ${VELOCITY_VERSION}: the builds game-stack.lock pins"
}

GAME_STACK_LOCK_KEYS="MC_VERSION LIMBO_VERSION LIMBO_JAR_URL LIMBO_JAR_SHA256 LIMBO_SCHEM_URL LIMBO_SCHEM_SHA256 PAPER_JAR_URL PAPER_JAR_SHA256 LUCKPERMS_JAR_URL LUCKPERMS_JAR_SHA256 VELOCITY_VERSION VELOCITY_JAR_URL VELOCITY_JAR_SHA256"

# load_game_stack_lock reads the lock's KEY=value lines into the globals of the same names.
# It never sources the file: only the keys above are accepted, every one has to be set, the
# values are limited to URL and version characters (they reach docker build-args), and each
# *_SHA256 has to be a sha256.
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

# resolve_latest_game_jars pins Limbo and Paper to the SAME Minecraft version. LOOHP/Limbo
# speaks exactly one protocol per build, so the login gate dictates the version and Paper
# follows — a client that can pass the gate must also be able to reach the lobby.
# MC_VERSION is read off Limbo's CI artifact name (Limbo-<limbo-ver>-<mc-ver>.jar), which
# is the only place the pairing is published.
#
# Limbo's CI and LuckPerms publish no digest, so latest hashes their downloads as it finds
# them: the image builds still check that they receive those bytes, but nothing vouches for
# the bytes themselves. That is what the lock file adds.
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

# luckperms_latest_jar prints the download URL of the current LuckPerms Bukkit build.
# Bukkit, not bukkit-legacy: legacy targets Minecraft 1.8-1.12, and Paper 26.2 is far
# past that. The same fetch-then-grep shape (and --retry rationale) as
# papermc_latest_jar; the metadata endpoint hands back every platform's URL at once, so
# the grep has to pin the /bukkit/ path segment or it would just as happily return the
# Fabric or Velocity jar, neither of which Paper can load.
luckperms_latest_jar() {
  local json url
  json="$(meta_get "https://metadata.luckperms.net/data/all")" || return 1
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
  # stack is resolved on this host at install time, so it is built here.
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
      docker build -f "${GAME_STACK_DIR}/deploy/limbo/Dockerfile" \
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
      docker build -f "${GAME_STACK_DIR}/deploy/lobby/Dockerfile" \
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
      docker build -f "${GAME_STACK_DIR}/deploy/paper/Dockerfile" \
        --build-arg PAPER_JAR_URL="$PAPER_JAR_URL" \
        --build-arg PAPER_JAR_SHA256="$PAPER_JAR_SHA256" \
        -t "$img" "$GAME_STACK_DIR"
      ;;
  esac

  # The builds the system servers run, for restart_existing_system_servers. Docker's layer
  # cache gives an unchanged build the same id, so a rerun that rebuilt nothing leaves the
  # login and lobby pods (and every player on them) alone.
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

# install_if_changed is atomic_install_file that leaves the target alone when it already
# has the same bytes, owner and mode, so its mtime keeps meaning "the content changed".
# felis domain check reads a proxy started before felis-link.properties' mtime as one
# still on the old names, and a re-run that rewrote the same bytes made every install look
# behind (and `felis domain set` restart the proxy for nothing).
# It never fixes a target in place: the proxy's account owns these directories and can
# swap the file for a symlink after the checks, and a chown or chmod by path would follow
# it to, say, k3s.yaml. Owner and group compare by name, as the callers pass them.
install_if_changed() {
  local source="$1" target="$2" mode="$3" owner="$4" group="$5"
  if [ -f "$target" ] && [ ! -L "$target" ] && cmp -s "$source" "$target" \
    && [ "$(stat -c '%U:%G %a' "$target")" = "${owner}:${group} ${mode#0}" ]; then
    return 0
  fi
  atomic_install_file "$@"
}

# build_velocity_plugin compiles plugins/velocity in the same gradle image the two
# Dockerfiles use, and drops the jar where Velocity will look for it. Docker is the
# toolchain here on purpose: the host needs no JDK and no gradle, only a JRE. Gradle
# checks every dependency against plugins/velocity/gradle/verification-metadata.xml.
build_velocity_plugin() {
  log "building felis-velocity.jar (gradle in a container; the host gets no JDK)"
  prepare_velocity_layout
  # :z relabels the bind mount for SELinux (Fedora/EL enforce it; elsewhere it is a no-op).
  docker run --rm \
    -v "${GAME_STACK_DIR}:/src:z" \
    -w /src/plugins/velocity \
    "$PLUGIN_BUILD_IMAGE" gradle --no-daemon clean build \
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
# Pin the three jars as one compatible set. ViaVersion/ViaBackwards 5.12.0 add
# the 26.3 protocol used by game-stack.lock; ViaRewind 4.2.0 explicitly supports
# that pair. The earlier FL-007 join measured 1.8 against Paper 1.21.11, not every
# client on 26.3. Release notes:
# https://github.com/ViaVersion/ViaBackwards/releases/tag/5.12.0
# https://github.com/ViaVersion/ViaRewind/releases/tag/4.2.0
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

  # A rerun moves the installer's own JRE to the pinned build, which is how a JRE security
  # release reaches the proxy: bump the pin, rerun, and install_velocity_service restarts
  # the proxy because the JRE's release file changed. A JRE someone else put here (another
  # vendor, or pre-staged for an architecture Temurin does not build) is left alone.
  if [ -x "${JRE_DIR}/bin/java" ]; then
    if grep -qxF "IMPLEMENTOR_VERSION=\"Temurin-${release}\"" "${JRE_DIR}/release" 2>/dev/null; then
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
  # The same gate the Via plugins and the fork jar pass: this jar is the proxy every
  # player connects through, and the lock file (or Fill's content-addressed URL) names its
  # digest — a truncated or tampered download becomes a refusal here, not a proxy that
  # won't boot.
  have="$(sha256sum <"$tmp" | cut -d' ' -f1)"
  [ "$have" = "$want" ] \
    || die "Velocity ${version} checksum mismatch: got ${have}, expected ${want}"
  atomic_install_file "$tmp" "${VELOCITY_DIR}/velocity.jar" 0644 root root
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
  local api_ip tmp panel_host admin_host
  api_ip="$(felis_internal_ip)"
  panel_host="$(auth_hostname panel_hostname "console.${FELIS_ROOT_DOMAIN}")"
  admin_host="$(auth_hostname admin_hostname "op.console.${FELIS_ROOT_DOMAIN}")"

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
  # Point Velocity (-Dmojang.sessionserver) at the felis-api hasJoined multiplexer so a
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
  # This value is the floor of the list. The felis-velocity plugin adds every server whose
  # MinecraftServer CR is labelled felis.lolicon.best/forwarding=legacy by rewriting the same
  # property on each server-list refresh (LegacyForwarding.java), and drops it again when the
  # label goes; the floor always stays in. A fork carrying patch 0004 re-reads the property on
  # every backend connection, so a label applies from the next connection. A fork with 0003
  # alone reads it once, after the plugin's first refresh, so a label applies at the next proxy
  # restart. Stock Velocity ignores it, and the plugin logs a warning for a labelled server.
  #
  # The -D below is double-quoted in ExecStart on purpose. The fork trims each element, so it
  # accepts "legacy18, legacy112", but systemd splits ExecStart on whitespace before java ever
  # sees it -- unquoted, that spelling would hand java a stray "legacy112" argument and the unit
  # would not start. Quoting keeps the whole property one argv item.
  local legacy_forwarding_servers="${FELIS_LEGACY_FORWARDING_SERVERS}"
  # The heap starts small and is not pre-touched: the proxy with its Via plugins holds about 50M
  # live, and a pre-touched 512M start kept ~0.7 GB resident on an idle network. Up to a 1G
  # ceiling (the default, sized for about 100 players) it runs the serial collector and only the
  # C1 compiler, 173 MiB idle against 267 MiB under G1 (both measured on the verification host);
  # with that little live, a young collection takes milliseconds, and the proxy's compression
  # and encryption run in Velocity's native library whichever compiler is on. A larger ceiling
  # is for a network where a serial full collection over a big heap would stall every player at
  # once, so it keeps G1, whose periodic collection hands the growth back once players have left.
  # FELIS_VELOCITY_XMX is at least 256M, so the start never exceeds the ceiling.
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
  # A restart disconnects every player on the network, so a rerun that changed nothing the
  # proxy runs leaves it up. Otherwise restart, not `enable --now`: on a re-run the old proxy
  # is already up and --now would leave it running against the new config. The fingerprint is
  # recorded only after the restart, so a run that died between writing and restarting still
  # restarts next time.
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

# velocity_fingerprint hashes what the proxy process runs: its unit (JVM flags and system
# properties), the JRE, the jars and the files the installer writes for it. The Via config
# and whatever else plugins write at runtime stay out; Via rewrites its config on every load.
# So does the service-token line of felis-link.properties: the plugin re-reads it on its own
# (`felis rotate-token velocity` counts on that), and a restart for it would only disconnect
# every player.
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

# ---------------------------------------------------------------------------
# 6. PostgreSQL: the felis-postgres Deployment in k3s (internal/platform/postgres.go),
#    from the official image pinned by digest (POSTGRES_IMAGE), its cluster on the
#    PG_DATA_DIR hostPath. Pods reach it at its Service; the host's own tools and the
#    migrations at 127.0.0.1:PG_HOST_PORT, a hostPort bound to loopback only. A host
#    PostgreSQL an earlier release installed is moved into it once (migrate_host_postgres)
#    and then left installed and stopped, so the move can be rolled back.
# ---------------------------------------------------------------------------

# Commands in the database container. pg_exec never attaches stdin: under
# `curl ... | sudo bash` stdin is the rest of this script, and kubectl exec -i would
# swallow it. pg_sql feeds the SQL on its stdin, so a password in it stays out of argv.
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

# check_postgres_major stops before a new major version meets an old cluster. The image
# keeps each major's cluster in its own directory (PGDATA /var/lib/postgresql/<major>/docker),
# so a new major would not fail on the old one: it would initialise an empty cluster beside
# it and the platform would come up with no users, no servers and no backups.
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
  and restore (docs/operations.md §4): on the release you have now, take a bundle with
  sudo felis db backup -label pre-upgrade, then follow that section."
}

# prepare_postgres_data_dir makes the hostPath the pod needs (hostPath type Directory, so a
# missing one is a pod that never starts). The cluster directory belongs to the image's
# postgres account, uid 999, which on the host is some unrelated account (systemd-coredump
# on EL); the root-only parent keeps that account out of it.
prepare_postgres_data_dir() {
  install -d -m 0700 -o root -g root "$(dirname "$PG_DATA_DIR")"
  install -d -m 0700 -o "$PG_UID" -g "$PG_UID" "$PG_DATA_DIR"
  chown "$PG_UID:$PG_UID" "$PG_DATA_DIR"
  chmod 0700 "$PG_DATA_DIR"
  label_postgres_data_dir
}

# label_postgres_data_dir gives the cluster directory the label containers may write, on a
# host with SELinux enabled. The file-context rule makes it survive a relabel; chcon is the
# fallback where semanage is not installed.
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

# ensure_postgres_superuser_secret creates the postgres role's password once. The image
# reads it only when it initialises the cluster, so replacing it later would change
# nothing but make the Secret lie about the cluster's password.
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
  "$HOST_BIN" manifests --only postgres --control-namespace "$CONTROL_NS" --minecraft-namespace "$MINECRAFT_NS" \
    --postgres-image "$POSTGRES_IMAGE" | kube apply -f -
  if ! kube -n "$CONTROL_NS" rollout status "deployment/${PG_DEPLOYMENT}" --timeout=600s; then
    diagnose_rollout "deployment/${PG_DEPLOYMENT}"
    die "${PG_DEPLOYMENT} did not become ready (docs/troubleshooting.md §16)"
  fi
  ensure_postgres_role
  ok "${PG_DEPLOYMENT} ready: pods at ${PG_SERVICE_ADDR}, this host at 127.0.0.1:${PG_HOST_PORT}"
}

# --- moving a host PostgreSQL into felis-postgres -----------------------------------------

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

# write_pg_hba_lockout heads the host's pg_hba.conf with a block that refuses every TCP
# connection to the felis database but the move's own dump over loopback. pg_hba.conf is
# first-match, so it overrides the rules earlier installers wrote, which it also removes.
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

# quiesce_host_postgres leaves the felis database on the host with no writer: the control
# plane at zero replicas, the host timers stopped, and pg_hba.conf refusing everyone else,
# including pods of a release older than this one that still hold its old address.
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

# retire_host_postgres stops the host server the move emptied of meaning. It stays
# installed with its data, for a rollback (docs/operations.md §4), unless it also serves
# databases that are not the platform's: then it keeps running, with the felis copy locked out.
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

# migrate_host_postgres moves the platform's database from the host PostgreSQL an earlier
# release installed into felis-postgres, once. The copy goes through the same bundle
# format as every backup: felis db backup against the host server, felis db restore into
# the pod, then every table's row count compared. Whatever felis-postgres held before is
# replaced: until the move it served nobody. Any failure before the host server is stopped
# puts the host back as it was (undo_postgres_move); the bundle stays in FELIS_DB_BACKUP_DIR.
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

# undo_postgres_move runs from cleanup when the install fails. Before the host server was
# retired it opens pg_hba.conf again and brings the control plane back, so the platform
# runs on the host database exactly as before the run. After it, the database lives in
# felis-postgres and a rerun finishes pointing everything at it.
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

# ---------------------------------------------------------------------------
# 7. Secrets + felis.toml (the pod copy reaches felis-postgres at its Service; the
#    host copy at its loopback hostPort, and names the pod felis db runs its tools in)
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
  # One felis-api internal token per caller (naming.CallerTokens), so each is scoped
  # to its own routes and a leak is contained to that caller: SERVICE_TOKEN is the
  # proxy's (felis-link.properties), LIMBO_TOKEN the login gate's, BUILD_TOKEN what a
  # build Job fetches its context with, OPS_TOKEN what `felis backup-now` presents.
  # `felis rotate-token <caller>` replaces one of them, and `felis rotate-token
  # registry|forwarding|db` the other values persisted below: every line of secrets.env
  # has a rotation that rewrites it (cmd/felis TestInstallerSecretsAreAllRotatable).
  SERVICE_TOKEN="${SERVICE_TOKEN:-$(openssl rand -hex 32)}"
  LIMBO_TOKEN="${LIMBO_TOKEN:-$(openssl rand -hex 32)}"
  BUILD_TOKEN="${BUILD_TOKEN:-$(openssl rand -hex 32)}"
  OPS_TOKEN="${OPS_TOKEN:-$(openssl rand -hex 32)}"
  # The Velocity modern-forwarding key. It is what makes a backend's UUID trustworthy:
  # the proxy does the Mojang handshake and HMACs the resulting profile with this key,
  # and a backend that cannot verify it would fall back to an offline UUID derived from
  # the username — i.e. anyone could join as anyone, the Owner included. Same value on
  # the proxy (forwarding.secret) and in every backend pod (felis-forwarding-secret).
  FORWARDING_SECRET="${FORWARDING_SECRET:-$(openssl rand -hex 32)}"
  # Registry write credentials, one per principal the registry gate knows
  # (internal/registrygate): platform pushes the installer's own images and the
  # Trivy DB mirrors, build is what a build Job's push container presents and may
  # never write under felis/ or mirror/, prune is felis-api deleting manifests
  # nothing references (internal/registryprune). Reads stay anonymous.
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
# The carry is the section's header and key lines only. Printing every line up to
# the next section header hoarded the generated [[auth_source]] comment block
# that sits below [smtp] into this carry: each re-run then re-emitted the hoard
# plus a fresh template copy, growing both config files by one comment block per
# run (audit #50). The extraction is idempotent, which is also why re-reading the
# freshly rewritten host file on the pod pass is safe. The pod toml remains the
# fallback for a host file with no [smtp] section at all.
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

# persisted_auth_lines echoes the key lines of the [auth] section an earlier run left
# behind, or nothing. Two writers own keys there that nothing in this script's inputs
# derives: the Cloudflare edge setup (access_jwt_aud, client_ip_header -- the header the
# sign-in rate limit keys on) and an operator who serves the panel or the admin console
# on a name other than console.<root> / op.console.<root>. A wholesale rewrite dropped
# all of them on every re-run: behind Cloudflare the rate limit fell back to the tunnel's
# address, one bucket for everyone. Header-and-keys only, like persisted_smtp_block, and
# the same first-readable-file rule.
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

# auth_hostname echoes the installed value of an [auth] hostname key, or the default
# derived from the root domain: `auth_hostname panel_hostname "console.${FELIS_ROOT_DOMAIN}"`.
# The lines are read into a variable before matching, as in auth_lines.
auth_hostname() {
  local lines v
  lines="$(persisted_auth_lines)"
  v="$(awk -F'"' -v k="$1" '$1 ~ "^[[:space:]]*" k "[[:space:]]*=[[:space:]]*$" { print $2; exit }' <<<"$lines")"
  printf '%s' "${v:-$2}"
}

# auth_lines is the body of the [auth] section this run writes: the carried keys, after
# the two hostnames derived from the root domain when the carry lacks them. A first
# install gets exactly the two derived lines; a re-run reproduces the carried section.
# The keys are matched in a here-string, never `printf | grep -q` (see offsite_enabled):
# a lost race there wrote a second admin_hostname, and the duplicate key broke the file.
auth_lines() {
  local carried
  carried="$(persisted_auth_lines)"
  grep -Eq '^[[:space:]]*admin_hostname[[:space:]]*=' <<<"$carried" ||
    printf 'admin_hostname = "op.console.%s"\n' "$FELIS_ROOT_DOMAIN"
  grep -Eq '^[[:space:]]*panel_hostname[[:space:]]*=' <<<"$carried" ||
    printf 'panel_hostname = "console.%s"\n' "$FELIS_ROOT_DOMAIN"
  if [ -n "$carried" ]; then printf '%s\n' "$carried"; fi
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
    # TOML also accepts [[ auth_source ]] and a quoted key; a header this does not
    # recognise would silently drop that table.
    awk '/^[[:space:]]*\[/ { f = /^[[:space:]]*\[\[[[:space:]]*["\047]?auth_source["\047]?[[:space:]]*\]\]/ }
         f { print }' "$f"
    return 0
  done
  printf '%s\n' '[[auth_source]]' 'tag = "littleskin"' 'prefix = "LS"' \
    'url = "https://littleskin.cn/api/yggdrasil/sessionserver/session/minecraft/hasJoined"'
}

# persisted_archive_block echoes the operator-owned [archive] keys an earlier run
# left behind — the retention window, the pre-reap warn offsets, the local cap,
# the on-demand backup retention/count/cooldown, the scheduled backup
# period/count/retention — so a re-run does not silently revert them to the
# built-ins (felis reaper, felis backup and felis api read these from the config
# Secret; defaults: 90d retention, 3d/1d warnings, no cap, manual backups kept
# 30d, 5 per server, one per 10m, a scheduled backup a day kept 7 per server for
# 90d). store and local_path are NOT carried: this script owns them
# (FELIS_ARCHIVE_LOCAL_PATH must equal the mount). Same first-readable-file rule
# as persisted_smtp_block; warn_before must be a single-line TOML array (the
# shape every writer here emits).
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

# persisted_registry_block echoes the operator-owned [registry] keys an earlier run
# left behind — the build-lane executor mirrors, the resource caps, the uploads
# backend and its [registry.s3] subtable — so §15's upgrade path (re-run the
# installer) does not silently revert them. Nothing in this script's inputs
# derives these: they are hand-written per docs/troubleshooting.md §8e or stamped
# by the storage wizard. url and build_namespace are NOT carried: this script
# owns them (they must match REGISTRY_URL / BUILD_NS). Same first-readable-file
# rule as persisted_smtp_block.
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

# persisted_offsite_block echoes the [offsite] section an earlier run (or the operator)
# left behind: after the first install the bucket is configured by editing felis.host.toml
# or by re-running with FELIS_OFFSITE_*, and a plain re-run must keep it. Deleting the
# section and re-running is how the off-site copy is turned off. Same first-readable-file
# rule as persisted_smtp_block.
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

# offsite_enabled: the [offsite] section this run writes names a bucket. The section is
# read into a variable before matching (as in import_platform_images): in `offsite_block |
# grep -q` grep exits at the bucket line, the lines after it then kill offsite_block with
# SIGPIPE, and pipefail made that "no bucket". A re-run that lost the race removed the
# off-site timer and ended with NO OFF-SITE COPY on a host that has one.
offsite_enabled() {
  local block
  block="$(offsite_block)"
  grep -Eq '^[[:space:]]*bucket[[:space:]]*=[[:space:]]*"[^"]+"' <<<"$block"
}

# write_felis_toml target host:port [namespace/deployment]: the deployment is the
# database's pod, where `felis db` runs pg_dump, pg_restore and psql (the host has no
# PostgreSQL client); only the host copy names it. The file is 0600: its url holds the
# database password.
write_felis_toml() {
  local target="$1" db_addr="$2" deployment="${3:-}" deployment_line="" smtp_block auth_body auth_source_blocks registry_block archive_block offsite_section
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
game_version = "${MC_VERSION:-}"

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

# ---------------------------------------------------------------------------
# 8. Migrate + deploy bundle
# ---------------------------------------------------------------------------
run_migrations() {
  local backup_flags=(-backup-dir "$FELIS_DB_BACKUP_DIR")
  write_felis_toml "${STATE_DIR}/felis.host.toml" "127.0.0.1:${PG_HOST_PORT}" "${CONTROL_NS}/${PG_DEPLOYMENT}"
  ensure_default_config
  if [ "$FELIS_PRE_MIGRATE_BACKUP" = 0 ]; then
    warn "FELIS_PRE_MIGRATE_BACKUP=0: pending migrations run without a database snapshot"
    backup_flags=(-no-backup)
  fi
  # Migrations only roll forward. On an existing database with migrations pending, the
  # binary bundles the database into FELIS_DB_BACKUP_DIR first and refuses to migrate
  # if that fails; a fresh database has nothing to protect and is migrated directly.
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

# The hourly off-site copy. The bucket is checked now, in the install, so wrong
# credentials, an unreachable endpoint, a key other than the one its objects are sealed
# with, or another host writing it show up here; the first copy itself runs in the
# background, since a host with many archives can take a long while to upload them.
# With no bucket configured the timer is removed (the operator deleted [offsite]) and the
# install says loudly that every backup is on this machine only.
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
      # A host built from another host's backup (a rehearsal on a spare machine, or a
      # rebuild) finds that host named as the bucket's writer and stands by: its copy
      # writes nothing there and its watchdog mails nobody while that host keeps writing.
      # A host another one took the bucket over from (5) stops copying and mails its
      # owners. The first copy still runs, to record either for the watchdog.
      local who wrc=0
      who="$("$HOST_BIN" offsite take-over -config "${STATE_DIR}/felis.host.toml" -env-file "$OFFSITE_ENV")" || wrc=$?
      systemctl start --no-block felis-offsite.service
      case "$wrc" in
        0) ok "off-site copy: hourly to the [offsite] bucket, first copy started (sudo felis offsite status; journalctl -u felis-offsite)" ;;
        4)
          OFFSITE_STANDBY=1
          warn "================================================================================"
          warn "Another host writes the [offsite] bucket, and this host was built from its backup:"
          warn "  $(printf '%s\n' "$who" | head -n 1 | sed 's/^felis offsite take-over: //')"
          warn "This host copies nothing into the bucket and, while that host keeps writing it,"
          warn "mails no watchdog alert: a rehearsal leaves that host and its owners alone. When"
          warn "this host replaces it for good, run sudo felis offsite take-over -yes, then"
          warn "sudo systemctl start felis-offsite.service (docs/troubleshooting.md §16)."
          warn "================================================================================"
          ;;
        5)
          OFFSITE_DISPLACED=1
          warn "================================================================================"
          warn "Another host took the [offsite] bucket over from this host:"
          warn "  $(printf '%s\n' "$who" | head -n 1 | sed 's/^felis offsite take-over: //')"
          warn "This host copies nothing into the bucket any more, and its watchdog mails the"
          warn "owners about it. If that host is a rehearsal machine, take the bucket back:"
          warn "sudo felis offsite take-over -yes, then sudo systemctl start felis-offsite.service"
          warn "(docs/troubleshooting.md §16)."
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
      warn "and re-run the installer (docs/troubleshooting.md §16)."
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
    warn "FELIS_OFFSITE_ACCESS_KEY and FELIS_OFFSITE_SECRET_KEY and re-run (docs/troubleshooting.md §16)."
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

# The daily version check. Felis applies no update on its own; `felis update --record`
# compares what this host runs with the newest upstream releases and stores the result,
# which the panel's Updates page shows with the command that applies each update. It runs
# on the host because that is where the installed versions are readable. The first check
# runs in the background: it waits on the release feeds, and nothing in the install
# depends on it.
install_update_check_timer() {
  cat > "$UPDATE_CHECK_SERVICE" <<EOF
[Unit]
Description=Felis component version check (felis update --record)
After=network-online.target k3s.service
Wants=network-online.target

[Service]
Type=oneshot
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

# The platform watchdog: every two minutes it checks the control plane, the login gate,
# the fleet, PostgreSQL, the game proxy, the database backups and the host's disks and
# memory, and mails the owners (their verified addresses, over the [smtp] relay) what
# has stayed wrong long enough to matter. It runs on the host so a k3s that is down is
# still reported. The first run happens now, so a broken unit shows up in this install.
# A run that fails starts felis-watchdog-failed.service (OnFailure=), which mails the
# failure once five runs in a row failed, through the relay the last good run cached, and
# pings the heartbeat's failure endpoint. The heartbeat (FELIS_WATCHDOG_HEARTBEAT_URL) is
# what notices a host that is down or a watchdog that no longer runs at all. The units
# name no heartbeat flag: felis watchdog reads WATCHDOG_HEARTBEAT_FILE by default, so an
# older binary put back under these units still runs.
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

# summary_alerts says where the watchdog's alerts go. They go by mail only: through the
# [smtp] relay `felis setup` configures (e, configure email), to every enabled Owner's
# verified address. With no relay each alert, like each sign-in code, is only written to
# the journal. Without this line the operator learns that from an outage no one heard of.
summary_alerts() {
  if persisted_smtp_block | grep -Eq '^[[:space:]]*host[[:space:]]*=[[:space:]]*"[^"]'; then
    log "Alerts: the watchdog mails the Owner's verified email address through the [smtp] relay."
    return 0
  fi
  warn "NO ALERT MAIL: no email relay is configured, so the watchdog's alerts and the sign-in codes are only written to the journal."
  warn "Configure one in 'sudo felis setup' (e: configure email) and verify the Owner's email in the panel; alerts go to that address."
}

# summary_heartbeat closes the install on the heartbeat: without one, nothing off this
# machine notices it going down.
summary_heartbeat() {
  if [ -f "$WATCHDOG_HEARTBEAT_FILE" ]; then
    log "Heartbeat: every watchdog run pings $(heartbeat_host); that service mails you when the pings stop."
    return 0
  fi
  warn "NO HEARTBEAT: nothing off this machine notices it going down or its watchdog stopping."
  warn "Create a check at a monitoring service (Healthchecks.io or alike; period 2 min, grace 10 min),"
  warn "then re-run with FELIS_WATCHDOG_HEARTBEAT_URL=<its ping URL> (docs/troubleshooting.md §14)."
}

# The build lane's tools: kaniko and trivy (pinned by digest in internal/build/tools.go)
# and Trivy's vulnerability and Java DBs, copied into the registry's mirror/ where build
# Jobs pull them; the build namespace has no internet egress. The timer refreshes the DBs
# twice a day (upstream publishes every six hours) and the watchdog warns when three days
# pass without a clean run. The first copy starts now in the background: the Java DB
# alone is several hundred MB.
install_build_tools_timer() {
  install -d -m 0755 "$(dirname "$BUILD_TOOLS_STATUS")"
  cat > "$BUILD_TOOLS_SERVICE" <<EOF
[Unit]
Description=Copy the Felis build tools and Trivy's vulnerability DBs into the registry
After=network-online.target k3s.service
Wants=network-online.target

[Service]
Type=oneshot
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

# The daily database backup. The first run happens now, so a broken pipeline (pg_dump
# missing, directory unwritable) shows up in this install rather than in the first
# restore someone needs.
install_db_backup_timer() {
  install -d -m 0700 "$FELIS_DB_BACKUP_DIR"
  cat > "$DB_BACKUP_SERVICE" <<EOF
[Unit]
Description=Felis control-plane database backup (pg_dump + /etc/felis state)
After=k3s.service

[Service]
Type=oneshot
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

# configure_offsite writes OFFSITE_ENV, the secrets behind [offsite]: the bucket's access
# keys and the key every off-site object is sealed with. Values in this run's environment
# replace the file's (rotating the bucket credentials is a re-run); the encryption key is
# generated when neither has one. A different key than the file's is refused: every object
# already in the bucket is sealed with the old one, and swapping it would make them
# unreadable without a word. Whether the bucket's objects agree with the key is checked once
# the binary is installed (install_offsite_timer).
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
    die "FELIS_OFFSITE_KEY differs from the key in ${OFFSITE_ENV}, which sealed what this host copied to the bucket. Unset FELIS_OFFSITE_KEY to keep it; when felis offsite check-key says the bucket's objects are sealed with another key, set FELIS_OFFSITE_KEY in ${OFFSITE_ENV} by hand instead (docs/troubleshooting.md §16)"
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
# The off-site copy's secrets (felis offsite, docs/troubleshooting.md §16). Keep a copy of
# FELIS_OFFSITE_KEY somewhere other than this machine: without it the copies in the
# bucket cannot be read, and this file goes with the machine.
FELIS_OFFSITE_ACCESS_KEY='${FELIS_OFFSITE_ACCESS_KEY}'
FELIS_OFFSITE_SECRET_KEY='${FELIS_OFFSITE_SECRET_KEY}'
FELIS_OFFSITE_KEY='${FELIS_OFFSITE_KEY}'
EOF
  ok "off-site copy: secrets in ${OFFSITE_ENV}"
}

# Releases before the reaper ran as root granted uid 1000 traverse on the worlds root: an
# ACL entry, or o+x where the host had no setfacl. uid 1000 is now the game servers' uid,
# and the per-volume directories below that root are 0777, so the grant let a game process
# (and, for o+x, every local account) reach any world by its directory name. Every run
# takes it back: the ACL entry from any worlds root, the other-bits only from k3s's storage
# root, which k3s ships 0700 root:root. A custom root keeps its mode, which may be the
# operator's own.
revoke_worlds_root_grant() {
  local dir
  for dir in "$K3S_STORAGE_ROOT" "$FELIS_WORLDS_HOST_PATH"; do
    [ -n "$dir" ] && [ -d "$dir" ] || continue
    if command -v getfacl >/dev/null 2>&1 && getfacl -cpn "$dir" 2>/dev/null | grep -q '^user:1000:'; then
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

  log "provisioning felis-config + internal caller tokens + felis-forwarding-secret + registry credentials + mail relay and uploads bucket credentials + panel TLS secrets (out-of-band, never in the bundle)"
  apply_felis_config_secrets
  # felis-api mounts all four caller tokens from the control namespace. The login
  # gate's and the build Job's are also applied into the namespace their pods run in
  # (a secretKeyRef is namespace-local); applying them here rather than leaving it to
  # `felis setup` means an upgrade has them in place before the new operator points
  # the login pod at felis-limbo-token. The proxy's and the ops token stay here only.
  apply_literal_secret "$CONTROL_NS" felis-service-token token "$SERVICE_TOKEN"
  apply_literal_secret "$CONTROL_NS" felis-limbo-token token "$LIMBO_TOKEN"
  apply_literal_secret "$MINECRAFT_NS" felis-limbo-token token "$LIMBO_TOKEN"
  apply_literal_secret "$CONTROL_NS" felis-build-token token "$BUILD_TOKEN"
  apply_literal_secret "$BUILD_NS" felis-build-token token "$BUILD_TOKEN"
  apply_literal_secret "$CONTROL_NS" felis-ops-token token "$OPS_TOKEN"
  # The forwarding key every backend verifies the proxy's handshake with. `felis setup`
  # replicates it into the minecraft namespace (ensureSecretReplica) before it creates
  # the pods that mount it; the operator injects it into EVERY backend, because Velocity's
  # forwarding mode is one proxy-wide setting — a backend that does not speak it is not
  # "less secure", it is unjoinable.
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
    local system_deployment
    for system_deployment in coredns local-path-provisioner; do
      if kube -n kube-system get deployment "$system_deployment" >/dev/null 2>&1; then
        kube -n kube-system patch deployment "$system_deployment" --type merge \
          -p "{\"spec\":{\"template\":{\"spec\":{\"nodeSelector\":{\"felis.node-restriction.kubernetes.io/identity\":\"$controller\"}}}}}"
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
  )
  if [ "${DISTRIBUTED:-0}" = 1 ]; then
    manifest_args+=(--distributed --controller-node "$controller" --egress-probe "felis-api.${CONTROL_NS}.svc:443")
    # Every node address, including global addresses, must be excluded from game egress.
    while read -r cidr; do
      [ -z "$cidr" ] || manifest_args+=(--server-egress-deny-cidr "$cidr")
    done < <(kube get nodes -o jsonpath='{range .items[*]}{range .status.addresses[*]}{.address}{"\n"}{end}{end}' | awk '/^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$/ {print $0"/32"}')
  fi
  local cidr
  while read -r cidr; do
    [ -n "$cidr" ] && manifest_args+=(--server-egress-deny-cidr "$cidr")
  done < <(node_global_cidrs)
  # Backups are on by default (the renderer's own default names felis-backups); an emptied
  # FELIS_BACKUP_PVC asks for the no-backup shape explicitly, and a custom name must be
  # passed through or the api would advertise a PVC the bundle never created.
  if [ -n "$FELIS_BACKUP_PVC" ]; then
    manifest_args+=(--backup-pvc "$FELIS_BACKUP_PVC")
  else
    manifest_args+=(--backup-pvc=)
  fi
  # With an archive store the reaper always renders, since backups past their expiry have
  # to leave it; it reaps idle worlds only when the operator names where the worlds live.
  # The archive path must equal the [archive] local_path written above.
  if [ -n "$FELIS_BACKUP_PVC" ]; then
    manifest_args+=(--archive-local-path "$FELIS_ARCHIVE_LOCAL_PATH")
    if [ -z "$FELIS_WORLDS_HOST_PATH" ]; then
      log "idle-world retention is off: the daily reaper deletes expired backups and keeps every world (set FELIS_WORLDS_HOST_PATH=${K3S_STORAGE_ROOT} to reap worlds idle for 15 days)"
    fi
  fi
  if [ -n "$FELIS_WORLDS_HOST_PATH" ]; then
    log "retention enabled: the daily reaper will read worlds from ${FELIS_WORLDS_HOST_PATH}"
    # The reaper reads this root as root with DAC_OVERRIDE (platform.reaperPodSecurityContext)
    # through a static hostPath PV, so the host directory keeps k3s's own 0700 root:root and
    # needs no extra grant. It must exist, though: the PV declares type Directory.
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
  if [ -n "$FELIS_BACKUP_PVC" ]; then
    size="$(pvc_size "$MINECRAFT_NS" "$FELIS_BACKUP_PVC" "$FELIS_BACKUP_STORAGE" FELIS_BACKUP_STORAGE)"
    if [ -n "$size" ]; then manifest_args+=(--backup-storage "$size"); fi
  fi
  "$HOST_BIN" manifests "${manifest_args[@]}" | kube apply -f -
  # A client-side apply leaves a replica count the manifest did not change alone, so the
  # zero a database move (or an operator's `felis db restore`) scaled the control plane
  # to would outlive this install.
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
  # Before per-caller tokens the proxy's token was replicated into the workload
  # namespaces for the login gate and the build Jobs. The new operator and api no
  # longer reference those copies; leaving them would keep the proxy's credential
  # readable from namespaces that have no business with it.
  kube -n "$MINECRAFT_NS" delete secret felis-service-token --ignore-not-found
  kube -n "$BUILD_NS" delete secret felis-service-token --ignore-not-found
}

# pvc_size <namespace> <claim> <wanted> <env name> prints the size to render the claim
# with: its current request when it exists, else the wanted size (empty = the renderer's
# default). A claim's request can only grow, and only on a storage class that allows
# expansion (k3s local-path does not), so re-applying a different size would fail the
# whole apply; a mismatch is reported and left to the operator.
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

# restart_existing_control_plane restarts the Deployments the bundle apply left as they were:
# those that already ran FELIS_IMAGE, whose tag now names a rebuilt image (a rerun of the same
# version, or a FELIS_IMAGE the operator reuses). A Deployment whose image changed is rolling
# from the apply already, and must not be restarted on top: the restart is a second template
# change, so `rollout undo` would step back to the new image instead of the previous release.
#
# The registry pod runs the same binary in its registry-gate and registry-gc containers, so it
# follows the same rule; the rollout wait below covers it before anything is pushed.
restart_existing_control_plane() {
  local prev_api="$1" prev_operator="$2" prev_gate="${3:-}"
  # `if`, not `[ test ] && cmd`: as the LAST command of the function the and-list returns 1
  # when the test is false, which becomes the function's exit status and kills the whole
  # install under `set -Eeuo pipefail` — right after the bundle is applied and before the
  # rollout wait.
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

# push_image_to_registry <ref> re-tags a locally built image for the node's
# loopback push endpoint and uploads it. The registry keys a repository by the
# path AFTER the host, so pushing 127.0.0.1:5000/felis/felis:demo lands exactly
# where a later kubelet pull of registry.felis.svc:5000/felis/felis:demo (the
# mirror rewrites the host) will look. A ref not under REGISTRY_URL is not
# mirrored — warn, don't fail: the install is still self-consistent, that image
# just has no pull source once GC collects its containerd copy.
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

# registry_docker_login logs a throwaway docker config into the registry gate as
# the platform principal: writes are refused anonymously, and this identity is
# the only one allowed under felis/. The config lives in a 0700 temp dir that the
# EXIT trap removes, so the token never lands in root's ~/.docker.
registry_docker_login() {
  REGISTRY_DOCKER_CONFIG="$(umask 077; mktemp -d)"
  remember_temp "$REGISTRY_DOCKER_CONFIG"
  printf '%s' "$REGISTRY_PLATFORM_TOKEN" | docker --config "$REGISTRY_DOCKER_CONFIG" \
    login --username platform --password-stdin "$REGISTRY_PUSH_HOST" >/dev/null \
    || die "could not log in to the internal registry at ${REGISTRY_PUSH_HOST} as platform — check the registry-gate container's log and the felis-registry-auth Secret"
}

# Every image this installer builds is hosted in the registry, so the copies it
# imported into containerd are a first-boot cache, not the only copy: kubelet
# re-pulls from the registry after any image GC. Runs AFTER deploy_bundle — the
# registry it pushes into does not exist before that.
#
# Docker is started once for the whole batch and stopped once at the end. A
# start/stop pair per image trips systemd's start rate limit — observed live on
# a re-run: three fast pushes, then "Start request repeated too quickly /
# start-limit-hit" and the fourth image never got mirrored. docker.service is
# socket-triggered, so each cycle counts twice against the burst limit.
#
# An image the release shipped (import_release_images) is pushed from its bundle by felis
# push-image, and needs no Docker at all; only the images built here go through docker push.
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
  stop_docker
  # Every bundle is in containerd and the registry now; a rerun that needs one fetches it again.
  if [ "$ARTIFACT_MODE" = release ]; then
    rm -rf -- "$ARTIFACT_CACHE"
  fi
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

# push_release_image <role> mirrors a release image into the platform registry under its tag
# and, for a game image, under push_version_tag's <Minecraft version>-<12 hex of its id> as
# well, the id being the image's config digest here as it is docker's image id there. A tag
# already naming the image's digest is left as it is, so a rerun uploads nothing.
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

# push_version_tag mirrors a game image a second time under a tag no later run
# rewrites: <Minecraft version>-<first 12 hex of the image id>, e.g.
# felis/paper:26.2-3f9c0a1b2c4d. The :demo tag moves with every run, and servers are
# pinned to the digest it named when they were created, so this is the readable name
# for each build: an admin can whitelist it to create servers on that exact
# Minecraft version long after :demo has moved on.
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

# The login/lobby images are built under mutable :demo tags, and an existing
# StatefulSet whose template still names that tag will not roll onto a new build by
# itself. When a build changed, each system server is pinned to the digest its tag
# names now (felis pin-images --system, after push_images_to_registry): the new ref
# changes the template, the operator rolls the pod onto it, and the build each one
# runs is written in its spec. A pin that fails (registry down) falls back to
# recreating the pod, which picks the build up only while the spec names the bare
# tag. Only a changed build does either: every player online is on one of these
# two, and a rerun that rebuilt nothing has nothing to start. SYSTEM_SERVER_IMAGES
# records the build each was last moved to; it is written after the loop, so a run
# that died in between moves them next time.
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
    log "The database stays migrated; docs/troubleshooting.md §16 has the full rollback."
  fi
  echo
  summary_offsite
  summary_alerts
  summary_heartbeat
  echo
  summary_next
  echo
}

# owner_state: whether the database holds a staff account (an Owner or an Admin), the test
# `felis setup` makes to choose between the Owner wizard and its status screen (AdminExists
# in internal/api/pgrepo.go). "unknown" when the database does not answer.
owner_state() {
  local out
  out="$(pg_exec psql -XtA -U postgres -d "$DB_NAME" -c "SELECT EXISTS (SELECT 1 FROM users WHERE role IN ('admin', 'owner'))" 2>/dev/null || true)"
  case "$out" in
    t) echo yes ;;
    f) echo no ;;
    *) echo unknown ;;
  esac
}

# setup_terminal: whether the full-screen setup console has a terminal to draw on and to
# read keys from. Under `curl | sudo bash` stdin is the script, so the console reads
# SETUP_TTY; stdout must be the terminal itself, which `| tee install.log`, cloud-init and
# CI are not. /dev/tty is mode 0666 everywhere, so only opening it tells.
setup_terminal() { [ -t 1 ] && (: <"$SETUP_TTY") 2>/dev/null; }

# summary_next is the installer's last word: what the operator does now. It comes after the
# warnings, so it is what the terminal is left showing. Until a staff account exists that is
# `felis setup`, which creates the Owner; on a terminal the installer starts it itself
# (start_setup_console), as the README's install line promises. With an Owner in place it
# is the address to sign in at. Under felis setup the console carries on by itself.
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
    log "(https://${NODE_IP}:${FELIS_PANEL_NODEPORT} until the edge routes it there)."
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

# start_setup_console runs `felis setup` when summary_next said it would. The install has
# succeeded by then, so the console's own failure never fails the run: the EXIT cleanup
# would take that for a failed install and undo the database move.
start_setup_console() {
  [ "$SETUP_CONSOLE" = 1 ] || return 0
  resume_package_background_timers
  # The install is done, and felis setup can start the installer itself (its host
  # bootstrap step): an inherited run lock would stop that run as a second one.
  exec 9>&-
  "$HOST_BIN" setup <"$SETUP_TTY" \
    || warn "the setup console exited with status $?; run 'sudo felis setup' to come back to it"
}

# ---------------------------------------------------------------------------
# 10. Felis-nano install path — the auth multiplexer only: felis binary + a
#     minimal [[auth_source]] config + a systemd unit running `felis nano`.
#     No k3s, no Postgres, no control-plane bundle. Chosen at the top-of-run
#     prompt (or FELIS_INSTALL_MODE=nano).
# ---------------------------------------------------------------------------
prompt_install_mode() {
  if [ "$INSTALL_MODE" = worker ]; then log "install mode: worker";return; fi
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

  # No override: ask on the controlling terminal. Under `curl | sudo bash` stdin
  # is the script, so we must read /dev/tty, not stdin. No tty (CI/cloud-init) →
  # take the default. Open it to find out: /dev/tty is mode 0666 on every Linux host, so
  # `-r` passes even when there is no controlling terminal and only the open fails.
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
  if [ -x "${GOROOT_DIR}/bin/go" ] && "${GOROOT_DIR}/bin/go" version | grep -q "go${FELIS_GO_VERSION} "; then
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
  keep_previous_host_binary
  rm -f "$HOST_BIN"
  install -m 0755 "$staged" "$HOST_BIN"
  rm -f "$staged"
  command -v restorecon >/dev/null 2>&1 && restorecon "$HOST_BIN" >/dev/null 2>&1 || true

  ok "felis binary on host at ${HOST_BIN}"
}

# acquire_felis_binary is the full install's four ways to a felis binary, in preference
# order. FELIS_ARTIFACT_DIR and the release download skip compiling: each is the CI artifact
# for its tag, panel included. The other arms leave HAVE_PREBUILT_BINARY unset where a source
# build is what actually happens, which is what routes build_image. Neither the download nor
# fetch_source is an if condition or a command before && or ||: errexit is off inside those.
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
  # The release channel takes the same prebuilt binary the control plane does. This is the
  # biggest win on this path: a host that only wants the auth multiplexer stops needing a Go
  # toolchain and a checkout at all.
  if use_release_binary; then
    resolve_install_ref
    download_release_binary
    [ -z "$HAVE_PREBUILT_BINARY" ] || return 0
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
  # directory. The mode is explicit because a hardened root umask (027) would leave it 0750,
  # which is what older installers did to nano-only hosts. Only the full install keeps its
  # own 0700: that directory holds secrets, and install_nano_service reports the lockout
  # rather than this widening it.
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

# resolve_nano_listen settles FELIS_NANO_LISTEN: the operator's value, else the address the
# installed felis-nano unit listens on, else loopback. Re-running this script is how a nano
# host updates, and without the middle step that re-run moved an off-host proxy's endpoint
# back to 127.0.0.1, so every login through it failed.
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
  # hasJoined takes no token, so the port is opened to the proxy alone. Earlier installers
  # opened it to every source, and a re-run must not leave that behind. A rule for a previous
  # FELIS_NANO_PROXY_CIDR is not tracked; it stays until removed by hand.
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
  systemctl --no-pager --full status felis-nano 2>/dev/null | head -n 6 || true
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

# Worker is a daemon-only branch. It neither generates Felis service credentials nor applies a controller bundle.
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
  if [ "$INSTALL_MODE" = worker ]; then main_worker; return; fi
  detect_node_ip
  # Before the first change to the host: a problem found here costs a rerun, one found
  # halfway through costs an install to unwind.
  preflight
  quiet_watchdog
  pause_package_background_timers
  ensure_swap
  install_base
  ensure_time_sync
  ensure_persistent_journal
  # Right after install_base because it is the first point curl exists, and well before
  # docker and k3s: a missing FELIS_GITHUB_TOKEN or an unpublished release should cost
  # the operator seconds, not a k3s install they then have to unwind. This is purely
  # fail-fast — fetch_source resolves again itself — so it must skip on exactly the paths
  # that never consume the result, or it invents a network dependency and a version they
  # do not have: the TUI rebuilds the binary it is already running, and FELIS_SKIP_FETCH
  # builds whatever is staged, which stamp_version reads the SHA off. Resolving anyway
  # would set FELIS_VERSION to the newest tag and stamp a staged tree as that release.
  # FELIS_ARTIFACT_DIR is its own release: its binary names the version.
  bootstrap_from_tui || [ -n "${FELIS_SKIP_FETCH:-}" ] || [ -n "$FELIS_ARTIFACT_DIR" ] || resolve_install_ref
  install_cloudflared
  load_or_make_secrets
  configure_offsite
  ensure_panel_tls_cert
  # No install_docker here: Docker comes in only for an image this run has to build
  # (ensure_docker), and an install from a release's assets builds none.
  if [ -z "${FELIS_DISTRIBUTED+x}" ] && [ -f "$K3S_CONFIG_DROPIN" ] && grep -q 'flannel-backend: "wireguard-native"' "$K3S_CONFIG_DROPIN"; then DISTRIBUTED=1; fi
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
    [ -n "$WORKER_PEERS" ] || WORKER_PEERS="${NODE_EXTERNAL_IP:-$NODE_IP}/32"
    "$HOST_BIN" node firewall --controller --controller-ip "${NODE_EXTERNAL_IP:-$NODE_IP}" --peers "$WORKER_PEERS" --pod-cidr "$POD_CIDR" --node-port "$FELIS_PANEL_NODEPORT" --control-namespace "$CONTROL_NS" --namespace "$MINECRAFT_NS"
  fi
  # After build_image imported the felis image: the registry pod's gate runs it.
  pin_platform_images
  # Before build_game_stack: the builds user servers run must be read off the
  # registry's tags before new ones replace them.
  pin_user_server_images
  build_game_stack
  deploy_postgres
  # Before run_migrations writes the host config that points at felis-postgres: until the
  # move, the database is the host PostgreSQL an earlier release installed.
  migrate_host_postgres
  run_migrations
  deploy_bundle
  # AFTER deploy_bundle: the registry the built images are mirrored into is part
  # of that bundle.
  push_images_to_registry
  # After deploy_bundle, like the pushes: it writes into the registry.
  install_build_tools_timer
  restart_existing_system_servers
  # After deploy_bundle: the proxy dials felis-api's internal ClusterIP, which does not
  # exist until the bundle is applied.
  install_velocity
  # After deploy_bundle: the bundle's MinecraftServer export reads the cluster.
  install_db_backup_timer
  # After the backup timer: its first bundle is part of the first copy.
  install_offsite_timer
  # After install_velocity: the check reads the installed proxy jar's version.
  install_update_check_timer
  # Last: its first run should see the platform as this install leaves it.
  install_watchdog_timer
  mark_bootstrap_done
  summary
  start_setup_console
}

main "$@"
