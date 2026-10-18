#!/bin/bash
# Run loader compile gates on JDK 17; initial Minecraft download/decompilation may take several minutes.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

major="$(java -version 2>&1 | sed -n 's/.*version "\([0-9]*\).*/\1/p' | sed -n 1p)"
[ "$major" = "17" ] || {
  echo "these mods target the Java-17 Minecraft lines and are built with JDK 17 (found java major ${major:-none}); set JAVA_HOME to a JDK 17" >&2
  exit 1
}

for module in fabric forge neoforge; do
  echo "==> plugins/${module}: ./gradlew --no-daemon build"
  ( cd "plugins/$module" && ./gradlew --no-daemon build )
done
