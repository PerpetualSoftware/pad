package store

import (
	"errors"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestRoleLaneOrder_StableTieBreak(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws := createTestWorkspace(t, s, "LaneOrderWS")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	role, err := s.CreateAgentRole(ws.ID, models.AgentRoleCreate{Name: "Implementer"})
	if err != nil {
		t.Fatalf("CreateAgentRole: %v", err)
	}

	var created []*models.Item
	for _, title := range []string{"First", "Second", "Third", "Fourth", "Fifth"} {
		it, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
			Title:       title,
			Fields:      `{"status":"open"}`,
			AgentRoleID: &role.ID,
		})
		if err != nil {
			t.Fatalf("CreateItem %s: %v", title, err)
		}
		created = append(created, it)
	}

	// Pin all items to identical role_sort_order and identical created_at second
	// so item_number alone must break the tie deterministically.
	sameTS := "2026-06-01T12:00:00Z"
	for _, it := range created {
		if _, err := s.db.Exec(s.q(`UPDATE items SET role_sort_order = 0, created_at = ? WHERE id = ?`), sameTS, it.ID); err != nil {
			t.Fatalf("pin timestamps: %v", err)
		}
	}

	// Verify GetRoleBoardItems returns ascending item_number order every run.
	for run := 0; run < 5; run++ {
		lanes, err := s.GetRoleBoardItems(ws.ID, RoleBoardParams{})
		if err != nil {
			t.Fatalf("run %d GetRoleBoardItems: %v", run, err)
		}
		var roleLane *RoleBoardLane
		for i := range lanes {
			if lanes[i].Role != nil && lanes[i].Role.ID == role.ID {
				roleLane = &lanes[i]
				break
			}
		}
		if roleLane == nil {
			t.Fatalf("run %d: role lane not found", run)
		}
		if len(roleLane.Items) != len(created) {
			t.Fatalf("run %d: got %d items, want %d", run, len(roleLane.Items), len(created))
		}
		for i, want := range created {
			if roleLane.Items[i].ID != want.ID {
				t.Fatalf("run %d pos %d: got %s (num=%v), want %s (num=%v)",
					run, i, roleLane.Items[i].Title, roleLane.Items[i].ItemNumber, want.Title, want.ItemNumber)
			}
		}
	}

	// Also exercise SortRoleLane directly on a reversed copy.
	rev := make([]models.Item, len(created))
	for i := range created {
		got, err := s.GetItem(created[len(created)-1-i].ID)
		if err != nil {
			t.Fatalf("GetItem: %v", err)
		}
		rev[i] = *got
	}
	SortRoleLane(rev)
	for i, want := range created {
		if rev[i].ID != want.ID {
			t.Fatalf("SortRoleLane pos %d: got %s, want %s", i, rev[i].Title, want.Title)
		}
	}
}

func seedRoleParkFixture(t *testing.T) (*Store, *models.Workspace, *models.Collection, *models.AgentRole, *models.User, []*models.Item) {
	t.Helper()
	s := testStore(t)
	user, err := s.CreateUser(models.UserCreate{
		Email: "parker@example.com", Name: "Parker", Username: "parker", Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	ws := createTestWorkspace(t, s, "ParkWS")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	role, err := s.CreateAgentRole(ws.ID, models.AgentRoleCreate{Name: "Implementer"})
	if err != nil {
		t.Fatalf("CreateAgentRole: %v", err)
	}
	var items []*models.Item
	for i, title := range []string{"Alpha", "Beta", "Gamma"} {
		it, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
			Title:       title,
			Fields:      `{"status":"open"}`,
			AgentRoleID: &role.ID,
		})
		if err != nil {
			t.Fatalf("CreateItem %s: %v", title, err)
		}
		if err := s.UpdateRoleSortOrder(ws.ID, []RoleSortUpdate{{ItemID: it.ID, RoleSortOrder: (i + 1) * 1024}}); err != nil {
			t.Fatalf("UpdateRoleSortOrder: %v", err)
		}
		it, err = s.GetItem(it.ID)
		if err != nil {
			t.Fatalf("GetItem: %v", err)
		}
		items = append(items, it)
	}
	return s, ws, col, role, user, items
}

