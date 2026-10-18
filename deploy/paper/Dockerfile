# The user Paper image receives forwarding configuration from the operator and matches the locked login protocol.

# 25-jre, not 21: Paper 26.2 declares java.version.minimum=25 (PaperMC Fill v3) and refuses
# to boot on anything older.
FROM eclipse-temurin:25-jre@sha256:fcd7fd7b387f94bb2ac461478a7436ad8e349924c374ea8313919624dceae636
ARG PAPER_JAR_URL
# Verify Paper's supplied digest even when the URL appears content-addressed.
ARG PAPER_JAR_SHA256
RUN set -eu; \
    if [ -z "${PAPER_JAR_URL:-}" ]; then \
      echo "ERROR: --build-arg PAPER_JAR_URL=<paper jar> is required" >&2; exit 1; \
    fi; \
    if [ -z "${PAPER_JAR_SHA256:-}" ]; then \
      echo "ERROR: --build-arg PAPER_JAR_SHA256=<paper jar sha256> is required" >&2; exit 1; \
    fi; \
    apt-get update && apt-get install -y --no-install-recommends curl ca-certificates; \
    mkdir -p /paper; \
    curl -fSL "$PAPER_JAR_URL" -o /paper/paper.jar; \
    echo "$PAPER_JAR_SHA256  /paper/paper.jar" | sha256sum -c; \
    apt-get purge -y curl && apt-get autoremove -y && rm -rf /var/lib/apt/lists/*
COPY deploy/paper/entrypoint.sh /usr/local/bin/felis-entrypoint.sh

# Keep runtime data on /data and the immutable image JAR under /paper, outside file-editor access.
WORKDIR /data
# Use the game UID and a writable /data seed for standalone container runs.
# The jar seed stays root-owned and read-only to the server.
RUN chown 1000:1000 /data
USER 1000:1000

# Keep FELIS_GAME_PORT aligned with the operator's GamePort.
ENV FELIS_GAME_PORT=25565
EXPOSE 25565
# felis-entrypoint.sh writes eula.txt + server.properties, then execs `java -jar
# /paper/paper.jar --nogui` from /data. Invoked via `sh` so no +x bit is needed from the
# (Windows) build host.
ENTRYPOINT ["/bin/sh", "/usr/local/bin/felis-entrypoint.sh"]
