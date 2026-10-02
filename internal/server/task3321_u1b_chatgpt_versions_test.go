package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/collab"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3321 U1b (ruling (b)): every content change through the ChatGPT
// catalog is saved as a version first, past the 1h throttle, so it can be
// undone from History. The door is a context marker set in-process by the
// ChatGPT tool handler; nothing on the wire can set it.

func chatGPTPatch(t *testing.T, srv *Server, path string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest("PATCH", path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(WithChatGPTSurface(req.Context()))
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

func versionSources(t *testing.T, srv *Server, itemID string) []string {
	t.Helper()
	vs, err := srv.store.ListItemVersions(itemID)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = v.Source
	}
	return out
}

func countSource(sources []string, want string) int {
	n := 0
	for _, s := range sources {
		if s == want {
			n++
		}
	}
	return n
}

func TestU1b_EveryChatGPTBodyEditIsVersioned(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, slug, "Item", `{"status":"open"}`)
	path := "/api/v1/workspaces/" + slug + "/items/" + item.Slug

	for i, body := range []string{"first ChatGPT body", "second ChatGPT body"} {
		if rr := chatGPTPatch(t, srv, path, map[string]any{"content": body}); rr.Code != http.StatusOK {
			t.Fatalf("edit %d: %d %s", i, rr.Code, rr.Body.String())
		}
	}
	if n := countSource(versionSources(t, srv, item.ID), models.VersionSourceChatGPT); n != 2 {
		t.Fatalf("two ChatGPT body edits within a minute left %d chatgpt versions, want 2", n)
	}

	// A field-only ChatGPT update writes no version.
	before := len(versionSources(t, srv, item.ID))
	if rr := chatGPTPatch(t, srv, path, map[string]any{"fields": `{"status":"done"}`}); rr.Code != http.StatusOK {
		t.Fatalf("field update: %d %s", rr.Code, rr.Body.String())
	}
	if after := len(versionSources(t, srv, item.ID)); after != before {
		t.Errorf("a field-only ChatGPT update wrote %d version rows", after-before)
	}
}

// Control: the same two edits through the plain API stay throttled, and a
// body claiming version_source "chatgpt" neither forces a version nor wears
// the label.
func TestU1b_PlainAPIIsNotForcedAndCannotClaimTheLabel(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, slug, "Item", `{"status":"open"}`)
	path := "/api/v1/workspaces/" + slug + "/items/" + item.Slug

	for i, body := range []string{"first API body", "second API body"} {
		rr := doRequest(srv, "PATCH", path, map[string]any{"content": body, "version_source": "chatgpt"})
		if rr.Code != http.StatusOK {
			t.Fatalf("edit %d: %d %s", i, rr.Code, rr.Body.String())
		}
	}
	sources := versionSources(t, srv, item.ID)
	if n := countSource(sources, models.VersionSourceChatGPT); n != 0 {
		t.Errorf("a plain API body claimed the chatgpt label %d times: %v", n, sources)
	}
	if len(sources) != 1 {
		t.Errorf("plain API edits left %d versions, want 1 (the second is throttled): %v", len(sources), sources)
	}
}

// With a tab's unflushed edits in the op-log, a ChatGPT body edit is refused
// with the chat-facing message, and nothing is written: the version would
// miss the tab's typing and an undo would lose it.
func TestU1b_PendingTabEditsRefuseTheChatGPTEdit(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, slug, "Item", `{"status":"open"}`)
	path := "/api/v1/workspaces/" + slug + "/items/" + item.Slug
	if _, err := srv.store.AppendYjsUpdate(item.ID, contentFrame(1), collab.DefaultSchemaVersion); err != nil {
		t.Fatal(err)
	}
	before := len(versionSources(t, srv, item.ID))

	rr := chatGPTPatch(t, srv, path, map[string]any{"content": "a ChatGPT body"})
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "content_pending_flush") {
		t.Fatalf("got %d %s, want 409 content_pending_flush", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "being edited in Pad right now") {
		t.Errorf("the refusal does not speak to a chat user: %s", rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "overwrite_pending_edits") {
		t.Errorf("the chat-facing refusal names an API override: %s", rr.Body.String())
	}
	if after := len(versionSources(t, srv, item.ID)); after != before {
		t.Errorf("a refused edit wrote %d version rows", after-before)
	}
	got, err := srv.store.GetItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content == "a ChatGPT body" {
		t.Error("a refused edit changed the body")
	}
}

// With a tab open and caught up (the applier path), the ChatGPT edit is
// versioned too, through BUG-3327's ExternalContent.
func TestU1b_ApplierPathChatGPTEditIsVersioned(t *testing.T) {
	srv := testServerWithCollab(t)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	slug := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, slug, "Item", `{"status":"open"}`)
	path := "/api/v1/workspaces/" + slug + "/items/" + item.Slug
	if rr := doRequest(srv, "PATCH", path, map[string]any{"content": "body before ChatGPT"}); rr.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", rr.Code, rr.Body.String())
	}
	conn, resp, err := dialCollab(t, ts.URL, item.ID, nil, "")
	if err != nil {
		status := ""
		if resp != nil {
			status = resp.Status
		}
		t.Fatalf("dialCollab: %v (%s)", err, status)
	}
	stop := applierEcho(t, conn)
	t.Cleanup(stop)
	waitForApplierPath(t, srv, slug, item.Slug, item.ID)

	rr := chatGPTPatch(t, srv, path, map[string]any{"content": "body from ChatGPT"})
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "applied_pending_flush") {
		t.Fatalf("got %d %s, want 200 through the applier", rr.Code, rr.Body.String())
	}
	vs, err := srv.store.ListItemVersions(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) == 0 || vs[0].Source != models.VersionSourceChatGPT || vs[0].Content != "body before ChatGPT" {
		t.Fatalf("newest version = %+v, want the replaced body with source chatgpt", vs[0])
	}
}
