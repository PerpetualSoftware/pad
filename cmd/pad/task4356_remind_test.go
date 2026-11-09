package main

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// TASK-4356: `pad remind` is the top-level name for `pad item remind`.

func findSub(root *cobra.Command, path ...string) *cobra.Command {
	cur := root
	for _, name := range path {
		var next *cobra.Command
		for _, c := range cur.Commands() {
			if c.Name() == name {
				next = c
			}
		}
		if next == nil {
			return nil
		}
		cur = next
	}
	return cur
}

func shape(c *cobra.Command) map[string][]string {
	out := map[string][]string{}
	var walk func(prefix string, c *cobra.Command)
	walk = func(prefix string, c *cobra.Command) {
		var flags []string
		c.Flags().VisitAll(func(f *pflag.Flag) { flags = append(flags, f.Name+"="+f.DefValue+"/"+f.Usage) })
		sort.Strings(flags)
		out[prefix+" "+c.Use] = append(flags, "short="+c.Short)
		for _, s := range c.Commands() {
			walk(prefix+" "+c.Name(), s)
		}
	}
	walk("", c)
	return out
}

func TestRemindTopLevelAndItemFormShareOneTree(t *testing.T) {
	root := newRootCmd()
	top, item := findSub(root, "remind"), findSub(root, "item", "remind")
	if top == nil || item == nil {
		t.Fatalf("top=%v item=%v", top, item)
	}
	if !reflect.DeepEqual(shape(top), shape(item)) {
		t.Fatalf("trees differ:\ntop:  %v\nitem: %v", shape(top), shape(item))
	}
	if !strings.Contains(item.Long, "alias of 'pad remind'") {
		t.Errorf("the item form must say it is an alias of pad remind: %q", item.Long)
	}
	if strings.Contains(top.Long, "alias of") {
		t.Errorf("the top-level form is the name, not an alias")
	}
	if findSub(root, "remind", "renegotiate") == nil || findSub(root, "item", "remind", "renegotiate") == nil {
		t.Error("renegotiate missing")
	}
}

// The two instances share no flag state.
func TestRemindFlagsDoNotLeakBetweenForms(t *testing.T) {
	root := newRootCmd()
	top, item := findSub(root, "remind"), findSub(root, "item", "remind")
	if err := top.Flags().Set("start", "2026-11-09T09:00"); err != nil {
		t.Fatal(err)
	}
	if v, _ := item.Flags().GetString("start"); v != "" {
		t.Fatalf("item form saw the top-level value %q", v)
	}
}

func TestRemindBothFormsAreIdentical(t *testing.T) {
	const occ = "2026-11-09T14:00:00Z"
	reply := func(c recordedCall) (int, string) {
		switch {
		case c.method == "GET" && c.path == "/workspaces/test/items/TASK-5":
			return 200, `{"id":"i1","ref":"TASK-5","slug":"t-5","title":"T","collection_slug":"tasks","fields":"{}"}`
		case c.method == "POST" && strings.HasSuffix(c.path, "/reminders"):
			return 201, `{"id":"r1","item_id":"i1","remind_at":"2099-01-01T09:00:00Z"}`
		case strings.HasSuffix(c.path, "/resolve"):
			return 200, resolved
		case c.method == "POST":
			return 201, weekdaysView
		}
		return 200, weekdaysView
	}
	for name, args := range map[string][]string{
		"create":   {"TASK-5", "--start", "2026-11-09T09:00", "--every", "weekdays", "--deadline", "hard"},
		"one-shot": {"TASK-5", "--remind-at", "2099-01-01T09:00:00Z"},
		"show":     {"show", "TASK-5"},
		"done":     {"done", "TASK-5", "-m", "ok", "--occurrence", occ},
		"skip":     {"skip", "TASK-5", "--occurrence", occ},
		"snooze":   {"snooze", "TASK-5", "--until", "2026-11-10T09:00", "--occurrence", occ},
		"move":     {"move", "TASK-5", "--until", "2026-11-10T09:00", "--occurrence", occ},
		"edit":     {"edit", "TASK-5", "--every", "weekly"},
	} {
		for _, format := range []string{"table", "json"} {
			t.Run(name+"/"+format, func(t *testing.T) {
				a, aerr, acalls := runSchedFull(t, reply, append([]string{"--format", format, "remind"}, args...)...)
				b, berr, bcalls := runSchedFull(t, reply, append([]string{"--format", format, "item", "remind"}, args...)...)
				if aerr != nil || berr != nil {
					t.Fatalf("top=%v item=%v", aerr, berr)
				}
				if a != b || !reflect.DeepEqual(acalls, bcalls) || len(acalls) == 0 {
					t.Fatalf("differ:\ntop:  %q %+v\nitem: %q %+v", a, acalls, b, bcalls)
				}
			})
		}
	}
}

