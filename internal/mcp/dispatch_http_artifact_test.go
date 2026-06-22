package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/artifact"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestRoute_ItemExport locks the `item export` route shape: a plain
// path-param GET to the workspace-scoped /export endpoint, modeled on
// `item show`. No body. The catalog's stdout-forcing `output` input is
// irrelevant over HTTP (the endpoint returns the artifact in the
// response body) and must NOT appear in the path.
func TestRoute_ItemExport(t *testing.T) {
	m, p, body, err := routeTable["item export"](map[string]any{
		"workspace": "docapp",
		"ref":       "PLAYB-3",
		// An agent-supplied `output` is meaningless over HTTP — the
		// route ignores it entirely.
		"output": "-",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if m != http.MethodGet {
		t.Errorf("method = %q, want GET", m)
	}
	if p != "/api/v1/workspaces/docapp/items/PLAYB-3/export" {
		t.Errorf("path = %q", p)
	}
	if body != nil {
		t.Errorf("expected nil body for GET export; got %s", body)
	}
}

// TestRoute_ItemExport_MissingPlaceholderErrors confirms the routeSpec
// surfaces a clear error when `ref` is absent (same placeholder
// machinery `item show` relies on).
func TestRoute_ItemExport_MissingRefErrors(t *testing.T) {
	_, _, _, err := routeTable["item export"](map[string]any{"workspace": "docapp"})
	if err == nil {
		t.Fatalf("expected error for missing ref placeholder")
	}
	if !strings.Contains(err.Error(), "ref") {
		t.Errorf("error should name the missing ref placeholder; got %v", err)
	}
}

// TestRouteTable_ImportIsNotARoute documents that `item import` is
// intentionally a special-case dispatch method (dispatchItemImport),
// NOT a routeTable entry: it needs a non-JSON (text/markdown) content
// type and a verbatim body that the RouteMapper/buildHTTPRequest path
// can't express. `item export`, being a clean GET, IS a routeTable
// entry.
func TestRouteTable_ImportIsNotARoute(t *testing.T) {
	if _, ok := routeTable["item export"]; !ok {
		t.Errorf("item export should be a routeTable entry")
	}
	if _, ok := routeTable["item import"]; ok {
		t.Errorf("item import must NOT be a routeTable entry — it's handled as a dispatcher special-case (text/markdown raw body)")
	}
}

// TestHTTPHandlerDispatcher_Integration_ArtifactRoundTrip drives the
// full import→export loop through the HTTP dispatcher against a real
// server:
//
//   - import POSTs the raw artifact bytes to /import-artifact (with
//     text/markdown content type) and creates a new DRAFT item.
//   - export then routes a GET to /items/{ref}/export and returns the
//     portable artifact bytes as the tool result.
//
// Uses the `startup` template so the workspace has a conventions
// system collection (a clean, argument-free exportable kind). The
// artifact is built directly via artifact.Encode so the test exercises
// the transport wiring rather than any seeded item's encoding quirks.
func TestHTTPHandlerDispatcher_Integration_ArtifactRoundTrip(t *testing.T) {
	srv, st := newPadServer(t)

	wsRec := doJSONReq(t, srv, http.MethodPost, "/api/v1/workspaces",
		map[string]any{"name": "DocApp", "template": "startup"})
	if wsRec.Code != http.StatusCreated {
		t.Fatalf("create workspace: %d %s", wsRec.Code, wsRec.Body.String())
	}
	var ws models.Workspace
	if err := json.Unmarshal(wsRec.Body.Bytes(), &ws); err != nil {
		t.Fatalf("decode workspace: %v", err)
	}

	user, err := st.CreateUser(models.UserCreate{
		Email: "dave@example.com", Name: "Dave", Password: "irrelevant",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := st.AddWorkspaceMember(ws.ID, user.ID, "owner"); err != nil {
		t.Fatalf("add member: %v", err)
	}

	d := &HTTPHandlerDispatcher{
		Handler:      srv,
		UserResolver: fixedUserResolver(user),
	}

	// Build a clean convention artifact to import.
	data, err := artifact.Encode(artifact.Artifact{
		Kind:          artifact.KindConvention,
		FormatVersion: artifact.FormatVersion,
		Title:         "Imported Convention",
		Fields:        map[string]any{"status": "active", "trigger": "on-commit", "scope": "all", "priority": "must"},
		Body:          "Imported via the MCP HTTP transport.\n",
	})
	if err != nil {
		t.Fatalf("encode artifact: %v", err)
	}

	// import — POST the raw artifact bytes to /import-artifact. Should
	// create a new draft item and return {ref, slug, ...}.
	importCtx := WithDispatchInput(context.Background(), map[string]any{
		"workspace": ws.Slug, "artifact": string(data),
	})
	importRes, err := d.Dispatch(importCtx, []string{"item", "import"}, nil)
	if err != nil || importRes.IsError {
		t.Fatalf("item import: err=%v IsError=%v %#v", err, importRes != nil && importRes.IsError, importRes)
	}
	payload, ok := importRes.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("import result not structured: %#v", importRes.StructuredContent)
	}
	newRef, _ := payload["ref"].(string)
	if newRef == "" {
		t.Fatalf("import result missing new item ref: %#v", payload)
	}

	// export — GET the freshly-imported item back out as an artifact.
	// Proves the export route reaches /items/{ref}/export with the ref
	// in the path and returns the artifact bytes as the result text.
	exportCtx := WithDispatchInput(context.Background(), map[string]any{
		"workspace": ws.Slug, "ref": newRef,
	})
	exportRes, err := d.Dispatch(exportCtx, []string{"item", "export"}, nil)
	if err != nil || exportRes.IsError {
		t.Fatalf("item export: err=%v IsError=%v %#v", err, exportRes != nil && exportRes.IsError, exportRes)
	}
	exported := textOf(exportRes)
	if !strings.HasPrefix(strings.TrimSpace(exported), "---") {
		t.Fatalf("export result does not look like an artifact (no frontmatter fence): %q", exported)
	}
	if !strings.Contains(exported, "Imported Convention") {
		t.Errorf("exported artifact should carry the imported title; got %q", exported)
	}
}

// TestHTTPHandlerDispatcher_Import_MissingArtifact confirms the
// dispatcher rejects an import with no artifact body before touching
// the handler.
func TestHTTPHandlerDispatcher_Import_MissingArtifact(t *testing.T) {
	d := &HTTPHandlerDispatcher{
		Handler:      http.NewServeMux(), // never reached
		UserResolver: fixedUserResolver(&models.User{ID: "u1", Email: "x@y.z"}),
	}
	ctx := WithDispatchInput(context.Background(), map[string]any{"workspace": "docapp"})
	res, err := d.Dispatch(ctx, []string{"item", "import"}, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError for missing artifact; got %s", textOf(res))
	}
	if !strings.Contains(textOf(res), "artifact is required") {
		t.Errorf("message %q should mention artifact requirement", textOf(res))
	}
}
