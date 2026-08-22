// Package idspace mints the ID base that identifies one INCARNATION of an
// in-process event counter.
//
// It exists because two packages need the same guarantee — internal/events and
// internal/watchevents each run an in-memory bus that assigns Last-Event-ID
// values from a process-local counter — and because the guarantee is a subtle
// one that must not drift between two copies. The alternative was a twin
// helper in each package, which is how a CAS invariant ends up correct in one
// place and quietly wrong in the other.
//
// It deliberately does NOT cover the Redis-backed buses. Those share a counter
// ACROSS processes, so their id space is identified by a shared epoch key
// rather than by anything a single process can compute — see
// internal/events/redis_bus.go, which explains why the two mechanisms differ
// and must not be symmetrized into one.
package idspace

import (
	"sync/atomic"
	"time"
)

// Shift is how far a process's start time is shifted left to form the base its
// event IDs count up from. It is the ID SPACE'S IDENTITY, carried in the ID's
// VALUE rather than in the SSE wire's FORMAT — which is the whole trick,
// because the format has no room for it (BUG-2736).
//
// WHY THIS EXISTS. An in-memory bus assigns IDs from a process-local counter.
// Before this, that counter restarted at 1 on every process start, so a client
// holding cursor 2 from a previous incarnation could reconnect to a restarted
// server, pass every coverage check, and be replayed the NEW space's 3, 4, 5
// as though they followed the OLD space's 2 — silently missing everything the
// dead space held above 2. Nothing local could tell the two 2s apart: the
// cursor carries no epoch, and (in internal/events) per-workspace IDs are
// non-consecutive by construction, so "did we issue this ID?" is numerically
// undecidable. BUG-2736 records the other four levers and why each is
// unavailable.
//
// WHAT THE SHIFT BUYS, stated as an invariant rather than a hope: an ID from
// incarnation N can equal one from incarnation N+1 only if the earlier process
// published more than 2^20 (1048576) events per millisecond of its own
// lifetime, because the next start's base is higher by (elapsed ms << Shift).
// That is a deterministic bound, not a probability — which is why this is not
// the "start from a random high number" BUG-2736's body rejects.
//
// IT RESTS ON ONE UNSTATED ASSUMPTION, so here it is stated (codex round 8):
// that a RESTART TAKES AT LEAST ONE MILLISECOND. The bases are separated by
// the clock, at millisecond resolution, and the CAS below separates only buses
// built inside ONE process — a second process starting inside the same
// millisecond as the first would take the same base and reissue its IDs, and a
// cursor from the dead run would then be accepted as live.
//
// The assumption is not a hope either: reaching the constructor means the OS
// reaped the old process and the new one bound its listener, opened its
// database and ran migrations. That is milliseconds at minimum, and is
// observable — a deployment where it were not true would have to be restarting
// faster than it can open a file. Closing the gap for real would need
// persistence, which BUG-2736's body rules out for a separate and stronger
// reason (durable event-bus state in single-process Pad, still lost on any
// data loss). So it is accepted, and named here rather than left for the next
// reader to discover in an incident.
//
// THE BACKWARDS-CLOCK CASE degrades in the SAFE direction, and that is why a
// wall clock is acceptable as the only available monotonic-across-restart
// source. A clock step backwards yields a LOWER base, so a previous
// incarnation's cursor sits ABOVE the new buffer's newest ID and the replay
// buffer refuses it on its foreign-ID check: the client is told sync_required
// rather than handed a wrong replay. It degrades to the old silent skip only
// if the new incarnation then climbs past that cursor, which needs the step
// back to exceed the process's own lifetime AND the new incarnation to
// out-publish the gap.
//
// OVERFLOW BOUND, computed rather than estimated: a base is startMillis<<20,
// so the last start instant that still fits in an int64 is millisecond
// 8796093022207 — 2248-09-26T15:10:22Z. Past that the shift overflows into the
// sign bit. Today's base is ~1.87e18, which is already past JavaScript's
// MAX_SAFE_INTEGER (9007199254740991) — safe only because no client numbers
// the ID: EventSource owns Last-Event-ID internally, and cmd/pad/cmd_watch.go
// carries `id:` as an opaque string. Anything that starts parsing an event ID
// as a JS number breaks here, which is why web's ItemEvent no longer declares
// one.
const Shift = 20

// last is the highest base New has handed out in THIS process, across every
// bus that uses this package. It exists because the wall clock alone does not
// separate two buses constructed inside the same millisecond — which the clock
// reasoning above quietly assumed away, and which a test constructing two
// buses back to back reproduces every time. A process restart takes far longer
// than a millisecond, so this is not the production case; it is the case that
// makes the invariant true unconditionally instead of true-in-practice, and it
// costs one CAS.
var last atomic.Int64

// New returns the ID base for a bus incarnation, STRICTLY GREATER than any
// base this process has already issued.
//
// The clock supplies cross-restart separation (a later start means a higher
// base); the CAS loop supplies within-process separation, including when the
// clock repeats a millisecond or steps backwards while the process is alive.
// When the clock cannot advance the base, the next one is bumped by a full
// stride — one millisecond's worth of ID space — so the 2^20-per-ms invariant
// above holds for these bases exactly as it does for clock-derived ones.
func New() int64 {
	candidate := time.Now().UnixMilli() << Shift
	for {
		prev := last.Load()
		if candidate <= prev {
			candidate = prev + (1 << Shift)
		}
		if last.CompareAndSwap(prev, candidate) {
			return candidate
		}
	}
}
