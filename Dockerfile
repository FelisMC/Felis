# Felis control-plane image.
#
# Builds the single multi-call `felis` binary (api / operator / migrate / reaper
# / restore / manifests / setup) as a static, CGO-free executable and ships it on
# a distroless base. Two contracts the rendered Deployments depend on:
#
#   1. The binary lives on PATH at /usr/local/bin/felis, because the bundle
#      invokes it by bare name (`command: ["felis", ...]` in workloads.go). PATH
#      is pinned explicitly so this holds regardless of base-image defaults.
#   2. The image is meant to be imported into a local containerd (k3s ctr import)
#      and referenced by a NON-:latest tag (e.g. felis:demo). k8s then resolves
#      the default IfNotPresent pull policy against the imported image instead of
#      trying to pull it from a registry that does not exist yet.
#
# deploy/bootstrap.sh also extracts this same binary onto the host (docker cp)
# so `felis migrate up` and the `felis setup` TUI run with the identical build.

FROM golang:1.26 AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOOS=linux GOARCH=amd64
# Prime the module cache first so source-only edits do not re-download deps.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /out/felis ./cmd/felis

FROM gcr.io/distroless/static-debian12:nonroot
# Guarantee bare `felis` resolves no matter what PATH the base image ships.
ENV PATH=/usr/local/bin:/usr/bin:/bin
COPY --from=build /out/felis /usr/local/bin/felis
# distroless "nonroot" is uid 65532; the rendered PodSecurityContext pins
# runAsUser 1000 at deploy time, and a static binary needs no /etc/passwd entry,
# so either uid runs the same binary from a read-only root filesystem.
USER 65532:65532
ENTRYPOINT ["felis"]
