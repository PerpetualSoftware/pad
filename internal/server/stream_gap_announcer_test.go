package server

import (
	"testing"
	"time"
)

// The announcer answers codex round 4's P1: a slow subscriber answered with a
// delta sync can be made slower, drop more, and be answered again. The bound
// has to hold WITHOUT losing a gap, because dropping one would be the same
// dishonesty this fix exists to remove, one layer up.
//
// Both halves are asserted here, and each is the other's control: the second
// gap inside the window must NOT announce immediately, and it must NOT be
// forgotten.
func TestGapAnnouncerBoundsTheRateWithoutLosingAGap(t *testing.T) {
	g := newGapAnnouncer(40 * time.Millisecond)
	defer g.stop()

	if !g.observe() {
		t.Fatal("the first gap on a fresh connection must announce immediately")
	}
	if g.observe() {
		t.Fatal("a second gap inside the window announced immediately; the rate is unbounded")
	}
	if g.observe() {
		t.Fatal("a third gap inside the window announced immediately")
	}

	select {
	case <-g.cool():
	case <-time.After(2 * time.Second):
		t.Fatal("the cooldown never fired, so the latched gap would never be announced")
	}

	if !g.flush() {
		t.Fatal("the gaps that arrived inside the window were forgotten rather than latched")
	}

	// Latching reopens the window: the announcement just made is subject to
	// the same bound as any other.
	if g.observe() {
		t.Error("a gap immediately after a latched announcement was not bounded")
	}
}

// The other direction: a window that closes with nothing latched must not
// announce. Without this, the announcer would emit a spurious sync_required
// every cooldown for the life of any connection that ever saw one gap.
func TestGapAnnouncerDoesNotAnnounceAnEmptyWindow(t *testing.T) {
	g := newGapAnnouncer(20 * time.Millisecond)
	defer g.stop()

	if !g.observe() {
		t.Fatal("the first gap must announce")
	}

	select {
	case <-g.cool():
	case <-time.After(2 * time.Second):
		t.Fatal("the cooldown never fired")
	}
	if g.flush() {
		t.Error("a window that closed with no latched gap announced anyway")
	}

	// And the case is now off: cool() must not fire again on its own, or the
	// handler's select would spin.
	select {
	case <-g.cool():
		t.Error("the cooldown re-armed itself with nothing to announce")
	case <-time.After(60 * time.Millisecond):
	}

	// A gap arriving after a quiet window announces immediately — the bound
	// applies to bursts, not to a connection that has settled.
	if !g.observe() {
		t.Error("a gap after a quiet window was bounded; the window had already closed")
	}
}

// TestGapReadyToAnnounceTerminatesUnderContinuousRefill is the bound the
// handlers' barrier rests on, asserted where it can be.
//
// The first version of that barrier waited for an empty channel and its
// comment claimed that could not starve. It can — a publisher refilling faster
// than a slow client drains keeps the depth above zero indefinitely — and an
// end-to-end test does not reliably reproduce it, because under most
// schedulings the channel does briefly empty. Which is exactly why the bound
// is a predicate: the starvation case can be stated here even though it cannot
// be provoked there.
func TestGapReadyToAnnounceTerminatesUnderContinuousRefill(t *testing.T) {
	cases := []struct {
		name           string
		latched        bool
		queued, budget int
		want           bool
	}{
		{"no gap latched, nothing to announce", false, 0, 0, false},
		{"no gap latched, and a drained budget does not invent one", false, 5, 0, false},
		{"latched with an empty channel announces at once", true, 0, 0, true},
		{"latched with queued events waits for them", true, 5, 5, false},
		{"latched, partway through the queue, still waits", true, 3, 2, false},
		// The starvation case. The channel is STILL not empty — a publisher
		// has refilled it — but every event that predated the hole has gone
		// out, so the wait is over. An emptiness-only condition answers false
		// here, forever.
		{"latched, budget spent, channel refilled: announces anyway", true, 40, 0, true},
		{"latched, budget overspent", true, 1, -3, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := gapReadyToAnnounce(tc.latched, tc.queued, tc.budget); got != tc.want {
				t.Errorf("gapReadyToAnnounce(%v, %d, %d) = %v, want %v",
					tc.latched, tc.queued, tc.budget, got, tc.want)
			}
		})
	}
}
