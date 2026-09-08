#!/usr/bin/env bash
#
# Rewrite `vendorHash` in nix/package.nix from a failed `nix build` log.
#
# WHY THIS EXISTS (TASK-2954). `vendorHash` pins the Go module set by content
# hash, so any go.mod/go.sum change invalidates it. Dependabot updates go.mod
# and go.sum and has no idea this file exists, so every Go-dependency bump PR
# failed `Nix build & check` on a hash mismatch — structurally, forever. A
# permanently-red check is not a check: a bump that genuinely breaks the build
# looked identical, at a glance, to one that only moved the hash.
#
# WHAT IT IS SCOPED TO, AND WHY THAT IS THE WHOLE CORRECTNESS ARGUMENT. `nix
# build` prints "hash mismatch in fixed-output derivation" for ANY FOD, and this
# build has many: the Go module set is one, and every per-package `fetchurl`
# that `importNpmLock` generates from web/package-lock.json is another. Taking
# "the `got:` hash" from such a log would happily write an npm tarball's hash
# into `vendorHash` — a green-looking rewrite that pins the wrong thing. So the
# match is anchored to the derivation whose name ends in `-go-modules.drv`, and
# a log with no such block is NOT an error this script can fix: it exits 1
# having changed nothing, and the caller leaves the build red.
#
# Usage:  bump-vendor-hash.sh <build-log> [package.nix]
# Exit:   0 = rewritten (new hash on stdout)
#         1 = no go-modules hash mismatch in the log; nothing written
#         2 = usage / the file does not look like what we expect
set -euo pipefail

log="${1:-}"
pkg="${2:-nix/package.nix}"

if [[ -z "$log" || ! -r "$log" ]]; then
	echo "usage: $0 <build-log> [package.nix]" >&2
	exit 2
fi
if [[ ! -w "$pkg" ]]; then
	echo "$0: $pkg is not writable" >&2
	exit 2
fi

# Find the `got:` line belonging to the go-modules mismatch block.
#
# The block nix prints is:
#
#   error: hash mismatch in fixed-output derivation '/nix/store/…-pad-…-go-modules.drv':
#            specified: sha256-…
#               got:    sha256-…
#
# and in a CI log every line carries a runner timestamp in front of it. The
# timestamp is stripped SEPARATELY from the indentation, because a log captured
# locally (`nix build 2>&1 | tee`) has the indentation and no timestamp — the
# first version folded the two and worked only on CI logs, which the suite's
# exit-code assertion is what caught. (The
# timestamp pattern avoids interval quantifiers like `{4}`, which older `mawk` —
# the default `awk` on the ubuntu runners — does not accept; the suite re-runs
# itself under every awk on the box for exactly this reason). So each
# line is stripped of a leading ISO timestamp and then matched WHOLE — anchored
# at both ends — rather than searched for a substring. Substring matching is what
# lets ordinary build output impersonate a nix diagnostic: a `warning:` carrying
# the same phrase, a path that merely CONTAINS `-go-modules.drv`, or a log line
# that happens to say `got:` next to a hash would each be enough.
#
# Four things the header must satisfy, all of them narrowing:
#   - `error:`, not `warning:` or any other severity;
#   - `/nix/store/` followed by a single path SEGMENT — no further `/`, which is
#     what makes it a store path rather than any path ending in the right
#     characters;
#   - that segment ends `-go-modules.drv':` with nothing at all after it;
#   - the segment begins with a 32-character store hash and then `-pad-`, which
#     is derivation IDENTITY rather than a resemblance: `-pad-` matched anywhere
#     in the name would also accept `…-other-pad-tool-…-go-modules.drv`, whose
#     hash is not ours. If this project's pname ever changes, this stops matching
#     and the script exits 1 — red, and a human reads the log. That is the
#     intended direction to fail in.
#
# The hashes are matched at their CANONICAL LENGTH (43 base64 characters plus
# the `=`), not as `sha256-<anything>`. A truncated or hand-typed value in a log
# is then not something this can write into the build.
#
# The `got:` line is accepted only as the line IMMEDIATELY AFTER a `specified:`
# line inside an armed block (`ready == NR - 1`), because after the timestamp
# strip a `got:  sha256-…` line from any other producer is byte-indistinguishable
# from nix's. Sequence alone was not enough — a valid header, a valid
# `specified:`, and then somebody else's `got:` twenty lines later would have
# been accepted. Adjacency is the binding: nix prints the two together.
#
# Arming disarms on the next `error:` line, so a mismatch whose `got:` never
# arrives (interleaved output) yields nothing rather than the next FOD's hash.
new_hash="$(awk '
	{
		body = $0
		sub(/^[0-9][0-9-]*T[0-9][0-9:.]*Z/, "", body)
		sub(/^[ \t]+/, "", body)
		sub(/[\r]+$/, "", body)
	}
	body ~ /^error: hash mismatch in fixed-output derivation \x27\/nix\/store\/[0-9a-z]{32}-pad-[^\x27\/]*-go-modules\.drv\x27:$/ {
		armed = 1
		ready = 0
		next
	}
	armed && body ~ /^specified:[ \t]+sha256-[A-Za-z0-9+\/]{43}=$/ {
		ready = NR
		next
	}
	armed && ready == NR - 1 && body ~ /^got:[ \t]+sha256-[A-Za-z0-9+\/]{43}=$/ {
		# The line is already matched WHOLE above, so the hash is what is left
		# after the label. Extracting with a second pattern would put the
		# canonical-length rule in two places, and a mutation run showed what
		# that costs: dropping it from the condition alone changed nothing,
		# because the extractor still enforced it. One rule, one place.
		hash = body
		sub(/^got:[ \t]+/, "", hash)
		print hash
		exit
	}
	body ~ /^error:/ { armed = 0 }
' "$log")"

if [[ -z "$new_hash" ]]; then
	echo "$0: no go-modules hash mismatch in $log — leaving $pkg alone" >&2
	exit 1
fi

if ! grep -qE '^[[:space:]]*vendorHash[[:space:]]*=[[:space:]]*"sha256-[A-Za-z0-9+/]+=*";[[:space:]]*$' "$pkg"; then
	echo "$0: no vendorHash line to rewrite in $pkg" >&2
	exit 2
fi

# The replacement is anchored on the whole line, and the hash is injected via an
# awk variable rather than interpolated into a sed script: a base64 hash contains
# `/` and `+`, which are a sed delimiter and a regex metacharacter respectively.
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
awk -v h="$new_hash" '
	/^[[:space:]]*vendorHash[[:space:]]*=[[:space:]]*"sha256-/ {
		match($0, /^[[:space:]]*/)
		printf "%svendorHash = \"%s\";\n", substr($0, 1, RLENGTH), h
		next
	}
	{ print }
' "$pkg" > "$tmp"
cat "$tmp" > "$pkg"

echo "$new_hash"
