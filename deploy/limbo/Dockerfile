# Felis login-limbo image: LOOHP/Limbo + the felis-limbo readiness plugin.
#
# CODE-ONLY in this repo — it is not built by the Go CI. It packages the always-on
# "login" auth gate the setup provisioner points [velocity] login_image at.
#
# LOOHP/Limbo has no official image and no release zip. Its CI
# (ci.loohpjames.com/job/Limbo) publishes two LOOSE artifacts per build:
#   - target/Limbo-<ver>.jar   (the server jar; Main-Class com.loohp.limbo.Limbo)
#   - spawn.schem              (the default spawn schematic)
# There is no bundled server.properties — Limbo writes a default on first run.
# So the runtime is assembled from those two URLs (not a zip) via --build-arg:
#
#   . <(grep '^LIMBO_' deploy/game-stack.lock)
#   docker build -f deploy/limbo/Dockerfile \
#     --build-arg LIMBO_JAR_URL="$LIMBO_JAR_URL" --build-arg LIMBO_JAR_SHA256="$LIMBO_JAR_SHA256" \
#     --build-arg LIMBO_SCHEM_URL="$LIMBO_SCHEM_URL" --build-arg LIMBO_SCHEM_SHA256="$LIMBO_SCHEM_SHA256" \
#     --build-arg LIMBO_VERSION="$LIMBO_VERSION" \
#     -t felis-limbo:demo .
#
# Note LIMBO_VERSION (the maven API version the plugin compiles against, e.g.
# 2026.0.2-ALPHA) differs from the jar's CI build-qualified filename (e.g.
# Limbo-2026.0.2-ALPHA-26.2.jar): the CI qualifier is not published to the maven
# repo. Then import it the same way as the control-plane image (k3s ctr import)
# and set [velocity] login_image = "felis-limbo:demo" in felis.toml before setup.
#
# Two contracts the operator depends on:
#   1. The game server listens on 25565 (the CRD's GamePort).
#   2. The felis-limbo readiness endpoint listens on 8080 (StartupSpec.HealthHTTPPort,
#      overridable via FELIS_HEALTH_PORT) and returns 200 only after the first tick.

# ---- build the felis-limbo plugin jar ----
# gradle:*-jdk21 — an official Gradle image on JDK 21. JDK 21 is required because
# current LOOHP/Limbo releases ship Java 21 API classes (class-file major 65); a
# JDK 17 fails to read them with "wrong version 65.0, should be 61.0". The image
# also provides the `gradle` binary (this tree vendors no Gradle wrapper).
# build.gradle still targets release 17 bytecode so the plugin loads on Java 17+.
FROM gradle:8.14-jdk21@sha256:5c4c0c4284de4a19951e82ac78f86dbcda2e136644bbfe159beba7ea3420cc80 AS plugin
WORKDIR /src
# Copy what the limbo module needs: its own tree plus the shared link core it
# srcDir-includes (../shared/src/main/java → /src/plugins/shared/src/main/java), so
# the account-link client + config loader compile straight into the jar.
COPY plugins/limbo/ ./plugins/limbo/
COPY plugins/shared/ ./plugins/shared/
ARG LIMBO_VERSION=+
RUN cd plugins/limbo \
    && (test -x ./gradlew && ./gradlew --no-daemon -PlimboVersion="$LIMBO_VERSION" build \
        || gradle --no-daemon -PlimboVersion="$LIMBO_VERSION" build) \
    && cp build/libs/*.jar /felis-limbo.jar

# ---- assemble the runtime ----
# 21-jre: the Limbo jar is Java 21 bytecode (class-file major 65), so a Java 17
# JRE cannot run it (UnsupportedClassVersionError). A 21 JRE also runs the
# plugin's release-17 bytecode fine.
FROM eclipse-temurin:21-jre@sha256:49e21e16e3c86eb7816a44a67549910ed090fbeb40c29c525d58bf5e02e91b0f
ARG LIMBO_JAR_URL
ARG LIMBO_JAR_SHA256
ARG LIMBO_SCHEM_URL
ARG LIMBO_SCHEM_SHA256
WORKDIR /limbo
# Pull the two loose LOOHP/Limbo CI artifacts: the server jar (required, saved as
# Limbo.jar) and the default spawn schematic (optional). Each is checked against the
# digest deploy/game-stack.lock names (bootstrap.sh passes it): the login gate is the
# first thing every player's connection reaches, and Limbo's CI publishes no digest of
# its own.
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
# The entrypoint pins the game port AND enables Velocity modern forwarding — refusing to
# start without the secret, because a login gate that derives offline UUIDs would let
# anyone claim any Minecraft identity (the Owner's included).
COPY deploy/limbo/entrypoint.sh /usr/local/bin/felis-entrypoint.sh

# The operator mounts the world PVC at /data. Runtime state lives there; /limbo
# remains the immutable image seed copied into the volume by the entrypoint.
WORKDIR /data
# Run as the game uid (naming.GameUID in the Go tree). The operator pins the same uid in
# the pod securityContext whatever USER an image declares; declaring it here as well
# keeps a plain `docker run` of this image off root, and chowning the empty /data seed
# lets that run write its world. The jar seed above stays root-owned and read-only to
# the server.
RUN chown 1000:1000 /data
USER 1000:1000

ENV FELIS_HEALTH_PORT=8080
# FELIS_GAME_PORT is the port the entrypoint pins Limbo to; it MUST equal the operator's
# GamePort (internal/operator/builders.go). Default 25565 — override only in lockstep
# with the operator.
ENV FELIS_GAME_PORT=25565
EXPOSE 25565 8080
# felis-entrypoint.sh pins server-port + velocity-modern/forwarding-secrets, then execs
# `java -jar Limbo.jar --nogui` from /data (headless: the pod has no console). Limbo writes
# the rest of server.properties on the persistent volume and loads ./spawn.schem as the
# spawn world. Invoked via `sh` so no +x bit is needed from the (Windows) build host.
ENTRYPOINT ["/bin/sh", "/usr/local/bin/felis-entrypoint.sh"]
