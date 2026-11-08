package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4351 PR-C1: the store half of the commitment contract. Runs on SQLite in
// make test and on Postgres in make test-pg.

const deadlineKey = deadlineOccAt + "|2026-10-26T09:00:00"

func i(n int) *int { return &n }

func nativeRoles() models.CommitmentRolesInput {
	return models.CommitmentRolesInput{
		TargetDays: 0, TargetSecs: 0,
		EarliestDays: i(0), EarliestSecs: i(-1800),
		DeadlineDays: i(0), DeadlineSecs: i(3600), DeadlineBehavior: ptr("stays_owed"),
		OccupiedDays: i(0), OccupiedSecs: i(7200),
		Outcome: "shipped",
	}
}

func mustRoles(t *testing.T, in models.CommitmentRolesInput) scheduleRoles {
	t.Helper()
	r, err := rolesFromInput(in)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// nativeCommit is a schedule created native, so no conversion guards its
// overdue deadline. commitFixture is the legacy row a native write converts.

// commitFixture is a legacy check schedule on f.item whose occurrence is forced
// to deadlineOccAt, ready for a native write.
func (f *schedFixture) commitFixture(t *testing.T, kind string) *models.ItemSchedule {
	t.Helper()
	in := models.ScheduleCreate{Kind: kind, Dtstart: "2026-10-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"}
	if kind == "window" {
		in.Dtend = "2026-10-01T11:30"
	}
	sc := f.create(t, in)
	f.force(t, sc, "2026-10-26T09:00:00", deadlineOccAt, deadlineOccAt)
	f.exec(t, `UPDATE item_schedules SET created_at=? WHERE id=?`, "2026-10-01T00:00:00Z", sc.ID)
	return f.reload(t, f.item.ID)
}

func (f *schedFixture) nativeCommit(t *testing.T) *models.ItemSchedule {
	t.Helper()
	sc, err := f.s.createScheduleRolesAt(f.ws.ID, f.item.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"}, ptr(mustRoles(t, nativeRoles())), schedNow)
	if err != nil {
		t.Fatalf("create native: %v", err)
	}
	f.force(t, sc, "2026-10-26T09:00:00", deadlineOccAt, deadlineOccAt)
	f.exec(t, `UPDATE item_schedules SET created_at=? WHERE id=?`, "2026-10-01T00:00:00Z", sc.ID)
	return f.reload(t, f.item.ID)
}

func (f *schedFixture) journal(t *testing.T, scheduleID string) []models.ScheduleChangeExport {
	t.Helper()
	return moveEntries(t, f, scheduleID)
}

func str(p *string) string { return derefStr(p) }

func TestGetCommitmentLegacyBackfillMatchesLegacyRoles(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	f.commitFixture(t, "deadline_hard")
	// The stored columns are not what a legacy row reads: legacyRoles is.
	f.exec(t, `UPDATE item_schedules SET deadline_days=5, deadline_secs=5, outcome='x' WHERE item_id=?`, f.item.ID)
	got, err := f.s.GetCommitment(f.ws.ID, f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := legacyRoles("deadline_hard", "2026-10-01T09:00:00", "")
	if got.Roles.RolesOrigin != want.Origin || str(got.Roles.DeadlineBehavior) != *want.DeadlineBehavior {
		t.Fatalf("roles = %+v", got.Roles)
	}
	if str(got.Roles.Deadline) != deadlineOccAt || str(got.Roles.Target) != deadlineOccAt || got.Roles.Earliest != nil || got.Roles.OccupiedUntil != nil {
		t.Fatalf("hard roles = %+v", got.Roles)
	}
	if got.OccurrenceKey != deadlineKey {
		t.Fatalf("key = %q", got.OccurrenceKey)
	}

	g := newSchedFixture(t)
	g.commitFixture(t, "window")
	win, err := g.s.GetCommitment(g.ws.ID, g.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if str(win.Roles.OccupiedUntil) != "2026-10-26T11:30:00Z" || win.Roles.Deadline != nil || win.Roles.DeadlineBehavior != nil {
		t.Fatalf("window roles = %+v", win.Roles)
	}
}

func TestGetCommitmentNativeReadsColumnsAndOverrides(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.nativeCommit(t)
	got, err := f.s.GetCommitment(f.ws.ID, f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	r := got.Roles
	if r.RolesOrigin != "native" || r.Outcome != "shipped" || str(r.DeadlineBehavior) != "stays_owed" ||
		str(r.Target) != deadlineOccAt || str(r.Earliest) != "2026-10-26T08:30:00Z" ||
		str(r.Deadline) != "2026-10-26T10:00:00Z" || str(r.OccupiedUntil) != "2026-10-26T11:00:00Z" {
		t.Fatalf("native roles = %+v", r)
	}
	f.deadlineOverride(t, sc, "2026-10-27T14:00:00Z")
	got, err = f.s.GetCommitment(f.ws.ID, f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if str(got.Roles.Deadline) != "2026-10-27T14:00:00Z" || str(got.Roles.Target) != deadlineOccAt {
		t.Fatalf("override not applied only to its role: %+v", got.Roles)
	}
}

func TestGetCommitmentJournalRowsAndNoSchedule(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	if _, err := f.s.GetCommitment(f.ws.ID, f.item.ID); !errors.Is(err, ErrScheduleNotFound) {
		t.Fatalf("no schedule: %v", err)
	}
	sc := f.nativeCommit(t)
	f.eval(t, "2026-10-26T10:00:01Z", 0)
	// A passed-over occurrence is journaled missed_recorded with no role; it is not the deadline's history.
	f.exec(t, `INSERT INTO schedule_changes (id,workspace_id,item_id,schedule_id,occurrence_key,change_type,actor,reason,reason_source,root_change_id,recorded_at) VALUES (?,?,?,?,?,'missed_recorded','system','','system',?,?)`,
		"role-less-miss", f.ws.ID, f.item.ID, sc.ID, "2026-10-25T09:00:00Z|2026-10-25T09:00:00", "role-less-miss", nowNano())
	got, err := f.s.GetCommitment(f.ws.ID, f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Journal) != 1 || got.Journal[0].ChangeType != "missed_recorded" || str(got.Journal[0].OccurrenceKey) != deadlineKey {
		t.Fatalf("journal = %+v (schedule %s)", got.Journal, sc.ID)
	}
}

func TestNativeWriteConvertsLegacyRowJournalsConversion(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.commitFixture(t, "deadline_hard")
	if _, err := f.s.SetCommitmentRoles(f.ws.ID, f.item.ID, nativeRoles()); err != nil {
		t.Fatal(err)
	}
	if f.reload(t, f.item.ID).ID != sc.ID || f.count(t, `SELECT COUNT(*) FROM item_schedules WHERE roles_origin='native' AND id=?`, sc.ID) != 1 {
		t.Fatal("row not native")
	}
	rows := f.journal(t, sc.ID)
	last := rows[len(rows)-1]
	var b, a scheduleDateSnapshot
	if last.ChangeType != "update" || last.BeforeJSON == nil || last.AfterJSON == nil || last.ReasonSource != "system" {
		t.Fatalf("journal = %+v", last)
	}
	if json.Unmarshal([]byte(*last.BeforeJSON), &b) != nil || json.Unmarshal([]byte(*last.AfterJSON), &a) != nil {
		t.Fatal("bad snapshots")
	}
	if b.Roles.Origin != "legacy_backfill" || a.Roles.Origin != "native" || a.Roles.Outcome != "shipped" {
		t.Fatalf("before %q after %q", b.Roles.Origin, a.Roles.Origin)
	}
	// A second native write is not a conversion.
	if _, err := f.s.SetCommitmentRoles(f.ws.ID, f.item.ID, nativeRoles()); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE schedule_id=? AND before_json LIKE '%legacy_backfill%'`, sc.ID); n != 1 {
		t.Fatalf("legacy before_json rows = %d, want 1", n)
	}
}

func TestNativeWriteRefusesBadRoles(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	f.commitFixture(t, "check")
	bad := nativeRoles()
	bad.DeadlineBehavior = nil
	if _, err := f.s.SetCommitmentRoles(f.ws.ID, f.item.ID, bad); err == nil {
		t.Fatal("deadline without behavior accepted")
	}
	if n := f.count(t, `SELECT COUNT(*) FROM item_schedules WHERE roles_origin='native'`); n != 0 {
		t.Fatal("refused write changed the row")
	}
}

func TestCreateCommitmentScheduleWritesNative(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc, err := f.s.CreateCommitmentSchedule(f.ws.ID, f.item.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"}, nativeRoles())
	if err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM item_schedules WHERE id=? AND roles_origin='native' AND deadline_secs=3600 AND outcome='shipped'`, sc.ID); n != 1 {
		t.Fatal("created row not native with the roles")
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE schedule_id=? AND change_type='create' AND after_json LIKE '%"roles_origin":"native"%'`, sc.ID); n != 1 {
		t.Fatal("create row does not show the native roles")
	}
}

func TestRenegotiateDeadlineWritesOverrideAndJournalsMoveUserReason(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.nativeCommit(t)
	until := rt("2026-10-27T14:00:00Z")
	if err := f.s.renegotiateDeadlineAt(f.ws.ID, f.item.ID, "user", until, "  client asked for more time ", renegNow); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_occurrence_overrides WHERE schedule_id=? AND occurrence_key=? AND role='deadline' AND value_at=?`, sc.ID, deadlineKey, "2026-10-27T14:00:00Z"); n != 1 {
		t.Fatal("deadline override missing")
	}
	rows := f.journal(t, sc.ID)
	e := rows[len(rows)-1]
	if e.ChangeType != "move" || str(e.Role) != "deadline" || e.ReasonSource != "user" || e.Reason != "client asked for more time" || str(e.OccurrenceKey) != deadlineKey || e.Actor != "user" {
		t.Fatalf("journal = %+v", e)
	}
	if e.BeforeJSON == nil || e.AfterJSON == nil || e.ParentChangeID != nil || e.RootChangeID != e.ID {
		t.Fatalf("snapshots or lineage = %+v", e)
	}
	got, _ := f.s.GetCommitment(f.ws.ID, f.item.ID)
	if str(got.Roles.Deadline) != "2026-10-27T14:00:00Z" {
		t.Fatalf("deadline = %v", got.Roles.Deadline)
	}
	// A second renegotiation replaces the override.
	if err := f.s.renegotiateDeadlineAt(f.ws.ID, f.item.ID, "user", rt("2026-10-28T14:00:00Z"), "again", renegNow); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_occurrence_overrides WHERE schedule_id=? AND role='deadline'`, sc.ID); n != 1 {
		t.Fatalf("override rows = %d", n)
	}
}

func TestRenegotiateRequiresReason(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.nativeCommit(t)
	before := len(f.journal(t, sc.ID))
	for _, reason := range []string{"", "   ", "\t\n"} {
		err := f.s.renegotiateDeadlineAt(f.ws.ID, f.item.ID, "user", rt("2026-10-27T14:00:00Z"), reason, renegNow)
		var fe *ScheduleFieldError
		if !errors.As(err, &fe) || fe.Field != "reason" || fe.Code != "required" {
			t.Fatalf("reason %q: %v", reason, err)
		}
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_occurrence_overrides WHERE schedule_id=?`, sc.ID); n != 0 || len(f.journal(t, sc.ID)) != before {
		t.Fatal("a refused renegotiation wrote something")
	}
}

func TestRenegotiateRefusesRowWithoutNativeDeadline(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	f.commitFixture(t, "deadline_hard") // legacy
	err := f.s.renegotiateDeadlineAt(f.ws.ID, f.item.ID, "user", rt("2026-10-27T14:00:00Z"), "why", renegNow)
	var fe *ScheduleFieldError
	if !errors.As(err, &fe) || fe.Field != "deadline" {
		t.Fatalf("legacy row: %v", err)
	}
	g := newSchedFixture(t)
	if err := g.s.renegotiateDeadlineAt(g.ws.ID, g.item.ID, "user", rt("2026-10-27T14:00:00Z"), "why", renegNow); !errors.Is(err, ErrScheduleNotFound) {
		t.Fatalf("no schedule: %v", err)
	}
}

func TestRenegotiateOnConvertedRowKeepsLaterMissesRecorded(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	f.commitFixture(t, "deadline_hard")
	if _, err := f.s.SetCommitmentRoles(f.ws.ID, f.item.ID, nativeRoles()); err != nil {
		t.Fatal(err)
	}
	if got := f.eval(t, "2026-10-26T12:00:00Z", 0); got.Missed != 0 {
		t.Fatalf("converted row recorded a miss: %+v", got)
	}
	until := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Second)
	if err := f.s.renegotiateDeadlineAt(f.ws.ID, f.item.ID, "user", until, "more time", time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := f.eval(t, until.Add(-time.Second).Format(time.RFC3339), 0); got.Missed != 0 {
		t.Fatalf("before the new deadline: %+v", got)
	}
	if got := f.eval(t, until.Add(time.Second).Format(time.RFC3339), 0); got.Missed != 1 || f.deadlineEvidence(t) != 1 {
		t.Fatalf("after the new deadline: %+v", got)
	}
}

func TestOnlyConversionWritesLegacyBackfillBeforeJSON(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	owner := f.user(t, "owner@example.com", "")
	other := f.user(t, "other@example.com", "")
	sc := f.commitFixture(t, "check")
	if _, err := f.s.SetCommitmentRoles(f.ws.ID, f.item.ID, nativeRoles()); err != nil {
		t.Fatal(err)
	}
	// renegotiation, a miss, the assignee step, a late on-time report, a move.
	until := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Second)
	if err := f.s.renegotiateDeadlineAt(f.ws.ID, f.item.ID, "user", until, "later", time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := f.eval(t, until.Add(time.Second).Format(time.RFC3339), 0); got.Missed != 1 {
		t.Fatalf("no miss: %+v", got)
	}
	for _, id := range []string{owner.ID, other.ID} {
		if _, err := f.s.UpdateItem(f.item.ID, models.ItemUpdate{AssignedUserID: &id}); err != nil {
			t.Fatal(err)
		}
	}
	at := rt("2026-10-26T10:00:00Z")
	f.mustResolve(t, f.item, ResolveInput{State: "done", ActualDoneAt: &at, OccurrenceAt: rt(deadlineOccAt), View: time.UTC, Actor: "user"}, until.Add(2*time.Second).Format(time.RFC3339))
	rows := f.journal(t, sc.ID)
	var marked []string
	for _, r := range rows {
		if r.BeforeJSON != nil && strings.Contains(*r.BeforeJSON, "legacy_backfill") {
			marked = append(marked, r.ChangeType+"/"+str(r.Role))
		}
	}
	if len(marked) != 1 || marked[0] != "update/" {
		t.Fatalf("rows with a legacy before_json = %v, want only the conversion", marked)
	}
	types := map[string]int{}
	for _, r := range rows {
		types[r.ChangeType]++
	}
	for _, want := range []string{"move", "missed_recorded", "done", "update"} {
		if types[want] == 0 {
			t.Fatalf("scenario never wrote a %s row: %v", want, types)
		}
	}
	if f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE schedule_id=? AND change_type='done' AND role='deadline' AND parent_change_id IS NOT NULL`, sc.ID) != 1 {
		t.Fatal("scenario never wrote the correction")
	}
}

// A done reported at or before the deadline, after the miss was recorded,
// leaves the miss and adds the correction that answers it.
func TestLateOnTimeReportWritesCorrectionKeepingMiss(t *testing.T) {
	t.Parallel()
	for _, actual := range []string{"2026-10-26T09:30:00Z", "2026-10-26T10:00:00Z"} {
		t.Run(actual, func(t *testing.T) {
			f := newSchedFixture(t)
			sc := f.nativeCommit(t)
			if got := f.eval(t, "2026-10-26T10:00:01Z", 0); got.Missed != 1 {
				t.Fatalf("no miss: %+v", got)
			}
			at := rt(actual)
			f.mustResolve(t, f.item, ResolveInput{State: "done", ActualDoneAt: &at, OccurrenceAt: rt(deadlineOccAt), View: time.UTC, Actor: "user"}, "2026-10-26T11:00:00Z")

			var miss, corr *models.ScheduleChangeExport
			rows := f.journal(t, sc.ID)
			for k := range rows {
				switch {
				case rows[k].ChangeType == "missed_recorded":
					miss = &rows[k]
				case rows[k].ChangeType == "done" && str(rows[k].Role) == "deadline":
					corr = &rows[k]
				}
			}
			if miss == nil || corr == nil {
				t.Fatalf("miss %v correction %v", miss, corr)
			}
			if str(corr.ParentChangeID) != miss.ID || corr.RootChangeID != miss.RootChangeID || corr.Reason != "Reported on time" || corr.ReasonSource != "system" || str(corr.OccurrenceKey) != deadlineKey {
				t.Fatalf("correction = %+v, miss = %+v", corr, miss)
			}
			if n := f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE id=?`, miss.ID); n != 1 {
				t.Fatal("original miss row not kept")
			}
			got, _ := f.s.GetCommitment(f.ws.ID, f.item.ID)
			if len(got.Journal) != 2 || got.Journal[1].ChangeType != "done" || str(got.Journal[1].ParentChangeID) != miss.ID {
				t.Fatalf("commitment journal = %+v", got.Journal)
			}
		})
	}
}

