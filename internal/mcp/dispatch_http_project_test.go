package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// --- project next / ready / stale ---

// TestDispatch_ProjectNext_SlicesToSuggestedNext is the post-BUG-987
// regression test. Pre-fix, `project next` was a route-table alias
// for /dashboard and returned the entire dashboard payload — making
// the action indistinguishable from `project dashboard`. Now it's
// dispatched as a method on HTTPHandlerDispatcher that fetches the
// dashboard then slices to suggested_next, matching the CLI's
// post-fix `pad project next --format json` behaviour.
func TestDispatch_ProjectNext_SlicesToSuggestedNext(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/docapp/dashboard", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"summary": {"total_items": 99},
			"active_items": [{"slug":"x"}],
			"suggested_next": [
				{"item_ref":"TASK-1","item_title":"First","reason":"high priority"},
				{"item_ref":"TASK-2","item_title":"Second","reason":"in_progress"}
			]
		}`))
	})
	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "u"})}
	res, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{"workspace": "docapp"}),
		[]string{"project", "next"}, nil,
	)
	if err != nil || res.IsError {
		t.Fatalf("Dispatch err=%v IsError=%v: %#v", err, res != nil && res.IsError, res)
	}
	// Wrapped as {items: [...]} per BUG-985 fix.
	wrapped, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structuredContent = %T, want map[string]any", res.StructuredContent)
	}
	items, ok := wrapped["items"].([]any)
	if !ok {
		t.Fatalf("items field missing or wrong type: %#v", wrapped)
	}
	if len(items) != 2 {
		t.Errorf("expected 2 suggestions, got %d", len(items))
	}
	// Critical: dashboard-only fields must NOT appear at the top level
	// of the structured content (the WHOLE point of project.next is to
	// be smaller than the dashboard).
	for _, leaked := range []string{"summary", "active_items"} {
		if _, present := wrapped[leaked]; present {
			t.Errorf("project.next leaked dashboard field %q at top level: %#v", leaked, wrapped)
		}
	}
}

// TestDispatch_ProjectNext_EmptyDashboardYieldsEmptyArray covers the
// "no candidates" path — the response must still produce a valid
// items envelope, not return an error or a missing field.
func TestDispatch_ProjectNext_EmptyDashboardYieldsEmptyArray(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/docapp/dashboard", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"suggested_next": []}`))
	})
	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "u"})}
	res, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{"workspace": "docapp"}),
		[]string{"project", "next"}, nil,
	)
	if err != nil || res.IsError {
		t.Fatalf("Dispatch err=%v IsError=%v: %#v", err, res != nil && res.IsError, res)
	}
	wrapped, _ := res.StructuredContent.(map[string]any)
	items, _ := wrapped["items"].([]any)
	if items == nil {
		t.Errorf("expected empty items array, got %#v", wrapped)
	}
	if len(items) != 0 {
		t.Errorf("expected 0 suggestions, got %d", len(items))
	}
}

