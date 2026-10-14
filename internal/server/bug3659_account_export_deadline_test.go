package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3659: the account export's deadline covered only the collection and
// item reads. The workspace listing and the access checks ran before it,
// and the app attribution read after them, with nothing bounding them, so a
// read held by a lock (on Postgres, an ACCESS EXCLUSIVE lock on app_installs)
// kept the request open indefinitely. And an attribution error was logged and
// the items shipped without it, in a file that read as complete.

// stall blocks until the test ends, or 8s at most, so a build without the
// deadline answers late and fails the bound instead of hanging the test.
func bug3659Stall(t *testing.T) func() {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	return func() {
		select {
		case <-release:
		case <-time.After(8 * time.Second):
		}
	}
}

func TestBUG3659_EveryExportReadIsBoundedByTheDeadline(t *testing.T) {
	for _, tc := range []struct {
		name string
		arm  func(srv *Server, stall func())
		// before the body: a status is still possible, and it is 503
		preBody bool
	}{
		{"workspace listing", func(srv *Server, stall func()) {
			srv.accountExportHooks.listWorkspaces = func(id string) ([]models.Workspace, error) {
				stall()
				return srv.store.GetUserWorkspaces(id)
			}
		}, true},
		{"access check", func(srv *Server, stall func()) {
			srv.accountExportHooks.visibleColls = func(r *http.Request, wsID string) ([]string, error) {
				stall()
				return srv.visibleCollectionIDs(r, wsID)
			}
		}, true},
		{"app attribution", func(srv *Server, stall func()) {
			srv.accountExportHooks.itemAttribution = func(ids []string) (map[string]store.AppAttribution, error) {
				stall()
				return srv.store.ItemsCreatedViaApp(ids)
			}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := testServer(t)
			tok := bug3626Owner(t, srv)
			srv.accountExportHooks.timeout = 300 * time.Millisecond
			tc.arm(srv, bug3659Stall(t))

			start := time.Now()
			code, body, readErr := bug3626Export(t, srv, tok)
			if elapsed := time.Since(start); elapsed > 5*time.Second {
				t.Fatalf("the export waited %v on a stalled %s; the 300ms deadline must bound it", elapsed, tc.name)
			}
			if readErr == nil && parsesAsCompleteExport(body) {
				t.Fatalf("a stalled export arrived as a valid, complete-looking file:\n%s", body)
			}
			if tc.preBody && code != http.StatusServiceUnavailable {
				t.Errorf("a stall before the body should answer 503, got %d %s", code, body)
			}
		})
	}
}

func TestBUG3659_AttributionErrorAbortsTheExport(t *testing.T) {
	srv := testServer(t)
	tok := bug3626Owner(t, srv)
	srv.accountExportHooks.itemAttribution = func([]string) (map[string]store.AppAttribution, error) {
		return nil, errors.New("injected attribution failure")
	}
	_, body, readErr := bug3626Export(t, srv, tok)
	if readErr == nil && parsesAsCompleteExport(body) {
		t.Fatalf("an export whose attribution read failed arrived as a complete file:\n%s", body)
	}
}

func bug3659OwnerAndWorkspace(t *testing.T, srv *Server) (string, *models.Workspace) {
	t.Helper()
	u, err := srv.store.GetUserByEmail("bug3626@example.com")
	if err != nil || u == nil {
		t.Fatalf("owner: %v", err)
	}
	wss, err := srv.store.GetUserWorkspaces(u.ID)
	if err != nil || len(wss) != 1 {
		t.Fatalf("workspaces: %d %v", len(wss), err)
	}
	return u.ID, &wss[0]
}

// Codex r1: access restricted after the preflight must not be exported.
// The snapshot re-checks it, so the export aborts rather than ship content
// the account can no longer see.
func TestBUG3659_AccessRestrictedAfterPreflightIsNotExported(t *testing.T) {
	srv := testServer(t)
	tok := bug3626Owner(t, srv)
	ownerID, ws := bug3659OwnerAndWorkspace(t, srv)
	colls, err := srv.store.ListCollections(ws.ID)
	if err != nil || len(colls) == 0 {
		t.Fatalf("collections: %v", err)
	}
	srv.accountExportHooks.afterPreflight = func() {
		if err := srv.store.SetMemberCollectionAccess(ws.ID, ownerID, "specific", []string{colls[0].ID}); err != nil {
			t.Errorf("restrict: %v", err)
		}
	}
	_, body, readErr := bug3626Export(t, srv, tok)
	if readErr == nil && parsesAsCompleteExport(body) {
		t.Fatalf("an export whose access was restricted mid-way arrived complete:\n%s", body)
	}
}

// Codex r1: a workspace's collections and items come from one snapshot, so a
// collection and item created between the two reads are in neither, and every
// exported item's collection is in the export.
func TestBUG3659_CollectionsAndItemsShareOneSnapshot(t *testing.T) {
	srv := testServer(t)
	tok := bug3626Owner(t, srv)
	_, ws := bug3659OwnerAndWorkspace(t, srv)
	srv.accountExportHooks.afterSnapshotCollections = func(wsID string) {
		c, err := srv.store.CreateCollection(wsID, models.CollectionCreate{Name: "Late", Slug: "late", Prefix: "LATE", Schema: `{"fields":[]}`})
		if err != nil {
			t.Errorf("late collection: %v", err)
			return
		}
		if _, err := srv.store.CreateItem(wsID, c.ID, models.ItemCreate{Title: "Late item", Fields: "{}"}); err != nil {
			t.Errorf("late item: %v", err)
		}
	}
	code, body, readErr := bug3626Export(t, srv, tok)
	if readErr != nil || code != http.StatusOK {
		t.Fatalf("export: %d %v", code, readErr)
	}
	var v struct {
		Complete   bool `json:"complete"`
		Workspaces []struct {
			ID          string              `json:"id"`
			Collections []models.Collection `json:"collections"`
			Items       []models.Item       `json:"items"`
		} `json:"workspaces"`
	}
	if err := json.Unmarshal(body, &v); err != nil || !v.Complete {
		t.Fatalf("export is not a complete file: %v\n%s", err, body)
	}
	for _, w := range v.Workspaces {
		if w.ID != ws.ID {
			continue
		}
		have := map[string]bool{}
		for _, c := range w.Collections {
			have[c.ID] = true
		}
		if len(w.Items) == 0 {
			t.Fatal("instrument: the workspace exported no items")
		}
		for _, it := range w.Items {
			if !have[it.CollectionID] {
				t.Errorf("item %q is exported without its collection %s", it.Title, it.CollectionID)
			}
		}
	}
}
