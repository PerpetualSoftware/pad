#!/usr/bin/env bash
#
# test-shard.sh: split one Go package's tests across N CI jobs (TASK-4141).
#
# internal/server under -race outgrew the 45m per-binary timeout on both
# drivers (2658s on PostgreSQL, 2519s on SQLite, against 2700s). No test or
# family is more than ~4% of the wall; ~88% of it is serial tests. So the
# package runs as N jobs, each with a disjoint slice of its top-level tests.
#
# Usage:
#   scripts/test-shard.sh regex <pkg> <N> <k> [go test build flags...]
#     Print the -run regex for shard k of N (1-based).
#   scripts/test-shard.sh check <pkg> <N> [go test build flags...]
#     Fail unless the N shards, as Go's own -run matcher reads them, are
#     pairwise disjoint and together equal the package's full test list.
#
# Shard membership: the package's top-level Test/Fuzz/Example names from
# `go test -list`, sorted (C locale), name i (0-based) goes to shard
# (i mod N) + 1.
#
# The build flags MUST be the ones the shard jobs test with. -list reports
# the tests of the binary those flags build, so a build-tagged file is in the
# list exactly when it is in the binary the shards run, and the two cannot
# disagree. TestMain is not a test: every shard runs the whole binary, so
# TestMain runs once per shard as it does for an unsharded run. Subtests ride
# with their top-level test (the regex has no '/').
#
# `check` asks Go, not this script, which tests each regex selects: it runs
# `go test -list <shard regex>`, so a regex that Go reads differently from
# how it was written is caught too.

set -euo pipefail

die() {
	echo "test-shard: $*" >&2
	exit 1
}

# One argv string is capped at 128KiB on Linux (MAX_ARG_STRLEN). Refuse well
# before it rather than let exec fail with E2BIG mid-run.
max_regex_bytes=120000

# list_tests <pkg> <regex> [flags...]: the top-level tests the regex selects.
list_tests() {
	local pkg=$1 regex=$2
	shift 2
	local out
	out=$(go test "$@" -list "$regex" "$pkg") || die "go test -list failed for $pkg"
	printf '%s\n' "$out" | grep -E '^(Test|Fuzz|Example)' | LC_ALL=C sort || true
}

check_n() {
	[[ $1 =~ ^[1-9][0-9]*$ ]] || die "N must be a positive integer, got '$1'"
}

shard_regex() {
	local pkg=$1 n=$2 k=$3
	shift 3
	local names
	names=$(list_tests "$pkg" '.' "$@" | awk -v n="$n" -v k="$k" '(NR - 1) % n == k - 1')
	[[ -n $names ]] || die "shard $k/$n of $pkg is empty"
	local regex="^($(printf '%s\n' "$names" | paste -sd'|'))\$"
	((${#regex} <= max_regex_bytes)) ||
		die "shard $k/$n regex is ${#regex} bytes (limit $max_regex_bytes): raise N"
	printf '%s\n' "$regex"
}

cmd=${1:-}
[[ -n $cmd ]] || die "usage: $0 regex <pkg> <N> <k> [flags...] | check <pkg> <N> [flags...]"
shift

case $cmd in
regex)
	(($# >= 3)) || die "usage: $0 regex <pkg> <N> <k> [flags...]"
	pkg=$1 n=$2 k=$3
	shift 3
	check_n "$n"
	[[ $k =~ ^[1-9][0-9]*$ ]] && ((k <= n)) || die "k must be in 1..$n, got '$k'"
	shard_regex "$pkg" "$n" "$k" "$@"
	;;
check)
	(($# >= 2)) || die "usage: $0 check <pkg> <N> [flags...]"
	pkg=$1 n=$2
	shift 2
	check_n "$n"
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	list_tests "$pkg" '.' "$@" >"$tmp/all"
	total=$(wc -l <"$tmp/all")
	((total > 0)) || die "$pkg lists no tests"
	: >"$tmp/union"
	for ((k = 1; k <= n; k++)); do
		list_tests "$pkg" "$(shard_regex "$pkg" "$n" "$k" "$@")" "$@" >"$tmp/shard$k"
		echo "shard $k/$n: $(wc -l <"$tmp/shard$k") tests"
		cat "$tmp/shard$k" >>"$tmp/union"
	done
	fail=0
	dups=$(LC_ALL=C sort "$tmp/union" | uniq -d)
	if [[ -n $dups ]]; then
		echo "test-shard: tests selected by MORE THAN ONE shard:" >&2
		printf '%s\n' "$dups" >&2
		fail=1
	fi
	missing=$(LC_ALL=C sort -u "$tmp/union" | LC_ALL=C comm -23 "$tmp/all" -)
	if [[ -n $missing ]]; then
		echo "test-shard: tests selected by NO shard:" >&2
		printf '%s\n' "$missing" >&2
		fail=1
	fi
	extra=$(LC_ALL=C sort -u "$tmp/union" | LC_ALL=C comm -13 "$tmp/all" -)
	if [[ -n $extra ]]; then
		echo "test-shard: shards select tests the full list does not have:" >&2
		printf '%s\n' "$extra" >&2
		fail=1
	fi
	((fail == 0)) || exit 1
	echo "test-shard: $pkg: $n shards, $total tests, disjoint and complete"
	;;
*)
	die "unknown command '$cmd'"
	;;
esac
