package main

import (
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/schedule"
)

func TestRemindParseTable(t *testing.T) {
	ok := func(f scheduleFlags) scheduleFlags {
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
		in   scheduleFlags
		want want
	}{
		{"plain check", ok(scheduleFlags{}), want{kind: "check", dtstart: "2026-11-09T09:00:00"}},
		{"all-day", scheduleFlags{Start: "2026-11-09"}, want{kind: "check", dtstart: "2026-11-09"}},
		{"raw weekly", ok(scheduleFlags{Every: "FREQ=WEEKLY;BYDAY=MO"}), want{kind: "check", dtstart: "2026-11-09T09:00:00", rrule: "FREQ=WEEKLY;BYDAY=MO"}},
		{"RRULE prefix", ok(scheduleFlags{Every: "RRULE:FREQ=DAILY"}), want{kind: "check", dtstart: "2026-11-09T09:00:00", rrule: "FREQ=DAILY"}},
		{"surrounding space", ok(scheduleFlags{Every: "  FREQ=DAILY  "}), want{kind: "check", dtstart: "2026-11-09T09:00:00", rrule: "FREQ=DAILY"}},
		{"mixed case", ok(scheduleFlags{Every: "Weekdays"}), want{kind: "check", dtstart: "2026-11-09T09:00:00", rrule: "FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR"}},
		{"hard", ok(scheduleFlags{Deadline: "hard"}), want{kind: "deadline_hard", dtstart: "2026-11-09T09:00:00"}},
		{"soft", ok(scheduleFlags{Deadline: "soft"}), want{kind: "deadline_soft", dtstart: "2026-11-09T09:00:00"}},
		{"window 90m", ok(scheduleFlags{Window: "90m"}), want{kind: "window", dtstart: "2026-11-09T09:00:00", dtend: "2026-11-09T10:30:00"}},
		{"window 2h", ok(scheduleFlags{Window: "2h"}), want{kind: "window", dtstart: "2026-11-09T09:00:00", dtend: "2026-11-09T11:00:00"}},
		{"window 3d", ok(scheduleFlags{Window: "3d"}), want{kind: "window", dtstart: "2026-11-09T09:00:00", dtend: "2026-11-12T09:00:00"}},
		{"window end wall", ok(scheduleFlags{Window: "2026-11-09T17:00"}), want{kind: "window", dtstart: "2026-11-09T09:00:00", dtend: "2026-11-09T17:00:00"}},
		{"all-day window days", scheduleFlags{Start: "2026-11-09", Window: "3d"}, want{kind: "window", dtstart: "2026-11-09", dtend: "2026-11-12"}},
		{"tz", ok(scheduleFlags{Tz: "Europe/Paris"}), want{kind: "check", dtstart: "2026-11-09T09:00:00", tz: "Europe/Paris"}},
		{"floating", ok(scheduleFlags{Floating: true}), want{kind: "check", dtstart: "2026-11-09T09:00:00", floating: true}},
		{"anchor audience", ok(scheduleFlags{Anchor: "completion", Audience: "agent"}), want{kind: "check", dtstart: "2026-11-09T09:00:00", anchor: "completion", audience: "agent"}},
	}
	for _, tc := range good {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildScheduleCreate(tc.in)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Kind != tc.want.kind || got.Dtstart != tc.want.dtstart || got.Dtend != tc.want.dtend ||
				got.Rrule != tc.want.rrule || got.Tz != tc.want.tz || got.Floating != tc.want.floating ||
				got.Anchor != tc.want.anchor || got.Audience != tc.want.audience {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
	// Every shorthand maps to the canonical rule, whatever the case.
	for _, name := range schedule.ShorthandNames() {
		rule, _ := schedule.Shorthand(name)
		got, err := buildScheduleCreate(ok(scheduleFlags{Every: strings.ToUpper(name)}))
		if err != nil || got.Rrule != rule {
			t.Fatalf("shorthand %s: rule=%q want %q err=%v", name, got.Rrule, rule, err)
		}
	}

	bad := []struct {
		name string
		in   scheduleFlags
		frag string
	}{
		{"garbage rule lists names", ok(scheduleFlags{Every: "fortnightly"}), "weekdays"},
		{"long rule", ok(scheduleFlags{Every: "FREQ=DAILY;BYDAY=" + strings.Repeat("MO,", 400) + "MO"}), "--every"},
		{"missing start", scheduleFlags{Every: "daily"}, "--start is required"},
		{"Z suffix", scheduleFlags{Start: "2026-11-09T09:00:00Z"}, "--start"},
		{"offset", scheduleFlags{Start: "2026-11-09T09:00:00+02:00"}, "--start"},
		{"junk deadline", ok(scheduleFlags{Deadline: "firm"}), "--deadline"},
		{"deadline and window", ok(scheduleFlags{Deadline: "hard", Window: "2h"}), "--deadline"},
		{"junk window", ok(scheduleFlags{Window: "soon"}), "--window"},
		{"hours window on all-day", scheduleFlags{Start: "2026-11-09", Window: "2h"}, "--window"},
		{"tz with floating", ok(scheduleFlags{Tz: "Europe/Paris", Floating: true}), "mutually exclusive"},
		{"bad tz", ok(scheduleFlags{Tz: "Mars/Base"}), "--tz"},
		{"Local tz", ok(scheduleFlags{Tz: "Local"}), "--tz"},
		{"bad audience", ok(scheduleFlags{Audience: "robots"}), "--audience"},
		{"bad anchor", ok(scheduleFlags{Anchor: "never"}), "--anchor"},
		{"remind-at", ok(scheduleFlags{RemindAt: "2026-11-09T09:00:00Z"}), "--start"},
		{"rearm", ok(scheduleFlags{Rearm: "r1"}), "--start"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildScheduleCreate(tc.in)
			if err == nil || !strings.Contains(err.Error(), tc.frag) {
				t.Fatalf("want error containing %q, got %v", tc.frag, err)
			}
		})
	}
}