func TestRecordedNowDoneNeverSupersedes(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.nativeCommit(t)
	f.eval(t, "2026-10-26T10:00:01Z", 0)
	f.mustResolve(t, f.item, ResolveInput{State: "done", OccurrenceAt: rt(deadlineOccAt), View: time.UTC, Actor: "user"}, "2026-10-26T09:30:00Z")
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE schedule_id=? AND change_type='done' AND role='deadline'`, sc.ID); n != 0 {
		t.Fatalf("a done with no reported time wrote %d corrections", n)
	}
}

func TestLateReportAfterDeadlineAndWithoutMissNeverSupersede(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.nativeCommit(t)
	f.eval(t, "2026-10-26T10:00:01Z", 0)
	late := rt("2026-10-26T10:00:01Z")
	f.mustResolve(t, f.item, ResolveInput{State: "done", ActualDoneAt: &late, OccurrenceAt: rt(deadlineOccAt), View: time.UTC, Actor: "user"}, "2026-10-26T11:00:00Z")
	g := newSchedFixture(t)
	gsc := g.nativeCommit(t) // no miss recorded
	ontime := rt("2026-10-26T09:30:00Z")
	g.mustResolve(t, g.item, ResolveInput{State: "done", ActualDoneAt: &ontime, OccurrenceAt: rt(deadlineOccAt), View: time.UTC, Actor: "user"}, "2026-10-26T09:45:00Z")
	for _, c := range []struct {
		f  *schedFixture
		id string
	}{{f, sc.ID}, {g, gsc.ID}} {
		if n := c.f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE schedule_id=? AND change_type='done' AND role='deadline'`, c.id); n != 0 {
			t.Fatalf("wrote %d corrections", n)
		}
	}
}

