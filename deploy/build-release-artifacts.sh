#!/usr/bin/env bash
# Builds everything a Felis release installs, for every architecture, into one directory:
#
#   felis-linux-<arch>                  the felis binary, panel included
#   felis-image-felis-linux-<arch>.tar  the control-plane image          (OCI layout tars:
#   felis-image-game-linux-<arch>.tar   the limbo, lobby and paper images  `ctr images import`
#   felis-image-base-linux-<arch>.tar   the registry and PostgreSQL        reads them as-is)
#   felis-images-linux-<arch>.txt       one "bundle role name manifest-digest config-digest"
#                                       line per image in the three tars
#   felis-velocity.jar                  the proxy plugin (JVM bytecode, one for every arch)
#   SHA256SUMS                          the sha256 of every file above
#
# Every name above is a contract with deploy/bootstrap.sh, which installs from a release's
# assets (or from a directory like this one, FELIS_ARTIFACT_DIR) instead of building on the
# host: with them a host needs neither Docker, Gradle, Go nor Docker Hub. The images are split
# in three because they change at different rates: the control plane with every release, the
# game images when game-stack.lock or a plugin changes, the base images almost never. An
# upgrade downloads only the tars holding an image the host does not have yet.
#
# .github/workflows/release.yml runs this on a tag and publishes the directory; e2e.yml runs
# it on a branch and installs from the directory.
#
# Needs docker with buildx, and binfmt/QEMU for the architectures other than the builder's:
# the game images' runtime stages run apt-get on the target platform. The builder's own felis
# binary writes the image tars, so Go is not needed here either.
#
# Usage: deploy/build-release-artifacts.sh <version> <out-dir>
#   FELIS_RELEASE_ARCHES   the architectures to build (default "amd64 arm64")
set -Eeuo pipefail

log() { printf '\033[1;36m[release]\033[0m %s\n' "$*"; }
die() { printf '\033[1;31m[fail]\033[0m %s\n' "$*" >&2; exit 1; }

[ "$#" -eq 2 ] || die "usage: $0 <version> <out-dir>"
VERSION="$1"
OUT="$2"
ARCHES="${FELIS_RELEASE_ARCHES:-amd64 arm64}"
cd "$(dirname "$0")/.."

# The Gradle image the plugin builds run in: deploy/{limbo,lobby}/Dockerfile and bootstrap's
# Velocity build name the same one (bootstrap_asset_test.go holds them together).
PLUGIN_BUILD_IMAGE="gradle:9.8.0-jdk25@sha256:2b2fc1b1dfc3604a2acc916839f36eb5ee48fd7f232427fc5faca224c73bcb01"

# The names and pins bootstrap uses, read from bootstrap itself so the two cannot drift.
bootstrap_value() {
  local v
  v="$(sed -n "s/^$1=\"\(.*\)\"\$/\1/p" deploy/bootstrap.sh)"
  [ -n "$v" ] || die "deploy/bootstrap.sh sets no $1"
  printf '%s' "$v"
}
REGISTRY_URL="$(bootstrap_value REGISTRY_URL)"
REGISTRY_IMAGE="$(bootstrap_value REGISTRY_IMAGE)"
POSTGRES_IMAGE="$(bootstrap_value POSTGRES_IMAGE)"
# The final stage of the repo Dockerfile, the base every felis image runs on.
FELIS_BASE_IMAGE="$(awk '$1 == "FROM" && $2 ~ /^gcr\.io\/distroless\// { print $2 }' Dockerfile)"
[ -n "$FELIS_BASE_IMAGE" ] || die "the Dockerfile names no distroless base"

# lock_value reads one KEY=value line of deploy/game-stack.lock, which bootstrap reads the
# same way: never sourced, values limited to URL and version characters.
lock_value() {
  local v
  v="$(awk -F= -v k="$1" '$1 == k { print substr($0, length(k) + 2); exit }' deploy/game-stack.lock)"
  case "$v" in
    ''|*[!A-Za-z0-9._:/+%-]*) die "deploy/game-stack.lock: $1 is missing or malformed" ;;
  esac
  printf '%s' "$v"
}

