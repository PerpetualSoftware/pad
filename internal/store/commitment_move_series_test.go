package store

import (
	"errors"
	"reflect"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// A series move writes the root's native target offset, measured from the
// outstanding occurrence's start, together with the occurrence move.

const seriesWhy = "Rescheduled this and future occurrences"

// seriesSchedule replaces item i's schedule with a native daily one.
func (f *linkFix) seriesSchedule(t *testing.T, i int, in models.ScheduleCreate) {
	t.Helper()
	f.exec(t, `DELETE FROM item_schedules WHERE id=?`, f.scheds[i].ID)
	if in.Kind == "" {
		in.Kind = "check"
	}
	if in.Rrule == "" {
		in.Rrule = "FREQ=DAILY"
	}
	sc, err := f.s.CreateCommitmentSchedule(f.ws.ID, f.items[i].ID, in, models.CommitmentRolesInput{})
	if err != nil {
		t.Fatal(err)
	}
	f.scheds[i] = sc
}

func (f *linkFix) seriesMove(i int, to string, mut func(*models.LinkedMoveInput)) (*models.LinkedMoveResult, *models.LinkedMoveResult, error) {
	preview, err := f.move(i, "target", to, func(in *models.LinkedMoveInput) {
		in.Scope = "series"
		if mut != nil {
			mut(in)
		}
		in.Preview = true
	})
	if err != nil {
		return preview, nil, err
	}
	applied, err := f.move(i, "target", to, func(in *models.LinkedMoveInput) {
		in.Scope = "series"
		if mut != nil {
			mut(in)
		}
		in.Revision = preview.Revision
	})
	return preview, applied, err
}

func (f *linkFix) targetOffset(t *testing.T, i int) models.TargetOffset {
	t.Helper()
	r, err := f.s.scheduleRolesQ(f.s.db, f.scheds[i].ID)
	if err != nil {
		t.Fatal(err)
	}
	return models.TargetOffset{TargetDays: r.TargetDays, TargetSecs: r.TargetSecs}
}

// doneAndNextTarget resolves the outstanding occurrence done and reads the next one's target.
func (f *linkFix) doneAndNextTarget(t *testing.T, i int) string {
	t.Helper()
	f.resolve(t, i, "done")
	return f.role(t, i, "target")
}

func wantFieldErr(t *testing.T, err error, field, code string) {
	t.Helper()
	var fe *ScheduleFieldError
	if !errors.As(err, &fe) || fe.Field != field || fe.Code != code {
		t.Fatalf("err=%v, want %s/%s", err, field, code)
	}
}

func TestMoveSeriesShiftsTargetOffsetFromOutstanding(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 1)
	f.seriesSchedule(t, 0, models.ScheduleCreate{Dtstart: "2030-03-04T09:00", Tz: "UTC"})
	key := f.outstandingKeyOf(t, 0)
	preview, applied, err := f.seriesMove(0, "2030-03-04T14:00:00Z", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := &models.LinkedMoveSeries{AppliesFrom: key, CurrentOffset: models.TargetOffset{}, NewOffset: models.TargetOffset{TargetSecs: 5 * 3600}}
	if !reflect.DeepEqual(preview.Series, want) || !reflect.DeepEqual(applied.Series, want) {
		t.Fatalf("preview=%+v applied=%+v", preview.Series, applied.Series)
	}
	if got := f.targetOffset(t, 0); got != want.NewOffset {
		t.Fatalf("offset=%+v", got)
	}
	if got := f.role(t, 0, "target"); got != "2030-03-04T14:00:00Z" {
		t.Fatalf("outstanding target=%s", got)
	}
	if got := f.doneAndNextTarget(t, 0); got != "2030-03-05T14:00:00Z" {
		t.Fatalf("next target=%s", got)
	}
}

func TestMoveSeriesDefaultScopeLeavesOffset(t *testing.T) {
	t.Parallel()
	for _, scope := range []string{"", "occurrence"} {
		f := newLinkFix(t, 1)
		f.seriesSchedule(t, 0, models.ScheduleCreate{Dtstart: "2030-03-04T09:00", Tz: "UTC"})
		res, err := f.move(0, "target", "2030-03-04T14:00:00Z", func(in *models.LinkedMoveInput) { in.Scope = scope })
		if err != nil || res.Series != nil {
			t.Fatalf("scope %q: res=%+v err=%v", scope, res, err)
		}
		if got := f.targetOffset(t, 0); got != (models.TargetOffset{}) {
			t.Fatalf("scope %q wrote the offset: %+v", scope, got)
		}
		if got := f.doneAndNextTarget(t, 0); got != "2030-03-05T09:00:00Z" {
			t.Fatalf("scope %q next target=%s", scope, got)
		}
	}
}

func TestMoveSeriesMeasuresFromSeedNotOverride(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 1)
	f.seriesSchedule(t, 0, models.ScheduleCreate{Dtstart: "2030-03-04T09:00", Tz: "UTC"})
	if _, err := f.move(0, "target", "2030-03-04T11:00:00Z", nil); err != nil {
		t.Fatal(err)
	}
	preview, _, err := f.seriesMove(0, "2030-03-04T14:00:00Z", nil)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Series.CurrentOffset != (models.TargetOffset{}) || preview.Series.NewOffset != (models.TargetOffset{TargetSecs: 5 * 3600}) {
		t.Fatalf("series=%+v", preview.Series)
	}
	if got := f.targetOffset(t, 0); got.TargetSecs != 5*3600 {
		t.Fatalf("offset=%+v", got)
	}
}

