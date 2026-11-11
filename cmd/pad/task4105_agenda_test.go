package main

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

const agendaBody = `{"from":"x","to":"y","tz":"America/New_York","entries":[
{"type":"schedule","at":"2026-11-01T05:30:00Z","day":"2026-11-01","state":"scheduled","item_ref":"TASK-1","item_title":"Early","roles":{}},
{"type":"schedule","at":"2026-11-01T06:30:00Z","day":"2026-11-01","state":"outstanding","item_ref":"TASK-2","item_title":"Late","roles":{"deadline":"2031-03-10T09:00:00Z"},"overdue":true,"missed_count":2},
{"type":"schedule","at":"2026-11-02T14:00:00Z","day":"2026-11-02","state":"scheduled","item_ref":"TASK-3","item_title":"Next day","roles":{"occupied_until":"2031-03-10T10:00:00Z"}},
{"type":"date","at":"2026-11-03T05:00:00Z","day":"2026-11-03","date":"2026-11-03","field":"due_date","item_ref":"TASK-4","item_title":"Dated","collection":"tasks"}]}`

func agendaCall(t *testing.T, calls []recordedCall) url.Values {
	t.Helper()
	for _, c := range calls {
		if c.method == "GET" && c.path == "/workspaces/test/agenda" {
			q, _ := url.ParseQuery(c.query)
			return q
		}
	}
	t.Fatalf("no agenda request: %+v", calls)
	return nil
}

func TestAgendaRendersInUserZoneOnUTCBox(t *testing.T) {
	for _, machine := range []*time.Location{time.UTC, time.FixedZone("Machine", 9*3600)} {
		t.Run(machine.String(), func(t *testing.T) {
			h, calls := recordingStub(func(c recordedCall) (int, string) { return 200, agendaBody })
			root := tzCLIWithLocal(t, h, machine)
			out, stderr, err := tzRun(t, root, "item", "remind", "agenda", "--from", "2026-11-01")
			if err != nil || stderr != "" {
				t.Fatalf("err=%v stderr=%q", err, stderr)
			}
			for _, want := range []string{"01:30 EDT", "01:30 EST", "09:00 EST", "all day", "2026-11-02", "overdue,missed"} {
				if !strings.Contains(out, want) {
					t.Errorf("missing %q in:\n%s", want, out)
				}
			}
			for _, bad := range []string{"UTC", "Machine", "+09"} {
				if strings.Contains(out, bad) {
					t.Errorf("printed %q in:\n%s", bad, out)
				}
			}
			q := agendaCall(t, calls())
			// The New York day starts at 04:00Z; seven calendar days on is
			// 05:00Z because the clocks went back on 2026-11-01.
			if q.Get("from") != "2026-11-01T04:00:00Z" || q.Get("to") != "2026-11-08T05:00:00Z" || q.Has("tz") {
				t.Fatalf("query=%v", q)
			}
		})
	}
}

func TestAgendaDefaultWindowStartsToday(t *testing.T) {
	loc, _ := time.LoadLocation("America/New_York")
	day := func() time.Time {
		n := time.Now().In(loc)
		return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, loc).UTC()
	}
	h, calls := recordingStub(func(c recordedCall) (int, string) { return 200, `{"window_complete":true,"entries":[]}` })
	root := tzCLIWithLocal(t, h, time.UTC)
	before := day()
	out, _, err := tzRun(t, root, "item", "remind", "agenda")
	after := day()
	q := agendaCall(t, calls())
	if err != nil || !strings.Contains(out, "Nothing on the agenda") {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if got := q.Get("from"); got != before.Format(time.RFC3339) && got != after.Format(time.RFC3339) {
		t.Fatalf("from=%s want start of today in New York (%s)", got, before.Format(time.RFC3339))
	}
}

func TestAgendaWithoutTimezoneSettingNotesUTC(t *testing.T) {
	h, calls := recordingStub(func(c recordedCall) (int, string) { return 200, agendaBody })
	root := tzCLIWithLocal(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/auth/me") {
			fmt.Fprint(w, `{"name":"Viewer"}`)
			return
		}
		h(w, r)
	}, time.FixedZone("Machine", 9*3600))
	out, stderr, err := tzRun(t, root, "item", "remind", "agenda", "--from", "2026-11-01")
	if err != nil || stderr != tzNotice {
		t.Fatalf("err=%v stderr=%q", err, stderr)
	}
	if !strings.Contains(out, "05:30 UTC") || strings.Contains(out, "Machine") {
		t.Fatalf("out:\n%s", out)
	}
	if q := agendaCall(t, calls()); q.Get("from") != "2026-11-01T00:00:00Z" {
		t.Fatalf("query=%v", q)
	}
}

