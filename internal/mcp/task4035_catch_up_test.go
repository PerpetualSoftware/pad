package mcp

// TASK-4035: pad_project action=catch-up is a read-only window onto the
// caller's own digest. Mark caught up (PUT /me/seen) stays web-only (ruling
// 380aa793): the cursor is the human's, and the wrappers run under the human's
// credentials. These tests drive the real dispatcher; the real-stack one also
// drives the real server and store.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/server"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

type task4035Req struct{ method, path, query string }

// task4035Recorder records every request that reaches the handler, then
// forwards to next.
type task4035Recorder struct {
	mu   sync.Mutex
	reqs []task4035Req
	next http.Handler
}

func (r *task4035Recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.reqs = append(r.reqs, task4035Req{req.Method, req.URL.Path, req.URL.RawQuery})
	r.mu.Unlock()
	r.next.ServeHTTP(w, req)
}

func (r *task4035Recorder) got() []task4035Req {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]task4035Req(nil), r.reqs...)
}

const task4035Payload = `{"window":{"since":"2026-10-01T00:00:00Z","until":"2026-10-02T00:00:00Z","has_cursor":false,"capped_at_ceiling":true},"needs_you":{"total":1,"lines":[{"ref":"TASK-9","title":"x","collection":"tasks","what":"blocked"}]},"finished":{"total":0,"lines":[]},"new_and_reopened":{"total":0,"lines":[]},"comments":{"total":0,"lines":[]},"agents":{"total":0,"groups":[]}}`

// The binding: exactly one GET on the catch-up path, no query forwarded even
// when the caller supplies params, the body passed through, and nothing that
// could move the cursor.
func TestTASK4035_CatchUpDispatchBindsToOneGET(t *testing.T) {
	rec := &task4035Recorder{next: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(task4035Payload))
	})}
	d := &HTTPHandlerDispatcher{Handler: rec, UserResolver: func(context.Context) *models.User { return &models.User{ID: "u1"} }}

	input := map[string]any{"workspace": "demo", "limit": 3, "since": "2026-01-01", "actor": "agent"}
	res, err := d.Dispatch(WithDispatchInput(context.Background(), input), []string{"project", "catch-up"}, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res.IsError {
		t.Fatalf("dispatch returned an error result: %s", textOf(res))
	}
	reqs := rec.got()
	if len(reqs) != 1 {
		t.Fatalf("requests = %+v, want exactly one", reqs)
	}
	want := task4035Req{http.MethodGet, "/api/v1/workspaces/demo/catch-up", ""}
	if reqs[0] != want {
		t.Errorf("request = %+v, want %+v (a GET with no query)", reqs[0], want)
	}
	for _, r := range reqs {
		if r.method != http.MethodGet || strings.Contains(r.path, "/me/seen") {
			t.Errorf("a request that could move the cursor: %+v", r)
		}
	}
	var got, wantBody map[string]any
	if err := json.Unmarshal([]byte(textOf(res)), &got); err != nil {
		t.Fatalf("result is not the endpoint's JSON: %v\n%s", err, textOf(res))
	}
	_ = json.Unmarshal([]byte(task4035Payload), &wantBody)
	gj, _ := json.Marshal(got)
	wj, _ := json.Marshal(wantBody)
	if string(gj) != string(wj) {
		t.Errorf("response not passed through:\n got %s\nwant %s", gj, wj)
	}
}

// pad_project stays read-only on its tool annotation with catch-up in it.
func TestTASK4035_PadProjectAnnotationStaysReadOnly(t *testing.T) {
	if !isReadOnlyAction("pad_project", "catch-up") {
		t.Fatal("pad_project.catch-up is not in readOnlyActions")
	}
	for _, def := range Catalog {
		if def.Name != "pad_project" {
			continue
		}
		if _, ok := def.Actions["catch-up"]; !ok {
			t.Fatal("pad_project has no catch-up action")
		}
		ann := buildToolFromDef(def).Annotations
		assertAnnotation(t, def.Name, ann, annotationExpectation{
			readOnly: true, destructive: false, idempotent: true, openWorld: openWorldTools[def.Name],
		})
		return
	}
	t.Fatal("pad_project not in Catalog")
}

// Real stack: a member's catch-up over the MCP dispatch leaves the cursor
// absent, twice. A GET that moved it would hide unseen work from the human.
func TestTASK4035_CatchUpOverMCPNeverCreatesTheCursor(t *testing.T) {
	s := storetest.NewSQLite(t)
	srv := server.New(s)
	t.Cleanup(srv.Stop)

	alice, err := s.CreateUser(models.UserCreate{Email: "alice-4035@example.com", Name: "Alice", Username: "alice-4035", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Catch 4035", Slug: "catch-4035", OwnerID: alice.ID})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if err := s.AddWorkspaceMember(ws.ID, alice.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	if err := s.SeedCollectionsFromTemplate(ws.ID, "startup"); err != nil {
		t.Fatalf("SeedCollectionsFromTemplate: %v", err)
	}

	rec := &task4035Recorder{next: srv}
	d := &HTTPHandlerDispatcher{Handler: rec, UserResolver: func(context.Context) *models.User { return alice }}

	for i := 0; i < 2; i++ {
		res, err := d.Dispatch(WithDispatchInput(context.Background(), map[string]any{"workspace": ws.Slug}), []string{"project", "catch-up"}, nil)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if res.IsError {
			t.Fatalf("call %d error: %s", i, textOf(res))
		}
		var body struct {
			Window struct {
				HasCursor bool `json:"has_cursor"`
			} `json:"window"`
		}
		if err := json.Unmarshal([]byte(textOf(res)), &body); err != nil {
			t.Fatalf("call %d: not the digest: %v\n%s", i, err, textOf(res))
		}
		if body.Window.HasCursor {
			t.Errorf("call %d: has_cursor = true after only reading", i)
		}
		if _, found, err := s.ReadCursor(alice.ID, ws.ID); err != nil || found {
			t.Fatalf("after call %d: ReadCursor found=%v err=%v, want not found", i, found, err)
		}
	}
	for _, r := range rec.got() {
		if r.method != http.MethodGet || !strings.HasSuffix(r.path, "/catch-up") {
			t.Errorf("unexpected request: %+v", r)
		}
	}
}
