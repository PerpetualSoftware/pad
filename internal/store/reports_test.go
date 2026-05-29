package store

import (
	"encoding/json"
	"strings"
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

func backdateItem(t *testing.T, s *Store, itemID string, hoursAgo float64) {
	t.Helper()
	ts := time.Now().UTC().Add(-time.Duration(hoursAgo * float64(time.Hour))).Format(time.RFC3339)
	if _, err := s.db.Exec(s.dialect.Rebind(`UPDATE items SET created_at = ? WHERE id = ?`), ts, itemID); err != nil {
		t.Fatalf("backdate %s: %v", itemID, err)
	}
}

func TestGetReport_CycleTime(t *testing.T) {
	s := testStore(t)
	wsID, colID := newTransitionTestWorkspace(t, s)
	item := createTestItem(t, s, wsID, colID, "slow task", "")
	backdateItem(t, s, item.ID, 48) // created 48h ago
	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{Fields: strPtr(`{"status":"done"}`)}); err != nil {
		t.Fatalf("complete: %v", err) // completion transition stamped ~now → cycle ≈ 48h
	}

	rep, err := s.GetReport(wsID, ReportOptions{Window: "week", Now: time.Now().UTC()})
	if err != nil {
		t.Fatalf("GetReport: %v", err)
	}
	if rep.CycleTime.SampleSize != 1 {
		t.Fatalf("expected 1 cycle-time sample, got %d", rep.CycleTime.SampleSize)
	}
	if rep.CycleTime.MedianHours < 47 || rep.CycleTime.MedianHours > 49 {
		t.Fatalf("expected median ~48h, got %.1f", rep.CycleTime.MedianHours)
	}
	if collDurMedian(rep.CycleTime.ByCollection, "tasks") < 47 {
		t.Fatalf("expected tasks cycle-time median ~48h, got %+v", rep.CycleTime.ByCollection)
	}
}

func TestGetReport_WIPAndAging(t *testing.T) {
	s := testStore(t)
	wsID, colID := newTransitionTestWorkspace(t, s)
	a := createTestItem(t, s, wsID, colID, "old open", "")
	backdateItem(t, s, a.ID, 100)                       // ~4.2d old, still open
	createTestItem(t, s, wsID, colID, "fresh open", "") // <1d old, open
	done := createTestItem(t, s, wsID, colID, "shipped", "")
	if _, err := s.UpdateItem(done.ID, models.ItemUpdate{Fields: strPtr(`{"status":"done"}`)}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	rep, err := s.GetReport(wsID, ReportOptions{Window: "week", Now: time.Now().UTC()})
	if err != nil {
		t.Fatalf("GetReport: %v", err)
	}
	if rep.WIP.OpenCount != 2 {
		t.Fatalf("expected 2 open items (done excluded), got %d", rep.WIP.OpenCount)
	}
	// Aging: one <1d, one 1-7d.
	band := map[string]int{}
	for _, b := range rep.WIP.AgingBuckets {
		band[b.Label] = b.Count
	}
	if band["<1d"] != 1 || band["1-7d"] != 1 {
		t.Fatalf("expected aging <1d=1, 1-7d=1, got %+v", rep.WIP.AgingBuckets)
	}
	if len(rep.WIP.AgingBuckets) != 4 {
		t.Fatalf("expected 4 fixed aging bands, got %d", len(rep.WIP.AgingBuckets))
	}
}

func collDurMedian(list []ReportDuration, slug string) float64 {
	for _, d := range list {
		if d.Collection == slug {
			return d.MedianHours
		}
	}
	return -1
}

func TestPercentile(t *testing.T) {
	if got := percentile(nil, 0.5); got != 0 {
		t.Errorf("empty percentile = %v, want 0", got)
	}
	if got := percentile([]float64{10}, 0.9); got != 10 {
		t.Errorf("single percentile = %v, want 10", got)
	}
	// median of 1..5 = 3; p90 ≈ 4.6
	if got := percentile([]float64{1, 2, 3, 4, 5}, 0.5); got != 3 {
		t.Errorf("median = %v, want 3", got)
	}
	if got := percentile([]float64{1, 2, 3, 4, 5}, 0.9); got < 4.5 || got > 4.7 {
		t.Errorf("p90 = %v, want ~4.6", got)
	}
}

func TestGetReport_EmptyScopeWellFormedJSON(t *testing.T) {
	s := testStore(t)
	wsID, _ := newTransitionTestWorkspace(t, s)
	// Empty visible set → no collections in scope (early-return path).
	rep, err := s.GetReport(wsID, ReportOptions{Window: "week", Now: time.Now().UTC(), ScopeToVisible: true, VisibleCollectionIDs: nil})
	if err != nil {
		t.Fatalf("GetReport: %v", err)
	}
	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	js := string(b)
	for _, frag := range []string{
		`"by_collection":null`, `"aging_buckets":null`, `"buckets":null`,
		`"completed_by_collection":null`, `"status_distribution":null`, `"collections":null`,
	} {
		if strings.Contains(js, frag) {
			t.Errorf("empty report has %s — should be []", frag)
		}
	}
}

func TestGetReport_OffsetShiftsWindow(t *testing.T) {
	s := testStore(t)
	wsID, colID := newTransitionTestWorkspace(t, s)
	createTestItem(t, s, wsID, colID, "recent", "") // created ~now
	old := createTestItem(t, s, wsID, colID, "old", "")
	backdateItem(t, s, old.ID, 10*24) // created ~10 days ago
	now := time.Now().UTC()

	// offset 0, week window [now-7d, now]: only the recent item.
	r0, err := s.GetReport(wsID, ReportOptions{Window: "week", Offset: 0, Now: now})
	if err != nil {
		t.Fatalf("offset 0: %v", err)
	}
	if r0.Offset != 0 || r0.Totals.Created != 1 {
		t.Fatalf("offset 0: expected offset=0 created=1 (recent), got offset=%d created=%d", r0.Offset, r0.Totals.Created)
	}

	// offset 1, week window [now-14d, now-7d]: only the 10-day-old item.
	r1, err := s.GetReport(wsID, ReportOptions{Window: "week", Offset: 1, Now: now})
	if err != nil {
		t.Fatalf("offset 1: %v", err)
	}
	if r1.Offset != 1 || r1.Totals.Created != 1 {
		t.Fatalf("offset 1: expected offset=1 created=1 (old), got offset=%d created=%d", r1.Offset, r1.Totals.Created)
	}
	if !(r1.RangeEnd < r0.RangeEnd && r1.RangeStart < r0.RangeStart) {
		t.Fatalf("offset 1 window should be earlier: r1 [%s,%s] vs r0 [%s,%s]", r1.RangeStart, r1.RangeEnd, r0.RangeStart, r0.RangeEnd)
	}

	// Negative offset clamps to 0 (no future).
	rNeg, err := s.GetReport(wsID, ReportOptions{Window: "week", Offset: -3, Now: now})
	if err != nil {
		t.Fatalf("neg offset: %v", err)
	}
	if rNeg.Offset != 0 || rNeg.RangeEnd != r0.RangeEnd {
		t.Fatalf("negative offset should clamp to 0, got offset=%d", rNeg.Offset)
	}
}
