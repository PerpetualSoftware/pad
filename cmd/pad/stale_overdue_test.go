package main

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/server"
)

// The CLI half of IDEA-2641's stale leg.
//
// `pad project stale` does no date work of its own — it filters the
// dashboard's attention list and keeps four types. The server-side leg pins
// that an overdue item lands in that list carrying type "overdue"; this pins
// the other half, that stale still keeps it. Split across the two packages
// because that is where the two halves actually live: a single test could not
// fail for the CLI's reason.
//
// MUTANT: removing "overdue" from filterAgentAttention's interesting map makes
// deadlines vanish from `pad project stale` while every server-side assertion
// stays green.
func TestStaleKeepsOverdueAttention(t *testing.T) {
	attention := []server.DashboardAttention{
		{Type: "overdue", ItemRef: "TASK-1", ItemTitle: "Late", Reason: "due date was 2020-01-01"},
		{Type: "plan_completion", ItemRef: "PLAN-1", ItemTitle: "Done plan"},
	}

	got := filterAgentAttention(attention)

	var sawOverdue bool
	for _, a := range got {
		if a.Type == "overdue" && a.ItemRef == "TASK-1" {
			sawOverdue = true
		}
		if a.Type == "plan_completion" {
			t.Error("plan_completion is not an agent-actionable attention type and must be filtered out")
		}
	}
	if !sawOverdue {
		t.Error("`pad project stale` dropped the overdue entry; deadlines never reach the CLI surface")
	}
}

// TestRemindArgsAcceptRearmWithoutARef — codex round 2.
//
// `--rearm` addresses a reminder by id and needs no item ref, but ExactArgs(1)
// forced one and the rearm branch then ignored it — so the flag could not be
// used at all, and the ref a user supplied to satisfy cobra was silently
// discarded.
//
// MUTANT: restore ExactArgs(1) and the zero-arg case fails; drop the
// ref-with-rearm refusal and the ambiguous case stops failing.
func TestRemindArgsAcceptRearmWithoutARef(t *testing.T) {
	cmd := remindCmd()
	if err := cmd.Args(cmd, []string{}); err != nil {
		t.Errorf("remind must accept zero args so --rearm is usable: %v", err)
	}
	if err := cmd.Args(cmd, []string{"TASK-1"}); err != nil {
		t.Errorf("remind must still accept an item ref: %v", err)
	}
	if err := cmd.Args(cmd, []string{"TASK-1", "TASK-2"}); err == nil {
		t.Error("remind accepted two positional args")
	}
}
