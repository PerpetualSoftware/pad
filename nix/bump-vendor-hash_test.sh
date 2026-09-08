#!/usr/bin/env bash
#
# Tests for bump-vendor-hash.sh (TASK-2954).
#
# The load-bearing case is NEGATIVE: this build's npm dependencies are
# per-package `fetchurl` derivations, so a log can carry a "hash mismatch in
# fixed-output derivation" block that has nothing to do with `vendorHash`.
# A parser that takes "the got: hash" would write an npm tarball's hash into
# vendorHash and the rewrite would look like it worked. Cases 3, 4 and 5 are
# there to fail if the anchoring on `-go-modules.drv` is ever loosened.
#
# The two positive logs are the REAL text from the failed runs on PR #1274 and
# #1275, timestamps included, because the timestamps are what break a
# fixed-offset parser.
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
script="$here/bump-vendor-hash.sh"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

pass=0
fail=0

ORIG='sha256-8L7gH7Yy5+Fig3wK2SPLYSJjcY9nF/jumQ7PATJ3RIE='

# A package.nix stub carrying the real vendorHash line at its real indentation.
make_pkg() {
	cat > "$1" <<EOF
{
  pname = "pad";

  # Update alongside go.sum.
  vendorHash = "$ORIG";

  subPackages = [ "cmd/pad" ];
}
EOF
}

check() {
	local name="$1" want="$2" got="$3"
	if [[ "$want" == "$got" ]]; then
		pass=$((pass + 1))
	else
		fail=$((fail + 1))
		echo "FAIL: $name"
		echo "  want: $want"
		echo "  got:  $got"
	fi
}

hash_in() { grep -oE 'sha256-[A-Za-z0-9+/]+=*' "$1" | head -1; }

# ---------------------------------------------------------------- case 1
# The real log from PR #1274 (go-minor-and-patch, 4 updates).
log="$work/1274.log"
cat > "$log" <<'EOF'
2026-09-07T13:11:02.5087484Z error: hash mismatch in fixed-output derivation '/nix/store/ghbb18gfh6pl8vqf1fzqrfcsjk4ikh8z-pad-0.15.0-go-modules.drv':
2026-09-07T13:11:02.5093617Z          specified: sha256-8L7gH7Yy5+Fig3wK2SPLYSJjcY9nF/jumQ7PATJ3RIE=
2026-09-07T13:11:02.5094211Z             got:    sha256-2hHhUx/J9CgDGU4fE5EE2Y/h6nRTA5vZ7GzuQ2oajkk=
2026-09-07T13:11:03.5316739Z ##[error]To correct the hash mismatch for pad-0.15.0-go-modules, use "sha256-2hHhUx/J9CgDGU4fE5EE2Y/h6nRTA5vZ7GzuQ2oajkk="
EOF
pkg="$work/p1.nix"; make_pkg "$pkg"
out="$("$script" "$log" "$pkg" 2>/dev/null)"; rc=$?
check "1274: exit 0" "0" "$rc"
check "1274: prints the new hash" "sha256-2hHhUx/J9CgDGU4fE5EE2Y/h6nRTA5vZ7GzuQ2oajkk=" "$out"
check "1274: rewrites the file" "sha256-2hHhUx/J9CgDGU4fE5EE2Y/h6nRTA5vZ7GzuQ2oajkk=" "$(hash_in "$pkg")"
check "1274: keeps the indentation" '  vendorHash = "sha256-2hHhUx/J9CgDGU4fE5EE2Y/h6nRTA5vZ7GzuQ2oajkk=";' "$(grep vendorHash "$pkg")"

