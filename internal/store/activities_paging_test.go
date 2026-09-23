package store

import (
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2781: activity feeds page over rows that MOVE (a debounce merge
// restamps created_at) and, before this fix, ordered them by a column stored
// at whole-second precision with no tie-break.

// insertActivityAt writes one activity with an explicit id and timestamp, so a
// test controls both halves of the (created_at, id) order.
func insertActivityAt(t *testing.T, s *Store, id, workspaceID, documentID, userID string, at time.Time) {
	t.Helper()
	_, err := s.db.Exec(s.q(`
		INSERT INTO activities (id, workspace_id, document_id, action, actor, source, metadata, user_id, created_at)
		VALUES (?, ?, ?, 'updated', 'user', 'web', '{}', ?, ?)
	`), id, nilIfEmpty(workspaceID), nilIfEmpty(documentID), nilIfEmpty(userID), at.UTC().Format(time.RFC3339))
	if err != nil {
		t.Fatalf("insert activity %s: %v", id, err)
	}
}

func activityIDs(as []models.Activity) []string {
	ids := make([]string, len(as))
	for i, a := range as {
		ids[i] = a.ID
	}
	return ids
}

// Rows sharing a second must come back in ONE defined order, (created_at, id)
// descending, on every feed.
//
// THE INSERTION ORDER IS THE INSTRUMENT. Without a tie-break an engine returns
// ties in whatever order its scan meets them, and SQLite walks the
// (workspace_id, created_at) index BACKWARDS for DESC, so ascending insertion
// comes back as descending ids and a first version of this test passed on the
// unfixed code. 2, 3, 1 differs from id-descending in BOTH scan directions
// (forward 2,3,1; backward 1,3,2), so the test can only pass on a real
// tie-break.
func TestActivityFeedsBreakTimestampTiesByIDDescending(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Ties")
	user := createTestUser(t, s, "ties@example.com", "Ties", "password123")

	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	for _, id := range []string{"tie-2", "tie-3", "tie-1"} {
		insertActivityAt(t, s, id, ws.ID, "doc-ties", user.ID, at)
	}
	want := []string{"tie-3", "tie-2", "tie-1"}

	feeds := map[string]func() ([]models.Activity, error){
		"workspace": func() ([]models.Activity, error) {
			return s.ListWorkspaceActivity(ws.ID, models.ActivityListParams{Limit: 10})
		},
		"document": func() ([]models.Activity, error) {
			return s.ListDocumentActivity("doc-ties", models.ActivityListParams{Limit: 10})
		},
		"user": func() ([]models.Activity, error) {
			return s.ListUserActivity(user.ID, models.ActivityListParams{Limit: 10})
		},
		"audit": func() ([]models.Activity, error) {
			return s.ListAuditLog(models.AuditLogParams{WorkspaceID: ws.ID, Limit: 10})
		},
	}
	for name, list := range feeds {
		t.Run(name, func(t *testing.T) {
			got, err := list()
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			ids := activityIDs(got)
			if len(ids) != len(want) {
				t.Fatalf("got %v, want %v", ids, want)
			}
			for i := range want {
				if ids[i] != want[i] {
					t.Fatalf("got %v, want %v", ids, want)
				}
			}
		})
	}
}

// The movement half, pinned as it behaves under OFFSET paging: a debounce
// merge restamps an older, not-yet-fetched row to now. Every row behind it
// shifts down one position, so the next offset page REPEATS the last row of
// the previous one; and the moved row is now ahead of everything fetched, so
// no later page contains it. The repeat is what keyset paging removes. The
// missing moved row is NOT closed by any paging scheme — see BUG-2781's
// trail — and this test pins that too, so a later claim that it is closed
// has to change a test.
func TestOffsetPagingRepeatsARowWhenADebounceMergeMovesAnOlderOne(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Moves")

	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	// a newest ... f oldest, one second apart.
	for i, id := range []string{"a", "b", "c", "d", "e", "f"} {
		insertActivityAt(t, s, "mv-"+id, ws.ID, "doc-"+id, "", base.Add(-time.Duration(i)*time.Second))
	}

	page1, err := s.ListWorkspaceActivity(ws.ID, models.ActivityListParams{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got := activityIDs(page1); len(got) != 2 || got[0] != "mv-a" || got[1] != "mv-b" {
		t.Fatalf("page 1: got %v", got)
	}

	// The real mover: the debounce merge's compare-and-set UPDATE.
	moved, err := s.mergeIntoUnlinkedActivity("mv-e", "{}", `{"changes":"x"}`, base.Add(time.Second).UTC().Format(time.RFC3339))
	if err != nil || !moved {
		t.Fatalf("precondition: the merge must restamp mv-e (moved=%v err=%v)", moved, err)
	}

	page2, err := s.ListWorkspaceActivity(ws.ID, models.ActivityListParams{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatal(err)
	}
	got := activityIDs(page2)
	if len(got) != 2 || got[0] != "mv-b" || got[1] != "mv-c" {
		t.Fatalf("page 2 under offset: got %v, want [mv-b mv-c] (mv-b repeated)", got)
	}
	for _, id := range append(activityIDs(page1), got...) {
		if id == "mv-e" {
			t.Fatal("the moved row appeared in a fetched page")
		}
	}
}

// The keyset twin of the offset test above: the same move between the same
// two pages, paged by (created_at, id) cursor. The repeat is gone; the moved
// row is still absent, which is the documented limit.
func TestKeysetPagingDoesNotRepeatWhenADebounceMergeMovesAnOlderOne(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Keyset moves")

	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	for i, id := range []string{"a", "b", "c", "d", "e", "f"} {
		insertActivityAt(t, s, "ks-"+id, ws.ID, "doc-"+id, "", base.Add(-time.Duration(i)*time.Second))
	}

	page1, err := s.ListWorkspaceActivity(ws.ID, models.ActivityListParams{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	last := page1[len(page1)-1]

	moved, err := s.mergeIntoUnlinkedActivity("ks-e", "{}", `{"changes":"x"}`, base.Add(time.Second).UTC().Format(time.RFC3339))
	if err != nil || !moved {
		t.Fatalf("precondition: the merge must restamp ks-e (moved=%v err=%v)", moved, err)
	}

	page2, err := s.ListWorkspaceActivity(ws.ID, models.ActivityListParams{Limit: 2, Before: last.CreatedAt, BeforeID: last.ID})
	if err != nil {
		t.Fatal(err)
	}
	got := activityIDs(page2)
	if len(got) != 2 || got[0] != "ks-c" || got[1] != "ks-d" {
		t.Fatalf("page 2 under keyset: got %v, want [ks-c ks-d]", got)
	}
	page3, err := s.ListWorkspaceActivity(ws.ID, models.ActivityListParams{Limit: 10, Before: page2[1].CreatedAt, BeforeID: page2[1].ID})
	if err != nil {
		t.Fatal(err)
	}
	if got := activityIDs(page3); len(got) != 1 || got[0] != "ks-f" {
		t.Fatalf("page 3 under keyset: got %v, want [ks-f] (ks-e moved ahead of every cursor)", got)
	}
}

// A cursor that lands INSIDE a run of equal timestamps must neither skip nor
// repeat the rest of the run, on every keyset-capable feed. Limit 1 puts a
// page boundary between every pair of tied rows.
func TestKeysetPagingWalksATimestampTieExactlyOnce(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Keyset ties")
	user := createTestUser(t, s, "kties@example.com", "KTies", "password123")

	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	insertActivityAt(t, s, "kt-0", ws.ID, "doc-kt", user.ID, at.Add(time.Second))
	for _, id := range []string{"kt-2", "kt-3", "kt-1"} {
		insertActivityAt(t, s, id, ws.ID, "doc-kt", user.ID, at)
	}
	insertActivityAt(t, s, "kt-z", ws.ID, "doc-kt", user.ID, at.Add(-time.Second))
	want := []string{"kt-0", "kt-3", "kt-2", "kt-1", "kt-z"}

	feeds := map[string]func(before time.Time, beforeID string) ([]models.Activity, error){
		"workspace": func(b time.Time, id string) ([]models.Activity, error) {
			return s.ListWorkspaceActivity(ws.ID, models.ActivityListParams{Limit: 1, Before: b, BeforeID: id})
		},
		"document": func(b time.Time, id string) ([]models.Activity, error) {
			return s.ListDocumentActivity("doc-kt", models.ActivityListParams{Limit: 1, Before: b, BeforeID: id})
		},
		"user": func(b time.Time, id string) ([]models.Activity, error) {
			return s.ListUserActivity(user.ID, models.ActivityListParams{Limit: 1, Before: b, BeforeID: id})
		},
		"audit": func(b time.Time, id string) ([]models.Activity, error) {
			return s.ListAuditLog(models.AuditLogParams{WorkspaceID: ws.ID, Limit: 1, Before: b, BeforeID: id})
		},
	}
	for name, page := range feeds {
		t.Run(name, func(t *testing.T) {
			var seen []string
			var before time.Time
			var beforeID string
			for range len(want) + 2 {
				got, err := page(before, beforeID)
				if err != nil {
					t.Fatal(err)
				}
				if len(got) == 0 {
					break
				}
				seen = append(seen, got[0].ID)
				before, beforeID = got[0].CreatedAt, got[0].ID
			}
			if len(seen) != len(want) {
				t.Fatalf("walked %v, want %v", seen, want)
			}
			for i := range want {
				if seen[i] != want[i] {
					t.Fatalf("walked %v, want %v", seen, want)
				}
			}
		})
	}
}

// mcp_audit_log gets the same tie-break (BUG-2781, the lead's ruling): its
// rows are append-only, but rows sharing a second still come back in scan
// order, which can differ between two page requests. Same insertion-order
// instrument as the activity ties test above.
func TestMCPAuditListsBreakTimestampTiesByIDDescending(t *testing.T) {
	s := testStore(t)
	user := createTestUser(t, s, "mcpties@example.com", "MCPTies", "password123")

	at := time.Now().Add(-time.Hour).Truncate(time.Second).UTC().Format(time.RFC3339)
	for _, id := range []string{"mt-2", "mt-3", "mt-1"} {
		if _, err := s.db.Exec(s.q(`
			INSERT INTO mcp_audit_log (
				id, timestamp, user_id, workspace_id,
				token_kind, token_ref, tool_name, args_hash,
				result_status, error_kind, latency_ms, request_id
			) VALUES (?, ?, ?, NULL, 'pat', 'ref-1', 'pad_item', 'h', 'ok', NULL, 1, ?)
		`), id, at, user.ID, "req-"+id); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	want := []string{"mt-3", "mt-2", "mt-1"}

	lists := map[string]func() ([]models.MCPAuditEntry, error){
		"by user": func() ([]models.MCPAuditEntry, error) { return s.ListMCPAuditByUser(user.ID, 10, 0) },
		"by connection": func() ([]models.MCPAuditEntry, error) {
			return s.ListMCPAuditByConnection(user.ID, models.TokenKindPAT, "ref-1", 10, 0)
		},
		"all": func() ([]models.MCPAuditEntry, error) { return s.ListAllMCPAudit(10, 0) },
	}
	for name, list := range lists {
		t.Run(name, func(t *testing.T) {
			got, err := list()
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, e := range got {
				ids = append(ids, e.ID)
			}
			if len(ids) != 3 || ids[0] != want[0] || ids[1] != want[1] || ids[2] != want[2] {
				t.Fatalf("got %v, want %v", ids, want)
			}
		})
	}
}