func TestDispatch_ProjectReady_ReturnsCountResultsShape(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/docapp/dashboard", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"suggested_next":[
				{"item_ref":"TASK-1","item_title":"First","reason":"high priority"},
				{"item_ref":"TASK-2","item_title":"Second","reason":"in_progress"}
			]
		}`))
	})
	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "u"})}
	res, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{"workspace": "docapp"}),
		[]string{"project", "ready"}, nil,
	)
	if err != nil || res.IsError {
		t.Fatalf("Dispatch err=%v IsError=%v: %#v", err, res != nil && res.IsError, res)
	}
	payload, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("not structured: %#v", res.StructuredContent)
	}
	if payload["count"].(float64) != 2 {
		t.Errorf("count = %v, want 2", payload["count"])
	}
	results, _ := payload["results"].([]any)
	if len(results) != 2 {
		t.Fatalf("results length = %d, want 2", len(results))
	}
}

func TestDispatch_ProjectStale_FiltersInterestingTypes(t *testing.T) {
	// Only stalled / blocked / overdue / orphaned_task survive the
	// filter; idle, info, etc. are excluded. Mirrors the CLI's
	// filterAgentAttention behaviour.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/docapp/dashboard", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"attention":[
				{"type":"stalled","item_ref":"TASK-1","item_title":"Stalled item","reason":"no activity","collection":"tasks","item_slug":"slug-1"},
				{"type":"info","item_ref":"TASK-2","item_title":"Just FYI","reason":"new"},
				{"type":"blocked","item_ref":"TASK-3","item_title":"Blocked","reason":"dep","collection":"tasks","item_slug":"slug-3"},
				{"type":"orphaned_task","item_ref":"TASK-4","item_title":"Orphan","reason":"no parent","collection":"tasks","item_slug":"slug-4"}
			]
		}`))
	})
	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "u"})}
	res, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{"workspace": "docapp"}),
		[]string{"project", "stale"}, nil,
	)
	if err != nil || res.IsError {
		t.Fatalf("Dispatch err=%v IsError=%v: %#v", err, res != nil && res.IsError, res)
	}
	payload, _ := res.StructuredContent.(map[string]any)
	if c := payload["count"].(float64); c != 3 {
		t.Errorf("count = %v, want 3 (info filtered out)", c)
	}
	results, _ := payload["results"].([]any)
	if len(results) != 3 {
		t.Fatalf("results length = %d, want 3", len(results))
	}
	// Sort order: type then item_ref. Expected: blocked TASK-3,
	// orphaned_task TASK-4, stalled TASK-1.
	wantOrder := []string{"blocked", "orphaned_task", "stalled"}
	for i, w := range wantOrder {
		entry, _ := results[i].(map[string]any)
		if entry["type"] != w {
			t.Errorf("results[%d].type = %v, want %v", i, entry["type"], w)
		}
	}
}

