package store

import (
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestDeleteAccountAtomic_FullyPopulatedUser is the TASK-1959 acceptance
// test: deleting a user who has rows in every table that references
// users(id) — activities (audit rows from multiple IPs), sessions, API
// tokens, memberships, invitations, comments/reactions, grants, share
// links + views, MCP audit, OAuth connections, item versions/links, stars,
// and an owned workspace — succeeds atomically with NO work-arounds. FK
// enforcement is on (testStore enables PRAGMA foreign_keys), so any
// unhandled reference would surface here as a FK violation on the final
// DELETE FROM users.
//
// Postures asserted:
//   - audit/history rows survive, de-identified (user reference nulled);
//   - owned/transient rows are removed;
//   - the workspace is soft-deleted (recoverable), the second user and
//     their data are untouched.
func TestDeleteAccountAtomic_FullyPopulatedUser(t *testing.T) {
	s := testStore(t)

	// The account under deletion, plus a bystander whose data must survive.
	u, err := s.CreateUser(models.UserCreate{Email: "delete-me@test.com", Name: "Delete Me", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	other, err := s.CreateUser(models.UserCreate{Email: "keep-me@test.com", Name: "Keep Me", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatalf("create other user: %v", err)
	}

	ws := createTestWorkspace(t, s, "Owned")
	coll := createTestCollection(t, s, ws.ID, "Tasks")
	item1 := createTestItem(t, s, ws.ID, coll.ID, "Item One", "")
	item2 := createTestItem(t, s, ws.ID, coll.ID, "Item Two", "")

	// Attribute item1 to u so the created/modified FKs are exercised.
	if _, err := s.db.Exec(s.q(`UPDATE items SET created_by_user_id = ?, last_modified_by_user_id = ? WHERE id = ?`), u.ID, u.ID, item1.ID); err != nil {
		t.Fatalf("attribute item: %v", err)
	}

	// Memberships: u owns, other edits.
	if err := s.AddWorkspaceMember(ws.ID, u.ID, "owner"); err != nil {
		t.Fatalf("add owner member: %v", err)
	}
	if err := s.AddWorkspaceMember(ws.ID, other.ID, "editor"); err != nil {
		t.Fatalf("add editor member: %v", err)
	}

	// Sessions + API tokens.
	if _, err := s.CreateSession(u.ID, "cli", "127.0.0.1", "test-agent", time.Hour); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := s.CreateAPIToken(u.ID, models.APITokenCreate{Name: "cli"}, 30, 365); err != nil {
		t.Fatalf("create api token: %v", err)
	}

	// Activities: audit rows from two different IPs, including a
	// session_ip_changed row like the auth middleware writes. Capture their
	// IDs so we can assert the rows survive (de-identified) post-delete.
	act1, err := s.CreateActivity(models.Activity{WorkspaceID: ws.ID, Action: "updated", Actor: "user", Source: "web", UserID: u.ID, IPAddress: "198.51.100.7"})
	if err != nil {
		t.Fatalf("create activity 1: %v", err)
	}
	act2, err := s.CreateActivity(models.Activity{Action: models.ActionSessionIPChanged, Actor: "user", Source: "web", UserID: u.ID, IPAddress: "203.0.113.9"})
	if err != nil {
		t.Fatalf("create activity 2: %v", err)
	}

	// Comment + reaction authored by u.
	comment, err := s.CreateComment(ws.ID, item1.ID, u.ID, models.CommentCreate{Body: "authored", Author: "Delete Me", CreatedBy: "user", Source: "web"})
	if err != nil {
		t.Fatalf("create comment: %v", err)
	}
	if _, err := s.AddReaction(comment.ID, u.ID, "user", "👍"); err != nil {
		t.Fatalf("add reaction: %v", err)
	}

	// Invitation sent by u.
	if _, err := s.CreateInvitation(ws.ID, "invitee@test.com", "editor", u.ID); err != nil {
		t.Fatalf("create invitation: %v", err)
	}

	// Transient auth tokens.
	if _, err := s.CreatePasswordReset(u.ID); err != nil {
		t.Fatalf("create password reset: %v", err)
	}
	if _, err := s.CreateEmailVerification(u.ID); err != nil {
		t.Fatalf("create email verification: %v", err)
	}

	// Grants: one issued BY u (granted_by → deleted directly), one issued TO
	// u (user_id → cascades on user delete).
	if _, err := s.CreateCollectionGrant(ws.ID, coll.ID, other.ID, "view", u.ID); err != nil {
		t.Fatalf("create collection grant (granted_by u): %v", err)
	}
	if _, err := s.CreateItemGrant(ws.ID, item1.ID, u.ID, "view", other.ID); err != nil {
		t.Fatalf("create item grant (user_id u): %v", err)
	}

	// Share links: one created by u (deleted, cascading its views); one
	// created by other but VIEWED by u (survives, view de-identified).
	if _, err := s.CreateShareLink(ws.ID, "item", item1.ID, "view", u.ID, nil); err != nil {
		t.Fatalf("create share link (u): %v", err)
	}
	otherLink, err := s.CreateShareLink(ws.ID, "item", item2.ID, "view", other.ID, nil)
	if err != nil {
		t.Fatalf("create share link (other): %v", err)
	}
	if _, err := s.RecordShareLinkView(otherLink.ID, "fp-1", u.ID, nil); err != nil {
		t.Fatalf("record share link view: %v", err)
	}

	// Item star (cascade), MCP audit row, OAuth connection.
	if err := s.StarItem(u.ID, item1.ID); err != nil {
		t.Fatalf("star item: %v", err)
	}
	if err := s.InsertMCPAuditEntry(models.MCPAuditEntryInput{UserID: u.ID, TokenKind: models.TokenKindPAT, TokenRef: "tok-1", ToolName: "pad_item", ResultStatus: models.MCPAuditResultOK, RequestID: "req-1"}); err != nil {
		t.Fatalf("insert mcp audit: %v", err)
	}
	if err := s.CreateOAuthConnection(OAuthConnection{RequestID: "conn-1", UserID: u.ID, Name: "Claude"}); err != nil {
		t.Fatalf("create oauth connection: %v", err)
	}

	// item_links / item_versions authored by u (no dedicated store creators).
	if _, err := s.db.Exec(s.q(`INSERT INTO item_links (id, workspace_id, source_id, target_id, link_type, created_by, user_id, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
		newID(), ws.ID, item1.ID, item2.ID, "related", "user", u.ID, now()); err != nil {
		t.Fatalf("insert item link: %v", err)
	}
	if _, err := s.db.Exec(s.q(`INSERT INTO item_versions (id, item_id, content, change_summary, created_by, source, is_diff, user_id, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		newID(), item1.ID, "v1", "", "user", "web", s.dialect.BoolToInt(false), u.ID, now()); err != nil {
		t.Fatalf("insert item version: %v", err)
	}

	count := func(query string, args ...interface{}) int {
		t.Helper()
		var n int
		if err := s.db.QueryRow(s.q(query), args...).Scan(&n); err != nil {
			t.Fatalf("count %q: %v", query, err)
		}
		return n
	}

	activitiesBefore := count(`SELECT COUNT(*) FROM activities`)

	// The whole point: this must NOT fail on any FK.
	if err := s.DeleteAccountAtomic(u.ID, []string{ws.Slug}); err != nil {
		t.Fatalf("DeleteAccountAtomic on fully-populated user: %v", err)
	}

	// --- User gone, bystander intact ---
	if got, err := s.GetUser(u.ID); err != nil || got != nil {
		t.Fatalf("expected deleted user to be gone (got %v, err %v)", got, err)
	}
	if got, err := s.GetUser(other.ID); err != nil || got == nil {
		t.Fatalf("bystander user must survive (got %v, err %v)", got, err)
	}

	// --- Owned workspace soft-deleted (row + contents recoverable) ---
	if count(`SELECT COUNT(*) FROM workspaces WHERE id = ? AND deleted_at IS NOT NULL`, ws.ID) != 1 {
		t.Error("owned workspace should be soft-deleted, not hard-deleted")
	}

	// --- Audit/history rows survive, de-identified ---
	if got := count(`SELECT COUNT(*) FROM activities WHERE user_id = ?`, u.ID); got != 0 {
		t.Errorf("activities.user_id must be nulled, got %d rows still referencing user", got)
	}
	if got := count(`SELECT COUNT(*) FROM activities WHERE id IN (?, ?)`, act1, act2); got != 2 {
		t.Errorf("audit rows must survive de-identification, found %d/2", got)
	}
	if got := count(`SELECT COUNT(*) FROM activities`); got != activitiesBefore {
		t.Errorf("no activity rows should be deleted (before=%d after=%d)", activitiesBefore, got)
	}
	if count(`SELECT COUNT(*) FROM items WHERE id = ? AND created_by_user_id IS NULL AND last_modified_by_user_id IS NULL`, item1.ID) != 1 {
		t.Error("item author/modifier references must be nulled, item preserved")
	}
	if count(`SELECT COUNT(*) FROM comments WHERE id = ? AND user_id IS NULL`, comment.ID) != 1 {
		t.Error("comment must survive with user_id nulled")
	}
	if got := count(`SELECT COUNT(*) FROM comment_reactions WHERE user_id = ?`, u.ID); got != 0 {
		t.Errorf("comment_reactions.user_id must be nulled, got %d", got)
	}
	if got := count(`SELECT COUNT(*) FROM item_links WHERE user_id = ?`, u.ID); got != 0 {
		t.Errorf("item_links.user_id must be nulled, got %d", got)
	}
	if got := count(`SELECT COUNT(*) FROM item_versions WHERE user_id = ?`, u.ID); got != 0 {
		t.Errorf("item_versions.user_id must be nulled, got %d", got)
	}
	// other's share link survives; the view by u is de-identified, not deleted.
	if count(`SELECT COUNT(*) FROM share_links WHERE id = ?`, otherLink.ID) != 1 {
		t.Error("bystander's share link must survive")
	}
	if got := count(`SELECT COUNT(*) FROM share_link_views WHERE viewer_user_id = ?`, u.ID); got != 0 {
		t.Errorf("share_link_views.viewer_user_id must be nulled, got %d", got)
	}
	if count(`SELECT COUNT(*) FROM share_link_views WHERE share_link_id = ? AND viewer_user_id IS NULL`, otherLink.ID) != 1 {
		t.Error("the de-identified view row must survive on the bystander's link")
	}

	// --- Owned / transient / audit rows removed ---
	for _, c := range []struct {
		what, query string
	}{
		{"sessions", `SELECT COUNT(*) FROM sessions WHERE user_id = ?`},
		{"api_tokens", `SELECT COUNT(*) FROM api_tokens WHERE user_id = ?`},
		{"workspace_members", `SELECT COUNT(*) FROM workspace_members WHERE user_id = ?`},
		{"workspace_invitations", `SELECT COUNT(*) FROM workspace_invitations WHERE invited_by = ?`},
		{"password_reset_tokens", `SELECT COUNT(*) FROM password_reset_tokens WHERE user_id = ?`},
		{"email_verification_tokens", `SELECT COUNT(*) FROM email_verification_tokens WHERE user_id = ?`},
		{"collection_grants (granted_by)", `SELECT COUNT(*) FROM collection_grants WHERE granted_by = ?`},
		{"item_grants (user_id, cascade)", `SELECT COUNT(*) FROM item_grants WHERE user_id = ?`},
		{"share_links (created_by)", `SELECT COUNT(*) FROM share_links WHERE created_by = ?`},
		{"mcp_audit_log", `SELECT COUNT(*) FROM mcp_audit_log WHERE user_id = ?`},
		{"oauth_connections", `SELECT COUNT(*) FROM oauth_connections WHERE user_id = ?`},
		{"item_stars (cascade)", `SELECT COUNT(*) FROM item_stars WHERE user_id = ?`},
	} {
		if got := count(c.query, u.ID); got != 0 {
			t.Errorf("%s: expected 0 rows for deleted user, got %d", c.what, got)
		}
	}

	// Bystander's membership must be untouched.
	if count(`SELECT COUNT(*) FROM workspace_members WHERE user_id = ?`, other.ID) != 1 {
		t.Error("bystander membership must survive")
	}
}
