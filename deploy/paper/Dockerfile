# Felis general-purpose Paper image: plain Paper, forwarding-ready.
#
# CODE-ONLY in this repo — not built by the Go CI. This is a drop-in base for a user's
# OWN world, offered as a platform-recommended image (see
# internal/store/migrations/0019_recommended_paper.sql). It is NOT a system server: it
# carries no felis-paper /menu plugin, no LuckPerms, and no forwarding-secret gate.
#
# It writes NO Velocity forwarding config itself. The operator injects a root
# `felis init-forwarding` initContainer into every USER server (internal/operator/
# builders.go: buildStatefulSet) that writes config/paper-global.yml + server.properties
# online-mode=false onto the /data PVC before this container starts. That external step is
# what makes an arbitrary Paper image joinable through the modern-forwarding proxy — so
# this image needs no forwarding logic of its own, and by the same mechanism ANY Paper
# image a user brings is made joinable the same way. If the initContainer is absent (no
# FELIS_IMAGE configured) Paper boots as a standalone online server: degraded, not broken.
#
# Build (deploy/bootstrap.sh does this for you; PAPER_JAR_URL comes from PaperMC's Fill v3
# API — the SAME url the lobby build resolves, so this reuses it and adds no new dependency):
#   docker build -f deploy/paper/Dockerfile \
#     --build-arg PAPER_JAR_URL=https://fill-data.papermc.io/v1/objects/<sha>/paper-<ver>-<build>.jar \
#     -t felis-paper:demo .
#   docker save felis-paper:demo | sudo k3s ctr images import -
#   # felis.toml → recommended via 0019_recommended_paper.sql (no [velocity] key points here)
#
# The Paper version must match MC_VERSION: the login gate (LOOHP/Limbo) speaks exactly ONE
# protocol per build, and a client that passes the gate must also reach this backend.
# bootstrap resolves both Paper and Limbo from the same MC_VERSION, so they always agree.

# 25-jre, not 21: Paper 26.2 declares java.version.minimum=25 (PaperMC Fill v3) and refuses
# to boot on anything older.
FROM eclipse-temurin:25-jre
ARG PAPER_JAR_URL
RUN set -eu; \
    if [ -z "${PAPER_JAR_URL:-}" ]; then \
      echo "ERROR: --build-arg PAPER_JAR_URL=<paper jar> is required" >&2; exit 1; \
    fi; \
    apt-get update && apt-get install -y --no-install-recommends curl ca-certificates; \
    mkdir -p /paper; \
    curl -fSL "$PAPER_JAR_URL" -o /paper/paper.jar; \
    apt-get purge -y curl && apt-get autoremove -y && rm -rf /var/lib/apt/lists/*
COPY deploy/paper/entrypoint.sh /usr/local/bin/felis-entrypoint.sh

# The operator mounts the world PVC at /data and the entrypoint runs Paper with it as the
# working directory, so worlds, generated config and paperclip's extracted runtime all land
# on the PVC. /paper stays the immutable image seed: the jar is never copied onto the
# volume, so the panel file editor (which sees only /data) cannot tamper with it.
WORKDIR /data

# FELIS_GAME_PORT is the port the entrypoint pins Paper to; it MUST equal the operator's
# GamePort (internal/operator/builders.go). Default 25565 — override only in lockstep with
# the operator.
ENV FELIS_GAME_PORT=25565
EXPOSE 25565
# felis-entrypoint.sh writes eula.txt + server.properties, then execs `java -jar
# /paper/paper.jar --nogui` from /data. Invoked via `sh` so no +x bit is needed from the
# (Windows) build host.
ENTRYPOINT ["/bin/sh", "/usr/local/bin/felis-entrypoint.sh"]