func TestRolePark_TopParksQueue(t *testing.T) {
	t.Parallel()
	s, ws, _, role, user, items := seedRoleParkFixture(t)

	// Before parking, role.Park is nil.
	gotRole, err := s.GetAgentRole(ws.ID, role.ID)
	if err != nil {
		t.Fatalf("GetAgentRole: %v", err)
	}
	if gotRole.Park != nil {
		t.Fatalf("expected nil Park initially, got %+v", gotRole.Park)
	}

	park, err := s.SetRolePark(ws.ID, role.ID, ParkAtTop(), nil, user.ID)
	if err != nil {
		t.Fatalf("SetRolePark AtTop: %v", err)
	}
	if park.Mode != models.ParkModeTop {
		t.Fatalf("mode = %q, want %q", park.Mode, models.ParkModeTop)
	}
	if park.SortOrder != nil {
		t.Fatalf("sort_order = %v, want nil", *park.SortOrder)
	}
	if park.ParkSeq != 1 {
		t.Fatalf("park_seq = %d, want 1", park.ParkSeq)
	}
	if park.SetAt == nil || park.SetAt.IsZero() {
		t.Fatalf("expected non-zero set_at")
	}
	if park.SetByUserID == nil || *park.SetByUserID != user.ID {
		t.Fatalf("set_by_user_id = %v, want %q", park.SetByUserID, user.ID)
	}

	for _, it := range items {
		if !ItemParked(park, it.RoleSortOrder) {
			t.Errorf("item %s (sort=%d) should be parked under top mode", it.Title, it.RoleSortOrder)
		}
	}

	// Re-reading through GetAgentRole returns the same park state.
	gotRole, err = s.GetAgentRole(ws.ID, role.ID)
	if err != nil {
		t.Fatalf("GetAgentRole after set: %v", err)
	}
	if gotRole.Park == nil || gotRole.Park.Mode != models.ParkModeTop || gotRole.Park.ParkSeq != 1 {
		t.Fatalf("GetAgentRole park = %+v, want top seq 1", gotRole.Park)
	}
}

func TestRolePark_BeforeAndAfter(t *testing.T) {
	t.Parallel()
	s, ws, _, role, user, items := seedRoleParkFixture(t)

	// Before Beta (items[1], sort=2048) -> P = 2048.
	park, err := s.SetRolePark(ws.ID, role.ID, ParkBefore(items[1].ID), nil, user.ID)
	if err != nil {
		t.Fatalf("SetRolePark Before(Beta): %v", err)
	}
	if park.Mode != models.ParkModeLine || park.SortOrder == nil || *park.SortOrder != 2048 {
		t.Fatalf("Before(Beta): got %+v, want line @ 2048", park)
	}
	if ItemParked(park, items[0].RoleSortOrder) {
		t.Errorf("Alpha should be above park line")
	}
	if !ItemParked(park, items[1].RoleSortOrder) || !ItemParked(park, items[2].RoleSortOrder) {
		t.Errorf("Beta and Gamma should be parked")
	}
	firstRef, err := s.FirstParkedQueueRef(ws.ID, role.ID, park, nil, nil)
	if err != nil {
		t.Fatalf("FirstParkedQueueRef: %v", err)
	}
	if firstRef == nil || *firstRef != items[1].Ref {
		t.Fatalf("first_parked_ref = %v, want %q", firstRef, items[1].Ref)
	}

	// After Beta -> equivalent to Before(Gamma), P = 3072.
	park, err = s.SetRolePark(ws.ID, role.ID, ParkAfter(items[1].ID), nil, user.ID)
	if err != nil {
		t.Fatalf("SetRolePark After(Beta): %v", err)
	}
	if park.Mode != models.ParkModeLine || park.SortOrder == nil || *park.SortOrder != 3072 {
		t.Fatalf("After(Beta): got %+v, want line @ 3072", park)
	}
	if ItemParked(park, items[0].RoleSortOrder) || ItemParked(park, items[1].RoleSortOrder) {
		t.Errorf("Alpha and Beta should be above park line")
	}
	if !ItemParked(park, items[2].RoleSortOrder) {
		t.Errorf("Gamma should be parked")
	}

	// After Gamma (last item, sort=3072) -> P = 3072 + 1024 = 4096, no parked queue items.
	park, err = s.SetRolePark(ws.ID, role.ID, ParkAfter(items[2].ID), nil, user.ID)
	if err != nil {
		t.Fatalf("SetRolePark After(Gamma): %v", err)
	}
	if park.Mode != models.ParkModeLine || park.SortOrder == nil || *park.SortOrder != 4096 {
		t.Fatalf("After(Gamma): got %+v, want line @ 4096", park)
	}
	for _, it := range items {
		if ItemParked(park, it.RoleSortOrder) {
			t.Errorf("item %s should not be parked after last item", it.Title)
		}
	}
	firstRef, err = s.FirstParkedQueueRef(ws.ID, role.ID, park, nil, nil)
	if err != nil {
		t.Fatalf("FirstParkedQueueRef after last: %v", err)
	}
	if firstRef != nil {
		t.Fatalf("first_parked_ref = %q, want nil", *firstRef)
	}
}

