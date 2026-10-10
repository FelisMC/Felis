# Felis control-plane image.
#
# Builds the panel, then the single multi-call `felis` binary (api / operator /
# migrate / reaper / restore / manifests / setup) as a static, CGO-free
# executable and ships it on a distroless base. Two contracts the rendered
# Deployments depend on:
#
#   1. The binary lives at /usr/local/bin/felis, and rendered Kubernetes
#      workloads invoke that absolute path rather than relying on PATH lookup.
#   2. The image is meant to be imported into a local containerd (k3s ctr import)
#      and referenced by a NON-:latest tag (e.g. felis:demo). k8s then resolves
#      the default IfNotPresent pull policy against the imported image instead of
#      trying to pull it from a registry that does not exist yet.
#
# deploy/bootstrap.sh also extracts this same binary onto the host (docker cp)
# so `felis migrate up` and the `felis setup` TUI run with the identical build.

# Both BUILD stages are pinned to the BUILDPLATFORM so a multi-platform buildx run never
# emulates them: the panel's output is plain JS and identical on every architecture, and the
# Go stage cross-compiles natively via TARGETARCH below. Under QEMU an `npm ci` alone costs
# minutes. The FINAL stage is deliberately NOT pinned — it must stay on the target platform
# or the published arm64 image would carry amd64 layers. It contains only COPY, which
# BuildKit performs itself, so it needs no QEMU either; adding a RUN there would.
#
# Every base image here and in deploy/{limbo,lobby,paper} is pinned by digest, so a rebuild
# of one release uses the same bytes; .github/dependabot.yml proposes the bumps (tag and
# digest together).
FROM --platform=$BUILDPLATFORM node:22-bookworm@sha256:363e1587494626837fa7f9a23bdb453d13b0ff3c67c705c2805cfc69c2d2fad7 AS panel
WORKDIR /panel
COPY panel/package*.json ./
RUN npm ci
COPY panel/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.26.9@sha256:f1f0bcc2c524a3ced375fcb4d1ecb7aa371aa7070e112599aaca45cc02d0101b AS build
WORKDIR /src
ARG TARGETOS=linux
ARG TARGETARCH
# Prime the module cache first so source-only edits do not re-download deps.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=panel /panel/dist ./internal/panel/static
# Declared HERE, not beside TARGETOS above: changing it invalidates every layer that
# follows, and the go mod download layer must survive a version bump.
#
# The stamp is what makes `felis version` and `felis update` mean anything — unstamped,
# main.version stays "dev" and the updater refuses to compare rather than treating it as
# 0.0.0. deploy/bootstrap.sh computes the value per channel; see the FELIS_VERSION_BOOTSTRAP
# block there for why the dev channel uses "+" build metadata and not `git describe`.
#
# The ${TARGETARCH:-...} fallback is a trap now that this stage is pinned to BUILDPLATFORM:
# `go env GOARCH` reports the BUILDER's architecture, so an empty TARGETARCH (a legacy
# `docker build`, or a setup-buildx step that quietly did not take) produces a working amd64
# binary that gets published under the arm64 name. .github/workflows/release.yml asserts the
# ELF machine type with file(1) for exactly this reason — an exec-based check cannot catch it,
# because CI runners have binfmt/QEMU registered and will happily run the wrong one.
ARG FELIS_VERSION=dev
RUN CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="${TARGETARCH:-$(go env GOARCH)}" \
    go build -trimpath -ldflags="-s -w -X main.version=${FELIS_VERSION}" -o /out/felis ./cmd/felis

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
ENV PATH=/usr/local/bin:/usr/bin:/bin
COPY --chmod=0755 --from=build /out/felis /usr/local/bin/felis
# distroless "nonroot" is uid 65532; the rendered PodSecurityContext pins
# runAsUser 1000 at deploy time, and a static binary needs no /etc/passwd entry,
# so either uid runs the same binary from a read-only root filesystem.
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/felis"]
