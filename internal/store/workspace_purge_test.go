package store

import (
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// seededWorkspace captures the IDs needed to assert — after a purge —
// that no child row was orphaned. Item-scoped child tables (versions,
// reactions, stars, ...) have no workspace_id, so they're checked
// against the original parent IDs rather than a workspace filter: a
// leaked row is only detectable that way once the parents are gone.
type seededWorkspace struct {
	wsID        string
	slug        string
	item1       string
	item2       string
	commentID   string
	docID       string
	shareLinkID string
	mcpAuditID  string
}

// wsChildTables is every table carrying a workspace_id column that the
// purge must clear (mcp_audit_log excluded — it's de-identified, not
// deleted, and asserted separately). Used to prove "no orphans" (all
// zero after purging a workspace) and "no over-purge" (all intact after
// purging a DIFFERENT workspace).
var wsChildTables = []string{
	"items", "comments", "item_links", "item_collection_moves", "item_grants",
	"collection_grants", "status_transitions", "views", "collections",
	"documents", "agent_roles", "progress_snapshots", "webhooks",
	"workspace_invitations", "custom_templates", "api_tokens", "share_links",
	"oauth_connection_workspaces", "user_report_layouts",
	"member_collection_access", "workspace_members", "attachments", "activities",
}

func (s *Store) mustExec(t *testing.T, query string, args ...interface{}) {
	t.Helper()
	if _, err := s.db.Exec(s.q(query), args...); err != nil {
		t.Fatalf("seed exec %q: %v", query, err)
	}
}

func (s *Store) countRows(t *testing.T, query string, args ...interface{}) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(s.q(query), args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

// seedFullWorkspace inserts one row into every workspace-scoped child
// table so a purge can be proven exhaustive. Raw inserts deliberately
// omit columns that have defaults (including every JSONB column) so the
// same statements run unchanged on SQLite and PostgreSQL.
func seedFullWorkspace(t *testing.T, s *Store, u *models.User, name string) seededWorkspace {
	t.Helper()

	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: name, OwnerID: u.ID})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	coll := createTestCollection(t, s, ws.ID, "Tasks")
	item1 := createTestItem(t, s, ws.ID, coll.ID, "Item One", "")
	item2 := createTestItem(t, s, ws.ID, coll.ID, "Item Two", "")

	if err := s.AddWorkspaceMember(ws.ID, u.ID, "owner"); err != nil {
		t.Fatalf("add member: %v", err)
	}

	ts := now()
	commentID := newID()
	docID := newID()
	shareLinkID := newID()
	mcpAuditID := newID()
	oauthReq := "conn-" + newID()

	// comments + comment_reactions
	s.mustExec(t, `INSERT INTO comments (id, item_id, workspace_id, body, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		commentID, item1.ID, ws.ID, "hi", ts, ts)
	s.mustExec(t, `INSERT INTO comment_reactions (id, comment_id, emoji, created_at) VALUES (?, ?, ?, ?)`,
		newID(), commentID, "👍", ts)

	// item_versions / item_links / item_stars / item_yjs_updates /
	// item_wiki_links / item_collection_moves / item_grants / status_transitions
	s.mustExec(t, `INSERT INTO item_versions (id, item_id, content, created_at) VALUES (?, ?, ?, ?)`,
		newID(), item1.ID, "v1", ts)
	s.mustExec(t, `INSERT INTO item_links (id, workspace_id, source_id, target_id, created_at) VALUES (?, ?, ?, ?, ?)`,
		newID(), ws.ID, item1.ID, item2.ID, ts)
	s.mustExec(t, `INSERT INTO item_stars (user_id, item_id, created_at) VALUES (?, ?, ?)`,
		u.ID, item1.ID, ts)
	s.mustExec(t, `INSERT INTO item_yjs_updates (item_id, update_data, schema_version, created_at) VALUES (?, ?, ?, ?)`,
		item1.ID, []byte{1, 2, 3}, "v1", ts)
	s.mustExec(t, `INSERT INTO item_wiki_links (source_item_id, target_kind, position) VALUES (?, ?, ?)`,
		item1.ID, "ref", 0)
	s.mustExec(t, `INSERT INTO item_collection_moves (id, workspace_id, item_id, from_collection_id, to_collection_id, seq, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		newID(), ws.ID, item1.ID, coll.ID, coll.ID, 1, ts)
	s.mustExec(t, `INSERT INTO item_grants (id, item_id, workspace_id, user_id, granted_by, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		newID(), item1.ID, ws.ID, u.ID, u.ID, ts)
	s.mustExec(t, `INSERT INTO status_transitions (id, item_id, workspace_id, collection_id, to_status, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		newID(), item1.ID, ws.ID, coll.ID, "done", ts)

	// collection_grants / views
	s.mustExec(t, `INSERT INTO collection_grants (id, collection_id, workspace_id, user_id, granted_by, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		newID(), coll.ID, ws.ID, u.ID, u.ID, ts)
	s.mustExec(t, `INSERT INTO views (id, workspace_id, name, slug, view_type, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		newID(), ws.ID, "Board", "board", "kanban", ts, ts)

	// member_collection_access (FK to workspace_members(ws,user))
	s.mustExec(t, `INSERT INTO member_collection_access (workspace_id, user_id, collection_id, created_at) VALUES (?, ?, ?, ?)`,
		ws.ID, u.ID, coll.ID, ts)

	// documents + versions
	s.mustExec(t, `INSERT INTO documents (id, workspace_id, title, slug, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		docID, ws.ID, "Doc", "doc", ts, ts)
	s.mustExec(t, `INSERT INTO versions (id, document_id, content, created_at) VALUES (?, ?, ?, ?)`,
		newID(), docID, "d1", ts)

	// agent_roles / progress_snapshots / webhooks / invitations / custom_templates
	s.mustExec(t, `INSERT INTO agent_roles (id, workspace_id, slug, name, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		newID(), ws.ID, "eng", "Engineer", ts, ts)
	s.mustExec(t, `INSERT INTO progress_snapshots (id, workspace_id, created_at) VALUES (?, ?, ?)`,
		newID(), ws.ID, ts)
	s.mustExec(t, `INSERT INTO webhooks (id, workspace_id, url) VALUES (?, ?, ?)`,
		newID(), ws.ID, "https://example.test/hook")
	s.mustExec(t, `INSERT INTO workspace_invitations (id, workspace_id, email, invited_by, code) VALUES (?, ?, ?, ?, ?)`,
		newID(), ws.ID, "invitee@test.com", u.ID, "code-"+newID())
	s.mustExec(t, `INSERT INTO custom_templates (id, workspace_id, name, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		newID(), ws.ID, "Tmpl", ts, ts)

	// workspace-scoped api token
	s.mustExec(t, `INSERT INTO api_tokens (id, workspace_id, name, token_hash, prefix) VALUES (?, ?, ?, ?, ?)`,
		newID(), ws.ID, "ci", "hash-"+newID(), "pad_")

	// share_links + share_link_views
	s.mustExec(t, `INSERT INTO share_links (id, token_hash, target_type, target_id, workspace_id, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		shareLinkID, "sh-"+newID(), "item", item1.ID, ws.ID, u.ID, ts)
	s.mustExec(t, `INSERT INTO share_link_views (id, share_link_id, viewed_at) VALUES (?, ?, ?)`,
		newID(), shareLinkID, ts)

	// oauth connection + join row
	if err := s.CreateOAuthConnection(OAuthConnection{RequestID: oauthReq, UserID: u.ID, Name: "Claude"}); err != nil {
		t.Fatalf("create oauth connection: %v", err)
	}
	s.mustExec(t, `INSERT INTO oauth_connection_workspaces (request_id, workspace_id) VALUES (?, ?)`,
		oauthReq, ws.ID)

	// user_report_layouts (ON DELETE CASCADE from workspaces — proves the
	// explicit delete is harmless and covered)
	s.mustExec(t, `INSERT INTO user_report_layouts (user_id, workspace_id, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		u.ID, ws.ID, ts, ts)

	// activity
	if _, err := s.CreateActivity(models.Activity{WorkspaceID: ws.ID, Action: "updated", Actor: "user", Source: "web", UserID: u.ID}); err != nil {
		t.Fatalf("create activity: %v", err)
	}

	// attachment (row only — blob reclamation is exercised at the server layer)
	if err := s.CreateAttachment(&models.Attachment{
		WorkspaceID: ws.ID, ItemID: &item1.ID, UploadedBy: u.ID,
		StorageKey: "fs:" + newID(), ContentHash: newID(),
		MimeType: "image/png", SizeBytes: 3, Filename: "a.png",
	}); err != nil {
		t.Fatalf("create attachment: %v", err)
	}

	// mcp_audit_log row scoped to this workspace (must be de-identified,
	// not deleted, by the purge).
	s.mustExec(t, `INSERT INTO mcp_audit_log (id, timestamp, user_id, workspace_id, token_kind, token_ref, tool_name, args_hash, result_status, latency_ms, request_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		mcpAuditID, ts, u.ID, ws.ID, "pat", "tok-1", "pad_item", "", "ok", 0, "req-"+newID())

	return seededWorkspace{
		wsID: ws.ID, slug: ws.Slug, item1: item1.ID, item2: item2.ID,
		commentID: commentID, docID: docID, shareLinkID: shareLinkID, mcpAuditID: mcpAuditID,
	}
}

// totalChildRows sums every seeded child row for a workspace, checking
// item-scoped tables against the original parent IDs so a leaked row is
// detectable even after its parents are gone.
func (s *Store) totalChildRows(t *testing.T, sw seededWorkspace) int {
	t.Helper()
	total := 0
	for _, tbl := range wsChildTables {
		total += s.countRows(t, `SELECT COUNT(*) FROM `+tbl+` WHERE workspace_id = ?`, sw.wsID)
	}
	// Item/parent-scoped tables without a workspace_id column.
	total += s.countRows(t, `SELECT COUNT(*) FROM comment_reactions WHERE comment_id = ?`, sw.commentID)
	total += s.countRows(t, `SELECT COUNT(*) FROM item_versions WHERE item_id IN (?, ?)`, sw.item1, sw.item2)
	total += s.countRows(t, `SELECT COUNT(*) FROM item_stars WHERE item_id IN (?, ?)`, sw.item1, sw.item2)
	total += s.countRows(t, `SELECT COUNT(*) FROM item_yjs_updates WHERE item_id IN (?, ?)`, sw.item1, sw.item2)
	total += s.countRows(t, `SELECT COUNT(*) FROM item_wiki_links WHERE source_item_id IN (?, ?)`, sw.item1, sw.item2)
	total += s.countRows(t, `SELECT COUNT(*) FROM versions WHERE document_id = ?`, sw.docID)
	total += s.countRows(t, `SELECT COUNT(*) FROM share_link_views WHERE share_link_id = ?`, sw.shareLinkID)
	return total
}

// TestPurgeWorkspaceData_RemovesAllChildDataNoOrphans is the acceptance
// test: a fully-populated soft-deleted workspace is purged with NO child
// row left behind on either dialect, while a second live workspace is
// completely untouched (no over-purge). mcp_audit_log is de-identified
// (workspace_id nulled) rather than deleted.
func TestPurgeWorkspaceData_RemovesAllChildDataNoOrphans(t *testing.T) {
	s := testStore(t)
	u, err := s.CreateUser(models.UserCreate{Email: "owner@test.com", Name: "Owner", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	doomed := seedFullWorkspace(t, s, u, "Doomed")
	bystander := seedFullWorkspace(t, s, u, "Bystander")

	// Sanity: both workspaces seeded a substantial number of child rows.
	if got := s.totalChildRows(t, doomed); got < 25 {
		t.Fatalf("doomed seed too small: %d child rows", got)
	}
	bystanderBefore := s.totalChildRows(t, bystander)

	// Soft-delete via the manual workspace-delete path, then purge.
	if err := s.DeleteWorkspace(doomed.slug); err != nil {
		t.Fatalf("soft-delete doomed: %v", err)
	}
	if err := s.PurgeWorkspaceData(doomed.wsID); err != nil {
		t.Fatalf("PurgeWorkspaceData: %v", err)
	}

	// --- No orphans: every child row of the doomed workspace is gone ---
	if got := s.totalChildRows(t, doomed); got != 0 {
		t.Errorf("doomed workspace left %d orphaned child rows after purge", got)
	}
	// Per-table detail to make a failure actionable.
	for _, tbl := range wsChildTables {
		if got := s.countRows(t, `SELECT COUNT(*) FROM `+tbl+` WHERE workspace_id = ?`, doomed.wsID); got != 0 {
			t.Errorf("table %s left %d rows for purged workspace", tbl, got)
		}
	}
	// Workspace row itself gone.
	if got := s.countRows(t, `SELECT COUNT(*) FROM workspaces WHERE id = ?`, doomed.wsID); got != 0 {
		t.Errorf("workspace row still present after purge")
	}

	// --- mcp_audit_log de-identified, not deleted ---
	if got := s.countRows(t, `SELECT COUNT(*) FROM mcp_audit_log WHERE workspace_id = ?`, doomed.wsID); got != 0 {
		t.Errorf("mcp_audit_log still references purged workspace: %d rows", got)
	}
	if got := s.countRows(t, `SELECT COUNT(*) FROM mcp_audit_log WHERE id = ?`, doomed.mcpAuditID); got != 1 {
		t.Errorf("mcp_audit_log row not preserved (de-identify posture): got %d, want 1", got)
	}

	// --- No over-purge: the live bystander workspace is fully intact ---
	if got := s.countRows(t, `SELECT COUNT(*) FROM workspaces WHERE id = ? AND deleted_at IS NULL`, bystander.wsID); got != 1 {
		t.Errorf("bystander workspace disturbed by purge")
	}
	if got := s.totalChildRows(t, bystander); got != bystanderBefore {
		t.Errorf("bystander child rows changed by purge: before=%d after=%d", bystanderBefore, got)
	}
}

// TestPurgeWorkspaceData_RefusesLiveWorkspace pins the critical safety
// guard: PurgeWorkspaceData must refuse to touch a workspace that is not
// soft-deleted, even if handed its ID directly — no child row is removed.
func TestPurgeWorkspaceData_RefusesLiveWorkspace(t *testing.T) {
	s := testStore(t)
	u, err := s.CreateUser(models.UserCreate{Email: "live@test.com", Name: "Live", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	live := seedFullWorkspace(t, s, u, "Alive")
	before := s.totalChildRows(t, live)

	if err := s.PurgeWorkspaceData(live.wsID); err == nil {
		t.Fatalf("expected PurgeWorkspaceData to refuse a live workspace")
	}

	if got := s.countRows(t, `SELECT COUNT(*) FROM workspaces WHERE id = ? AND deleted_at IS NULL`, live.wsID); got != 1 {
		t.Errorf("live workspace row disturbed by refused purge")
	}
	if got := s.totalChildRows(t, live); got != before {
		t.Errorf("live workspace child rows changed by refused purge: before=%d after=%d", before, got)
	}
}

// TestListPurgeableWorkspaces_Boundary pins the 30-day boundary: a
// workspace soft-deleted just past the cutoff is eligible; one deleted
// just inside the window and a live workspace are NOT.
func TestListPurgeableWorkspaces_Boundary(t *testing.T) {
	s := testStore(t)
	u, err := s.CreateUser(models.UserCreate{Email: "b@test.com", Name: "B", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	old := createWSWithDeletedAt(t, s, u, "Old", time.Now().Add(-31*24*time.Hour))
	recent := createWSWithDeletedAt(t, s, u, "Recent", time.Now().Add(-29*24*time.Hour))
	liveWS, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Live", OwnerID: u.ID})
	if err != nil {
		t.Fatalf("create live workspace: %v", err)
	}

	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	got, err := s.ListPurgeableWorkspaces(cutoff)
	if err != nil {
		t.Fatalf("ListPurgeableWorkspaces: %v", err)
	}

	ids := map[string]bool{}
	for _, c := range got {
		ids[c.ID] = true
	}
	if !ids[old] {
		t.Errorf("workspace soft-deleted 31d ago should be purgeable")
	}
	if ids[recent] {
		t.Errorf("workspace soft-deleted 29d ago must NOT be purgeable (boundary)")
	}
	if ids[liveWS.ID] {
		t.Errorf("live workspace must NEVER be purgeable")
	}
}

// createWSWithDeletedAt creates a workspace and stamps its deleted_at to
// a specific time so the boundary test can inject a deterministic age.
func createWSWithDeletedAt(t *testing.T, s *Store, u *models.User, name string, deletedAt time.Time) string {
	t.Helper()
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: name, OwnerID: u.ID})
	if err != nil {
		t.Fatalf("create workspace %s: %v", name, err)
	}
	s.mustExec(t, `UPDATE workspaces SET deleted_at = ? WHERE id = ?`,
		deletedAt.UTC().Format(time.RFC3339), ws.ID)
	return ws.ID
}
