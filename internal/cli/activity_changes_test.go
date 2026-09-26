package cli

import (
	"encoding/json"
	"os"
	"testing"
)

// TestFormatChangesForDisplayMatchesTheJSFixture asserts the corpus the web
// suite asserts (BUG-2789): the two implementations agree because both agree
// with one file, not because one was read beside the other.
func TestFormatChangesForDisplayMatchesTheJSFixture(t *testing.T) {
	raw, err := os.ReadFile("../../web/src/lib/utils/activityChanges.fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Cases []struct{ Name, Input, Display string }
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	if len(fx.Cases) < 15 {
		t.Fatalf("fixture has %d cases; the corpus is missing", len(fx.Cases))
	}
	for _, c := range fx.Cases {
		if got := FormatChangesForDisplay(c.Input); got != c.Display {
			t.Errorf("%s: got %q, want %q", c.Name, got, c.Display)
		}
	}
}
