#!/bin/bash
# Checks for deploy/e2e_release.sh. Run it as: bash deploy/e2e_release_test.sh
#
# The installer logs it reads are built from bootstrap.sh's own ok/warn messages, expanded
# with a release's values, so rewording one of them there fails here rather than in a
# two-hour e2e run. gh is a stub that prints what a release listing would.
set -u

here="$(dirname "$0")"
ER="${1:-${here}/e2e_release.sh}"
BS="${2:-${here}/bootstrap.sh}"
[ -f "$ER" ] || { echo "no such script: $ER"; exit 1; }
[ -f "$BS" ] || { echo "no such script: $BS"; exit 1; }
fails=0

expect() { # label needle haystack
  case "$3" in
    *"$2"*) echo "PASS $1" ;;
    *) echo "FAIL $1: expected <$2> in:"; echo "$3"; fails=$((fails + 1)) ;;
  esac
}
status() { # label want got
  if [ "$2" = "$3" ]; then echo "PASS $1"; else echo "FAIL $1: exit $3, want $2"; fails=$((fails + 1)); fi
}

root="$(mktemp -d)"
trap 'rm -rf "$root"' EXIT

# msg <marker> prints bootstrap.sh's first ok/warn message holding <marker>, expanded with
# the variables below the way the installer expands it. They are read only through that eval.
# shellcheck disable=SC2034
{
  tag=v1.2.3
  FELIS_REF="$tag"
  v="$tag"
  asset=felis-linux-amd64
  name="$asset"
  HOST_BIN=/usr/local/bin/felis
  digest=sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
}
msg() {
  local line body
  line="$(grep -F -- "$1" "$BS" | grep -E '^[[:space:]]*(ok|warn) "' | head -n 1)"
  if [ -z "$line" ]; then
    echo "FAIL bootstrap.sh prints no message holding <$1>" >&2
    fails=$((fails + 1))
    return
  fi
  body="${line#*\"}"
  body="${body%\"*}"
  eval "printf '%s\n' \"${body}\""
}
# unusable <marker> prints the warning of bootstrap.sh's first artifact_unusable call holding
# <marker>; artifact_unusable joins its two arguments with "; ".
unusable() {
  local line
  line="$(grep -F -- "$1" "$BS" | grep -E '^[[:space:]]*artifact_unusable "' | head -n 1)"
  if [ -z "$line" ]; then
    echo "FAIL bootstrap.sh has no artifact_unusable call holding <$1>" >&2
    fails=$((fails + 1))
    return
  fi
  artifact_unusable() { printf '%s; %s\n' "$1" "$2"; }
  eval "$line"
}
image_line() { # target
  # shellcheck disable=SC2034 # read by msg's eval
  local target="$1"
  msg 'is the release'"'"'s (${digest:7:12})'
}

# What an install from a complete release prints, in the installer's order.
{
  msg 'ok "installed ${asset} ${FELIS_REF} at ${HOST_BIN}"'
  msg 'matches release ${tag}'"'"'s SHA256SUMS'
  msg 'prebuilt images and Velocity plugin are installed as published'
  for t in felis/felis:v1.2.3 felis/limbo:v1.2.3 felis/lobby:v1.2.3 felis/paper:v1.2.3 \
      docker.io/library/registry@sha256:aa docker.io/library/postgres@sha256:bb; do
    image_line "$t"
  done
  msg 'felis-velocity.jar is the release'"'"'s"'
} > "$root/complete.log"

readme() { # log binary sums
  TAG="$tag" BINARY="$2" SUMS="$3" bash "$ER" check-readme "$1" 2>&1
}
own() { # log binary
  TAG="$tag" BINARY="$2" bash "$ER" check-own "$1" 2>&1
}

out="$(readme "$root/complete.log" yes yes)"
status "a complete release's install passes" 0 $?
expect "  and every image is checked" "PASS felis/paper is the release's" "$out"

grep -v 'felis/limbo:' "$root/complete.log" > "$root/nolimbo.log"
out="$(readme "$root/nolimbo.log" yes yes)"
status "an image missing from the log fails" 1 $?
expect "  and names it" "FAIL felis/limbo is the release's" "$out"

{
  cat "$root/complete.log"
  name=felis-images-linux-amd64.txt unusable '} cannot be used" "building its images on this host instead"'
} > "$root/fallback.log"
out="$(readme "$root/fallback.log" yes yes)"
status "an image built on the host fails" 1 $?
expect "  and quotes the fallback" "building its images on this host instead" "$out"

{ cat "$root/complete.log"; msg 'ok "docker already installed"'; } > "$root/docker.log"
out="$(readme "$root/docker.log" yes yes)"
status "Docker touched fails" 1 $?