func TestDispatch_ProjectStale_PreservesAllFields(t *testing.T) {
	// Codex review on PR #348 round 1 caught the previous
	// typed-struct approach dropping `collection` (and would have
	// dropped any future server-side field addition). Pin that
	// every field on each attention entry survives the filter +
	// sort path. Treat the dispatcher's attention slice as a
	// transparent wire-shape forwarder.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/docapp/dashboard", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"attention":[
				{
					"type":"stalled",
					"item_slug":"slug-stale",
					"item_ref":"TASK-9",
					"item_title":"Stale Task",
					"collection":"tasks",
					"reason":"no activity 7d",
					"future_field":"forward-compat"
				}
			]
		}`))
	})
	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "u"})}
	res, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{"workspace": "docapp"}),
		[]string{"project", "stale"}, nil,
	)
	if err != nil || res.IsError {
		t.Fatalf("Dispatch err=%v IsError=%v: %#v", err, res != nil && res.IsError, res)
	}
	payload, _ := res.StructuredContent.(map[string]any)
	results, _ := payload["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("expected 1 result; got %d", len(results))
	}
	entry, _ := results[0].(map[string]any)
	wantFields := map[string]string{
		"type":         "stalled",
		"item_slug":    "slug-stale",
		"item_ref":     "TASK-9",
		"item_title":   "Stale Task",
		"collection":   "tasks",
		"reason":       "no activity 7d",
		"future_field": "forward-compat",
	}
	for k, v := range wantFields {
		if got, _ := entry[k].(string); got != v {
			t.Errorf("entry[%q] = %q, want %q", k, got, v)
		}
	}
}

func TestDispatch_ProjectStaleAndReady_RequireWorkspace(t *testing.T) {
	d := &HTTPHandlerDispatcher{
		Handler:      errorHandler(t, "must not be called when workspace missing"),
		UserResolver: fixedUserResolver(&models.User{ID: "u"}),
	}
	for _, cmd := range []string{"project ready", "project stale"} {
		t.Run(cmd, func(t *testing.T) {
			ctx := WithDispatchInput(context.Background(), map[string]any{})
			res, err := d.Dispatch(ctx, strings.Split(cmd, " "), nil)
			if err != nil {
				t.Fatalf("Dispatch err: %v", err)
			}
			if !res.IsError {
				t.Errorf("expected IsError when workspace missing")
			}
		})
	}
}

// --- project reconcile (noRemoteEquivalent) ---

func TestDispatch_ProjectReconcileRejectedAsCLIOnly(t *testing.T) {
	d := &HTTPHandlerDispatcher{
		Handler:      errorHandler(t, "reconcile must not reach handler"),
		UserResolver: fixedUserResolver(&models.User{ID: "u"}),
	}
	res, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{"workspace": "docapp"}),
		[]string{"project", "reconcile"}, nil,
	)
	if err != nil {
		t.Fatalf("Dispatch err: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected IsError; got %#v", res)
	}
	if !containsToolText(res, "no remote equivalent") {
		t.Errorf("expected stable noRemoteEquivalent message; got %#v", res)
	}
}

// --- collection create ---

func TestMapCollectionCreate_RequiresNameAndWorkspace(t *testing.T) {
	if _, _, _, err := mapCollectionCreate(map[string]any{"name": "X"}); err == nil {
		t.Errorf("expected error when workspace missing")
	}
	if _, _, _, err := mapCollectionCreate(map[string]any{"workspace": "ws"}); err == nil {
		t.Errorf("expected error when name missing")
	}
}

func TestMapCollectionCreate_ParsesFieldsDSL(t *testing.T) {
	_, p, body, err := mapCollectionCreate(map[string]any{
		"workspace":   "docapp",
		"name":        "Bugs",
		"icon":        "🐞",
		"description": "Defect tracker",
		"fields":      "status:select:new,triaged,fixing;severity:select:low,medium,high;component:text",
	})
	if err != nil {
		t.Fatalf("mapCollectionCreate: %v", err)
	}
	if p != "/api/v1/workspaces/docapp/collections" {
		t.Errorf("path = %q", p)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got["name"] != "Bugs" {
		t.Errorf("name = %v", got["name"])
	}
	if got["icon"] != "🐞" {
		t.Errorf("icon = %v", got["icon"])
	}
	schemaStr, _ := got["schema"].(string)
	var schema map[string]any
	if err := json.Unmarshal([]byte(schemaStr), &schema); err != nil {
		t.Fatalf("decode schema: %v", err)
	}
	fields, _ := schema["fields"].([]any)
	if len(fields) != 3 {
		t.Fatalf("fields length = %d, want 3", len(fields))
	}
	first, _ := fields[0].(map[string]any)
	if first["key"] != "status" || first["type"] != "select" {
		t.Errorf("first field unexpected: %v", first)
	}
	if first["required"] != true {
		t.Errorf("status select should be required: %v", first)
	}
	if first["default"] != "new" {
		t.Errorf("status default should be first option (new); got %v", first["default"])
	}
	opts, _ := first["options"].([]any)
	wantOpts := []string{"new", "triaged", "fixing"}
	if len(opts) != len(wantOpts) {
		t.Fatalf("options length = %d, want %d", len(opts), len(wantOpts))
	}
	for i, w := range wantOpts {
		if opts[i] != w {
			t.Errorf("options[%d] = %v, want %v", i, opts[i], w)
		}
	}
	// Settings defaults populated.
	settingsStr, _ := got["settings"].(string)
	var settings map[string]any
	if err := json.Unmarshal([]byte(settingsStr), &settings); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	if settings["layout"] != "fields-primary" {
		t.Errorf("default layout = %v", settings["layout"])
	}
	if settings["default_view"] != "list" {
		t.Errorf("default_view = %v", settings["default_view"])
	}
	if settings["board_group_by"] != "status" {
		t.Errorf("board_group_by = %v", settings["board_group_by"])
	}
}

func TestParseCollectionFieldsDSL_LabelTitleCasesUnderscoredKey(t *testing.T) {
	got, err := parseCollectionFieldsDSL("due_date:date")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	fields, _ := got["fields"].([]struct {
		Key      string   `json:"key"`
		Label    string   `json:"label"`
		Type     string   `json:"type"`
		Options  []string `json:"options,omitempty"`
		Required bool     `json:"required,omitempty"`
		Default  string   `json:"default,omitempty"`
	})
	// The slice is the inner-typed struct; reflect via JSON round-trip.
	jb, _ := json.Marshal(got["fields"])
	var rt []map[string]any
	_ = json.Unmarshal(jb, &rt)
	if len(rt) != 1 {
		t.Fatalf("fields length = %d, want 1 (got %v)", len(rt), fields)
	}
	if rt[0]["label"] != "Due Date" {
		t.Errorf("label = %v, want \"Due Date\"", rt[0]["label"])
	}
}

func TestParseCollectionFieldsDSL_RejectsMalformedEntry(t *testing.T) {
	_, err := parseCollectionFieldsDSL("bare-key-no-type")
	if err == nil {
		t.Errorf("expected error for entry with no type")
	}
}

func TestParseCollectionFieldsDSL_EmptyReturnsEmptyFields(t *testing.T) {
	got, err := parseCollectionFieldsDSL("")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	jb, _ := json.Marshal(got["fields"])
	if string(jb) != "[]" {
		t.Errorf("expected empty fields array; got %s", jb)
	}
}

// --- library list ---

func TestDispatch_LibraryList_BothEndpoints(t *testing.T) {
	mux := http.NewServeMux()
	convCalls, pbCalls := 0, 0
	mux.HandleFunc("/api/v1/convention-library", func(w http.ResponseWriter, _ *http.Request) {
		convCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"categories":[{"name":"git","conventions":[{"title":"C1"}]}]}`))
	})
	mux.HandleFunc("/api/v1/playbook-library", func(w http.ResponseWriter, _ *http.Request) {
		pbCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"categories":[{"name":"flow","playbooks":[{"title":"P1"}]}]}`))
	})
	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "u"})}
	res, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{}),
		[]string{"library", "list"}, nil,
	)
	if err != nil || res.IsError {
		t.Fatalf("Dispatch err=%v IsError=%v: %#v", err, res != nil && res.IsError, res)
	}
	if convCalls != 1 || pbCalls != 1 {
		t.Errorf("expected 1 call each; got conv=%d, pb=%d", convCalls, pbCalls)
	}
	payload, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("expected composed map; got %#v", res.StructuredContent)
	}
	if _, ok := payload["conventions"]; !ok {
		t.Errorf("missing conventions: %v", payload)
	}
	if _, ok := payload["playbooks"]; !ok {
		t.Errorf("missing playbooks: %v", payload)
	}
}

func TestDispatch_LibraryList_TypeFilterSkipsOtherEndpoint(t *testing.T) {
	mux := http.NewServeMux()
	convCalls, pbCalls := 0, 0
	mux.HandleFunc("/api/v1/convention-library", func(w http.ResponseWriter, _ *http.Request) {
		convCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"categories":[]}`))
	})
	mux.HandleFunc("/api/v1/playbook-library", func(w http.ResponseWriter, _ *http.Request) {
		pbCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"categories":[]}`))
	})
	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "u"})}

	// type=conventions skips the playbook endpoint.
	_, _ = d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{"type": "conventions"}),
		[]string{"library", "list"}, nil,
	)
	if pbCalls != 0 {
		t.Errorf("playbook endpoint should not be called for type=conventions; got %d", pbCalls)
	}
	convCalls = 0

	// type=playbooks skips conventions.
	_, _ = d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{"type": "playbooks"}),
		[]string{"library", "list"}, nil,
	)
	if convCalls != 0 {
		t.Errorf("conventions endpoint should not be called for type=playbooks; got %d", convCalls)
	}
}

func TestDispatch_LibraryList_RejectsUnknownType(t *testing.T) {
	d := &HTTPHandlerDispatcher{
		Handler:      errorHandler(t, "no endpoint should be hit for unknown type"),
		UserResolver: fixedUserResolver(&models.User{ID: "u"}),
	}
	res, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{"type": "bogus"}),
		[]string{"library", "list"}, nil,
	)
	if err != nil {
		t.Fatalf("Dispatch err: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected IsError for unknown --type")
	}
}

// --- item bulk-update ---

func TestBulkUpdateRefs_AcceptsCommonShapes(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want []string
	}{
		{"single string", "TASK-5", []string{"TASK-5"}},
		{"[]string", []string{"TASK-1", "TASK-2"}, []string{"TASK-1", "TASK-2"}},
		{"[]any", []any{"TASK-3", "TASK-4"}, []string{"TASK-3", "TASK-4"}},
		{"nil", nil, nil},
		{"empty string", "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := bulkUpdateRefs(tc.in)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("length = %d, want %d", len(got), len(tc.want))
			}
			for i, v := range tc.want {
				if got[i] != v {
					t.Errorf("[%d] = %q, want %q", i, got[i], v)
				}
			}
		})
	}
}

func TestBulkUpdateRefs_RejectsBadEntries(t *testing.T) {
	if _, err := bulkUpdateRefs([]any{"OK", 42}); err == nil {
		t.Errorf("expected error for non-string entry")
	}
	if _, err := bulkUpdateRefs([]any{"OK", ""}); err == nil {
		t.Errorf("expected error for empty entry")
	}
	if _, err := bulkUpdateRefs(map[string]any{}); err == nil {
		t.Errorf("expected error for unsupported type")
	}
}

func TestDispatch_ItemBulkUpdate_RequiresStatusOrPriority(t *testing.T) {
	d := &HTTPHandlerDispatcher{
		Handler:      errorHandler(t, "must not run without status or priority"),
		UserResolver: fixedUserResolver(&models.User{ID: "u"}),
	}
	res, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{
			"workspace": "docapp",
			"ref":       []any{"TASK-1"},
		}),
		[]string{"item", "bulk-update"}, nil,
	)
	if err != nil {
		t.Fatalf("Dispatch err: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected IsError")
	}
	if !containsToolText(res, "at least one") {
		t.Errorf("error should explain status/priority requirement; got %#v", res)
	}
}

func TestDispatch_ItemBulkUpdate_PerItemFailureDoesNotAbort(t *testing.T) {
	// First ref fails GET (404); second succeeds. The dispatcher
	// must report both — successes get Updated:true, failures get
	// Error populated. Mirrors the CLI's per-item green/red output.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/docapp/items/TASK-9", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/api/v1/workspaces/docapp/items/TASK-1", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ref":"TASK-1","fields":"{\"status\":\"open\"}"}`))
		case http.MethodPatch:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ref":"TASK-1"}`))
		}
	})
	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "u"})}
	res, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{
			"workspace": "docapp",
			"ref":       []any{"TASK-9", "TASK-1"},
			"status":    "done",
		}),
		[]string{"item", "bulk-update"}, nil,
	)
	if err != nil || res.IsError {
		t.Fatalf("Dispatch err=%v IsError=%v: %#v", err, res != nil && res.IsError, res)
	}
	payload, _ := res.StructuredContent.(map[string]any)
	if payload["updated"].(float64) != 1 {
		t.Errorf("updated = %v, want 1", payload["updated"])
	}
	if payload["total"].(float64) != 2 {
		t.Errorf("total = %v, want 2", payload["total"])
	}
	results, _ := payload["results"].([]any)
	if len(results) != 2 {
		t.Fatalf("results length = %d, want 2", len(results))
	}
	first, _ := results[0].(map[string]any)
	if first["error"] == nil || first["error"] == "" {
		t.Errorf("first result should have error: %v", first)
	}
	second, _ := results[1].(map[string]any)
	if second["updated"] != true {
		t.Errorf("second result should be updated: %v", second)
	}
}

