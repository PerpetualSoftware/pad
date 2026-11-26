package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4871: POST /collab-watermark is honoured only from a browser cookie
// session (writeDoorWeb). Token-authenticated doors get 200 {"advanced": false}
// without advancing the watermark, while 400 (malformed input) and 403 (viewer)
// answers are unchanged.

type task4871Fixture struct {
	*stampFixture
	path             string
	editorPAT        string
	editorBearerSess string
	editorCookieSess string
	viewerPAT        string
}

func newTask4871Fixture(t *testing.T) *task4871Fixture {
	t.Helper()
	sf := newStampFixture(t)
	ws, err := sf.srv.store.GetWorkspaceBySlug(sf.ws)
	if err != nil || ws == nil {
		t.Fatalf("GetWorkspaceBySlug(%q): ws=%v err=%v", sf.ws, ws, err)
	}

	editor, err := sf.srv.store.CreateUser(models.UserCreate{
		Email:    "editor@example.com",
		Name:     "Editor",
		Password: "correct-horse-battery-staple",
		Role:     "member",
	})
	if err != nil {
		t.Fatalf("CreateUser editor: %v", err)
	}
	if err := sf.srv.store.AddWorkspaceMember(ws.ID, editor.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember editor: %v", err)
	}
	editorPAT, err := sf.srv.store.CreateAPIToken(editor.ID, models.APITokenCreate{
		Name:        "task4871-editor-pat",
		WorkspaceID: ws.ID,
	}, 0, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken editor: %v", err)
	}
	bearerSess, err := sf.srv.store.CreateSession(editor.ID, "cli-test", "127.0.0.1", "", webSessionTTL)
	if err != nil {
		t.Fatalf("CreateSession bearer: %v", err)
	}
	if !strings.HasPrefix(bearerSess, "padsess_") {
		t.Fatalf("bearer session token %q does not start with padsess_", bearerSess)
	}
	cookieSess, err := sf.srv.store.CreateSession(editor.ID, "web-test", "192.0.2.1", "", webSessionTTL)
	if err != nil {
		t.Fatalf("CreateSession cookie: %v", err)
	}
	if !strings.HasPrefix(cookieSess, "padsess_") {
		t.Fatalf("cookie session token %q does not start with padsess_", cookieSess)
	}

	viewer, err := sf.srv.store.CreateUser(models.UserCreate{
		Email:    "viewer@example.com",
		Name:     "Viewer",
		Password: "correct-horse-battery-staple",
		Role:     "member",
	})
	if err != nil {
		t.Fatalf("CreateUser viewer: %v", err)
	}
	if err := sf.srv.store.AddWorkspaceMember(ws.ID, viewer.ID, "viewer"); err != nil {
		t.Fatalf("AddWorkspaceMember viewer: %v", err)
	}
	viewerPAT, err := sf.srv.store.CreateAPIToken(viewer.ID, models.APITokenCreate{
		Name:        "task4871-viewer-pat",
		WorkspaceID: ws.ID,
	}, 0, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken viewer: %v", err)
	}

	f := &task4871Fixture{
		stampFixture:     sf,
		path:             "/api/v1/workspaces/" + sf.ws + "/items/" + sf.slug + "/collab-watermark",
		editorPAT:        editorPAT.Token,
		editorBearerSess: bearerSess,
		editorCookieSess: cookieSess,
		viewerPAT:        viewerPAT.Token,
	}
	if f.state(t) == "" {
		t.Fatal("premise: item must start pending")
	}
	if wm := f.watermark(t); wm.Valid {
		t.Fatalf("premise: watermark must start NULL, got %d", wm.Int64)
	}
	return f
}

func (f *task4871Fixture) validBody() map[string]any {
	return map[string]any{
		"op_log_cursor":  f.lastOpLog,
		"content_sha256": sha(f.content),
	}
}

func (f *task4871Fixture) watermark(t *testing.T) sql.NullInt64 {
	t.Helper()
	var wm sql.NullInt64
	if err := f.srv.store.DB().QueryRow(`SELECT content_flushed_op_log_id FROM items WHERE id = ?`, f.itemID).Scan(&wm); err != nil {
		t.Fatalf("read content_flushed_op_log_id: %v", err)
	}
	return wm
}

