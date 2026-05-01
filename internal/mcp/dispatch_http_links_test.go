package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// itemPrefetchHandler routes /api/v1/workspaces/{ws}/items/{ref} GETs
// to a static map keyed by ref so each link-test can declare just the
// items it needs. Other paths fall through to next so the same mux
// can host the actual link endpoint.
func itemPrefetchHandler(t *testing.T, items map[string]itemPrefetch) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		// Only handle the GET-on-item shape; let the caller decide
		// what to do with everything else.
		if r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		// Path is /api/v1/workspaces/{ws}/items/{ref}; pull the last
		// segment as the ref.
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if len(parts) < 6 {
			http.NotFound(w, r)
			return
		}
		ref := parts[5]
		// Subpaths like .../items/{ref}/links shouldn't hit this — the
		// path will have more than 6 segments. Only match the leaf
		// item route.
		if len(parts) != 6 {
			http.NotFound(w, r)
			return
		}
		got, ok := items[ref]
		if !ok {
			http.Error(w, `{"error":"item not found"}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(got)
	}
}

// linkCaptureHandler records every POST/DELETE on the link endpoint
// under /items/{ref}/links or /links/{linkID} so the create/delete
// tests can assert on path + body. Returns 201/204 with a small JSON
// body so packageHTTPResponse's success branch is exercised.
type linkCaptureHandler struct {
	t              *testing.T
	postCount      int
	deleteCount    int
	lastPostPath   string
	lastPostBody   string
	lastDeletePath string
	respMembers    []models.ItemLink
}

func (h *linkCaptureHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		h.postCount++
		h.lastPostPath = r.URL.Path
		buf, _ := io.ReadAll(r.Body)
		h.lastPostBody = string(buf)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"new-link","link_type":"blocks"}`))
	case http.MethodDelete:
		h.deleteCount++
		h.lastDeletePath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(h.respMembers)
	default:
		http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
	}
}

// --- Create-link dispatchers ---

func TestDispatch_ItemBlock_ResolvesAndPostsLink(t *testing.T) {
	items := map[string]itemPrefetch{
		"TASK-5": {ID: "id-task-5", Slug: "task-5"},
		"TASK-8": {ID: "id-task-8", Slug: "task-8"},
	}
	cap := &linkCaptureHandler{t: t}
	mux := http.NewServeMux()
	mux.Handle("/api/v1/workspaces/docapp/items/task-5/links", cap)
	// Item-resolution catches every other GET on /items/{ref}.
	mux.HandleFunc("/api/v1/workspaces/docapp/items/", itemPrefetchHandler(t, items))

	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "u"})}
	ctx := WithDispatchInput(context.Background(), map[string]any{
		"workspace":  "docapp",
		"source_ref": "TASK-5",
		"target_ref": "TASK-8",
	})
	res, err := d.Dispatch(ctx, []string{"item", "block"}, nil)
	if err != nil || res.IsError {
		t.Fatalf("Dispatch err=%v IsError=%v: %#v", err, res != nil && res.IsError, res)
	}
	if cap.postCount != 1 {
		t.Fatalf("expected 1 POST, got %d", cap.postCount)
	}
	if cap.lastPostPath != "/api/v1/workspaces/docapp/items/task-5/links" {
		t.Errorf("URL slug should come from source_ref; got %q", cap.lastPostPath)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(cap.lastPostBody), &body); err != nil {
		t.Fatalf("decode body: %v\n%s", err, cap.lastPostBody)
	}
	if body["target_id"] != "id-task-8" {
		t.Errorf("target_id should be target_ref's resolved ID; got %v", body)
	}
	if body["link_type"] != "blocks" {
		t.Errorf("link_type = %v, want blocks", body["link_type"])
	}
}