func TestDispatch_ItemBulkUpdate_MergesExistingFields(t *testing.T) {
	// Bulk-update applies the same RMW merge item.update uses: the
	// existing priority "high" must survive when only --status is
	// being changed.
	mux := http.NewServeMux()
	patchedFields := ""
	mux.HandleFunc("/api/v1/workspaces/docapp/items/TASK-1", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ref":"TASK-1","fields":"{\"status\":\"open\",\"priority\":\"high\",\"category\":\"bug\"}"}`))
		case http.MethodPatch:
			body := make([]byte, r.ContentLength)
			_, _ = r.Body.Read(body)
			var got map[string]any
			_ = json.Unmarshal(body, &got)
			patchedFields, _ = got["fields"].(string)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ref":"TASK-1"}`))
		}
	})
	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "u"})}
	_, _ = d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{
			"workspace": "docapp",
			"ref":       []any{"TASK-1"},
			"status":    "in-progress",
		}),
		[]string{"item", "bulk-update"}, nil,
	)
	var fields map[string]any
	if err := json.Unmarshal([]byte(patchedFields), &fields); err != nil {
		t.Fatalf("decode patched fields: %v", err)
	}
	if fields["status"] != "in-progress" {
		t.Errorf("status not updated: %v", fields)
	}
	if fields["priority"] != "high" {
		t.Errorf("priority should survive RMW: %v", fields)
	}
	if fields["category"] != "bug" {
		t.Errorf("category should survive RMW: %v", fields)
	}
}

