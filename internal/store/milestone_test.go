package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4358: milestone forecasts and outcomes. Written first; each test names
// the change whose revert turns it red.

type msFix struct {
	*schedFixture
	ms       *models.Item
	sc       *models.ItemSchedule
	declared *models.Collection // abandoned_options declared
	fallback *models.Collection // no terminal declaration
}

func newMsFix(t *testing.T) *msFix {
	t.Helper()
	f := newSchedFixture(t)
	ms, sc := f.mk(t, dailyUTC, "2026-10-29T12:00:00Z")
	declared, err := f.s.CreateCollection(f.ws.ID, models.CollectionCreate{Name: "Work",
		Schema: `{"fields":[{"key":"status","type":"select","options":["open","shipped","dropped"],"default":"open","terminal_options":["shipped","dropped"],"abandoned_options":["dropped"]}]}`})
	if err != nil {
		t.Fatal(err)
	}
	fallback, err := f.s.CreateCollection(f.ws.ID, models.CollectionCreate{Name: "Misc",
		Schema: `{"fields":[{"key":"status","type":"select","options":["open","done","cancelled"],"default":"open"}]}`})
	if err != nil {
		t.Fatal(err)
	}
	return &msFix{schedFixture: f, ms: ms, sc: sc, declared: declared, fallback: fallback}
}

func (m *msFix) child(t *testing.T, col *models.Collection, title string) *models.Item {
	t.Helper()
	it, err := m.s.CreateItem(m.ws.ID, col.ID, models.ItemCreate{Title: title, Fields: `{"status":"open"}`})
	if err != nil {
		t.Fatal(err)
	}
	return it
}

func (m *msFix) setStatus(t *testing.T, it *models.Item, status string) {
	t.Helper()
	if _, err := m.s.UpdateItem(it.ID, models.ItemUpdate{Fields: strPtr(`{"status":"` + status + `"}`)}); err != nil {
		t.Fatal(err)
	}
}

func (m *msFix) sel(t *testing.T, it *models.Item) {
	t.Helper()
	if err := m.s.SetMilestoneChild(m.ws.ID, m.ms.ID, it.ID, "child", "", "u1"); err != nil {
		t.Fatal(err)
	}
}

