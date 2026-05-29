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

func TestReportSnapshotAsOf(t *testing.T) {
	s := testStore(t)
	wsID, colID := newTransitionTestWorkspace(t, s) // tasks; "done" is a default terminal
	item := createTestItem(t, s, wsID, colID, "historical", "")
	// Control the transition history precisely.
	if _, err := s.db.Exec(s.dialect.Rebind(`DELETE FROM status_transitions WHERE item_id = ?`), item.ID); err != nil {
		t.Fatalf("clear transitions: %v", err)
	}
	backdateItem(t, s, item.ID, 20*24) // existed 20d ago

	now := time.Now().UTC()
	ins := func(id, from, to string, hoursAgo float64) {
		ts := now.Add(-time.Duration(hoursAgo * float64(time.Hour))).Format(time.RFC3339)
		if _, err := s.db.Exec(s.q(`INSERT INTO status_transitions (id, item_id, workspace_id, collection_id, field_key, from_status, to_status, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
			id, item.ID, wsID, colID, "status", from, to, ts); err != nil {
			t.Fatalf("insert transition: %v", err)
		}
	}
	ins("t1", "", "open", 20*24)    // 20d ago: open
	ins("t2", "open", "done", 5*24) // 5d ago: done

	colls, err := s.resolveReportCollections(wsID, ReportOptions{})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	statusOf := func(dist []ReportStatusCount, status string) int {
		for _, d := range dist {
			if d.Collection == "tasks" && d.Status == status {
				return d.Count
			}
		}
		return 0
	}

	// As of 10 days ago: latest transition <= then is t1 → "open" (the t2 done
	// at 5d ago is AFTER T, so it must NOT be reflected).
	dist10, wip10, err := s.reportSnapshotAsOf(wsID, colls, now.Add(-10*24*time.Hour))
	if err != nil {
		t.Fatalf("asof 10d: %v", err)
	}
	if statusOf(dist10, "open") != 1 || statusOf(dist10, "done") != 0 {
		t.Fatalf("as-of-10d should be open (pre-done), got %+v", dist10)
	}
	if wip10.OpenCount != 1 {
		t.Fatalf("as-of-10d WIP open should be 1, got %d", wip10.OpenCount)
	}

	// As of 3 days ago: latest <= then is t2 → "done" (terminal → not WIP).
	dist3, wip3, err := s.reportSnapshotAsOf(wsID, colls, now.Add(-3*24*time.Hour))
	if err != nil {
		t.Fatalf("asof 3d: %v", err)
	}
	if statusOf(dist3, "done") != 1 || statusOf(dist3, "open") != 0 {
		t.Fatalf("as-of-3d should be done, got %+v", dist3)
	}
	if wip3.OpenCount != 0 {
		t.Fatalf("as-of-3d WIP open should be 0 (done is terminal), got %d", wip3.OpenCount)
	}

	// Existed-at-T: as of 30 days ago (before the item's created_at), it must
	// not appear at all.
	dist30, wip30, err := s.reportSnapshotAsOf(wsID, colls, now.Add(-30*24*time.Hour))
	if err != nil {
		t.Fatalf("asof 30d: %v", err)
	}
	if len(dist30) != 0 || wip30.OpenCount != 0 {
		t.Fatalf("as-of-30d (before creation) should be empty, got dist=%+v wip.open=%d", dist30, wip30.OpenCount)
	}

	// An item that existed at T with NO done-field transition (created with no
	// status) must count as open — matching the live WIP path — not be dropped.
	noStatus, err := s.CreateItem(wsID, colID, models.ItemCreate{Title: "no-status"})
	if err != nil {
		t.Fatalf("create no-status item: %v", err)
	}
	if _, err := s.db.Exec(s.dialect.Rebind(`DELETE FROM status_transitions WHERE item_id = ?`), noStatus.ID); err != nil {
		t.Fatalf("clear: %v", err)
	}
	backdateItem(t, s, noStatus.ID, 8*24) // existed 8d ago, no transitions ever
	_, wipNow, err := s.reportSnapshotAsOf(wsID, colls, now.Add(-3*24*time.Hour))
	if err != nil {
		t.Fatalf("asof 3d (with no-status item): %v", err)
	}
	// As of 3d ago: the original item is "done" (t2 at 5d ago is <= 3d ago →
	// terminal, not open); the no-status item existed (8d ago) with no
	// transition → counts as open. So exactly 1 open.
	if wipNow.OpenCount != 1 {
		t.Fatalf("expected the no-transition no-status item to count as open (1), got %d", wipNow.OpenCount)
	}
}

func TestGetReport_CompletedItems(t *testing.T) {
	s := testStore(t)
	wsID, colID := newTransitionTestWorkspace(t, s)
	a := createTestItem(t, s, wsID, colID, "Ship A", "")
	b := createTestItem(t, s, wsID, colID, "Ship B", "")
	createTestItem(t, s, wsID, colID, "Still open", "") // not completed
	for _, it := range []string{a.ID, b.ID} {
		if _, err := s.UpdateItem(it, models.ItemUpdate{Fields: strPtr(`{"status":"done"}`)}); err != nil {
			t.Fatalf("complete %s: %v", it, err)
		}
	}
	// Reopen + re-complete A → it must appear ONCE (deduped by item).
	if _, err := s.UpdateItem(a.ID, models.ItemUpdate{Fields: strPtr(`{"status":"open"}`)}); err != nil {
		t.Fatalf("reopen A: %v", err)
	}
	if _, err := s.UpdateItem(a.ID, models.ItemUpdate{Fields: strPtr(`{"status":"done"}`)}); err != nil {
		t.Fatalf("re-complete A: %v", err)
	}

	now := time.Now().UTC()
	// Opt-in: completed_items present, deduped to A + B.
	rep, err := s.GetReport(wsID, ReportOptions{Window: "week", Now: now, IncludeItems: true})
	if err != nil {
		t.Fatalf("GetReport: %v", err)
	}
	if len(rep.CompletedItems) != 2 {
		t.Fatalf("expected 2 distinct completed items (A deduped), got %d: %+v", len(rep.CompletedItems), rep.CompletedItems)
	}
	titles := map[string]bool{}
	for _, ci := range rep.CompletedItems {
		titles[ci.Title] = true
		if ci.Collection != "tasks" {
			t.Fatalf("expected collection 'tasks', got %q", ci.Collection)
		}
		if !strings.HasPrefix(ci.Ref, "TASK-") {
			t.Fatalf("expected TASK- ref, got %q", ci.Ref)
		}
		if ci.CompletedAt == "" {
			t.Fatalf("expected completed_at on %q", ci.Ref)
		}
	}
	if !titles["Ship A"] || !titles["Ship B"] {
		t.Fatalf("missing expected titles: %+v", titles)
	}

	// Not requested: completed_items omitted (nil).
	rep2, err := s.GetReport(wsID, ReportOptions{Window: "week", Now: now})
	if err != nil {
		t.Fatalf("GetReport (no items): %v", err)
	}
	if rep2.CompletedItems != nil {
		t.Fatalf("completed_items should be nil when not requested, got %+v", rep2.CompletedItems)
	}
}

func TestGetReport_CompletedItemsRespectsCurrentCollectionVisibility(t *testing.T) {
	s := testStore(t)
	u, _ := s.CreateUser(models.UserCreate{Name: "V", Email: "v@example.com"})
	ws, _ := s.CreateWorkspace(models.WorkspaceCreate{Name: "Vis", Slug: "vis", OwnerID: u.ID})
	visible := createTestCollection(t, s, ws.ID, "Visible")
	secret := createTestCollection(t, s, ws.ID, "Secret")

	item := createTestItem(t, s, ws.ID, visible.ID, "Mover", "")
	// Complete it while in the visible collection (transition stamped visible).
	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{Fields: strPtr(`{"status":"done"}`)}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	// Move it (status-preserving) into the hidden collection.
	if _, err := s.MoveItem(item.ID, secret.ID, `{"status":"done"}`); err != nil {
		t.Fatalf("move: %v", err)
	}

	now := time.Now().UTC()
	// Scoped to the visible collection only: the item now lives in the hidden
	// collection, so it must NOT appear in completed_items (no current title /
	// hidden-collection-slug leak), even though it completed while visible.
	scoped, err := s.GetReport(ws.ID, ReportOptions{
		Window: "week", Now: now, IncludeItems: true,
		ScopeToVisible: true, VisibleCollectionIDs: []string{visible.ID},
	})
	if err != nil {
		t.Fatalf("scoped: %v", err)
	}
	if len(scoped.CompletedItems) != 0 {
		t.Fatalf("item moved to a hidden collection must not leak into completed_items, got %+v", scoped.CompletedItems)
	}

	// Unscoped (owner/full): the item appears, attributed to its current collection.
	full, err := s.GetReport(ws.ID, ReportOptions{Window: "week", Now: now, IncludeItems: true})
	if err != nil {
		t.Fatalf("full: %v", err)
	}
	if len(full.CompletedItems) != 1 || full.CompletedItems[0].Collection != "secret" {
		t.Fatalf("unscoped should list the item under its current collection, got %+v", full.CompletedItems)
	}
}

func TestReportSnapshotAsOf_SameSecondSeqTiebreak(t *testing.T) {
	s := testStore(t)
	wsID, colID := newTransitionTestWorkspace(t, s)
	item := createTestItem(t, s, wsID, colID, "flipper", "")
	if _, err := s.db.Exec(s.dialect.Rebind(`DELETE FROM status_transitions WHERE item_id = ?`), item.ID); err != nil {
		t.Fatalf("clear: %v", err)
	}
	backdateItem(t, s, item.ID, 10*24)

	// Three hops with the SAME created_at (one second) — only seq disambiguates
	// which is "latest". Without the seq tiebreak the random-id order could pick
	// the middle (done) hop and undercount open work.
	same := time.Now().UTC().Add(-5 * 24 * time.Hour).Format(time.RFC3339)
	ins := func(id, from, to string, seq int) {
		if _, err := s.db.Exec(s.q(`INSERT INTO status_transitions (id, item_id, workspace_id, collection_id, field_key, from_status, to_status, created_at, seq) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			id, item.ID, wsID, colID, "status", from, to, same, seq); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	// IDs and seq deliberately DISAGREE on the answer:
	//   - highest seq (3, the true latest) is "open"  → correct result: 1 open
	//   - highest id ("z…") is "done"                 → an id-only tiebreak: 0 open
	// So asserting 1 open proves seq (not id) is the tiebreak.
	ins("m-first-open", "", "open", 1)
	ins("z-mid-done", "open", "done", 2)   // highest id, but NOT the latest
	ins("a-final-open", "done", "open", 3) // lowest id, highest seq → the latest

	colls, err := s.resolveReportCollections(wsID, ReportOptions{})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	_, wip, err := s.reportSnapshotAsOf(wsID, colls, time.Now().UTC().Add(-3*24*time.Hour))
	if err != nil {
		t.Fatalf("asof: %v", err)
	}
	if wip.OpenCount != 1 {
		t.Fatalf("expected seq tiebreak to resolve to the highest-seq (open) hop → 1 open, got %d (id-only tiebreak would give 0)", wip.OpenCount)
	}
}

func TestBackfillStatusTransitions_SeedSeqBelowHop(t *testing.T) {
	s := testStore(t)
	wsID, colID := newTransitionTestWorkspace(t, s)
	item := createTestItem(t, s, wsID, colID, "create-and-change", "")
	// Clear the live create-seed so only the backfill populates the table.
	if _, err := s.db.Exec(s.q(`DELETE FROM status_transitions`)); err != nil {
		t.Fatalf("clear: %v", err)
	}
	// A historical status change → the backfill produces a hop AND a create-seed.
	if _, err := s.CreateActivity(models.Activity{
		WorkspaceID: wsID, DocumentID: item.ID, Action: "updated", Actor: "user", Source: "web",
		Metadata: `{"changes":"status: open → done"}`,
	}); err != nil {
		t.Fatalf("activity: %v", err)
	}
	if _, err := s.BackfillStatusTransitions(); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	// The create-seed must get a LOWER seq than the hop, so a same-second
	// create-and-change resolves to the hop (the later state), not the seed.
	rows, err := s.db.Query(s.q(`SELECT id, seq FROM status_transitions WHERE item_id = ?`), item.ID)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	seedSeq, hopSeq := -1, -1
	for rows.Next() {
		var id string
		var seq int
		if err := rows.Scan(&id, &seq); err != nil {
			t.Fatalf("scan: %v", err)
		}
		switch {
		case strings.HasPrefix(id, "create_"):
			seedSeq = seq
		case strings.HasPrefix(id, "bf_"):
			hopSeq = seq
		}
	}
	if seedSeq < 0 || hopSeq < 0 {
		t.Fatalf("expected both a seed and a hop row, got seedSeq=%d hopSeq=%d", seedSeq, hopSeq)
	}
	if !(seedSeq < hopSeq) {
		t.Fatalf("create-seed seq (%d) must be below the hop seq (%d)", seedSeq, hopSeq)
	}
}
