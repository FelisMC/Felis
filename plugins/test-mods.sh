#!/bin/bash
# Compile gates for the three loader mods (fabric / forge / neoforge).
#
#     bash plugins/test-mods.sh
#
# Nothing installs these mods — each module's jar is its own deliverable — so
# nothing ever compiled them either: no CI job, no install path, and the README's
# one-liners failed on a fresh clone because the vendored gradlew scripts were
# committed without their exec bit (fixed in the batch that added this script).
# This is the gate that keeps "the mod still compiles" true.
#
# Each module pins its own Gradle via its vendored wrapper (fabric/forge: 8.8,
# neoforge: 8.14) and targets a Java-17 Minecraft line (1.20.1 / 1.20.4), so run
# this on JDK 17. The plugin jars bootstrap installs are different modules with a
# different gate: plugins/test.sh (JDK 25). The first run here downloads and
# decompiles Minecraft (minutes); the Gradle caches make later runs much faster.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

major="$(java -version 2>&1 | sed -n 's/.*version "\([0-9]*\).*/\1/p' | head -1)"
[ "$major" = "17" ] || {
  echo "these mods target the Java-17 Minecraft lines and are built with JDK 17 (found java major ${major:-none}); set JAVA_HOME to a JDK 17" >&2
  exit 1
}

for module in fabric forge neoforge; do
  echo "==> plugins/${module}: ./gradlew --no-daemon build"
  ( cd "plugins/$module" && ./gradlew --no-daemon build )
done