# ---------------------------------------------------------------- case 2
# The real log from PR #1275 (mcp-go 0.58.0 -> 1.0.0).
log="$work/1275.log"
cat > "$log" <<'EOF'
2026-09-07T13:12:44.1000000Z error: hash mismatch in fixed-output derivation '/nix/store/zzzz1qv8w0k3n7d2m5x9c4b6f8h0j2l4-pad-0.15.0-go-modules.drv':
2026-09-07T13:12:44.1000001Z          specified: sha256-8L7gH7Yy5+Fig3wK2SPLYSJjcY9nF/jumQ7PATJ3RIE=
2026-09-07T13:12:44.1000002Z             got:    sha256-6xXfuNrBAJZ3KPXPFlwt3i8cRs9w+wpAWtEnbTj0Ebc=
EOF
pkg="$work/p2.nix"; make_pkg "$pkg"
out="$("$script" "$log" "$pkg" 2>/dev/null)"
check "1275: rewrites to that bump's hash" "sha256-6xXfuNrBAJZ3KPXPFlwt3i8cRs9w+wpAWtEnbTj0Ebc=" "$out"

# ---------------------------------------------------------------- case 3
# THE ONE THAT MATTERS. An npm fetchurl FOD mismatch and NOTHING else. A parser
# that greps for `got:` rewrites vendorHash to a tarball hash here.
log="$work/npm.log"
cat > "$log" <<'EOF'
2026-09-07T13:20:00.0000000Z error: hash mismatch in fixed-output derivation '/nix/store/aaaa-vitest-4.1.11.tgz.drv':
2026-09-07T13:20:00.0000001Z          specified: sha512-AAAA=
2026-09-07T13:20:00.0000002Z             got:    sha256-NPMnpmNPMnpmNPMnpmNPMnpmNPMnpmNPMnpmNPMnpmA=
EOF
pkg="$work/p3.nix"; make_pkg "$pkg"
out="$("$script" "$log" "$pkg" 2>/dev/null)"; rc=$?
check "npm-only: exits 1" "1" "$rc"
check "npm-only: prints nothing on stdout" "" "$out"
check "npm-only: leaves vendorHash alone" "$ORIG" "$(hash_in "$pkg")"

# ---------------------------------------------------------------- case 4
# npm mismatch FIRST, go-modules second: the go-modules hash must win.
log="$work/both.log"
cat "$work/npm.log" "$work/1274.log" > "$log"
pkg="$work/p4.nix"; make_pkg "$pkg"
out="$("$script" "$log" "$pkg" 2>/dev/null)"
check "npm-then-go: takes the go-modules hash" "sha256-2hHhUx/J9CgDGU4fE5EE2Y/h6nRTA5vZ7GzuQ2oajkk=" "$out"

# ---------------------------------------------------------------- case 5
# go-modules mismatch FIRST, an unrelated FOD after it: the later block must not
# be attributed to the armed one.
log="$work/both2.log"
cat "$work/1274.log" "$work/npm.log" > "$log"
pkg="$work/p5.nix"; make_pkg "$pkg"
out="$("$script" "$log" "$pkg" 2>/dev/null)"
check "go-then-npm: still the go-modules hash" "sha256-2hHhUx/J9CgDGU4fE5EE2Y/h6nRTA5vZ7GzuQ2oajkk=" "$out"

# ---------------------------------------------------------------- case 6
# A build that failed for a REAL reason. Nothing to rewrite, and the caller
# must be able to tell, because this is the case where red is correct.
log="$work/real.log"
cat > "$log" <<'EOF'
2026-09-07T13:30:00.0000000Z internal/store/items.go:412:2: undefined: ErrNope
2026-09-07T13:30:00.0000001Z error: builder for '/nix/store/bbbb-pad-0.15.0.drv' failed with exit code 1
EOF
pkg="$work/p6.nix"; make_pkg "$pkg"
"$script" "$log" "$pkg" >/dev/null 2>&1; rc=$?
check "real failure: exits 1" "1" "$rc"
check "real failure: leaves vendorHash alone" "$ORIG" "$(hash_in "$pkg")"

