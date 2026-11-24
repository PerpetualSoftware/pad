#!/usr/bin/env bash
#
# test-shard.sh: split one Go package's tests across N CI jobs (TASK-4141).
#
# internal/server under -race outgrew the 45m per-binary timeout on both
# drivers (2658s on PostgreSQL, 2519s on SQLite, against 2700s). No test or
# family is more than ~4% of the wall; ~88% of it is serial tests. So the
# package runs as N jobs, each with a disjoint slice of its top-level tests.
# internal/store followed (3155s on SQLite against 3600s; TASK-4648) and is
# split the same way.
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
# (i mod N) + 1. Names pinned for the package in scripts/test-shard-spread.txt
# (or the file $TEST_SHARD_SPREAD names) come first, in file order, so the
# heaviest tests are dealt to different shards whatever names are added
# around them; the rest follow in sorted order. A pin that names no test in
# the package, or names one twice, is an error, so a renamed test cannot
# leave a stale pin behind.
#
# The build flags MUST be the ones the shard jobs test with. -list reports
# the tests of the binary those flags build, so a build-tagged file is in the
# list exactly when it is in the binary the shards run, and the two cannot
# disagree. TestMain is not a test: every shard runs the whole binary, so
# TestMain runs once per shard as it does for an unsharded run. Subtests ride
# with their top-level test (the regex has no '/').
#
# `check` asks Go, not this script, which tests each regex selects: the test
# binary lists them with -test.list <shard regex>, the matcher `go test -list`
# uses, so a regex that Go reads differently from how it was written is caught
# too. The binary is built once per run with the given flags and run in the
# package directory, as `go test` runs it; a `go test -list` per listing
# relinked it every time (TASK-4814).

set -euo pipefail

die() {
	echo "test-shard: $*" >&2
	exit 1
}

# One argv string is capped at 128KiB on Linux (MAX_ARG_STRLEN). Refuse well
# before it rather than let exec fail with E2BIG mid-run.
max_regex_bytes=120000

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
bin=$work/pkg.test
bin_pkg=""
bin_dir=""

# build_bin <pkg> [flags...]: build the package's test binary, once. A package
# with no test files builds none, and lists no tests.
build_bin() {
	local pkg=$1
	shift
	[[ -z $bin_pkg ]] || { [[ $bin_pkg == "$pkg" ]] || die "one package per run"; return 0; }
	go test "$@" -c -o "$bin" "$pkg" >/dev/null || die "go test -c failed for $pkg"
	bin_dir=$(go list -f '{{.Dir}}' "$pkg") || die "go list failed for $pkg"
	bin_pkg=$pkg
}

# list_tests <pkg> <regex> [flags...]: the top-level tests the regex selects.
list_tests() {
	local pkg=$1 regex=$2
	shift 2
	local out
	build_bin "$pkg" "$@"
	[[ -f $bin ]] || return 0
	out=$(cd "$bin_dir" && "$bin" -test.list "$regex") || die "listing tests failed for $pkg"
	printf '%s\n' "$out" | grep -E '^(Test|Fuzz|Example)' | LC_ALL=C sort || true
}

check_n() {
	[[ $1 =~ ^[1-9][0-9]*$ ]] || die "N must be a positive integer, got '$1'"
}

spread=${TEST_SHARD_SPREAD:-$(dirname "$0")/test-shard-spread.txt}

# ordered_tests <pkg> [flags...]: the package's tests in dealing order, the
# pinned ones first.
ordered_tests() {
	local pkg=$1
	shift
	local all key pins="" p rest
	all=$(list_tests "$pkg" '.' "$@") || exit 1
	key=${pkg#./}
	key=${key%/}
	if [[ -f $spread ]]; then
		pins=$(awk -v key="$key" '
			/^[[:space:]]*(#|$)/ { next }
			NF != 2 { printf "bad line %d: %s\n", NR, $0 > "/dev/stderr"; exit 1 }
			$1 == key { print $2 }' "$spread") || die "malformed $spread"
	fi
	if [[ -n $pins ]]; then
		[[ -z $(printf '%s\n' "$pins" | LC_ALL=C sort | uniq -d) ]] || die "$spread pins a $key test twice"
		while IFS= read -r p; do
			grep -qxF -- "$p" <<<"$all" || die "$spread pins $p, which $pkg does not have"
		done <<<"$pins"
		printf '%s\n' "$pins"
		rest=$(LC_ALL=C comm -23 <(printf '%s\n' "$all") <(printf '%s\n' "$pins" | LC_ALL=C sort))
		[[ -z $rest ]] || printf '%s\n' "$rest"
	else
		printf '%s\n' "$all"
	fi
}

shard_regex() {
	local pkg=$1 n=$2 k=$3
	shift 3
	local order names
	# Its own assignment, so a die in ordered_tests stops the script.
	order=$(ordered_tests "$pkg" "$@")
	names=$(printf '%s\n' "$order" | awk -v n="$n" -v k="$k" 'NF && (NR - 1) % n == k - 1')
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
	# Built here, not inside a substitution: a subshell's build is lost to
	# the next one.
	build_bin "$pkg" "$@"
	shard_regex "$pkg" "$n" "$k" "$@"
	;;
check)
	(($# >= 2)) || die "usage: $0 check <pkg> <N> [flags...]"
	pkg=$1 n=$2
	shift 2
	check_n "$n"
	build_bin "$pkg" "$@"
	list_tests "$pkg" '.' "$@" >"$work/all"
	total=$(wc -l <"$work/all")
	((total > 0)) || die "$pkg lists no tests"
	: >"$work/union"
	for ((k = 1; k <= n; k++)); do
		# Its own assignment, not an argument: set -e ignores a die inside a
		# substitution used as an argument, and -list "" then selects nothing,
		# so an empty shard read as 0 tests and the guard passed.
		re=$(shard_regex "$pkg" "$n" "$k" "$@")
		list_tests "$pkg" "$re" "$@" >"$work/shard$k"
		echo "shard $k/$n: $(wc -l <"$work/shard$k") tests"
		cat "$work/shard$k" >>"$work/union"
	done
	fail=0
	dups=$(LC_ALL=C sort "$work/union" | uniq -d)
	if [[ -n $dups ]]; then
		echo "test-shard: tests selected by MORE THAN ONE shard:" >&2
		printf '%s\n' "$dups" >&2
		fail=1
	fi
	missing=$(LC_ALL=C sort -u "$work/union" | LC_ALL=C comm -23 "$work/all" -)
	if [[ -n $missing ]]; then
		echo "test-shard: tests selected by NO shard:" >&2
		printf '%s\n' "$missing" >&2
		fail=1
	fi
	extra=$(LC_ALL=C sort -u "$work/union" | LC_ALL=C comm -13 "$work/all" -)
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