func TestDispatch_ItemBlockedBy_InvertsURLAndBody(t *testing.T) {
	// `blocked-by` writes the link as (blocker → source). The URL
	// path goes through the BLOCKER's slug, body's target_id is the
	// SOURCE item's id. This is the asymmetry that makes the
	// link-spec table necessary.
	items := map[string]itemPrefetch{
		"TASK-5": {ID: "id-task-5", Slug: "task-5"}, // blocked
		"TASK-3": {ID: "id-task-3", Slug: "task-3"}, // blocker
	}
	cap := &linkCaptureHandler{t: t}
	mux := http.NewServeMux()
	mux.Handle("/api/v1/workspaces/docapp/items/task-3/links", cap) // blocker's URL
	mux.HandleFunc("/api/v1/workspaces/docapp/items/", itemPrefetchHandler(t, items))

	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "u"})}
	ctx := WithDispatchInput(context.Background(), map[string]any{
		"workspace":   "docapp",
		"source_ref":  "TASK-5",
		"blocker_ref": "TASK-3",
	})
	res, err := d.Dispatch(ctx, []string{"item", "blocked-by"}, nil)
	if err != nil || res.IsError {
		t.Fatalf("Dispatch err=%v IsError=%v: %#v", err, res != nil && res.IsError, res)
	}
	if cap.postCount != 1 {
		t.Fatalf("expected 1 POST, got %d", cap.postCount)
	}
	if cap.lastPostPath != "/api/v1/workspaces/docapp/items/task-3/links" {
		t.Errorf("URL slug must come from blocker_ref (task-3); got %q", cap.lastPostPath)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(cap.lastPostBody), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["target_id"] != "id-task-5" {
		t.Errorf("target_id must be source_ref's resolved ID (id-task-5); got %v", body)
	}
}

func TestDispatch_ItemImplements_PostsLineageLink(t *testing.T) {
	items := map[string]itemPrefetch{
		"TASK-9":  {ID: "id-task-9", Slug: "task-9"},
		"IDEA-12": {ID: "id-idea-12", Slug: "idea-12"},
	}
	cap := &linkCaptureHandler{t: t}
	mux := http.NewServeMux()
	mux.Handle("/api/v1/workspaces/docapp/items/task-9/links", cap)
	mux.HandleFunc("/api/v1/workspaces/docapp/items/", itemPrefetchHandler(t, items))

	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "u"})}
	ctx := WithDispatchInput(context.Background(), map[string]any{
		"workspace":       "docapp",
		"implementer_ref": "TASK-9",
		"target_ref":      "IDEA-12",
	})
	res, err := d.Dispatch(ctx, []string{"item", "implements"}, nil)
	if err != nil || res.IsError {
		t.Fatalf("Dispatch err=%v IsError=%v: %#v", err, res != nil && res.IsError, res)
	}
	if cap.lastPostPath != "/api/v1/workspaces/docapp/items/task-9/links" {
		t.Errorf("URL slug = %q", cap.lastPostPath)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(cap.lastPostBody), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["link_type"] != models.ItemLinkTypeImplements {
		t.Errorf("link_type = %v", body["link_type"])
	}
	if body["target_id"] != "id-idea-12" {
		t.Errorf("target_id = %v", body["target_id"])
	}
}

func TestDispatch_ItemSplitFrom_UsesCanonicalLinkType(t *testing.T) {
	// CLI accepts "split-from" but the canonical wire form is
	// "split_from" (models.ItemLinkTypeSplitFrom). Make sure the
	// dispatcher writes the canonical form to the body.
	items := map[string]itemPrefetch{
		"TASK-22": {ID: "id-22", Slug: "task-22"},
		"TASK-21": {ID: "id-21", Slug: "task-21"},
	}
	cap := &linkCaptureHandler{t: t}
	mux := http.NewServeMux()
	mux.Handle("/api/v1/workspaces/docapp/items/task-22/links", cap)
	mux.HandleFunc("/api/v1/workspaces/docapp/items/", itemPrefetchHandler(t, items))

	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "u"})}
	ctx := WithDispatchInput(context.Background(), map[string]any{
		"workspace":  "docapp",
		"child_ref":  "TASK-22",
		"parent_ref": "TASK-21",
	})
	res, err := d.Dispatch(ctx, []string{"item", "split-from"}, nil)
	if err != nil || res.IsError {
		t.Fatalf("Dispatch err=%v IsError=%v: %#v", err, res != nil && res.IsError, res)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(cap.lastPostBody), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["link_type"] != "split_from" {
		t.Errorf("link_type should be canonical \"split_from\"; got %v", body["link_type"])
	}
}

