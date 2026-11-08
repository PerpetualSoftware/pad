package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/PerpetualSoftware/pad/internal/schedule"
)

// Schedules ride `pad item remind`: the flags below switch it from the
// one-shot reminder to a schedule, and the subcommands resolve and inspect one.
// The rule grammar is internal/schedule's; this file only turns flags into the
// request and renders the answer. Rules the server alone can judge (the firing
// floor, anchor completion with a BYxxx part, an occurrence that never comes)
// are not copied here: the server's message is passed through unchanged.

// scheduleFlags are the schedule-mode flags of `pad item remind` and
// `pad item remind edit`. RemindAt and Rearm belong to the one-shot path; they
// are carried only so a mixed call is refused with a pointer to --start.
type scheduleFlags struct {
	Every, Start, Deadline, Window, Anchor, Audience, Tz string
	Floating                                             bool
	RemindAt, Rearm                                      string
}

const scheduleFlagHelp = `
  --start     wall time the first occurrence is read at: 2026-11-09T09:00, or 2026-11-09 for an all-day date (no Z, no offset)
  --every     cadence: daily, weekdays, weekly, biweekly, monthly, monthly-last, quarterly, yearly, or an RRULE such as FREQ=WEEKLY;BYDAY=MO
  --deadline  hard: a deadline that stays owed; soft or none (edit): no deadline, and a window stays
  --window    a span from --start: a duration (90m, 2h, 3d) or an end wall (2026-11-09T17:00)
  --anchor    calendar (default) | completion (next one an interval after you mark it done)
  --audience  human | agent | both (default both)
  --tz        IANA zone the wall is read in (default: your account zone)
  --floating  read the wall in the zone of whoever it fires for; exclusive with --tz`

func bindScheduleFlags(cmd *cobra.Command, f *scheduleFlags) {
	cmd.Flags().StringVar(&f.Start, "start", "", "schedule: wall time, 2026-11-09T09:00 or 2026-11-09 (no Z or offset)")
	cmd.Flags().StringVar(&f.Every, "every", "", "schedule: daily|weekdays|weekly|biweekly|monthly|monthly-last|quarterly|yearly, or an RRULE")
	cmd.Flags().StringVar(&f.Deadline, "deadline", "", "schedule: hard sets a deadline; soft or none (edit) clears it and keeps any window")
	cmd.Flags().StringVar(&f.Window, "window", "", "schedule: a span from --start, a duration (90m, 2h, 3d) or an end wall")
	cmd.Flags().StringVar(&f.Anchor, "anchor", "", "schedule: calendar|completion")
	cmd.Flags().StringVar(&f.Audience, "audience", "", "schedule: human|agent|both")
	cmd.Flags().StringVar(&f.Tz, "tz", "", "schedule: IANA zone the wall is read in (default: your account zone)")
	cmd.Flags().BoolVar(&f.Floating, "floating", false, "schedule: read the wall in the recipient's zone; exclusive with --tz")
}

var scheduleModeFlagNames = []string{"every", "start", "deadline", "window", "anchor", "audience", "tz", "floating"}

func scheduleModeRequested(cmd *cobra.Command) bool {
	for _, n := range scheduleModeFlagNames {
		if cmd.Flags().Changed(n) {
			return true
		}
	}
	return false
}

func commitmentFlags(f scheduleFlags) schedule.CommitmentFlags {
	return schedule.CommitmentFlags{Every: f.Every, Start: f.Start, Deadline: f.Deadline, Window: f.Window, Anchor: f.Anchor, Audience: f.Audience, Tz: f.Tz, Floating: f.Floating}
}

func buildScheduleCreate(f scheduleFlags) (map[string]any, error) {
	if f.RemindAt != "" || f.Rearm != "" {
		return nil, fmt.Errorf("--remind-at and --rearm arm a one-shot reminder and cannot be combined with schedule flags; give the time with --start")
	}
	return schedule.CommitmentBody(commitmentFlags(f), nil, "")
}

func buildScheduleUpdate(f scheduleFlags, changed func(string) bool, currentStart string) (map[string]any, error) {
	return schedule.CommitmentBody(commitmentFlags(f), changed, currentStart)
}
