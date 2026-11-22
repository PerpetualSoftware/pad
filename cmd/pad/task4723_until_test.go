package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TASK-4723: a bare --until (snooze, move) and a bare --to (linked move) go to
// the server unchanged; the server places them at the caller's day start and
// the CLI prints the placed instant in the account zone, marked.
func TestTask4723_BareUntilIsSentAndThePlacedInstantShown(t *testing.T) {
	const placed = `"until":"2026-11-10T12:30:00Z","resolved_from":"day_start"`
	for _, c := range []struct {
		state, want string
	}{
		{"snooze", "Snoozed TASK-5 until 2026-11-10T07:30:00-05:00 (start of your day)"},
		{"move", "Moved TASK-5 to 2026-11-10T07:30:00-05:00 (start of your day)"},
	} {
		reply := func(rc recordedCall) (int, string) {
			if strings.HasSuffix(rc.path, "/resolve") {
				return 200, `{"commitment":` + weekdaysView + `,"resolution":{"occurrence_at":"2026-11-09T14:00:00Z","state":"` + c.state + `","missed_recorded":0,` + placed + `}}`
			}
			return 200, weekdaysView
		}
		out, _, err, calls := runSched(t, reply, "", "item", "remind", c.state, "TASK-5", "--until", "2026-11-10")
		if err != nil {
			t.Fatalf("%s: %v", c.state, err)
		}
		var body map[string]any
		_ = json.Unmarshal([]byte(calls[len(calls)-1].body), &body)
		if body["until"] != "2026-11-10" {
			t.Fatalf("%s sent %s, want until 2026-11-10 unchanged", c.state, calls[len(calls)-1].body)
		}
		if !strings.Contains(out, c.want) {
			t.Errorf("%s: out=%q, want %q", c.state, out, c.want)
		}
	}
}

func TestTask4723_LinkedMoveBareToIsSentAndTheRootMarked(t *testing.T) {
	reply := okReply(`{"revision":"r","resolved_from":"day_start","changes":[{"item_id":"i1","role":"target","from":"2026-11-09T14:00:00Z","to":"2026-11-10T12:30:00Z","root":true},{"item_id":"i2","role":"target","from":"2026-11-09T15:00:00Z","to":"2026-11-10T13:30:00Z"}]}`)
	out, _, err, calls := runSched(t, reply, "", "item", "remind", "move", "TASK-5", "--role", "target", "--to", "2026-11-10")
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].body != `{"role":"target","to":"2026-11-10"}` {
		t.Fatalf("calls=%+v, want the bare to unchanged", calls)
	}
	if !strings.Contains(out, "-> 2026-11-10T07:30:00-05:00 (start of your day)\n") {
		t.Errorf("root line not marked: %q", out)
	}
	if strings.Count(out, "(start of your day)") != 1 {
		t.Errorf("only the root change is marked: %q", out)
	}
}

// --at on done still refuses a bare date, and sends nothing.
func TestTask4723_DoneAtStillRefusesABareDate(t *testing.T) {
	_, _, err, calls := runSched(t, okReply(weekdaysView), "", "item", "remind", "done", "TASK-5", "--at", "2026-11-08")
	if err == nil || !strings.Contains(err.Error(), "bare date") || len(calls) != 0 {
		t.Fatalf("err=%v calls=%+v", err, calls)
	}
}
