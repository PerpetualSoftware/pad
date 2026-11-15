package main

import (
	"strings"
	"testing"
	"time"
)

func TestBUG4570RemindShowSnoozeAndMove(t *testing.T) {
	for _, snoozed := range []bool{true, false} {
		body := strings.TrimSuffix(weekdaysView, "}") + `,"next_override_at":"2026-11-21T14:30:00Z"`
		if snoozed {
			body += `,"snoozed_until":"2026-11-21T14:00:00Z"`
		}
		out, _, err, _ := runSched(t, okReply(body+"}"), "", "remind", "show", "TASK-5")
		if err != nil {
			t.Fatal(err)
		}
		if snoozed {
			if !strings.Contains(out, "Snoozed until:") || !strings.Contains(out, "2026-11-21 09:00 EST") || strings.Contains(out, "Deferred to:") {
				t.Errorf("snooze output:\n%s", out)
			}
		} else if !strings.Contains(out, "Deferred to:") || strings.Contains(out, "Snoozed until:") {
			t.Errorf("move output:\n%s", out)
		}
	}
}

func TestBUG4570AgendaSnoozeFlag(t *testing.T) {
	body := strings.Replace(agendaBody, `"overdue":true`, `"overdue":true,"snoozed_until":"2026-11-21T14:00:00Z"`, 1)
	h, _ := recordingStub(okReply(body))
	out, _, err := tzRun(t, tzCLIWithLocal(t, h, time.UTC), "remind", "agenda", "--from", "2026-11-01")
	if err != nil || !strings.Contains(out, "snoozed until 2026-11-21 09:00 EST") || !strings.Contains(out, "overdue") {
		t.Fatalf("output=%s err=%v", out, err)
	}
}
