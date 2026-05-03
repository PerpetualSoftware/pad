package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Store-layer tests for the MCP audit log (PLAN-943 TASK-960).
//
// What's covered:
//
//   - InsertMCPAuditEntry validates required fields and round-trips
//     correctly via ListMCPAuditByUser.
//   - WorkspaceID + ErrorKind round-trip cleanly through the
//     nullable-pointer path.
//   - MCPConnectionStatsForUser produces correct last-used + 30-day
//     counts, grouped by (token_kind, token_ref).
//   - SweepMCPAuditOlderThan deletes only rows older than the cutoff.
//   - Per-user / per-connection ordering is reverse-chronological.
//   - Pagination via limit + offset.

func newAuditTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := New(filepath.Join(dir, "audit.db"))
	if err != nil {
		t.Fatalf("New store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// auditUser creates a user that subsequent audit rows can FK-reference.
// The schema requires user_id REFERENCES users(id), so we can't insert
// audit rows for a phantom UUID.
func auditUser(t *testing.T, s *Store, email string) string {
	t.Helper()
	u, err := s.CreateUser(models.UserCreate{
		Email:    email,
		Name:     "Audit Tester",
		Password: "pw-audit-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return u.ID
}

func TestInsertMCPAuditEntry_RequiredFields(t *testing.T) {
	s := newAuditTestStore(t)
	uid := auditUser(t, s, "req-fields@example.com")

	cases := []struct {
		name string
		in   models.MCPAuditEntryInput
		want string
	}{
		{"missing user_id", models.MCPAuditEntryInput{TokenKind: "oauth", TokenRef: "ref", ToolName: "x", RequestID: "r"}, "user_id"},
		{"missing token_kind", models.MCPAuditEntryInput{UserID: uid, TokenRef: "ref", ToolName: "x", RequestID: "r"}, "token_kind"},
		{"missing token_ref", models.MCPAuditEntryInput{UserID: uid, TokenKind: "oauth", ToolName: "x", RequestID: "r"}, "token_ref"},
		{"missing tool_name", models.MCPAuditEntryInput{UserID: uid, TokenKind: "oauth", TokenRef: "ref", RequestID: "r"}, "tool_name"},
		{"missing request_id", models.MCPAuditEntryInput{UserID: uid, TokenKind: "oauth", TokenRef: "ref", ToolName: "x"}, "request_id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := s.InsertMCPAuditEntry(tc.in)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
		})
	}
}

func TestInsertMCPAuditEntry_RoundTrip(t *testing.T) {
	s := newAuditTestStore(t)
	uid := auditUser(t, s, "roundtrip@example.com")
	wsID := "" // workspace nullable; leave empty so the column is NULL

	now := time.Now().UTC().Truncate(time.Second)
	in := models.MCPAuditEntryInput{
		Timestamp:    now,
		UserID:       uid,
		WorkspaceID:  wsID,
		TokenKind:    models.TokenKindOAuth,
		TokenRef:     "req-id-abc",
		ToolName:     "pad_item",
		ArgsHash:     "deadbeef",
		ResultStatus: models.MCPAuditResultOK,
		LatencyMs:    42,
		RequestID:    "req-x",
	}
	if err := s.InsertMCPAuditEntry(in); err != nil {
		t.Fatalf("InsertMCPAuditEntry: %v", err)
	}

	rows, err := s.ListMCPAuditByUser(uid, 10, 0)
	if err != nil {
		t.Fatalf("ListMCPAuditByUser: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("len rows = %d, want 1", len(rows))
	}
	got := rows[0]
	if got.UserID != uid || got.TokenKind != models.TokenKindOAuth ||
		got.TokenRef != "req-id-abc" || got.ToolName != "pad_item" ||
		got.ArgsHash != "deadbeef" || got.LatencyMs != 42 ||
		got.RequestID != "req-x" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if got.WorkspaceID != nil {
		t.Errorf("WorkspaceID = %v, want nil", *got.WorkspaceID)
	}
	if got.ErrorKind != nil {
		t.Errorf("ErrorKind = %v, want nil", *got.ErrorKind)
	}
	if !got.Timestamp.Equal(now) {
		// RFC3339 truncates to seconds; compare via .Equal which handles location.
		t.Errorf("Timestamp = %v, want %v", got.Timestamp, now)
	}
}

func TestInsertMCPAuditEntry_NullableFieldsPopulated(t *testing.T) {
	s := newAuditTestStore(t)
	uid := auditUser(t, s, "nullable@example.com")
	// Need an actual workspace row for the FK on workspace_id
	w, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "WS-Audit", Slug: "ws-audit"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}

	in := models.MCPAuditEntryInput{
		UserID:       uid,
		WorkspaceID:  w.ID,
		TokenKind:    models.TokenKindOAuth,
		TokenRef:     "ref",
		ToolName:     "tools/list",
		ArgsHash:     "",
		ResultStatus: models.MCPAuditResultError,
		ErrorKind:    "client_error_400",
		LatencyMs:    7,
		RequestID:    "req-y",
	}
	if err := s.InsertMCPAuditEntry(in); err != nil {
		t.Fatalf("InsertMCPAuditEntry: %v", err)
	}
	rows, err := s.ListMCPAuditByUser(uid, 10, 0)
	if err != nil {
		t.Fatalf("ListMCPAuditByUser: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("len rows = %d", len(rows))
	}
	if rows[0].WorkspaceID == nil || *rows[0].WorkspaceID != w.ID {
		t.Errorf("WorkspaceID round-trip failed: %v", rows[0].WorkspaceID)
	}
	if rows[0].ErrorKind == nil || *rows[0].ErrorKind != "client_error_400" {
		t.Errorf("ErrorKind round-trip failed: %v", rows[0].ErrorKind)
	}
}

func TestListMCPAuditByUser_OrderingAndPagination(t *testing.T) {
	s := newAuditTestStore(t)
	uid := auditUser(t, s, "ordering@example.com")
	base := time.Now().UTC().Truncate(time.Second)

	// Insert 5 rows, each 1 second apart. ListMCPAuditByUser should
	// return them in reverse-chronological order regardless of insert
	// order. Insert in scrambled order to prove the sort.
	for _, off := range []int{2, 4, 0, 1, 3} {
		err := s.InsertMCPAuditEntry(models.MCPAuditEntryInput{
			Timestamp:    base.Add(time.Duration(off) * time.Second),
			UserID:       uid,
			TokenKind:    models.TokenKindOAuth,
			TokenRef:     "r",
			ToolName:     "t",
			ResultStatus: models.MCPAuditResultOK,
			RequestID:    "rq",
		})
		if err != nil {
			t.Fatalf("InsertMCPAuditEntry: %v", err)
		}
	}

	rows, err := s.ListMCPAuditByUser(uid, 100, 0)
	if err != nil {
		t.Fatalf("ListMCPAuditByUser: %v", err)
	}
	if len(rows) != 5 {
		t.Fatalf("len rows = %d, want 5", len(rows))
	}
	for i := 1; i < len(rows); i++ {
		if rows[i-1].Timestamp.Before(rows[i].Timestamp) {
			t.Errorf("rows out of order at %d: %v before %v", i, rows[i-1].Timestamp, rows[i].Timestamp)
		}
	}

	// Pagination — limit=2, offset=2 should return rows[2:4] from the
	// reverse-chronological list (= the 3rd and 4th newest).
	page, err := s.ListMCPAuditByUser(uid, 2, 2)
	if err != nil {
		t.Fatalf("paged ListMCPAuditByUser: %v", err)
	}
	if len(page) != 2 {
		t.Fatalf("paged len = %d, want 2", len(page))
	}
	if !page[0].Timestamp.Equal(rows[2].Timestamp) || !page[1].Timestamp.Equal(rows[3].Timestamp) {
		t.Errorf("pagination drift: page[0]=%v rows[2]=%v page[1]=%v rows[3]=%v",
			page[0].Timestamp, rows[2].Timestamp, page[1].Timestamp, rows[3].Timestamp)
	}
}

func TestListMCPAuditByConnection_FiltersByOwner(t *testing.T) {
	s := newAuditTestStore(t)
	alice := auditUser(t, s, "alice@example.com")
	bob := auditUser(t, s, "bob@example.com")

	// Both users have an entry against the SAME (oauth, ref-shared)
	// pair. The store query MUST filter on user_id to prevent Bob
	// from reading Alice's row.
	insert := func(uid string) {
		err := s.InsertMCPAuditEntry(models.MCPAuditEntryInput{
			UserID:       uid,
			TokenKind:    models.TokenKindOAuth,
			TokenRef:     "ref-shared",
			ToolName:     "pad_item",
			ResultStatus: models.MCPAuditResultOK,
			RequestID:    "req",
		})
		if err != nil {
			t.Fatalf("InsertMCPAuditEntry %s: %v", uid, err)
		}
	}
	insert(alice)
	insert(bob)

	got, err := s.ListMCPAuditByConnection(alice, models.TokenKindOAuth, "ref-shared", 10, 0)
	if err != nil {
		t.Fatalf("ListMCPAuditByConnection: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1 (Alice's row only)", len(got))
	}
	if got[0].UserID != alice {
		t.Errorf("returned wrong user's row: %s, want Alice (%s)", got[0].UserID, alice)
	}
}

func TestMCPConnectionStatsForUser_LastUsedAndCalls30d(t *testing.T) {
	s := newAuditTestStore(t)
	uid := auditUser(t, s, "stats@example.com")

	now := time.Now().UTC()

	// Connection A: three recent calls (within 30d) + one old call (>30d ago).
	// last_used should be the most recent recent call; calls_30d = 3.
	connA := []time.Time{
		now.Add(-1 * time.Hour),       // recent
		now.Add(-2 * time.Hour),       // recent
		now.Add(-72 * time.Hour),      // recent (3 days)
		now.Add(-40 * 24 * time.Hour), // outside 30d
	}
	for _, ts := range connA {
		err := s.InsertMCPAuditEntry(models.MCPAuditEntryInput{
			Timestamp:    ts,
			UserID:       uid,
			TokenKind:    models.TokenKindOAuth,
			TokenRef:     "conn-a",
			ToolName:     "pad_item",
			ResultStatus: models.MCPAuditResultOK,
			RequestID:    "rq",
		})
		if err != nil {
			t.Fatalf("InsertMCPAuditEntry: %v", err)
		}
	}

	// Connection B: one recent call. Different (kind, ref) so it
	// produces a separate group.
	err := s.InsertMCPAuditEntry(models.MCPAuditEntryInput{
		Timestamp:    now.Add(-30 * time.Minute),
		UserID:       uid,
		TokenKind:    models.TokenKindPAT,
		TokenRef:     "conn-b",
		ToolName:     "pad_item",
		ResultStatus: models.MCPAuditResultOK,
		RequestID:    "rq",
	})
	if err != nil {
		t.Fatalf("InsertMCPAuditEntry: %v", err)
	}

	stats, err := s.MCPConnectionStatsForUser(uid)
	if err != nil {
		t.Fatalf("MCPConnectionStatsForUser: %v", err)
	}
	if len(stats) != 2 {
		t.Fatalf("got %d connection groups, want 2", len(stats))
	}

	a, ok := stats[MCPConnectionStatsKey(models.TokenKindOAuth, "conn-a")]
	if !ok {
		t.Fatal("missing conn-a stats")
	}
	if a.Calls30d != 3 {
		t.Errorf("conn-a Calls30d = %d, want 3", a.Calls30d)
	}
	if a.LastUsedAt == nil {
		t.Fatal("conn-a LastUsedAt == nil")
	}
	// LastUsedAt should be the most recent insert (~1h ago), within
	// 1 second of the source timestamp once round-tripped through
	// RFC3339 + RFC3339 parse.
	if got := a.LastUsedAt.UTC(); got.Before(connA[0].Add(-time.Second)) || got.After(connA[0].Add(time.Second)) {
		t.Errorf("conn-a LastUsedAt = %v, want ~ %v", got, connA[0])
	}

	b, ok := stats[MCPConnectionStatsKey(models.TokenKindPAT, "conn-b")]
	if !ok {
		t.Fatal("missing conn-b stats")
	}
	if b.Calls30d != 1 {
		t.Errorf("conn-b Calls30d = %d, want 1", b.Calls30d)
	}
}

func TestSweepMCPAuditOlderThan_DeletesOldRowsOnly(t *testing.T) {
	s := newAuditTestStore(t)
	uid := auditUser(t, s, "sweep@example.com")
	now := time.Now().UTC()

	// 1 row inside retention, 1 row outside.
	in := func(ts time.Time, ref string) {
		err := s.InsertMCPAuditEntry(models.MCPAuditEntryInput{
			Timestamp:    ts,
			UserID:       uid,
			TokenKind:    models.TokenKindOAuth,
			TokenRef:     ref,
			ToolName:     "t",
			ResultStatus: models.MCPAuditResultOK,
			RequestID:    "rq",
		})
		if err != nil {
			t.Fatalf("InsertMCPAuditEntry: %v", err)
		}
	}
	in(now.Add(-1*time.Hour), "fresh")
	in(now.Add(-100*24*time.Hour), "stale")

	cutoff := now.Add(-90 * 24 * time.Hour)
	n, err := s.SweepMCPAuditOlderThan(cutoff)
	if err != nil {
		t.Fatalf("SweepMCPAuditOlderThan: %v", err)
	}
	if n != 1 {
		t.Errorf("swept rows = %d, want 1", n)
	}

	rows, err := s.ListMCPAuditByUser(uid, 100, 0)
	if err != nil {
		t.Fatalf("ListMCPAuditByUser: %v", err)
	}
	if len(rows) != 1 || rows[0].TokenRef != "fresh" {
		t.Errorf("after sweep, got %d rows; want 1 ('fresh'). got=%+v", len(rows), rows)
	}
}

func TestListAllMCPAudit_ReturnsAcrossUsers(t *testing.T) {
	s := newAuditTestStore(t)
	a := auditUser(t, s, "all-a@example.com")
	b := auditUser(t, s, "all-b@example.com")
	for _, uid := range []string{a, b} {
		err := s.InsertMCPAuditEntry(models.MCPAuditEntryInput{
			UserID:       uid,
			TokenKind:    models.TokenKindOAuth,
			TokenRef:     "r",
			ToolName:     "t",
			ResultStatus: models.MCPAuditResultOK,
			RequestID:    "rq",
		})
		if err != nil {
			t.Fatalf("InsertMCPAuditEntry %s: %v", uid, err)
		}
	}
	rows, err := s.ListAllMCPAudit(100, 0)
	if err != nil {
		t.Fatalf("ListAllMCPAudit: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("len = %d, want 2 (across users)", len(rows))
	}
}
