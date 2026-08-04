# Felis lobby image: Paper + the felis-paper /menu plugin + LuckPerms.
#
# CODE-ONLY in this repo — not built by the Go CI. It packages the always-on
# "lobby" hub the setup provisioner points [velocity] lobby_image at. The lobby is
# the POST-auth /menu hub: it is reached only when the login gate transfers an
# authenticated player onward, and it must never be a fallback target.
#
# Build (deploy/bootstrap.sh does this for you; the PAPER_JAR_URL comes from PaperMC's
# Fill v3 API — api.papermc.io v2 has returned HTTP 410 since 2026-07-01):
#   docker build -f deploy/lobby/Dockerfile \
#     --build-arg PAPER_JAR_URL=https://fill-data.papermc.io/v1/objects/<sha>/paper-26.2-<build>.jar \
#     --build-arg PAPER_JAR_SHA256=<that same sha — the objects/ path segment> \
#     --build-arg LUCKPERMS_JAR_URL="$(curl -fsSL https://metadata.luckperms.net/data/all \
#       | grep -o 'https://download.luckperms.net/[^"]*/bukkit/loader/[^"]*\.jar')" \
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

# ---- build the felis-paper plugin jar (Paper API is Java 21) ----
# gradle:8.14-jdk21 — an official Gradle image on JDK 21 (this tree vendors no Gradle
# wrapper, and a bare JDK image ships no `gradle`). JDK 21 matches the Paper API.
FROM gradle:8.14-jdk21 AS plugin
WORKDIR /src
COPY plugins/paper/ ./plugins/paper/
COPY plugins/shared/ ./plugins/shared/
RUN cd plugins/paper \
    && (test -x ./gradlew && ./gradlew --no-daemon build \
        || gradle --no-daemon build) \
    && cp build/libs/*.jar /felis-paper.jar

# ---- assemble the runtime ----
# 25-jre, not 21: Paper 26.2 declares `java.version.minimum = 25` (PaperMC Fill v3,
# GET /v3/projects/paper/versions/26.2) and refuses to boot on anything older. A 25 JRE
# also runs the plugin's Java-21 bytecode, so only the runtime moves.
FROM eclipse-temurin:25-jre
ARG PAPER_JAR_URL
# Required alongside the URL: Fill's URLs are content-addressed, but nothing enforces
# that shape at build time. Checking the digest after the download turns a truncated or
# tampered fetch into a failed build instead of a lobby booted on the wrong bytes.
ARG PAPER_JAR_SHA256
# LuckPerms is required, not optional: the panel's whole permission surface
# (internal/api/handlers_access.go) issues `lp user ...` over RCON, so a lobby built
# without it answers every grant with "Unknown command" — a failure the operator only
# discovers in production, because the server itself starts and runs perfectly well.
# Failing the build is the cheap place to notice. Resolved by URL rather than pinned
# here for the same reason PAPER_JAR_URL is: bootstrap.sh asks upstream for the current
# build, so this file does not go stale on every LuckPerms release.
ARG LUCKPERMS_JAR_URL
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
    apt-get update && apt-get install -y --no-install-recommends curl ca-certificates; \
    mkdir -p /paper/plugins; \
    curl -fSL "$PAPER_JAR_URL" -o /paper/paper.jar; \
    echo "$PAPER_JAR_SHA256  /paper/paper.jar" | sha256sum -c; \
    curl -fSL "$LUCKPERMS_JAR_URL" -o /paper/plugins/LuckPerms.jar; \
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

# FELIS_GAME_PORT is the port the entrypoint pins Paper to; it MUST equal the operator's
# GamePort (internal/operator/builders.go). Default 25565 — override only in lockstep
# with the operator.
ENV FELIS_GAME_PORT=25565
EXPOSE 25565
# felis-entrypoint.sh writes config/paper-global.yml + server.properties, then execs
# `java -jar paper.jar --nogui` from /data (headless: the pod has no console). Invoked via
# `sh` so no +x bit is needed from the (Windows) build host.
ENTRYPOINT ["/bin/sh", "/usr/local/bin/felis-entrypoint.sh"]
