package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/collections"
)

// This test lives in internal/store rather than beside DerivePrefix because
// parseItemRef is unexported and the point is to assert the two against EACH
// OTHER — collections generates the prefix, store decides whether a ref built
// from it resolves. Restating store's rule inside a collections test would let
// the two drift apart while both suites stayed green, which is exactly how
// BUG-2943 happened: two functions, each locally reasonable, disagreeing about
// what a prefix may contain.

// The other half of the claim, and the one that makes this a bug rather than a
// preference: what DerivePrefix produces must be what the resolver accepts.
// Asserted against the resolver itself rather than against a restatement of
// its rule, so the two cannot drift apart silently.
func TestDerivePrefix_OutputIsAcceptedByTheRefParser(t *testing.T) {
	for _, name := range []string{
		"TEMP Rook A 2870", "2026 Goals", "Sprint 42", "Tasks",
		"Customer Feedback", "Ünsicherheit", "v2 Roadmap",
	} {
		prefix := collections.DerivePrefix(name)
		if prefix == "" {
			continue // the caller substitutes ITEM; covered above
		}
		gotPrefix, gotNum, ok := parseItemRef(prefix + "-7")
		if !ok {
			t.Errorf("name %q derived prefix %q, and parseItemRef REFUSES %q-7 — "+
				"every item in that collection would be unresolvable by its own issue ID", name, prefix, prefix)
			continue
		}
		if gotPrefix != prefix || gotNum != 7 {
			t.Errorf("round trip changed the ref: %q-7 parsed as %q-%d", prefix, gotPrefix, gotNum)
		}
	}
}