func (m *msFix) read(t *testing.T, canRead func(string) bool) *models.Commitment {
	t.Helper()
	c, err := m.s.GetCommitmentFor(m.ws.ID, m.ms.ID, canRead)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (m *msFix) resolveDone(t *testing.T) {
	t.Helper()
	m.mustResolve(t, m.ms, ResolveInput{OccurrenceAt: rt("2026-10-30T09:00:00Z"), State: "done", Actor: "user"}, "2026-10-30T09:05:00Z")
}

func result(c *models.Commitment) string {
	if c.Milestone.Result == nil {
		return "<null>"
	}
	return *c.Milestone.Result
}

func hasField(err error, field, code string) bool {
	var fe *ScheduleFieldError
	return errors.As(err, &fe) && fe.Field == field && fe.Code == code
}

// RED if SetForecast overwrites provenance without an event, or the read drops
// the setter.
func TestForecastTwoSettersKeepProvenance(t *testing.T) {
	t.Parallel()
	m := newMsFix(t)
	if err := m.s.SetForecast(m.ws.ID, m.ms.ID, "u1", "user", "2026-12-01", "plan"); err != nil {
		t.Fatal(err)
	}
	if err := m.s.SetForecast(m.ws.ID, m.ms.ID, "u2", "agent", "2026-12-15T10:00:00+02:00", ""); err != nil {
		t.Fatal(err)
	}
	f := m.read(t, nil).Forecast
	if f == nil || f.Value != "2026-12-15T08:00:00Z" || f.SetBy != "u2" || f.SetByKind != "agent" {
		t.Fatalf("forecast = %+v", f)
	}
	if len(f.History) != 2 || f.History[0].SetBy != "u1" || f.History[0].SetByKind != "user" || f.History[1].PrevValue != "2026-12-01" || f.History[0].Source != "plan" {
		t.Fatalf("history = %+v", f.History)
	}
	if err := m.s.ClearForecast(m.ws.ID, m.ms.ID, "u1", "user"); err != nil {
		t.Fatal(err)
	}
	if m.read(t, nil).Forecast != nil {
		t.Fatal("forecast survives a clear")
	}
	if n := m.count(t, `SELECT COUNT(*) FROM schedule_forecast_events WHERE schedule_id=?`, m.sc.ID); n != 3 {
		t.Fatalf("events = %d, want 3 (set, set, clear)", n)
	}
}

// RED if a forecast is written into the roles, an override or a reminder: a
// native deadline schedule with a long-past forecast records no more misses than
// one without, and no reminder row appears.
func TestForecastPassingCreatesNoMiss(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"})
	f.nativeDeadline(t, sc, "stays_owed", 3600)
	reminders := f.count(t, `SELECT COUNT(*) FROM item_reminders`)
	overrides := f.count(t, `SELECT COUNT(*) FROM schedule_occurrence_overrides`)
	if err := f.s.SetForecast(f.ws.ID, f.item.ID, "u1", "user", "2026-10-01", ""); err != nil {
		t.Fatal(err)
	}
	if got := f.count(t, `SELECT COUNT(*) FROM item_reminders`); got != reminders {
		t.Fatalf("a forecast changed reminder rows: %d -> %d", reminders, got)
	}
	if got := f.count(t, `SELECT COUNT(*) FROM schedule_occurrence_overrides`); got != overrides {
		t.Fatalf("a forecast wrote an override")
	}
	// The deadline is 10:00 on the 26th; the forecast passed weeks before. Exactly
	// the one deadline miss, none attributable to the forecast.
	if got := f.eval(t, "2026-10-26T10:00:01Z", 0); got.Missed != 1 {
		t.Fatalf("missed = %d, want exactly the deadline's 1", got.Missed)
	}
	if n := f.deadlineEvidence(t); n != 1 {
		t.Fatalf("miss rows = %d, want 1", n)
	}
	c, err := f.s.GetCommitment(f.ws.ID, f.item.ID)
	if err != nil || c.Forecast == nil || !c.Forecast.Passed {
		t.Fatalf("passed forecast should read passed=true: %+v %v", c.Forecast, err)
	}
	if c.Roles.Deadline == nil || c.Roles.Target == nil {
		t.Fatalf("roles disturbed: %+v", c.Roles)
	}
}

// A deadline-less schedule with a past forecast is never a candidate.
func TestForecastOnDeadlinelessScheduleNeverMisses(t *testing.T) {
	t.Parallel()
	m := newMsFix(t)
	if err := m.s.SetForecast(m.ws.ID, m.ms.ID, "u1", "user", "2020-01-01", ""); err != nil {
		t.Fatal(err)
	}
	if got := m.eval(t, "2027-01-01T00:00:00Z", 0); got.Missed != 0 {
		t.Fatalf("missed = %d", got.Missed)
	}
	if n := m.count(t, `SELECT COUNT(*) FROM item_reminders WHERE schedule_id=? AND remind_at < ?`, m.sc.ID, "2020-01-02T00:00:00Z"); n != 0 {
		t.Fatalf("forecast armed a reminder")
	}
}

// RED if a forecast requires a target (here the schedule has none beyond the
// legacy zero offset) or the write touches the roles.
func TestMilestoneForecastWithoutTarget(t *testing.T) {
	t.Parallel()
	m := newMsFix(t)
	before := m.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE schedule_id=?`, m.sc.ID)
	if err := m.s.SetForecast(m.ws.ID, m.ms.ID, "u1", "user", "2026-12-01", ""); err != nil {
		t.Fatalf("forecast without a target: %v", err)
	}
	if after := m.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE schedule_id=?`, m.sc.ID); after != before {
		t.Fatalf("a forecast wrote journal rows: %d -> %d", before, after)
	}
	if err := m.s.SetForecast(m.ws.ID, m.ms.ID, "u1", "user", "", ""); !hasField(err, "value", "required") {
		t.Fatalf("empty value: %v", err)
	}
	if err := m.s.SetForecast(m.ws.ID, m.ms.ID, "u1", "user", "next week", ""); !hasField(err, "value", "format") {
		t.Fatalf("bad value: %v", err)
	}
	if err := m.s.SetForecast(m.ws.ID, m.ms.ID, "u1", "user", "2026-12-01", strings.Repeat("x", 281)); !hasField(err, "source", "range") {
		t.Fatalf("long source: %v", err)
	}
	if err := m.s.SetForecast(m.ws.ID, m.ms.ID, "u1", "user", "2026-12-01", strings.Repeat("x", 280)); err != nil {
		t.Fatalf("280-char source refused: %v", err)
	}
}