func runSchedFull(t *testing.T, reply func(recordedCall) (int, string), args ...string) (string, error, []recordedCall) {
	t.Helper()
	out, _, err, calls := runSched(t, reply, "", args...)
	return out, err, calls
}

func TestRemindDoneAtActualCompletion(t *testing.T) {
	const occ = "2026-11-09T14:00:00Z"
	reply := func(c recordedCall) (int, string) {
		if strings.HasSuffix(c.path, "/resolve") {
			return 200, resolved
		}
		return 200, weekdaysView
	}
	out, _, err, calls := runSched(t, reply, "", "remind", "done", "TASK-5", "--occurrence", occ, "--at", "2026-11-08T20:30:00+02:00")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := calls[0].body, `{"occurrence_at":"`+occ+`","state":"done","actual_done_at":"2026-11-08T18:30:00Z"}`; got != want {
		t.Fatalf("body=%s want %s", got, want)
	}
	if !strings.Contains(out, "2026-11-08 13:30 EST") {
		t.Fatalf("the sentence must carry the actual time: %q", out)
	}
	// A wall time reads in the viewer's zone.
	_, _, err, calls = runSched(t, reply, "", "remind", "done", "TASK-5", "--occurrence", occ, "--at", "2026-11-08T13:30")
	if err != nil || !strings.Contains(calls[0].body, `"actual_done_at":"2026-11-08T18:30:00Z"`) {
		t.Fatalf("err=%v calls=%+v", err, calls)
	}
	// JSON passes the server body through.
	out, _, _, _ = runSched(t, reply, "", "remind", "done", "TASK-5", "--occurrence", occ, "--at", "2026-11-08T13:30", "--format", "json")
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil || got["resolution"] == nil {
		t.Fatalf("out=%q", out)
	}
}

func TestRemindAtRefusals(t *testing.T) {
	const occ = "2026-11-09T14:00:00Z"
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"skip", "TASK-5", "--occurrence", occ, "--at", "2026-11-08T13:30"}, "done"},
		{[]string{"snooze", "TASK-5", "--occurrence", occ, "--until", "2h", "--at", "2026-11-08T13:30"}, "done"},
		{[]string{"move", "TASK-5", "--occurrence", occ, "--until", "2026-11-12T09:00", "--at", "2026-11-08T13:30"}, "done"},
		{[]string{"done", "TASK-5", "--occurrence", occ, "--at", "2026-11-08"}, "bare date"},
		{[]string{"done", "TASK-5", "--occurrence", occ, "--at", "2h"}, "--at"},
		{[]string{"done", "TASK-5", "--occurrence", occ, "--at", "yesterday"}, "--at"},
	} {
		_, _, err, calls := runSched(t, okReply(weekdaysView), "", append([]string{"remind"}, tc.args...)...)
		if err == nil || !strings.Contains(err.Error(), tc.want) || len(calls) != 0 {
			t.Fatalf("%v: err=%v calls=%+v", tc.args, err, calls)
		}
	}
}

func TestRemindAtServerRefusalsPassThrough(t *testing.T) {
	const occ = "2026-11-09T14:00:00Z"
	for _, msg := range []string{
		"actual completion cannot be in the future",
		"actual completion cannot be before schedule creation; the earliest allowed is 2026-11-01T00:00:00Z",
	} {
		_, _, err, calls := runSched(t, apiError(400, "validation_error", msg, `{"field":"actual_done_at","code":"range"}`), "",
			"remind", "done", "TASK-5", "--occurrence", occ, "--at", "2026-11-08T13:30")
		if err == nil || err.Error() != msg || len(calls) != 1 {
			t.Fatalf("err=%v calls=%+v", err, calls)
		}
	}
}

func TestRemindStaleThroughTopLevelName(t *testing.T) {
	stale := apiError(409, "stale", "That occurrence is no longer the outstanding one",
		`{"ended":false,"next_occurrence":{"at":"2026-11-10T14:00:00Z","wall":"2026-11-10T09:00:00"}}`)
	_, _, err, _ := runSched(t, stale, "", "remind", "done", "TASK-5", "--occurrence", "2026-11-09T14:00:00Z")
	if err == nil || !strings.Contains(err.Error(), "--occurrence 2026-11-10T14:00:00Z") {
		t.Fatalf("err=%v", err)
	}
}