// Resolution reads the occurrence rows, never the journal: a correction row is
// not a resolution, and a forged one resolves nothing.
func TestCorrectionIsNotAResolution(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.nativeCommit(t)
	f.eval(t, "2026-10-26T10:00:01Z", 0)
	var missID string
	if err := f.s.db.QueryRow(f.s.q(`SELECT id FROM schedule_changes WHERE schedule_id=? AND change_type='missed_recorded'`), sc.ID).Scan(&missID); err != nil {
		t.Fatal(err)
	}
	// A correction row with no resolution behind it.
	f.exec(t, `INSERT INTO schedule_changes (id,workspace_id,item_id,schedule_id,occurrence_key,change_type,role,actor,reason,reason_source,root_change_id,parent_change_id,recorded_at) VALUES (?,?,?,?,?,'done','deadline','system','Reported on time','system',?,?,?)`,
		newID(), f.ws.ID, f.item.ID, sc.ID, deadlineKey, missID, missID, nowNano())
	if n := f.occTotal(t, sc.ID); n != 0 {
		t.Fatalf("occurrence rows = %d before any resolve", n)
	}
	at := rt("2026-10-26T09:30:00Z")
	f.mustResolve(t, f.item, ResolveInput{State: "done", ActualDoneAt: &at, OccurrenceAt: rt(deadlineOccAt), View: time.UTC, Actor: "user"}, "2026-10-26T11:00:00Z")
	if n := f.occRows(t, sc.ID, "done"); n != 1 {
		t.Fatalf("done occurrences = %d, want 1", n)
	}
	// The occurrence is now resolved: naming it again is stale, whatever the journal says.
	_, err := f.resolve(t, f.item, ResolveInput{State: "done", ActualDoneAt: &at, OccurrenceAt: rt(deadlineOccAt), View: time.UTC, Actor: "user"}, "2026-10-26T11:00:05Z")
	if isStale(err) == nil {
		t.Fatalf("second resolve: %v", err)
	}
}

