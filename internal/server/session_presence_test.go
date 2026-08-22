package server

import (
	"sync"
	"testing"
	"time"
)

func TestMemorySessionPresence_AddListRemove(t *testing.T) {
	t.Parallel()
	p := NewMemorySessionPresence()

	if got := mustList(t, p, "u1"); len(got) != 0 {
		t.Fatalf("expected no sessions for an unknown user, got %d", len(got))
	}

	a := p.Add("u1", SessionIdentity{Label: "docapp"})
	b := p.Add("u1", SessionIdentity{Label: "voiapp"})
	other := p.Add("u2", SessionIdentity{Label: "poker"})

	if a == b {
		t.Fatal("two connections for the same user must get distinct ids")
	}

	got := mustList(t, p, "u1")
	if len(got) != 2 {
		t.Fatalf("expected 2 sessions for u1, got %d", len(got))
	}
	// u2's session must not leak into u1's list — the whole endpoint is
	// self-scoped, so cross-user bleed here is the bug that matters.
	for _, s := range got {
		if s.ID == other {
			t.Fatal("u2's session appeared in u1's list")
		}
		if s.ConnectedAt.IsZero() {
			t.Fatalf("session %s has a zero ConnectedAt", s.ID)
		}
	}

	p.Remove("u1", a)
	got = mustList(t, p, "u1")
	if len(got) != 1 || got[0].ID != b {
		t.Fatalf("expected only session %s to remain, got %+v", b, got)
	}
	if len(mustList(t, p, "u2")) != 1 {
		t.Fatal("removing u1's session disturbed u2's list")
	}
}

// TestMemorySessionPresence_RemoveIsForgiving pins the contract the
// stream handler's defer depends on: Remove runs on exit paths where
// Add may never have happened, and must not panic or corrupt state.
func TestMemorySessionPresence_RemoveIsForgiving(t *testing.T) {
	t.Parallel()
	p := NewMemorySessionPresence()

	p.Remove("nobody", "no-such-session") // unknown user
	p.Remove("u1", "")                    // empty id (Add never ran)

	id := p.Add("u1", SessionIdentity{})
	p.Remove("u1", id)
	p.Remove("u1", id) // double-remove: idempotent, not a panic

	if got := mustList(t, p, "u1"); len(got) != 0 {
		t.Fatalf("expected u1 to have no sessions, got %d", len(got))
	}
}

// TestMemorySessionPresence_ListIsOldestFirstAndStable covers the
// ordering the S5 target picker relies on. Two sessions can share a
// ConnectedAt under a coarse clock, so the id tiebreaker — not the map
// iteration order — has to decide, or the rendered list jumps around
// under the user's cursor between polls.
func TestMemorySessionPresence_ListIsOldestFirstAndStable(t *testing.T) {
	t.Parallel()
	p := NewMemorySessionPresence()

	now := time.Now().UTC()
	p.mu.Lock()
	p.byUser["u1"] = map[string]LiveSession{
		"ccc": {ID: "ccc", ConnectedAt: now},
		"aaa": {ID: "aaa", ConnectedAt: now},
		"bbb": {ID: "bbb", ConnectedAt: now.Add(-time.Minute)},
	}
	p.mu.Unlock()

	// Same input, repeated reads: the order must not depend on Go's
	// randomized map iteration.
	for i := 0; i < 20; i++ {
		got := mustList(t, p, "u1")
		if len(got) != 3 {
			t.Fatalf("expected 3 sessions, got %d", len(got))
		}
		if got[0].ID != "bbb" {
			t.Fatalf("expected the oldest connection first, got %q", got[0].ID)
		}
		if got[1].ID != "aaa" || got[2].ID != "ccc" {
			t.Fatalf("expected the id tiebreaker to order the same-timestamp pair aaa,ccc; got %q,%q", got[1].ID, got[2].ID)
		}
	}
}

// TestMemorySessionPresence_ListReturnsACopy pins the doc-comment
// promise that a caller may retain the returned slice: mutating it must
// not reach back into the registry.
func TestMemorySessionPresence_ListReturnsACopy(t *testing.T) {
	t.Parallel()
	p := NewMemorySessionPresence()
	id := p.Add("u1", SessionIdentity{Label: "docapp"})

	got := mustList(t, p, "u1")
	got[0].Label = "tampered"

	fresh := mustList(t, p, "u1")
	if fresh[0].Label != "docapp" {
		t.Fatalf("mutating the returned slice changed the registry: label is now %q", fresh[0].Label)
	}
	if fresh[0].ID != id {
		t.Fatalf("expected id %q, got %q", id, fresh[0].ID)
	}
}

// TestMemorySessionPresence_ArmedRoundTrips is PLAN-2613 S1's wiring
// check for the registry layer: SessionIdentity.Armed must survive
// Add/ListForUser unchanged, and the default (unset) shape must be
// false — the legacy/unarmed session a pre-S1 client produces.
func TestMemorySessionPresence_ArmedRoundTrips(t *testing.T) {
	t.Parallel()
	p := NewMemorySessionPresence()

	armedID := p.Add("u1", SessionIdentity{Label: "docapp", Armed: true})
	unarmedID := p.Add("u1", SessionIdentity{Label: "voiapp"}) // Armed left at its zero value

	got := mustList(t, p, "u1")
	if len(got) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(got))
	}
	byID := map[string]LiveSession{}
	for _, s := range got {
		byID[s.ID] = s
	}
	if !byID[armedID].Armed {
		t.Fatalf("expected the armed session to round-trip as armed=true, got %+v", byID[armedID])
	}
	if byID[unarmedID].Armed {
		t.Fatalf("expected the session with no Armed declaration to round-trip as armed=false, got %+v", byID[unarmedID])
	}
}

// TestMemorySessionPresence_ConcurrentAccess is the race-detector's
// entry point for the connect/disconnect-vs-read pattern the stream
// handler produces in production.
func TestMemorySessionPresence_ConcurrentAccess(t *testing.T) {
	t.Parallel()
	p := NewMemorySessionPresence()

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				id := p.Add("u1", SessionIdentity{})
				mustList(t, p, "u1")
				p.Remove("u1", id)
			}
		}()
	}
	wg.Wait()

	if got := mustList(t, p, "u1"); len(got) != 0 {
		t.Fatalf("every Add was paired with a Remove, expected 0 sessions, got %d", len(got))
	}
	// The user's bucket should be reclaimed too, not left as an empty map.
	p.mu.RLock()
	_, stillThere := p.byUser["u1"]
	p.mu.RUnlock()
	if stillThere {
		t.Fatal("expected the empty user bucket to be reclaimed")
	}
}

// mustList reads a registry's session list, failing the test if the read
// itself failed.
//
// ListForUser gained an error when an out-of-process implementation made
// "I could not find out" a reachable state distinct from "there are none"
// (BUG-2698, codex round 1 P1). Going through this helper makes every
// existing test assert its own premise — that the read SUCCEEDED — rather
// than comparing lengths against a nil slice that an error would also
// produce.
func mustList(t *testing.T, p SessionPresence, userID string) []LiveSession {
	t.Helper()
	sessions, err := p.ListForUser(userID)
	if err != nil {
		t.Fatalf("ListForUser(%q): %v", userID, err)
	}
	return sessions
}
