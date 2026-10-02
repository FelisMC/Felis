#!/bin/bash
# Whether a failed installer run in the e2e workflow stopped on an upstream download that
# was refused or dropped, which says nothing about the commit under test. Each install step
# hands its log and the installer's status here when the installer fails:
#
#   sudo ... bash deploy/bootstrap.sh 2>&1 | tee install.log || bash deploy/e2e_upstream.sh install.log $?
#
# It reads the installer's last ERR-trap line ("bootstrap failed near line N (exit E)") and
# the line just before it. When that line is curl's own error for the same status, and the
# error is one a mirror or GitHub answers with on a bad minute (a connection refused, reset
# or timed out, a 403, 408, 429 or 5xx), it leaves a warning and E2E_UPSTREAM_SKIP=1 in
# $GITHUB_ENV and exits 0: the job's later steps are gated on that variable, so the job ends
# green with the warning on the run. Anything else exits with the installer's status. A 404
# is a URL or a version the installer names, which a commit can break, so it fails.
#
# deploy/e2e_upstream_test.sh holds its checks, against bootstrap.sh's own trap message.
set -euo pipefail

log="${1:?usage: e2e_upstream.sh LOG STATUS}"
status="${2:?usage: e2e_upstream.sh LOG STATUS}"

# The last run of trap lines and the line before it. bash may fire the trap again for each
# function the failure unwinds through, and the EXIT cleanup can print after it. The log
# keeps the installer's colour codes, so nothing is anchored on the left.
code="" before=""
{ read -r code && IFS= read -r before; } < <(awk '
  /bootstrap failed near line [0-9]+ \(exit [0-9]+\)$/ {
    code = $0; sub(/.*\(exit /, "", code); sub(/\)$/, "", code)
    found = code; foundprev = last
    next
  }
  { last = $0 }
  END { if (found != "") { print found; print foundprev } }
' "$log") || true

upstream=""
case "$before" in
  *"curl: (${code}) "*)
    case "$code" in
      # Could not resolve or connect, an HTTP/2 or TLS failure, a transfer cut short, no
      # reply, a send or receive failure, a timeout.
      5 | 6 | 7 | 16 | 18 | 28 | 35 | 52 | 55 | 56 | 92) upstream=yes ;;
      # -f's HTTP error; curl before 7.75 appends the reason phrase.
      22) if [[ "$before" =~ returned\ error:\ (403|408|429|5[0-9][0-9])([^0-9]|$) ]]; then upstream=yes; fi ;;
    esac
    ;;
esac

if [ -n "$upstream" ]; then
  echo "::warning::the installer stopped on an upstream download (curl: ${before#*curl: }); this job skips its remaining steps"
  echo "E2E_UPSTREAM_SKIP=1" >> "${GITHUB_ENV:-/dev/null}"
  exit 0
fi
echo "the installer failed (exit ${status}) on something other than a refused upstream download; its log is above"
[ "$status" -ne 0 ] 2>/dev/null || status=1
exit "$status"
