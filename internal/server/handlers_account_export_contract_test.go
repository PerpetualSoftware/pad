package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestExportAccount_HappyPath_ArtifactContract pins the account-export
// artifact shape a consumer relies on — the web Danger Zone "Export my data"
// download (web/src/lib/utils/artifacts.ts::exportAndDownloadAccountData) and
// any offline GDPR-portability parse. This fills the TASK-508 gap: the export
// endpoint had a restricted-owner gate test but no positive assertion on the
// bytes it produces.
//
// Distinct from TestExportAccount_UnrestrictedOwner_OK in
// handlers_account_export_gate_test.go, which is a substring smoke proving
// BUG-1945's gate does not 403 the unrestricted case. This test decodes the
// body and asserts the STRUCTURE: the exact attachment header (the filename a
// browser saves under), the JSON content type, the top-level {user,
// workspaces} envelope, the user projection's fields, and that an owned
// workspace carries its collections + items inline.
func TestExportAccount_HappyPath_ArtifactContract(t *testing.T) {
	srv := testServer(t)

	owner, err := srv.store.CreateUser(models.UserCreate{
		Email: "export-contract@example.com", Name: "Export Contract", Username: "export-contract",
		Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Exported", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("add member: %v", err)
	}
	coll, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Tasks", Slug: "tasks", Prefix: "TASK", Schema: `{"fields":[]}`,
	})
	if err != nil {
		t.Fatalf("create collection: %v", err)
	}
	if _, err := srv.store.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "Exported Item", Fields: `{}`}); err != nil {
		t.Fatalf("create item: %v", err)
	}

	sessTok, err := srv.store.CreateSession(owner.ID, "go-test", "192.0.2.1", "", 24*time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	rr := doRequestWithCookie(srv, "GET", "/api/v1/auth/export", nil, sessTok)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// The download headers a browser keys the "Save as pad-export.json"
	// prompt off. Exact match, not substring: the filename is the contract.
	if cd := rr.Header().Get("Content-Disposition"); cd != `attachment; filename="pad-export.json"` {
		t.Errorf("Content-Disposition = %q, want `attachment; filename=\"pad-export.json\"`", cd)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	// Decode the streamed body and assert the portability envelope.
	var payload struct {
		User struct {
			ID       string `json:"id"`
			Email    string `json:"email"`
			Username string `json:"username"`
			Name     string `json:"name"`
		} `json:"user"`
		Workspaces []struct {
			ID          string              `json:"id"`
			Slug        string              `json:"slug"`
			Role        string              `json:"role"`
			Collections []models.Collection `json:"collections"`
			Items       []models.Item       `json:"items"`
		} `json:"workspaces"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("export body is not valid JSON matching the documented shape: %v\nbody: %s", err, rr.Body.String())
	}

	if payload.User.ID != owner.ID {
		t.Errorf("user.id = %q, want %q", payload.User.ID, owner.ID)
	}
	if payload.User.Email != "export-contract@example.com" {
		t.Errorf("user.email = %q, want export-contract@example.com", payload.User.Email)
	}
	if payload.User.Username == "" || payload.User.Name == "" {
		t.Errorf("user projection missing username/name: %+v", payload.User)
	}

	if len(payload.Workspaces) != 1 {
		t.Fatalf("expected exactly 1 exported workspace, got %d", len(payload.Workspaces))
	}
	wsOut := payload.Workspaces[0]
	if wsOut.ID != ws.ID || wsOut.Slug != ws.Slug {
		t.Errorf("workspace id/slug mismatch: got %s/%s, want %s/%s", wsOut.ID, wsOut.Slug, ws.ID, ws.Slug)
	}
	if wsOut.Role != "owner" {
		t.Errorf("owned workspace role = %q, want owner", wsOut.Role)
	}
	// Owned workspaces carry their collections + items inline (the whole point
	// of the export). The seeded collection + item must round-trip.
	if len(wsOut.Collections) == 0 {
		t.Error("owned workspace export missing collections")
	}
	foundItem := false
	for _, it := range wsOut.Items {
		if it.Title == "Exported Item" {
			foundItem = true
			break
		}
	}
	if !foundItem {
		t.Errorf("owned workspace export missing the seeded item; items=%+v", wsOut.Items)
	}
}
