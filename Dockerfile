# Workloads and host commands share /usr/local/bin/felis; imported images use non-latest tags for IfNotPresent.

# Build on BUILDPLATFORM and cross-compile Go; keep the target runtime stage COPY-only to avoid QEMU.
FROM --platform=$BUILDPLATFORM node:22-bookworm@sha256:363e1587494626837fa7f9a23bdb453d13b0ff3c67c705c2805cfc69c2d2fad7 AS panel
WORKDIR /panel
COPY panel/package*.json ./
RUN npm ci
COPY panel/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.27.2@sha256:820153096fda4de6ac1b7194d12e416b7dd8f4e9a33d17e0345b111b945162d8 AS build
WORKDIR /src
ARG TARGETOS=linux
ARG TARGETARCH
# Prime the module cache first so source-only edits do not re-download deps.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=panel /panel/dist ./internal/panel/static
# Declare version after dependency downloads for cache reuse; require TARGETARCH and stamp main.version.
ARG FELIS_VERSION=dev
RUN CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="${TARGETARCH:-$(go env GOARCH)}" \
    go build -trimpath -ldflags="-s -w -X main.version=${FELIS_VERSION}" -o /out/felis ./cmd/felis

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
ENV PATH=/usr/local/bin:/usr/bin:/bin
COPY --chmod=0755 --from=build /out/felis /usr/local/bin/felis
# The static binary supports both distroless UID 65532 and the operator's UID 1000 on a read-only root filesystem.
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/felis"]
