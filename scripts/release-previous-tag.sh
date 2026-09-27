#!/usr/bin/env bash
# Print the tag a release's changelog should start from: the highest STABLE
# tag (vX.Y.Z, no pre-release suffix) that sorts below the current tag's
# X.Y.Z, by version and regardless of branch.
#
# Why not GoReleaser's own default: it runs `git describe --tags --abbrev=0`
# on the current tag's parent, which only sees tags on the current tag's
# ancestry. Releases are cut on release-X.Y.x branches, so their tags are not
# on main, and a tag cut from main described back to v0.15.0 while v0.16.0
# already existed. Its notes would have re-listed the whole of v0.16.0.
# TASK-3268 has the measurement.
#
#   v0.17.0       -> v0.16.0   (the previous stable release)
#   v0.17.0-rc.1  -> v0.16.0   (an RC previews everything its stable will list)
#   v0.16.1       -> v0.16.0
#
# Usage: release-previous-tag.sh <current-tag>
# Exits non-zero, printing nothing on stdout, when the current tag is not
# vX.Y.Z[-suffix] or when no stable tag sorts below it. A release must not
# silently fall back to the describe base this script exists to replace.
set -euo pipefail

current="${1:-}"
if [[ ! "$current" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
	echo "release-previous-tag: '$current' is not a vX.Y.Z[-suffix] tag" >&2
	exit 1
fi
base="${current%%-*}"

previous=""
while IFS= read -r tag; do
	[[ "$tag" == "$base" ]] && break
	previous="$tag"
done < <({ git tag --list 'v*' | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | grep -vxF "$base" || true; echo "$base"; } | sort -V)

if [[ -z "$previous" ]]; then
	echo "release-previous-tag: no stable tag sorts below $base" >&2
	exit 1
fi
echo "$previous"