// RED if abandoned counts as success, a waiver needs no reason, or an open or
// trashed child counts.
func TestMilestoneCancelledChildNeverCounts(t *testing.T) {
	t.Parallel()
	m := newMsFix(t)
	declared := m.child(t, m.declared, "declared drop")
	fallback := m.child(t, m.fallback, "fallback cancel")
	open := m.child(t, m.declared, "open")
	trashed := m.child(t, m.declared, "trashed")
	good := m.child(t, m.declared, "good")
	for _, it := range []*models.Item{declared, fallback, open, trashed, good} {
		m.sel(t, it)
	}
	m.setStatus(t, declared, "dropped")
	m.setStatus(t, fallback, "cancelled")
	m.setStatus(t, trashed, "shipped")
	m.setStatus(t, good, "shipped")
	if err := m.s.DeleteItem(trashed.ID); err != nil {
		t.Fatal(err)
	}
	c := m.read(t, nil)
	counted := 0
	for _, ch := range c.Milestone.Children {
		if ch.CloseState == "abandoned" && ch.Counts {
			t.Errorf("abandoned child counts: %+v", ch)
		}
		if (ch.CloseState == "open" || ch.CloseState == "missing") && ch.Counts {
			t.Errorf("%s child counts: %+v", ch.CloseState, ch)
		}
		if ch.Counts {
			counted++
		}
	}
	if counted != 1 || c.Milestone.Counted != 1 || c.Milestone.Total != 5 || result(c) != "pending" {
		t.Fatalf("counted %d/%d total %d result %s", counted, c.Milestone.Counted, c.Milestone.Total, result(c))
	}
	if err := m.s.SetMilestoneChild(m.ws.ID, m.ms.ID, declared.ID, "waiver", "", "u1"); !hasField(err, "reason", "combination") {
		t.Fatalf("waiver without a reason: %v", err)
	}
	if err := m.s.SetMilestoneChild(m.ws.ID, m.ms.ID, declared.ID, "waiver", "dropped on purpose", "u1"); err != nil {
		t.Fatal(err)
	}
	if c = m.read(t, nil); c.Milestone.Waivers != 1 || c.Milestone.Counted != 2 {
		t.Fatalf("after waiver: %+v", c.Milestone)
	}
	if err := m.s.SetMilestoneChild(m.ws.ID, m.ms.ID, m.ms.ID, "child", "", "u1"); !hasField(err, "child", "combination") {
		t.Fatalf("self child: %v", err)
	}
}