# ---------------------------------------------------------------- case 7
# Idempotence: a log whose got: equals what is already pinned rewrites to the
# same bytes, so a re-run of the fixing workflow pushes nothing.
log="$work/same.log"
cat > "$log" <<EOF
error: hash mismatch in fixed-output derivation '/nix/store/cccc1qv8w0k3n7d2m5x9c4b6f8h0j2l4-pad-0.15.0-go-modules.drv':
         specified: $ORIG
            got:    $ORIG
EOF
pkg="$work/p7.nix"; make_pkg "$pkg"
before="$(cat "$pkg")"
"$script" "$log" "$pkg" >/dev/null 2>&1
check "idempotent: file is byte-identical" "$before" "$(cat "$pkg")"

# ---------------------------------------------------------------- case 8
# A package.nix with no vendorHash line is a usage error (2), not a silent pass.
log="$work/1274.log"
pkg="$work/p8.nix"; echo '{ pname = "pad"; }' > "$pkg"
"$script" "$log" "$pkg" >/dev/null 2>&1; rc=$?
check "no vendorHash line: exits 2" "2" "$rc"

# ---------------------------------------------------------------- case 9
# An unrelated `error:` line lands BETWEEN the go-modules header and its `got:`
# — CI interleaves output, so this is a real log shape. The armed state must be
# dropped rather than carried across it: carrying it would attribute the NEXT
# FOD's `got:` to vendorHash, and a wrong hash written confidently is worse than
# a build left red. Red is the correct outcome here.
log="$work/interleaved.log"
cat > "$log" <<'EOF'
2026-09-07T13:40:00.0000000Z error: hash mismatch in fixed-output derivation '/nix/store/dddd1qv8w0k3n7d2m5x9c4b6f8h0j2l4-pad-0.15.0-go-modules.drv':
2026-09-07T13:40:00.0000001Z          specified: sha256-8L7gH7Yy5+Fig3wK2SPLYSJjcY9nF/jumQ7PATJ3RIE=
2026-09-07T13:40:00.0000002Z error: unrelated interleaved failure from another build
2026-09-07T13:40:00.0000003Z             got:    sha256-NPMnpmNPMnpmNPMnpmNPMnpmNPMnpmNPMnpmNPMnpmA=
EOF
pkg="$work/p9.nix"; make_pkg "$pkg"
"$script" "$log" "$pkg" >/dev/null 2>&1; rc=$?
check "interleaved error: exits 1 rather than guessing" "1" "$rc"
check "interleaved error: leaves vendorHash alone" "$ORIG" "$(hash_in "$pkg")"

# --------------------------------------------------------------- case 10
# Two go-modules mismatch blocks in one log. FIRST wins, and exactly one hash is
# printed — the caller reads stdout as a single value, so a second line would be
# a silent corruption of whatever consumes it.
log="$work/twice.log"
cat "$work/1274.log" "$work/1275.log" > "$log"
pkg="$work/p10.nix"; make_pkg "$pkg"
out="$("$script" "$log" "$pkg" 2>/dev/null)"
check "two blocks: first wins" "sha256-2hHhUx/J9CgDGU4fE5EE2Y/h6nRTA5vZ7GzuQ2oajkk=" "$out"
check "two blocks: exactly one line on stdout" "1" "$(printf '%s\n' "$out" | wc -l | tr -d ' ')"

# --------------------------------------------------------------- case 11
# IMPERSONATION SET. Each of these carries the shape of a nix diagnostic without
# being one, and each must leave vendorHash alone. They exist because the first
# draft of this parser matched substrings, and every line below defeats that
# draft while looking, to a reader, like the real thing.
impersonation_case() {
	local name="$1" text="$2"
	local l="$work/imp.log" k="$work/imp.nix"
	printf '%s\n' "$text" > "$l"
	make_pkg "$k"
	"$script" "$l" "$k" >/dev/null 2>&1
	check "impersonation ($name): vendorHash untouched" "$ORIG" "$(hash_in "$k")"
}
impersonation_case "warning, not error" \
"warning: hash mismatch in fixed-output derivation '/nix/store/eeee1qv8w0k3n7d2m5x9c4b6f8h0j2l4-pad-0.15.0-go-modules.drv':
         specified: $ORIG
            got:    sha256-IMPimpIMPimpIMPimpIMPimpIMPimpIMPimpIMPimpA="