# image_tag_for_version is bootstrap's: the tag bootstrap names this release's image with.
image_tag_for_version() {
  local v
  v="$(printf '%s' "$1" | tr -c 'A-Za-z0-9_.-' '-')"
  case "$v" in
    ""|dev|[!A-Za-z0-9_]*) printf 'demo' ;;
    *) printf '%s' "${v:0:128}" ;;
  esac
}

host_arch() {
  case "$(uname -m)" in
    x86_64|amd64) printf 'amd64' ;;
    aarch64|arm64) printf 'arm64' ;;
    *) die "unsupported builder architecture $(uname -m)" ;;
  esac
}

mkdir -p "$OUT"
[ -z "$(ls -A "$OUT")" ] || die "${OUT} is not empty; SHA256SUMS must describe exactly what one run built"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
platforms=""
for arch in $ARCHES; do
  case "$arch" in amd64|arm64) ;; *) die "unsupported architecture ${arch}" ;; esac
  platforms="${platforms:+${platforms},}linux/${arch}"
done

# ---- the felis binaries --------------------------------------------------------------
# Through the repo Dockerfile, the one recipe that runs the panel build before go build:
# a plain `go build` compiles against the placeholder panel and ships it.
log "building felis ${VERSION} for ${platforms}"
docker buildx build --platform "$platforms" \
  --build-arg FELIS_VERSION="$VERSION" \
  --output "type=local,dest=${WORK}/bin" .
for arch in $ARCHES; do
  src="${WORK}/bin/usr/local/bin/felis"
  # buildx nests the output per platform only when it builds more than one.
  [ -f "${WORK}/bin/linux_${arch}/usr/local/bin/felis" ] && src="${WORK}/bin/linux_${arch}/usr/local/bin/felis"
  install -m 0755 "$src" "${OUT}/felis-linux-${arch}"
  # By ELF machine, never by running it: binfmt would run the wrong architecture happily.
  case "$arch" in
    amd64) want='x86-64' ;;
    arm64) want='ARM aarch64' ;;
  esac
  file "${OUT}/felis-linux-${arch}" | grep -q "$want" \
    || die "felis-linux-${arch} is not a ${want} ELF: TARGETARCH did not reach the go build"
done
HOST_FELIS="${OUT}/felis-linux-$(host_arch)"
[ -x "$HOST_FELIS" ] || die "no felis binary for the builder's own architecture ($(host_arch)); add it to FELIS_RELEASE_ARCHES"
got="$("$HOST_FELIS" version | head -n 1)"
[ "$got" = "felis ${VERSION}" ] || die "felis reports '${got}', want 'felis ${VERSION}': the version stamp did not reach the binary"

# ---- the Velocity plugin -------------------------------------------------------------
# The command bootstrap's source path runs, on a copy of the tree so the checkout stays clean.
log "building felis-velocity.jar in ${PLUGIN_BUILD_IMAGE%%@*}"
mkdir -p "${WORK}/velocity"
tar -C . --exclude=build --exclude=.gradle -cf - plugins/velocity plugins/shared | tar -C "${WORK}/velocity" -xf -
docker run --rm --user "$(id -u):$(id -g)" -e HOME=/tmp -e GRADLE_USER_HOME=/tmp/gradle \
  -v "${WORK}/velocity:/src" -w /src/plugins/velocity \
  "$PLUGIN_BUILD_IMAGE" gradle --no-daemon clean build --console=plain --init-script ../shared/build-progress.gradle
jars=( "${WORK}"/velocity/plugins/velocity/build/libs/felis-velocity-*.jar )
[ "${#jars[@]}" -eq 1 ] && [ -f "${jars[0]}" ] || die "the felis-velocity build must produce exactly one plugin jar"
install -m 0644 "${jars[0]}" "${OUT}/felis-velocity.jar"

