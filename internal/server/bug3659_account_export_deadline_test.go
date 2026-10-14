package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
	found := false
	for _, w := range v.Workspaces {
		if w.ID != ws.ID {
			continue
		}
		found = true
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
	if !found {
		t.Fatalf("instrument: the export does not carry workspace %s", ws.ID)
	}
}

// TASK-3401 review of the snapshot path (lead, on BUG-3659): the access
// re-check inside the export's snapshot applies the app ceiling exactly as
// visibleCollectionIDs does, because both run visibleCollectionIDsQ. The
// account export route sits outside the app routes, so a real request never
// carries an app context there; this pins the ceiling anyway, so a future
// re-check that bypassed it fails here. Given an app context whose ceiling
// hides the workspace's collection, the pool path and the snapshot path both
// read the caller as restricted, and the snapshot refuses the workspace.
func TestBUG3659_SnapshotAccessCheckAppliesTheAppCeiling(t *testing.T) {
	srv := testServer(t)
	_ = bug3626Owner(t, srv)
	ownerID, ws := bug3659OwnerAndWorkspace(t, srv)
	owner, err := srv.store.GetUser(ownerID)
	if err != nil || owner == nil {
		t.Fatalf("owner: %v", err)
	}
	base := httptest.NewRequest(http.MethodGet, "/api/v1/auth/export", nil)
	base = base.WithContext(context.WithValue(base.Context(), ctxCurrentUser, owner))

	// Control: without an app context the owner is unrestricted on both paths.
	if ids, err := srv.visibleCollectionIDs(base, ws.ID); err != nil || ids != nil {
		t.Fatalf("control, pool path: %v %v", ids, err)
	}
	if _, _, err := srv.accountExportWorkspace(context.Background(), base, ws.ID); err != nil {
		t.Fatalf("control, snapshot path: %v", err)
	}

	ac := &appContext{WorkspaceID: ws.ID, ReadCeiling: []string{}}
	app := base.WithContext(context.WithValue(base.Context(), appCtxKey{}, ac))
	pool, err := srv.visibleCollectionIDs(app, ws.ID)
	if err != nil || pool == nil {
		t.Fatalf("the pool path did not apply the ceiling: %v %v", pool, err)
	}
	tx, err := srv.store.BeginReadSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	snap, err := srv.visibleCollectionIDsQ(app, tx, ws.ID)
	_ = tx.Rollback()
	if err != nil || snap == nil || len(snap) != len(pool) {
		t.Errorf("the snapshot path's ceiling (%v, %v) differs from the pool path's (%v)", snap, err, pool)
	}
	if _, _, err := srv.accountExportWorkspace(context.Background(), app, ws.ID); !errors.Is(err, errAccountExportRestricted) {
		t.Errorf("the snapshot read a ceilinged request as unrestricted: %v", err)
	}
}