func (f *schedFixture) updateAssignee(t *testing.T, userID string) {
	t.Helper()
	if _, err := f.s.UpdateItem(f.item.ID, models.ItemUpdate{AssignedUserID: &userID}); err != nil {
		t.Fatalf("UpdateItem: %v", err)
	}
}

func (f *schedFixture) assigneeRows(t *testing.T, scheduleID string) []models.ScheduleChangeExport {
	t.Helper()
	var out []models.ScheduleChangeExport
	for _, r := range f.journal(t, scheduleID) {
		if strings.HasPrefix(r.Reason, "Assignee changed") {
			out = append(out, r)
		}
	}
	return out
}

func TestAssigneeChangeOnHardDeadlineJournalsUpdateStep(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	a, b := f.user(t, "a@example.com", ""), f.user(t, "b@example.com", "")
	sc := f.nativeCommit(t)
	f.updateAssignee(t, a.ID)
	f.updateAssignee(t, b.ID)
	rows := f.assigneeRows(t, sc.ID)
	if len(rows) != 2 {
		t.Fatalf("assignee rows = %d", len(rows))
	}
	for k, want := range []string{"Assignee changed from  to " + a.ID, "Assignee changed from " + a.ID + " to " + b.ID} {
		r := rows[k]
		if r.Reason != want || r.ChangeType != "update" || r.Role != nil || r.ReasonSource != "system" || r.ParentChangeID != nil || r.RootChangeID != r.ID {
			t.Fatalf("row %d = %+v, want reason %q", k, r, want)
		}
	}
	// An update that keeps the assignee writes nothing.
	f.updateAssignee(t, b.ID)
	if n := len(f.assigneeRows(t, sc.ID)); n != 2 {
		t.Fatalf("unchanged assignee wrote a row: %d", n)
	}
}

