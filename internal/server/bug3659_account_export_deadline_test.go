package server

import (
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
