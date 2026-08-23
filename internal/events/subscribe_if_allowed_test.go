package events

import "testing"

// TestSubscribeIfAllowedBounds covers the bus's per-workspace admission
// bound directly (codex round 5).
//
// It had no package-level test — it was exercised only through
// internal/server's HTTP tests, which since BUG-2726 also route through
// an admission gate and so no longer discriminate this bound on their
// own. The GLOBAL bound this method used to take was removed in that
// same change; internal/server's streamAdmission owns it, because it is a
// property of the process rather than of either bus.
func TestSubscribeIfAllowedBounds(t *testing.T) {
	t.Parallel()

	t.Run("per-workspace bound", func(t *testing.T) {
		t.Parallel()
		b := New()
		defer b.Close()

		if _, _, ok := b.SubscribeIfAllowed("ws-1", 1); !ok {
			t.Fatal("premise failed: first subscribe refused under a per-workspace limit of 1")
		}
		if _, _, ok := b.SubscribeIfAllowed("ws-1", 1); ok {
			t.Fatal("second subscribe to the same workspace admitted past a limit of 1")
		}
		// Another workspace is unaffected — the property that makes this
		// a per-workspace bound rather than a global one wearing its name.
		if _, _, ok := b.SubscribeIfAllowed("ws-2", 1); !ok {
			t.Fatal("a different workspace was refused by a per-workspace limit")
		}
	})

	t.Run("zero means unlimited", func(t *testing.T) {
		t.Parallel()
		b := New()
		defer b.Close()

		for i := 0; i < 200; i++ {
			if _, _, ok := b.SubscribeIfAllowed("ws-1", 0); !ok {
				t.Fatalf("subscribe %d refused with the bound at 0", i)
			}
		}
	})

	t.Run("unsubscribing frees a slot", func(t *testing.T) {
		t.Parallel()
		b := New()
		defer b.Close()

		ch, _, ok := b.SubscribeIfAllowed("ws-1", 1)
		if !ok {
			t.Fatal("premise failed: first subscribe refused")
		}
		if _, _, ok := b.SubscribeIfAllowed("ws-1", 1); ok {
			t.Fatal("premise failed: the budget was not actually spent")
		}
		b.Unsubscribe(ch)
		if _, _, ok := b.SubscribeIfAllowed("ws-1", 1); !ok {
			t.Fatal("the slot was never freed by Unsubscribe")
		}
	})
}
