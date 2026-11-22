package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TASK-4699 PR2: a bare --remind-at goes to the server, which places it at the
// caller's day start; the CLI prints the resolved instant in the caller's zone.
// --day-start asks the server to place a bare --start the same way.
func TestTask4699_RemindBareDatePrintsTheResolvedInstant(t *testing.T) {
	reminder := `{"id":"r1","item_id":"i1","remind_at":"2026-11-09T12:30:00Z","resolved_from":"day_start"}`
	reply := func(c recordedCall) (int, string) {
		switch {
		case c.method == "GET" && c.path == "/workspaces/test/items/TASK-5":
			return 200, `{"id":"i1","ref":"TASK-5","slug":"t-5","title":"T","collection_slug":"tasks","fields":"{}"}`
		case c.method == "POST" && c.path == "/workspaces/test/items/t-5/reminders":
			return 201, reminder
		case c.method == "PATCH" && c.path == "/workspaces/test/reminders/r1":
			return 200, reminder
		}
		return 404, `{"error":{"code":"not_found","message":"no"}}`
	}
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"arm", []string{"item", "remind", "TASK-5", "--remind-at", "2026-11-09"}, "Run-once commitment created on TASK-5 for 2026-11-09T07:30:00-05:00 (start of your day) (id r1)\n"},
		{"rearm", []string{"item", "remind", "--rearm", "r1", "--remind-at", "2026-11-09"}, "Re-armed commitment alarm r1 for 2026-11-09T07:30:00-05:00 (start of your day)\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, get := recordingStub(reply)
			root := tzCLIWithLocal(t, h, time.UTC)
			out, _, err := tzRun(t, root, c.args...)
			if err != nil || out != c.want {
				t.Fatalf("out=%q err=%v, want %q", out, err, c.want)
			}
			calls := get()
			if last := calls[len(calls)-1]; last.body != `{"remind_at":"2026-11-09"}` {
				t.Fatalf("sent %q, want the bare date", last.body)
			}
		})
	}
}

func TestTask4699_DayStartFlag(t *testing.T) {
	_, _, err, calls := runSched(t, func(recordedCall) (int, string) { return 201, weekdaysView },
		"", "item", "remind", "TASK-5", "--start", "2026-11-09", "--every", "daily", "--day-start")
	if err != nil || len(calls) != 1 || calls[0].method != "POST" {
		t.Fatalf("create: err=%v calls=%+v", err, calls)
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(calls[0].body), &body)
	if body["day_start"] != true || body["dtstart"] != "2026-11-09" {
		t.Fatalf("create body=%s", calls[0].body)
	}

	_, _, err, calls = runSched(t, okReply(weekdaysView), "", "item", "remind", "edit", "TASK-5", "--start", "2026-11-10", "--day-start")
	if err != nil || len(calls) != 1 || calls[0].method != "PATCH" || calls[0].body != `{"day_start":true,"dtstart":"2026-11-10"}` {
		t.Fatalf("edit: err=%v calls=%+v", err, calls)
	}

	for _, args := range [][]string{
		{"item", "remind", "TASK-5", "--start", "2026-11-09", "--window", "3d", "--day-start"},
		{"item", "remind", "edit", "TASK-5", "--start", "2026-11-09", "--window", "3d", "--day-start"},
	} {
		_, _, err, calls = runSched(t, okReply(`{}`), "", args...)
		if err == nil || !strings.Contains(err.Error(), "--day-start") || !strings.Contains(err.Error(), "--window") || len(calls) != 0 {
			t.Fatalf("%v: err=%v calls=%+v", args, err, calls)
		}
	}
}
