#!/bin/bash
# Run Java self-tests on JDK 25 with pinned Gradle wrappers and verified module dependencies.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT


# adventure-api 4.26.1 is what velocity-api 3.5.1 resolves through its adventure-bom.
# Pinned with digests because these exact bytes go on the compile/run classpath.
fetch() { # url sha256 -> path
  local url="$1" want="$2" file
  file="$work/$(basename "$url")"
  curl -fsSL --retry 5 --retry-delay 2 -o "$file" "$url"
  printf '%s  %s\n' "$want" "$file" | sha256sum -c - >&2
  printf '%s\n' "$file"
}
central=https://repo1.maven.org/maven2/net/kyori
adventure_api="$(fetch "$central/adventure-api/4.26.1/adventure-api-4.26.1.jar" \
  551e536b9ea868f30e72c7900a309b35124ee7d4889fa3b3aed0910299751a26)"
adventure_key="$(fetch "$central/adventure-key/4.26.1/adventure-key-4.26.1.jar" \
  eec172d63db77b40eb7abeeb25f65eedea89bd30264d057b68b12fecb731be5e)"
examination_api="$(fetch "$central/examination-api/1.3.0/examination-api-1.3.0.jar" \
  c9237ffecb05428f6eff86216246ac70ce0b47b04c08ea7ca35020fde57f8492)"

