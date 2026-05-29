package store

import (
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func findBucket(buckets []ReportBucket, label string) (ReportBucket, bool) {
	for _, b := range buckets {
		if b.Bucket == label {
			return b, true
		}
	}
	return ReportBucket{}, false
}

func collCount(list []ReportCollectionCount, slug string) int {
	for _, c := range list {
		if c.Collection == slug {
			return c.Count
		}
	}
	return 0
}

func TestGetReport_ThroughputAndTotals(t *testing.T) {
	s := testStore(t)
	wsID, colID := newTransitionTestWorkspace(t, s)

	// Two items created; one moved to done.
	a := createTestItem(t, s, wsID, colID, "A", "")
	createTestItem(t, s, wsID, colID, "B", "")
	if _, err := s.UpdateItem(a.ID, models.ItemUpdate{Fields: strPtr(`{"status":"done"}`)}); err != nil {
		t.Fatalf("complete A: %v", err)
	}

	now := time.Now().UTC()
	rep, err := s.GetReport(wsID, ReportOptions{Window: "week", Now: now})
	if err != nil {
		t.Fatalf("GetReport: %v", err)
	}

	if rep.Granularity != "day" {
		t.Fatalf("week window should bucket by day, got %q", rep.Granularity)
	}
	if rep.Totals.Created != 2 {
		t.Fatalf("expected 2 created, got %d", rep.Totals.Created)
	}
	if rep.Totals.Completed != 1 {
		t.Fatalf("expected 1 completed (A → done), got %d", rep.Totals.Completed)
	}
	if rep.Totals.NetFlow != 1 {
		t.Fatalf("expected net flow 1, got %d", rep.Totals.NetFlow)
	}

	// Today's bucket should hold the activity.
	today := now.Format("2006-01-02")
	b, ok := findBucket(rep.Buckets, today)
	if !ok {
		t.Fatalf("today's bucket %q missing from series", today)
	}
	if b.Created != 2 || b.Completed != 1 {
		t.Fatalf("today bucket = created %d completed %d; want 2/1", b.Created, b.Completed)
	}

	if collCount(rep.CompletedByCollection, "tasks") != 1 {
		t.Fatalf("expected tasks completed=1, got %+v", rep.CompletedByCollection)
	}
}

func TestGetReport_NegativeTerminalNotCompleted(t *testing.T) {
	s := testStore(t)
	wsID, colID := newTransitionTestWorkspace(t, s)
	item := createTestItem(t, s, wsID, colID, "Doomed idea", "")

	// Move into a negative terminal — recorded as a transition, but must NOT
	// count as a completion.
	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{Fields: strPtr(`{"status":"rejected"}`)}); err != nil {
		t.Fatalf("reject: %v", err)
	}

	rep, err := s.GetReport(wsID, ReportOptions{Window: "week", Now: time.Now().UTC()})
	if err != nil {
		t.Fatalf("GetReport: %v", err)
	}
	if rep.Totals.Completed != 0 {
		t.Fatalf("rejected should not count as completed, got %d", rep.Totals.Completed)
	}
	if rep.Totals.Created != 1 {
		t.Fatalf("expected 1 created, got %d", rep.Totals.Created)
	}
}

