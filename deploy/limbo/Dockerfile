# Lock Limbo artifacts; use game port 25565 and health port 8080, ready only after the first tick.

# Build platform-independent plugin bytecode on the build architecture, with JDK 21+ for dependencies.
FROM --platform=$BUILDPLATFORM gradle:9.8.1-jdk25@sha256:4a80c8083135754f689c3e31bb858a22282c66f5625cafb09ed217814b363ee5 AS plugin
WORKDIR /src
# Include the shared link sources alongside the Limbo module so they compile into its JAR.
COPY plugins/limbo/ ./plugins/limbo/
COPY plugins/shared/ ./plugins/shared/
# Require the locked Limbo API version and its verified dependency checksum.
ARG LIMBO_VERSION
RUN if [ -z "${LIMBO_VERSION:-}" ]; then \
        echo "LIMBO_VERSION is required (deploy/game-stack.lock)" >&2; exit 1; \
    fi \
    && cd plugins/limbo \
    && gradle --no-daemon --console=plain --init-script ../shared/build-progress.gradle -PlimboVersion="$LIMBO_VERSION" build \
    && cp build/libs/*.jar /felis-limbo.jar

# 21-jre: the Limbo jar is Java 21 bytecode, so a Java 17 JRE cannot run it. A 21 JRE also runs the plugin's release-17 bytecode.
FROM eclipse-temurin:21-jre@sha256:cff19e6215689161eb6162c11b86b0c60ddf802164f2eaf48d570f8fb79a36c5
ARG LIMBO_JAR_URL
ARG LIMBO_JAR_SHA256
ARG LIMBO_SCHEM_URL
ARG LIMBO_SCHEM_SHA256
WORKDIR /limbo
# Verify the required Limbo JAR and optional spawn schematic against lock-file digests.
RUN set -eu; \
    if [ -z "${LIMBO_JAR_URL:-}" ]; then \
      echo "ERROR: --build-arg LIMBO_JAR_URL=<Limbo server jar> is required" >&2; exit 1; \
    fi; \
    if [ -z "${LIMBO_JAR_SHA256:-}" ]; then \
      echo "ERROR: --build-arg LIMBO_JAR_SHA256=<Limbo jar sha256> is required" >&2; exit 1; \
    fi; \
    if [ -n "${LIMBO_SCHEM_URL:-}" ] && [ -z "${LIMBO_SCHEM_SHA256:-}" ]; then \
      echo "ERROR: --build-arg LIMBO_SCHEM_SHA256=<spawn.schem sha256> is required with LIMBO_SCHEM_URL" >&2; exit 1; \
    fi; \
    apt-get update && apt-get install -y --no-install-recommends curl ca-certificates; \
    curl -fSL "$LIMBO_JAR_URL" -o /limbo/Limbo.jar; \
    echo "$LIMBO_JAR_SHA256  /limbo/Limbo.jar" | sha256sum -c; \
    if [ -n "${LIMBO_SCHEM_URL:-}" ]; then \
      curl -fSL "$LIMBO_SCHEM_URL" -o /limbo/spawn.schem; \
      echo "$LIMBO_SCHEM_SHA256  /limbo/spawn.schem" | sha256sum -c; \
    fi; \
    apt-get purge -y curl && apt-get autoremove -y && rm -rf /var/lib/apt/lists/*; \
    mkdir -p /limbo/plugins
# Drop the login+readiness plugin in beside Limbo.jar.
COPY --from=plugin /felis-limbo.jar /limbo/plugins/felis-limbo.jar
# Refuse missing forwarding secrets before the authentication gate can accept forged offline identities.
COPY deploy/limbo/entrypoint.sh /usr/local/bin/felis-entrypoint.sh

# The operator mounts the world PVC at /data. Runtime state lives there; /limbo
# remains the immutable image seed copied into the volume by the entrypoint.
WORKDIR /data
# Use the game UID and a writable /data seed for standalone container runs.
# The jar seed stays root-owned and read-only to the server.
RUN chown 1000:1000 /data
USER 1000:1000

ENV FELIS_HEALTH_PORT=8080
# Keep FELIS_GAME_PORT aligned with the operator's GamePort.
ENV FELIS_GAME_PORT=25565
EXPOSE 25565 8080
# Invoke the entrypoint with sh so Windows checkout executable bits are irrelevant.
ENTRYPOINT ["/bin/sh", "/usr/local/bin/felis-entrypoint.sh"]
