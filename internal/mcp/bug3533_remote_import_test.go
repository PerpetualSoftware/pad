package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/artifact"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3533: pad_item.import over the REMOTE transport went through the
// catalog action, which is written for stdio: it spills the artifact to a
// temp file and dispatched an input with `artifact` dropped and `file`
// added. The HTTP dispatcher reads `artifact` from that input, so every
// remote import answered "artifact is required". The dispatcher's own
// tests passed `artifact` to it directly, which is why nothing caught it.
// This drives the CATALOG ACTION through the real dispatcher and server
// (team CONVE-19: the binding, not the component).
func TestBUG3533_RemoteImportThroughTheCatalogAction(t *testing.T) {
	srv, st := newPadServer(t)
	wsRec := doJSONReq(t, srv, http.MethodPost, "/api/v1/workspaces",
		map[string]any{"name": "Import WS", "template": "startup"})
	if wsRec.Code != http.StatusCreated {
		t.Fatalf("create workspace: %d %s", wsRec.Code, wsRec.Body.String())
	}
	var ws models.Workspace
	if err := json.Unmarshal(wsRec.Body.Bytes(), &ws); err != nil {
		t.Fatal(err)
	}
	user, err := st.CreateUser(models.UserCreate{Email: "importer@example.com", Name: "Importer", Password: "irrelevant"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddWorkspaceMember(ws.ID, user.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	env := ActionEnv{
		Doc:        liveCmdhelpDoc(t),
		Workspace:  NewWorkspaceState(ws.Slug),
		Dispatcher: &HTTPHandlerDispatcher{Handler: srv, UserResolver: fixedUserResolver(user)},
	}

	data, err := artifact.Encode(artifact.Artifact{
		Kind:          artifact.KindConvention,
		FormatVersion: artifact.FormatVersion,
		Title:         "Imported Remotely",
		Fields:        map[string]any{"status": "active", "trigger": "on-commit", "scope": "all", "priority": "must"},
		Body:          "Imported through the catalog action.\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := actionItemImport(context.Background(), map[string]any{
		"workspace": ws.Slug, "artifact": string(data),
	}, env)
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || res.IsError {
		t.Fatalf("remote import through the catalog action failed: %s", resultTextOf(res))
	}
	payload, _ := res.StructuredContent.(map[string]any)
	ref, _ := payload["ref"].(string)
	if ref == "" {
		t.Fatalf("import result has no ref: %#v", res.StructuredContent)
	}
	item, err := st.ResolveItem(ws.ID, ref)
	if err != nil || item == nil || item.Title != "Imported Remotely" {
		t.Fatalf("imported item %s: %+v err=%v", ref, item, err)
	}
}

func resultTextOf(res *CallToolResult) string {
	if res == nil {
		return "<nil>"
	}
	return textOf(res)
}