func TestAgendaWindowFlags(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--days", "93"}, "capped at 92"},
		{[]string{"--days", "0"}, "capped at 92"},
		{[]string{"--days", "3", "--to", "2026-12-01"}, "mutually exclusive"},
		{[]string{"--from", "2026-11-01", "--to", "2027-03-01"}, "capped at 92"},
		{[]string{"--from", "yesterday"}, "--from"},
		{[]string{"--tz", "Local"}, "--tz"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			h, calls := recordingStub(func(c recordedCall) (int, string) { return 200, `{"window_complete":true,"entries":[]}` })
			root := tzCLIWithLocal(t, h, time.UTC)
			_, _, err := tzRun(t, root, append([]string{"item", "remind", "agenda"}, tc.args...)...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%v: err=%v", tc.args, err)
			}
			for _, c := range calls() {
				if strings.Contains(c.path, "agenda") {
					t.Fatalf("%v reached the server", tc.args)
				}
			}
		})
	}
	// 92 days exactly is allowed, and --tz is forwarded.
	t.Run("ninety-two days", func(t *testing.T) {
		h, calls := recordingStub(func(c recordedCall) (int, string) { return 200, `{"window_complete":true,"entries":[]}` })
		root := tzCLIWithLocal(t, h, time.UTC)
		if _, _, err := tzRun(t, root, "item", "remind", "agenda", "--from", "2026-11-01", "--days", "92", "--tz", "Asia/Tokyo"); err != nil {
			t.Fatal(err)
		}
		q := agendaCall(t, calls())
		if q.Get("tz") != "Asia/Tokyo" || q.Get("from") != "2026-10-31T15:00:00Z" {
			t.Fatalf("query=%v", q)
		}
	})
}

func TestAgendaTruncationHints(t *testing.T) {
	entry := `"entries":[{"type":"schedule","at":"2026-11-02T14:00:00Z","day":"2026-11-02","state":"scheduled","item_ref":"TASK-3","item_title":"T","roles":{}}]`
	unexpanded := `"unexpanded":[{"schedule_id":"s1","item_ref":"TASK-8","item_title":"Busy","collection":"tasks","roles":{},"audience":"agent","reason":"cap_instances"}]`
	unavailable := `"unavailable":[{"item_ref":"TASK-9","code":"cap_time"}]`
	for _, tc := range []struct {
		name, body string
		want, not  []string
	}{
		{"next_from", `{` + entry + `,"truncated":true,"next_from":"2026-11-05T05:00:00Z"}`,
			[]string{"more after 2026-11-05 00:00 EST: pad item remind agenda --from 2026-11-05T05:00:00Z --to 2026-11-08T05:00:00Z"}, []string{"incomplete"}},
		{"next_from is the window end (only unexpanded schedules cut)", `{` + entry + `,"truncated":true,"next_from":"2026-11-08T05:00:00Z",` + unexpanded + `}`,
			[]string{"TASK-8  agent  cap_instances"}, []string{"more after", "--from", "incomplete"}},
		{"no next_from", `{` + entry + `,"truncated":true}`,
			[]string{"list incomplete; narrow the window"}, []string{"more after"}},
		{"unexpanded", `{` + entry + `,` + unexpanded + `}`,
			[]string{"TASK-8  agent  cap_instances"}, []string{"incomplete", "more after"}},
		{"unavailable", `{` + entry + `,` + unavailable + `}`,
			[]string{"TASK-9  unavailable  cap_time"}, []string{"incomplete", "more after"}},
		{"none", `{` + entry + `}`, []string{"TASK-3"}, []string{"incomplete", "more after", "unavailable", "cap_"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := recordingStub(func(c recordedCall) (int, string) { return 200, tc.body })
			root := tzCLIWithLocal(t, h, time.UTC)
			out, _, err := tzRun(t, root, "item", "remind", "agenda", "--from", "2026-11-01")
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("missing %q in:\n%s", w, out)
				}
			}
			for _, n := range tc.not {
				if strings.Contains(out, n) {
					t.Errorf("unexpected %q in:\n%s", n, out)
				}
			}
		})
	}
}

func TestAgendaJSONIsVerbatim(t *testing.T) {
	body := `{"window_complete":true,"entries":[],"future_field":{"a":1}}`
	h, _ := recordingStub(func(c recordedCall) (int, string) { return 200, body })
	root := tzCLIWithLocal(t, h, time.UTC)
	out, stderr, err := tzRun(t, root, "item", "remind", "agenda", "--from", "2026-11-01", "--format", "json")
	if err != nil || stderr != "" || !strings.Contains(out, `"future_field"`) {
		t.Fatalf("out=%q stderr=%q err=%v", out, stderr, err)
	}
}