func TestAssigneeChangeWithoutNativeScheduleWritesNothingExtra(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	a := f.user(t, "a@example.com", "")
	// No schedule at all, a legacy row, and a native row with no deadline.
	f.updateAssignee(t, a.ID)
	legacy := f.another(t, "Legacy")
	if _, err := f.s.createScheduleAt(f.ws.ID, legacy.ID, models.ScheduleCreate{Kind: "deadline_hard", Dtstart: "2026-10-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"}, schedNow); err != nil {
		t.Fatal(err)
	}
	soft := f.another(t, "Soft")
	if _, err := f.s.createScheduleAt(f.ws.ID, soft.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"}, schedNow); err != nil {
		t.Fatal(err)
	}
	f.exec(t, `UPDATE item_schedules SET roles_origin='native', deadline_days=NULL, deadline_secs=NULL, deadline_behavior=NULL WHERE item_id=?`, soft.ID)
	before := f.count(t, `SELECT COUNT(*) FROM schedule_changes`)
	for _, it := range []*models.Item{legacy, soft} {
		if _, err := f.s.UpdateItem(it.ID, models.ItemUpdate{AssignedUserID: &a.ID}); err != nil {
			t.Fatal(err)
		}
	}
	if after := f.count(t, `SELECT COUNT(*) FROM schedule_changes`); after != before {
		t.Fatalf("journal rows %d -> %d", before, after)
	}
}

