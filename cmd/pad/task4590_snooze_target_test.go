package main

import (
	"strings"
	"testing"
)

// TASK-4590: a snoozed occurrence shows its target beside the snooze, the moved
// target when the snooze followed a move.
func TestTASK4590RemindShowSnoozeKeepsTarget(t *testing.T) {
	snooze := `,"next_override_at":"2026-11-21T14:30:00Z","snoozed_until":"2026-11-21T14:00:00Z"}`
	for name, tc := range map[string]struct{ body, want string }{
		"moved then snoozed": {strings.Replace(weekdaysView, `"target":"2026-11-09T14:00:00Z"`, `"target":"2026-11-21T15:00:00Z"`, 1), "2026-11-21 10:00 EST"},
		"snoozed":            {weekdaysView, "2026-11-09 09:00 EST"},
		"no target":          {strings.Replace(weekdaysView, `"target":"2026-11-09T14:00:00Z"`, `"target":null`, 1), ""},
	} {
		t.Run(name, func(t *testing.T) {
			out, _, err, _ := runSched(t, okReply(strings.TrimSuffix(tc.body, "}")+snooze), "", "remind", "show", "TASK-5")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out, "Snoozed until:") || !strings.Contains(out, "2026-11-21 09:00 EST") || strings.Contains(out, "Deferred to:") {
				t.Fatalf("snooze output:\n%s", out)
			}
			line := ""
			for _, l := range strings.Split(out, "\n") {
				if strings.HasPrefix(l, "Target:") {
					line = l
				}
			}
			if tc.want == "" {
				if line != "" {
					t.Fatalf("no target, got %q", line)
				}
			} else if !strings.Contains(line, tc.want) {
				t.Fatalf("target line = %q, want %s\n%s", line, tc.want, out)
			}
		})
	}
}