func TestDispatch_ItemBlock_MissingRefSurfacesAsToolError(t *testing.T) {
	d := &HTTPHandlerDispatcher{
		Handler:      errorHandler(t, "must not call handler when refs missing"),
		UserResolver: fixedUserResolver(&models.User{ID: "u"}),
	}
	for _, missing := range []string{"workspace", "source_ref", "target_ref"} {
		t.Run("missing-"+missing, func(t *testing.T) {
			input := map[string]any{
				"workspace":  "ws",
				"source_ref": "A",
				"target_ref": "B",
			}
			delete(input, missing)
			ctx := WithDispatchInput(context.Background(), input)
			res, err := d.Dispatch(ctx, []string{"item", "block"}, nil)
			if err != nil {
				t.Fatalf("Dispatch err: %v", err)
			}
			if !res.IsError {
				t.Errorf("expected IsError when %s missing", missing)
			}
		})
	}
}

func TestDispatch_ItemBlock_PrefetchFailureBlocksPost(t *testing.T) {
	// If the source ref doesn't resolve, the link POST must not run.
	postCount := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/docapp/items/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			postCount++
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "u"})}
	ctx := WithDispatchInput(context.Background(), map[string]any{
		"workspace": "docapp", "source_ref": "TASK-5", "target_ref": "TASK-8",
	})
	res, err := d.Dispatch(ctx, []string{"item", "block"}, nil)
	if err != nil {
		t.Fatalf("Dispatch err: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected IsError after prefetch 404; got %#v", res)
	}
	if postCount != 0 {
		t.Errorf("link POST must not run after prefetch failure; ran %d times", postCount)
	}
}

// --- Delete-link dispatchers ---

func TestDispatch_ItemUnblock_FindsAndDeletesMatchingLink(t *testing.T) {
	items := map[string]itemPrefetch{
		"TASK-5": {ID: "id-5", Slug: "task-5"},
		"TASK-8": {ID: "id-8", Slug: "task-8"},
	}
	mux := http.NewServeMux()

	// Links list endpoint returns one matching `blocks` link plus
	// some noise so the delete dispatcher has to actually find the
	// right ID.
	mux.HandleFunc("/api/v1/workspaces/docapp/items/task-5/links", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "unexpected", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]models.ItemLink{
			{ID: "wrong-link-1", SourceID: "id-5", TargetID: "id-8", LinkType: "implements"}, // wrong type
			{ID: "wrong-link-2", SourceID: "id-5", TargetID: "id-99", LinkType: "blocks"},    // wrong target
			{ID: "the-link", SourceID: "id-5", TargetID: "id-8", LinkType: "blocks"},
		})
	})

	deleteCount := 0
	deletedID := ""
	mux.HandleFunc("/api/v1/workspaces/docapp/links/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.Error(w, "unexpected", http.StatusMethodNotAllowed)
			return
		}
		deleteCount++
		deletedID = strings.TrimPrefix(r.URL.Path, "/api/v1/workspaces/docapp/links/")
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("/api/v1/workspaces/docapp/items/", itemPrefetchHandler(t, items))

	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "u"})}
	ctx := WithDispatchInput(context.Background(), map[string]any{
		"workspace": "docapp", "source_ref": "TASK-5", "target_ref": "TASK-8",
	})
	res, err := d.Dispatch(ctx, []string{"item", "unblock"}, nil)
	if err != nil || res.IsError {
		t.Fatalf("Dispatch err=%v IsError=%v: %#v", err, res != nil && res.IsError, res)
	}
	if deleteCount != 1 {
		t.Fatalf("expected 1 DELETE, got %d", deleteCount)
	}
	if deletedID != "the-link" {
		t.Errorf("deleted wrong link; got %q want %q", deletedID, "the-link")
	}
}

