package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// --- project standup ---

func TestDispatch_ProjectStandup_BuildsExpectedShape(t *testing.T) {
	// One completed item, one in-progress, one blocker (dashboard
	// attention), one suggested-next. Verify the response shape
	// matches the CLI's standupCmd JSON branch.
	now := time.Now().UTC()
	yesterday := now.Add(-12 * time.Hour).Format(time.RFC3339)
	weekAgo := now.Add(-7 * 24 * time.Hour).Format(time.RFC3339)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/docapp/dashboard", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"attention":[{"item_title":"Blocked task","reason":"waiting on review"}],
			"suggested_next":[{"item_title":"Next thing","reason":"top priority"}]
		}`))
	})
	mux.HandleFunc("/api/v1/workspaces/docapp/items", func(w http.ResponseWriter, r *http.Request) {
		status := r.URL.Query().Get("status")
		w.Header().Set("Content-Type", "application/json")
		switch status {
		case "done":
			_, _ = w.Write([]byte(`[
				{"collection_prefix":"TASK","item_number":1,"title":"Recently done","collection_slug":"tasks","fields":"{\"status\":\"done\"}","updated_at":"` + yesterday + `"},
				{"collection_prefix":"TASK","item_number":99,"title":"Old done","collection_slug":"tasks","fields":"{\"status\":\"done\"}","updated_at":"` + weekAgo + `"}
			]`))
		case "in-progress":
			_, _ = w.Write([]byte(`[
				{"collection_prefix":"TASK","item_number":2,"title":"Working","collection_slug":"tasks","fields":"{\"priority\":\"high\"}","updated_at":"` + yesterday + `"}
			]`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	})

	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "u"})}
	res, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{
			"workspace": "docapp",
			"days":      float64(1),
		}),
		[]string{"project", "standup"}, nil,
	)
	if err != nil || res.IsError {
		t.Fatalf("Dispatch err=%v IsError=%v: %#v", err, res != nil && res.IsError, res)
	}
	payload, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("not structured: %#v", res.StructuredContent)
	}
	if payload["days"].(float64) != 1 {
		t.Errorf("days = %v, want 1", payload["days"])
	}
	if _, ok := payload["date"].(string); !ok {
		t.Errorf("date should be a string: %v", payload["date"])
	}
	completed, _ := payload["completed"].([]any)
	if len(completed) != 1 {
		t.Errorf("completed should have 1 entry (old item filtered by cutoff); got %d", len(completed))
	} else {
		entry := completed[0].(map[string]any)
		if entry["ref"] != "TASK-1" {
			t.Errorf("completed[0].ref = %v", entry["ref"])
		}
		if entry["status"] != "done" {
			t.Errorf("completed[0].status = %v", entry["status"])
		}
	}
	inProgress, _ := payload["in_progress"].([]any)
	if len(inProgress) != 1 {
		t.Fatalf("in_progress length = %d, want 1", len(inProgress))
	}
	ipEntry := inProgress[0].(map[string]any)
	if ipEntry["priority"] != "high" {
		t.Errorf("in_progress priority = %v", ipEntry["priority"])
	}
	blockers, _ := payload["blockers"].([]any)
	if len(blockers) != 1 || blockers[0].(map[string]any)["title"] != "Blocked task" {
		t.Errorf("blockers unexpected: %v", blockers)
	}
	suggested, _ := payload["suggested_next"].([]any)
	if len(suggested) != 1 || suggested[0].(map[string]any)["title"] != "Next thing" {
		t.Errorf("suggested_next unexpected: %v", suggested)
	}
}

func TestDispatch_ProjectStandup_DefaultsDaysToOne(t *testing.T) {
	// CLI default for --days is 1. Without input, dispatcher must
	// apply that default rather than zeroing the cutoff (which
	// would surface every terminal-status item ever).
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/docapp/dashboard", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/api/v1/workspaces/docapp/items", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	})
	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "u"})}
	res, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{"workspace": "docapp"}),
		[]string{"project", "standup"}, nil,
	)
	if err != nil || res.IsError {
		t.Fatalf("Dispatch err=%v IsError=%v: %#v", err, res != nil && res.IsError, res)
	}
	payload, _ := res.StructuredContent.(map[string]any)
	if payload["days"].(float64) != 1 {
		t.Errorf("default days = %v, want 1", payload["days"])
	}
}

func TestDispatch_ProjectStandup_RequiresWorkspace(t *testing.T) {
	d := &HTTPHandlerDispatcher{
		Handler:      errorHandler(t, "must not be called when workspace missing"),
		UserResolver: fixedUserResolver(&models.User{ID: "u"}),
	}
	res, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{}),
		[]string{"project", "standup"}, nil,
	)
	if err != nil {
		t.Fatalf("Dispatch err: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected IsError when workspace missing")
	}
}

// --- project changelog ---

func TestDispatch_ProjectChangelog_GroupsByCollection(t *testing.T) {
	now := time.Now().UTC()
	recent := now.Add(-2 * 24 * time.Hour).Format(time.RFC3339)
	stale := now.Add(-30 * 24 * time.Hour).Format(time.RFC3339)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/docapp/items", func(w http.ResponseWriter, r *http.Request) {
		status := r.URL.Query().Get("status")
		w.Header().Set("Content-Type", "application/json")
		switch status {
		case "done":
			_, _ = w.Write([]byte(`[
				{"collection_prefix":"TASK","item_number":1,"title":"Recent task","collection_slug":"tasks","collection_name":"Tasks","collection_icon":"📋","fields":"{\"status\":\"done\"}","updated_at":"` + recent + `"},
				{"collection_prefix":"TASK","item_number":99,"title":"Old task","collection_slug":"tasks","fields":"{\"status\":\"done\"}","updated_at":"` + stale + `"}
			]`))
		case "completed":
			_, _ = w.Write([]byte(`[
				{"collection_prefix":"DOC","item_number":3,"title":"Recent doc","collection_slug":"docs","collection_name":"Docs","collection_icon":"📄","fields":"{\"status\":\"completed\"}","updated_at":"` + recent + `"}
			]`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	})

	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "u"})}
	res, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{
			"workspace": "docapp",
			"days":      float64(7),
		}),
		[]string{"project", "changelog"}, nil,
	)
	if err != nil || res.IsError {
		t.Fatalf("Dispatch err=%v IsError=%v: %#v", err, res != nil && res.IsError, res)
	}
	payload, _ := res.StructuredContent.(map[string]any)
	if payload["total"].(float64) != 2 {
		t.Errorf("total = %v, want 2 (old item filtered)", payload["total"])
	}
	if !strings.Contains(payload["period"].(string), "last 7 days") {
		t.Errorf("period = %v", payload["period"])
	}
	groups, _ := payload["groups"].([]any)
	if len(groups) != 2 {
		t.Fatalf("groups length = %d, want 2 (tasks + docs)", len(groups))
	}
	// Both groups should appear; map first-seen ordering is
	// deterministic per the dispatcher's groupOrder slice.
	gotCollections := map[string]bool{}
	for _, g := range groups {
		gm := g.(map[string]any)
		gotCollections[gm["collection"].(string)] = true
		if gm["count"].(float64) != 1 {
			t.Errorf("group %v count = %v, want 1", gm["collection"], gm["count"])
		}
	}
	if !gotCollections["Tasks"] || !gotCollections["Docs"] {
		t.Errorf("expected Tasks + Docs groups; got %v", gotCollections)
	}
}

func TestDispatch_ProjectChangelog_SinceOverridesDays(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/docapp/items", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	})
	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "u"})}
	res, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{
			"workspace": "docapp",
			"days":      float64(7), // ignored when --since set
			"since":     "2025-01-01",
		}),
		[]string{"project", "changelog"}, nil,
	)
	if err != nil || res.IsError {
		t.Fatalf("Dispatch err=%v IsError=%v: %#v", err, res != nil && res.IsError, res)
	}
	payload, _ := res.StructuredContent.(map[string]any)
	if payload["since"] != "2025-01-01" {
		t.Errorf("since = %v, want 2025-01-01 (must use --since over --days)", payload["since"])
	}
	if !strings.Contains(payload["period"].(string), "since 2025-01-01") {
		t.Errorf("period label should reflect --since: %v", payload["period"])
	}
}

func TestDispatch_ProjectChangelog_FiltersByParent(t *testing.T) {
	now := time.Now().UTC()
	recent := now.Add(-1 * 24 * time.Hour).Format(time.RFC3339)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/docapp/items", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("status") == "done" {
			_, _ = w.Write([]byte(`[
				{"collection_prefix":"TASK","item_number":1,"title":"In plan","collection_slug":"tasks","fields":"{}","updated_at":"` + recent + `","parent_ref":"PLAN-3"},
				{"collection_prefix":"TASK","item_number":2,"title":"Outside plan","collection_slug":"tasks","fields":"{}","updated_at":"` + recent + `","parent_ref":"PLAN-99"}
			]`))
			return
		}
		_, _ = w.Write([]byte(`[]`))
	})
	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "u"})}
	res, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{
			"workspace": "docapp",
			"parent":    "PLAN-3",
		}),
		[]string{"project", "changelog"}, nil,
	)
	if err != nil || res.IsError {
		t.Fatalf("Dispatch err=%v IsError=%v: %#v", err, res != nil && res.IsError, res)
	}
	payload, _ := res.StructuredContent.(map[string]any)
	if payload["total"].(float64) != 1 {
		t.Errorf("total = %v, want 1 (only PLAN-3 child)", payload["total"])
	}
	if !strings.Contains(payload["period"].(string), "parent: PLAN-3") {
		t.Errorf("period label should mention parent: %v", payload["period"])
	}
}

func TestDispatch_ProjectChangelog_RejectsBadSinceDate(t *testing.T) {
	d := &HTTPHandlerDispatcher{
		Handler:      errorHandler(t, "must not be called for invalid since"),
		UserResolver: fixedUserResolver(&models.User{ID: "u"}),
	}
	res, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{
			"workspace": "docapp",
			"since":     "not-a-date",
		}),
		[]string{"project", "changelog"}, nil,
	)
	if err != nil {
		t.Fatalf("Dispatch err: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected IsError for malformed --since")
	}
	if !containsToolText(res, "invalid --since") {
		t.Errorf("error should mention invalid --since: %#v", res)
	}
}

// --- library activate ---

func TestDispatch_LibraryActivate_ConventionByTitle(t *testing.T) {
	// Pick a known convention from the seed library — tied to the
	// convention_library.go constants.
	const wantTitle = "Conventional commit format"

	mux := http.NewServeMux()
	posted := ""
	mux.HandleFunc("/api/v1/workspaces/docapp/collections/conventions/items", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST; got %s", r.Method)
		}
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		posted = string(buf)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"item-1","title":"Conventional commit format"}`))
	})
	// Playbook endpoint MUST NOT be hit (we found a convention).
	mux.HandleFunc("/api/v1/workspaces/docapp/collections/playbooks/items", func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("playbook endpoint should not be hit when convention matches")
	})

	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "u"})}
	res, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{
			"workspace": "docapp",
			"title":     wantTitle,
		}),
		[]string{"library", "activate"}, nil,
	)
	if err != nil || res.IsError {
		t.Fatalf("Dispatch err=%v IsError=%v: %#v", err, res != nil && res.IsError, res)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(posted), &got); err != nil {
		t.Fatalf("decode posted body: %v\n%s", err, posted)
	}
	if got["title"] != wantTitle {
		t.Errorf("posted title = %v, want %v", got["title"], wantTitle)
	}
	// Convention fields must include the canonical metadata.
	fieldsStr, _ := got["fields"].(string)
	var fields map[string]any
	if err := json.Unmarshal([]byte(fieldsStr), &fields); err != nil {
		t.Fatalf("decode fields: %v", err)
	}
	if fields["status"] != "active" {
		t.Errorf("status = %v, want active", fields["status"])
	}
	if fields["category"] != "git" {
		t.Errorf("category = %v, want git (from seed library)", fields["category"])
	}
	if fields["trigger"] != "on-commit" {
		t.Errorf("trigger = %v, want on-commit", fields["trigger"])
	}
}