impersonation_case "not a store path" \
"error: hash mismatch in fixed-output derivation '/tmp/fake-pad-0.15.0-go-modules.drv':
         specified: $ORIG
            got:    sha256-IMPimpIMPimpIMPimpIMPimpIMPimpIMPimpIMPimpA="
impersonation_case "a name that merely CONTAINS -pad-" \
"error: hash mismatch in fixed-output derivation '/nix/store/oooo1qv8w0k3n7d2m5x9c4b6f8h0j2l4-other-pad-tool-1.0-go-modules.drv':
         specified: $ORIG
            got:    sha256-IMPimpIMPimpIMPimpIMPimpIMPimpIMPimpIMPimpA="
impersonation_case "another package's go-modules FOD" \
"error: hash mismatch in fixed-output derivation '/nix/store/ffff1qv8w0k3n7d2m5x9c4b6f8h0j2l4-othertool-1.2.3-go-modules.drv':
         specified: $ORIG
            got:    sha256-IMPimpIMPimpIMPimpIMPimpIMPimpIMPimpIMPimpA="
impersonation_case "trailing text after the drv path" \
"error: hash mismatch in fixed-output derivation '/nix/store/gggg1qv8w0k3n7d2m5x9c4b6f8h0j2l4-pad-0.15.0-go-modules.drv': while evaluating something
         specified: $ORIG
            got:    sha256-IMPimpIMPimpIMPimpIMPimpIMPimpIMPimpIMPimpA="
impersonation_case "a bare got: line with no header at all" \
"some build tool says: got:    sha256-IMPimpIMPimpIMPimpIMPimpIMPimpIMPimpIMPimpA="

# --------------------------------------------------------------- case 12
# A stray `got:` inside the armed block, BEFORE the real specified/got pair.
# After the timestamp strip such a line is byte-identical to nix's, so the only
# thing that separates them is the sequence.
log="$work/strayget.log"
cat > "$log" <<EOF
2026-09-07T14:00:00.0000000Z error: hash mismatch in fixed-output derivation '/nix/store/hhhh1qv8w0k3n7d2m5x9c4b6f8h0j2l4-pad-0.15.0-go-modules.drv':
2026-09-07T14:00:00.0000001Z got:    sha256-STRAYstrayHASHstrayHASHstrayHASHstrayHASHstrayA=
2026-09-07T14:00:00.0000002Z          specified: $ORIG
2026-09-07T14:00:00.0000003Z             got:    sha256-2hHhUx/J9CgDGU4fE5EE2Y/h6nRTA5vZ7GzuQ2oajkk=
EOF
pkg="$work/p12.nix"; make_pkg "$pkg"
out="$("$script" "$log" "$pkg" 2>/dev/null)"
check "stray got: before the pair is ignored" "sha256-2hHhUx/J9CgDGU4fE5EE2Y/h6nRTA5vZ7GzuQ2oajkk=" "$out"

# --------------------------------------------------------------- case 13
# Formatting variants of the vendorHash line. The rewrite must still land — an
# `= ` with extra spaces is a nix-fmt away and would otherwise silently return
# the whole class of PR to permanently red.
log="$work/1274.log"
for variant in 'vendorHash   =   "PLACEHOLDER";' '	vendorHash = "PLACEHOLDER";' 'vendorHash="PLACEHOLDER";'; do
	pkg="$work/pv.nix"
	printf '{\n%s\n}\n' "${variant/PLACEHOLDER/$ORIG}" > "$pkg"
	"$script" "$log" "$pkg" >/dev/null 2>&1
	check "spacing variant [$variant]" "sha256-2hHhUx/J9CgDGU4fE5EE2Y/h6nRTA5vZ7GzuQ2oajkk=" "$(hash_in "$pkg")"
