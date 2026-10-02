package server

import (
	"database/sql"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3335: restoring a document is scoped to the workspace in the URL. An
// editor of workspace A naming an archived document of workspace B must get
// a 404 and leave B's document exactly as it was: still archived, same status
// and updated_at. The handler used to restore by ID, commit, and only then
// compare workspaces and re-delete as a separate write.
func TestBUG3335_RestoreDocumentIsScopedToTheURLWorkspace(t *testing.T) {
	srv := testServer(t)
	ownerCookie := bootstrapFirstUser(t, srv, "owner@test.com", "Owner")
	mkWS := func(name string) *models.Workspace {
		rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces", map[string]string{"name": name}, ownerCookie)
		if rr.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", name, rr.Code, rr.Body.String())
		}
		var ws models.Workspace
		parseJSON(t, rr, &ws)
		return &ws
	}
	wsA, wsB := mkWS("Alpha"), mkWS("Bravo")

	editor, err := srv.store.CreateUser(models.UserCreate{Email: "editor@test.com", Name: "Editor", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.AddWorkspaceMember(wsA.ID, editor.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	editorCookie, err := srv.store.CreateSession(editor.ID, "web-test", "192.0.2.1", "", webSessionTTL)
	if err != nil {
		t.Fatal(err)
	}

	doc, err := srv.store.CreateDocument(wsB.ID, models.DocumentCreate{Title: "B secret", Content: "b body", Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.DeleteDocument(doc.ID); err != nil {
		t.Fatal(err)
	}
	read := func() (deleted bool, status, updated string) {
		t.Helper()
		var deletedAt sql.NullString
		if err := srv.store.DB().QueryRow(`SELECT deleted_at, status, updated_at FROM documents WHERE id = ?`, doc.ID).
			Scan(&deletedAt, &status, &updated); err != nil {
			t.Fatal(err)
		}
		return deletedAt.Valid, status, updated
	}
	// updated_at has one-second resolution; backdate it so a write in this
	// same second (restore, then the compensating re-delete) is visible.
	if _, err := srv.store.DB().Exec(`UPDATE documents SET updated_at = '2020-01-01T00:00:00Z' WHERE id = ?`, doc.ID); err != nil {
		t.Fatal(err)
	}
	wasDeleted, wasStatus, wasUpdated := read()
	if !wasDeleted {
		t.Fatal("precondition: the document should be archived")
	}

	rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+wsA.Slug+"/documents/"+doc.ID+"/restore", nil, editorCookie)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-workspace restore: status %d %s, want 404", rr.Code, rr.Body.String())
	}
	if deleted, status, updated := read(); !deleted || status != wasStatus || updated != wasUpdated {
		t.Fatalf("B's document changed: deleted=%v status=%q updated=%q, want deleted=true status=%q updated=%q",
			deleted, status, updated, wasStatus, wasUpdated)
	}

	// Control: restoring through the document's own workspace still works.
	rr = doRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+wsB.Slug+"/documents/"+doc.ID+"/restore", nil, ownerCookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("own-workspace restore: status %d %s, want 200", rr.Code, rr.Body.String())
	}
	if deleted, _, _ := read(); deleted {
		t.Fatal("own-workspace restore left the document archived")
	}
}