# ---- the images ----------------------------------------------------------------------
FELIS_IMAGE="${REGISTRY_URL}/felis/felis:$(image_tag_for_version "$VERSION")"
game_build_args=(
  --build-arg "LIMBO_JAR_URL=$(lock_value LIMBO_JAR_URL)"
  --build-arg "LIMBO_JAR_SHA256=$(lock_value LIMBO_JAR_SHA256)"
  --build-arg "LIMBO_SCHEM_URL=$(lock_value LIMBO_SCHEM_URL)"
  --build-arg "LIMBO_SCHEM_SHA256=$(lock_value LIMBO_SCHEM_SHA256)"
  --build-arg "LIMBO_VERSION=$(lock_value LIMBO_VERSION)"
  --build-arg "PAPER_JAR_URL=$(lock_value PAPER_JAR_URL)"
  --build-arg "PAPER_JAR_SHA256=$(lock_value PAPER_JAR_SHA256)"
  --build-arg "LUCKPERMS_JAR_URL=$(lock_value LUCKPERMS_JAR_URL)"
  --build-arg "LUCKPERMS_JAR_SHA256=$(lock_value LUCKPERMS_JAR_SHA256)"
)

# oci_image <arch> <dest> <docker build args...> builds one image for one platform into an
# OCI layout tar. No attestations: the bundle carries images only.
oci_image() {
  local arch="$1" dest="$2"
  shift 2
  docker buildx build --platform "linux/${arch}" --provenance=false --sbom=false \
    --output "type=oci,dest=${dest}" "$@"
}

# bundle <arch> <group> <image-bundle flags...> writes one image tar and appends its lines
# to the architecture's listing.
bundle() {
  local arch="$1" group="$2" name
  shift 2
  name="felis-image-${group}-linux-${arch}.tar"
  "$HOST_FELIS" image-bundle --platform "linux/${arch}" \
    --out "${OUT}/${name}" --list "${WORK}/${name}.txt" "$@"
  awk -v b="$name" '{ print b, $0 }' "${WORK}/${name}.txt" >> "${OUT}/felis-images-linux-${arch}.txt"
}

for arch in $ARCHES; do
  # The control-plane image wraps the released binary itself, byte for byte, the way
  # bootstrap wraps a downloaded binary.
  log "building the linux/${arch} images"
  mkdir -p "${WORK}/felis-${arch}"
  cp "${OUT}/felis-linux-${arch}" "${WORK}/felis-${arch}/felis"
  cat > "${WORK}/felis-${arch}/Dockerfile" <<EOF
FROM ${FELIS_BASE_IMAGE}
ENV PATH=/usr/local/bin:/usr/bin:/bin
COPY --chmod=0755 felis /usr/local/bin/felis
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/felis"]
EOF
  oci_image "$arch" "${WORK}/felis-${arch}.tar" "${WORK}/felis-${arch}"
  for role in limbo lobby paper; do
    oci_image "$arch" "${WORK}/${role}-${arch}.tar" -f "deploy/${role}/Dockerfile" "${game_build_args[@]}" .
  done

  bundle "$arch" felis --layout "felis=${FELIS_IMAGE}=${WORK}/felis-${arch}.tar"
  bundle "$arch" game \
    --layout "limbo=${REGISTRY_URL}/felis/limbo:demo=${WORK}/limbo-${arch}.tar" \
    --layout "lobby=${REGISTRY_URL}/felis/lobby:demo=${WORK}/lobby-${arch}.tar" \
    --layout "paper=${REGISTRY_URL}/felis/paper:demo=${WORK}/paper-${arch}.tar"
  bundle "$arch" base --pull "registry=${REGISTRY_IMAGE}" --pull "postgres=${POSTGRES_IMAGE}"
  rm -f "${WORK}"/*-"${arch}".tar
done

log "writing SHA256SUMS"
(cd "$OUT" && sha256sum -- *) > "${WORK}/SHA256SUMS"
mv "${WORK}/SHA256SUMS" "${OUT}/SHA256SUMS"
ls -l "$OUT"
