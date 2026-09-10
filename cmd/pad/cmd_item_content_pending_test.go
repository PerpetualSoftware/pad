package main

import (
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestWarnContentPendingFlush pins the gating (fires for the outcome the server
// sends, silent for everything else) and the wording contract.
//
// What it does NOT guard, stated so nobody reads more into it: the value cannot
// drift between the server that writes it and this renderer, because both take it
// from models.ContentOutcomeAppliedPendingFlush. That risk was closed by sharing the
// constant, not by this test — an earlier draft of this change had the literal
// spelled out in both packages, where a rename on one side would have left the
// warning silently never firing (BUG-2995, codex round 2).
func TestWarnContentPendingFlush(t *testing.T) {
	cases := []struct {
		name string
		item *models.Item
		want bool
	}{
		{"nil item", nil, false},
		{"no warnings", &models.Item{}, false},
		{"warnings but no content outcome", &models.Item{
			Warnings: &models.ItemWriteWarnings{UndeclaredFields: []string{"whatever"}},
		}, false},
		{"some other outcome", &models.Item{
			Warnings: &models.ItemWriteWarnings{ContentOutcome: "not_applied"},
		}, false},
		{"the outcome the server sends", &models.Item{
			Warnings: &models.ItemWriteWarnings{ContentOutcome: models.ContentOutcomeAppliedPendingFlush},
		}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := captureStderr(t, func() { warnContentPendingFlush(tc.item) })
			if got := out != ""; got != tc.want {
				t.Fatalf("warned=%v want=%v (stderr: %q)", got, tc.want, out)
			}
			if !tc.want {
				return
			}
			// The line has to say the write LANDED and the read may not show it yet.
			// A warning that only said "pending" would read as a failure and send a
			// caller into a re-send loop, which is the behaviour BUG-2995 exists to
			// stop.
			for _, want := range []string{"applied", "collaborative document", "what you sent", "previous content"} {
				if !strings.Contains(out, want) {
					t.Errorf("warning does not mention %q: %s", want, out)
				}
			}
			// No duration claim: the flush is not established to always land (BUG-3000).
			for _, forbidden := range []string{"second", "5s", "shortly", "momentarily"} {
				if strings.Contains(strings.ToLower(out), forbidden) {
					t.Errorf("warning claims a duration (%q), which nothing measured supports: %s", forbidden, out)
				}
			}
		})
	}
}
