package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/cmdhelp"
)

const weekdaysView = `{"id":"s1","item_ref":"TASK-5","kind":"check","dtstart":"2026-11-09T09:00:00","tz":"America/New_York","floating":false,"rrule":"FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR","anchor":"calendar","audience":"both","next_occurrence":{"at":"2026-11-09T14:00:00Z","wall":"2026-11-09T09:00:00"},"missed_count":3,"stale":true}`

const resolved = `{"schedule":` + weekdaysView + `,"resolution":{"occurrence_at":"2026-11-09T14:00:00Z","state":"done","missed_recorded":0}}`

const schedPath = "/workspaces/test/items/TASK-5/schedule"

func okReply(body string) func(recordedCall) (int, string) {
	return func(recordedCall) (int, string) { return 200, body }
}

func runSched(t *testing.T, reply func(recordedCall) (int, string), stdin string, args ...string) (out, stderr string, err error, calls []recordedCall) {
	t.Helper()
	h, get := recordingStub(reply)
	root := tzCLIWithLocal(t, h, time.UTC)
	root.SetIn(strings.NewReader(stdin))
	out, stderr, err = tzRun(t, root, args...)
	return out, stderr, err, get()
}

func apiError(status int, code, msg, details string) func(recordedCall) (int, string) {
	return func(recordedCall) (int, string) {
		extra := ""
		if details != "" {
			extra = `,"details":` + details
		}
		return status, `{"error":{"code":"` + code + `","message":"` + msg + `"` + extra + `}}`
	}
}

func TestRemindScheduleCreate(t *testing.T) {
	out, stderr, err, calls := runSched(t, func(c recordedCall) (int, string) {
		return 201, strings.TrimSuffix(weekdaysView, "}") + `,"timezone_note":"pinned to UTC; set your zone"}`
	}, "", "item", "remind", "TASK-5", "--start", "2026-11-09T09:00", "--every", "Weekdays")
	if err != nil {
		t.Fatal(err)
	}
	want := "Scheduled TASK-5: check, weekdays from 2026-11-09 09:00 America/New_York; next 2026-11-09 09:00 EST\n"
	if out != want {
		t.Fatalf("out=%q want %q", out, want)
	}
	if stderr != "pinned to UTC; set your zone\n" {
		t.Fatalf("stderr=%q", stderr)
	}
	if len(calls) != 1 || calls[0].method != "POST" || calls[0].path != schedPath {
		t.Fatalf("calls=%+v", calls)
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(calls[0].body), &body)
	if _, hasTz := body["tz"]; hasTz || body["rrule"] != "FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR" || body["dtstart"] != "2026-11-09T09:00:00" || body["kind"] != "check" {
		t.Fatalf("body=%s", calls[0].body)
	}

	// JSON is the server body, and the note stays out of stderr.
	out, stderr, err, _ = runSched(t, func(recordedCall) (int, string) {
		return 201, strings.TrimSuffix(weekdaysView, "}") + `,"timezone_note":"n","extra":7}`
	}, "", "item", "remind", "TASK-5", "--start", "2026-11-09T09:00", "--format", "json")
	if err != nil || stderr != "" || !strings.Contains(out, `"extra": 7`) {
		t.Fatalf("out=%q stderr=%q err=%v", out, stderr, err)
	}
}

func TestRemindScheduleRefusesLocally(t *testing.T) {
	_, _, err, calls := runSched(t, okReply(`{}`), "", "item", "remind", "TASK-5", "--every", "daily")
	if err == nil || !strings.Contains(err.Error(), "--start is required") || len(calls) != 0 {
		t.Fatalf("err=%v calls=%+v", err, calls)
	}
	_, _, err, calls = runSched(t, okReply(`{}`), "", "item", "remind", "TASK-5", "--remind-at", "2099-01-01T09:00:00Z", "--tz", "Europe/Paris")
	if err == nil || !strings.Contains(err.Error(), "--start") || len(calls) != 0 {
		t.Fatalf("err=%v calls=%+v", err, calls)
	}
}

