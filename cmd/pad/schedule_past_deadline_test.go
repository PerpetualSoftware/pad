package main

import (
	"strings"
	"testing"
)

func TestRemindPastDeadlineFlag(t *testing.T) {
	for _, args := range [][]string{
		{"--until", "2030-03-08T09:00:00Z", "--occurrence", "2030-03-04T09:00:00Z"},
		{"--role", "target", "--to", "2030-03-08T09:00:00Z", "--apply", "--revision", strings.Repeat("a", 64)},
	} {
		argv := append([]string{"remind", "move", "TASK-5", "--past-deadline"}, args...)
		_, _, err, calls := runSched(t, okReply(`{"revision":"r","changes":[],"commitment":`+weekdaysView+`,"resolution":{"state":"move"}}`), "", argv...)
		if err != nil || len(calls) != 1 {
			t.Fatalf("err=%v calls=%v", err, calls)
		}
		if resolveBody(t, calls[0])["confirm_past_deadline"] != true {
			t.Fatalf("body=%s", calls[0].body)
		}
	}
}

func TestRemindPastDeadlineFeedback(t *testing.T) {
	for _, args := range [][]string{
		{"--until", "2030-03-08T09:00:00Z", "--occurrence", "2030-03-04T09:00:00Z"},
		{"--role", "target", "--to", "2030-03-08T09:00:00Z", "--apply", "--revision", strings.Repeat("a", 64)},
	} {
		_, _, err, _ := runSched(t, apiError(409, "target_after_deadline", "Refused", `{"deadline":"2030-03-06T09:00:00Z"}`), "", append([]string{"remind", "move", "TASK-5"}, args...)...)
		if err == nil || !strings.Contains(err.Error(), "Re-run with --past-deadline to move it anyway.") || !strings.Contains(err.Error(), "2030-03-06 04:00 EST") {
			t.Fatalf("err=%v", err)
		}
	}
	out, _, err, _ := runSched(t, okReply(`{"revision":"r","changes":[],"past_deadline":"2030-03-06T09:00:00Z"}`), "", "remind", "move", "TASK-5", "--role", "target", "--to", "2030-03-08T09:00:00Z")
	if err != nil || !strings.Contains(out, "Crosses its hard deadline (") || !strings.Contains(out, "): --apply needs --past-deadline.") {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

func TestRemindPastDeadlineFlagCombinations(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		message string
	}{
		{[]string{"--role", "target", "--to", "2030-03-08T09:00:00Z"}, "--apply"},
		{[]string{"--role", "deadline", "--to", "2030-03-08T09:00:00Z", "--apply"}, "target"},
	} {
		_, _, err, calls := runSched(t, okReply(`{}`), "", append([]string{"remind", "move", "TASK-5", "--past-deadline"}, tc.args...)...)
		if err == nil || !strings.Contains(err.Error(), tc.message) || len(calls) != 0 {
			t.Fatalf("err=%v calls=%v", err, calls)
		}
	}
}