sed 's/installed felis-linux-amd64 v1.2.3/installed felis-linux-amd64 v1.2.2/' "$root/complete.log" > "$root/other.log"
out="$(readme "$root/other.log" yes yes)"
status "another release's binary fails" 1 $?

# A release that publishes its binary but no SHA256SUMS: this commit's installer builds the
# tag from source and says so.
msg 'publishes no SHA256SUMS, so ${name} cannot be verified' > "$root/nosums.log"
out="$(readme "$root/nosums.log" yes "")"
status "a release without SHA256SUMS passes when the fallback is announced" 0 $?
expect "  and warns in the run" "::warning::release v1.2.3 publishes no SHA256SUMS" "$out"
out="$(readme "$root/complete.log" yes "")"
status "a release without SHA256SUMS fails when nothing announced the fallback" 1 $?

msg 'publishes no usable ${asset}' > "$root/nobinary.log"
out="$(readme "$root/nobinary.log" "" "")"
status "a release without a binary passes when the fallback is announced" 0 $?
out="$(readme "$root/nosums.log" "" "")"
status "a release without a binary fails when the log says otherwise" 1 $?

# check-own: the release's own installer, which may predate SHA256SUMS.
out="$(own "$root/complete.log" yes)"
status "the release's installer downloading its binary passes" 0 $?
out="$(own "$root/nobinary.log" yes)"
status "the release's installer building from source fails" 1 $?
out="$(own "$root/other.log" yes)"
status "the release's installer downloading another release fails" 1 $?
out="$(own "$root/nobinary.log" "")"
status "a release without a binary asks nothing of its installer" 0 $?

# find: the listing gh answers with, in the step's outputs, exactly. GH_FAIL is what a
# failing gh prints on stderr before it exits 1.
mkdir -p "$root/bin"
cat > "$root/bin/gh" <<'STUB'
#!/bin/sh
if [ -n "${GH_FAIL:-}" ]; then
  printf '%s\n' "$GH_FAIL" >&2
  exit 1
fi
printf '%s\n' $GH_LISTING
STUB
chmod +x "$root/bin/gh"
find_run() { # listing [gh's error]: prints what find says; its outputs land in $root/out
  : > "$root/out"
  GH_LISTING="$1" GH_FAIL="${2:-}" GITHUB_REPOSITORY=FelisMC/Felis GITHUB_OUTPUT="$root/out" PATH="$root/bin:$PATH" bash "$ER" find 2>&1
}
same() { # label want got
  if [ "$2" = "$3" ]; then echo "PASS $1"; else printf 'FAIL %s: got\n%s\nwant\n%s\n' "$1" "$3" "$2"; fails=$((fails + 1)); fi
}
find_run "v1.2.3 felis-linux-amd64 felis-linux-arm64 SHA256SUMS felis-velocity.jar" >/dev/null
same "find: a complete release" "$(printf 'tag=v1.2.3\nbinary=yes\nsums=yes')" "$(cat "$root/out")"
find_run "v0.1.0 felis-linux-amd64 felis-linux-arm64" >/dev/null
same "find: a release without SHA256SUMS" "$(printf 'tag=v0.1.0\nbinary=yes\nsums=')" "$(cat "$root/out")"
find_run "v0.1.0 felis-linux-arm64 felis-linux-amd64.cdx.json SHA256SUMS.sig" >/dev/null
same "find: only exact asset names count" "$(printf 'tag=v0.1.0\nbinary=\nsums=')" "$(cat "$root/out")"
out="$(find_run "" "release not found")"
status "find: no release passes" 0 $?
same "  with an empty tag, which skips the release jobs" "$(printf 'tag=\nbinary=\nsums=')" "$(cat "$root/out")"
same "  and says so" "::notice::no published release yet; the readme and upgrade jobs have nothing to install" "$out"

# Anything else gh fails on leaves the newest release unknown: the step fails, and writes no
# tag that would skip the jobs.
for e in "HTTP 401: Bad credentials (https://api.github.com/graphql)" \
    "API rate limit exceeded for installation ID 1." \
    "error connecting to api.github.com"; do
  out="$(find_run "" "$e")"
  status "find: gh failing with <$e> fails" 1 $?
  same "  and quotes gh" "::error::gh release view failed (exit 1), so the newest release is unknown: $e " "$out"
  same "  and writes no outputs" "" "$(cat "$root/out")"
done
out="$(find_run "")"
status "find: gh answering nothing fails" 1 $?
same "  and says so" "::error::gh release view answered without a tag" "$out"
same "  and writes no outputs" "" "$(cat "$root/out")"

echo
if [ "$fails" -eq 0 ]; then echo "ALL PASS"; else echo "${fails} FAILED"; fi
exit "$fails"
