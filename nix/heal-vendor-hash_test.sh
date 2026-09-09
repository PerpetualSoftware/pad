#!/usr/bin/env bash
#
# Tests for heal-vendor-hash.sh (BUG-2974), driven against a real git remote.
#
# The load-bearing case is CASE 1: a heal that is owed by an EARLIER push, on a
# push that touched no go.mod/go.sum. That is the shape that shipped green — the
# pre-fix job gated on the push RANGE, so after a lost push race every later
# merge refused to fix main and exited 0 while `nix build` from a clean checkout
# stayed broken. Case 1's pre-fix leg is a FROZEN reproduction of that gate: it
# is here to show what the fix is for, and it must keep failing to heal.
#
# Cases 4 and 5 are the pair that make the design honest rather than merely
# working: a rejected push must leave the run RED and main UNTOUCHED (4), and
# the NEXT push to main must then heal it without anyone intervening (5). Case 3
# covers the contradiction arm — an artifact identical to the committed file
# means the two jobs disagree about the same tree, which is loud, not a no-op.
#
# `git` is the only dependency; no nix, no toolchain, ~2s.
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
script="$here/heal-vendor-hash.sh"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

pass=0
fail=0

STALE='sha256-STALEcccccccccccccccccccccccccccccccccccccc='
GOOD='sha256-GOODcccccccccccccccccccccccccccccccccccccccc='

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

make_pkg() { printf '{\n  pname = "pad";\n\n  vendorHash = "%s";\n}\n' "$1" > "$2"; }
hash_on_main() { git -C "$1" show origin/main:nix/package.nix | grep -oE 'sha256-[A-Za-z0-9+/]+=*' | head -1; }

# The job under test, with the download-artifact step it depends on.
heal() { # workdir artifact sha
	( cd "$1" || exit 9
	  cp "$2" nix/package.nix
	  SHA="$3" HEAL_ALLOW_LOCAL=1 "$script" )
}

# A FROZEN reproduction of the pre-BUG-2974 job: the push-range gate, then the
# same commit-and-push. Not the shipped code and never will be again — it is the
# negative control that gives case 1 its meaning.
heal_prefix_form() { # workdir artifact before after
	( cd "$1" || exit 9
	  if ! git diff --name-only "$3" "$4" | command grep -qE '^go\.(mod|sum)$'; then
		echo "go.mod/go.sum unchanged across $3..$4 — nothing here could have moved the hash."
		exit 0
	  fi
	  cp "$2" nix/package.nix
	  git diff --quiet -- nix/package.nix && exit 0
	  git add nix/package.nix
	  git commit -q -m "chore(nix): heal vendorHash for the module set in $4 (TASK-2954)"
	  git push -q )
}

# A remote whose main is STALE, having got there the way BUG-2974 describes: M1
# moved the module set, M1's heal lost the push race and never landed, and M2
# then landed touching nothing the hash depends on.
scenario() { # name -> prints the clone path; writes $d/M1, $d/M2, $d/artifact.nix
	local name="$1"
	local d="$work/$name"
	mkdir -p "$d"
	git init -q --bare "$d/origin.git"
	git -C "$d/origin.git" symbolic-ref HEAD refs/heads/main
	git clone -q "$d/origin.git" "$d/work" 2>/dev/null
	(
		cd "$d/work" || exit 9
		git config user.email pad@test; git config user.name pad
		mkdir -p nix
		make_pkg "$GOOD" nix/package.nix
		echo 'module x' > go.mod; echo 'sum' > go.sum; echo hi > README.md
		git add -A; git commit -q -m base; git push -q origin HEAD:main
		git branch -q --set-upstream-to=origin/main "$(git rev-parse --abbrev-ref HEAD)" 2>/dev/null

		# M1 moves the module set, so the committed hash is stale from here on.
		echo 'module x2' > go.mod
		make_pkg "$STALE" nix/package.nix
		git add -A; git commit -q -m M1
		git rev-parse HEAD > "$d/M1"

		# M2 is an ordinary merge that touches no go.mod/go.sum.
		echo bye > README.md; git add -A; git commit -q -m M2
		git rev-parse HEAD > "$d/M2"
		git push -q origin HEAD:main
	)
	make_pkg "$GOOD" "$d/artifact.nix"
	printf '%s' "$d"
}

# ---------------------------------------------------------------- case 1
# The heal is owed by an EARLIER push; this push touched no go.mod/go.sum.
d="$(scenario case1_prefix)"
heal_prefix_form "$d/work" "$d/artifact.nix" "$(cat "$d/M1")" "$(cat "$d/M2")" >/dev/null 2>&1
check "1 pre-fix form: exits 0" "0" "$?"
git -C "$d/work" fetch -q origin main
check "1 pre-fix form: leaves main STALE (this is BUG-2974)" "$STALE" "$(hash_on_main "$d/work")"