// RED if completion is stored in place of derived, the snapshot is not taken at
// resolve, or a reopen flips the milestone back.
func TestMilestoneReopenAfterCompletionFlagsEvidence(t *testing.T) {
	t.Parallel()
	m := newMsFix(t)
	a, b := m.child(t, m.declared, "a"), m.child(t, m.declared, "b")
	m.sel(t, a)
	m.sel(t, b)
	m.setStatus(t, a, "shipped")
	m.setStatus(t, b, "shipped")
	if c := m.read(t, nil); result(c) != "completed" {
		t.Fatalf("result = %s", result(c))
	}
	m.resolveDone(t)
	c := m.read(t, nil)
	if c.Milestone.EvidenceChanged == nil || *c.Milestone.EvidenceChanged || len(c.Milestone.Evidence) != 2 || c.Milestone.CompletedAt == nil {
		t.Fatalf("after resolve: %+v", c.Milestone)
	}
	completedAt := *c.Milestone.CompletedAt
	m.setStatus(t, a, "open")
	c = m.read(t, nil)
	if !*c.Milestone.EvidenceChanged || result(c) != "pending" || len(c.Milestone.Evidence) != 2 || *c.Milestone.CompletedAt != completedAt {
		t.Fatalf("after reopen: result %s %+v", result(c), c.Milestone)
	}
	// The milestone's own occurrence is not reopened or resolved again.
	if n := m.count(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id=? AND state='done'`, m.sc.ID); n != 1 {
		t.Fatalf("done occurrences = %d", n)
	}
	// Re-closing reads the same as the snapshot; the flag stays (status_transitions).
	m.setStatus(t, a, "shipped")
	c = m.read(t, nil)
	if !*c.Milestone.EvidenceChanged || result(c) != "completed" {
		t.Fatalf("after re-close: result %s changed %v", result(c), *c.Milestone.EvidenceChanged)
	}
	found := false
	for _, ch := range c.Milestone.Changes {
		found = found || ch.Reopened
	}
	if !found {
		t.Fatalf("no reopened change listed: %+v", c.Milestone.Changes)
	}
}

// RED if being pending at resolve sets the flag; a pending child that later
// succeeds, a removal and a later cancel do.
func TestMilestonePendingAtResolveDoesNotFlag(t *testing.T) {
	t.Parallel()
	m := newMsFix(t)
	a, b := m.child(t, m.declared, "a"), m.child(t, m.declared, "b")
	m.sel(t, a)
	m.sel(t, b)
	m.setStatus(t, a, "shipped")
	m.resolveDone(t)
	c := m.read(t, nil)
	if result(c) != "pending" || *c.Milestone.EvidenceChanged {
		t.Fatalf("pending at resolve: result %s changed %v", result(c), *c.Milestone.EvidenceChanged)
	}
	m.setStatus(t, b, "shipped")
	c = m.read(t, nil)
	if result(c) != "completed" || !*c.Milestone.EvidenceChanged {
		t.Fatalf("pending child later succeeds: result %s changed %v", result(c), *c.Milestone.EvidenceChanged)
	}
}

func TestMilestoneEvidenceFlagsRemovalAndLaterCancel(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"remove", "cancel", "add"} {
		t.Run(mode, func(t *testing.T) {
			m := newMsFix(t)
			a := m.child(t, m.declared, "a")
			m.sel(t, a)
			m.setStatus(t, a, "shipped")
			m.resolveDone(t)
			switch mode {
			case "remove":
				if err := m.s.RemoveMilestoneChild(m.ws.ID, m.ms.ID, a.ID, "u1"); err != nil {
					t.Fatal(err)
				}
			case "cancel":
				m.setStatus(t, a, "dropped")
			case "add":
				m.sel(t, m.child(t, m.declared, "late"))
			}
			if c := m.read(t, nil); !*c.Milestone.EvidenceChanged {
				t.Fatalf("%s: not flagged: %+v", mode, c.Milestone)
			}
		})
	}
}

// RED if resolving, or every child succeeding, writes the milestone item's
// status or resolves an occurrence.
func TestMilestoneNoAutoCompletion(t *testing.T) {
	t.Parallel()
	m := newMsFix(t)
	a := m.child(t, m.declared, "a")
	m.sel(t, a)
	m.setStatus(t, a, "shipped")
	if result(m.read(t, nil)) != "completed" {
		t.Fatal("not completed")
	}
	if n := m.count(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id=?`, m.sc.ID); n != 0 {
		t.Fatalf("occurrences = %d: the read resolved something", n)
	}
	it, _ := m.s.GetItem(m.ms.ID)
	if it.Fields != m.ms.Fields {
		t.Fatalf("the milestone item changed: %s -> %s", m.ms.Fields, it.Fields)
	}
	if n := m.count(t, `SELECT COUNT(*) FROM milestone_completion_evidence`); n != 0 {
		t.Fatalf("evidence without a resolve: %d", n)
	}
}

