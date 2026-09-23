package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3156 (a): remote bulk-update is BUG-3049's revert on the transport that
// fix missed. It read the item, set status on the DECODED blob, and PATCHed
// the whole blob back, so a field another client wrote between the read and
// the write was reverted. The CLI has sent `fields_patch` since BUG-3049.
//
// The interleave is made deterministic by wrapping the real server: when the
// bulk-update's PATCH arrives, another client's write to a DIFFERENT field is
// applied first, exactly where production can land one.
func TestDispatch_ItemBulkUpdate_DoesNotRevertAConcurrentFieldWrite(t *testing.T) {
	srv, st := newPadServer(t)
	wsRec := doJSONReq(t, srv, http.MethodPost, "/api/v1/workspaces", map[string]any{"name": "Revert"})
	if wsRec.Code != http.StatusCreated {
		t.Fatalf("create workspace: %d %s", wsRec.Code, wsRec.Body.String())
	}
	var ws models.Workspace
	if err := json.Unmarshal(wsRec.Body.Bytes(), &ws); err != nil {
		t.Fatal(err)
	}
	owner, err := st.CreateUser(models.UserCreate{Email: "owner@example.com", Name: "Owner", Password: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatal(err)
	}

	var once sync.Once
	var interleaved bool
	wrapper := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/items/") {
			once.Do(func() {
				// Another client lowers the priority between bulk-update's
				// read and its write. Same user and auth context, different
				// request, different field.
				body, _ := json.Marshal(map[string]any{"fields_patch": map[string]any{"priority": "low"}})
				other := r.Clone(r.Context())
				other.Body = io.NopCloser(bytes.NewReader(body))
				other.ContentLength = int64(len(body))
				rec := httptest.NewRecorder()
				srv.ServeHTTP(rec, other)
				if rec.Code != http.StatusOK {
					t.Errorf("the interleaved write failed: %d %s", rec.Code, rec.Body.String())
				}
				interleaved = true
			})
		}
		srv.ServeHTTP(w, r)
	})
	d := &HTTPHandlerDispatcher{Handler: wrapper, UserResolver: fixedUserResolver(owner)}

	res, err := d.Dispatch(WithDispatchInput(context.Background(), map[string]any{
		"workspace": ws.Slug, "collection": "tasks", "title": "Race me", "priority": "high",
	}), []string{"item", "create"}, nil)
	if err != nil || res.IsError {
		t.Fatalf("create: %v %#v", err, res)
	}
	ref, _ := res.StructuredContent.(map[string]any)["ref"].(string)
	// The create's own PATCH-free path must not have consumed the interleave.
	if interleaved {
		t.Fatal("precondition: the interleave fired before the bulk-update")
	}

	bulk, err := d.Dispatch(WithDispatchInput(context.Background(), map[string]any{
		"workspace": ws.Slug, "ref": []any{ref}, "status": "in-progress",
	}), []string{"item", "bulk-update"}, nil)
	if err != nil || bulk.IsError {
		t.Fatalf("bulk-update: %v %#v", err, bulk)
	}
	if !interleaved {
		t.Fatal("precondition: the concurrent write never landed between the read and the write")
	}

	show, err := d.Dispatch(WithDispatchInput(context.Background(), map[string]any{
		"workspace": ws.Slug, "ref": ref,
	}), []string{"item", "show"}, nil)
	if err != nil || show.IsError {
		t.Fatalf("show: %v %#v", err, show)
	}
	fields := itemFieldsAsMap(t, show.StructuredContent.(map[string]any))
	if fields["status"] != "in-progress" {
		t.Errorf("status = %v, want in-progress (the bulk-update itself must apply)", fields["status"])
	}
	if fields["priority"] != "low" {
		t.Errorf("priority = %v, want low: the bulk-update reverted another client's write", fields["priority"])
	}
}

// BUG-3156 (b): an item whose collection declares no `priority` is REFUSED by
// remote bulk-update, per row, and nothing is written, the answer WebMCP
// (POST /items/bulk) and stdio give. A declared item in the same call still
// applies.
func TestDispatch_ItemBulkUpdate_RefusesAnUndeclaredKeyPerRow(t *testing.T) {
	srv, st := newPadServer(t)
	wsRec := doJSONReq(t, srv, http.MethodPost, "/api/v1/workspaces", map[string]any{"name": "Strict"})
	if wsRec.Code != http.StatusCreated {
		t.Fatalf("create workspace: %d %s", wsRec.Code, wsRec.Body.String())
	}
	var ws models.Workspace
	_ = json.Unmarshal(wsRec.Body.Bytes(), &ws)
	owner, err := st.CreateUser(models.UserCreate{Email: "strict@example.com", Name: "S", Password: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	d := &HTTPHandlerDispatcher{Handler: srv, UserResolver: fixedUserResolver(owner)}

	if _, err := d.Dispatch(WithDispatchInput(context.Background(), map[string]any{
		"workspace": ws.Slug, "name": "Notes", "fields": "status:select:open,done",
	}), []string{"collection", "create"}, nil); err != nil {
		t.Fatal(err)
	}
	mk := func(coll, title string) string {
		res, err := d.Dispatch(WithDispatchInput(context.Background(), map[string]any{
			"workspace": ws.Slug, "collection": coll, "title": title,
		}), []string{"item", "create"}, nil)
		if err != nil || res.IsError {
			t.Fatalf("create %s: %v %#v", title, err, res)
		}
		return res.StructuredContent.(map[string]any)["ref"].(string)
	}
	note := mk("notes", "A note")
	task := mk("tasks", "A task")

	res, err := d.Dispatch(WithDispatchInput(context.Background(), map[string]any{
		"workspace": ws.Slug, "ref": []any{note, task}, "priority": "high",
	}), []string{"item", "bulk-update"}, nil)
	if err != nil || res.IsError {
		t.Fatalf("bulk-update: %v %#v", err, res)
	}
	payload := res.StructuredContent.(map[string]any)
	if payload["updated"].(float64) != 1 {
		t.Fatalf("updated = %v, want 1 (the task only): %v", payload["updated"], payload)
	}
	rows := payload["results"].([]any)
	first := rows[0].(map[string]any)
	if first["updated"] == true || first["error"] == nil {
		t.Fatalf("the note row must be refused: %v", first)
	}
	// Remote's row vocabulary: validation_failed, with the server's own
	// message carried in the hint (classifyHTTPStatusKind).
	rowErr := first["error"].(map[string]any)
	if rowErr["code"] != "validation_failed" {
		t.Errorf("row code = %v, want validation_failed", rowErr["code"])
	}
	if hint, _ := rowErr["hint"].(string); !strings.Contains(hint, `"priority"`) {
		t.Errorf("the refusal must name the field: %v", rowErr)
	}

	show, _ := d.Dispatch(WithDispatchInput(context.Background(), map[string]any{
		"workspace": ws.Slug, "ref": note,
	}), []string{"item", "show"}, nil)
	if _, stored := itemFieldsAsMap(t, show.StructuredContent.(map[string]any))["priority"]; stored {
		t.Error("an orphan priority was stored on the note")
	}
}
