#!/bin/bash
# Self-tests for the Java plugin layer.
#
#     bash plugins/test.sh
#
# Two gates, both runnable on any machine with a JDK 21 and Gradle:
#
#   1. The hand-written, framework-free test mains under shared/test and
#      velocity/test. They check what "compiles" cannot: the felis:control codec
#      round-trips every frame kind (spec §12), no server name can re-aim a
#      felis-api request path, only the lobby and the login gate may drive
#      felis:control (and each only with its own frames), the link-status outage
#      fallback fails closed outside its window, /invite prompts cannot double-fire
#      or outlive their TTL, and the invite card really is a green/red clickable
#      prompt. InviteCardTest needs the adventure jars the velocity plugin compiles
#      against; they are fetched from Maven Central below, pinned by version and
#      checked by digest (a test run against silently-substituted bytes is not a
#      test of what we ship).
#
#   2. Production compile gates: the velocity/paper/limbo plugin jars — the three
#      bootstrap bakes into the proxy and the game images — are built with the same
#      Gradle major the plugin Dockerfiles pin, so a compile break is a red check
#      here instead of an install-time surprise. limbo compiles against the API
#      release bootstrap would bundle (resolved below, same source the installer
#      reads), because the module's `+` default cannot resolve on its own.
#
# No test framework and no wrapper: the mains are the same javac one-liners their
# javadocs document, so a local run and CI run the same bytes.
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

# --- 2. production compile gates ------------------------------------------------

for module in velocity paper; do
  echo "==> gradle --no-daemon -p plugins/$module build"
  gradle --no-daemon -p "plugins/$module" build
done

# limbo is special: it compiles against the LOOHP/Limbo API release that the
# installer bundles, and that release is published nowhere except the CI artifact
# name (Limbo-<version>-<mc>.jar) — the same place deploy/bootstrap.sh reads it.
# The module's `+` version default cannot resolve (LOOHP's repository serves no
# maven-metadata.xml), so a bare `gradle -p plugins/limbo build` is never a valid
# command; the version must come from here or from bootstrap.
echo "==> resolving the newest LOOHP/Limbo CI build (for -PlimboVersion)"
limbo_meta="$(curl -fsSL --retry 5 --retry-delay 2 \
  https://ci.loohpjames.com/job/Limbo/lastSuccessfulBuild/api/json)" \
  || { echo "cannot read the LOOHP/Limbo CI build metadata; the limbo gate cannot pick a version" >&2; exit 1; }
limbo_file="$(printf '%s' "$limbo_meta" | grep -o 'Limbo-[0-9A-Za-z._-]*\.jar' || true)"
limbo_file="${limbo_file%%$'\n'*}"
[ -n "$limbo_file" ] || { echo "no Limbo jar in the LOOHP/Limbo CI artifact list" >&2; exit 1; }
limbo_version="${limbo_file%.jar}"; limbo_version="${limbo_version%-*}"; limbo_version="${limbo_version#Limbo-}"
[ -n "$limbo_version" ] || { echo "cannot parse the Limbo version out of ${limbo_file}" >&2; exit 1; }
echo "==> gradle --no-daemon -p plugins/limbo build (Limbo ${limbo_version})"
gradle --no-daemon -p plugins/limbo -PlimboVersion="$limbo_version" build