echo "==> ControlRoundTripTest (felis:control codec, shared)"
mkdir -p "$work/shared-classes"
javac -d "$work/shared-classes" \
  plugins/shared/src/main/java/best/lolicon/felis/link/*.java \
  plugins/shared/test/best/lolicon/felis/link/ControlRoundTripTest.java
java -cp "$work/shared-classes" best.lolicon.felis.link.ControlRoundTripTest

echo "==> FelisApiClientTest (request paths cannot be re-aimed, shared)"
javac -d "$work/shared-classes" \
  plugins/shared/src/main/java/best/lolicon/felis/link/*.java \
  plugins/shared/test/best/lolicon/felis/link/FelisApiClientTest.java
java -cp "$work/shared-classes" best.lolicon.felis.link.FelisApiClientTest

echo "==> FelisApiClientRetryTest (which failures are retried, request timeout, shared)"
javac -d "$work/shared-classes" \
  plugins/shared/src/main/java/best/lolicon/felis/link/*.java \
  plugins/shared/test/best/lolicon/felis/link/FelisApiClientRetryTest.java
java -cp "$work/shared-classes" best.lolicon.felis.link.FelisApiClientRetryTest

echo "==> LinkConfigLoaderTest (env/file precedence, template, timeouts, shared)"
javac -d "$work/shared-classes" \
  plugins/shared/src/main/java/best/lolicon/felis/link/*.java \
  plugins/shared/test/best/lolicon/felis/link/LinkConfigLoaderTest.java
java -cp "$work/shared-classes" best.lolicon.felis.link.LinkConfigLoaderTest

echo "==> OutageTrackerTest (outage start / reminder / recovery log decisions, shared)"
javac -d "$work/shared-classes" \
  plugins/shared/src/main/java/best/lolicon/felis/link/*.java \
  plugins/shared/test/best/lolicon/felis/link/OutageTrackerTest.java
java -cp "$work/shared-classes" best.lolicon.felis.link.OutageTrackerTest

echo "==> ExperienceConfigTest (a damaged settings file falls back to the defaults, shared)"
javac -d "$work/shared-classes" \
  plugins/shared/src/main/java/best/lolicon/felis/link/*.java \
  plugins/shared/test/best/lolicon/felis/link/ExperienceConfigTest.java
java -cp "$work/shared-classes" best.lolicon.felis.link.ExperienceConfigTest

echo "==> ModLinkTest (the loader mods' shared /link, shared)"
javac -d "$work/shared-classes" \
  plugins/shared/src/main/java/best/lolicon/felis/link/*.java \
  plugins/shared/test/best/lolicon/felis/link/ModLinkTest.java
java -cp "$work/shared-classes" best.lolicon.felis.link.ModLinkTest

echo "==> ControlPolicyTest (who may send what on felis:control, velocity)"
mkdir -p "$work/policy-classes"
javac -d "$work/policy-classes" \
  plugins/shared/src/main/java/best/lolicon/felis/link/*.java \
  plugins/velocity/src/main/java/best/lolicon/felis/velocity/ControlPolicy.java \
  plugins/velocity/src/main/java/best/lolicon/felis/velocity/LinkGate.java \
  plugins/velocity/src/main/java/best/lolicon/felis/velocity/FrameBudget.java \
  plugins/velocity/test/best/lolicon/felis/velocity/ControlPolicyTest.java \
  plugins/velocity/test/best/lolicon/felis/velocity/LinkGateTest.java
java -cp "$work/policy-classes" best.lolicon.felis.velocity.ControlPolicyTest

echo "==> LinkGateTest (outage fallback + frame budget, velocity)"
java -cp "$work/policy-classes" best.lolicon.felis.velocity.LinkGateTest

echo "==> ServerListSourceTest (saved server list for a restart during an outage, velocity)"
mkdir -p "$work/list-classes"
javac -d "$work/list-classes" \
  plugins/shared/src/main/java/best/lolicon/felis/link/*.java \
  plugins/velocity/src/main/java/best/lolicon/felis/velocity/ServerListSource.java \
  plugins/velocity/test/best/lolicon/felis/velocity/ServerListSourceTest.java
java -cp "$work/list-classes" best.lolicon.felis.velocity.ServerListSourceTest

echo "==> LegacyForwardingTest (CR-marked servers reach the fork's legacy-forwarding list, velocity)"
mkdir -p "$work/legacy-classes"
javac -d "$work/legacy-classes" \
  plugins/shared/src/main/java/best/lolicon/felis/link/*.java \
  plugins/velocity/src/main/java/best/lolicon/felis/velocity/LegacyForwarding.java \
  plugins/velocity/test/best/lolicon/felis/velocity/LegacyForwardingTest.java
java -cp "$work/legacy-classes" best.lolicon.felis.velocity.LegacyForwardingTest

echo "==> ApiPoolTest (bounded felis-api pool, non-overlapping repeats, velocity)"
mkdir -p "$work/pool-classes"
javac -d "$work/pool-classes" \
  plugins/velocity/src/main/java/best/lolicon/felis/velocity/BoundedExecutor.java \
  plugins/velocity/src/main/java/best/lolicon/felis/velocity/SkipIfRunning.java \
  plugins/velocity/test/best/lolicon/felis/velocity/ApiPoolTest.java
java -cp "$work/pool-classes" best.lolicon.felis.velocity.ApiPoolTest

echo "==> ProxyStatsTest (proxy failure counters and the periodic health line, velocity)"
mkdir -p "$work/stats-classes"
javac -d "$work/stats-classes" \
  plugins/shared/src/main/java/best/lolicon/felis/link/*.java \
  plugins/velocity/src/main/java/best/lolicon/felis/velocity/ProxyStats.java \
  plugins/velocity/test/best/lolicon/felis/velocity/ProxyStatsTest.java
java -cp "$work/stats-classes" best.lolicon.felis.velocity.ProxyStatsTest

echo "==> InviteBookTest (/invite prompt store, velocity)"
mkdir -p "$work/velocity-classes"
javac -d "$work/velocity-classes" \
  plugins/velocity/src/main/java/best/lolicon/felis/velocity/InviteBook.java \
  plugins/velocity/test/best/lolicon/felis/velocity/InviteBookTest.java
java -cp "$work/velocity-classes" best.lolicon.felis.velocity.InviteBookTest

echo "==> InviteCardTest (/invite card, velocity)"
mkdir -p "$work/card-classes"
# Adventure signatures require examination-api at compile time; include adventure-key for event payloads.
javac -cp "$adventure_api:$adventure_key:$examination_api" -d "$work/card-classes" \
  plugins/velocity/src/main/java/best/lolicon/felis/velocity/InviteCard.java \
  plugins/velocity/test/best/lolicon/felis/velocity/InviteCardTest.java
java -cp "$work/card-classes:$adventure_api:$adventure_key:$examination_api" \
  best.lolicon.felis.velocity.InviteCardTest

echo "==> OpApprovalCardTest (/felis web op approve card, velocity)"
mkdir -p "$work/opcard-classes"
javac -cp "$adventure_api:$adventure_key:$examination_api" -d "$work/opcard-classes" \
  plugins/shared/src/main/java/best/lolicon/felis/link/*.java \
  plugins/velocity/src/main/java/best/lolicon/felis/velocity/OpApprovalCard.java \
  plugins/velocity/test/best/lolicon/felis/velocity/OpApprovalCardTest.java
java -cp "$work/opcard-classes:$adventure_api:$adventure_key:$examination_api" \
  best.lolicon.felis.velocity.OpApprovalCardTest

echo "==> LinkCodeCardTest (/link reply, velocity)"
mkdir -p "$work/linkcard-classes"
javac -cp "$adventure_api:$adventure_key:$examination_api" -d "$work/linkcard-classes" \
  plugins/velocity/src/main/java/best/lolicon/felis/velocity/LinkCodeCard.java \
  plugins/velocity/test/best/lolicon/felis/velocity/LinkCodeCardTest.java
java -cp "$work/linkcard-classes:$adventure_api:$adventure_key:$examination_api" \
  best.lolicon.felis.velocity.LinkCodeCardTest


for module in velocity paper; do
  echo "==> plugins/$module: ./gradlew --no-daemon build"
  ( cd "plugins/$module" && ./gradlew --no-daemon --console=plain --init-script ../shared/build-progress.gradle build )
done

echo "==> plugins/velocity: ./gradlew --no-daemon routingTest"
( cd plugins/velocity && ./gradlew --no-daemon routingTest )

echo "==> plugins/paper: ./gradlew --no-daemon lobbyTest"
( cd plugins/paper && ./gradlew --no-daemon lobbyTest )

limbo_version="$(sed -n 's/^LIMBO_VERSION=//p' deploy/game-stack.lock)"
[ -n "$limbo_version" ] || { echo "deploy/game-stack.lock sets no LIMBO_VERSION" >&2; exit 1; }
echo "==> plugins/limbo: ./gradlew --no-daemon -PlimboVersion=${limbo_version} build loginTest"
( cd plugins/limbo && ./gradlew --no-daemon --console=plain --init-script ../shared/build-progress.gradle -PlimboVersion="$limbo_version" build loginTest )