func decodeWatermarkAdvanced(t *testing.T, rr *httptest.ResponseRecorder) bool {
	t.Helper()
	var out struct {
		Advanced bool `json:"advanced"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response %q: %v", rr.Body.String(), err)
	}
	return out.Advanced
}

func assertStampIgnored(t *testing.T, f *task4871Fixture, rr *httptest.ResponseRecorder) {
	t.Helper()
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if adv := decodeWatermarkAdvanced(t, rr); adv {
		t.Fatalf("want advanced=false on non-web door, got true (%s)", rr.Body.String())
	}
	if s := f.state(t); s == "" {
		t.Fatal("ignored stamp must leave item pending")
	}
	if wm := f.watermark(t); wm.Valid {
		t.Fatalf("ignored stamp must leave watermark NULL, got %d", wm.Int64)
	}
}

// Case 1: Editor PAT is a non-web door -> 200 advanced:false, item still pending, watermark NULL.
func TestTask4871_EditorPATIgnored(t *testing.T) {
	t.Parallel()
	f := newTask4871Fixture(t)
	rr := doRequestWithBearer(f.srv, "POST", f.path, f.editorPAT, f.validBody())
	assertStampIgnored(t, f, rr)
}

// Case 2: Editor session token sent as Bearer (CLI-style) -> 200 advanced:false, item still pending, watermark NULL.
func TestTask4871_EditorSessionBearerIgnored(t *testing.T) {
	t.Parallel()
	f := newTask4871Fixture(t)
	rr := doRequestWithBearer(f.srv, "POST", f.path, f.editorBearerSess, f.validBody())
	assertStampIgnored(t, f, rr)
}

// Case 3: Editor PAT with X-Pad-Client: cli -> 200 advanced:false, item still pending, watermark NULL.
func TestTask4871_EditorPATWithCLIClientHeaderIgnored(t *testing.T) {
	t.Parallel()
	f := newTask4871Fixture(t)
	payload, err := json.Marshal(f.validBody())
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest("POST", f.path, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.editorPAT)
	req.Header.Set("X-Pad-Client", "cli")
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	f.srv.ServeHTTP(rr, req)
	assertStampIgnored(t, f, rr)
}

// Case 4: Editor browser cookie session -> 200 advanced:true, item reads clean.
func TestTask4871_EditorBrowserCookieSucceeds(t *testing.T) {
	t.Parallel()
	f := newTask4871Fixture(t)
	rr := doRequestWithCookie(f.srv, "POST", f.path, f.validBody(), f.editorCookieSess)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if adv := decodeWatermarkAdvanced(t, rr); !adv {
		t.Fatalf("want advanced=true on web door, got false (%s)", rr.Body.String())
	}
	if s := f.state(t); s != "" {
		t.Fatalf("after web-door stamp item must read clean, got %q", s)
	}
	if wm := f.watermark(t); !wm.Valid || wm.Int64 != f.lastOpLog {
		t.Fatalf("watermark = %+v, want %d", wm, f.lastOpLog)
	}
}

// Case 5: Malformed body from a PAT is still 400 (validation runs before the door check).
func TestTask4871_MalformedBodyFromPATIs400(t *testing.T) {
	t.Parallel()
	f := newTask4871Fixture(t)
	for name, body := range map[string]map[string]any{
		"negative cursor and short hash": {"op_log_cursor": -1, "content_sha256": "zz"},
		"cursor zero":                    {"op_log_cursor": 0, "content_sha256": sha(f.content)},
		"non-hex hash":                   {"op_log_cursor": f.lastOpLog, "content_sha256": sha(f.content)[:62] + "zz"},
		"hash missing":                   {"op_log_cursor": f.lastOpLog},
	} {
		rr := doRequestWithBearer(f.srv, "POST", f.path, f.editorPAT, body)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s: want 400, got %d: %s", name, rr.Code, rr.Body.String())
		}
	}
	if f.state(t) == "" {
		t.Fatal("malformed requests must leave item pending")
	}
	if wm := f.watermark(t); wm.Valid {
		t.Fatalf("malformed requests must leave watermark NULL, got %d", wm.Int64)
	}
}

// Case 6: Viewer PAT is still 403 (door check runs after permission check).
func TestTask4871_ViewerPATIs403(t *testing.T) {
	t.Parallel()
	f := newTask4871Fixture(t)
	rr := doRequestWithBearer(f.srv, "POST", f.path, f.viewerPAT, f.validBody())
	if rr.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d: %s", rr.Code, rr.Body.String())
	}
	if f.state(t) == "" {
		t.Fatal("refused viewer stamp must leave item pending")
	}
	if wm := f.watermark(t); wm.Valid {
		t.Fatalf("refused viewer stamp must leave watermark NULL, got %d", wm.Int64)
	}
}