func TestMoveSeriesCivilAcrossDST(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 1)
	// US daylight saving starts on 2030-03-10.
	f.seriesSchedule(t, 0, models.ScheduleCreate{Dtstart: "2030-03-09T09:00", Tz: "America/New_York"})
	// 15:00 EST on the outstanding day, and on the next day 15:00 EDT.
	if _, _, err := f.seriesMove(0, "2030-03-09T20:00:00Z", nil); err != nil {
		t.Fatal(err)
	}
	if got := f.targetOffset(t, 0); got != (models.TargetOffset{TargetSecs: 6 * 3600}) {
		t.Fatalf("offset=%+v", got)
	}
	if got := f.doneAndNextTarget(t, 0); got != "2030-03-10T19:00:00Z" {
		t.Fatalf("next target=%s, want 15:00 EDT", got)
	}
	// Into the next local day, earlier in the day: days and a negative clock.
	if _, _, err := f.seriesMove(0, "2030-03-11T12:00:00Z", nil); err != nil {
		t.Fatal(err)
	}
	if got := f.targetOffset(t, 0); got != (models.TargetOffset{TargetDays: 1, TargetSecs: -3600}) {
		t.Fatalf("offset=%+v", got)
	}
}

func TestMoveSeriesFloatingRecipientZone(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 1)
	u := f.user(t, "series-floating@example.com", "America/Los_Angeles")
	f.assign(t, f.items[0].ID, u.ID)
	f.seriesSchedule(t, 0, models.ScheduleCreate{Dtstart: "2030-03-04T09:00", Floating: true})
	// 14:00 PST; read in UTC it would be 22:00, thirteen hours after 09:00.
	if _, _, err := f.seriesMove(0, "2030-03-04T22:00:00Z", nil); err != nil {
		t.Fatal(err)
	}
	if got := f.targetOffset(t, 0); got != (models.TargetOffset{TargetSecs: 5 * 3600}) {
		t.Fatalf("offset=%+v", got)
	}
}

