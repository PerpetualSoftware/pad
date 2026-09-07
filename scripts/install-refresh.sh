#!/usr/bin/env bash
#
# install-refresh.sh — the honest half of `make install` (BUG-2897, TASK-2787).
#
# The target used to: kill the running pad, cp the binary, then bring the
# server back BY SIDE EFFECT (`pad auth whoami` triggers an auto-start) and
# print "Server restarted." unconditionally. Three things were wrong with
# that, and the third is the one that makes the other two invisible:
#
#   1. The auto-start does not know the killed process's argv, so a server
#      running `--host 0.0.0.0` came back bound to the default host alone.
#      Measured: curl 127.0.0.1:7777 -> 000 while the LAN address -> 200,
#      with the process count and the version both reading correct.
#   2. `cp` copies whatever is at the repo path, not what THIS invocation
#      built. Two sessions sharing the checkout interleave, and the loser's
#      build is installed by the winner with every exit code green.
#   3. "Server restarted." was printed after a command ending in `|| true`,
#      with no probe of any kind. It is not that the wrong address went
#      unverified — nothing was verified.
#
# So this script checks OUTCOMES, not steps: what got installed, and what is
# answering afterwards.
#
# Usage: install-refresh.sh <built-binary> <install-path> <expected-commit>
#
# It performs the stop, the install and the restart; the caller does the
# build. Ordering is load-bearing and matches CONVE-2687's day-74 clause:
# nothing irreversible happens until the artifact has been checked.

set -uo pipefail

BUILT="${1:?built binary path required}"
INSTALLED="${2:?install path required}"
EXPECT_COMMIT="${3:-}"
# PORT and HOST are resolved AFTER the argv capture, from the same
# precedence the server itself uses: flag, then environment, then default.
# See the resolution block below the capture.
PORT=""
PROBE_TIMEOUT="${PAD_PROBE_TIMEOUT:-20}"

die() { printf 'install-refresh: %s\n' "$*" >&2; exit 1; }
note() { printf 'install-refresh: %s\n' "$*"; }

# flag_value prints the value of a long flag from the captured argv,
# accepting BOTH supported spellings: `--host 1.2.3.4` and `--host=1.2.3.4`.
#
# Codex round 2 (P1): the first version matched only the space-separated
# form. Cobra accepts both, so a server started `--port=8080` was restarted
# on 8080 and probed on 7777 — a healthy restart failed by this script's
# most confident message. A parser that reads a subset of the syntax it is
# quoting back is the same defect class as a probe that reads half an
# invocation.
flag_value() {
	local want="$1"
	shift
	local i
	for ((i = 0; i < $#; i++)); do
		local a="${@:i+1:1}"
		case "$a" in
		"$want")
			printf '%s' "${@:i+2:1}"
			return 0
			;;
		"$want"=*)
			printf '%s' "${a#*=}"
			return 0
			;;
		esac
	done
	return 1
}

# config_value prints a top-level key from the pad config file, or nothing.
#
# The file is FLAT TOML (internal/config/config.go: Host, Port, Mode, ... are
# all top-level, no sections), so a line match is a correct reader for it
# rather than a hopeful one. Path resolution mirrors userConfigPath().
#
# Codex round 3 (P1): without this, a server whose host or port comes from
# the config file alone restarts correctly and is then probed on
# 127.0.0.1:7777 and reported as failed — on EVERY install, for that user.
# A stated boundary was the previous answer; it is a bad one when the cost of
# closing it is a grep and the cost of leaving it is a permanent false alarm.
config_value() {
	local key="$1" path="$HOME/.pad/config.toml"
	[ -n "${PAD_DATA_DIR:-}" ] && path="$PAD_DATA_DIR/config.toml"
	[ -n "${PAD_DB_PATH:-}" ] && path="$(dirname "$PAD_DB_PATH")/config.toml"
	[ -r "$path" ] || return 1
	# Strip an inline comment before taking the value: `port = 8080 # dev`
	# is valid TOML and the Go parser accepts it, so a regex that swallows
	# the comment resolves the port to "8080 # dev" and probes nothing
	# (codex round 4). A quoted value is unquoted first, so a `#` inside
	# quotes survives.
	sed -n "s/^[[:space:]]*$key[[:space:]]*=[[:space:]]*//p" "$path" |
		sed -e 's/^"\([^"]*\)".*$/\1/' -e 's/[[:space:]]*#.*$//' -e 's/[[:space:]]*$//' |
		head -1
}