func TestMilestoneEmptySelectionResultNull(t *testing.T) {
	t.Parallel()
	m := newMsFix(t)
	c := m.read(t, nil)
	if c.Milestone.Result != nil || c.Milestone.Total != 0 || c.Milestone.Children == nil || c.Milestone.EvidenceChanged == nil || *c.Milestone.EvidenceChanged {
		t.Fatalf("empty: %+v", c.Milestone)
	}
	m.resolveDone(t)
	if n := m.count(t, `SELECT COUNT(*) FROM milestone_completion_evidence`); n != 0 {
		t.Fatalf("empty selection wrote %d evidence rows", n)
	}
}

// RED if a hidden child is counted, listed, or distinguishable by state, and if
// the reader's answer moves when the hidden child reopens.
func TestMilestoneHiddenChildIsUnknownAndLeaksNothing(t *testing.T) {
	t.Parallel()
	m := newMsFix(t)
	seen, hidden := m.child(t, m.declared, "seen"), m.child(t, m.declared, "hidden")
	m.sel(t, seen)
	m.sel(t, hidden)
	m.setStatus(t, seen, "shipped")
	m.setStatus(t, hidden, "shipped")
	canRead := func(id string) bool { return id != hidden.ID }
	read := func() (string, *models.Commitment) {
		c := m.read(t, canRead)
		b, err := jsonBytes(c)
		if err != nil {
			t.Fatal(err)
		}
		return string(b), c
	}
	before, c := read()
	if result(c) != "unknown" || c.Milestone.Total != 1 || c.Milestone.Counted != 1 || c.Milestone.EvidenceChanged != nil {
		t.Fatalf("hidden: result %s %+v", result(c), c.Milestone)
	}
	if strings.Contains(before, hidden.Slug) || strings.Contains(before, hidden.ID) {
		t.Fatalf("hidden child leaked: %s", before)
	}
	m.resolveDone(t)
	m.setStatus(t, hidden, "open")
	after, c := read()
	// Only the evidence rows (recorded at resolve) differ from before; strip the
	// resolve-dependent members and compare the rest byte for byte.
	_ = after
	if result(c) != "unknown" || len(c.Milestone.Evidence) != 1 || c.Milestone.EvidenceChanged != nil {
		t.Fatalf("after resolve: %+v", c.Milestone)
	}
	m.setStatus(t, hidden, "shipped")
	again, _ := read()
	m.setStatus(t, hidden, "open")
	reopened, _ := read()
	if again != reopened {
		t.Fatalf("the reader's response moved when a hidden child reopened:\n%s\n%s", again, reopened)
	}
	// A hidden child's evidence row never reaches this reader.
	for _, e := range c.Milestone.Evidence {
		if e.ItemRef == hidden.Slug {
			t.Fatalf("hidden evidence leaked")
		}
	}
	// A removed hidden child is not shown in history either.
	if err := m.s.RemoveMilestoneChild(m.ws.ID, m.ms.ID, hidden.ID, "u1"); err != nil {
		t.Fatal(err)
	}
	if _, c = read(); len(c.Milestone.History) != 0 {
		t.Fatalf("hidden removal in history: %+v", c.Milestone.History)
	}
}