done

# --------------------------------------------------------------- case 14
# Exit codes on the positive paths, and on the idempotent one. Asserting only
# the file contents lets a script that fails AFTER writing pass as success.
log="$work/1274.log"
pkg="$work/p14.nix"; make_pkg "$pkg"
"$script" "$log" "$pkg" >/dev/null 2>&1
check "positive path: exit 0" "0" "$?"
log="$work/same.log"
pkg="$work/p14b.nix"; make_pkg "$pkg"
"$script" "$log" "$pkg" >/dev/null 2>&1
check "idempotent path: exit 0" "0" "$?"

# --------------------------------------------------------------- case 15
# A log captured LOCALLY, with nix's real indentation and no runner timestamps.
# This is the shape a human gets from `nix build 2>&1 | tee`, which is what
# nix/package.nix's comment tells them to run. The first version of the parser
# folded the timestamp and the indentation into one strip and silently handled
# only the CI shape; nothing caught it until a case asserted the EXIT CODE
# rather than just that the file was unchanged — for a no-op input, "did not
# write" and "could not parse" leave identical files.
log="$work/local.log"
cat > "$log" <<EOF
error: hash mismatch in fixed-output derivation '/nix/store/iiii1qv8w0k3n7d2m5x9c4b6f8h0j2l4-pad-0.15.0-go-modules.drv':
         specified: $ORIG
            got:    sha256-2hHhUx/J9CgDGU4fE5EE2Y/h6nRTA5vZ7GzuQ2oajkk=
EOF
pkg="$work/p15.nix"; make_pkg "$pkg"
out="$("$script" "$log" "$pkg" 2>/dev/null)"; rc=$?
check "local log (no timestamps): exit 0" "0" "$rc"
check "local log (no timestamps): rewrites" "sha256-2hHhUx/J9CgDGU4fE5EE2Y/h6nRTA5vZ7GzuQ2oajkk=" "$out"

# --------------------------------------------------------------- case 16
# Round-2 impersonations. Each one satisfied an EARLIER version of this parser.
impersonation_case "nested path under /nix/store" \
"error: hash mismatch in fixed-output derivation '/nix/store/fake/not-a-store-pad-x-go-modules.drv':
         specified: $ORIG
            got:    sha256-IMPimpIMPimpIMPimpIMPimpIMPimpIMPimpIMPimpA="
impersonation_case "got: adjacent to nothing — three lines after specified:" \
"error: hash mismatch in fixed-output derivation '/nix/store/jjjj1qv8w0k3n7d2m5x9c4b6f8h0j2l4-pad-0.15.0-go-modules.drv':
         specified: $ORIG
building '/nix/store/kkkk-something-else.drv'
copying path '/nix/store/llll-another' from 'https://cache.nixos.org'
            got:    sha256-IMPimpIMPimpIMPimpIMPimpIMPimpIMPimpIMPimpA="
impersonation_case "non-canonical (short) hash on both lines" \
"error: hash mismatch in fixed-output derivation '/nix/store/mmmm1qv8w0k3n7d2m5x9c4b6f8h0j2l4-pad-0.15.0-go-modules.drv':
         specified: sha256-A=
            got:    sha256-B="
# Canonical `specified:`, short `got:`. Without this the length check on the got
# line is never exercised on its own — the specified line's check refuses the
# case above before the got line is reached, and a mutant that drops only the
# got-side length survives. Found by mutating, not by reading.
impersonation_case "non-canonical hash on the got: line only" \
"error: hash mismatch in fixed-output derivation '/nix/store/nnnn1qv8w0k3n7d2m5x9c4b6f8h0j2l4-pad-0.15.0-go-modules.drv':
         specified: $ORIG
            got:    sha256-B="