# probe_url builds a health URL, bracketing IPv6 literals.
#
# Codex round 3 (P2): `::1` produced http://::1:7777/... — not a URL, so a
# healthy IPv6-bound server reads as unreachable. Accepts a value that is
# already bracketed, since that is how a user would write it in a flag.
probe_url() {
	local host="$1" port="$2"
	case "$host" in
	\[*\]) : ;;
	*:*) host="[$host]" ;;
	esac
	printf 'http://%s:%s/api/v1/health' "$host" "$port"
}

# commit_of prints the commit token a `--version` line carries, or nothing.
# Output shape: `pad version dev (a3a1d58 2026-09-07T12:33:21Z)`.
commit_of() {
	printf '%s' "$1" | sed -n 's/.*(\([0-9a-f][0-9a-f]*\)[ )].*/\1/p'
}

# commit_matches compares an EXPECTED abbreviation against a FOUND one,
# tolerating different abbreviation LENGTHS.
#
# This is not defensive slack. `git rev-parse --short` returns the shortest
# UNAMBIGUOUS prefix, so its length grows as the object database does — the
# end-to-end run that found this had a binary embedding `a3a1d58` (7) while
# the Makefile, computing the same expression minutes later after a fetch,
# produced `a3a1d586` (8). An equality test fails there and reports "another
# session rebuilt it", which is a confident, specific and completely wrong
# diagnosis of a healthy build.
#
# The unit tests could not have caught it: their fixtures use fixed strings
# of equal length, so the two sides always agreed on width. Production does
# not have that property, and neither does any check written against it.
commit_matches() {
	local expect="$1" found="$2"
	[ -n "$expect" ] && [ -n "$found" ] || return 1
	[ "$expect" = "$found" ] && return 0

	# Resolve both to FULL object ids and compare those. This is the exact
	# answer and it is available because `make install` runs in the repo.
	#
	# Codex round 5 (P1) on the previous prefix comparison: `a3a1d586`
	# accepted `a3a1d58` even when those are DIFFERENT commits that happen
	# to share seven characters. That is not hypothetical — it is the same
	# mechanism that made the abbreviation grow in the first place, so the
	# fix for the false alarm had introduced a false pass in the case the
	# check exists for. Prefix matching cannot tell the two apart; full ids
	# can.
	local a b
	a="$(git rev-parse --verify --quiet "${expect}^{commit}" 2>/dev/null || true)"
	b="$(git rev-parse --verify --quiet "${found}^{commit}" 2>/dev/null || true)"
	if [ -n "$a" ] && [ -n "$b" ]; then
		[ "$a" = "$b" ]
		return
	fi

	# FAIL CLOSED when an id cannot be resolved (codex round 9). A prefix
	# comparison admits exactly the collision this function was rewritten to
	# stop admitting, so keeping it as a fallback would reintroduce the
	# defect on the path where verification is weakest.
	#
	# What this refuses: a binary whose embedded commit is not an object in
	# THIS checkout — built from another history, or from a repository this
	# one does not share. `make install` computes the expected id with `git
	# rev-parse` in the same checkout it builds from, so the supported path
	# always resolves; anything that does not is a case where the script
	# genuinely cannot tell whether the artifact is the right one, and
	# saying so is the whole point of this unit.
	return 1
}