func TestAssigneeChangeRollsBackWithItemUpdate(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	a := f.user(t, "a@example.com", "")
	sc := f.nativeCommit(t)
	boom := errors.New("boom")
	_, err := f.s.UpdateItem(f.item.ID, models.ItemUpdate{AssignedUserID: &a.ID, AfterWriteTx: func(_ *sql.Tx, _ *models.Item) error { return boom }})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if n := len(f.assigneeRows(t, sc.ID)); n != 0 {
		t.Fatalf("rolled-back update left %d journal rows", n)
	}
	if it, _ := f.s.GetItem(f.item.ID); it.AssignedUserID != nil {
		t.Fatalf("assignee = %v", *it.AssignedUserID)
	}
	f.updateAssignee(t, a.ID)
	if n := len(f.assigneeRows(t, sc.ID)); n != 1 {
		t.Fatalf("committed update journal rows = %d", n)
	}
}

// renegNow is the clock the renegotiation tests run at: before every deadline they use.
var renegNow = rt("2026-10-26T00:00:00Z")

func TestRenegotiateJournalCarriesOldAndNewDeadline(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.nativeCommit(t)
	if err := f.s.renegotiateDeadlineAt(f.ws.ID, f.item.ID, "user", rt("2026-10-27T14:00:00Z"), "one", renegNow); err != nil {
		t.Fatal(err)
	}
	if err := f.s.renegotiateDeadlineAt(f.ws.ID, f.item.ID, "user", rt("2026-10-28T14:00:00Z"), "two", renegNow); err != nil {
		t.Fatal(err)
	}
	rows := f.journal(t, sc.ID)
	e := rows[len(rows)-1]
	if !strings.Contains(*e.BeforeJSON, "2026-10-27T14:00:00Z") || !strings.Contains(*e.AfterJSON, "2026-10-28T14:00:00Z") || strings.Contains(*e.BeforeJSON, "legacy_backfill") {
		t.Fatalf("before=%s after=%s", *e.BeforeJSON, *e.AfterJSON)
	}
}

