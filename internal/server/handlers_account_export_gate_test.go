package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestExportAccount_RestrictedOwner_Forbidden pins BUG-1945: account-export
// (GET /api/v1/auth/export) dumps full collections/items for every
// workspace the caller owns, which bypasses BUG-1922's workspace-export
// gate via a different route. A workspace-role "owner" independently
// restricted via collection_access="specific" (member_collection_access has
// no role exclusion — BUG-1920) must be denied the account export outright,
// over either auth class, mirroring BUG-1922's own gate.
func TestExportAccount_RestrictedOwner_Forbidden(t *testing.T) {
	f := newRestrictedOwnerVisibilityFixture(t)

	cases := []struct {
		name string
		do   func() *httptest.ResponseRecorder
	}{
		{"bearer", func() *httptest.ResponseRecorder {
			return doRequestWithHeaders(f.srv, "GET", "/api/v1/auth/export", nil, f.bearerHeaders())
		}},
		{"session", func() *httptest.ResponseRecorder {
			return doRequestWithCookie(f.srv, "GET", "/api/v1/auth/export", nil, f.sessionToken)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := tc.do()
			if rr.Code != http.StatusForbidden {
				t.Fatalf("expected 403, got %d: %s", rr.Code, rr.Body.String())
			}
			// The gate must fire before the streaming response starts, so
			// the success-path headers/content must be entirely absent —
			// not just the status code.
			if cd := rr.Header().Get("Content-Disposition"); cd != "" {
				t.Fatalf("expected no Content-Disposition on 403 (streaming must not have started), got %q", cd)
			}
			if strings.Contains(rr.Body.String(), "\"workspaces\"") {
				t.Fatalf("403 body leaked export content: %s", rr.Body.String())
			}
		})
	}
}

// TestExportAccount_UnrestrictedOwner_OK confirms BUG-1945's gate doesn't
// regress the common case: an owner with unrestricted (mode "all")
// workspace access still gets the full account export exactly as before.
//
// Can't reuse createWSForTest + bare doRequest here (as
// TestExportWorkspace_UnrestrictedOwner_OK does for workspace-export):
// that pattern relies on RequireAuth's "no users exist yet" bypass, which
// leaves currentUser(r) nil — handleExportAccount's own explicit auth
// check would then 401 rather than exercise the success path.
func TestExportAccount_UnrestrictedOwner_OK(t *testing.T) {
	srv := testServer(t)

	owner, err := srv.store.CreateUser(models.UserCreate{
		Email: "unrestricted-owner@example.com", Name: "Unrestricted Owner", Username: "unrestricted-owner",
		Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "AccountExport", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("add member: %v", err)
	}
	sessTok, err := srv.store.CreateSession(owner.ID, "go-test", "192.0.2.1", "", 24*time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	rr := doRequestWithCookie(srv, "GET", "/api/v1/auth/export", nil, sessTok)
	if rr.Code != http.StatusOK {
		t.Fatalf("unrestricted owner export: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if cd := rr.Header().Get("Content-Disposition"); !strings.Contains(cd, "pad-export.json") {
		t.Fatalf("expected Content-Disposition attachment header on success, got %q", cd)
	}
	if !strings.Contains(rr.Body.String(), "\"workspaces\"") {
		t.Fatalf("expected export body to contain workspaces: %s", rr.Body.String())
	}
}

// TestExportAccount_RestrictedInOneOfTwoOwnedWorkspaces_Forbidden pins the
// multi-workspace deny semantics: account-export can span every workspace
// the caller owns in a single call. Per Dave's ruling, if the caller is
// restricted in ANY owned workspace, the WHOLE export is denied — not just
// the restricted workspace's content silently dropped — even though a
// second owned workspace here has fully unrestricted access.
func TestExportAccount_RestrictedInOneOfTwoOwnedWorkspaces_Forbidden(t *testing.T) {
	srv := testServer(t)

	owner, err := srv.store.CreateUser(models.UserCreate{
		Email: "multi-ws-owner@example.com", Name: "Multi WS Owner", Username: "multi-ws-owner",
		Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}

	// Workspace 1: fully unrestricted.
	wsOpen, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Open", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("create open workspace: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(wsOpen.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("add member (open): %v", err)
	}

	// Workspace 2: restricted to a specific collection.
	wsRestricted, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Restricted", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("create restricted workspace: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(wsRestricted.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("add member (restricted): %v", err)
	}
	schema := `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"}]}`
	visible, err := srv.store.CreateCollection(wsRestricted.ID, models.CollectionCreate{
		Name: "Visible", Slug: "visible", Prefix: "VIS", Schema: schema,
	})
	if err != nil {
		t.Fatalf("create visible collection: %v", err)
	}
	if err := srv.store.SetMemberCollectionAccess(wsRestricted.ID, owner.ID, "specific", []string{visible.ID}); err != nil {
		t.Fatalf("SetMemberCollectionAccess: %v", err)
	}

	sessTok, err := srv.store.CreateSession(owner.ID, "go-test", "192.0.2.1", "", 24*time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	rr := doRequestWithCookie(srv, "GET", "/api/v1/auth/export", nil, sessTok)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 (whole export denied), got %d: %s", rr.Code, rr.Body.String())
	}
	if cd := rr.Header().Get("Content-Disposition"); cd != "" {
		t.Fatalf("expected no Content-Disposition on 403, got %q", cd)
	}
}