func TestAgendaClampIsNotSilent(t *testing.T) {
	run := func(t *testing.T, args ...string) (stderr string, q url.Values) {
		t.Helper()
		h, calls := recordingStub(func(c recordedCall) (int, string) { return 200, `{"window_complete":true,"entries":[]}` })
		root := tzCLIWithLocal(t, h, time.UTC)
		_, stderr, err := tzRun(t, root, append([]string{"item", "remind", "agenda"}, args...)...)
		if err != nil {
			t.Fatal(err)
		}
		return stderr, agendaCall(t, calls())
	}
	t.Run("fall-back shortens the window", func(t *testing.T) {
		stderr, q := run(t, "--from", "2026-11-01", "--days", "92", "--tz", "America/New_York")
		if q.Get("to") != "2027-02-01T04:00:00Z" {
			t.Fatalf("to=%s", q.Get("to"))
		}
		for _, w := range []string{"92-day limit", "2027-01-31 23:00 EST"} {
			if !strings.Contains(stderr, w) {
				t.Errorf("stderr %q missing %q", stderr, w)
			}
		}
	})
	t.Run("no DST change, no note", func(t *testing.T) {
		stderr, _ := run(t, "--from", "2026-11-01", "--days", "92", "--tz", "Asia/Tokyo")
		if strings.Contains(stderr, "92-day") {
			t.Fatalf("unexpected note: %q", stderr)
		}
	})
}

func TestAgendaResumeHintKeepsWindowAndZone(t *testing.T) {
	body := `{"window_complete":true,"entries":[],"truncated":true,"next_from":"2026-11-05T05:00:00Z"}`
	h, calls := recordingStub(func(c recordedCall) (int, string) { return 200, body })
	root := tzCLIWithLocal(t, h, time.UTC)
	out, _, err := tzRun(t, root, "item", "remind", "agenda", "--from", "2026-11-01", "--days", "30", "--tz", "Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	to := agendaCall(t, calls()).Get("to")
	want := "pad item remind agenda --from 2026-11-05T05:00:00Z --to " + to + " --tz Asia/Tokyo"
	if !strings.Contains(out, want) || strings.Contains(out, "--days") {
		t.Fatalf("want %q in:\n%s", want, out)
	}
	if _, err := time.Parse(time.RFC3339, to); err != nil {
		t.Fatal(err)
	}
}

func TestAgendaResumeHintWithoutTzHasNone(t *testing.T) {
	body := `{"window_complete":true,"entries":[],"truncated":true,"next_from":"2026-11-05T05:00:00Z"}`
	h, _ := recordingStub(func(c recordedCall) (int, string) { return 200, body })
	out, _, err := tzRun(t, tzCLIWithLocal(t, h, time.UTC), "item", "remind", "agenda", "--from", "2026-11-01")
	if err != nil || strings.Contains(out, "--tz") || !strings.Contains(out, " --to ") {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

// The request sends the window end in whole seconds, so the server's next_from
// can equal that truncated end while the user typed a fraction of a second more.
// The comparison that decides whether there is anything to resume uses the end
// as sent.
func TestAgendaNoResumeHintWhenNextFromIsTheSentEnd(t *testing.T) {
	body := `{"window_complete":true,"entries":[],"truncated":true,"next_from":"2026-11-08T05:00:00Z",` +
		`"unexpanded":[{"schedule_id":"s1","item_ref":"TASK-8","item_title":"Busy","collection":"tasks","roles":{},"audience":"agent","reason":"scan_limit"}]}`
	h, _ := recordingStub(func(c recordedCall) (int, string) { return 200, body })
	root := tzCLIWithLocal(t, h, time.UTC)
	out, _, err := tzRun(t, root, "item", "remind", "agenda", "--from", "2026-11-01T05:00:00Z", "--to", "2026-11-08T05:00:00.5Z")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "more after") || strings.Contains(out, "--from") {
		t.Errorf("a resume command was printed for a cut at the window end:\n%s", out)
	}
	if !strings.Contains(out, "TASK-8  agent  scan_limit") {
		t.Errorf("the unexpanded schedule is not named:\n%s", out)
	}
}

func TestAgendaLabelsFromRoles(t *testing.T) {
	h, _ := recordingStub(func(c recordedCall) (int, string) {
		return 200, `{"tz":"UTC","entries":[{"type":"schedule","at":"2026-11-05T09:00:00Z","day":"2026-11-05","state":"outstanding","item_ref":"TASK-1","item_title":"Appointment","roles":{"target":"2026-11-05T09:00:00Z","occupied_until":"2026-11-05T10:00:00Z"}}]}`
	})
	out, _, err := tzRun(t, tzCLIWithLocal(t, h, time.UTC), "item", "remind", "agenda")
	if err != nil || !strings.Contains(out, "window") {
		t.Fatalf("role label missing: %s %v", out, err)
	}
}
