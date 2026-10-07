package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// --- library activate ---

// The dispatcher no longer resolves a destination or builds fields: since
// TASK-3462 the server's POST /library/activate does both (and records the
// item's built-in origin), so this door, the CLI and the web create the same
// item. What the dispatcher still owns is naming the entry, and only that.
// Where the server lands it, including a renamed collection that declares the
// artifact kind (BUG-2702), is internal/server's
// TestTASK3462_ActivateFollowsTheDeclaredKind.
func TestDispatch_LibraryActivate_PostsTheTitleToTheServer(t *testing.T) {
	for _, title := range []string{"Conventional commit format", "Ship tasks"} {
		t.Run(title, func(t *testing.T) {
			mux := http.NewServeMux()
			posted := ""
			mux.HandleFunc("/api/v1/workspaces/docapp/library/activate", func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					t.Errorf("expected POST; got %s", r.Method)
				}
				buf := make([]byte, r.ContentLength)
				_, _ = r.Body.Read(buf)
				posted = string(buf)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"id":"item-1","title":` + strconv.Quote(title) + `}`))
			})
			mux.HandleFunc("/", func(_ http.ResponseWriter, r *http.Request) {
				t.Errorf("activation reached %s %s; it must POST /library/activate only", r.Method, r.URL.Path)
			})
			d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "u"})}
			res, err := d.Dispatch(
				WithDispatchInput(context.Background(), map[string]any{"workspace": "docapp", "title": title}),
				[]string{"library", "activate"}, nil,
			)
			if err != nil || res.IsError {
				t.Fatalf("Dispatch err=%v IsError=%v: %#v", err, res != nil && res.IsError, res)
			}
			var got map[string]any
			if err := json.Unmarshal([]byte(posted), &got); err != nil {
				t.Fatalf("decode posted body: %v\n%s", err, posted)
			}
			if len(got) != 1 || got["title"] != title {
				t.Errorf("posted %v, want exactly {title: %q}", got, title)
			}
		})
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

// --- Integration smoke ---

// TestHTTPHandlerDispatcher_Integration_ProjectIntelAndLibrary exercises
// project standup/changelog (proxied to the REST project-intel
// endpoints as of TASK-1916) and library activate against a real
// in-process server + store, guarding against 500s on a workspace with
// no data yet.
func TestHTTPHandlerDispatcher_Integration_ProjectIntelAndLibrary(t *testing.T) {
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
