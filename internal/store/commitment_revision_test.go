package store

import (
	"errors"
	"sort"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// checkCommitment is a native check commitment without a deadline, so a timing
// or lead edit is not refused as a hard-deadline renegotiation.
func (f *schedFixture) checkCommitment(t *testing.T) *models.ItemSchedule {
	t.Helper()
	roles := nativeRoles()
	roles.DeadlineDays, roles.DeadlineSecs, roles.DeadlineBehavior = nil, nil, nil
	sc, err := f.s.CreateCommitmentSchedule(f.ws.ID, f.item.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2031-03-10T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"}, roles)
	if err != nil {
		t.Fatalf("CreateCommitmentSchedule: %v", err)
	}
	return sc
}

func (f *schedFixture) revision(t *testing.T) string {
	t.Helper()
	sc, rev, err := f.s.GetScheduleRevision(f.ws.ID, f.item.ID)
	if err != nil || sc == nil {
		t.Fatalf("GetScheduleRevision: %v %v", sc, err)
	}
	return rev
}

func TestCommitmentRevisionCoversRowAndOverrides(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.checkCommitment(t)
	r0 := f.revision(t)
	if len(r0) != 43 {
		t.Fatalf("token %q: want 43 characters", r0)
	}
	if again := f.revision(t); again != r0 {
		t.Fatalf("second read %q, want %q", again, r0)
	}

	f.exec(t, `UPDATE item_schedules SET lead_minutes=7 WHERE id=?`, sc.ID)
	if r1 := f.revision(t); r1 == r0 {
		t.Fatal("a row change kept the token")
	}
	f.exec(t, `UPDATE item_schedules SET lead_minutes=? WHERE id=?`, sc.LeadMinutes, sc.ID)
	if back := f.revision(t); back != r0 {
		t.Fatalf("restored row gives %q, want %q", back, r0)
	}

	// A key that is not the outstanding occurrence still counts.
	f.exec(t, `INSERT INTO schedule_occurrence_overrides (id,schedule_id,occurrence_key,role,value_at,created_at,updated_at) VALUES (?,?,?,'target',?,?,?)`,
		newID(), sc.ID, "2031-04-01T09:00:00Z|2031-04-01T09:00:00", "2031-04-02T09:00:00Z", "2031-03-01T00:00:00Z", "2031-03-01T00:00:00Z")
	r2 := f.revision(t)
	if r2 == r0 {
		t.Fatal("an override insert kept the token")
	}

	// A reminder fire is not a commitment edit.
	f.exec(t, `UPDATE item_reminders SET fired_at=? WHERE schedule_id=?`, "2031-03-10T09:00:00Z", sc.ID)
	if r3 := f.revision(t); r3 != r2 {
		t.Fatalf("a reminder fire moved the token: %q -> %q", r2, r3)
	}
}

func TestCommitmentRevisionCoversEveryScheduleColumn(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	if s.dialect.Driver() == DriverPostgres {
		t.Skip("PRAGMA table_info is SQLite only")
	}
	rows, err := s.db.Query(`PRAGMA table_info(item_schedules)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var (
			cid, notNull, pk int
			name, typ        string
			def              any
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &def, &pk); err != nil {
			t.Fatal(err)
		}
		got = append(got, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := append([]string(nil), commitmentRevisionColumns...)
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("item_schedules columns %v, commitmentRevisionColumns %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("item_schedules columns %v, commitmentRevisionColumns %v", got, want)
		}
	}
}

func TestUpdateCommitmentExpectingRefusesStaleRevision(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.checkCommitment(t)
	r0 := f.revision(t)
	five, ten := 5, 10
	if _, err := f.s.UpdateCommitment(f.ws.ID, f.item.ID, models.ScheduleUpdate{LeadMinutes: &five}, nil); err != nil {
		t.Fatal(err)
	}
	journalN := len(f.journal(t, sc.ID))
	changes := f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE schedule_id=?`, sc.ID)

	_, err := f.s.UpdateCommitmentExpecting(f.ws.ID, f.item.ID, models.ScheduleUpdate{LeadMinutes: &ten}, nil, r0)
	if !errors.Is(err, ErrCommitmentRevisionStale) {
		t.Fatalf("stale token: err=%v, want ErrCommitmentRevisionStale", err)
	}
	if got := f.reload(t, f.item.ID).LeadMinutes; got != 5 {
		t.Fatalf("lead_minutes=%d after a refused write, want 5", got)
	}
	if n := len(f.journal(t, sc.ID)); n != journalN {
		t.Fatalf("journal grew %d -> %d on a refused write", journalN, n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE schedule_id=?`, sc.ID); n != changes {
		t.Fatalf("schedule_changes %d -> %d on a refused write", changes, n)
	}

	fresh := f.revision(t)
	got, err := f.s.UpdateCommitmentExpecting(f.ws.ID, f.item.ID, models.ScheduleUpdate{LeadMinutes: &ten}, nil, fresh)
	if err != nil || got.LeadMinutes != 10 {
		t.Fatalf("fresh token: %v %v", got, err)
	}
	if got, err := f.s.UpdateCommitmentExpecting(f.ws.ID, f.item.ID, models.ScheduleUpdate{LeadMinutes: &five}, nil, ""); err != nil || got.LeadMinutes != 5 {
		t.Fatalf("no token: %v %v", got, err)
	}
}

func TestUpdateCommitmentExpectingStaleBeatsHardDeadline(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.nativeCommit(t)
	r0 := f.revision(t)
	f.exec(t, `UPDATE item_schedules SET lead_minutes=3 WHERE id=?`, sc.ID)
	start := "2031-03-10T09:00"
	if _, err := f.s.UpdateCommitment(f.ws.ID, f.item.ID, models.ScheduleUpdate{Dtstart: &start}, nil); !errors.Is(err, ErrHardDeadlineRenegotiate) {
		t.Fatalf("control: err=%v, want ErrHardDeadlineRenegotiate", err)
	}
	_, err := f.s.UpdateCommitmentExpecting(f.ws.ID, f.item.ID, models.ScheduleUpdate{Dtstart: &start}, nil, r0)
	if !errors.Is(err, ErrCommitmentRevisionStale) {
		t.Fatalf("err=%v, want ErrCommitmentRevisionStale", err)
	}
}

func TestRenegotiateExpectedRevision(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.nativeCommit(t)
	r0 := f.revision(t)
	in := models.CommitmentRenegotiateInput{Scope: "series", Reason: "agreed", DeadlineBehavior: ptr("lapses"), Preview: true, ExpectedRevision: r0}

	// (a) A current token previews.
	if _, err := f.s.renegotiateCommitmentAt(f.ws.ID, f.item.ID, "user", in, schedNow, nil); err != nil {
		t.Fatalf("preview with a current token: %v", err)
	}

	// (b) A stale token refuses the preview.
	f.exec(t, `UPDATE item_schedules SET lead_minutes=3 WHERE id=?`, sc.ID)
	if _, err := f.s.renegotiateCommitmentAt(f.ws.ID, f.item.ID, "user", in, schedNow, nil); !errors.Is(err, ErrCommitmentRevisionStale) {
		t.Fatalf("stale preview: err=%v, want ErrCommitmentRevisionStale", err)
	}

	// (c) A valid preview revision does not rescue a stale expected_revision.
	in.ExpectedRevision = ""
	p, err := f.s.renegotiateCommitmentAt(f.ws.ID, f.item.ID, "user", in, schedNow, nil)
	if err != nil {
		t.Fatalf("preview without a token: %v", err)
	}
	journalN := len(f.journal(t, sc.ID))
	overrides := f.count(t, `SELECT COUNT(*) FROM schedule_occurrence_overrides WHERE schedule_id=?`, sc.ID)
	apply := in
	apply.Preview, apply.Revision, apply.ExpectedRevision = false, p.Revision, r0
	if _, err := f.s.renegotiateCommitmentAt(f.ws.ID, f.item.ID, "user", apply, schedNow, nil); !errors.Is(err, ErrCommitmentRevisionStale) {
		t.Fatalf("stale apply: err=%v, want ErrCommitmentRevisionStale", err)
	}
	if n := len(f.journal(t, sc.ID)); n != journalN {
		t.Fatalf("journal grew %d -> %d on a refused apply", journalN, n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_occurrence_overrides WHERE schedule_id=?`, sc.ID); n != overrides {
		t.Fatalf("overrides %d -> %d on a refused apply", overrides, n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM item_schedules WHERE id=? AND deadline_behavior='stays_owed'`, sc.ID); n != 1 {
		t.Fatal("deadline_behavior changed on a refused apply")
	}

	// (d) The ended refusal comes before the token check.
	f.exec(t, `UPDATE item_schedules SET ended_at=?, next_at=NULL WHERE id=?`, "2026-10-27T00:00:00Z", sc.ID)
	in.ExpectedRevision = r0
	_, err = f.s.renegotiateCommitmentAt(f.ws.ID, f.item.ID, "user", in, schedNow, nil)
	var field *ScheduleFieldError
	if !errors.As(err, &field) || field.Field != "schedule" || field.Code != "ended" {
		t.Fatalf("ended: err=%v, want schedule/ended", err)
	}
}