// The server alone judges these; the CLI forwards the request and returns its
// message untouched.
func TestRemindServerRefusalsPassThrough(t *testing.T) {
	const anchorMsg = "anchor completion is only valid for a plain interval rule"
	_, _, err, calls := runSched(t, apiError(400, "validation_error", anchorMsg, `{"field":"anchor","code":"combination"}`), "",
		"item", "remind", "TASK-5", "--start", "2026-11-09T09:00", "--every", "FREQ=WEEKLY;BYDAY=MO", "--anchor", "completion")
	if err == nil || err.Error() != anchorMsg || len(calls) != 1 || calls[0].method != "POST" {
		t.Fatalf("err=%v calls=%+v", err, calls)
	}

	const floorMsg = "a rule that fires more often than every 15 minutes is not allowed"
	_, _, err, calls = runSched(t, apiError(400, "validation_error", floorMsg, `{"field":"rrule","code":"range"}`), "",
		"item", "remind", "edit", "TASK-5", "--every", "FREQ=MINUTELY;INTERVAL=5")
	if err == nil || err.Error() != floorMsg || len(calls) != 1 || calls[0].method != "PATCH" || calls[0].path != schedPath {
		t.Fatalf("err=%v calls=%+v", err, calls)
	}

	const busy = "The schedule could not be computed right now; retry"
	_, _, err, _ = runSched(t, apiError(503, "schedule_busy", busy, ""), "", "item", "remind", "edit", "TASK-5", "--audience", "agent")
	if err == nil || err.Error() != busy {
		t.Fatalf("err=%v", err)
	}

	const softMsg = "A comment is required to move a soft deadline"
	_, _, err, calls = runSched(t, func(c recordedCall) (int, string) {
		if c.method == "GET" {
			return 200, weekdaysView
		}
		return apiError(400, "validation_error", softMsg, `{"field":"comment","code":"required"}`)(c)
	}, "", "item", "remind", "move", "TASK-5", "--until", "2026-11-10T09:00")
	if err == nil || err.Error() != softMsg || len(calls) != 2 {
		t.Fatalf("err=%v calls=%+v", err, calls)
	}
}

func TestRemindScheduleSubcommandsHTTP(t *testing.T) {
	viewOrResolved := func(c recordedCall) (int, string) {
		switch {
		case c.method == "DELETE":
			return 200, `{"id":"s1","deleted":true}`
		case strings.HasSuffix(c.path, "/resolve"):
			return 200, resolved
		}
		return 200, weekdaysView
	}
	const occ = "2026-11-09T14:00:00Z"
	for _, tc := range []struct {
		name      string
		args      []string
		stdin     string
		methods   []string // method and path suffix of each call, in order
		lastBody  string   // "" = not checked
		outHas    []string
		stderrHas string
	}{
		{name: "done", args: []string{"done", "TASK-5"},
			methods:  []string{"GET " + schedPath, "POST " + schedPath + "/resolve"},
			lastBody: `{"occurrence_at":"` + occ + `","state":"done"}`, outHas: []string{"Marked done TASK-5 (2026-11-09 09:00 EST)"}},
		{name: "skip with comment and occurrence", args: []string{"skip", "TASK-5", "-m", "away", "--occurrence", occ},
			methods:  []string{"POST " + schedPath + "/resolve"},
			lastBody: `{"occurrence_at":"` + occ + `","state":"skip","comment":"away"}`, outHas: []string{"Skipped TASK-5"}},
		{name: "snooze wall", args: []string{"snooze", "TASK-5", "--until", "2026-11-10T09:00"},
			methods:  []string{"GET " + schedPath, "POST " + schedPath + "/resolve"},
			lastBody: `{"occurrence_at":"` + occ + `","state":"snooze","until":"2026-11-10T14:00:00Z"}`, outHas: []string{"Snoozed TASK-5 until 2026-11-10 09:00 EST"}},
		{name: "move instant", args: []string{"move", "TASK-5", "--until", "2026-11-10T20:00:00+02:00", "-m", "later"},
			methods:  []string{"GET " + schedPath, "POST " + schedPath + "/resolve"},
			lastBody: `{"occurrence_at":"` + occ + `","state":"move","comment":"later","until":"2026-11-10T18:00:00Z"}`, outHas: []string{"Moved TASK-5 to 2026-11-10 13:00 EST"}},
		{name: "show", args: []string{"show", "TASK-5"}, methods: []string{"GET " + schedPath},
			outHas: []string{"weekdays", "America/New_York", "2026-11-09 09:00 EST", "3 (stale)", "active"}},
		{name: "edit rule", args: []string{"edit", "TASK-5", "--every", "weekly"}, methods: []string{"PATCH " + schedPath},
			lastBody: `{"rrule":"FREQ=WEEKLY"}`, outHas: []string{"Updated TASK-5"}},
		{name: "edit clears rule", args: []string{"edit", "TASK-5", "--every", ""}, methods: []string{"PATCH " + schedPath},
			lastBody: `{"rrule":""}`},
		{name: "edit tz", args: []string{"edit", "TASK-5", "--tz", "Europe/Paris"}, methods: []string{"PATCH " + schedPath},
			lastBody: `{"tz":"Europe/Paris","floating":false}`},
		{name: "edit floating", args: []string{"edit", "TASK-5", "--floating"}, methods: []string{"PATCH " + schedPath},
			lastBody: `{"floating":true}`},
		{name: "edit soft deadline", args: []string{"edit", "TASK-5", "--deadline", "soft"}, methods: []string{"PATCH " + schedPath},
			lastBody: `{"kind":"deadline_soft"}`},
		{name: "rm yes", args: []string{"rm", "TASK-5", "--yes"}, methods: []string{"DELETE " + schedPath}, outHas: []string{"Deleted the schedule on TASK-5"}},
		{name: "rm confirmed", args: []string{"rm", "TASK-5"}, stdin: "y\n", methods: []string{"DELETE " + schedPath}, stderrHas: "[y/N]"},
		{name: "rm declined", args: []string{"rm", "TASK-5"}, stdin: "n\n", methods: nil, stderrHas: "Cancelled"},
		{name: "rm no answer", args: []string{"rm", "TASK-5"}, stdin: "", methods: nil, stderrHas: "Cancelled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, stderr, err, calls := runSched(t, viewOrResolved, tc.stdin, append([]string{"item", "remind"}, tc.args...)...)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, c := range calls {
				got = append(got, c.method+" "+c.path)
			}
			if strings.Join(got, "|") != strings.Join(tc.methods, "|") {
				t.Fatalf("calls=%v want %v", got, tc.methods)
			}
			if tc.lastBody != "" && calls[len(calls)-1].body != tc.lastBody {
				t.Fatalf("body=%s want %s", calls[len(calls)-1].body, tc.lastBody)
			}
			for _, w := range tc.outHas {
				if !strings.Contains(out, w) {
					t.Errorf("missing %q in %q", w, out)
				}
			}
			if !strings.Contains(stderr, tc.stderrHas) {
				t.Errorf("stderr=%q want %q", stderr, tc.stderrHas)
			}
		})
	}
}