# --- 1. Capture the running server's argv BEFORE anything is killed ---------
#
# Identity, not count: other `pad` processes may exist (a sibling's CLI call,
# an e2e-spawned server from another worktree) and none of them is the thing
# being replaced. The server is the one with `server start` in its argv.
# Read from /proc so the exact tokens come back, including `--host`.
#
# Empty capture is a legitimate state — a box with no server running — and
# is handled at restart time rather than treated as an error here.
SERVER_ARGV=()
SERVER_PIDS=()
if command -v pgrep >/dev/null 2>&1; then
	while read -r pid; do
		[ -n "$pid" ] || continue
		# /proc where it exists, `ps` where it does not.
		#
		# Codex round 5 (P1): macOS has no /proc, so this loop found
		# nothing there, SERVER_ARGV stayed empty, and the restart fell
		# back to defaults — the exact defect BUG-2897 is about, silently
		# unfixed on the platform the setsid fallback had just been added
		# for. A fix that is Linux-only while advertising portability is
		# worse than one that admits its platform.
		#
		# /proc is preferred because it gives the argv NUL-separated and
		# therefore exactly; `ps -o args=` returns one space-joined string,
		# so an argument containing a space is split. Named rather than
		# hidden: no `pad server start` flag takes such a value today.
		# No `local` here: this loop runs at TOP LEVEL, and bash answers
		# `local` outside a function with an error on stderr for every
		# matching process (codex round 6). It kept working — the arrays
		# were still assigned — while printing a spurious error on every
		# normal refresh, which is precisely the class of noise this unit
		# exists to remove.
		argv=()
		# PAD_NO_PROC forces the `ps` path. Same reasoning as PAD_NO_SETSID:
		# the branch is unreachable on any machine with /proc, and an
		# untestable branch is exactly how the macOS half of this fix would
		# have shipped broken.
		if [ -z "${PAD_NO_PROC:-}" ] && [ -r "/proc/$pid/cmdline" ]; then
			mapfile -d '' -t argv < "/proc/$pid/cmdline"
		else
			line="$(ps -o args= -p "$pid" 2>/dev/null || true)"
			[ -n "$line" ] || continue
			read -r -a argv <<<"$line"
		fi
		[ ${#argv[@]} -gt 0 ] || continue
		# argv[0] is the binary; look for the `server start` verb pair.
		for ((i = 1; i < ${#argv[@]}; i++)); do
			if [ "${argv[i]}" = "server" ] && [ "${argv[i + 1]:-}" = "start" ]; then
				SERVER_PIDS+=("$pid")
				[ ${#SERVER_ARGV[@]} -eq 0 ] && SERVER_ARGV=("${argv[@]}")
				break
			fi
		done
	done < <(pgrep -x "$(basename "$BUILT")" 2>/dev/null || true)
fi

# MORE THAN ONE server is a case this script cannot answer honestly (codex
# round 9). The stop is `pkill -x`, which is system-wide and kills every
# matching process, while the restart can only bring back the argv of ONE.
# A sibling's worktree server would be killed and left down, or restarted
# with another server's arguments.
#
# Refusing here rather than guessing: CONVE-2687's manual sibling-safe
# recipe exists precisely for a box with more than one live server, and it
# aims its kill by /proc identity instead of by name.
if [ "${#SERVER_PIDS[@]}" -gt 1 ]; then
	die "more than one \`$(basename "$BUILT") server start\` process is running (pids: ${SERVER_PIDS[*]}).
  The stop below is a system-wide pkill and the restart can only restore one
  argv, so this would kill a server it cannot bring back.
  Nothing was stopped or installed. Use the manual sibling-safe refresh."
fi

if [ ${#SERVER_ARGV[@]} -gt 0 ]; then
	note "captured server argv: ${SERVER_ARGV[*]}"
else
	note "no running server found; will start with defaults after install"
fi

# --- 2. SNAPSHOT the artifact, then check the snapshot ------------------------
#
# The copy happens BEFORE the stop, into a temporary file beside the final
# path, and the check runs on that snapshot.
#
# Codex round 2 (P1) found the window this closes: checking $BUILT and then
# copying it later leaves room for another session to rebuild the shared repo
# binary in between, so the destination check catches the swap only AFTER the
# old server has been killed — turning a detectable race into an outage. A
# snapshot cannot change under us, so the expensive failure mode disappears
# rather than being detected later.
#
# The temp file lives in the DESTINATION directory so the final move is a
# rename within one filesystem, which is atomic: no window in which
# ~/.local/bin/pad is half-written.
mkdir -p "$(dirname "$INSTALLED")"
STAGED="$(mktemp "$(dirname "$INSTALLED")/.$(basename "$INSTALLED").XXXXXX")" ||
	die "could not stage the install next to $INSTALLED"
cleanup() { [ -n "${STAGED:-}" ] && rm -f "$STAGED"; }
trap cleanup EXIT

cp -f "$BUILT" "$STAGED" || die "cp failed"
chmod +x "$STAGED"

# The ARTIFACT check, on the snapshot, while the old server is still serving.
# Failing here costs nothing.
if [ -n "$EXPECT_COMMIT" ]; then
	staged_version="$("$STAGED" --version 2>/dev/null || true)"
	if ! commit_matches "$EXPECT_COMMIT" "$(commit_of "$staged_version")"; then
		die "the binary at $BUILT is not this invocation's build; refusing to stop the server.
  expected commit: $EXPECT_COMMIT
  $BUILT reads:    ${staged_version:-<no version output>}
  Another session very likely rebuilt it between this build and now.
  Nothing was stopped or installed; re-run the build and install together."
	fi
fi

# --- 3. Stop -----------------------------------------------------------------
#
# Unchanged from the previous target, including its system-wide reach: this
# is the plain path, and CONVE-2687's manual recipe remains the sibling-safe
# one. SIGTERM first so the graceful-shutdown path runs (BUG-1531).
BIN_NAME="$(basename "$BUILT")"
pkill -TERM -x "$BIN_NAME" 2>/dev/null || true
for _ in 1 2 3 4 5; do
	pgrep -x "$BIN_NAME" >/dev/null 2>&1 || break
	sleep 1
done
pkill -KILL -x "$BIN_NAME" 2>/dev/null || true

# --- 4. Move into place, then check the OUTCOME -------------------------------
mv -f "$STAGED" "$INSTALLED" || die "could not move the staged binary into $INSTALLED"
STAGED=""
rm -f "$HOME/.pad/pad.pid"

# The check TASK-2787 asks for, and it is deliberately on the installed file
# rather than on any step: read what is now at the install path and require it
# to carry the commit this invocation built with. Distinct from step 2 — that
# asked "is the source right", this asks "is the DESTINATION right", and it
# is the leg that survives another writer racing the final path.
#
# WHAT THIS ANSWERS, stated because the check reads stronger than it is: it
# catches "the binary here was built at a DIFFERENT commit", which is the
# reversed-ordering case TASK-2787 names. It does NOT establish byte
# provenance: a sibling who built the SAME commit leaves a binary this check
# accepts. That case is benign by the task's own scope note, and pretending
# otherwise would be the kind of overclaimed instrument this repo keeps
# filing bugs about.
if [ -n "$EXPECT_COMMIT" ]; then
	installed_version="$("$INSTALLED" --version 2>/dev/null || true)"
	if ! commit_matches "$EXPECT_COMMIT" "$(commit_of "$installed_version")"; then
		die "installed binary is not the one this invocation built.
  expected commit: $EXPECT_COMMIT
  installed reads: ${installed_version:-<no version output>}
  Another session very likely wrote $INSTALLED between this move and this read.
  Re-run the build and install together; do not trust the previous output."
	fi
fi
note "installed $BIN_NAME to $INSTALLED ($EXPECT_COMMIT)"

# --- 5. Restart with the argv that was killed --------------------------------
#
# The whole point of BUG-2897: a replacement is only correct if it preserves
# what it replaced, and that includes the INVOCATION, not just the bytes.
if [ ${#SERVER_ARGV[@]} -gt 0 ]; then
	restart=("$INSTALLED" "${SERVER_ARGV[@]:1}")
else
	restart=("$INSTALLED" server start)
fi
# mkdir before the redirect: a missing ~/.pad makes the redirection fail and
# the server never starts, which the probe then reports as "did not answer" —
# true, but three steps downstream of the cause. Found by the probe-failure
# test on a fresh HOME, where the directory does not exist; on a developer's
# box it exists by luck of history, which is why nothing had caught it.
mkdir -p "$HOME/.pad"
# setsid where it exists, plain nohup where it does not. Codex round 2
# (P1): setsid is Linux-specific and absent from a default macOS
# environment, where an unconditional call would fail AFTER the old server
# had been stopped — the one ordering this script exists to avoid. nohup
# plus the background job is sufficient detachment on both.
# PAD_NO_SETSID forces the fallback branch. It exists because the branch is
# otherwise unreachable on the machines this is developed and tested on —
# every Linux box has setsid — and an untestable branch is how the macOS
# case would have shipped broken a second time. Test-only knob, documented
# rather than hidden.
# Re-read the installed binary IMMEDIATELY before executing it.
#
# Codex round 4 (P1): a concurrent `make install` can replace $INSTALLED
# between the outcome check above and this exec, so this invocation would
# launch a build it never verified and report success.
#
# RESIDUAL, stated rather than glossed: the window is narrowed, not closed —
# the complete answer is an exclusive lock across the whole
# check-install-restart sequence. Deliberately not added here: flock is
# Linux-only, and a half-portable lock that silently does nothing on macOS is
# worse than a named residual. Filed separately.
if [ -n "$EXPECT_COMMIT" ]; then
	preexec_version="$("$INSTALLED" --version 2>/dev/null || true)"
	if ! commit_matches "$EXPECT_COMMIT" "$(commit_of "$preexec_version")"; then
		die "another session replaced $INSTALLED between the install check and the restart.
  expected commit: $EXPECT_COMMIT
  installed reads: ${preexec_version:-<no version output>}
  Refusing to start a binary this invocation did not verify. No server is running."
	fi
fi

if [ -z "${PAD_NO_SETSID:-}" ] && command -v setsid >/dev/null 2>&1; then
	setsid nohup "${restart[@]}" >>"$HOME/.pad/server.log" 2>&1 </dev/null &
else
	nohup "${restart[@]}" >>"$HOME/.pad/server.log" 2>&1 </dev/null &
fi
disown 2>/dev/null || true

# --- 6. Probe BOTH addresses before claiming anything -------------------------
#
# `--host 0.0.0.0` is a bind spec, not an address to curl, so the configured
# host resolves to the primary LAN address in that case. Loopback is always
# probed: it is the address the previous defect killed, and the one every
# local CLI call and browser tab uses.
configured_host="$(flag_value --host "${SERVER_ARGV[@]}" || true)"

# PORT resolution, in the server's own precedence order.
#
# Codex round 1: probing a fixed 7777 while faithfully restarting a server
# that was started with `--port 8080` fails a HEALTHY restart, and does it
# with this script's most confident message. The flag exists
# (cmd_server.go: `--port`, default 7777) so the case is reachable, and the
# whole point of this script is that the restart preserves the invocation —
# a probe that ignores half of that invocation is checking the wrong server.
configured_port="$(flag_value --port "${SERVER_ARGV[@]}" || true)"
# Each source is accepted only if it is NUMERIC, mirroring the application:
# config.go ignores a non-integer PAD_PORT and falls through to the next
# source. Without that, a mistyped or inherited PAD_PORT is probed
# literally and every refresh fails against a server that is running fine
# (codex round 7).
numeric() { case "$1" in '' | *[!0-9]*) return 1 ;; *) return 0 ;; esac; }

PORT=""
for candidate in "$configured_port" "${PAD_PORT:-}" "$(config_value port || true)"; do
	if numeric "$candidate"; then
		PORT="$candidate"
		break
	fi
	if [ -n "$candidate" ]; then
		note "ignoring non-numeric port value \"$candidate\" (as the server does)"
	fi
done
PORT="${PORT:-7777}"

# HOST likewise. When the argv carries no --host, fall back to PAD_HOST,
# which config.go reads for exactly this purpose.
#
# Precedence: flag, then environment, then the config file, then the
# default — the same order the server itself resolves them in. The config
# file is read because leaving it out meant a permanent false failure for
# anyone who configures the bind there (codex round 3).
if [ -z "$configured_host" ]; then
	configured_host="${PAD_HOST:-$(config_value host || true)}"
fi

# The probe set is exactly what the BIND PROMISES, no more and no less.
#
# A wildcard bind promises loopback AND the LAN address, and the original
# defect was precisely a wildcard server coming back bound to one host with
# 127.0.0.1 dead — so both are required there. An EXPLICIT single host
# promises only itself: demanding 127.0.0.1 from a server deliberately bound
# to ::1 or to a LAN interface would fail a correct configuration, which is
# the same false alarm in the opposite direction.
#
# The first version required 127.0.0.1 unconditionally, and the IPv6 test
# caught it: a server bound ::1 restarted correctly and was reported dead
# because a v4 loopback it never promised did not answer.
probe_hosts=()
case "$configured_host" in
"" | "127.0.0.1")
	probe_hosts=("127.0.0.1")
	;;
"localhost")
	# `localhost` may resolve to either loopback family, and which one the
	# server binds is not knowable from here (codex round 8). Requiring both
	# would fail a healthy v4-only or v6-only bind, so this is the one case
	# where ANY member answering is the pass — recorded because it is a
	# deliberate exception to "every probed address must answer".
	probe_any=1
	probe_hosts=("127.0.0.1" "::1")
	;;
"0.0.0.0")
	probe_hosts=("127.0.0.1")
	lan="$(hostname -I 2>/dev/null | awk '{print $1}')"
	if [ -z "$lan" ]; then
		lan="$(ipconfig getifaddr en0 2>/dev/null || true)"
	fi
	if [ -n "$lan" ]; then
		probe_hosts+=("$lan")
	else
		# FAIL CLOSED (codex rounds 5 and 8). A wildcard bind promises the
		# external address, and being unable to check it is not the same as
		# it being fine. The first version warned and exited 0, which is
		# this unit's own defect wearing a different hat: a success line
		# covering something that was not verified.
		die "cannot determine a LAN address to verify the wildcard bind (tried \`hostname -I\` and \`ipconfig getifaddr en0\`).
  The binary is installed and loopback answers, but the external half of
  \`--host 0.0.0.0\` is unverified and this refuses to claim otherwise.
  Start the server with an explicit --host, or set PAD_HOST, to make the
  address checkable."
	fi
	;;
"::")
	probe_hosts=("127.0.0.1" "::1")
	;;
*)
	probe_hosts=("$configured_host")
	;;
esac

probe() {
	local host="$1" deadline=$((SECONDS + PROBE_TIMEOUT))
	while [ $SECONDS -lt $deadline ]; do
		if curl -fsS -m 2 -o /dev/null "$(probe_url "$host" "$PORT")" 2>/dev/null; then
			return 0
		fi
		sleep 1
	done
	return 1
}

failed=()
answered=0
for host in "${probe_hosts[@]}"; do
	if probe "$host"; then
		answered=$((answered + 1))
	else
		failed+=("$host")
	fi
done

# Every probed address must answer, EXCEPT the `localhost` case above, where
# one loopback family answering is the whole of what can be required.
if [ "${probe_any:-0}" = "1" ] && [ "$answered" -gt 0 ]; then
	failed=()
	probe_hosts=("localhost")
fi

if [ ${#failed[@]} -gt 0 ]; then
	die "server did not answer on: ${failed[*]} (port $PORT, waited ${PROBE_TIMEOUT}s each).
  The binary is installed; the server is not serving those addresses.
  Restart argv was: ${restart[*]}"
fi

note "server restarted and answering on: ${probe_hosts[*]} (port $PORT)"
