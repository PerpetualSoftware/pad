package mcp

// `item history` over the HTTP transport (BUG-2304). Real server +
// store, same rationale as the item-list summary tests directly above
// it in spirit: the claim is about the shape an agent actually
// receives, and the versions endpoint genuinely returns full content
// bodies — only driving the real handler proves the projection
// happens (a stub could encode a shape the endpoint never produces).

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/server"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// historyContentMarker must appear in version rows only when the
// caller opted into full=true.
const historyContentMarker = "HISTORY-FULL-BODY-MARKER: summaries must not carry this"

func newHistoryFixture(t *testing.T) *HTTPHandlerDispatcher {
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
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Hist WS", Slug: "hist-ws", OwnerID: owner.ID})
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
	// Creating with content records the initial item_versions row
	// (store.CreateItem), so history has something to list.
	if _, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{
		Title:   "Versioned item",
		Content: historyContentMarker,
	}); err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	return &HTTPHandlerDispatcher{
		Handler:      srv,
		UserResolver: func(context.Context) *models.User { return owner },
	}
}

func dispatchHistory(t *testing.T, d *HTTPHandlerDispatcher, input map[string]any) string {
	t.Helper()
	ctx := WithDispatchInput(context.Background(), input)
	res, err := d.Dispatch(ctx, []string{"item", "history"}, nil)
	if err != nil {
		t.Fatalf("Dispatch(item history): %v", err)
	}
	if res.IsError {
		t.Fatalf("Dispatch(item history) returned an error result: %s", textOf(res))
	}
	return textOf(res)
}

// TestDispatchItemHistory_SummaryShape pins BOTH halves of the fix:
// the action dispatches at all over HTTP (it previously answered "not
// yet implemented over HTTP transport"), and its default shape is the
// CLI's token-light itemVersionSummary projection — metadata present,
// content absent.
func TestDispatchItemHistory_SummaryShape(t *testing.T) {
	t.Parallel()
	d := newHistoryFixture(t)

	text := dispatchHistory(t, d, map[string]any{"workspace": "hist-ws", "ref": "TASK-1"})

	if strings.Contains(text, historyContentMarker) {
		t.Errorf("default history leaked version content over HTTP transport:\n%s", text)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(text), &rows); err != nil {
		t.Fatalf("decode summary payload: %v\n%s", err, text)
	}
	if len(rows) != 1 {
		t.Fatalf("expected exactly 1 version row, got %d\n%s", len(rows), text)
	}
	row := rows[0]
	for _, key := range []string{"id", "created_at", "created_by", "source"} {
		if v, _ := row[key].(string); v == "" {
			t.Errorf("summary row missing %q: %v", key, row)
		}
	}
	// The projection must drop content STRUCTURALLY, not just happen to
	// be empty — mirror of the CLI's itemVersionSummary field set.
	if _, present := row["content"]; present {
		t.Errorf("summary row carries a content key: %v", row)
	}
}

// TestDispatchItemHistory_FullIncludesContent pins the full=true escape
// hatch — the same opt-in the stdio path forwards as the CLI's --full.
func TestDispatchItemHistory_FullIncludesContent(t *testing.T) {
	t.Parallel()
	d := newHistoryFixture(t)

	text := dispatchHistory(t, d, map[string]any{"workspace": "hist-ws", "ref": "TASK-1", "full": true})

	if !strings.Contains(text, historyContentMarker) {
		t.Errorf("full=true history should include version content:\n%s", text)
	}
}

// TestDispatch_BacklinksAndReport_RouteToRealEndpoints smoke-drives
// the two new routeSpec entries (BUG-2304) through the real server —
// a mapper unit test can't catch a pathTemplate that doesn't match the
// server's actual route registration.
func TestDispatch_BacklinksAndReport_RouteToRealEndpoints(t *testing.T) {
	t.Parallel()
	d := newHistoryFixture(t)

	for _, tc := range []struct {
		cmdPath []string
		input   map[string]any
	}{
		{[]string{"item", "backlinks"}, map[string]any{"workspace": "hist-ws", "ref": "TASK-1"}},
		{[]string{"project", "report"}, map[string]any{"workspace": "hist-ws", "window": "week"}},
	} {
		ctx := WithDispatchInput(context.Background(), tc.input)
		res, err := d.Dispatch(ctx, tc.cmdPath, nil)
		if err != nil {
			t.Fatalf("Dispatch(%v): %v", tc.cmdPath, err)
		}
		if res.IsError {
			t.Errorf("Dispatch(%v) returned an error result: %s", tc.cmdPath, textOf(res))
		}
	}
}

// TestDispatchItemHistory_UnknownItemIsNotFound pins the error
// envelope: a missing item must classify as a structured error, not
// a raw 404 body.
func TestDispatchItemHistory_UnknownItemIsNotFound(t *testing.T) {
	t.Parallel()
	d := newHistoryFixture(t)
	ctx := WithDispatchInput(context.Background(), map[string]any{"workspace": "hist-ws", "ref": "TASK-999"})
	res, err := d.Dispatch(ctx, []string{"item", "history"}, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected error result for unknown item, got: %s", textOf(res))
	}
	// Kind-aware classification: the item-shaped 404 must carry the
	// specific item_not_found code and echo the ref in the payload.
	if !strings.Contains(textOf(res), "item_not_found") {
		t.Errorf("expected structured item_not_found envelope, got: %s", textOf(res))
	}
	if !strings.Contains(textOf(res), "TASK-999") {
		t.Errorf("expected the missing ref to appear in the error payload, got: %s", textOf(res))
	}
}
