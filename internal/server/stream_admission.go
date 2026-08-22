package server

import "sync"

// streamAdmission bounds how many long-held streaming connections this
// process serves, globally and per user (BUG-2726).
//
// WHY IT IS NOT ON THE BUSES. Pad has two SSE endpoints, backed by two
// different buses: /api/v1/events (workspace-scoped, events.EventBus) and
// /api/v1/events/stream (user-scoped, watchevents.Bus). Each bus can bound
// its OWN subscribers atomically, and events.EventBus already does. Neither
// can bound the two together, because a shared global budget is a property
// of the PROCESS — the goroutine, the socket, the subscription and (for
// the watch stream) the presence registration cost the same whichever
// endpoint opened them. Putting the global on one bus would have let a
// user exhaust the machine through the other one while every configured
// limit still read as satisfied.
//
// So the global bound moves HERE and is passed to the events bus as 0
// (unlimited) — see handleSSE. The per-WORKSPACE bound stays on that bus,
// because it is genuinely workspace-scoped and the watch stream has no
// coherent workspace to count against.
//
// COMPATIBILITY, ruled rather than assumed: PAD_SSE_MAX_CONNECTIONS now
// covers both endpoints, so an operator who tuned it for one is now
// bounding both and may reach the limit sooner. That re-point is
// deliberate — a knob that silently bounded half the connections it named
// is worse than one that bounds more than you expected, because the first
// failure mode is invisible and the second announces itself and is
// tunable. The startup log line reports the effective limits so the change
// is visible without reading release notes.
//
// TOCTOU: acquire takes the lock, checks and RESERVES in one critical
// section, exactly as events.EventBus.SubscribeIfAllowed does. Two
// concurrent requests cannot both pass a check for the last slot.
type streamAdmission struct {
	mu       sync.Mutex
	total    int
	perUser  map[string]int
	maxTotal int
	maxUser  int

	// onTotal, when non-nil, is called with the new total after every
	// admit and release. It drives pad_stream_connections_active without
	// this type importing the metrics package — and it is a CALLBACK
	// rather than a scrape-time collector because the gate's lock is on
	// the connection path and a scraper should never contend for it.
	onTotal func(total int)
}

func newStreamAdmission(maxTotal, maxUser int) *streamAdmission {
	return &streamAdmission{
		perUser:  map[string]int{},
		maxTotal: maxTotal,
		maxUser:  maxUser,
	}
}

// setTotalObserver attaches the gauge callback. Config-time only, like
// the limits themselves.
func (a *streamAdmission) setTotalObserver(fn func(total int)) {
	a.mu.Lock()
	a.onTotal = fn
	a.mu.Unlock()
}

// notifyTotal reports the current total. Called with a.mu HELD, and it
// reads a.total under that lock, so the value it reports is the one that
// was true at the moment of the change rather than whatever a later
// racing update leaves behind.
func (a *streamAdmission) notifyTotal() {
	if a.onTotal != nil {
		a.onTotal(a.total)
	}
}

// admissionRefusal names which bound refused a connection, for the log
// line and the error message. Bounded values, safe to expose.
type admissionRefusal string

const (
	admissionRefusalNone   admissionRefusal = ""
	admissionRefusalGlobal admissionRefusal = "global"
	admissionRefusalUser   admissionRefusal = "per_user"
)

// acquire reserves one stream slot. The returned release MUST be called
// when the connection ends; it is idempotent so a handler can defer it
// unconditionally.
//
// userID may be empty — the workspace-scoped stream admits legacy
// workspace-token and fresh-install callers with no resolved user. Those
// count toward the global bound but not toward any per-user one, since
// attributing them all to a single empty-string bucket would make
// unrelated anonymous callers evict each other.
func (a *streamAdmission) acquire(userID string) (release func(), refusal admissionRefusal) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.maxTotal > 0 && a.total >= a.maxTotal {
		return nil, admissionRefusalGlobal
	}
	if a.maxUser > 0 && userID != "" && a.perUser[userID] >= a.maxUser {
		return nil, admissionRefusalUser
	}

	a.total++
	if userID != "" {
		a.perUser[userID]++
	}
	a.notifyTotal()

	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			a.total--
			if userID != "" {
				a.perUser[userID]--
				// Delete at zero so the map does not grow without bound
				// on a deployment where many users connect once. Leaving
				// zero entries behind would make this a slow leak keyed
				// by user id.
				if a.perUser[userID] <= 0 {
					delete(a.perUser, userID)
				}
			}
			a.notifyTotal()
		})
	}, admissionRefusalNone
}

// counts returns the current global total and this user's count, for log
// lines. Snapshot only — never use it to decide admission, which is
// acquire's job under one lock.
func (a *streamAdmission) counts(userID string) (total, user int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.total, a.perUser[userID]
}