// --- item note + decide ---

func TestDispatch_ItemNote_AppendsToFields(t *testing.T) {
	mux := http.NewServeMux()
	patched := ""
	mux.HandleFunc("/api/v1/workspaces/docapp/items/TASK-5", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ref":"TASK-5","fields":"{\"status\":\"open\"}"}`))
		case http.MethodPatch:
			buf := make([]byte, r.ContentLength)
			_, _ = r.Body.Read(buf)
			var got map[string]any
			_ = json.Unmarshal(buf, &got)
			patched, _ = got["fields"].(string)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ref":"TASK-5"}`))
		}
	})
	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "u", Name: "Dave"})}
	res, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{
			"workspace": "docapp",
			"ref":       "TASK-5",
			"summary":   "Investigated mutex bug",
			"details":   "Race in handler; needs lock around shared state",
		}),
		[]string{"item", "note"}, nil,
	)
	if err != nil || res.IsError {
		t.Fatalf("Dispatch err=%v IsError=%v: %#v", err, res != nil && res.IsError, res)
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(patched), &fields); err != nil {
		t.Fatalf("decode patched fields: %v", err)
	}
	notes, ok := fields["implementation_notes"].([]any)
	if !ok || len(notes) != 1 {
		t.Fatalf("expected one implementation_note; got %#v", fields["implementation_notes"])
	}
	note, _ := notes[0].(map[string]any)
	if note["summary"] != "Investigated mutex bug" {
		t.Errorf("summary = %v", note["summary"])
	}
	if note["details"] != "Race in handler; needs lock around shared state" {
		t.Errorf("details = %v", note["details"])
	}
	if note["created_by"] != "Dave" {
		t.Errorf("created_by = %v, want Dave (user.Name fallback)", note["created_by"])
	}
}