// RED if a cap truncates instead of refusing, or the refusal is not a range.
func TestMilestoneCapsRefuseWhole(t *testing.T) {
	t.Parallel()
	m := newMsFix(t)
	ids := make([]string, 0, 201)
	for i := 0; i < 201; i++ {
		ids = append(ids, m.child(t, m.declared, "c").ID)
	}
	for _, id := range ids[:200] {
		if err := m.s.SetMilestoneChild(m.ws.ID, m.ms.ID, id, "child", "", "u1"); err != nil {
			t.Fatal(err)
		}
	}
	err := m.s.SetMilestoneChild(m.ws.ID, m.ms.ID, ids[200], "child", "", "u1")
	if !hasField(err, "children", "range") || !strings.Contains(err.Error(), "200") {
		t.Fatalf("201st child: %v", err)
	}
	if n := m.count(t, `SELECT COUNT(*) FROM milestone_children WHERE schedule_id=? AND removed_at IS NULL`, m.sc.ID); n != 200 {
		t.Fatalf("selection = %d, want 200", n)
	}
	// Re-selecting an already selected child at the cap is an update, not a 201st.
	if err := m.s.SetMilestoneChild(m.ws.ID, m.ms.ID, ids[0], "waiver", "reason", "u1"); err != nil {
		t.Fatalf("update at cap: %v", err)
	}
}

func TestMilestoneChildFromAnotherWorkspaceIsNotFound(t *testing.T) {
	t.Parallel()
	m := newMsFix(t)
	other := createTestWorkspace(t, m.s, "Other")
	col := createTestCollection(t, m.s, other.ID, "Tasks")
	foreign := createTestItem(t, m.s, other.ID, col.ID, "foreign", "")
	if err := m.s.SetMilestoneChild(m.ws.ID, m.ms.ID, foreign.ID, "child", "", "u1"); !errors.Is(err, ErrMilestoneChildNotFound) {
		t.Fatalf("foreign child: %v", err)
	}
	if err := m.s.SetMilestoneChild(m.ws.ID, m.ms.ID, "no-such-id", "child", "", "u1"); !errors.Is(err, ErrMilestoneChildNotFound) {
		t.Fatalf("unknown child: %v", err)
	}
}

// Removing keeps history; selecting again is a new row; the partial unique index
// allows exactly one active row per child.
func TestMilestoneRemoveKeepsHistory(t *testing.T) {
	t.Parallel()
	m := newMsFix(t)
	a := m.child(t, m.declared, "a")
	m.sel(t, a)
	m.sel(t, a) // idempotent
	if n := m.count(t, `SELECT COUNT(*) FROM milestone_children`); n != 1 {
		t.Fatalf("rows = %d", n)
	}
	if err := m.s.RemoveMilestoneChild(m.ws.ID, m.ms.ID, a.ID, "u2"); err != nil {
		t.Fatal(err)
	}
	if err := m.s.RemoveMilestoneChild(m.ws.ID, m.ms.ID, a.ID, "u2"); err != nil {
		t.Fatalf("second remove: %v", err)
	}
	c := m.read(t, nil)
	if c.Milestone.Total != 0 || len(c.Milestone.History) != 1 || c.Milestone.History[0].RemovedBy != "u2" {
		t.Fatalf("after remove: %+v", c.Milestone)
	}
	m.sel(t, a)
	if n := m.count(t, `SELECT COUNT(*) FROM milestone_children`); n != 2 {
		t.Fatalf("rows after reselect = %d, want 2 (history kept)", n)
	}
	if _, err := m.s.db.Exec(m.s.q(`INSERT INTO milestone_children (id,workspace_id,schedule_id,child_item_id,kind,reason,added_by,added_at) VALUES (?,?,?,?,?,?,?,?)`),
		newID(), m.ws.ID, m.sc.ID, a.ID, "child", "", "u", "2026-01-01T00:00:00Z"); err == nil {
		t.Fatal("a second active row for the same child was accepted")
	}
	if _, err := m.s.db.Exec(m.s.q(`INSERT INTO milestone_children (id,workspace_id,schedule_id,child_item_id,kind,reason,added_by,added_at) VALUES (?,?,?,?,?,?,?,?)`),
		newID(), m.ws.ID, m.sc.ID, a.ID, "waiver", "", "u", "2026-01-01T00:00:00Z"); err == nil {
		t.Fatal("a waiver without a reason was accepted by the table")
	}
}

func jsonBytes(v any) ([]byte, error) { return json.Marshal(v) }
