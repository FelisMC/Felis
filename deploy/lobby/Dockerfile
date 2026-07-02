# Felis lobby image: Paper + the felis-paper /menu plugin.
#
# CODE-ONLY in this repo — not built by the Go CI. It packages the always-on
# "lobby" hub the setup provisioner points [velocity] lobby_image at. The lobby is
# the POST-auth /menu hub: it is reached only when the login gate transfers an
# authenticated player onward, and it must never be a fallback target.
#
# Build:
#   docker build -f deploy/lobby/Dockerfile \
#     --build-arg PAPER_JAR_URL=https://<mirror>/paper-1.21.x-<build>.jar \
#     -t felis-lobby:demo .
#   docker save felis-lobby:demo | sudo k3s ctr images import -
#   # felis.toml → [velocity] lobby_image = "felis-lobby:demo"
#
# Contract: the game server listens on 25565 (the CRD GamePort). The lobby speaks
# only the felis:control plugin-message channel (spec §12) — it holds no felis-api
# token.

# ---- build the felis-paper plugin jar (Paper API is Java 21) ----
FROM eclipse-temurin:21-jdk AS plugin
WORKDIR /src
COPY plugins/paper/ ./plugins/paper/
COPY plugins/shared/ ./plugins/shared/
RUN cd plugins/paper \
    && (test -x ./gradlew && ./gradlew --no-daemon build \
        || gradle --no-daemon build) \
    && cp build/libs/*.jar /felis-paper.jar

# ---- assemble the runtime ----
FROM eclipse-temurin:21-jre
ARG PAPER_JAR_URL
WORKDIR /paper
RUN set -eu; \
    if [ -z "${PAPER_JAR_URL:-}" ]; then \
      echo "ERROR: --build-arg PAPER_JAR_URL=<paper jar> is required" >&2; exit 1; \
    fi; \
    apt-get update && apt-get install -y --no-install-recommends curl ca-certificates; \
    curl -fSL "$PAPER_JAR_URL" -o /paper/paper.jar; \
    apt-get purge -y curl && apt-get autoremove -y && rm -rf /var/lib/apt/lists/*; \
    mkdir -p /paper/plugins; \
    echo "eula=true" > /paper/eula.txt
COPY --from=plugin /felis-paper.jar /paper/plugins/felis-paper.jar

EXPOSE 25565
# nogui headless; the first boot generates server.properties (align online-mode /
# forwarding with the Velocity proxy afterwards — see README).
ENTRYPOINT ["java", "-jar", "paper.jar", "--nogui"]
