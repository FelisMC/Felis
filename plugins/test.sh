#!/bin/bash
# Self-tests for the Java plugin layer.
#
#     bash plugins/test.sh
#
# Three gates, all runnable on any machine with a JDK 25 (Gradle comes from each
# module's wrapper, sha256-pinned):
#
#   1. The hand-written, framework-free test mains under shared/test and
#      velocity/test. They check what "compiles" cannot: the felis:control codec
#      round-trips every frame kind (spec §12), no server name can re-aim a
#      felis-api request path, only the lobby and the login gate may drive
#      felis:control (and each only with its own frames), a GET is retried once
#      after a dropped reply or a 502/503/504 and a POST never, the request timeout
#      cuts a slow call off, felis-link.properties timeouts are validated, the
#      felis-api call pool refuses instead of growing and a repeating task never
#      overlaps itself, every felis-api attempt is counted by outcome and the
#      periodic health line reports only a window's own counts (and stays quiet
#      when idle), an outage is logged when it starts, every few minutes while it
#      lasts and when it ends, the link-status outage
#      fallback fails closed outside its window, a proxy restarted during an API
#      outage routes on the last saved server list (and only until a fetch
#      succeeds), /invite prompts cannot double-fire
#      or outlive their TTL, the invite card really is a green/red clickable
#      prompt, and the op-login approval card names the account and leaves its
#      name for the admin to type. InviteCardTest and OpApprovalCardTest need the
#      adventure jars the velocity plugin compiles
#      against; they are fetched from Maven Central below, pinned by version and
#      checked by digest (a test run against silently-substituted bytes is not a
#      test of what we ship).
#
#   2. Production compile gates: the velocity/paper/limbo plugin jars — the three
#      bootstrap bakes into the proxy and the game images — are built through each
#      module's wrapper, the same Gradle the plugin build image runs, with every
#      dependency checked against the module's gradle/verification-metadata.xml. A
#      compile break or a swapped artifact is a red check here instead of an
#      install-time surprise. limbo compiles against the API release the login gate
#      bundles: deploy/game-stack.lock's LIMBO_VERSION, which bootstrap passes too.
#
#   3. The velocity routing self-tests (`./gradlew routingTest`): ServerRegistry,
#      WaitingRouter and ControlChannel run against the real velocity-api with a
#      fake proxy and a stub felis-api — a refresh registers, moves and drops
#      backends and lets go of a renamed subdomain, the login gate and host routing
#      admit only linked players, each wake refusal reaches the player as its own
#      message, felis:control acts only for the connection's player and holds its
#      frame budget. They ride the module's verified dependency set, which is why
#      they live in Gradle rather than in the javac mains above.
#
# No test framework: the mains are the same javac one-liners their javadocs document,
# so a local run and CI run the same bytes.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# --- 1. hermetic test mains ----------------------------------------------------

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
# examination-api is on the compile classpath too: adventure-api 4.26.1's Component
# signatures reference Examinable, so javac needs the class even though the test
# never names it. adventure-key rides along for ClickEvent/HoverEvent payloads.
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

# --- 2. production compile gates ------------------------------------------------

for module in velocity paper; do
  echo "==> plugins/$module: ./gradlew --no-daemon build"
  ( cd "plugins/$module" && ./gradlew --no-daemon build )
done

echo "==> plugins/velocity: ./gradlew --no-daemon routingTest"
( cd plugins/velocity && ./gradlew --no-daemon routingTest )

limbo_version="$(sed -n 's/^LIMBO_VERSION=//p' deploy/game-stack.lock)"
[ -n "$limbo_version" ] || { echo "deploy/game-stack.lock sets no LIMBO_VERSION" >&2; exit 1; }
echo "==> plugins/limbo: ./gradlew --no-daemon -PlimboVersion=${limbo_version} build"
( cd plugins/limbo && ./gradlew --no-daemon -PlimboVersion="$limbo_version" build )
