#!/bin/bash
# Skip classified transient upstream failures only; 404 and all unclassified installer errors still fail.
set -euo pipefail

log="${1:?usage: e2e_upstream.sh LOG STATUS}"
status="${2:?usage: e2e_upstream.sh LOG STATUS}"

# Match the final nested error-trap chain and preceding curl error, allowing color codes and later cleanup output.
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
