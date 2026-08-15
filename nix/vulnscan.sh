#!/usr/bin/env bash
# Scan a built pad binary with govulncheck and compare the advisories it
# reports against nix/accepted-advisories.txt. Closes the gap BUG-2567
# documents: CI's govulncheck job scans a `go build` binary, so the
# Nix-built artifact (different toolchain, different ldflags) ships with no
# gate over it at all.
#
# Usage: nix/vulnscan.sh <path-to-binary>
#
# Exit 0: every reported advisory is in the accepted list. Advisories in
#         the accepted list that are NO LONGER reported produce a warning
#         (GitHub annotation when running in Actions) — that is the signal
#         to prune the list and, once the stdlib group is quiet, close
#         BUG-2567.
# Exit 1: an advisory NOT in the accepted list was reported — new exposure
#         in this artifact. Fails the job.
# Exit 2: operational error (missing tool, scan failure, missing file).
#
# Requires govulncheck and jq on PATH. GOVULNCHECK overrides the
# govulncheck executable (the workflow installs a pinned version).
set -euo pipefail

BINARY="${1:?usage: nix/vulnscan.sh <path-to-binary>}"
ACCEPTED_FILE="$(dirname "$0")/accepted-advisories.txt"
GOVULNCHECK="${GOVULNCHECK:-govulncheck}"

[ -f "$BINARY" ] || { echo "vulnscan: binary not found: $BINARY" >&2; exit 2; }
[ -f "$ACCEPTED_FILE" ] || { echo "vulnscan: accepted list not found: $ACCEPTED_FILE" >&2; exit 2; }
command -v "$GOVULNCHECK" >/dev/null || { echo "vulnscan: govulncheck not on PATH" >&2; exit 2; }
command -v jq >/dev/null || { echo "vulnscan: jq not on PATH" >&2; exit 2; }

# JSON mode exits 0 even when findings exist (unlike text mode's exit 3),
# which is what lets us do the comparison ourselves. A non-zero exit here
# is an operational failure (bad binary, no network to vuln.go.dev, ...).
scan_json="$(mktemp)"
trap 'rm -f "$scan_json"' EXIT
if ! "$GOVULNCHECK" -mode binary -format json "$BINARY" > "$scan_json"; then
  echo "vulnscan: govulncheck failed (operational error, not a finding)" >&2
  exit 2
fi

# A finding whose top trace frame carries a function is one govulncheck
# considers reachable — the same criterion that drives text mode's
# "Your code is affected" list and its failing exit code. On this stripped
# binary (-s -w) precision degrades to module level, which is why the
# accepted list carries module-level entries; see the comments there.
reported="$(jq -r 'select(.finding != null)
                   | select(.finding.trace[0].function != null)
                   | .finding.osv' "$scan_json" | sort -u)"

accepted="$(sed -e 's/#.*//' -e 's/[[:space:]].*//' -e '/^$/d' "$ACCEPTED_FILE" | sort -u)"

new="$(comm -23 <(printf '%s' "$reported") <(printf '%s' "$accepted"))"
resolved="$(comm -13 <(printf '%s' "$reported") <(printf '%s' "$accepted"))"

echo "vulnscan: $(printf '%s' "$reported" | grep -c . || true) advisories reported, $(printf '%s' "$accepted" | grep -c . || true) accepted"

if [ -n "$resolved" ]; then
  while IFS= read -r id; do
    msg="accepted advisory $id is no longer reported against $BINARY — remove it from nix/accepted-advisories.txt (when the stdlib group clears, close BUG-2567)"
    if [ "${GITHUB_ACTIONS:-}" = "true" ]; then
      echo "::warning file=nix/accepted-advisories.txt::$msg"
    else
      echo "vulnscan: WARNING: $msg" >&2
    fi
  done <<< "$resolved"
fi

if [ -n "$new" ]; then
  echo "vulnscan: NEW advisories in the Nix-built artifact (not in $ACCEPTED_FILE):" >&2
  echo "$new" | sed 's/^/  /' >&2
  echo "vulnscan: investigate; only add to the accepted list with a dated comment explaining why." >&2
  exit 1
fi

echo "vulnscan: OK — no unaccepted advisories"