const reneg = `{"preview":{"scope":"occurrence","occurrence_key":"2026-11-09T14:00:00Z","applies_from":"2026-11-09T14:00:00Z","current_deadline":"2026-11-09T20:00:00Z","new_deadline":"2099-01-01T09:00:00Z","miss_recorded_first":false},"revision":"rev-1"}`

func renegReply(c recordedCall) (int, string) {
	if c.method == "POST" && strings.HasSuffix(c.path, "/commitment/renegotiate") {
		if strings.Contains(c.body, `"preview":true`) {
			return 200, reneg
		}
		return 200, weekdaysView
	}
	return 404, `{"error":{"code":"not_found","message":"no"}}`
}

func bodyOf(t *testing.T, c recordedCall) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(c.body), &m); err != nil {
		t.Fatalf("%s: %v", c.body, err)
	}
	return m
}

func TestRemindRenegotiate(t *testing.T) {
	base := []string{"remind", "renegotiate", "TASK-5", "--reason", "slipped", "--deadline", "2099-01-01T09:00:00+02:00"}
	t.Run("default is a preview", func(t *testing.T) {
		out, _, err, calls := runSched(t, renegReply, "", base...)
		if err != nil || len(calls) != 1 || calls[0].path != schedPath+"/renegotiate" {
			t.Fatalf("err=%v calls=%+v", err, calls)
		}
		b := bodyOf(t, calls[0])
		if b["preview"] != true || b["reason"] != "slipped" || b["deadline"] != "2099-01-01T07:00:00Z" || b["scope"] != "occurrence" {
			t.Fatalf("body=%v", b)
		}
		if !strings.Contains(out, "rev-1") || !strings.Contains(out, "--apply") || !strings.Contains(out, "nothing changed") {
			t.Fatalf("out=%q", out)
		}
	})
	t.Run("apply previews then applies the returned revision", func(t *testing.T) {
		_, _, err, calls := runSched(t, renegReply, "", append(base, "--apply")...)
		if err != nil || len(calls) != 2 {
			t.Fatalf("err=%v calls=%+v", err, calls)
		}
		if bodyOf(t, calls[0])["preview"] != true {
			t.Fatalf("first=%s", calls[0].body)
		}
		b := bodyOf(t, calls[1])
		if b["preview"] != false || b["revision"] != "rev-1" {
			t.Fatalf("second=%v", b)
		}
	})
	t.Run("apply with a revision skips the preview", func(t *testing.T) {
		_, _, err, calls := runSched(t, renegReply, "", append(base, "--apply", "--revision", "rev-9")...)
		if err != nil || len(calls) != 1 || bodyOf(t, calls[0])["revision"] != "rev-9" || bodyOf(t, calls[0])["preview"] != false {
			t.Fatalf("err=%v calls=%+v", err, calls)
		}
	})
	t.Run("series", func(t *testing.T) {
		_, _, err, calls := runSched(t, renegReply, "", "remind", "renegotiate", "TASK-5", "--reason", "r", "--scope", "series",
			"--deadline-offset-days", "2", "--deadline-offset-secs", "3600", "--behavior", "lapses")
		if err != nil {
			t.Fatal(err)
		}
		b := bodyOf(t, calls[0])
		want := map[string]any{"deadline_days": float64(2), "deadline_secs": float64(3600)}
		if !reflect.DeepEqual(b["deadline_offset"], want) || b["deadline_behavior"] != "lapses" || b["scope"] != "series" {
			t.Fatalf("body=%v", b)
		}
		_, _, err, calls = runSched(t, renegReply, "", "remind", "renegotiate", "TASK-5", "--reason", "r", "--scope", "series", "--remove-deadline")
		if err != nil || bodyOf(t, calls[0])["remove_deadline"] != true {
			t.Fatalf("err=%v calls=%+v", err, calls)
		}
	})
	t.Run("json passes the server body", func(t *testing.T) {
		out, _, err, _ := runSched(t, renegReply, "", append(base, "--format", "json")...)
		var got map[string]any
		if err != nil || json.Unmarshal([]byte(out), &got) != nil || got["revision"] != "rev-1" {
			t.Fatalf("out=%q err=%v", out, err)
		}
	})
	t.Run("refusals before any call", func(t *testing.T) {
		for _, args := range [][]string{
			{"remind", "renegotiate", "TASK-5", "--deadline", "2099-01-01T09:00:00Z"},
			{"remind", "renegotiate", "TASK-5", "--reason", "  ", "--deadline", "2099-01-01T09:00:00Z"},
			{"remind", "renegotiate", "TASK-5", "--reason", "r", "--deadline", "2099-01-01"},
			{"remind", "renegotiate", "TASK-5", "--reason", "r", "--deadline", "2099-01-01T09:00"},
		} {
			_, _, err, calls := runSched(t, renegReply, "", args...)
			if err == nil || len(calls) != 0 {
				t.Errorf("%v: err=%v calls=%+v", args, err, calls)
			}
		}
	})
	t.Run("combinations are the servers to judge", func(t *testing.T) {
		const msg = "an instant deadline needs scope occurrence"
		_, _, err, calls := runSched(t, apiError(400, "validation_error", msg, `{"field":"deadline","code":"combination"}`), "",
			"remind", "renegotiate", "TASK-5", "--reason", "r", "--scope", "series", "--deadline", "2099-01-01T09:00:00Z")
		if err == nil || err.Error() != msg || len(calls) != 1 {
			t.Fatalf("err=%v calls=%+v", err, calls)
		}
	})
	t.Run("server answers pass through", func(t *testing.T) {
		for _, tc := range []struct {
			status    int
			code, msg string
			details   string
			want      string
		}{
			{409, "commitment_changed", "The commitment changed; re-preview before applying", `{"field":"revision","code":"stale"}`, "re-preview"},
			{403, "deadline_owner_required", "Only the assignee or an owner may change a hard deadline", "", "Only the assignee"},
			{400, "schedule:ended", "the schedule has ended", "", "the schedule has ended"},
			{409, "stale", "That occurrence is no longer the outstanding one", `{"ended":false,"next_occurrence":{"at":"2026-11-10T14:00:00Z","wall":"2026-11-10T09:00:00"}}`, "2026-11-10 09:00 EST"},
		} {
			_, _, err, calls := runSched(t, apiError(tc.status, tc.code, tc.msg, tc.details), "", append(base, "--apply", "--revision", "rev-1")...)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), tc.msg) || len(calls) != 1 {
				t.Errorf("%s: err=%v calls=%+v", tc.code, err, calls)
			}
		}
		_, _, err, _ := runSched(t, apiError(409, "commitment_changed", "The commitment changed; re-preview before applying", ""), "", append(base, "--apply", "--revision", "rev-1")...)
		if err == nil || !strings.Contains(err.Error(), "pad remind renegotiate TASK-5") {
			t.Errorf("commitment_changed must hint at a re-preview: %v", err)
		}
	})
}