func TestDispatch_ItemUnblock_MissingLinkSurfacesError(t *testing.T) {
	// Empty links list → the dispatcher must NOT fall back to
	// deleting something else; it returns an IsError tool result.
	items := map[string]itemPrefetch{
		"TASK-5": {ID: "id-5", Slug: "task-5"},
		"TASK-8": {ID: "id-8", Slug: "task-8"},
	}
	deleteCount := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/docapp/items/task-5/links", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	})
	mux.HandleFunc("/api/v1/workspaces/docapp/links/", func(_ http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleteCount++
		}
	})
	mux.HandleFunc("/api/v1/workspaces/docapp/items/", itemPrefetchHandler(t, items))

	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "u"})}
	ctx := WithDispatchInput(context.Background(), map[string]any{
		"workspace": "docapp", "source_ref": "TASK-5", "target_ref": "TASK-8",
	})
	res, err := d.Dispatch(ctx, []string{"item", "unblock"}, nil)
	if err != nil {
		t.Fatalf("Dispatch err: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected IsError when no matching link found; got %#v", res)
	}
	if deleteCount != 0 {
		t.Errorf("DELETE must not run when no link matches; ran %d times", deleteCount)
	}
}

// --- Read-only link queries ---

func TestDispatch_ItemDeps_GetsLinks(t *testing.T) {
	gotPath := ""
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/docapp/items/TASK-5/links", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "unexpected", http.StatusMethodNotAllowed)
			return
		}
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"l1","link_type":"blocks","source_id":"a","target_id":"b"}]`))
	})

	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "u"})}
	for _, cmd := range []string{"item deps", "item related", "item implemented-by"} {
		t.Run(cmd, func(t *testing.T) {
			ctx := WithDispatchInput(context.Background(), map[string]any{
				"workspace": "docapp", "ref": "TASK-5",
			})
			res, err := d.Dispatch(ctx, strings.Split(cmd, " "), nil)
			if err != nil || res.IsError {
				t.Fatalf("Dispatch err=%v IsError=%v: %#v", err, res != nil && res.IsError, res)
			}
			if gotPath != "/api/v1/workspaces/docapp/items/TASK-5/links" {
				t.Errorf("path = %q", gotPath)
			}
		})
	}
}

func TestDispatch_ItemDeps_RequiresWorkspaceAndRef(t *testing.T) {
	d := &HTTPHandlerDispatcher{
		Handler:      errorHandler(t, "must not be called when input incomplete"),
		UserResolver: fixedUserResolver(&models.User{ID: "u"}),
	}
	for _, missing := range []string{"workspace", "ref"} {
		t.Run("missing-"+missing, func(t *testing.T) {
			input := map[string]any{"workspace": "ws", "ref": "TASK-1"}
			delete(input, missing)
			ctx := WithDispatchInput(context.Background(), input)
			res, err := d.Dispatch(ctx, []string{"item", "deps"}, nil)
			if err != nil {
				t.Fatalf("Dispatch err: %v", err)
			}
			if !res.IsError {
				t.Errorf("expected IsError when %s missing", missing)
			}
		})
	}
}

// --- Slug-based --role resolution ---

func TestResolveRoleSlug_PassesThroughWhenMissing(t *testing.T) {
	d := &HTTPHandlerDispatcher{
		Handler:      errorHandler(t, "must not call agent-roles when role absent"),
		UserResolver: fixedUserResolver(&models.User{ID: "u"}),
	}
	out, err := d.resolveRoleSlug(context.Background(), &models.User{ID: "u"}, map[string]any{
		"workspace": "ws",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if _, has := out["agent_role_id"]; has {
		t.Errorf("output should not introduce agent_role_id when role absent; got %v", out)
	}
}

func TestResolveRoleSlug_ResolvesByExactSlug(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/docapp/agent-roles/implementer", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"role-uuid-imp","slug":"implementer"}`))
	})
	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "u"})}

	out, err := d.resolveRoleSlug(context.Background(), &models.User{ID: "u"}, map[string]any{
		"workspace": "docapp",
		"role":      "implementer",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out["agent_role_id"] != "role-uuid-imp" {
		t.Errorf("expected role-uuid-imp; got %v", out["agent_role_id"])
	}
	if _, present := out["role"]; present {
		t.Errorf("role key should be removed after resolution; got %v", out)
	}
}

func TestResolveRoleSlug_ExplicitIDWins(t *testing.T) {
	d := &HTTPHandlerDispatcher{
		Handler:      errorHandler(t, "must not call agent-roles when explicit ID present"),
		UserResolver: fixedUserResolver(&models.User{ID: "u"}),
	}
	out, err := d.resolveRoleSlug(context.Background(), &models.User{ID: "u"}, map[string]any{
		"workspace":     "docapp",
		"role":          "implementer",
		"agent_role_id": "explicit-uuid",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out["agent_role_id"] != "explicit-uuid" {
		t.Errorf("expected explicit ID to win; got %v", out)
	}
	if _, present := out["role"]; present {
		t.Errorf("role key should be cleared even when ID-only path runs: %v", out)
	}
}

func TestResolveRoleSlug_404SurfacesAsClearError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/docapp/agent-roles/ghost", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	})
	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "u"})}

	_, err := d.resolveRoleSlug(context.Background(), &models.User{ID: "u"}, map[string]any{
		"workspace": "docapp", "role": "ghost",
	})
	if err == nil {
		t.Fatalf("expected error for unknown role slug")
	}
	if !strings.Contains(err.Error(), `"ghost"`) {
		t.Errorf("error should mention the unmatched slug; got %v", err)
	}
}

