package main

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cmdhelp"

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

// TestReminderCommandsExposeTheArgsMCPExpects — codex round 5, P1.
//
// cmdhelp derives positionals by regex from a command's `Use` string, and
// `<instant>` inside `remind <ref> --remind-at <instant>` matched: it became a
// second REQUIRED positional, so local stdio MCP dispatch failed with
// `missing required argument "instant"` — the action was advertised and
// unusable on that transport.
//
// The MCP catalog's own test did not catch it because its cmdhelp document is
// HAND-BUILT: I wrote `Args: mkArgs("ref")` there, so the fixture agreed with
// what I meant rather than with what the CLI says. This test reads the REAL
// tree, which is the only thing that can disagree with me.
//
// MUTANT: put a `<...>` placeholder back in any of these Use strings and the
// matching case fails.
func TestReminderCommandsExposeTheArgsMCPExpects(t *testing.T) {
	doc := cmdhelp.Build(newRootCmd(), newRootCmd(), cmdhelp.Options{MaxDepth: -1})

	for _, tc := range []struct {
		path string
		want []string
	}{
		{"item remind", []string{"ref"}},
		{"item ack", []string{"reminder-id"}},
		{"item reminders", []string{"ref"}},
		{"item unremind", []string{"reminder-id"}},
	} {
		cmd, ok := doc.Commands[tc.path]
		if !ok {
			t.Errorf("%q is missing from cmdhelp entirely", tc.path)
			continue
		}
		var got []string
		for _, a := range cmd.Args {
			got = append(got, a.Name)
		}
		if len(got) != len(tc.want) {
			t.Errorf("%q positionals = %v, want %v", tc.path, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%q positionals = %v, want %v", tc.path, got, tc.want)
				break
			}
		}
	}

	// The flag MCP actually sends must exist under the name it sends.
	if _, ok := doc.Commands["item remind"].Flags["remind-at"]; !ok {
		t.Error("`item remind` has no --remind-at flag; the MCP remind_at param maps to nothing")
	}
}