func TestRemindAgendaMatchesTheEndpoint(t *testing.T) {
	const body = `{"timezone_note":"","entries":[{"type":"occurrence","at":"2026-11-09T14:00:00Z","item_ref":"TASK-5"}],"truncated":false,"extra":1}`
	out, _, err, calls := runSched(t, okReply(body), "", "remind", "agenda", "--days", "7", "--format", "json")
	if err != nil || len(calls) != 1 || calls[0].method != "GET" || calls[0].path != "/workspaces/test/agenda" {
		t.Fatalf("err=%v calls=%+v", err, calls)
	}
	var a, b any
	if json.Unmarshal([]byte(out), &a) != nil || json.Unmarshal([]byte(body), &b) != nil || !reflect.DeepEqual(a, b) {
		t.Fatalf("out=%s", out)
	}
}

func TestRemindShowMatchesTheCommitmentEndpoint(t *testing.T) {
	const view = `{"item_ref":"TASK-5","roles":{"target":"2026-11-09T14:00:00Z","deadline":"2026-11-09T20:00:00Z","occupied_until":null},"dtstart":"2026-11-09T09:00:00","tz":"UTC","anchor":"calendar","audience":"both","next_occurrence":{"at":"2026-11-09T14:00:00Z","wall":"2026-11-09T09:00:00"},"extra":{"k":1}}`
	out, _, err, calls := runSched(t, okReply(view), "", "remind", "show", "TASK-5", "--format", "json")
	if err != nil || len(calls) != 1 || calls[0].method != "GET" || calls[0].path != schedPath {
		t.Fatalf("err=%v calls=%+v", err, calls)
	}
	var a, b any
	_ = json.Unmarshal([]byte(out), &a)
	_ = json.Unmarshal([]byte(view), &b)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("out=%s", out)
	}
	// create with flags sends target + deadline roles the server reads back.
	_, _, err, calls = runSched(t, okReply(view), "", "remind", "TASK-5", "--start", "2026-11-09T09:00", "--deadline", "hard")
	if err != nil || bodyOf(t, calls[0])["roles"] == nil {
		t.Fatalf("err=%v calls=%+v", err, calls)
	}
}
