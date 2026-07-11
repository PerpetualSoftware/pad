package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func setupItemOpenTest(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	t.Setenv("HOME", t.TempDir())

	previousWorkspace := workspaceFlag
	previousURL := urlFlag
	workspaceFlag = "demo"
	urlFlag = server.URL
	t.Cleanup(func() {
		workspaceFlag = previousWorkspace
		urlFlag = previousURL
	})

	return server
}

func itemOpenTestHandler(ref string, item models.Item) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/workspaces/demo/items/" + ref:
			_ = json.NewEncoder(w).Encode(item)
		case "/api/v1/workspaces/demo":
			_ = json.NewEncoder(w).Encode(models.Workspace{
				Slug:          "demo",
				OwnerUsername: "owner",
			})
		default:
			http.NotFound(w, r)
		}
	})
}

func TestItemOpenResolvesRefAndOpensBrowser(t *testing.T) {
	itemNumber := 5
	server := setupItemOpenTest(t, itemOpenTestHandler("task-5", models.Item{
		Slug:             "open-item-in-browser",
		CollectionSlug:   "tasks",
		CollectionPrefix: "TASK",
		ItemNumber:       &itemNumber,
	}))

	var openedURL string
	cmd := itemOpenCmdWithOpener(func(target string) error {
		openedURL = target
		return nil
	})
	cmd.SetArgs([]string{"task-5"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute item open: %v", err)
	}

	want := server.URL + "/login?redirect=%2Fowner%2Fdemo%2Ftasks%2FTASK-5"
	if openedURL != want {
		t.Fatalf("opened URL = %q, want %q", openedURL, want)
	}
}

func TestItemOpenDoesNotOpenBrowserWhenItemIsMissing(t *testing.T) {
	setupItemOpenTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"code":    "not_found",
				"message": "Item not found",
			},
		})
	}))

	opened := false
	cmd := itemOpenCmdWithOpener(func(string) error {
		opened = true
		return nil
	})
	cmd.SetArgs([]string{"TASK-999"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected missing item error, got nil")
	}
	if !strings.Contains(err.Error(), "item TASK-999 not found in workspace demo") {
		t.Fatalf("unexpected error: %v", err)
	}
	if opened {
		t.Fatal("browser opener called for missing item")
	}
}

func TestItemOpenUsesSlugForArchivedItemWithoutCanonicalRef(t *testing.T) {
	deletedAt := time.Now()
	server := setupItemOpenTest(t, itemOpenTestHandler("legacy-item", models.Item{
		Slug:           "legacy-item",
		CollectionSlug: "tasks",
		DeletedAt:      &deletedAt,
	}))

	var openedURL string
	cmd := itemOpenCmdWithOpener(func(target string) error {
		openedURL = target
		return nil
	})
	cmd.SetArgs([]string{"legacy-item"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute item open: %v", err)
	}

	want := server.URL + "/login?redirect=%2Fowner%2Fdemo%2Ftasks%2Flegacy-item"
	if openedURL != want {
		t.Fatalf("opened URL = %q, want %q", openedURL, want)
	}
}

func TestItemOpenReturnsBrowserError(t *testing.T) {
	itemNumber := 5
	setupItemOpenTest(t, itemOpenTestHandler("TASK-5", models.Item{
		Slug:             "open-item-in-browser",
		CollectionSlug:   "tasks",
		CollectionPrefix: "TASK",
		ItemNumber:       &itemNumber,
	}))

	wantErr := errors.New("browser unavailable")
	cmd := itemOpenCmdWithOpener(func(string) error { return wantErr })
	cmd.SetArgs([]string{"TASK-5"})

	if err := cmd.Execute(); !errors.Is(err, wantErr) {
		t.Fatalf("execute item open error = %v, want %v", err, wantErr)
	}
}

func TestItemWebPathRequiresRouteMetadata(t *testing.T) {
	tests := []struct {
		name      string
		workspace models.Workspace
		item      models.Item
	}{
		{name: "owner username", workspace: models.Workspace{Slug: "demo"}, item: models.Item{Slug: "item", CollectionSlug: "tasks"}},
		{name: "workspace slug", workspace: models.Workspace{OwnerUsername: "owner"}, item: models.Item{Slug: "item", CollectionSlug: "tasks"}},
		{name: "collection slug", workspace: models.Workspace{Slug: "demo", OwnerUsername: "owner"}, item: models.Item{Slug: "item"}},
		{name: "item identifier", workspace: models.Workspace{Slug: "demo", OwnerUsername: "owner"}, item: models.Item{CollectionSlug: "tasks"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := itemWebPath(tt.workspace, tt.item); err == nil {
				t.Fatal("expected missing route metadata error")
			}
		})
	}
}

func TestItemOpenRequiresExactlyOneRef(t *testing.T) {
	cmd := itemOpenCmd()

	if err := cmd.Args(cmd, nil); err == nil {
		t.Fatal("expected no-argument validation error")
	}
	if err := cmd.Args(cmd, []string{"TASK-1", "TASK-2"}); err == nil {
		t.Fatal("expected too-many-arguments validation error")
	}
	if err := cmd.Args(cmd, []string{"TASK-1"}); err != nil {
		t.Fatalf("expected one argument to pass validation: %v", err)
	}
}

func TestItemCommandRegistersOpen(t *testing.T) {
	cmd, _, err := itemCmd().Find([]string{"open"})
	if err != nil {
		t.Fatalf("find item open command: %v", err)
	}
	if cmd == nil || cmd.Name() != "open" {
		t.Fatalf("item open command not registered: %#v", cmd)
	}
}
