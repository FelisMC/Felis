# Felis lobby image: Paper + the felis-paper /menu plugin + LuckPerms.
#
# CODE-ONLY in this repo — not built by the Go CI. It packages the always-on
# "lobby" hub the setup provisioner points [velocity] lobby_image at. The lobby is
# the POST-auth /menu hub: it is reached only when the login gate transfers an
# authenticated player onward, and it must never be a fallback target.
#
# Build (deploy/bootstrap.sh does this for you, with the URLs and digests
# deploy/game-stack.lock names):
#   . <(grep -E '^(PAPER|LUCKPERMS)_' deploy/game-stack.lock)
#   docker build -f deploy/lobby/Dockerfile \
#     --build-arg PAPER_JAR_URL="$PAPER_JAR_URL" --build-arg PAPER_JAR_SHA256="$PAPER_JAR_SHA256" \
#     --build-arg LUCKPERMS_JAR_URL="$LUCKPERMS_JAR_URL" \
#     --build-arg LUCKPERMS_JAR_SHA256="$LUCKPERMS_JAR_SHA256" \
#     -t felis-lobby:demo .
#   docker save felis-lobby:demo | sudo k3s ctr images import -
#   # felis.toml → [velocity] lobby_image = "felis-lobby:demo"
#
# The Paper version must match the LOGIN gate's: LOOHP/Limbo speaks exactly ONE protocol
# per build (its SERVER_IMPLEMENTATION_VERSION), and a client has to satisfy both hops.
#
# Contract: the game server listens on 25565 (the CRD GamePort). The lobby speaks only the
# felis:control plugin-message channel (spec §12) — it holds no felis-api token. It DOES
# receive FELIS_FORWARDING_SECRET (operator-injected from the felis-forwarding-secret
# Secret) and refuses to start without it: a lobby that cannot verify the proxy's signed
# handshake would trust an offline, forgeable UUID.

# ---- build the felis-paper plugin jar (Paper API is Java 25) ----
# The official Gradle image on JDK 25, the Gradle version plugins/*/gradle/wrapper pins.
# The limbo Dockerfile and bootstrap's velocity build use this same image, digest and all
# (bootstrap_asset_test.go holds the three together). The image's own `gradle` runs the
# build rather than the module's wrapper, which would download the same distribution
# again on every image build. The build checks every dependency against
# plugins/paper/gradle/verification-metadata.xml and fails on a mismatch.
FROM gradle:9.8.0-jdk25@sha256:2b2fc1b1dfc3604a2acc916839f36eb5ee48fd7f232427fc5faca224c73bcb01 AS plugin
WORKDIR /src
COPY plugins/paper/ ./plugins/paper/
COPY plugins/shared/ ./plugins/shared/
RUN cd plugins/paper \
    && gradle --no-daemon build \
    && cp build/libs/*.jar /felis-paper.jar

# ---- assemble the runtime ----
# 25-jre, not 21: Paper 26.2 declares `java.version.minimum = 25` (PaperMC Fill v3,
# GET /v3/projects/paper/versions/26.2) and refuses to boot on anything older. A 25 JRE
# also runs the plugin's Java-21 bytecode, so only the runtime moves.
FROM eclipse-temurin:25-jre@sha256:bb036ed6cfdc57e3da7c22634d15f1b840d2caf76183861c80e81ca4b5104abb
ARG PAPER_JAR_URL
# Required alongside the URL: Fill's URLs are content-addressed, but nothing enforces
# that shape at build time. Checking the digest after the download turns a truncated or
# tampered fetch into a failed build instead of a lobby booted on the wrong bytes.
ARG PAPER_JAR_SHA256
# LuckPerms is required, not optional: the panel's whole permission surface
# (internal/api/handlers_access.go) issues `lp user ...` over RCON, so a lobby built
# without it answers every grant with "Unknown command" — a failure the operator only
# discovers in production, because the server itself starts and runs perfectly well.
# Failing the build is the cheap place to notice. Passed in rather than pinned here for
# the same reason PAPER_JAR_URL is: deploy/game-stack.lock names the build, so this file
# does not change on every LuckPerms release. The digest is required like Paper's; the
# jar runs inside the lobby with the server's full permissions.
ARG LUCKPERMS_JAR_URL
ARG LUCKPERMS_JAR_SHA256
WORKDIR /paper
RUN set -eu; \
    if [ -z "${PAPER_JAR_URL:-}" ]; then \
      echo "ERROR: --build-arg PAPER_JAR_URL=<paper jar> is required" >&2; exit 1; \
    fi; \
    if [ -z "${PAPER_JAR_SHA256:-}" ]; then \
      echo "ERROR: --build-arg PAPER_JAR_SHA256=<paper jar sha256> is required" >&2; exit 1; \
    fi; \
    if [ -z "${LUCKPERMS_JAR_URL:-}" ]; then \
      echo "ERROR: --build-arg LUCKPERMS_JAR_URL=<luckperms bukkit jar> is required" >&2; exit 1; \
    fi; \
    if [ -z "${LUCKPERMS_JAR_SHA256:-}" ]; then \
      echo "ERROR: --build-arg LUCKPERMS_JAR_SHA256=<luckperms jar sha256> is required" >&2; exit 1; \
    fi; \
    apt-get update && apt-get install -y --no-install-recommends curl ca-certificates; \
    mkdir -p /paper/plugins; \
    curl -fSL "$PAPER_JAR_URL" -o /paper/paper.jar; \
    echo "$PAPER_JAR_SHA256  /paper/paper.jar" | sha256sum -c; \
    curl -fSL "$LUCKPERMS_JAR_URL" -o /paper/plugins/LuckPerms.jar; \
    echo "$LUCKPERMS_JAR_SHA256  /paper/plugins/LuckPerms.jar" | sha256sum -c; \
    apt-get purge -y curl && apt-get autoremove -y && rm -rf /var/lib/apt/lists/*; \
    echo "eula=true" > /paper/eula.txt
COPY --from=plugin /felis-paper.jar /paper/plugins/felis-paper.jar
# The entrypoint writes the Velocity modern-forwarding config (and REFUSES to start
# without the secret — an offline-mode lobby would trust forged identities) before
# launching Paper.
COPY deploy/lobby/entrypoint.sh /usr/local/bin/felis-entrypoint.sh

# The operator mounts the world PVC at /data. Runtime state lives there; /paper
# remains the immutable image seed copied into the volume by the entrypoint.
WORKDIR /data
# Run as the game uid (naming.GameUID in the Go tree). The operator pins the same uid in
# the pod securityContext whatever USER an image declares; declaring it here as well
# keeps a plain `docker run` of this image off root, and chowning the empty /data seed
# lets that run write its world. The jar seed above stays root-owned and read-only to
# the server.
RUN chown 1000:1000 /data
USER 1000:1000

# FELIS_GAME_PORT is the port the entrypoint pins Paper to; it MUST equal the operator's
# GamePort (internal/operator/builders.go). Default 25565 — override only in lockstep
# with the operator.
ENV FELIS_GAME_PORT=25565
EXPOSE 25565
# felis-entrypoint.sh writes config/paper-global.yml + server.properties, then execs
# `java -jar paper.jar --nogui` from /data (headless: the pod has no console). Invoked via
# `sh` so no +x bit is needed from the (Windows) build host.
ENTRYPOINT ["/bin/sh", "/usr/local/bin/felis-entrypoint.sh"]
