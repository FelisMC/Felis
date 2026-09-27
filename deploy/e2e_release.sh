#!/bin/bash
# The newest published release, for the e2e workflow's readme and upgrade jobs, and what
# their installer logs must show about how that release reached the host:
#
#   bash deploy/e2e_release.sh find                # tag, binary, sums into $GITHUB_OUTPUT
#   bash deploy/e2e_release.sh check-own LOG       # the release's own installer (upgrade)
#   bash deploy/e2e_release.sh check-readme LOG    # this commit's installer on its default
#                                                  # channel, the README's command (readme)
#
# The checks read TAG, BINARY and SUMS from the environment, as find wrote them. The
# runners are x86_64, so the binary asset is felis-linux-amd64. deploy/e2e_release_test.sh
# holds this script's own checks, against bootstrap.sh's own messages.
set -euo pipefail

ASSET=felis-linux-amd64
HOST_BIN=/usr/local/bin/felis
fails=0

pass() { printf 'PASS %s\n' "$*"; }
fail() { printf 'FAIL %s\n' "$*"; fails=$((fails + 1)); }
has() { # label fixed-string log
  if grep -qF -- "$2" "$3"; then pass "$1"; else fail "$1: no line with <$2> in $3"; fi
}
has_re() { # label regex log
  if grep -qE -- "$2" "$3"; then pass "$1"; else fail "$1: no line matching <$2> in $3"; fi
}
lacks_re() { # label regex log
  local hit
  if hit="$(grep -E -m 1 -- "$2" "$3")"; then fail "$1: ${hit}"; else pass "$1"; fi
}

# find_release: `gh release view` with no tag answers with the newest release that is not a
# prerelease, the one the installer's release channel resolves.
#
# An empty tag skips the readme and upgrade jobs, green. Only gh's own `release not found`
# means there is no release; any other failure (a token it refused, a rate limit, a network
# error) fails the step, or every release's upgrade would go untested without a word.
find_release() {
  local lines err rc=0 tag names binary="" sums=""
  err="$(mktemp)"
  lines="$(gh release view --repo "$GITHUB_REPOSITORY" --json tagName,assets --jq '.tagName, .assets[].name' 2>"$err")" || rc=$?
  if [ "$rc" -ne 0 ] && grep -qx 'release not found' "$err"; then
    rm -f "$err"
    echo "::notice::no published release yet; the readme and upgrade jobs have nothing to install"
    printf 'tag=\nbinary=\nsums=\n' >> "${GITHUB_OUTPUT:-/dev/stdout}"
    return 0
  fi
  if [ "$rc" -ne 0 ]; then
    echo "::error::gh release view failed (exit ${rc}), so the newest release is unknown: $(tr '\n' ' ' < "$err")"
    rm -f "$err"
    fails=$((fails + 1))
    return
  fi
  rm -f "$err"
  tag="$(printf '%s\n' "$lines" | head -n 1)"
  names="$(printf '%s\n' "$lines" | tail -n +2)"
  if [ -z "$tag" ]; then
    echo "::error::gh release view answered without a tag"
    fails=$((fails + 1))
    return
  fi
  if printf '%s\n' "$names" | grep -qxF "$ASSET"; then binary=yes; fi
  if printf '%s\n' "$names" | grep -qxF SHA256SUMS; then sums=yes; fi
  printf 'tag=%s\nbinary=%s\nsums=%s\n' "$tag" "$binary" "$sums" >> "${GITHUB_OUTPUT:-/dev/stdout}"
}

# check_own: a release's installer, however old, downloads the release's binary when the
# release publishes one, and says so in the same words.
check_own() {
  local log="$1"
  if [ "${BINARY:-}" != yes ]; then
    echo "::notice::release ${TAG} publishes no ${ASSET}; its installer builds it from source"
    return 0
  fi
  has "the release's installer installed the release's binary" "installed ${ASSET} ${TAG} at ${HOST_BIN}" "$log"
}

# check_readme: this commit's installer takes everything from a release that publishes its
# SHA256SUMS and builds nothing on the host; from one without, it builds the tag from source
# and says why.
check_readme() {
  local log="$1" role
  if [ "${BINARY:-}" = yes ] && [ "${SUMS:-}" = yes ]; then
    has "the binary is the release's" "installed ${ASSET} ${TAG} at ${HOST_BIN}" "$log"
    has "the images and plugin come from the release" "release ${TAG}'s prebuilt images and Velocity plugin are installed as published" "$log"
    for role in felis limbo lobby paper; do
      has_re "felis/${role} is the release's" "felis/${role}:[^ ]* is the release's" "$log"
    done
    has_re "the registry image is the release's" "docker.io/library/registry@sha256:[0-9a-f]* is the release's" "$log"
    has_re "the postgres image is the release's" "docker.io/library/postgres@sha256:[0-9a-f]* is the release's" "$log"
    has "felis-velocity.jar is the release's" "felis-velocity.jar is the release's" "$log"
    lacks_re "nothing fell back to a build or a pull" "on this host instead|from Docker Hub instead|building felis from source" "$log"
    lacks_re "Docker was left alone" "docker already installed|installing docker|docker running" "$log"
  elif [ "${BINARY:-}" = yes ]; then
    has "the source build says why" "release ${TAG} publishes no SHA256SUMS, so ${ASSET} cannot be verified; building ${TAG} from source on this host instead" "$log"
    echo "::warning::release ${TAG} publishes no SHA256SUMS, so the README's install builds ${TAG} from source on the host, Docker included; a release cut by release.yml puts it on the assets"
  else
    has "the source build says why" "release ${TAG} publishes no usable ${ASSET}; building ${TAG} from source on this host instead" "$log"
    echo "::warning::release ${TAG} publishes no ${ASSET}, so the README's install builds ${TAG} from source on the host, Docker included; a release cut by release.yml puts it on the assets"
  fi
}

case "${1:-}" in
  find) find_release ;;
  check-own) check_own "${2:?usage: e2e_release.sh check-own LOG}" ;;
  check-readme) check_readme "${2:?usage: e2e_release.sh check-readme LOG}" ;;
  *)
    echo "usage: e2e_release.sh find | check-own LOG | check-readme LOG" >&2
    exit 2
    ;;
esac
exit "$fails"