func TestRolePark_BeforeRespacesTiedLane(t *testing.T) {
	t.Parallel()
	s, ws, _, role, user, items := seedRoleParkFixture(t)

	// Force all three items to tie at role_sort_order = 0 with identical timestamp.
	sameTS := "2026-06-01T12:00:00Z"
	for _, it := range items {
		if _, err := s.db.Exec(s.q(`UPDATE items SET role_sort_order = 0, created_at = ? WHERE id = ?`), sameTS, it.ID); err != nil {
			t.Fatalf("tie items: %v", err)
		}
	}
	seqBefore := make([]int64, len(items))
	for i, it := range items {
		cur, err := s.GetItem(it.ID)
		if err != nil {
			t.Fatalf("GetItem before: %v", err)
		}
		seqBefore[i] = cur.Seq
	}

	// Park Before Beta (index 1, tied with Alpha at index 0) -> must respace first!
	park, err := s.SetRolePark(ws.ID, role.ID, ParkBefore(items[1].ID), nil, user.ID)
	if err != nil {
		t.Fatalf("SetRolePark Before tied: %v", err)
	}
	if park.Mode != models.ParkModeLine || park.SortOrder == nil || *park.SortOrder != 2048 {
		t.Fatalf("park = %+v, want line @ 2048", park)
	}

	wantOrders := []int{1024, 2048, 3072}
	var prevSeq int64
	for i, it := range items {
		cur, err := s.GetItem(it.ID)
		if err != nil {
			t.Fatalf("GetItem after %d: %v", i, err)
		}
		if cur.RoleSortOrder != wantOrders[i] {
			t.Errorf("item %d role_sort_order = %d, want %d", i, cur.RoleSortOrder, wantOrders[i])
		}
		if cur.Seq <= seqBefore[i] {
			t.Errorf("item %d seq did not bump: before=%d after=%d", i, seqBefore[i], cur.Seq)
		}
		if i > 0 && cur.Seq <= prevSeq {
			t.Errorf("item %d seq %d not strictly greater than previous bumped row seq %d", i, cur.Seq, prevSeq)
		}
		prevSeq = cur.Seq
	}
}