func TestDispatch_PreprocessesRoleForItemCreate(t *testing.T) {
	// End-to-end: role: "implementer" → resolved → mapItemCreate
	// sees agent_role_id and the create POST carries the UUID at
	// the top level (not in fields).
	captured := newRequestCapture()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/docapp/agent-roles/implementer", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"role-uuid-imp","slug":"implementer"}`))
	})
	mux.Handle("/api/v1/workspaces/docapp/collections/tasks/items", captured)

	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "u"})}
	ctx := WithDispatchInput(context.Background(), map[string]any{
		"workspace":  "docapp",
		"collection": "tasks",
		"title":      "Fix oauth",
		"role":       "implementer",
	})
	res, err := d.Dispatch(ctx, []string{"item", "create"}, nil)
	if err != nil || res.IsError {
		t.Fatalf("Dispatch err=%v IsError=%v: %#v", err, res != nil && res.IsError, res)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(captured.lastBody), &body); err != nil {
		t.Fatalf("decode body: %v\n%s", err, captured.lastBody)
	}
	if body["agent_role_id"] != "role-uuid-imp" {
		t.Errorf("create body did not carry resolved role id: %v", body)
	}
	// And NOT inside fields blob — fields belongs to category /
	// status / priority / parent + custom KVPs only.
	if fields, _ := body["fields"].(string); fields != "" {
		var f map[string]any
		_ = json.Unmarshal([]byte(fields), &f)
		if _, present := f["agent_role_id"]; present {
			t.Errorf("agent_role_id should not be in fields blob: %v", f)
		}
	}
}