func TestDispatch_ItemNote_RequiresArgs(t *testing.T) {
	d := &HTTPHandlerDispatcher{
		Handler:      errorHandler(t, "must not run when args missing"),
		UserResolver: fixedUserResolver(&models.User{ID: "u"}),
	}
	for _, missing := range []string{"workspace", "ref", "summary"} {
		t.Run("missing-"+missing, func(t *testing.T) {
			input := map[string]any{
				"workspace": "ws", "ref": "TASK-1", "summary": "x",
			}
			delete(input, missing)
			res, err := d.Dispatch(
				WithDispatchInput(context.Background(), input),
				[]string{"item", "note"}, nil,
			)
			if err != nil {
				t.Fatalf("Dispatch err: %v", err)
			}
			if !res.IsError {
				t.Errorf("expected IsError when %s missing", missing)
			}
		})
	}
}

func TestDispatch_ItemDecide_AppendsToDecisionLog(t *testing.T) {
	mux := http.NewServeMux()
	patched := ""
	mux.HandleFunc("/api/v1/workspaces/docapp/items/TASK-5", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ref":"TASK-5","fields":"{}"}`))
		case http.MethodPatch:
			buf := make([]byte, r.ContentLength)
			_, _ = r.Body.Read(buf)
			var got map[string]any
			_ = json.Unmarshal(buf, &got)
			patched, _ = got["fields"].(string)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ref":"TASK-5"}`))
		}
	})
	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "u", Name: "Dave"})}
	res, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{
			"workspace": "docapp",
			"ref":       "TASK-5",
			"decision":  "Use Redis for caching",
			"rationale": "Memory pressure on the in-memory cache",
		}),
		[]string{"item", "decide"}, nil,
	)
	if err != nil || res.IsError {
		t.Fatalf("Dispatch err=%v IsError=%v: %#v", err, res != nil && res.IsError, res)
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(patched), &fields); err != nil {
		t.Fatalf("decode patched: %v", err)
	}
	log, ok := fields["decision_log"].([]any)
	if !ok || len(log) != 1 {
		t.Fatalf("expected one decision_log entry; got %#v", fields["decision_log"])
	}
	entry, _ := log[0].(map[string]any)
	if entry["decision"] != "Use Redis for caching" {
		t.Errorf("decision = %v", entry["decision"])
	}
	if entry["rationale"] != "Memory pressure on the in-memory cache" {
		t.Errorf("rationale = %v", entry["rationale"])
	}
}

// --- Integration smoke ---