func TestRolePark_AnchorCompletedLineHolds(t *testing.T) {
	t.Parallel()
	s, ws, _, role, user, items := seedRoleParkFixture(t)

	// Park Before Beta (2048).
	park, err := s.SetRolePark(ws.ID, role.ID, ParkBefore(items[1].ID), nil, user.ID)
	if err != nil {
		t.Fatalf("SetRolePark: %v", err)
	}
	if park.SortOrder == nil || *park.SortOrder != 2048 {
		t.Fatalf("park = %+v, want 2048", park)
	}

	// Complete Beta (terminal status).
	doneFields := `{"status":"done"}`
	if _, err := s.UpdateItem(items[1].ID, models.ItemUpdate{Fields: &doneFields}); err != nil {
		t.Fatalf("complete Beta: %v", err)
	}

	// Line P must stay 2048; Gamma (3072) becomes the first parked item.
	gotRole, err := s.GetAgentRole(ws.ID, role.ID)
	if err != nil {
		t.Fatalf("GetAgentRole: %v", err)
	}
	if gotRole.Park == nil || gotRole.Park.SortOrder == nil || *gotRole.Park.SortOrder != 2048 {
		t.Fatalf("park after anchor completed = %+v, want line @ 2048", gotRole.Park)
	}
	firstRef, err := s.FirstParkedQueueRef(ws.ID, role.ID, gotRole.Park, nil, nil)
	if err != nil {
		t.Fatalf("FirstParkedQueueRef: %v", err)
	}
	if firstRef == nil || *firstRef != items[2].Ref {
		t.Fatalf("first_parked_ref = %v, want %q (Gamma)", firstRef, items[2].Ref)
	}

	// Deleting the role removes its park (the row is gone).
	if err := s.DeleteAgentRole(ws.ID, role.ID); err != nil {
		t.Fatalf("DeleteAgentRole: %v", err)
	}
	deletedRole, err := s.GetAgentRole(ws.ID, role.ID)
	if err != nil {
		t.Fatalf("GetAgentRole deleted: %v", err)
	}
	if deletedRole != nil {
		t.Fatalf("expected nil after DeleteAgentRole, got %+v", deletedRole)
	}
}

func TestRolePark_ExpectedSeqConflict(t *testing.T) {
	t.Parallel()
	s, ws, _, role, user, items := seedRoleParkFixture(t)

	// Initial park_seq is 0; passing expected=1 must conflict with actual=0.
	wrong := 1
	_, err := s.SetRolePark(ws.ID, role.ID, ParkAtTop(), &wrong, user.ID)
	if !errors.Is(err, ErrParkConflict) {
		t.Fatalf("expected ErrParkConflict, got %v", err)
	}
	var ce *ParkConflictError
	if !errors.As(err, &ce) || ce.ActualParkSeq != 0 {
		t.Fatalf("expected ParkConflictError{ActualParkSeq:0}, got %+v (%v)", ce, err)
	}

	// Passing expected=0 succeeds and bumps to 1.
	zero := 0
	park, err := s.SetRolePark(ws.ID, role.ID, ParkBefore(items[1].ID), &zero, user.ID)
	if err != nil {
		t.Fatalf("SetRolePark expected=0: %v", err)
	}
	if park.ParkSeq != 1 {
		t.Fatalf("park_seq = %d, want 1", park.ParkSeq)
	}

	// ClearRolePark with stale expected=0 conflicts with actual=1 and leaves park intact.
	_, err = s.ClearRolePark(ws.ID, role.ID, &zero)
	if !errors.Is(err, ErrParkConflict) {
		t.Fatalf("ClearRolePark stale expected: got %v, want ErrParkConflict", err)
	}
	if !errors.As(err, &ce) || ce.ActualParkSeq != 1 {
		t.Fatalf("ClearRolePark conflict actual = %+v, want 1", ce)
	}
	gotRole, err := s.GetAgentRole(ws.ID, role.ID)
	if err != nil || gotRole.Park == nil || gotRole.Park.Mode != models.ParkModeLine {
		t.Fatalf("stale clear mutated park: %+v, err=%v", gotRole, err)
	}
}

