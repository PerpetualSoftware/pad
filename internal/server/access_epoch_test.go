package server

import "testing"

// The properties the two item doors depend on. Each case names what a
// FAILURE would mean for the client, because that is what decides whether
// the property is worth a test (CONVE-12: assert what the wrong behaviour
// would do, not what the right behaviour leaves looking unchanged).
func TestComputeAccessEpoch(t *testing.T) {
	t.Run("stable across input ordering", func(t *testing.T) {
		// A different row order out of the store must not read as a changed
		// access set. If it did, every poll would trigger a full authoritative
		// resync — strictly worse than the defect this closes.
		a := computeAccessEpoch([]string{"c1", "c2", "c3"}, []string{"i2", "i1"})
		b := computeAccessEpoch([]string{"c3", "c1", "c2"}, []string{"i1", "i2"})
		if a != b {
			t.Errorf("epoch flapped on ordering alone: %q vs %q", a, b)
		}
	})

	t.Run("does not reorder its inputs", func(t *testing.T) {
		// The callers pass the same slices to the query that produced them.
		// Sorting in place would be an invisible side effect of computing a
		// fingerprint, and the door would then query a differently-ordered set.
		ids := []string{"c3", "c1", "c2"}
		computeAccessEpoch(ids, nil)
		if ids[0] != "c3" || ids[1] != "c1" || ids[2] != "c2" {
			t.Errorf("input slice was reordered: %v", ids)
		}
	})

	t.Run("unrestricted is distinct from empty", func(t *testing.T) {
		// nil = no filtering (owner). Empty = a restricted member who can see
		// nothing. Collapsing them would make a revocation-to-nothing carry an
		// owner's epoch, which is the exact silence this whole change removes.
		if computeAccessEpoch(nil, nil) == computeAccessEpoch([]string{}, nil) {
			t.Error("unrestricted and empty-restricted share an epoch")
		}
	})

	t.Run("narrowing changes the epoch", func(t *testing.T) {
		before := computeAccessEpoch([]string{"c1", "c2"}, nil)
		after := computeAccessEpoch([]string{"c1"}, nil)
		if before == after {
			t.Error("dropping a collection left the epoch unchanged")
		}
	})

	t.Run("item grants are part of the set", func(t *testing.T) {
		// A guest keeps collection-level visibility while an item grant is
		// revoked. Hashing only the collections would miss it entirely.
		before := computeAccessEpoch([]string{"c1"}, []string{"i1", "i2"})
		after := computeAccessEpoch([]string{"c1"}, []string{"i1"})
		if before == after {
			t.Error("revoking an item grant left the epoch unchanged")
		}
	})

	t.Run("the two lists cannot be confused", func(t *testing.T) {
		// Without a separator, ["a","b"]+[] and ["a"]+["b"] hash the same, so
		// moving a resource between the collection and item dimensions would
		// be invisible. The value is opaque, so nothing downstream would catch
		// this — only here.
		if computeAccessEpoch([]string{"a", "b"}, nil) == computeAccessEpoch([]string{"a"}, []string{"b"}) {
			t.Error("collection and item lists collide")
		}
	})
}

// TestEffectiveAccessEpoch pins the two rules effectiveAccessEpoch adds over
// computeAccessEpoch (BUG-3347): with item grants the collection half is the
// full-access set, and a nil full-access set is still a FILTERED caller, never
// the unrestricted sentinel.
func TestEffectiveAccessEpoch(t *testing.T) {
	nav := []string{"c-kept", "c-deleted"}
	full := []string{"c-kept"}
	grants := []string{"i-1"}

	if got, want := effectiveAccessEpoch(nav, full, grants), computeAccessEpoch(full, grants); got != want {
		t.Errorf("with item grants the epoch must hash the full-access set: got %q, want %q", got, want)
	}
	if got, want := effectiveAccessEpoch(nav, full, nil), computeAccessEpoch(nav, nil); got != want {
		t.Errorf("without item grants the epoch must hash the nav set: got %q, want %q", got, want)
	}
	if got := effectiveAccessEpoch(nav, nil, grants); got == accessEpochUnrestricted {
		t.Errorf("a caller with item grants and no full-access collections read as unrestricted")
	}
	if got, want := effectiveAccessEpoch(nil, nil, nil), accessEpochUnrestricted; got != want {
		t.Errorf("unrestricted caller: got %q, want %q", got, want)
	}
}