func TestMoveSeriesAllDayMidnightStaysAllDay(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 1)
	f.seriesSchedule(t, 0, models.ScheduleCreate{Dtstart: "2030-03-04", Tz: "UTC"})
	if _, _, err := f.seriesMove(0, "2030-03-06T00:00:00Z", nil); err != nil {
		t.Fatal(err)
	}
	if got := f.targetOffset(t, 0); got != (models.TargetOffset{TargetDays: 2}) {
		t.Fatalf("offset=%+v", got)
	}
	if got := f.doneAndNextTarget(t, 0); got != "2030-03-07" {
		t.Fatalf("next target=%q, want the all-day date", got)
	}
}

func TestMoveSeriesLinkedTargetRefused(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	f.seriesSchedule(t, 0, models.ScheduleCreate{Dtstart: "2030-03-04T09:00", Tz: "UTC"})
	f.link(t, 0, 1, "target", "target", oneDay)
	before := deadlineWriteSnapshot(t, f.schedFixture)
	for _, preview := range []bool{true, false} {
		_, err := f.move(0, "target", "2030-03-04T14:00:00Z", func(in *models.LinkedMoveInput) {
			in.Scope, in.Preview, in.Revision = "series", preview, "x"
		})
		if !errors.Is(err, ErrScheduleLinked) {
			t.Fatalf("preview=%v err=%v", preview, err)
		}
	}
	if !reflect.DeepEqual(before, deadlineWriteSnapshot(t, f.schedFixture)) {
		t.Fatal("refused series move wrote")
	}

	// A link on another role of the root does not stop it.
	g := newLinkFix(t, 2)
	g.seriesSchedule(t, 0, models.ScheduleCreate{Dtstart: "2030-03-04T09:00", Tz: "UTC"})
	g.exec(t, `UPDATE item_schedules SET deadline_days=2,deadline_secs=0,deadline_behavior='stays_owed' WHERE id=?`, g.scheds[0].ID)
	g.linkRaw(t, 1, 0, "target", "deadline", oneDay)
	if _, _, err := g.seriesMove(0, "2030-03-04T14:00:00Z", nil); err != nil {
		t.Fatal(err)
	}
	if got := g.targetOffset(t, 0); got.TargetSecs != 5*3600 {
		t.Fatalf("offset=%+v", got)
	}
}

func TestMoveSeriesLegacyRefused(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 1)
	f.seriesSchedule(t, 0, models.ScheduleCreate{Dtstart: "2030-03-04T09:00", Tz: "UTC"})
	f.exec(t, `UPDATE item_schedules SET roles_origin='legacy_backfill' WHERE id=?`, f.scheds[0].ID)
	_, _, err := f.seriesMove(0, "2030-03-04T14:00:00Z", nil)
	wantFieldErr(t, err, "scope", "combination")
}

func TestMoveSeriesScopeValidation(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 1)
	f.seriesSchedule(t, 0, models.ScheduleCreate{Dtstart: "2030-03-04T09:00", Tz: "UTC"})
	_, err := f.move(0, "target", "2030-03-04T14:00:00Z", func(in *models.LinkedMoveInput) { in.Scope, in.Preview = "bogus", true })
	wantFieldErr(t, err, "scope", "enum")
	_, err = f.move(0, "earliest", "2030-03-04T14:00:00Z", func(in *models.LinkedMoveInput) { in.Scope, in.Preview = "series", true })
	wantFieldErr(t, err, "scope", "combination")
}