func TestRolePark_SortLimitRespace(t *testing.T) {
	t.Parallel()
	s, ws, _, role, user, items := seedRoleParkFixture(t)

	// Put items near 2^30 so After(Gamma) would exceed 2^30.
	nearLimit := (1 << 30) - 512
	if err := s.UpdateRoleSortOrder(ws.ID, []RoleSortUpdate{
		{ItemID: items[0].ID, RoleSortOrder: nearLimit - 2048},
		{ItemID: items[1].ID, RoleSortOrder: nearLimit - 1024},
		{ItemID: items[2].ID, RoleSortOrder: nearLimit},
	}); err != nil {
		t.Fatalf("UpdateRoleSortOrder near limit: %v", err)
	}

	park, err := s.SetRolePark(ws.ID, role.ID, ParkAfter(items[2].ID), nil, user.ID)
	if err != nil {
		t.Fatalf("SetRolePark After(last near limit): %v", err)
	}
	if park.Mode != models.ParkModeLine || park.SortOrder == nil {
		t.Fatalf("unexpected park: %+v", park)
	}
	if *park.SortOrder != 4096 {
		t.Fatalf("after respace, P = %d, want 4096 (3*1024 + 1024)", *park.SortOrder)
	}
	wantOrders := []int{1024, 2048, 3072}
	for i, it := range items {
		cur, err := s.GetItem(it.ID)
		if err != nil {
			t.Fatalf("GetItem %d: %v", i, err)
		}
		if cur.RoleSortOrder != wantOrders[i] {
			t.Errorf("item %d role_sort_order = %d, want %d", i, cur.RoleSortOrder, wantOrders[i])
		}
	}
}

func TestRolePark_ClearIdempotent(t *testing.T) {
	t.Parallel()
	s, ws, _, role, user, _ := seedRoleParkFixture(t)

	// Clearing when already none does not bump park_seq (stays 0).
	seq, err := s.ClearRolePark(ws.ID, role.ID, nil)
	if err != nil {
		t.Fatalf("ClearRolePark initial: %v", err)
	}
	if seq != 0 {
		t.Fatalf("seq = %d, want 0", seq)
	}

	// Park -> seq becomes 1.
	park, err := s.SetRolePark(ws.ID, role.ID, ParkAtTop(), nil, user.ID)
	if err != nil {
		t.Fatalf("SetRolePark: %v", err)
	}
	if park.ParkSeq != 1 {
		t.Fatalf("park_seq = %d, want 1", park.ParkSeq)
	}

	// Clear -> seq becomes 2 and role.Park is nil.
	seq, err = s.ClearRolePark(ws.ID, role.ID, nil)
	if err != nil {
		t.Fatalf("ClearRolePark first: %v", err)
	}
	if seq != 2 {
		t.Fatalf("seq after clear = %d, want 2", seq)
	}
	gotRole, err := s.GetAgentRole(ws.ID, role.ID)
	if err != nil || gotRole.Park != nil {
		t.Fatalf("expected nil park after clear, got %+v err=%v", gotRole, err)
	}

	// Second clear is idempotent and does NOT bump seq (stays 2).
	seq2, err := s.ClearRolePark(ws.ID, role.ID, nil)
	if err != nil {
		t.Fatalf("ClearRolePark second: %v", err)
	}
	if seq2 != 2 {
		t.Fatalf("second clear bumped seq: got %d, want 2", seq2)
	}
}

func TestRolePark_NowItemNeverParked(t *testing.T) {
	t.Parallel()
	s, ws, _, role, user, items := seedRoleParkFixture(t)

	// Mark Alpha as in_progress (active status -> Now item).
	inProg := `{"status":"in_progress"}`
	if _, err := s.UpdateItem(items[0].ID, models.ItemUpdate{Fields: &inProg}); err != nil {
		t.Fatalf("set Alpha in_progress: %v", err)
	}
	// Give Beta a live execution lease (live lease -> Now item).
	if _, err := s.ClaimItemLease(items[1].ID, "agent-1", "", user.ID, 15*time.Minute); err != nil {
		t.Fatalf("ClaimItemLease Beta: %v", err)
	}

	// Neither Alpha nor Beta may be used as a park anchor (not in queue).
	for _, id := range []string{items[0].ID, items[1].ID} {
		if _, err := s.SetRolePark(ws.ID, role.ID, ParkBefore(id), nil, user.ID); !errors.Is(err, ErrParkAnchorNotInQueue) {
			t.Errorf("Before(%s): got %v, want ErrParkAnchorNotInQueue", id, err)
		}
		if _, err := s.SetRolePark(ws.ID, role.ID, ParkAfter(id), nil, user.ID); !errors.Is(err, ErrParkAnchorNotInQueue) {
			t.Errorf("After(%s): got %v, want ErrParkAnchorNotInQueue", id, err)
		}
	}

	// Parking at top: first parked queue ref skips Alpha & Beta (Now) and returns Gamma.
	park, err := s.SetRolePark(ws.ID, role.ID, ParkAtTop(), nil, user.ID)
	if err != nil {
		t.Fatalf("SetRolePark AtTop: %v", err)
	}
	firstRef, err := s.FirstParkedQueueRef(ws.ID, role.ID, park, nil, nil)
	if err != nil {
		t.Fatalf("FirstParkedQueueRef: %v", err)
	}
	if firstRef == nil || *firstRef != items[2].Ref {
		t.Fatalf("first_parked_ref = %v, want %q (Gamma)", firstRef, items[2].Ref)
	}

	// Verify shared IsItemNow helper agrees on both Now items and not on Gamma.
	alphaCur, _ := s.GetItem(items[0].ID)
	betaCur, _ := s.GetItem(items[1].ID)
	gammaCur, _ := s.GetItem(items[2].ID)
	if !IsItemNow(alphaCur, "status", false) {
		t.Error("Alpha (in_progress) must be Now")
	}
	if !IsItemNow(betaCur, "status", true) {
		t.Error("Beta (live lease) must be Now")
	}
	if IsItemNow(gammaCur, "status", false) {
		t.Error("Gamma (open, no lease) must NOT be Now")
	}
}