func TestRemindUntilForms(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"snooze", "TASK-5", "--until", "2026-11-10"}, "bare date"},
		{[]string{"move", "TASK-5", "--until", "2026-11-10"}, "bare date"},
		{[]string{"move", "TASK-5", "--until", "2h"}, "snooze only"},
		{[]string{"snooze", "TASK-5", "--until", "soon"}, "--until"},
		{[]string{"snooze", "TASK-5"}, "--until is required"},
		{[]string{"done", "TASK-5", "--until", "2h"}, "unknown flag: --until"},
		{[]string{"done", "TASK-5", "--occurrence", "tomorrow"}, "--occurrence"},
	} {
		_, _, err, calls := runSched(t, okReply(weekdaysView), "", append([]string{"item", "remind"}, tc.args...)...)
		if err == nil || !strings.Contains(err.Error(), tc.want) || len(calls) != 0 {
			t.Fatalf("%v: err=%v calls=%+v", tc.args, err, calls)
		}
	}
	// A duration snoozes from now.
	before := time.Now()
	_, _, err, calls := runSched(t, func(c recordedCall) (int, string) {
		if strings.HasSuffix(c.path, "/resolve") {
			return 200, resolved
		}
		return 200, weekdaysView
	}, "", "item", "remind", "snooze", "TASK-5", "--until", "2h")
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Until string `json:"until"`
	}
	_ = json.Unmarshal([]byte(calls[1].body), &body)
	u, perr := time.Parse(time.RFC3339, body.Until)
	if perr != nil || u.Before(before.Add(2*time.Hour-time.Second)) || u.After(time.Now().Add(2*time.Hour+time.Second)) {
		t.Fatalf("until=%q", body.Until)
	}
}

func TestRemindStaleResolvePrintsCurrentOccurrence(t *testing.T) {
	stale := apiError(409, "stale", "That occurrence is no longer the outstanding one",
		`{"ended":false,"next_occurrence":{"at":"2026-11-10T14:00:00Z","wall":"2026-11-10T09:00:00"}}`)
	_, _, err, _ := runSched(t, stale, "", "item", "remind", "done", "TASK-5", "--occurrence", "2026-11-09T14:00:00Z")
	if err == nil {
		t.Fatal("want error")
	}
	for _, w := range []string{"That occurrence is no longer the outstanding one", "2026-11-10 09:00 EST", "--occurrence 2026-11-10T14:00:00Z"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("missing %q in %q", w, err)
		}
	}
	ended := apiError(409, "stale", "gone", `{"ended":true}`)
	_, _, err, _ = runSched(t, ended, "", "item", "remind", "done", "TASK-5", "--occurrence", "2026-11-09T14:00:00Z")
	if err == nil || !strings.Contains(err.Error(), "has ended") {
		t.Fatalf("err=%v", err)
	}
}

// cmdhelp keys commands by flat path, so a leaf under a command that also has
// RunE must still be addressable.
func TestRemindSubcommandsAreNestedLeaves(t *testing.T) {
	doc := cmdhelp.Build(newRootCmd(), newRootCmd(), cmdhelp.Options{MaxDepth: -1})
	for _, p := range []string{"item remind", "item remind show", "item remind done", "item remind skip", "item remind snooze", "item remind move", "item remind agenda", "item remind edit", "item remind rm"} {
		if _, ok := doc.Commands[p]; !ok {
			t.Errorf("%q missing from cmdhelp", p)
		}
	}
	if _, ok := doc.Commands["item remind"].Flags["start"]; !ok {
		t.Error("item remind has no --start")
	}
}
