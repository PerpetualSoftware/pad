package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// BUG-3124 unit B: a caught-up tab stamps the flush watermark without writing
// content. The proof is cursor == MAX(op-log id) AND sha256(items.content) ==
// the tab's hash, applied in one conditional UPDATE.

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

type stampFixture struct {
	srv       *Server
	ws, slug  string
	itemID    string
	content   string
	lastOpLog int64
}

// newStampFixture makes the state the stamp exists for: an item whose op-log
// holds a row above its (NULL) watermark while items.content is current — what
// a tab's seed update leaves behind when the view dedupe skips the flush.
func newStampFixture(t *testing.T) *stampFixture {
	t.Helper()
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, ws, "Stamp me", `{"status":"open"}`)
	got, err := srv.store.GetItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	id, err := srv.store.AppendYjsUpdate(item.ID, []byte{0x00, 0x02, 0x03, 0x01, 0x02, 0x03}, "1")
	if err != nil {
		t.Fatal(err)
	}
	return &stampFixture{srv: srv, ws: ws, slug: item.Slug, itemID: item.ID, content: got.Content, lastOpLog: id}
}

func (f *stampFixture) stamp(t *testing.T, body map[string]any) (int, bool) {
	t.Helper()
	rr := doRequest(f.srv, "POST", "/api/v1/workspaces/"+f.ws+"/items/"+f.slug+"/collab-watermark", body)
	var out struct {
		Advanced bool `json:"advanced"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr.Code, out.Advanced
}

func (f *stampFixture) state(t *testing.T) string {
	t.Helper()
	it, err := f.srv.store.GetItem(f.itemID)
	if err != nil {
		t.Fatal(err)
	}
	return it.ContentState
}

func TestWatermarkStamp_CaughtUpTabClearsPendingWithoutWriting(t *testing.T) {
	f := newStampFixture(t)
	if f.state(t) == "" {
		t.Fatal("premise: an op-log row above a NULL watermark must read pending")
	}
	before, _ := f.srv.store.GetItem(f.itemID)
	versionsBefore, err := f.srv.store.ListItemVersionsResolved(f.itemID, before.Content)
	if err != nil {
		t.Fatal(err)
	}

	code, advanced := f.stamp(t, map[string]any{"op_log_cursor": f.lastOpLog, "content_sha256": sha(f.content)})
	if code != http.StatusOK || !advanced {
		t.Fatalf("a caught-up tab with a matching body must stamp; got %d advanced=%v", code, advanced)
	}
	if s := f.state(t); s != "" {
		t.Fatalf("after the stamp the item must read clean; got %q", s)
	}
	after, _ := f.srv.store.GetItem(f.itemID)
	if after.Seq != before.Seq || !after.UpdatedAt.Equal(before.UpdatedAt) || after.Content != before.Content {
		t.Fatalf("the stamp must not touch seq/updated_at/content: seq %d→%d updated_at %v→%v",
			before.Seq, after.Seq, before.UpdatedAt, after.UpdatedAt)
	}
	versionsAfter, err := f.srv.store.ListItemVersionsResolved(f.itemID, after.Content)
	if err != nil {
		t.Fatal(err)
	}
	if len(versionsAfter) != len(versionsBefore) {
		t.Fatalf("the stamp must not write a version row: %d → %d", len(versionsBefore), len(versionsAfter))
	}
}

// The ruling's named case: a cursor behind MAX is a no-op. The tab has not
// applied every row, so it cannot vouch that the body covers them.
func TestWatermarkStamp_StaleCursorIsANoOp(t *testing.T) {
	f := newStampFixture(t)
	newer, err := f.srv.store.AppendYjsUpdate(f.itemID, []byte{0x00, 0x02, 0x03, 0x04, 0x05, 0x06}, "1")
	if err != nil {
		t.Fatal(err)
	}
	code, advanced := f.stamp(t, map[string]any{"op_log_cursor": f.lastOpLog, "content_sha256": sha(f.content)})
	if code != http.StatusOK || advanced {
		t.Fatalf("a cursor behind MAX (%d < %d) must not stamp; got %d advanced=%v", f.lastOpLog, newer, code, advanced)
	}
	if f.state(t) == "" {
		t.Fatal("a refused stamp must leave the item pending")
	}
	// CONTROL: the current cursor on the same item does stamp, so the refusal
	// above is about the cursor and not about this item.
	if _, adv := f.stamp(t, map[string]any{"op_log_cursor": newer, "content_sha256": sha(f.content)}); !adv {
		t.Fatal("control: the current cursor must stamp")
	}
}

func TestWatermarkStamp_BodyMismatchIsANoOp(t *testing.T) {
	f := newStampFixture(t)
	if _, adv := f.stamp(t, map[string]any{"op_log_cursor": f.lastOpLog, "content_sha256": sha(f.content + "x")}); adv {
		t.Fatal("a tab whose document renders to a different body must not stamp")
	}
	if f.state(t) == "" {
		t.Fatal("the item must stay pending")
	}
}

// The body changed between the tab's load and its stamp (a CLI write): the
// tab's hash describes content the row no longer holds.
func TestWatermarkStamp_BodyChangedSinceTheTabLoadedIsANoOp(t *testing.T) {
	f := newStampFixture(t)
	rr := doRequest(f.srv, "PATCH", "/api/v1/workspaces/"+f.ws+"/items/"+f.slug, map[string]any{"content": "rewritten by the CLI"})
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH: %d %s", rr.Code, rr.Body.String())
	}
	// A server-driven content write stamps MAX itself; append a row above it so
	// the item is pending again, then stamp with the tab's OLD body hash.
	id, err := f.srv.store.AppendYjsUpdate(f.itemID, []byte{0x00, 0x02, 0x03, 0x07, 0x08, 0x09}, "1")
	if err != nil {
		t.Fatal(err)
	}
	if _, adv := f.stamp(t, map[string]any{"op_log_cursor": id, "content_sha256": sha(f.content)}); adv {
		t.Fatal("a hash of the body the tab loaded must not stamp over a body that has since changed")
	}
}

func TestWatermarkStamp_MalformedInputIs400(t *testing.T) {
	f := newStampFixture(t)
	for name, body := range map[string]map[string]any{
		"cursor zero":     {"op_log_cursor": 0, "content_sha256": sha(f.content)},
		"negative cursor": {"op_log_cursor": -3, "content_sha256": sha(f.content)},
		"short hash":      {"op_log_cursor": f.lastOpLog, "content_sha256": "abc"},
		"non-hex hash":    {"op_log_cursor": f.lastOpLog, "content_sha256": sha(f.content)[:62] + "zz"},
		"hash missing":    {"op_log_cursor": f.lastOpLog},
	} {
		if code, _ := f.stamp(t, body); code != http.StatusBadRequest {
			t.Errorf("%s: want 400, got %d", name, code)
		}
	}
}

// Moving the watermark is a write decision (it makes rows GC-eligible), so it
// takes edit permission like the flush it stands in for.
func TestWatermarkStamp_ViewerIsRefused(t *testing.T) {
	f := newStampFixture(t)
	ws, err := f.srv.store.GetWorkspaceBySlug(f.ws)
	if err != nil || ws == nil {
		t.Fatalf("GetWorkspaceBySlug: %v", err)
	}
	viewer, tok := loginTestUserAs(t, f.srv, "viewer@example.com", "Viewer", "correct-horse-battery")
	if err := f.srv.store.AddWorkspaceMember(ws.ID, viewer.ID, "viewer"); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{"op_log_cursor": f.lastOpLog, "content_sha256": sha(f.content)})
	req := httptest.NewRequest("POST", "/api/v1/workspaces/"+f.ws+"/items/"+f.slug+"/collab-watermark", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	f.srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("a viewer must be refused; got %d %s", rr.Code, rr.Body.String())
	}
	if f.state(t) == "" {
		t.Fatal("a refused stamp must leave the item pending")
	}
}