func TestGetReport_StatusDistribution(t *testing.T) {
	s := testStore(t)
	wsID, colID := newTransitionTestWorkspace(t, s)
	createTestItem(t, s, wsID, colID, "open1", "")
	createTestItem(t, s, wsID, colID, "open2", "")
	done := createTestItem(t, s, wsID, colID, "done1", "")
	if _, err := s.UpdateItem(done.ID, models.ItemUpdate{Fields: strPtr(`{"status":"done"}`)}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	rep, err := s.GetReport(wsID, ReportOptions{Window: "week", Now: time.Now().UTC()})
	if err != nil {
		t.Fatalf("GetReport: %v", err)
	}
	got := map[string]int{}
	for _, d := range rep.StatusDistribution {
		if d.Collection == "tasks" {
			got[d.Status] = d.Count
		}
	}
	if got["open"] != 2 || got["done"] != 1 {
		t.Fatalf("status distribution wrong: %+v", rep.StatusDistribution)
	}
}

func TestGetReport_CollectionFilter(t *testing.T) {
	s := testStore(t)
	wsID, tasksID := newTransitionTestWorkspace(t, s)
	bugs := createTestCollection(t, s, wsID, "Bugs")
	createTestItem(t, s, wsID, tasksID, "task", "")
	createTestItem(t, s, wsID, bugs.ID, "bug", "")

	// Filter to bugs only.
	rep, err := s.GetReport(wsID, ReportOptions{Window: "week", Collections: []string{"bugs"}, Now: time.Now().UTC()})
	if err != nil {
		t.Fatalf("GetReport: %v", err)
	}
	if len(rep.Collections) != 1 || rep.Collections[0] != "bugs" {
		t.Fatalf("expected only bugs in scope, got %+v", rep.Collections)
	}
	if rep.Totals.Created != 1 {
		t.Fatalf("expected 1 created (bug only), got %d", rep.Totals.Created)
	}
}

func TestGetReport_SoftDeletedExcludedFromCompleted(t *testing.T) {
	s := testStore(t)
	wsID, colID := newTransitionTestWorkspace(t, s)
	item := createTestItem(t, s, wsID, colID, "ship then delete", "")
	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{Fields: strPtr(`{"status":"done"}`)}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	// Soft delete — the status_transitions row survives (only a HARD delete
	// cascades it). It must drop out of completed counts to stay consistent
	// with created/status_distribution.
	if err := s.DeleteItem(item.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	rep, err := s.GetReport(wsID, ReportOptions{Window: "week", Now: time.Now().UTC()})
	if err != nil {
		t.Fatalf("GetReport: %v", err)
	}
	if rep.Totals.Completed != 0 {
		t.Fatalf("soft-deleted item must not count as completed, got %d", rep.Totals.Completed)
	}
	if collCount(rep.CompletedByCollection, "tasks") != 0 {
		t.Fatalf("soft-deleted item must not appear in completed_by_collection, got %+v", rep.CompletedByCollection)
	}
	if rep.Totals.Created != 0 {
		t.Fatalf("soft-deleted item should also be absent from created, got %d", rep.Totals.Created)
	}
}

func TestGetReport_ScopeToVisibleExcludesHidden(t *testing.T) {
	s := testStore(t)
	wsID, tasksID := newTransitionTestWorkspace(t, s)
	secret := createTestCollection(t, s, wsID, "Secret")
	createTestItem(t, s, wsID, tasksID, "visible task", "")
	createTestItem(t, s, wsID, secret.ID, "hidden", "")

	// Caller can only see the tasks collection.
	rep, err := s.GetReport(wsID, ReportOptions{
		Window:               "week",
		Now:                  time.Now().UTC(),
		ScopeToVisible:       true,
		VisibleCollectionIDs: []string{tasksID},
	})
	if err != nil {
		t.Fatalf("GetReport: %v", err)
	}
	if len(rep.Collections) != 1 || rep.Collections[0] != "tasks" {
		t.Fatalf("expected only the visible 'tasks' collection, got %+v", rep.Collections)
	}
	if rep.Totals.Created != 1 {
		t.Fatalf("hidden collection's item must not be counted; created=%d", rep.Totals.Created)
	}

	// Empty visible set → empty report (guest with no full-collection access).
	empty, err := s.GetReport(wsID, ReportOptions{Window: "week", Now: time.Now().UTC(), ScopeToVisible: true, VisibleCollectionIDs: nil})
	if err != nil {
		t.Fatalf("GetReport empty: %v", err)
	}
	if len(empty.Collections) != 0 || empty.Totals.Created != 0 {
		t.Fatalf("empty visible set should yield empty report, got %+v", empty)
	}
}

func TestGetReport_NonStatusDoneField(t *testing.T) {
	s := testStore(t)
	u, _ := s.CreateUser(models.UserCreate{Name: "H", Email: "h@example.com"})
	ws, _ := s.CreateWorkspace(models.WorkspaceCreate{Name: "Hiring", Slug: "hiring", OwnerID: u.ID})
	col, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name:     "Candidates",
		Schema:   `{"fields":[{"key":"stage","label":"Stage","type":"select","options":["applied","interview","hired"],"terminal_options":["hired"],"default":"applied"}]}`,
		Settings: `{"board_group_by":"stage"}`,
	})
	if err != nil {
		t.Fatalf("create collection: %v", err)
	}
	c, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: "Ada", Fields: `{"stage":"applied"}`})
	if err != nil {
		t.Fatalf("create item: %v", err)
	}
	if _, err := s.UpdateItem(c.ID, models.ItemUpdate{Fields: strPtr(`{"stage":"hired"}`)}); err != nil {
		t.Fatalf("hire: %v", err)
	}

	rep, err := s.GetReport(ws.ID, ReportOptions{Window: "week", Now: time.Now().UTC()})
	if err != nil {
		t.Fatalf("GetReport: %v", err)
	}
	if rep.Totals.Completed != 1 {
		t.Fatalf("expected 1 completion on the stage done-field, got %d", rep.Totals.Completed)
	}
	if collCount(rep.CompletedByCollection, "candidates") != 1 {
		t.Fatalf("expected candidates completed=1, got %+v", rep.CompletedByCollection)
	}
}

func TestGetReport_ExcludesOutOfWindow(t *testing.T) {
	s := testStore(t)
	wsID, colID := newTransitionTestWorkspace(t, s)
	old := createTestItem(t, s, wsID, colID, "ancient", "")
	// Backdate the item well outside a 1-week window.
	if _, err := s.db.Exec(s.dialect.Rebind(`UPDATE items SET created_at = ? WHERE id = ?`),
		"2020-01-01T00:00:00Z", old.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	createTestItem(t, s, wsID, colID, "recent", "")

	rep, err := s.GetReport(wsID, ReportOptions{Window: "week", Now: time.Now().UTC()})
	if err != nil {
		t.Fatalf("GetReport: %v", err)
	}
	if rep.Totals.Created != 1 {
		t.Fatalf("expected only the recent item (1) created in window, got %d", rep.Totals.Created)
	}
}

func TestGetReport_DayWindowBucketsByHour(t *testing.T) {
	s := testStore(t)
	wsID, _ := newTransitionTestWorkspace(t, s)
	rep, err := s.GetReport(wsID, ReportOptions{Window: "day", Now: time.Now().UTC()})
	if err != nil {
		t.Fatalf("GetReport: %v", err)
	}
	if rep.Granularity != "hour" {
		t.Fatalf("day window should bucket by hour, got %q", rep.Granularity)
	}
	// Hourly labels are "YYYY-MM-DDTHH" (13 chars).
	if len(rep.Buckets) > 0 && len(rep.Buckets[0].Bucket) != 13 {
		t.Fatalf("hourly bucket label should be 13 chars, got %q", rep.Buckets[0].Bucket)
	}
}
