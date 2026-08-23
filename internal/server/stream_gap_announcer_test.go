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

// The production cooldown is a real number with a stated reason (a delta-sync
// round trip), and every handler test overrides it — so nothing else would
// notice it being set to zero, which would disable the bound entirely on the
// only deployment that matters.
func TestProductionGapCooldownIsNotDisabled(t *testing.T) {
	var s Server
	if got := s.gapCooldown(); got != midStreamGapCooldown {
		t.Errorf("a server with no override must use the production cooldown, got %v", got)
	}
	if midStreamGapCooldown <= 0 {
		t.Fatal("the production cooldown is not positive; the rate limit is off")
	}
	if midStreamGapCooldown >= sseKeepaliveInterval {
		t.Errorf("cooldown %v is not shorter than the %v keepalive; a new hole would wait "+
			"longer than the connection's own heartbeat", midStreamGapCooldown, sseKeepaliveInterval)
	}

	s.midStreamGapCooldownOverride = 7 * time.Millisecond
	if got := s.gapCooldown(); got != 7*time.Millisecond {
		t.Errorf("the override was ignored: %v", got)
	}
}
