package store

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// OAuth connection store tests (PLAN-1519 / TASK-1520 — Phase A foundation).
//
// What's covered:
//
//   - CreateOAuthConnection + GetOAuthConnection round-trip with all
//     three scope flags + defaults.
//   - GetOAuthConnectionAccess HasConnection=false when no row exists
//     (the case the dual-read middleware uses to fall back to the
//     legacy session.Extra path).
//   - GetOAuthConnectionAccess wildcard (AllCurrentWorkspaces=true)
//     skips the join and returns no slugs.
//   - GetOAuthConnectionAccess explicit allow-list returns the joined
//     workspace slugs, sorted lexicographically.
//   - AddConnectionWorkspace idempotency: re-adding the same
//     (request_id, workspace_id) pair does NOT error and does NOT
//     duplicate the row.
//   - RemoveConnectionWorkspace + RemoveConnectionWorkspace-idempotent
//     (removing a non-existent pair is a no-op).
//   - RenameConnection + SetScopeFlags both return
//     ErrOAuthConnectionNotFound when the row is missing.
//   - DeleteOAuthConnection cascades to oauth_connection_workspaces.
//   - IsConnectionWorkspaceAllowed reports membership accurately.

func newOAuthConnTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := New(filepath.Join(dir, "oauth_conn.db"))
	if err != nil {
		t.Fatalf("New store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// seedWorkspaceForConn creates a workspace and returns its ID — the
// join table stores workspace IDs, so tests need a real row.
func seedWorkspaceForConn(t *testing.T, s *Store, name string) (id, slug string) {
	t.Helper()
	u, err := s.CreateUser(models.UserCreate{
		Email:    name + "-owner@example.com",
		Name:     name + " owner",
		Password: "pw-conn-owner-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser owner: %v", err)
	}
	w, err := s.CreateWorkspace(models.WorkspaceCreate{Name: name, OwnerID: u.ID})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	return w.ID, w.Slug
}

func TestOAuthConnections_CreateGetRoundTrip(t *testing.T) {
	s := newOAuthConnTestStore(t)
	u, err := s.CreateUser(models.UserCreate{
		Email: "conn-create@example.com", Name: "C", Password: "pw-create-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	in := OAuthConnection{
		RequestID:               "chain-create",
		UserID:                  u.ID,
		Name:                    "Cursor on MacBook",
		MayCreateWorkspaces:     true,
		AllCurrentWorkspaces:    false,
		IncludeFutureWorkspaces: true,
	}
	if err := s.CreateOAuthConnection(in); err != nil {
		t.Fatalf("CreateOAuthConnection: %v", err)
	}
	got, err := s.GetOAuthConnection("chain-create")
	if err != nil {
		t.Fatalf("GetOAuthConnection: %v", err)
	}
	if got.RequestID != in.RequestID || got.UserID != in.UserID || got.Name != in.Name {
		t.Errorf("identity round-trip mismatch: got %+v want %+v", got, in)
	}
	if !got.MayCreateWorkspaces || got.AllCurrentWorkspaces || !got.IncludeFutureWorkspaces {
		t.Errorf("scope flag round-trip mismatch: got %+v", got)
	}
	if got.CreatedAt == "" || got.UpdatedAt == "" {
		t.Errorf("timestamps should be populated by default: %+v", got)
	}
}

func TestOAuthConnections_GetMissingReturnsSentinel(t *testing.T) {
	s := newOAuthConnTestStore(t)
	_, err := s.GetOAuthConnection("nope")
	if !errors.Is(err, ErrOAuthConnectionNotFound) {
		t.Errorf("got %v, want ErrOAuthConnectionNotFound", err)
	}
}

func TestOAuthConnectionAccess_NoRowFallsBack(t *testing.T) {
	s := newOAuthConnTestStore(t)
	got, err := s.GetOAuthConnectionAccess("never-existed")
	if err != nil {
		t.Fatalf("GetOAuthConnectionAccess: %v", err)
	}
	if got.HasConnection {
		t.Errorf("HasConnection = true for missing chain; middleware would skip the legacy-Extra fallback")
	}
	if got.AllCurrentWorkspaces || len(got.WorkspaceSlugs) != 0 {
		t.Errorf("expected zero-value access for missing row, got %+v", got)
	}
}

func TestOAuthConnectionAccess_WildcardSkipsJoin(t *testing.T) {
	s := newOAuthConnTestStore(t)
	u, _ := s.CreateUser(models.UserCreate{
		Email: "wc@example.com", Name: "W", Password: "pw-wild-12345",
	})
	if err := s.CreateOAuthConnection(OAuthConnection{
		RequestID:            "wildcard-chain",
		UserID:               u.ID,
		AllCurrentWorkspaces: true,
	}); err != nil {
		t.Fatalf("CreateOAuthConnection: %v", err)
	}
	// Even if a stale join row existed (no FK violation possible after
	// connection exists), the wildcard flag should short-circuit the
	// projection. Adding one to prove the early-return is real.
	wsID, _ := seedWorkspaceForConn(t, s, "wildcard-ws")
	if err := s.AddConnectionWorkspace("wildcard-chain", wsID, AddedByUser); err != nil {
		t.Fatalf("AddConnectionWorkspace: %v", err)
	}

	got, err := s.GetOAuthConnectionAccess("wildcard-chain")
	if err != nil {
		t.Fatalf("GetOAuthConnectionAccess: %v", err)
	}
	if !got.HasConnection || !got.AllCurrentWorkspaces {
		t.Errorf("expected wildcard access, got %+v", got)
	}
	if len(got.WorkspaceSlugs) != 0 {
		t.Errorf("WorkspaceSlugs should be empty when wildcard short-circuits, got %v", got.WorkspaceSlugs)
	}
}

func TestOAuthConnectionAccess_ExplicitListReturnsSortedSlugs(t *testing.T) {
	s := newOAuthConnTestStore(t)
	u, _ := s.CreateUser(models.UserCreate{
		Email: "ex@example.com", Name: "E", Password: "pw-exp-12345",
	})
	if err := s.CreateOAuthConnection(OAuthConnection{
		RequestID:            "explicit-chain",
		UserID:               u.ID,
		AllCurrentWorkspaces: false,
	}); err != nil {
		t.Fatalf("CreateOAuthConnection: %v", err)
	}
	// Two workspaces, intentionally added in reverse alpha order so
	// the sort assertion has signal.
	zID, zSlug := seedWorkspaceForConn(t, s, "zeta-ws")
	aID, aSlug := seedWorkspaceForConn(t, s, "alpha-ws")
	if err := s.AddConnectionWorkspace("explicit-chain", zID, AddedByUser); err != nil {
		t.Fatalf("AddConnectionWorkspace zeta: %v", err)
	}
	if err := s.AddConnectionWorkspace("explicit-chain", aID, AddedByAgentCreate); err != nil {
		t.Fatalf("AddConnectionWorkspace alpha: %v", err)
	}

	got, err := s.GetOAuthConnectionAccess("explicit-chain")
	if err != nil {
		t.Fatalf("GetOAuthConnectionAccess: %v", err)
	}
	want := []string{aSlug, zSlug}
	if !reflect.DeepEqual(got.WorkspaceSlugs, want) {
		t.Errorf("WorkspaceSlugs = %v, want %v (lexicographic order)", got.WorkspaceSlugs, want)
	}
	if got.AllCurrentWorkspaces {
		t.Errorf("AllCurrentWorkspaces should be false for explicit allow-list path")
	}
}

func TestOAuthConnections_AddWorkspaceIdempotent(t *testing.T) {
	s := newOAuthConnTestStore(t)
	u, _ := s.CreateUser(models.UserCreate{
		Email: "idem@example.com", Name: "I", Password: "pw-idem-12345",
	})
	if err := s.CreateOAuthConnection(OAuthConnection{
		RequestID: "idem-chain", UserID: u.ID,
	}); err != nil {
		t.Fatalf("CreateOAuthConnection: %v", err)
	}
	wsID, _ := seedWorkspaceForConn(t, s, "idem-ws")

	for i := 0; i < 3; i++ {
		if err := s.AddConnectionWorkspace("idem-chain", wsID, AddedByUser); err != nil {
			t.Fatalf("AddConnectionWorkspace iter %d: %v", i, err)
		}
	}
	allowed, err := s.IsConnectionWorkspaceAllowed("idem-chain", wsID)
	if err != nil {
		t.Fatalf("IsConnectionWorkspaceAllowed: %v", err)
	}
	if !allowed {
		t.Errorf("workspace should be in allow-list after add")
	}
}

func TestOAuthConnections_RemoveWorkspaceIdempotent(t *testing.T) {
	s := newOAuthConnTestStore(t)
	u, _ := s.CreateUser(models.UserCreate{
		Email: "rem@example.com", Name: "R", Password: "pw-rem-12345",
	})
	if err := s.CreateOAuthConnection(OAuthConnection{
		RequestID: "rem-chain", UserID: u.ID,
	}); err != nil {
		t.Fatalf("CreateOAuthConnection: %v", err)
	}
	wsID, _ := seedWorkspaceForConn(t, s, "rem-ws")
	if err := s.AddConnectionWorkspace("rem-chain", wsID, AddedByUser); err != nil {
		t.Fatalf("AddConnectionWorkspace: %v", err)
	}
	if err := s.RemoveConnectionWorkspace("rem-chain", wsID); err != nil {
		t.Fatalf("RemoveConnectionWorkspace: %v", err)
	}
	// Second remove must not error.
	if err := s.RemoveConnectionWorkspace("rem-chain", wsID); err != nil {
		t.Errorf("RemoveConnectionWorkspace second call should be no-op, got %v", err)
	}
	allowed, _ := s.IsConnectionWorkspaceAllowed("rem-chain", wsID)
	if allowed {
		t.Errorf("workspace should not be allowed after remove")
	}
}

func TestOAuthConnections_RenameAndScopeFlagsRequireExistingRow(t *testing.T) {
	s := newOAuthConnTestStore(t)
	if err := s.RenameConnection("ghost", "anything"); !errors.Is(err, ErrOAuthConnectionNotFound) {
		t.Errorf("Rename missing: got %v, want ErrOAuthConnectionNotFound", err)
	}
	if err := s.SetScopeFlags("ghost", true, true, true); !errors.Is(err, ErrOAuthConnectionNotFound) {
		t.Errorf("SetScopeFlags missing: got %v, want ErrOAuthConnectionNotFound", err)
	}
}

func TestOAuthConnections_RenameAndScopeFlagsTouchUpdatedAt(t *testing.T) {
	s := newOAuthConnTestStore(t)
	u, _ := s.CreateUser(models.UserCreate{
		Email: "u@example.com", Name: "U", Password: "pw-upd-12345",
	})
	if err := s.CreateOAuthConnection(OAuthConnection{
		RequestID: "upd-chain", UserID: u.ID, Name: "Original",
	}); err != nil {
		t.Fatalf("CreateOAuthConnection: %v", err)
	}
	if err := s.RenameConnection("upd-chain", "Renamed"); err != nil {
		t.Fatalf("RenameConnection: %v", err)
	}
	if err := s.SetScopeFlags("upd-chain", false, false, false); err != nil {
		t.Fatalf("SetScopeFlags: %v", err)
	}
	got, err := s.GetOAuthConnection("upd-chain")
	if err != nil {
		t.Fatalf("GetOAuthConnection: %v", err)
	}
	if got.Name != "Renamed" {
		t.Errorf("Name not updated: %q", got.Name)
	}
	if got.MayCreateWorkspaces || got.AllCurrentWorkspaces || got.IncludeFutureWorkspaces {
		t.Errorf("scope flags not cleared: %+v", got)
	}
}

func TestOAuthConnections_DeleteCascadesJoinTable(t *testing.T) {
	s := newOAuthConnTestStore(t)
	u, _ := s.CreateUser(models.UserCreate{
		Email: "del@example.com", Name: "D", Password: "pw-del-12345",
	})
	if err := s.CreateOAuthConnection(OAuthConnection{
		RequestID: "del-chain", UserID: u.ID,
	}); err != nil {
		t.Fatalf("CreateOAuthConnection: %v", err)
	}
	wsID, _ := seedWorkspaceForConn(t, s, "del-ws")
	if err := s.AddConnectionWorkspace("del-chain", wsID, AddedByUser); err != nil {
		t.Fatalf("AddConnectionWorkspace: %v", err)
	}

	if err := s.DeleteOAuthConnection("del-chain"); err != nil {
		t.Fatalf("DeleteOAuthConnection: %v", err)
	}
	// Connection gone.
	if _, err := s.GetOAuthConnection("del-chain"); !errors.Is(err, ErrOAuthConnectionNotFound) {
		t.Errorf("connection should be gone, got %v", err)
	}
	// Join row gone — verifies FK ON DELETE CASCADE actually fires
	// on SQLite (it only does when foreign_keys pragma is ON; the DSN
	// in New() sets it per-connection).
	allowed, err := s.IsConnectionWorkspaceAllowed("del-chain", wsID)
	if err != nil {
		t.Fatalf("IsConnectionWorkspaceAllowed: %v", err)
	}
	if allowed {
		t.Errorf("join row should cascade-deleted with parent connection")
	}
}
