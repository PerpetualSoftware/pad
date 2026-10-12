package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// PLAN-3598 role backfill. Each of the lead's terms is pinned here: a valid
// field value is copied into an EMPTY column; a set column is never
// overwritten; the field is never cleared; a value naming no live role is
// reported, not copied. Plus the seq bump, the system activity row, the
// per-boot invalid cap, and the conditional write.

type crbFixture struct {
	s                  *Store
	ws                 *models.Workspace
	conventions, tasks *models.Collection
	implementer, rev   *models.AgentRole
}

func newCRBFixture(t *testing.T, slug string) *crbFixture {
	t.Helper()
	s := testStore(t)
	ws := newTestWorkspace(t, s, slug)
	if err := s.SeedCollectionsFromTemplate(ws.ID, "startup"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	conv, err := s.GetCollectionBySlug(ws.ID, "conventions")
	if err != nil {
		t.Fatalf("conventions collection: %v", err)
	}
	tasks, err := s.GetCollectionBySlug(ws.ID, "tasks")
	if err != nil {
		t.Fatalf("tasks collection: %v", err)
	}
	impl, err := s.CreateAgentRole(ws.ID, models.AgentRoleCreate{Name: "Implementer", Slug: "implementer"})
	if err != nil {
		t.Fatalf("role: %v", err)
	}
	rev, err := s.CreateAgentRole(ws.ID, models.AgentRoleCreate{Name: "Reviewer", Slug: "reviewer"})
	if err != nil {
		t.Fatalf("role: %v", err)
	}
	return &crbFixture{s: s, ws: ws, conventions: conv, tasks: tasks, implementer: impl, rev: rev}
}

func (f *crbFixture) item(t *testing.T, coll *models.Collection, title, role string, column *string) *models.Item {
	t.Helper()
	fields := map[string]any{"status": "active", "trigger": "always"}
	if role != "" {
		fields["role"] = role
	}
	b, _ := json.Marshal(fields)
	it, err := f.s.CreateItem(f.ws.ID, coll.ID, models.ItemCreate{Title: title, Fields: string(b), AgentRoleID: column})
	if err != nil {
		t.Fatalf("create %s: %v", title, err)
	}
	return it
}

func (f *crbFixture) row(t *testing.T, id string) (roleID string, seq int64, role string) {
	t.Helper()
	it, err := f.s.GetItem(id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	if it.AgentRoleID != nil {
		roleID = *it.AgentRoleID
	}
	return roleID, it.Seq, conventionRoleFieldValue(it.Fields)
}

func TestBackfillConventionRoles_AdditiveOnly(t *testing.T) {
	t.Parallel()
	f := newCRBFixture(t, "crb-additive")
	revID := f.rev.ID

	valid := f.item(t, f.conventions, "valid field, empty column", "implementer", nil)
	set := f.item(t, f.conventions, "column already set", "implementer", &revID)
	cased := f.item(t, f.conventions, "wrong case", "Implementer", nil)
	ghost := f.item(t, f.conventions, "no such role", "ghost", nil)
	none := f.item(t, f.conventions, "no field", "", nil)
	task := f.item(t, f.tasks, "a task, not a convention", "implementer", nil)

	_, validSeq0, _ := f.row(t, valid.ID)
	_, setSeq0, _ := f.row(t, set.ID)
	_, taskSeq0, _ := f.row(t, task.ID)

	res, err := f.s.BackfillConventionRoles()
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if res.Copied != 1 || res.Invalid != 2 || res.Raced != 0 {
		t.Fatalf("result = %+v, want Copied 1, Invalid 2, Raced 0", res)
	}

	// A valid field value fills the EMPTY column, moves the seq, keeps the field.
	roleID, seq, field := f.row(t, valid.ID)
	if roleID != f.implementer.ID {
		t.Errorf("valid: column = %q, want %q", roleID, f.implementer.ID)
	}
	if seq <= validSeq0 {
		t.Errorf("valid: seq %d did not move past %d", seq, validSeq0)
	}
	if field != "implementer" {
		t.Errorf("valid: field = %q, want it left as implementer", field)
	}

	// A set column is never overwritten, and its seq does not move.
	roleID, seq, field = f.row(t, set.ID)
	if roleID != revID || seq != setSeq0 || field != "implementer" {
		t.Errorf("set column: column %q seq %d field %q, want %q %d implementer", roleID, seq, field, revID, setSeq0)
	}

	// Invalid values are reported, not copied, and the field stays.
	for _, it := range []*models.Item{cased, ghost} {
		roleID, _, field = f.row(t, it.ID)
		if roleID != "" {
			t.Errorf("%s: column = %q, want empty", it.Title, roleID)
		}
		if field == "" {
			t.Errorf("%s: field was cleared", it.Title)
		}
	}
	got := map[string]string{}
	for _, inv := range res.InvalidSample {
		got[inv.ItemID] = inv.Value
	}
	if got[cased.ID] != "Implementer" || got[ghost.ID] != "ghost" || len(got) != 2 {
		t.Errorf("InvalidSample = %+v, want the wrong-case and ghost rows", res.InvalidSample)
	}

	// A convention with no field and a task with one are not candidates.
	if roleID, _, _ = f.row(t, none.ID); roleID != "" {
		t.Errorf("no field: column = %q", roleID)
	}
	if roleID, seq, _ = f.row(t, task.ID); roleID != "" || seq != taskSeq0 {
		t.Errorf("task: column %q seq %d, want untouched", roleID, seq)
	}

	// A second boot copies nothing, moves no seq, and reports the invalid
	// rows again (they never self-heal).
	_, validSeq1, _ := f.row(t, valid.ID)
	res2, err := f.s.BackfillConventionRoles()
	if err != nil {
		t.Fatalf("second backfill: %v", err)
	}
	if res2.Copied != 0 || res2.Invalid != 2 {
		t.Fatalf("second run = %+v, want Copied 0, Invalid 2", res2)
	}
	if _, seq, _ = f.row(t, valid.ID); seq != validSeq1 {
		t.Errorf("second run moved seq %d -> %d", validSeq1, seq)
	}
}

func TestBackfillConventionRoles_ActivityIsSystemAndStamped(t *testing.T) {
	t.Parallel()
	f := newCRBFixture(t, "crb-activity")
	it := f.item(t, f.conventions, "gets a role", "implementer", nil)
	_, prev, _ := f.row(t, it.ID)

	if _, err := f.s.BackfillConventionRoles(); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	_, seq, _ := f.row(t, it.ID)

	var actor, source, action, meta string
	err := f.s.db.QueryRow(f.s.q(`SELECT actor, source, action, metadata FROM activities
		WHERE document_id = ? AND actor = 'system'`), it.ID).Scan(&actor, &source, &action, &meta)
	if err != nil {
		t.Fatalf("system activity row: %v", err)
	}
	if source != "system" || action != "updated" {
		t.Errorf("activity source %q action %q, want system / updated", source, action)
	}
	gotSeq, gotPrev, _, broken, ok := seqStamp(meta)
	if !ok || broken || gotSeq != seq || gotPrev != prev {
		t.Errorf("stamp = seq %d prev %d ok %v broken %v, want seq %d prev %d (meta %s)", gotSeq, gotPrev, ok, broken, seq, prev, meta)
	}
}

func TestBackfillConventionRoles_ConditionalOnColumnAndSeq(t *testing.T) {
	t.Parallel()
	f := newCRBFixture(t, "crb-race")
	it := f.item(t, f.conventions, "raced", "implementer", nil)
	_, seq, _ := f.row(t, it.ID)

	// A stale seq: a writer moved the row after the read.
	_, err := f.s.copyConventionRole(f.ws.ID, conventionRoleCandidate{id: it.ID, value: "implementer", seq: seq - 1}, f.implementer.ID, "Implementer")
	if !errors.Is(err, errConventionRoleRaced) {
		t.Fatalf("stale seq: err = %v, want errConventionRoleRaced", err)
	}
	// A column set after the read, at the same seq.
	if _, err := f.s.db.Exec(f.s.q(`UPDATE items SET agent_role_id = ? WHERE id = ?`), f.rev.ID, it.ID); err != nil {
		t.Fatal(err)
	}
	_, err = f.s.copyConventionRole(f.ws.ID, conventionRoleCandidate{id: it.ID, value: "implementer", seq: seq}, f.implementer.ID, "Implementer")
	if !errors.Is(err, errConventionRoleRaced) {
		t.Fatalf("set column: err = %v, want errConventionRoleRaced", err)
	}
	roleID, seqAfter, _ := f.row(t, it.ID)
	if roleID != f.rev.ID || seqAfter != seq {
		t.Errorf("after refused writes: column %q seq %d, want %q %d", roleID, seqAfter, f.rev.ID, seq)
	}
	var n int
	if err := f.s.db.QueryRow(f.s.q(`SELECT COUNT(*) FROM activities WHERE document_id = ? AND actor = 'system'`), it.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("a refused write left %d system activity rows", n)
	}
}

func TestBackfillConventionRoles_InvalidSampleIsCapped(t *testing.T) {
	t.Parallel()
	f := newCRBFixture(t, "crb-cap")
	for i := 0; i < conventionRoleInvalidLogLimit+5; i++ {
		f.item(t, f.conventions, fmt.Sprintf("ghost %d", i), "ghost", nil)
	}
	res, err := f.s.BackfillConventionRoles()
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if res.Invalid != conventionRoleInvalidLogLimit+5 {
		t.Errorf("Invalid = %d, want %d", res.Invalid, conventionRoleInvalidLogLimit+5)
	}
	if len(res.InvalidSample) != conventionRoleInvalidLogLimit {
		t.Errorf("InvalidSample has %d rows, want the cap %d", len(res.InvalidSample), conventionRoleInvalidLogLimit)
	}
}
