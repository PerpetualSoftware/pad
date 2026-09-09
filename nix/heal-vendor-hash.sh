#!/usr/bin/env bash
#
# Commit and push the recomputed `vendorHash` to main. Called by the
# `push-vendor-hash` job in .github/workflows/nix.yml, with the recomputed
# nix/package.nix already downloaded over the checked-out one.
#
# WHY THIS IS A FILE AND NOT A `run:` BLOCK (BUG-2974). The logic here decides
# whether main's committed hash gets fixed, and it had a defect that shipped
# green for a day because nothing could exercise it outside a real merge race.
# A file can be driven by nix/heal-vendor-hash_test.sh against a real git
# remote; a `run:` block can only be read. Same reason nix/bump-vendor-hash.sh
# is a file.
#
# THE GATE IS A QUESTION ABOUT STATE: does what main COMMITTED differ from what
# the build job RECOMPUTED? It used to be a question about the push RANGE — did
# THIS push touch go.mod/go.sum — which is the right question for loop
# prevention and the wrong one for recovery. After a lost push race the tree
# that needs healing was written by an EARLIER push, so every later merge that
# did not itself move the module set refused to fix it, exited GREEN, and left
# main carrying a hash a clean `nix build` rejects. That was BUG-2974.
#
# The loop still terminates, and never depended on the range: the commit this
# writes corrects package.nix, so the run it triggers finds the build green,
# never sets `bumped`, and the calling job does not start at all.
#
# Exit 1 is the loud arm and it is deliberate — an unhealed committed hash must
# leave the run RED saying so, because a green run is indistinguishable from a
# healthy one and main stays broken for anyone doing a plain `nix build`.
set -euo pipefail

: "${SHA:?SHA must be set to the commit whose module set was recomputed}"

# This script commits and pushes. Refuse to run outside CI unless a caller says
# it means it — the test suite sets this, a stray local invocation does not.
if [ -z "${GITHUB_ACTIONS:-}" ] && [ "${HEAL_ALLOW_LOCAL:-}" != "1" ]; then
	echo "$0: refusing to run outside GitHub Actions (set HEAL_ALLOW_LOCAL=1 to override)" >&2
	exit 2
fi

if git diff --quiet -- nix/package.nix; then
	echo "::error::The build job reported a vendorHash mismatch, but the recomputed nix/package.nix is byte-identical to the committed one. Those two cannot both be true - a mismatch means the computed hash differs from the committed one. Read the build job's recompute step before trusting either. Nothing pushed."
	exit 1
fi

git config user.name 'github-actions[bot]'
git config user.email '41898282+github-actions[bot]@users.noreply.github.com'
git add nix/package.nix
git commit -m "chore(nix): heal vendorHash for the module set in ${SHA} (TASK-2954)"

if ! git push; then
	echo "::error::The vendorHash heal did NOT land - the push was rejected, so another commit reached main during this run. main still carries a vendorHash that a clean nix build rejects. It is not lost: the next push to main meets the same mismatch and heals it from its own run, because the heal is gated on the committed hash rather than on whether that push touched go.mod or go.sum (BUG-2974)."
	exit 1
fi
