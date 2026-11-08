package store

import (
	"reflect"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestNativeRolesDeriveKindAndEnd(t *testing.T) {
	for _, door := range []string{"create", "edit"} {
		for _, prior := range []string{"check", "deadline_soft", "deadline_hard", "window"} {
			for _, deadline := range []bool{false, true} {
				for _, occupied := range []bool{false, true} {
					name := door + "/" + prior
					if deadline {
						name += "/deadline"
					}
					if occupied {
						name += "/occupied"
					}
					t.Run(name, func(t *testing.T) {
						f := newSchedFixture(t)
						roles := models.CommitmentRolesInput{}
						if deadline {
							roles.DeadlineDays = i(1)
							roles.DeadlineSecs = i(0)
							roles.DeadlineBehavior = ptr("stays_owed")
						}
						if occupied {
							roles.OccupiedDays = i(0)
							roles.OccupiedSecs = i(7200)
						}
						var sc *models.ItemSchedule
						var err error
						if door == "create" {
							sc, err = f.s.createScheduleRolesAt(f.ws.ID, f.item.ID, models.ScheduleCreate{Kind: prior, Dtstart: "2026-10-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"}, ptr(mustRoles(t, roles)), schedNow)
						} else {
							sc = f.commitFixture(t, prior)
							f.exec(t, `UPDATE item_schedules SET next_override_at=? WHERE id=?`, "2026-10-27T09:00:00Z", sc.ID)
							f.deadlineOverride(t, sc, "2026-10-28T09:00:00Z")
							overridesBefore, readErr := f.s.occurrenceOverrides(f.s.db, sc.ID, deadlineKey)
							if readErr != nil {
								t.Fatal(readErr)
							}
							before := f.reload(t, f.item.ID)
							reminder := f.reminder(t, *sc.ReminderID)
							sc, err = f.s.SetCommitmentRoles(f.ws.ID, f.item.ID, roles)
							if err == nil {
								if !reflect.DeepEqual(before.NextAt, sc.NextAt) || !reflect.DeepEqual(before.NextOverrideAt, sc.NextOverrideAt) || !reflect.DeepEqual(reminder, f.reminder(t, *sc.ReminderID)) {
									t.Fatal("kind derivation moved occurrence or reminder")
								}
								overridesAfter, readErr := f.s.occurrenceOverrides(f.s.db, sc.ID, deadlineKey)
								if readErr != nil || !reflect.DeepEqual(overridesBefore, overridesAfter) {
									t.Fatal("kind derivation changed overrides")
								}
							}
						}
						if err != nil {
							t.Fatal(err)
						}
						want := "check"
						end := ""
						if door == "edit" && prior == "deadline_soft" {
							want = "deadline_soft"
						}
						if occupied {
							want = "window"
							end = "2026-10-01T11:00:00"
						}
						if deadline {
							want = "deadline_hard"
							end = ""
						}
						if sc.Kind != want || str(sc.Dtend) != end {
							t.Fatalf("kind/end=%s/%s want %s/%s", sc.Kind, str(sc.Dtend), want, end)
						}
						got, err := f.s.GetCommitment(f.ws.ID, f.item.ID)
						if err != nil {
							t.Fatal(err)
						}
						if occupied && got.Roles.OccupiedUntil == nil {
							t.Fatal("occupied date lost")
						}
						if door == "edit" && prior == "deadline_soft" && !deadline && !occupied {
							roles.Outcome = "another edit"
							edited, err := f.s.SetCommitmentRoles(f.ws.ID, f.item.ID, roles)
							if err != nil || edited.Kind != "deadline_soft" {
								t.Fatalf("native soft label changed: %+v %v", edited, err)
							}
						}
					})
				}
			}
		}
	}
}

func TestNativeDeadlineAddedThenRenegotiatedAway(t *testing.T) {
	for _, occupied := range []bool{false, true} {
		t.Run(map[bool]string{false: "check", true: "window"}[occupied], func(t *testing.T) {
			f := newSchedFixture(t)
			sc := f.commitFixture(t, "check")
			roles := models.CommitmentRolesInput{DeadlineDays: i(1), DeadlineSecs: i(0), DeadlineBehavior: ptr("stays_owed")}
			if occupied {
				roles.OccupiedDays = i(0)
				roles.OccupiedSecs = i(7200)
			}
			sc, err := f.s.SetCommitmentRoles(f.ws.ID, f.item.ID, roles)
			if err != nil {
				t.Fatal(err)
			}
			if sc.Kind != "deadline_hard" {
				t.Fatalf("added deadline label: %s", sc.Kind)
			}
			before := f.reload(t, f.item.ID)
			reminder := f.reminder(t, *sc.ReminderID)
			in := models.CommitmentRenegotiateInput{Scope: "series", Reason: "agreed", RemoveDeadline: true, Preview: true}
			p, err := f.s.renegotiateCommitmentAt(f.ws.ID, f.item.ID, "user", in, schedNow, nil)
			if err != nil {
				t.Fatal(err)
			}
			in.Preview, in.Revision = false, p.Revision
			if _, err = f.s.renegotiateCommitmentAt(f.ws.ID, f.item.ID, "user", in, schedNow, nil); err != nil {
				t.Fatal(err)
			}
			sc = f.reload(t, f.item.ID)
			want := "check"
			if occupied {
				want = "window"
			}
			if sc.Kind != want {
				t.Fatalf("removed deadline label: %s want %s", sc.Kind, want)
			}
			if !reflect.DeepEqual(before.NextAt, sc.NextAt) || !reflect.DeepEqual(reminder, f.reminder(t, *sc.ReminderID)) {
				t.Fatal("renegotiation moved trigger")
			}
			last := f.journal(t, sc.ID)
			row := last[len(last)-1]
			if row.Role == nil || *row.Role != "deadline" {
				t.Fatalf("journal: %+v", row)
			}
		})
	}
}

func TestNativeCarryUsesRolesWriter(t *testing.T) {
	c := newSchedCopy(t)
	sc := c.scheduleFuture(t, daily())
	// Drifted native labels correct on a roles write, including a native create
	// through the shared carry door. The source remains untouched.
	c.exec(t, `UPDATE item_schedules SET deadline_days=1,deadline_secs=0,deadline_behavior='stays_owed',roles_origin='native' WHERE id=?`, sc.ID)
	result := c.do(t, false)
	dst := c.sched(t, c.wsB.ID, result.Item.ID)
	if dst.Kind != "deadline_hard" || dst.Dtend != nil {
		t.Fatalf("carried label/end: %+v", dst)
	}
	if c.sched(t, c.wsA.ID, c.src.ID).Kind != "check" {
		t.Fatal("carry changed source label")
	}
}

func TestNativeCarryKeepsBundleFieldValidation(t *testing.T) {
	for _, tc := range []struct{ field, value, reason string }{
		{"kind", "bogus", "kind:enum"},
		{"dtend", "2026-10-01T11:00:00", "dtend:combination"},
	} {
		t.Run(tc.field, func(t *testing.T) {
			f, b, row := nativeBundle(t)
			row[tc.field] = tc.value
			out, report := restoredBundle(t, f, b)
			found := false
			for _, drop := range report.DroppedSchedules {
				found = found || drop.Reason == tc.reason
			}
			if !found || len(out.Schedules) != 0 {
				t.Fatalf("invalid carried %s: %+v", tc.field, report)
			}
		})
	}
}
