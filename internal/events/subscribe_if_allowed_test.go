package events

import "testing"

// TestSubscribeIfAllowedBounds covers the bus's own admission bounds
// directly (codex round 5).
//
// Neither branch had a package-level test. Both were exercised only
// through internal/server's HTTP tests — and since BUG-2726 moved the
// GLOBAL bound to a process-wide admission gate, the handler passes
// maxGlobal=0, so that branch is now exercised by nothing at all in
// production and would have been exercised by nothing in tests either.
// It remains part of this type's contract, so it is tested here rather
// than left to rot into a branch nobody can vouch for.
func TestSubscribeIfAllowedBounds(t *testing.T) {
	t.Parallel()

	t.Run("global bound", func(t *testing.T) {
		t.Parallel()
		b := New()
		defer b.Close()

		if _, ok := b.SubscribeIfAllowed("ws-1", 2, 0); !ok {
			t.Fatal("premise failed: first subscribe refused under a limit of 2")
		}
		// A DIFFERENT workspace, so a per-workspace bound could not be
		// what refuses the third.
		if _, ok := b.SubscribeIfAllowed("ws-2", 2, 0); !ok {
			t.Fatal("premise failed: second subscribe refused under a limit of 2")
		}
		if _, ok := b.SubscribeIfAllowed("ws-3", 2, 0); ok {
			t.Fatal("third subscribe admitted past a global limit of 2")
		}
	})

	t.Run("per-workspace bound", func(t *testing.T) {
		t.Parallel()
		b := New()
		defer b.Close()

		if _, ok := b.SubscribeIfAllowed("ws-1", 0, 1); !ok {
			t.Fatal("premise failed: first subscribe refused under a per-workspace limit of 1")
		}
		if _, ok := b.SubscribeIfAllowed("ws-1", 0, 1); ok {
			t.Fatal("second subscribe to the same workspace admitted past a limit of 1")
		}
		// Another workspace is unaffected — the property that makes this
		// a per-workspace bound rather than a global one wearing its name.
		if _, ok := b.SubscribeIfAllowed("ws-2", 0, 1); !ok {
			t.Fatal("a different workspace was refused by a per-workspace limit")
		}
	})

	t.Run("zero means unlimited", func(t *testing.T) {
		t.Parallel()
		b := New()
		defer b.Close()

		for i := 0; i < 200; i++ {
			if _, ok := b.SubscribeIfAllowed("ws-1", 0, 0); !ok {
				t.Fatalf("subscribe %d refused with both bounds at 0", i)
			}
		}
	})

	t.Run("unsubscribing frees a slot", func(t *testing.T) {
		t.Parallel()
		b := New()
		defer b.Close()

		ch, ok := b.SubscribeIfAllowed("ws-1", 1, 0)
		if !ok {
			t.Fatal("premise failed: first subscribe refused")
		}
		if _, ok := b.SubscribeIfAllowed("ws-2", 1, 0); ok {
			t.Fatal("premise failed: the budget was not actually spent")
		}
		b.Unsubscribe(ch)
		if _, ok := b.SubscribeIfAllowed("ws-2", 1, 0); !ok {
			t.Fatal("the slot was never freed by Unsubscribe")
		}
	})
}