func TestMoveSeriesTargetAfterDeadlineRefused(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"crosses the occurrence", "crosses only the offset"} {
		t.Run(name, func(t *testing.T) {
			f := newLinkFix(t, 1)
			f.seriesSchedule(t, 0, models.ScheduleCreate{Dtstart: "2030-03-04T09:00", Tz: "UTC"})
			f.exec(t, `UPDATE item_schedules SET deadline_days=1,deadline_secs=0,deadline_behavior='stays_owed' WHERE id=?`, f.scheds[0].ID)
			if name == "crosses only the offset" {
				f.deadlineOverride(t, f.reload(t, f.items[0].ID), "2030-03-10T09:00:00Z")
			}
			before := deadlineWriteSnapshot(t, f.schedFixture)
			for _, preview := range []bool{true, false} {
				_, err := f.move(0, "target", "2030-03-05T10:00:00Z", func(in *models.LinkedMoveInput) {
					in.Scope, in.Preview, in.Revision, in.ConfirmPastDeadline = "series", preview, "x", !preview
				})
				wantFieldErr(t, err, "to", "range")
			}
			if !reflect.DeepEqual(before, deadlineWriteSnapshot(t, f.schedFixture)) {
				t.Fatal("refused series move wrote")
			}
			// Equal to the deadline is allowed.
			if _, _, err := f.seriesMove(0, "2030-03-05T09:00:00Z", nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMoveSeriesRevisionRequiredAndScopeBound(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 1)
	f.seriesSchedule(t, 0, models.ScheduleCreate{Dtstart: "2030-03-04T09:00", Tz: "UTC"})
	const to = "2030-03-04T14:00:00Z"
	series := func(in *models.LinkedMoveInput) { in.Scope = "series" }
	_, err := f.move(0, "target", to, series)
	wantFieldErr(t, err, "revision", "required")
	occ, err := f.move(0, "target", to, func(in *models.LinkedMoveInput) { in.Preview = true })
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.move(0, "target", to, func(in *models.LinkedMoveInput) { in.Scope, in.Revision = "series", occ.Revision }); !errors.Is(err, ErrCommitmentChanged) {
		t.Fatalf("occurrence token on a series apply: %v", err)
	}
	preview, err := f.move(0, "target", to, func(in *models.LinkedMoveInput) { in.Scope, in.Preview = "series", true })
	if err != nil {
		t.Fatal(err)
	}
	if preview.Revision == occ.Revision || len(preview.Revision) != 43 {
		t.Fatalf("series revision %q, occurrence %q", preview.Revision, occ.Revision)
	}
	// A roles write in the same second, which leaves every date and the
	// row's updated_at as they were, still makes the series preview stale.
	stamp := f.reload(t, f.items[0].ID).UpdatedAt
	if _, err := f.s.UpdateCommitment(f.ws.ID, f.items[0].ID, models.ScheduleUpdate{}, &models.CommitmentRolesPatch{Outcome: models.CommitmentRolePatchValue[string]{Present: true, Value: ptr("another outcome")}}); err != nil {
		t.Fatal(err)
	}
	f.exec(t, `UPDATE item_schedules SET updated_at=? WHERE id=?`, stamp, f.scheds[0].ID)
	if _, err = f.move(0, "target", to, func(in *models.LinkedMoveInput) { in.Scope, in.Revision = "series", preview.Revision }); !errors.Is(err, ErrCommitmentChanged) {
		t.Fatalf("stale series token: %v", err)
	}
	if got := f.targetOffset(t, 0); got != (models.TargetOffset{}) {
		t.Fatalf("refused apply wrote the offset: %+v", got)
	}
	if _, _, err := f.seriesMove(0, to, nil); err != nil {
		t.Fatal(err)
	}
}

func TestMoveSeriesJournalsUpdateUnderMoveRoot(t *testing.T) {
	t.Parallel()
	for _, reason := range []string{"Standup moved", ""} {
		f := newLinkFix(t, 1)
		f.seriesSchedule(t, 0, models.ScheduleCreate{Dtstart: "2030-03-04T09:00", Tz: "UTC"})
		key := f.outstandingKeyOf(t, 0)
		if _, _, err := f.seriesMove(0, "2030-03-04T14:00:00Z", func(in *models.LinkedMoveInput) { in.Reason = reason }); err != nil {
			t.Fatal(err)
		}
		var move, update *models.ScheduleChangeExport
		for _, c := range f.journal(t, f.scheds[0].ID) {
			c := c
			if c.Role == nil || *c.Role != "target" {
				continue
			}
			switch c.ChangeType {
			case "move":
				move = &c
			case "update":
				update = &c
			}
		}
		if move == nil || update == nil {
			t.Fatalf("reason %q: move=%+v update=%+v", reason, move, update)
		}
		wantWhy, wantSource := reason, "user"
		if reason == "" {
			wantWhy, wantSource = seriesWhy, "system"
		}
		if update.RootChangeID != move.ID || str(update.ParentChangeID) != move.ID || str(update.OccurrenceKey) != key ||
			update.Reason != wantWhy || update.ReasonSource != wantSource || update.Actor != "user" ||
			str(update.BeforeJSON) != `{"target_days":0,"target_secs":0}` || str(update.AfterJSON) != `{"target_days":0,"target_secs":18000}` {
			t.Fatalf("reason %q: update=%+v move=%+v", reason, update, move)
		}
		view, err := f.s.GetCommitment(f.ws.ID, f.items[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, c := range view.Journal {
			found = found || c.ID == update.ID
		}
		if !found {
			t.Fatalf("reason %q: the commitment journal omits the series row", reason)
		}
	}
}

func TestMoveSeriesKeepsHistoryAndBlocksUndo(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 1)
	f.seriesSchedule(t, 0, models.ScheduleCreate{Dtstart: "2030-03-04T09:00", Tz: "UTC"})
	f.resolve(t, 0, "done")
	if _, err := f.move(0, "target", "2030-03-05T11:00:00Z", nil); err != nil {
		t.Fatal(err)
	}
	view, err := f.s.GetCommitment(f.ws.ID, f.items[0].ID)
	if err != nil || view.LastResolution == nil || !view.LastResolution.CanReopen || view.LastResolution.State != "move" {
		t.Fatalf("occurrence move undo: %+v %v", view, err)
	}
	moveUndo := view.LastResolution.ResolutionID
	history := f.count(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id=? AND state='done'`, f.scheds[0].ID)
	checkpoints := f.count(t, `SELECT COUNT(*) FROM schedule_reopen_checkpoints WHERE schedule_id=?`, f.scheds[0].ID)
	if _, _, err := f.seriesMove(0, "2030-03-05T14:00:00Z", nil); err != nil {
		t.Fatal(err)
	}
	view, err = f.s.GetCommitment(f.ws.ID, f.items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if lr := view.LastResolution; lr == nil || lr.ResolutionID != moveUndo || lr.CanReopen {
		t.Fatalf("a series move left an undo: %+v", lr)
	}
	if _, err := f.s.ReopenOccurrence(f.ws.ID, f.items[0].ID, ReopenInput{ResolutionID: moveUndo, Actor: "user"}); err == nil {
		t.Fatal("the earlier occurrence move reopened across a series move")
	}
	if got := f.count(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id=? AND state='done'`, f.scheds[0].ID); got != history || history != 1 {
		t.Fatalf("done history %d -> %d", history, got)
	}
	if got := f.count(t, `SELECT COUNT(*) FROM schedule_reopen_checkpoints WHERE schedule_id=?`, f.scheds[0].ID); got != checkpoints {
		t.Fatalf("checkpoints %d -> %d", checkpoints, got)
	}
}

func TestMoveSeriesOffsetOnlyWhenTargetUnchanged(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 1)
	f.seriesSchedule(t, 0, models.ScheduleCreate{Dtstart: "2030-03-04T09:00", Tz: "UTC"})
	const to = "2030-03-04T14:00:00Z"
	if _, err := f.move(0, "target", to, nil); err != nil {
		t.Fatal(err)
	}
	moves := f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE change_type='move' AND schedule_id=?`, f.scheds[0].ID)
	preview, applied, err := f.seriesMove(0, to, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Changes) != 0 || len(applied.Changes) != 0 || preview.Series == nil || preview.Series.NewOffset.TargetSecs != 5*3600 {
		t.Fatalf("preview=%+v applied=%+v", preview, applied)
	}
	if got := f.targetOffset(t, 0); got.TargetSecs != 5*3600 {
		t.Fatalf("offset=%+v", got)
	}
	if got := f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE change_type='move' AND schedule_id=?`, f.scheds[0].ID); got != moves {
		t.Fatalf("move rows %d -> %d", moves, got)
	}
	var update *models.ScheduleChangeExport
	for _, c := range f.journal(t, f.scheds[0].ID) {
		c := c
		if c.ChangeType == "update" && c.Role != nil && *c.Role == "target" {
			update = &c
		}
	}
	if update == nil || update.ParentChangeID != nil || update.RootChangeID != update.ID {
		t.Fatalf("update=%+v", update)
	}
	if got := f.role(t, 0, "target"); got != to {
		t.Fatalf("target=%s", got)
	}
}

func TestMoveSeriesOneShot(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 1)
	if _, _, err := f.seriesMove(0, "2030-03-04T14:00:00Z", nil); err != nil {
		t.Fatal(err)
	}
	if got := f.targetOffset(t, 0); got != (models.TargetOffset{TargetSecs: 5 * 3600}) {
		t.Fatalf("offset=%+v", got)
	}
	if got := f.role(t, 0, "target"); got != "2030-03-04T14:00:00Z" {
		t.Fatalf("target=%s", got)
	}
}

func TestMoveSeriesEnded(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 1)
	f.resolve(t, 0, "done")
	if sc := f.reload(t, f.items[0].ID); sc.EndedAt == nil {
		t.Fatal("the one-shot did not end")
	}
	_, _, err := f.seriesMove(0, "2030-03-04T14:00:00Z", nil)
	wantFieldErr(t, err, "schedule", "ended")
}