func TestDispatch_LibraryActivate_PlaybookByTitle_FallsThroughConventionLookup(t *testing.T) {
	// A playbook title — the dispatcher should NOT find it in
	// conventions, then fall through to the playbook library.
	const wantTitle = "Implementation Workflow"

	mux := http.NewServeMux()
	posted := ""
	mux.HandleFunc("/api/v1/workspaces/docapp/collections/playbooks/items", func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		posted = string(buf)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"pb-1","title":"Implementation Workflow"}`))
	})

	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "u"})}
	res, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{
			"workspace": "docapp",
			"title":     wantTitle,
		}),
		[]string{"library", "activate"}, nil,
	)
	if err != nil || res.IsError {
		t.Fatalf("Dispatch err=%v IsError=%v: %#v", err, res != nil && res.IsError, res)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(posted), &got); err != nil {
		t.Fatalf("decode posted body: %v\n%s", err, posted)
	}
	if got["title"] != wantTitle {
		t.Errorf("posted title = %v", got["title"])
	}
	fieldsStr, _ := got["fields"].(string)
	var fields map[string]any
	if err := json.Unmarshal([]byte(fieldsStr), &fields); err != nil {
		t.Fatalf("decode fields: %v", err)
	}
	if fields["status"] != "active" {
		t.Errorf("status = %v, want active", fields["status"])
	}
	if _, ok := fields["trigger"]; !ok {
		t.Errorf("playbook fields should include trigger: %v", fields)
	}
	if _, ok := fields["scope"]; !ok {
		t.Errorf("playbook fields should include scope: %v", fields)
	}
}

func TestDispatch_LibraryActivate_NotFoundReturnsError(t *testing.T) {
	d := &HTTPHandlerDispatcher{
		Handler:      errorHandler(t, "must not POST when title is unmatched"),
		UserResolver: fixedUserResolver(&models.User{ID: "u"}),
	}
	res, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{
			"workspace": "docapp",
			"title":     "NoSuchLibraryEntryEverShouldExist",
		}),
		[]string{"library", "activate"}, nil,
	)
	if err != nil {
		t.Fatalf("Dispatch err: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected IsError when title not in either library")
	}
	if !containsToolText(res, "not found in convention or playbook library") {
		t.Errorf("error should mention library lookup; got %#v", res)
	}
}

func TestDispatch_LibraryActivate_RequiresWorkspaceAndTitle(t *testing.T) {
	d := &HTTPHandlerDispatcher{
		Handler:      errorHandler(t, "must not POST when args missing"),
		UserResolver: fixedUserResolver(&models.User{ID: "u"}),
	}
	for _, missing := range []string{"workspace", "title"} {
		t.Run("missing-"+missing, func(t *testing.T) {
			input := map[string]any{
				"workspace": "docapp", "title": "Anything",
			}
			delete(input, missing)
			res, err := d.Dispatch(
				WithDispatchInput(context.Background(), input),
				[]string{"library", "activate"}, nil,
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

// --- Helpers around stringFromMap / itemRefFromMap / extractItemFieldString ---

func TestItemRefFromMap_NumericFormVariants(t *testing.T) {
	cases := []struct {
		name   string
		number any
		want   string
	}{
		{"float64 (typical)", float64(5), "TASK-5"},
		{"int", 42, "TASK-42"},
		{"int64", int64(99), "TASK-99"},
		{"json.Number", json.Number("7"), "TASK-7"},
		{"missing", nil, ""},
		{"non-numeric string", "abc", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := itemRefFromMap(map[string]any{
				"collection_prefix": "TASK",
				"item_number":       tc.number,
			})
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestItemRefFromMap_NoPrefixReturnsEmpty(t *testing.T) {
	if got := itemRefFromMap(map[string]any{"item_number": float64(5)}); got != "" {
		t.Errorf("expected empty string when collection_prefix missing; got %q", got)
	}
}

func TestExtractItemFieldString_HandlesEmptyAndMalformedJSON(t *testing.T) {
	cases := []struct {
		fields string
		want   string
	}{
		{"", ""},
		{"{}", ""},
		{"not json", ""},
		{`{"status":"done"}`, "done"},
		{`{"status":42}`, ""}, // wrong type → empty
	}
	for _, tc := range cases {
		t.Run(tc.fields, func(t *testing.T) {
			got := extractItemFieldString(map[string]any{"fields": tc.fields}, "status")
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestItemMatchesParent_CaseInsensitiveAcrossThreeFields(t *testing.T) {
	item := map[string]any{
		"parent_link_id": "abc-uuid",
		"parent_ref":     "PLAN-3",
		"parent_title":   "API Redesign",
	}
	if !itemMatchesParent(item, "ABC-UUID") {
		t.Errorf("uppercase UUID should match parent_link_id")
	}
	if !itemMatchesParent(item, "plan-3") {
		t.Errorf("lowercase ref should match parent_ref")
	}
	if !itemMatchesParent(item, "api redesign") {
		t.Errorf("lowercase title should match parent_title")
	}
	if itemMatchesParent(item, "OTHER") {
		t.Errorf("unrelated value should not match")
	}
	// Empty parent never matches even when an item has an empty value
	// (defensive — caller is expected to gate on parentFilter != "").
	emptyItem := map[string]any{"parent_ref": ""}
	if itemMatchesParent(emptyItem, "") {
		t.Errorf("empty value should not falsely match")
	}
}

// --- Integration smoke ---

func TestHTTPHandlerDispatcher_Integration_Slice4(t *testing.T) {
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

	// project standup against a fresh workspace — should not 500
	// even though there's no completed work.
	standupRes, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{"workspace": ws.Slug}),
		[]string{"project", "standup"}, nil,
	)
	if err != nil || standupRes.IsError {
		t.Fatalf("standup: err=%v IsError=%v: %#v", err, standupRes != nil && standupRes.IsError, standupRes)
	}

	// project changelog same — empty workspace, no error.
	clRes, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{"workspace": ws.Slug}),
		[]string{"project", "changelog"}, nil,
	)
	if err != nil || clRes.IsError {
		t.Fatalf("changelog: err=%v IsError=%v: %#v", err, clRes != nil && clRes.IsError, clRes)
	}

	// library activate with a known seed convention. The default
	// "startup" template seeds Conventions; this should succeed.
	actRes, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{
			"workspace": ws.Slug,
			"title":     "Conventional commit format",
		}),
		[]string{"library", "activate"}, nil,
	)
	if err != nil || actRes.IsError {
		t.Fatalf("library activate: err=%v IsError=%v: %#v", err, actRes != nil && actRes.IsError, actRes)
	}
	created, _ := actRes.StructuredContent.(map[string]any)
	if title, _ := created["title"].(string); title != "Conventional commit format" {
		t.Errorf("activated item title = %v", title)
	}
}