d="$(scenario case1)"
heal "$d/work" "$d/artifact.nix" "$(cat "$d/M2")" >/dev/null 2>&1
check "1: exits 0" "0" "$?"
git -C "$d/work" fetch -q origin main
check "1: heals main" "$GOOD" "$(hash_on_main "$d/work")"

# ---------------------------------------------------------------- case 2
# The ordinary case the pre-fix form also handled: this push moved the module
# set itself. Must not regress.
d="$(scenario case2)"
heal "$d/work" "$d/artifact.nix" "$(cat "$d/M1")" >/dev/null 2>&1
check "2 same-push heal: exits 0" "0" "$?"
git -C "$d/work" fetch -q origin main
check "2 same-push heal: heals main" "$GOOD" "$(hash_on_main "$d/work")"

# ---------------------------------------------------------------- case 3
# The recomputed artifact is identical to the committed file. The build job said
# mismatch; both cannot be true, so this is loud rather than a silent success.
d="$(scenario case3)"
make_pkg "$STALE" "$d/artifact.nix"
out="$(heal "$d/work" "$d/artifact.nix" "$(cat "$d/M2")" 2>&1)"
check "3 contradiction: exits 1" "1" "$?"
case "$out" in *"::error::"*) got=yes ;; *) got=no ;; esac
check "3 contradiction: annotates the run" "yes" "$got"

# ---------------------------------------------------------------- case 4
# A competing commit reaches main mid-job: the push is rejected.
d="$(scenario case4)"
git clone -q "$d/origin.git" "$d/rival" 2>/dev/null
(
	cd "$d/rival" || exit 9
	git config user.email rival@test; git config user.name rival
	git checkout -q -B main origin/main
	echo rival > RIVAL.md; git add -A; git commit -q -m rival; git push -q origin main
)
out="$(heal "$d/work" "$d/artifact.nix" "$(cat "$d/M2")" 2>&1)"
check "4 lost race: exits 1" "1" "$?"
case "$out" in *"did NOT land"*) got=yes ;; *) got=no ;; esac
check "4 lost race: says the heal did not land" "yes" "$got"
git -C "$d/work" fetch -q origin main
check "4 lost race: leaves main untouched" "$STALE" "$(hash_on_main "$d/work")"

# ---------------------------------------------------------------- case 5
# ...and the NEXT push to main heals it, with nobody intervening. This is the
# recovery the push-range gate refused, and the whole point of gating on state.
git -C "$d/work" fetch -q origin
git -C "$d/work" reset -q --hard origin/main
heal "$d/work" "$d/artifact.nix" "$(git -C "$d/work" rev-parse HEAD)" >/dev/null 2>&1
check "5 recovery: exits 0" "0" "$?"
git -C "$d/work" fetch -q origin main
check "5 recovery: the following run heals main" "$GOOD" "$(hash_on_main "$d/work")"

# ---------------------------------------------------------------- case 6
# The script commits and pushes; running it by hand outside CI must refuse.
d="$(scenario case6)"
out="$( cd "$d/work" && cp "$d/artifact.nix" nix/package.nix && SHA=deadbeef "$script" 2>&1 )"
check "6 local guard: exits 2" "2" "$?"
case "$out" in *"refusing to run outside GitHub Actions"*) got=yes ;; *) got=no ;; esac
check "6 local guard: says why" "yes" "$got"

# ---------------------------------------------------------------- case 7
# SHA is what the commit message names; an unset one is a usage error, not a
# commit that says "in ". `set -u` would also stop this — one line later, at
# `git commit`, with the index already dirty and a message naming a shell
# variable rather than the missing input. The assertion that discriminates the
# explicit check from that fallback is WHERE it stops, so this asserts the
# index, not just the exit code.
d="$(scenario case7)"
out="$( cd "$d/work" && cp "$d/artifact.nix" nix/package.nix && HEAL_ALLOW_LOCAL=1 "$script" 2>&1 )"
check "7 missing SHA: exits non-zero" "1" "$?"
case "$out" in *SHA*) got=yes ;; *) got=no ;; esac
check "7 missing SHA: names the missing input" "yes" "$got"
git -C "$d/work" diff --cached --quiet
check "7 missing SHA: stops before staging anything" "0" "$?"

echo "heal-vendor-hash: $pass passed, $fail failed"
[[ $fail -eq 0 ]]