// TASK-4713: every later occurrence's reminder follows the new offset.
func TestMoveSeriesLaterOccurrenceReminderFollowsOffset(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 1)
	f.seriesSchedule(t, 0, models.ScheduleCreate{Dtstart: "2030-03-04T09:00", Tz: "UTC", LeadMinutes: 30})
	if _, _, err := f.seriesMove(0, "2030-03-04T14:00:00Z", nil); err != nil {
		t.Fatal(err)
	}
	if got := f.remindAt(t, 0); got != "2030-03-04T13:30:00Z" {
		t.Fatalf("outstanding remind_at=%s", got)
	}
	f.resolve(t, 0, "done")
	if got := f.remindAt(t, 0); got != "2030-03-05T13:30:00Z" {
		t.Fatalf("next remind_at=%s, want start+offset-lead", got)
	}
}

// A series move that changes only the offset leaves an overridden outstanding
// row where its move armed it (here the target itself, inside the lead), and
// the next occurrence still arms at the new offset.
func TestMoveSeriesOffsetOnlyRetimesUnoverriddenRow(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 1)
	f.seriesSchedule(t, 0, models.ScheduleCreate{Dtstart: "2030-03-04T09:00", Tz: "UTC", LeadMinutes: 30})
	f.atClock(t, "2030-03-04T13:45:00Z")
	if _, err := f.move(0, "target", "2030-03-04T14:00:00Z", nil); err != nil {
		t.Fatal(err)
	}
	if got := f.remindAt(t, 0); got != "2030-03-04T14:00:00Z" {
		t.Fatalf("inside-lead move armed at %s", got)
	}
	if _, _, err := f.seriesMove(0, "2030-03-04T14:00:00Z", nil); err != nil {
		t.Fatal(err)
	}
	if got := f.targetOffset(t, 0); got != (models.TargetOffset{TargetSecs: 5 * 3600}) {
		t.Fatalf("offset=%+v", got)
	}
	if got := f.remindAt(t, 0); got != "2030-03-04T14:00:00Z" {
		t.Fatalf("overridden row retimed to %s", got)
	}
	f.resolve(t, 0, "done")
	if got := f.remindAt(t, 0); got != "2030-03-05T13:30:00Z" {
		t.Fatalf("next remind_at=%s", got)
	}
}