func TestDispatch_PreprocessRoleFailureSurfacesAsToolError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/docapp/agent-roles/ghost", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	})
	createCount := 0
	mux.HandleFunc("/api/v1/workspaces/docapp/collections/tasks/items", func(_ http.ResponseWriter, _ *http.Request) {
		createCount++
	})
	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "u"})}
	ctx := WithDispatchInput(context.Background(), map[string]any{
		"workspace": "docapp", "collection": "tasks", "title": "x",
		"role": "ghost",
	})
	res, err := d.Dispatch(ctx, []string{"item", "create"}, nil)
	if err != nil {
		t.Fatalf("Dispatch err: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected IsError when role resolution fails; got %#v", res)
	}
	if createCount != 0 {
		t.Errorf("create handler must not run after role resolution failure; ran %d times", createCount)
	}
}

func TestDispatch_RolePreprocessSkippedForCommandsNotInAllowlist(t *testing.T) {
	// `item show` doesn't take --role. Even if the schema cache
	// happens to carry a stale role, the dispatcher must not call
	// the agent-roles endpoint just to throw the value away.
	captured := newRequestCapture()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/docapp/agent-roles/", func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("agent-roles endpoint must not be called for item.show")
	})
	mux.Handle("/api/v1/workspaces/docapp/items/TASK-5", captured)

	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "u"})}
	ctx := WithDispatchInput(context.Background(), map[string]any{
		"workspace": "docapp", "ref": "TASK-5",
		"role": "implementer", // ignored
	})
	res, err := d.Dispatch(ctx, []string{"item", "show"}, nil)
	if err != nil || res.IsError {
		t.Fatalf("Dispatch err=%v IsError=%v: %#v", err, res != nil && res.IsError, res)
	}
}

// --- Integration: drive link commands against the real *server.Server ---

