#!/bin/sh
# pad-monitor.sh — the gated push/watch stream wrapper (PLAN-2613 S3).
#
# Two callers run THIS script: the plugin mod (plugin/hooks/register.js),
# which starts it at session start only when `pad session should-arm`
# already said yes (TASK-3513: an unarmed session runs and shows nothing),
# and the on-skill-invoke:connect monitor, the explicit /pad:connect path
# and the path where mods do not load. It is the mechanism behind D1's rule
# — "no consent → no monitor process, no stream, nothing listening":
#
#   1. Consent gate. `pad session should-arm` decides whether this session
#      has consented right now (a live `pad session arm`, or auto_arm with
#      no explicit disarm). If not, this script exits immediately. The mod
#      checks first, so for it this is the re-check; for the connect
#      monitor it is the gate.
#
#   2. Dedupe. When both callers start it for one armed session, a
#      per-session lockfile lets only one hold the stream; the loser exits
#      0. The lock is keyed on CLAUDE_CODE_MESSAGING_SOCKET, which the mod's
#      processes inherit exactly as monitors do (measured, TASK-3513), so
#      both callers of the SAME session share it while different sessions
#      never collide. A cwd fallback covers a caller without it rather than
#      keying globally.
#
#   3. Resilience. Dead monitors never resurrect, so the reconnect loop
#      lives HERE: it re-streams with backoff and re-checks consent each
#      time, so a within-session `pad session disarm` stops the stream on
#      its next reconnect. It exits only when consent is withdrawn or the
#      lock can't be held.
#
# Silent by construction (DOC-2479): it prints nothing on its own; only the
# monitored stream emits lines, and only when there is something to say.

set -u

# --- 0. pad must be installed. Silently wait rather than error: a session
# may install pad after start, and a noisy failure would spam the panel.
while ! pad watch --help >/dev/null 2>&1; do
	sleep 3600
done

# --- 0.5. Presence. Record this session in the local session registry
# (~/.pad/sessions, TASK-2767) BEFORE the consent gate: presence is a fact
# about the session, consent is a grant it may or may not make, and the
# record is local, 0600, and never crosses the wire — so an unarmed session
# still registers. Both monitors of one session run this; the second is an
# idempotent overwrite. The record is keyed on the harness session pid
# (PAD_SESSION_PID if exported, else Claude Code's CLAUDE_PID) and named by
# the agent name (.pad.toml agent_name, else PAD_AGENT, else the detected
# runtime); `pad session list` reads it back.
# Silent by construction — a failure here must not cost the stream. No
# test harness drives this script; the step is verified by hand with
# `pad session list` after a session starts (TASK-2767).
pad session register >/dev/null 2>&1 || true

# --- 1. Session key for the lockfile.
sock="${CLAUDE_CODE_MESSAGING_SOCKET:-}"
if [ -n "$sock" ]; then
	key="sess-$(printf '%s' "$sock" | cksum | cut -d' ' -f1)"
else
	key="repo-$(printf '%s' "$(pwd)" | cksum | cut -d' ' -f1)"
fi
lock="${TMPDIR:-/tmp}/pad-monitor-${key}.lock"

# acquire_lock: returns 0 if we now hold the lock, 1 if a LIVE peer holds
# it (we should exit). A stale lock left by a crashed monitor (holder pid
# gone) is stolen — mirroring the arm-state file's liveness rule so a dead
# owner can never block a live one.
acquire_lock() {
	while :; do
		if mkdir "$lock" 2>/dev/null; then
			echo $$ >"$lock/pid"
			return 0
		fi
		holder=$(cat "$lock/pid" 2>/dev/null || true)
		if [ -z "$holder" ]; then
			# The lock dir exists but the pid is not published yet — a peer
			# is mid-startup (the window between its mkdir and its pid
			# write). Treat it as live and exit: conservative dedupe means
			# when in doubt the second monitor stays out, which is the safe
			# direction (at worst one session doesn't stream; it never
			# double-streams). Avoids the steal race where two monitors both
			# see an empty pid and both proceed.
			return 1
		fi
		if kill -0 "$holder" 2>/dev/null; then
			return 1 # a live peer monitor owns the stream
		fi
		# A published-but-dead holder: a crashed monitor. Steal and retry.
		# (kill -0 can't tell a reused pid from the original, but a lock is
		# dedupe not consent — a false "live" only costs one non-streaming
		# session, never an unconsented stream.)
		rm -rf "$lock" 2>/dev/null || true
	done
}

if ! acquire_lock; then
	exit 0
fi
# Release the lock on any exit so a clean session end frees it promptly.
# INT/TERM must EXIT (a trap handler otherwise resumes the loop, which
# would keep reconnecting without a lock while a new monitor starts); the
# exit trap (condition 0, the portable spelling of EXIT — some strict
# /bin/sh reject the EXIT name) then does the cleanup on the way out.
# The stream runs as a child the traps can name: a caller that stops this
# script (the mod at session end) must stop the stream too, and a signal to
# this shell does not reach a foreground child of its own. `pad watch` is
# one process and starts none of its own, so the child is the whole tree.
child=
trap 'rm -rf "$lock" 2>/dev/null' 0
trap '[ -n "$child" ] && kill "$child" 2>/dev/null; exit 130' INT TERM

# --- 2/3. Gate + reconnect loop. Re-check consent before every stream
# attempt so an in-session disarm ends the loop on the next reconnect.
while pad session should-arm >/dev/null 2>&1; do
	pad watch --stream --for-session &
	child=$!
	wait "$child"
	child=
	# The stream returned (server closed it, padd restart, network blip).
	# Brief backoff, then re-check consent and reconnect. A withdrawn
	# consent falls out of the while-condition and the monitor exits.
	sleep 5
done
