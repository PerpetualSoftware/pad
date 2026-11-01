package server

import (
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4104 D3: the preflight and the copy report the source schedule's fate
// from one store function, and a schedule-less copy's response is unchanged.
func TestCopyEndpoint_ScheduleMembersMatchThePreflight(t *testing.T) {
	cases := []struct {
		name        string
		setup       func(t *testing.T, f *copyPreflightFixture)
		wantCarried bool
		wantSkipped string
		wantHistory int
	}{
		{"carried", func(t *testing.T, f *copyPreflightFixture) {
			mustSchedule(t, f)
		}, true, "", 0},
		{"skipped: the rule does not compile", func(t *testing.T, f *copyPreflightFixture) {
			sc := mustSchedule(t, f)
			if _, err := f.srv.store.DB().Exec(f.srv.store.D().Rebind(`UPDATE item_schedules SET rrule = 'NOT A RULE' WHERE id = ?`), sc.ID); err != nil {
				t.Fatal(err)
			}
		}, false, "timing:syntax", 0},
		{"skipped: ended", func(t *testing.T, f *copyPreflightFixture) {
			sc := mustSchedule(t, f)
			if _, err := f.srv.store.DB().Exec(f.srv.store.D().Rebind(`UPDATE item_schedules SET ended_at = '2026-01-01T00:00:00Z', next_at = NULL, next_wall = NULL WHERE id = ?`), sc.ID); err != nil {
				t.Fatal(err)
			}
		}, false, "ended", 0},
		{"none", func(t *testing.T, f *copyPreflightFixture) {}, false, "", 0},
	}
	for _, tc := range cases {
		for _, move := range []bool{false, true} {
			name := tc.name
			if move {
				name += " (move)"
			}
			t.Run(name, func(t *testing.T) {
				f := newCopyPreflightFixture(t)
				tc.setup(t, f)
				body := f.resolvableBody()
				body["archive_source"] = move

				pre := f.ok(body).Warnings
				res := f.copyOK(body).Warnings
				if pre.ScheduleCarried != tc.wantCarried || pre.ScheduleSkipped != tc.wantSkipped || pre.ScheduleHistoryDropped != tc.wantHistory {
					t.Errorf("preflight = carried %v skipped %q history %d, want %v %q %d", pre.ScheduleCarried, pre.ScheduleSkipped, pre.ScheduleHistoryDropped, tc.wantCarried, tc.wantSkipped, tc.wantHistory)
				}
				if res.ScheduleCarried != pre.ScheduleCarried || res.ScheduleSkipped != pre.ScheduleSkipped || res.ScheduleHistoryDropped != pre.ScheduleHistoryDropped {
					t.Errorf("copy %+v disagrees with the preflight %+v", res, pre)
				}
			})
		}
	}
}

// A schedule-less copy carries none of the new members on the wire, in either
// response.
func TestCopyEndpoint_ScheduleMembersAbsentWithoutASchedule(t *testing.T) {
	f := newCopyPreflightFixture(t)
	body := f.resolvableBody()
	if rr := f.call(f.owner, reqOpts{}, body); strings.Contains(rr.Body.String(), "schedule_") {
		t.Errorf("preflight body names a schedule member: %s", rr.Body.String())
	}
	if rr := f.callCopy(f.owner, reqOpts{}, body); strings.Contains(rr.Body.String(), "schedule_") {
		t.Errorf("copy body names a schedule member: %s", rr.Body.String())
	}
}

func mustSchedule(t *testing.T, f *copyPreflightFixture) *models.ItemSchedule {
	t.Helper()
	sc, err := f.srv.store.CreateSchedule(f.wsA.ID, f.source.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2099-01-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"})
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	return sc
}