func TestHTTPHandlerDispatcher_Integration_Slice3(t *testing.T) {
	srv, st := newPadServer(t)
	wsRec := doJSONReq(t, srv, http.MethodPost, "/api/v1/workspaces",
		map[string]any{"name": "DocApp"})
	if wsRec.Code != http.StatusCreated {
		t.Fatalf("create workspace: %d %s", wsRec.Code, wsRec.Body.String())
	}
	var ws models.Workspace
	if err := json.Unmarshal(wsRec.Body.Bytes(), &ws); err != nil {
		t.Fatalf("decode workspace: %v", err)
	}
	owner, err := st.CreateUser(models.UserCreate{Email: "dave@example.com", Name: "Dave", Password: "x"})
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	if err := st.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("add owner: %v", err)
	}

	d := &HTTPHandlerDispatcher{Handler: srv, UserResolver: fixedUserResolver(owner)}

	// Create two items, then bulk-update them.
	mkItem := func(title string) string {
		ctx := WithDispatchInput(context.Background(), map[string]any{
			"workspace": ws.Slug, "collection": "tasks", "title": title,
			"priority": "high",
		})
		res, err := d.Dispatch(ctx, []string{"item", "create"}, nil)
		if err != nil || res.IsError {
			t.Fatalf("create %q: err=%v IsError=%v: %#v", title, err, res != nil && res.IsError, res)
		}
		m, _ := res.StructuredContent.(map[string]any)
		ref, _ := m["ref"].(string)
		if ref == "" {
			t.Fatalf("no ref: %#v", m)
		}
		return ref
	}
	ref1 := mkItem("Item one")
	ref2 := mkItem("Item two")

	bulkRes, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{
			"workspace": ws.Slug,
			"ref":       []any{ref1, ref2},
			"status":    "in-progress",
		}),
		[]string{"item", "bulk-update"}, nil,
	)
	if err != nil || bulkRes.IsError {
		t.Fatalf("bulk-update: err=%v IsError=%v: %#v", err, bulkRes != nil && bulkRes.IsError, bulkRes)
	}
	bulk, _ := bulkRes.StructuredContent.(map[string]any)
	if bulk["updated"].(float64) != 2 {
		t.Errorf("expected both refs updated; got %v", bulk["updated"])
	}

	// Verify priority survived via item show.
	showRes, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{
			"workspace": ws.Slug, "ref": ref1,
		}),
		[]string{"item", "show"}, nil,
	)
	if err != nil || showRes.IsError {
		t.Fatalf("item show: %#v", showRes)
	}
	shown, _ := showRes.StructuredContent.(map[string]any)
	fieldsStr, _ := shown["fields"].(string)
	var fields map[string]any
	_ = json.Unmarshal([]byte(fieldsStr), &fields)
	if fields["priority"] != "high" {
		t.Errorf("priority should survive bulk-update; got %v", fields["priority"])
	}
	if fields["status"] != "in-progress" {
		t.Errorf("status should be updated; got %v", fields["status"])
	}

	// Add a note via item.note.
	noteRes, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{
			"workspace": ws.Slug, "ref": ref1,
			"summary": "Investigated dependency",
			"details": "Found root cause",
		}),
		[]string{"item", "note"}, nil,
	)
	if err != nil || noteRes.IsError {
		t.Fatalf("item note: %#v", noteRes)
	}

	// project ready / project stale shouldn't 500 against the real handler.
	for _, cmd := range []string{"project ready", "project stale"} {
		res, err := d.Dispatch(
			WithDispatchInput(context.Background(), map[string]any{"workspace": ws.Slug}),
			strings.Split(cmd, " "), nil,
		)
		if err != nil || res.IsError {
			t.Errorf("%s: err=%v IsError=%v: %#v", cmd, err, res != nil && res.IsError, res)
		}
	}

	// Collection create end-to-end.
	collRes, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{
			"workspace":   ws.Slug,
			"name":        "Bugs",
			"icon":        "🐞",
			"description": "Defect tracker",
			"fields":      "status:select:new,fixing,resolved;severity:text",
		}),
		[]string{"collection", "create"}, nil,
	)
	if err != nil || collRes.IsError {
		t.Fatalf("collection create: %#v", collRes)
	}

	// Library list (global, no workspace).
	libRes, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{}),
		[]string{"library", "list"}, nil,
	)
	if err != nil || libRes.IsError {
		t.Fatalf("library list: %#v", libRes)
	}
	libPayload, _ := libRes.StructuredContent.(map[string]any)
	if _, has := libPayload["conventions"]; !has {
		t.Errorf("library list (composed) should include conventions: %v", libPayload)
	}
	if _, has := libPayload["playbooks"]; !has {
		t.Errorf("library list (composed) should include playbooks: %v", libPayload)
	}
}