# --------------------------------------------------------------- case 17
# ROUND-3 MUTATION HOLES. Each of these was added because a faithful mutant
# SURVIVED the suite: the case that should have caught it was being refused for
# a second reason, so it discriminated nothing about the rule it was written for.
H32='aaaa1qv8w0k3n7d2m5x9c4b6f8h0j2l4'

# Ours, but not the module set: another FOD of the same package. Only the
# `-go-modules` part of the anchor separates it.
impersonation_case "our package, a different FOD" \
"error: hash mismatch in fixed-output derivation '/nix/store/$H32-pad-0.15.0-source.drv':
         specified: $ORIG
            got:    sha256-IMPimpIMPimpIMPimpIMPimpIMPimpIMPimpIMPimpA="

# A store path whose ONLY defect is a slash — everything else, including the
# 32-character store hash, is well formed. Without the single-segment rule this
# is accepted.
impersonation_case "a slash inside the derivation name" \
"error: hash mismatch in fixed-output derivation '/nix/store/$H32-pad-0.15.0/x-go-modules.drv':
         specified: $ORIG
            got:    sha256-IMPimpIMPimpIMPimpIMPimpIMPimpIMPimpIMPimpA="

# Short `specified:`, CANONICAL `got:`. The mirror of case 16's last entry: with
# both short, the got-side rule refuses it first and the specified-side rule is
# never exercised.
impersonation_case "non-canonical hash on the specified: line only" \
"error: hash mismatch in fixed-output derivation '/nix/store/$H32-pad-0.15.0-go-modules.drv':
         specified: sha256-A=
            got:    sha256-2hHhUx/J9CgDGU4fE5EE2Y/h6nRTA5vZ7GzuQ2oajkk="

# Our header, then a DIFFERENT FOD's complete mismatch block. The adjacency rule
# alone cannot refuse this — the npm block's specified/got ARE adjacent — so
# what refuses it is dropping the armed state at the second `error:`.
log="$work/twoblocks.log"
cat > "$log" <<EOF
error: hash mismatch in fixed-output derivation '/nix/store/$H32-pad-0.15.0-go-modules.drv':
error: hash mismatch in fixed-output derivation '/nix/store/${H32}bb-vitest-4.1.11.tgz.drv':
         specified: $ORIG
            got:    sha256-IMPimpIMPimpIMPimpIMPimpIMPimpIMPimpIMPimpA=
EOF
pkg="$work/p17.nix"; make_pkg "$pkg"
"$script" "$log" "$pkg" >/dev/null 2>&1
check "a second error: block does not inherit our arming" "$ORIG" "$(hash_in "$pkg")"

echo "bump-vendor-hash: $pass passed, $fail failed${BVH_AWK:+ (awk: $BVH_AWK)}"
[[ "$fail" -eq 0 ]] || exit 1

# PORTABILITY LEG. The script is awk, and `awk` is a different program depending
# on where it runs: gawk on this workstation, mawk on the ubuntu-24.04 runners
# where the Dependabot path actually executes. A regex feature one accepts and
# the other rejects would pass every case above and still fail the only run that
# matters. So the suite re-runs itself once per awk implementation present,
# through a PATH shim, and is green only if all of them are.
if [[ -z "${BVH_SUBRUN:-}" ]]; then
	rc=0
	for impl in mawk gawk busybox-awk; do
		case "$impl" in
			busybox-awk) command -v busybox >/dev/null || continue; target="$(command -v busybox)" ;;
			*) command -v "$impl" >/dev/null || continue; target="$(command -v "$impl")" ;;
		esac
		shim="$(mktemp -d)"
		if [[ "$impl" == busybox-awk ]]; then
			printf '#!/bin/sh\nexec %s awk "$@"\n' "$target" > "$shim/awk"
			chmod +x "$shim/awk"
		else
			ln -s "$target" "$shim/awk"
		fi
		BVH_SUBRUN=1 BVH_AWK="$impl" PATH="$shim:$PATH" "$0" || rc=1
		rm -rf "$shim"
	done
	exit "$rc"
fi
