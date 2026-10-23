# Post-auth lobby requires signed forwarding and the same protocol as the login gate.

# Use pinned Gradle/JDK 25 and dependency verification; build architecture-independent JARs without target emulation.
FROM --platform=$BUILDPLATFORM gradle:9.8.1-jdk25@sha256:4a80c8083135754f689c3e31bb858a22282c66f5625cafb09ed217814b363ee5 AS plugin
WORKDIR /src
COPY plugins/paper/ ./plugins/paper/
COPY plugins/shared/ ./plugins/shared/
RUN cd plugins/paper \
    && gradle --no-daemon --console=plain --init-script ../shared/build-progress.gradle build \
    && cp build/libs/*.jar /felis-paper.jar

# 25-jre, not 21: Paper 26.2 declares java.version.minimum = 25 and refuses to boot on anything older. A 25 JRE also runs the plugin's Java 21 bytecode, so only the runtime moves.
FROM eclipse-temurin:25-jre@sha256:fcd7fd7b387f94bb2ac461478a7436ad8e349924c374ea8313919624dceae636
ARG PAPER_JAR_URL
# Verify Paper's supplied digest even when the URL appears content-addressed.
ARG PAPER_JAR_SHA256
# Require checksum-verified LuckPerms so permission commands work without trusting unchecked plugin bytes.
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
# Use the game UID and a writable /data seed for standalone container runs.
# The jar seed stays root-owned and read-only to the server.
RUN chown 1000:1000 /data
USER 1000:1000

# Keep FELIS_GAME_PORT aligned with the operator's GamePort.
ENV FELIS_GAME_PORT=25565
EXPOSE 25565
# Invoke through sh and run Paper headlessly from the world volume.
ENTRYPOINT ["/bin/sh", "/usr/local/bin/felis-entrypoint.sh"]