// TestHTTPHandlerDispatcher_Integration_ItemLinkLifecycle exercises
// the full link-command surface end-to-end against a real handler
// chain + SQLite store. Catches regressions in:
//
//   - URL/body asymmetry (block vs blocked-by)
//   - Canonical link-type wire form (split_from vs split-from)
//   - Read-after-write: deps must surface the freshly-created link
//   - Delete-by-ID: unblock must locate the right link by source/
//     target/type and delete it without the agent ever seeing the ID
//
// The unit tests above stub the handler; this is the only test that
// would catch a regression in handler shape, model schema, or
// middleware ordering.
func TestHTTPHandlerDispatcher_Integration_ItemLinkLifecycle(t *testing.T) {
	srv, st := newPadServer(t)

	// Bootstrap workspace + owner.
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

	create := func(title string) string {
		t.Helper()
		ctx := WithDispatchInput(context.Background(), map[string]any{
			"workspace": ws.Slug, "collection": "tasks", "title": title,
		})
		res, err := d.Dispatch(ctx, []string{"item", "create"}, nil)
		if err != nil || res.IsError {
			t.Fatalf("create %q: err=%v IsError=%v: %#v", title, err, res != nil && res.IsError, res)
		}
		m, ok := res.StructuredContent.(map[string]any)
		if !ok {
			t.Fatalf("create result not structured: %#v", res.StructuredContent)
		}
		ref, _ := m["ref"].(string)
		if ref == "" {
			t.Fatalf("create %q missing ref: %#v", title, m)
		}
		return ref
	}

	a := create("A")
	b := create("B")

	// `item block A B` → A blocks B. The dispatcher must resolve
	// both refs, post the link with A as URL slug + B as target_id.
	blockCtx := WithDispatchInput(context.Background(), map[string]any{
		"workspace": ws.Slug, "source_ref": a, "target_ref": b,
	})
	blockRes, err := d.Dispatch(blockCtx, []string{"item", "block"}, nil)
	if err != nil || blockRes.IsError {
		t.Fatalf("block: err=%v IsError=%v: %#v", err, blockRes != nil && blockRes.IsError, blockRes)
	}

	// `item deps A` should surface the freshly-created blocking link.
	depsCtx := WithDispatchInput(context.Background(), map[string]any{
		"workspace": ws.Slug, "ref": a,
	})
	depsRes, err := d.Dispatch(depsCtx, []string{"item", "deps"}, nil)
	if err != nil || depsRes.IsError {
		t.Fatalf("deps: err=%v IsError=%v: %#v", err, depsRes != nil && depsRes.IsError, depsRes)
	}
	links, ok := depsRes.StructuredContent.([]any)
	if !ok {
		t.Fatalf("deps result not a JSON array: %#v", depsRes.StructuredContent)
	}
	if len(links) != 1 {
		t.Fatalf("expected 1 link, got %d: %#v", len(links), links)
	}
	link, _ := links[0].(map[string]any)
	if link["link_type"] != "blocks" {
		t.Errorf("link type = %v, want blocks", link["link_type"])
	}

	// `item unblock A B` must locate the link and DELETE it. After
	// the delete, deps should be empty.
	unblockCtx := WithDispatchInput(context.Background(), map[string]any{
		"workspace": ws.Slug, "source_ref": a, "target_ref": b,
	})
	unblockRes, err := d.Dispatch(unblockCtx, []string{"item", "unblock"}, nil)
	if err != nil || unblockRes.IsError {
		t.Fatalf("unblock: err=%v IsError=%v: %#v", err, unblockRes != nil && unblockRes.IsError, unblockRes)
	}

	depsAfterCtx := WithDispatchInput(context.Background(), map[string]any{
		"workspace": ws.Slug, "ref": a,
	})
	depsAfterRes, err := d.Dispatch(depsAfterCtx, []string{"item", "deps"}, nil)
	if err != nil || depsAfterRes.IsError {
		t.Fatalf("deps after unblock: err=%v IsError=%v: %#v", err, depsAfterRes != nil && depsAfterRes.IsError, depsAfterRes)
	}
	if linksAfter, ok := depsAfterRes.StructuredContent.([]any); !ok || len(linksAfter) != 0 {
		t.Errorf("expected empty links after unblock, got %#v", depsAfterRes.StructuredContent)
	}

	// `item supersedes new old` exercises the lineage path against
	// the real handler — the canonical wire form of the link type
	// MUST be `supersedes` (not `supersede` or any other variant)
	// or the store rejects the create.
	supersedesCtx := WithDispatchInput(context.Background(), map[string]any{
		"workspace": ws.Slug, "new_ref": a, "old_ref": b,
	})
	supRes, err := d.Dispatch(supersedesCtx, []string{"item", "supersedes"}, nil)
	if err != nil || supRes.IsError {
		t.Fatalf("supersedes: err=%v IsError=%v: %#v", err, supRes != nil && supRes.IsError, supRes)
	}
}

// --- noRemoteEquivalent ---

func TestDispatch_NoRemoteEquivalentReturnsStableError(t *testing.T) {
	d := &HTTPHandlerDispatcher{
		Handler:      errorHandler(t, "handler must not run for CLI-only commands"),
		UserResolver: fixedUserResolver(&models.User{ID: "u"}),
	}
	cmds := [][]string{
		{"agent", "status"},
		{"mcp", "status"},
		{"mcp", "uninstall"},
		{"server", "info"},
		{"server", "open"},
		{"workspace", "link"},
		{"workspace", "switch"},
		{"workspace", "context"},
	}
	for _, cmd := range cmds {
		t.Run(strings.Join(cmd, "."), func(t *testing.T) {
			ctx := WithDispatchInput(context.Background(), map[string]any{})
			res, err := d.Dispatch(ctx, cmd, nil)
			if err != nil {
				t.Fatalf("Dispatch err: %v", err)
			}
			if !res.IsError {
				t.Fatalf("expected IsError for CLI-only command; got %#v", res)
			}
			if !containsToolText(res, "no remote equivalent") {
				t.Errorf("error should call out CLI-only nature; got %#v", res)
			}
		})
	}
}