// TestRolePark_SetByNeverEmail pins the setter display: the name when there is
// one, else the username, never the email. Role reads go to every member,
// guests included, so an email there would leak an address.
func TestRolePark_SetByNeverEmail(t *testing.T) {
	t.Parallel()
	s, ws, _, role, user, _ := seedRoleParkFixture(t)

	park, err := s.SetRolePark(ws.ID, role.ID, ParkAtTop(), nil, user.ID)
	if err != nil {
		t.Fatalf("SetRolePark: %v", err)
	}
	if park.SetBy == nil || *park.SetBy != "Parker" {
		t.Fatalf("set_by = %v, want Parker", park.SetBy)
	}

	if _, err := s.db.Exec(s.q(`UPDATE users SET name = '' WHERE id = ?`), user.ID); err != nil {
		t.Fatalf("blank name: %v", err)
	}
	park, err = s.SetRolePark(ws.ID, role.ID, ParkAtTop(), nil, user.ID)
	if err != nil {
		t.Fatalf("SetRolePark again: %v", err)
	}
	if park.SetBy == nil || *park.SetBy != "parker" {
		t.Fatalf("set_by with blank name = %v, want the username parker", park.SetBy)
	}
	got, err := s.GetAgentRole(ws.ID, role.ID)
	if err != nil {
		t.Fatalf("GetAgentRole: %v", err)
	}
	roles, err := s.ListAgentRoles(ws.ID)
	if err != nil {
		t.Fatalf("ListAgentRoles: %v", err)
	}
	for _, p := range []*models.RolePark{got.Park, roles[0].Park} {
		if p == nil || p.SetBy == nil || *p.SetBy != "parker" {
			t.Fatalf("role read set_by = %v, want parker", p)
		}
	}
}

// TestRoleArrival_FastPathBelowEveryLiveItem pins that an arrival
// lands below every live item of the role, terminal ones included, without
// loading the lane: the value is max(role_sort_order) + 1024.
func TestRoleArrival_FastPathBelowEveryLiveItem(t *testing.T) {
	t.Parallel()
	s, ws, col, role, _, items := seedRoleParkFixture(t)

	if err := s.UpdateRoleSortOrder(ws.ID, []RoleSortUpdate{{ItemID: items[0].ID, RoleSortOrder: 50000}}); err != nil {
		t.Fatalf("UpdateRoleSortOrder: %v", err)
	}
	if _, err := s.UpdateItem(items[0].ID, models.ItemUpdate{Fields: strPtr(`{"status":"done"}`)}); err != nil {
		t.Fatalf("mark done: %v", err)
	}
	it, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: "Arrival", Fields: `{"status":"open"}`, AgentRoleID: &role.ID})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	if it.RoleSortOrder != 50000+1024 {
		t.Fatalf("arrival role_sort_order = %d, want %d", it.RoleSortOrder, 50000+1024)
	}
}
