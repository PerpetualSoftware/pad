package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4610: role_offsets is the schedule row's own stored offsets, the base a
// PATCH merges against. Runs on SQLite in make test and on Postgres in make
// test-pg.

func intPtrEq(p *int, want int) bool { return p != nil && *p == want }

func TestGetCommitmentRoleOffsetsNative(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	roles := nativeRoles()
	roles.DeadlineDays, roles.DeadlineSecs = i(1), i(-28800)
	sc, err := f.s.createScheduleRolesAt(f.ws.ID, f.item.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"}, ptr(mustRoles(t, roles)), schedNow)
	if err != nil {
		t.Fatal(err)
	}
	f.force(t, sc, "2026-10-26T09:00:00", deadlineOccAt, deadlineOccAt)
	before, err := f.s.GetCommitment(f.ws.ID, f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.deadlineOverride(t, sc, "2026-10-29T12:00:00Z")
	got, err := f.s.GetCommitment(f.ws.ID, f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if str(got.Roles.Deadline) == str(before.Roles.Deadline) || str(got.Roles.Deadline) != "2026-10-29T12:00:00Z" {
		t.Fatalf("override did not change the effective deadline: before %q after %q", str(before.Roles.Deadline), str(got.Roles.Deadline))
	}
	o := got.RoleOffsets
	if o == nil {
		t.Fatal("role_offsets absent on a live native schedule")
	}
	if o.TargetDays != 0 || o.TargetSecs != 0 || !intPtrEq(o.EarliestDays, 0) || !intPtrEq(o.EarliestSecs, -1800) ||
		!intPtrEq(o.DeadlineDays, 1) || !intPtrEq(o.DeadlineSecs, -28800) || !intPtrEq(o.OccupiedDays, 0) || !intPtrEq(o.OccupiedSecs, 7200) {
		t.Fatalf("role_offsets %+v", *o)
	}
	if before.RoleOffsets == nil || !sameOffsets(before.RoleOffsets, o) {
		t.Fatalf("an override moved the stored offsets: before %+v after %+v", before.RoleOffsets, *o)
	}
}

func sameOffsets(a, b *models.CommitmentRoleOffsets) bool {
	eq := func(x, y *int) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	return a.TargetDays == b.TargetDays && a.TargetSecs == b.TargetSecs && eq(a.EarliestDays, b.EarliestDays) && eq(a.EarliestSecs, b.EarliestSecs) &&
		eq(a.DeadlineDays, b.DeadlineDays) && eq(a.DeadlineSecs, b.DeadlineSecs) && eq(a.OccupiedDays, b.OccupiedDays) && eq(a.OccupiedSecs, b.OccupiedSecs)
}

func TestGetCommitmentRoleOffsetsLegacy(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"window", "deadline_hard"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			f := newSchedFixture(t)
			sc := f.commitFixture(t, kind)
			// The stored columns are not what a legacy row reads: legacyRoles is.
			f.exec(t, `UPDATE item_schedules SET deadline_days=5, deadline_secs=5, deadline_behavior='lapses', occupied_days=9, occupied_secs=9 WHERE id=?`, sc.ID)
			want, err := legacyRoles(sc.Kind, sc.Dtstart, derefStr(sc.Dtend))
			if err != nil {
				t.Fatal(err)
			}
			got, err := f.s.GetCommitment(f.ws.ID, f.item.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Roles.RolesOrigin != "legacy_backfill" || got.RoleOffsets == nil {
				t.Fatalf("origin %q offsets %v", got.Roles.RolesOrigin, got.RoleOffsets)
			}
			wantOff := &models.CommitmentRoleOffsets{TargetDays: want.TargetDays, TargetSecs: want.TargetSecs, EarliestDays: want.EarliestDays, EarliestSecs: want.EarliestSecs,
				DeadlineDays: want.DeadlineDays, DeadlineSecs: want.DeadlineSecs, OccupiedDays: want.OccupiedDays, OccupiedSecs: want.OccupiedSecs}
			if !sameOffsets(got.RoleOffsets, wantOff) {
				t.Fatalf("role_offsets %+v, legacyRoles %+v", *got.RoleOffsets, *wantOff)
			}
			if kind == "window" && !intPtrEq(got.RoleOffsets.OccupiedSecs, 9000) {
				t.Fatalf("window occupied %+v", *got.RoleOffsets)
			}
			if kind == "deadline_hard" && !intPtrEq(got.RoleOffsets.DeadlineDays, 0) {
				t.Fatalf("deadline %+v", *got.RoleOffsets)
			}
		})
	}
}

func TestGetCommitmentRoleOffsetsEnded(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	f.nativeCommit(t)
	f.exec(t, `UPDATE item_schedules SET ended_at=?, next_at=NULL, next_wall=NULL WHERE item_id=?`, "2026-10-25T00:00:00Z", f.item.ID)
	got, err := f.s.GetCommitment(f.ws.ID, f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Roles.Target != nil || got.Roles.Deadline != nil {
		t.Fatalf("ended roles %+v", got.Roles)
	}
	if got.RoleOffsets != nil {
		t.Fatalf("role_offsets present on an ended schedule: %+v", *got.RoleOffsets)
	}
}
