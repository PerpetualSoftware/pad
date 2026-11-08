package schedule

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestRemindParseTable(t *testing.T) {
	ok := func(f CommitmentFlags) CommitmentFlags {
		if f.Start == "" {
			f.Start = "2026-11-09T09:00"
		}
		return f
	}
	type want struct {
		kind, dtstart, dtend, rrule, tz, anchor, audience string
		floating                                          bool
	}
	good := []struct {
		name string
		in   CommitmentFlags
		want want
	}{
		{"plain check", ok(CommitmentFlags{}), want{kind: "check", dtstart: "2026-11-09T09:00:00"}},
		{"all-day", CommitmentFlags{Start: "2026-11-09"}, want{kind: "check", dtstart: "2026-11-09"}},
		{"raw weekly", ok(CommitmentFlags{Every: "FREQ=WEEKLY;BYDAY=MO"}), want{kind: "check", dtstart: "2026-11-09T09:00:00", rrule: "FREQ=WEEKLY;BYDAY=MO"}},
		{"RRULE prefix", ok(CommitmentFlags{Every: "RRULE:FREQ=DAILY"}), want{kind: "check", dtstart: "2026-11-09T09:00:00", rrule: "FREQ=DAILY"}},
		{"surrounding space", ok(CommitmentFlags{Every: "  FREQ=DAILY  "}), want{kind: "check", dtstart: "2026-11-09T09:00:00", rrule: "FREQ=DAILY"}},
		{"mixed case", ok(CommitmentFlags{Every: "Weekdays"}), want{kind: "check", dtstart: "2026-11-09T09:00:00", rrule: "FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR"}},
		{"hard", ok(CommitmentFlags{Deadline: "hard"}), want{kind: "deadline_hard", dtstart: "2026-11-09T09:00:00"}},
		{"soft", ok(CommitmentFlags{Deadline: "soft"}), want{kind: "check", dtstart: "2026-11-09T09:00:00"}},
		{"window 90m", ok(CommitmentFlags{Window: "90m"}), want{kind: "window", dtstart: "2026-11-09T09:00:00", dtend: "2026-11-09T10:30:00"}},
		{"window 2h", ok(CommitmentFlags{Window: "2h"}), want{kind: "window", dtstart: "2026-11-09T09:00:00", dtend: "2026-11-09T11:00:00"}},
		{"window 3d", ok(CommitmentFlags{Window: "3d"}), want{kind: "window", dtstart: "2026-11-09T09:00:00", dtend: "2026-11-12T09:00:00"}},
		{"window end wall", ok(CommitmentFlags{Window: "2026-11-09T17:00"}), want{kind: "window", dtstart: "2026-11-09T09:00:00", dtend: "2026-11-09T17:00:00"}},
		{"all-day window days", CommitmentFlags{Start: "2026-11-09", Window: "3d"}, want{kind: "window", dtstart: "2026-11-09", dtend: "2026-11-12"}},
		{"tz", ok(CommitmentFlags{Tz: "Europe/Paris"}), want{kind: "check", dtstart: "2026-11-09T09:00:00", tz: "Europe/Paris"}},
		{"floating", ok(CommitmentFlags{Floating: true}), want{kind: "check", dtstart: "2026-11-09T09:00:00", floating: true}},
		{"anchor audience", ok(CommitmentFlags{Anchor: "completion", Audience: "agent"}), want{kind: "check", dtstart: "2026-11-09T09:00:00", anchor: "completion", audience: "agent"}},
	}
	for _, tc := range good {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CommitmentBody(tc.in, nil, "")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			roles := got["roles"].(map[string]any)
			end := ""
			if days, ok := roles["occupied_days"].(int); ok {
				start, _ := ParseWall(got["dtstart"].(string))
				at := start.naive().AddDate(0, 0, days).Add(time.Duration(roles["occupied_secs"].(int)) * time.Second)
				end = at.Format("2006-01-02T15:04:05")
				if start.AllDay {
					end = at.Format("2006-01-02")
				}
			}
			text := func(key string) string { value, _ := got[key].(string); return value }
			floating, _ := got["floating"].(bool)
			if CommitmentKind(roles["deadline_days"] != nil, roles["occupied_days"] != nil) != tc.want.kind || text("dtstart") != tc.want.dtstart || end != tc.want.dtend ||
				text("rrule") != tc.want.rrule || text("tz") != tc.want.tz || floating != tc.want.floating || text("anchor") != tc.want.anchor || text("audience") != tc.want.audience {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			if roles["target_days"] != 0 || roles["target_secs"] != 0 {
				t.Fatal("target must map to 0/0")
			}
			if _, hard := roles["deadline_days"]; hard && (roles["deadline_days"] != 0 || roles["deadline_secs"] != 0 || roles["deadline_behavior"] != "stays_owed") {
				t.Fatal("hard deadline must map to 0/0 stays_owed")
			}
			for _, key := range []string{"kind", "dtend"} {
				if _, ok := got[key]; ok {
					t.Fatalf("forbidden member %s", key)
				}
			}

		})
	}
	// Every shorthand maps to the canonical rule, whatever the case.
	for _, name := range ShorthandNames() {
		rule, _ := Shorthand(name)
		got, err := CommitmentBody(ok(CommitmentFlags{Every: strings.ToUpper(name)}), nil, "")
		if err != nil || got["rrule"] != rule {
			t.Fatalf("shorthand %s: rule=%q want %q err=%v", name, got["rrule"], rule, err)
		}
	}

	bad := []struct {
		name string
		in   CommitmentFlags
		frag string
	}{
		{"garbage rule lists names", ok(CommitmentFlags{Every: "fortnightly"}), "weekdays"},
		{"long rule", ok(CommitmentFlags{Every: "FREQ=DAILY;BYDAY=" + strings.Repeat("MO,", 400) + "MO"}), "--every"},
		{"missing start", CommitmentFlags{Every: "daily"}, "--start is required"},
		{"Z suffix", CommitmentFlags{Start: "2026-11-09T09:00:00Z"}, "--start"},
		{"offset", CommitmentFlags{Start: "2026-11-09T09:00:00+02:00"}, "--start"},
		{"junk deadline", ok(CommitmentFlags{Deadline: "firm"}), "--deadline"},
		{"deadline and window", ok(CommitmentFlags{Deadline: "hard", Window: "2h"}), "--deadline"},
		{"junk window", ok(CommitmentFlags{Window: "soon"}), "--window"},
		{"hours window on all-day", CommitmentFlags{Start: "2026-11-09", Window: "2h"}, "--window"},
		{"tz with floating", ok(CommitmentFlags{Tz: "Europe/Paris", Floating: true}), "mutually exclusive"},
		{"bad tz", ok(CommitmentFlags{Tz: "Mars/Base"}), "--tz"},
		{"Local tz", ok(CommitmentFlags{Tz: "Local"}), "--tz"},
		{"bad audience", ok(CommitmentFlags{Audience: "robots"}), "--audience"},
		{"bad anchor", ok(CommitmentFlags{Anchor: "never"}), "--anchor"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CommitmentBody(tc.in, nil, "")
			if err == nil || !strings.Contains(err.Error(), tc.frag) {
				t.Fatalf("want error containing %q, got %v", tc.frag, err)
			}
		})
	}
}

func TestCommitmentEditRolesBody(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags CommitmentFlags
		want  string
	}{
		{"hard", CommitmentFlags{Deadline: "hard"}, `{"roles":{"deadline_behavior":"stays_owed","deadline_days":0,"deadline_secs":0}}`},
		{"soft", CommitmentFlags{Deadline: "soft"}, `{"roles":{"deadline_behavior":null,"deadline_days":null,"deadline_secs":null}}`},
		{"none", CommitmentFlags{Deadline: "none"}, `{"roles":{"deadline_behavior":null,"deadline_days":null,"deadline_secs":null}}`},
		{"window", CommitmentFlags{Window: "2h"}, `{"roles":{"occupied_days":0,"occupied_secs":7200}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			given := func(n string) bool {
				return n == "deadline" && tc.flags.Deadline != "" || n == "window" && tc.flags.Window != ""
			}
			body, err := CommitmentBody(tc.flags, given, "2099-01-01T09:00")
			raw, _ := json.Marshal(body)
			if err != nil || string(raw) != tc.want {
				t.Fatalf("body=%s err=%v want %s", raw, err, tc.want)
			}
		})
	}
}