func TestRenegotiateUntilRange(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	f.nativeCommit(t)
	for _, until := range []time.Time{renegNow, renegNow.Add(-time.Hour), renegNow.Add(maxResolveUntil + time.Hour)} {
		if err := f.s.renegotiateDeadlineAt(f.ws.ID, f.item.ID, "user", until, "why", renegNow); err == nil {
			t.Fatalf("until %v accepted", until)
		}
	}
}

func TestRenegotiateRangeUsesStoredInstant(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	f.nativeCommit(t)
	now := renegNow.Add(500 * time.Millisecond)
	// now+366d is inside the limit only before the writer rounds it up.
	if err := f.s.renegotiateDeadlineAt(f.ws.ID, f.item.ID, "user", now.Add(maxResolveUntil), "why", now); err == nil {
		t.Fatal("a deadline that rounds past the limit was accepted")
	}
}

// A schedule that is not native with a deadline never gets a correction row, even
// when a miss row exists for its key and the report is on time: the resolve
// still succeeds.
func TestLateOnTimeReportOnLegacyRowWritesNoCorrection(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.commitFixture(t, "deadline_hard") // legacy_backfill
	f.exec(t, `INSERT INTO schedule_changes (id,workspace_id,item_id,schedule_id,occurrence_key,change_type,role,after_json,actor,reason,reason_source,root_change_id,recorded_at) VALUES (?,?,?,?,?,'missed_recorded','deadline',?,'system','','system',?,?)`,
		"legacy-miss", f.ws.ID, f.item.ID, sc.ID, deadlineKey, `{"deadline":"2026-10-26T10:00:00Z"}`, "legacy-miss", nowNano())
	at := rt("2026-10-26T09:30:00Z")
	f.mustResolve(t, f.item, ResolveInput{State: "done", ActualDoneAt: &at, OccurrenceAt: rt(deadlineOccAt), View: time.UTC, Actor: "user"}, "2026-10-26T11:00:00Z")
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE schedule_id=? AND change_type='done' AND role='deadline'`, sc.ID); n != 0 {
		t.Fatalf("correction rows on a legacy row = %d", n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE id='legacy-miss'`); n != 1 {
		t.Fatal("the miss row was not kept")
	}
}

