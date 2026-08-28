package links

import (
	"strings"
	"testing"
	"time"
)

// ReplaceTitle used to re-scan its own output: it looped "find old in result,
// splice new in" until no match remained, so the text it had just inserted was
// searched again. When the NEW title contains the OLD link token that never
// terminates and the string grows without bound.
//
// The caller runs inside the rename transaction, so a hang here does not merely
// wedge one request — it holds that transaction open while exhausting memory.
// On Postgres it also holds the workspace rename advisory lock (BUG-2778) and
// blocks every other rename in the workspace; on SQLite that lock is a no-op
// and the transaction's own BEGIN IMMEDIATE write lock does the equivalent
// damage.
//
// Found by Codex round 2 on BUG-2785, while enumerating the ways the cascade's
// retry loop could fail to terminate.
func TestReplaceTitle_TerminatesWhenNewTitleEmbedsOldToken(t *testing.T) {
	// `[[A]]` -> `[[A]] [[A]]`, whose output still contains `[[A]]`. Against the
	// old implementation this grew until it was killed; a probe ran 3s without
	// finishing.
	cases := []struct{ name, old, new string }{
		{"new title re-embeds the whole old token", "A", "A]] [[A"},
		{"new title re-embeds it twice", "A", "A]] [[A]] [[A"},
		{"old token embedded mid-title", "X", "pre A]] [[X]] [[post"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan string, 1)
			go func() { done <- ReplaceTitle("x [["+tc.old+"]] y", tc.old, tc.new) }()

			select {
			case got := <-done:
				// The property under test is TERMINATION, and specifically that
				// the substitution happens once per occurrence in the INPUT.
				//
				// Asserting the exact output would pin escape semantics this
				// codebase does not have: the document API accepts these titles
				// (handlers_documents.go validates doc_type and status, never the
				// title), and ReplaceTitle emits them raw, so renaming to
				// `A]] [[A` yields `[[A]] [[A]]` — two links to nothing rather
				// than one link to the new title. That is a REAL second defect,
				// filed as BUG-2796; it is not what this test is about, and
				// pinning the output here would freeze the broken rendering as
				// though it were intended.
				if n := strings.Count(got, "[["+tc.new+"]]"); n != 1 {
					t.Errorf("substituted %d times, want exactly 1 (one occurrence in the input)\n got: %q", n, got)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("ReplaceTitle did not terminate: the replacement re-scanned its own output, " +
					"which grows without bound when the new title contains the old link token")
			}
		})
	}
}

// The ordinary path has to keep working, and this is the control that would
// catch a "fix" that terminated by doing nothing.
func TestReplaceTitle_StillRewritesEveryOccurrence(t *testing.T) {
	got := ReplaceTitle("[[Old]] middle [[Old]] end", "Old", "New")
	if want := "[[New]] middle [[New]] end"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if strings.Contains(got, "[[Old]]") {
		t.Errorf("an occurrence survived: %q", got)
	}
}

// TestReplaceTitleN_MatchesReplaceTitleWhenGivenTheTrueCount pins the
// obligation ReplaceTitleN puts on its caller: given the real occurrence
// count, it must produce exactly what ReplaceTitle produces.
//
// The under-count leg is the counterfactual, and it is why the two are
// separate functions rather than one with an optional parameter — passing a
// number that is too small does not error, it silently leaves later
// occurrences unrewritten, which on the rename path means links left pointing
// at a title that no longer exists.
func TestReplaceTitleN_MatchesReplaceTitleWhenGivenTheTrueCount(t *testing.T) {
	for _, tc := range []struct{ name, content, old, new string }{
		{"several occurrences", "a [[Old]] b [[Old]] c [[Old]] d", "Old", "New"},
		{"none", "nothing to see here", "Old", "New"},
		{"new embeds old", "x [[A]] y", "A", "A]] [[A"},
		{"shrinking", "[[LongOldTitle]] and [[LongOldTitle]]", "LongOldTitle", "n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := ReplaceTitle(tc.content, tc.old, tc.new)
			n := strings.Count(tc.content, "[["+tc.old+"]]")
			if got := ReplaceTitleN(tc.content, tc.old, tc.new, n); got != want {
				t.Errorf("ReplaceTitleN with the true count %d:\n got: %q\nwant: %q", n, got, want)
			}

			// Under-counting must visibly diverge, or the count is not
			// load-bearing and this function has no contract worth stating.
			if n > 1 {
				if got := ReplaceTitleN(tc.content, tc.old, tc.new, n-1); got == want {
					t.Errorf("ReplaceTitleN with a count one too low produced the correct result; " +
						"the caller's obligation is not real, so the API is misleading")
				}
			}
		})
	}
}
