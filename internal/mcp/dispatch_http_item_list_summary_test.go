package mcp

// BUG-2305 — a bare `pad_item.list` over the HTTP transport must return
// the same token-thrifty SUMMARY shape the exec path produces, not full
// content bodies. The exec path projects via cli.ToItemSummaries in the
// CLI itself; HTTP needs the hand-written dispatchItemList because a
// RouteMapper cannot transform responses and the server's list endpoint
// has no projection parameter.
//
// These tests drive the REAL server + store (same rationale as the
// clear-parent tests): the claim is about what an agent actually
// receives, and a recording-handler stub could encode a response shape
// the endpoint never produces — which is exactly how the old fixtures
// hid this bug (they stubbed `{"items":[]}` where the endpoint returns
// a bare array).

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/server"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// listSummaryContentMarker sits on the SECOND line of the item body,
// beyond the reach of cli.ItemSummary's content_preview (which keeps
// only the first non-empty line, capped at contentPreviewLimit runes).
// A summary legitimately carries a preview — the leak this test hunts
// is the FULL body, which only the marker's placement can distinguish.
const listSummaryContentMarker = "FULL-BODY-MARKER: this line must not appear in summary output"

const listSummaryContent = "An unremarkable first line that the preview may keep.\n\n" +
	listSummaryContentMarker

// listSummaryFixture: one workspace, one collection, one item whose
// content carries a recognizable marker string.
func newListSummaryFixture(t *testing.T) *HTTPHandlerDispatcher {
	t.Helper()
	s := storetest.NewSQLite(t)
	srv := server.New(s)
	t.Cleanup(srv.Stop)

	owner, err := s.CreateUser(models.UserCreate{
		Email:    "owner@example.com",
		Name:     "Owner",
		Password: "correct-horse-battery-staple",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "List WS", Slug: "list-ws", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if err := s.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	coll, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name:   "Tasks",
		Slug:   "tasks",
		Prefix: "TASK",
		Schema: `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"}]}`,
	})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	if _, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{
		Title:   "Item with a big body",
		Content: listSummaryContent,
	}); err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	return &HTTPHandlerDispatcher{
		Handler:      srv,
		UserResolver: func(context.Context) *models.User { return owner },
	}
}

func dispatchList(t *testing.T, d *HTTPHandlerDispatcher, input map[string]any) string {
	t.Helper()
	ctx := WithDispatchInput(context.Background(), input)
	res, err := d.Dispatch(ctx, []string{"item", "list"}, nil)
	if err != nil {
		t.Fatalf("Dispatch(item list): %v", err)
	}
	if res.IsError {
		t.Fatalf("Dispatch(item list) returned an error result: %s", textOf(res))
	}
	return textOf(res)
}

// TestHTTPItemList_DefaultIsSummaryShape pins the fix: no content
// bodies by default, while the items themselves still arrive (the
// summary must be a projection, not an empty result — an accidentally
// broken query would also contain no marker).
func TestHTTPItemList_DefaultIsSummaryShape(t *testing.T) {
	d := newListSummaryFixture(t)

	text := dispatchList(t, d, map[string]any{"workspace": "list-ws"})

	if strings.Contains(text, listSummaryContentMarker) {
		t.Errorf("default item list leaked full content over HTTP transport:\n%s", text)
	}
	// The tool result's TEXT is the bare summaries array (the
	// {items: ...} wrap lives in structuredContent only).
	var items []struct {
		Ref     string `json:"ref"`
		Title   string `json:"title"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(text), &items); err != nil {
		t.Fatalf("decode summary payload: %v\n%s", err, text)
	}
	if len(items) != 1 {
		t.Fatalf("expected exactly 1 item in summary output, got %d\n%s", len(items), text)
	}
	if items[0].Ref != "TASK-1" || items[0].Title != "Item with a big body" {
		t.Errorf("summary lost item identity: %+v", items[0])
	}
	if items[0].Content != "" {
		t.Errorf("summary item carries a content field: %q", items[0].Content)
	}
}

// TestHTTPItemList_ScopeDenialIsPermissionDenied pins the error
// envelope on the SUMMARY path (codex round 1 P2): dispatchItemList
// calls buildAuthedRequest directly, and before the
// buildRequestErrorResult extraction a scope rejection there degraded
// to a generic server_error — while the routeTable path and the
// full:true branch said permission_denied for the same failure. The
// assertion on the code string discriminates: the broken version
// answers server_error.
func TestHTTPItemList_ScopeDenialIsPermissionDenied(t *testing.T) {
	user := &models.User{ID: "user-1", Name: "Dave", Email: "dave@example.com"}
	rec := &recordingHandler{t: t}
	d := &HTTPHandlerDispatcher{
		Handler:      rec,
		UserResolver: func(context.Context) *models.User { return user },
	}

	// JSON null scopes: tokenScopeAllows denies every method on them
	// (a client-serializer bug signal), including this GET.
	ctx := server.WithTokenScopes(context.Background(), `null`)
	ctx = WithDispatchInput(ctx, map[string]any{"workspace": "list-ws"})

	res, err := d.Dispatch(ctx, []string{"item", "list"}, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError for scope-denied list; got success: %#v", res)
	}
	if rec.requestCount != 0 {
		t.Errorf("handler must not be invoked when the scope check fails; got %d calls", rec.requestCount)
	}
	text := textOf(res)
	if !strings.Contains(text, "permission_denied") {
		t.Errorf("scope denial on the summary path must surface as permission_denied, got: %s", text)
	}
}

// TestHTTPItemList_FullOptsIntoCompleteBodies is the counterfactual
// leg: the SAME fixture and the SAME dispatch path returns the marker
// when full is requested — proving the default's missing marker is the
// projection at work, not a fixture that never had content (CONVE-12).
// It also pins transport symmetry for the opt-in: stdio forwards
// full:true as the CLI's --full.
func TestHTTPItemList_FullOptsIntoCompleteBodies(t *testing.T) {
	d := newListSummaryFixture(t)

	text := dispatchList(t, d, map[string]any{"workspace": "list-ws", "full": true})

	if !strings.Contains(text, listSummaryContentMarker) {
		t.Errorf("full: true should return complete content bodies; marker absent:\n%s", text)
	}
}