// Converting a legacy hard deadline that has already passed: the evaluator reads
// native rows only, so the old deadline's miss is never recorded, before or after
// the conversion. Only the deadline the conversion sets is judged afterwards, and
// only if it is later than the conversion itself.
func TestConvertingAPastLegacyHardDeadlineRecordsNoMissForTheOldOne(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		days, secs int
		laterMiss  string // when the new deadline passes, "" if it never records
	}{
		"new deadline in the future": {1, 0, "2026-10-27T09:00:01Z"},
		"new deadline already past":  {0, 1800, ""},
	} {
		t.Run(name, func(t *testing.T) {
			f := newSchedFixture(t)
			sc := f.create(t, models.ScheduleCreate{Kind: "deadline_hard", Dtstart: "2026-10-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"})
			f.force(t, sc, "2026-10-26T09:00:00", deadlineOccAt, deadlineOccAt)
			if got := f.eval(t, "2026-10-26T10:00:00Z", 0); got.Missed != 0 {
				t.Fatalf("a legacy row is never evaluated: %+v", got)
			}
			in := models.CommitmentRolesInput{DeadlineDays: ptr(tc.days), DeadlineSecs: ptr(tc.secs), DeadlineBehavior: ptr("stays_owed")}
			if _, err := f.s.SetCommitmentRoles(f.ws.ID, f.item.ID, in); err != nil {
				t.Fatal(err)
			}
			f.exec(t, `UPDATE schedule_changes SET recorded_at=? WHERE schedule_id=? AND change_type='update'`, "2026-10-26T10:00:00Z", sc.ID)
			if got := f.eval(t, "2026-10-26T10:00:01Z", 0); got.Missed != 0 {
				t.Fatalf("the old deadline's miss must not appear after the conversion: %+v", got)
			}
			want := 0
			if tc.laterMiss != "" {
				want = 1
			}
			if got := f.eval(t, "2026-10-28T00:00:00Z", 0); got.Missed != want {
				t.Fatalf("later: missed %d, want %d", got.Missed, want)
			}
		})
	}
}

func TestDeleteScheduleCheckedProtectsANativeDeadline(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	f.nativeCommit(t)
	if removed, err := f.s.DeleteScheduleChecked(f.ws.ID, f.item.ID, false, nil); removed || !errors.Is(err, ErrDeadlineDeleteAuthority) {
		t.Fatalf("refused delete: %v %v", removed, err)
	}
	if sc := f.reload(t, f.item.ID); sc == nil {
		t.Fatal("a refused delete removed the schedule")
	}
	if removed, err := f.s.DeleteScheduleChecked(f.ws.ID, f.item.ID, true, nil); !removed || err != nil {
		t.Fatalf("allowed delete: %v %v", removed, err)
	}
}

func TestDeleteScheduleCheckedLeavesOtherSchedulesAlone(t *testing.T) {
	t.Parallel()
	t.Run("legacy row", func(t *testing.T) {
		f := newSchedFixture(t)
		f.create(t, models.ScheduleCreate{Kind: "deadline_hard", Dtstart: "2026-10-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"})
		if removed, err := f.s.DeleteScheduleChecked(f.ws.ID, f.item.ID, false, nil); !removed || err != nil {
			t.Fatalf("legacy: %v %v", removed, err)
		}
	})
	t.Run("native without a deadline", func(t *testing.T) {
		f := newSchedFixture(t)
		in := nativeRoles()
		in.DeadlineDays, in.DeadlineSecs, in.DeadlineBehavior = nil, nil, nil
		if _, err := f.s.createScheduleRolesAt(f.ws.ID, f.item.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"}, ptr(mustRoles(t, in)), schedNow); err != nil {
			t.Fatal(err)
		}
		if removed, err := f.s.DeleteScheduleChecked(f.ws.ID, f.item.ID, false, nil); !removed || err != nil {
			t.Fatalf("native without deadline: %v %v", removed, err)
		}
	})
	t.Run("no schedule", func(t *testing.T) {
		f := newSchedFixture(t)
		if removed, err := f.s.DeleteScheduleChecked(f.ws.ID, f.item.ID, false, nil); removed || err != nil {
			t.Fatalf("missing: %v %v", removed, err)
		}
	})
}

// A delete that needs no authority decision (an owner, or an unguarded caller)
// must not depend on reading the schedule's roles: a legacy window row whose
// stored dates no longer parse still deletes.
func TestDeleteScheduleCheckedUnguardedDoesNotReadRoles(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.create(t, models.ScheduleCreate{Kind: "window", Dtstart: "2026-10-30T09:00", Dtend: "2026-10-30T10:00", Tz: "UTC"})
	f.exec(t, `UPDATE item_schedules SET dtstart=? WHERE id=?`, "not-a-date", sc.ID)
	if removed, err := f.s.DeleteScheduleChecked(f.ws.ID, f.item.ID, true, nil); !removed || err != nil {
		t.Fatalf("unguarded delete of a legacy row with unreadable dates: removed=%v err=%v", removed, err)
	}
}
