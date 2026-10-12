package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3626: an account export that runs out of time, or hits a store error,
// must never hand the user a file that parses as a complete export. "Leave Pad
// cleanly" is the promise; a quietly short file breaks it.

// bug3626Owner makes a user who owns one workspace holding one item, and
// returns a session token.
func bug3626Owner(t *testing.T, srv *Server) string {
	t.Helper()
	owner, err := srv.store.CreateUser(models.UserCreate{
		Email: "bug3626@example.com", Name: "Exporter", Username: "bug3626", Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Mine", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("add member: %v", err)
	}
	coll, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Tasks", Slug: "tasks", Prefix: "TASK", Schema: `{"fields":[]}`,
	})
	if err != nil {
		t.Fatalf("create collection: %v", err)
	}
	if _, err := srv.store.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "Keep me", Fields: `{}`}); err != nil {
		t.Fatalf("create item: %v", err)
	}
	tok, err := srv.store.CreateSession(owner.ID, "go-test", "192.0.2.1", "", 24*time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return tok
}

// bug3626Export fetches the export over a real connection (an aborted
// response is only visible on the wire) and reports the body and the read
// error.
func bug3626Export(t *testing.T, srv *Server, tok string) (int, []byte, error) {
	t.Helper()
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/auth/export", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("User-Agent", testSessionUA)
	req.AddCookie(&http.Cookie{Name: sessionCookieName(srv.secureCookies), Value: tok})
	resp, err := ts.Client().Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, body, err
}

// parsesAsCompleteExport reports whether body is a JSON export that claims to
// be whole: valid JSON whose workspaces array is present.
func parsesAsCompleteExport(body []byte) bool {
	var v struct {
		Workspaces []json.RawMessage `json:"workspaces"`
	}
	return json.Unmarshal(body, &v) == nil && v.Workspaces != nil
}

func TestBUG3626_TimedOutExportIsNotAValidCompleteFile(t *testing.T) {
	srv := testServer(t)
	tok := bug3626Owner(t, srv)

	// Out of time before the first workspace (the deadline has passed by
	// the time the loop checks it).
	srv.accountExportHooks.timeout = time.Nanosecond

	_, body, readErr := bug3626Export(t, srv, tok)
	if readErr == nil && parsesAsCompleteExport(body) {
		t.Fatalf("a timed-out export arrived as a valid, complete-looking file with no workspace in it:\n%s", body)
	}
}

// A store error after the body has started aborts the download. Before
// BUG-3626 a collections error was swallowed and an items error was noted as
// "export_error" inside one workspace, and the file still parsed as the whole
// account. Each read is failed through its seam: a broken table fails an
// earlier query and answers 500 before the body starts, which proves nothing.
func TestBUG3626_StoreErrorMidExportIsNotAValidCompleteFile(t *testing.T) {
	for _, c := range []struct {
		name   string
		break_ func(srv *Server, calls *atomic.Int32)
	}{
		{"collections", func(srv *Server, calls *atomic.Int32) {
			srv.accountExportHooks.listCollections = func(string) ([]models.Collection, error) {
				calls.Add(1)
				return nil, errors.New("injected collections failure")
			}
		}},
		{"items", func(srv *Server, calls *atomic.Int32) {
			srv.accountExportHooks.listItems = func(string, models.ItemListParams) ([]models.Item, error) {
				calls.Add(1)
				return nil, errors.New("injected items failure")
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := testServer(t)
			tok := bug3626Owner(t, srv)
			var calls atomic.Int32
			// Set before the test server starts serving, so the handler's
			// read of the hook is ordered after this write.
			c.break_(srv, &calls)

			code, body, readErr := bug3626Export(t, srv, tok)
			// The fault must land where the export is streaming: the hook
			// was reached, and the answer is not an ordinary error status
			// from an earlier step. A dropped connection (code 0: the status
			// line was still buffered when the export aborted) and a 200
			// whose body is cut off are both the loud failure wanted.
			if n := calls.Load(); n != 1 {
				t.Fatalf("the %s hook ran %d times, want 1: the fault did not land in the export loop", c.name, n)
			}
			if code != 0 && code != http.StatusOK {
				t.Fatalf("status %d: an error status before the body, not the mid-export fault under test (body %s)", code, body)
			}
			if readErr == nil && parsesAsCompleteExport(body) {
				t.Fatalf("an export whose %s could not be read arrived as a valid, complete-looking file:\n%s", c.name, body)
			}
		})
	}
}

// The success path: the format and version lead, the trailer says complete,
// the item is in it, and the file is no longer named like a workspace bundle.
func TestBUG3626_CompleteExportCarriesFormatAndTrailer(t *testing.T) {
	srv := testServer(t)
	tok := bug3626Owner(t, srv)

	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/auth/export", nil)
	req.Header.Set("User-Agent", testSessionUA)
	req.AddCookie(&http.Cookie{Name: sessionCookieName(srv.secureCookies), Value: tok})
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading a complete export: %v", err)
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != `attachment; filename="pad-account-export.json"` {
		t.Errorf("Content-Disposition = %q, want pad-account-export.json (pad-export.json is the workspace bundle's file)", cd)
	}
	var v struct {
		Format     string `json:"format"`
		Version    int    `json:"version"`
		Complete   bool   `json:"complete"`
		Workspaces []struct {
			Items []struct {
				Title string `json:"title"`
			} `json:"items"`
		} `json:"workspaces"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("complete export is not valid JSON: %v\n%s", err, body)
	}
	if v.Format != "pad-account-export" || v.Version != 1 {
		t.Errorf("format/version = %q/%d, want pad-account-export/1", v.Format, v.Version)
	}
	if !v.Complete {
		t.Error(`complete export lacks "complete": true`)
	}
	if len(v.Workspaces) != 1 || len(v.Workspaces[0].Items) != 1 || v.Workspaces[0].Items[0].Title != "Keep me" {
		t.Errorf("export does not hold the one workspace and its item: %+v", v.Workspaces)
	}
}

// A store read that stalls cannot hold the export open past its deadline: the
// export stops waiting, aborts, and the read's late result is dropped.
func TestBUG3626_StalledReadIsBoundedByTheDeadline(t *testing.T) {
	srv := testServer(t)
	tok := bug3626Owner(t, srv)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	srv.accountExportHooks.timeout = 300 * time.Millisecond
	srv.accountExportHooks.listItems = func(wsID string, p models.ItemListParams) ([]models.Item, error) {
		// Stalls until the test ends, or 8s at most, so a build without the
		// deadline answers late (and fails the bound below) instead of
		// hanging the test.
		select {
		case <-release:
		case <-time.After(8 * time.Second):
		}
		return srv.store.ListItems(wsID, p)
	}

	start := time.Now()
	_, body, readErr := bug3626Export(t, srv, tok)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the export waited %v on a stalled read; the 300ms deadline must bound it", elapsed)
	}
	if readErr == nil && parsesAsCompleteExport(body) {
		t.Fatalf("a stalled export arrived as a valid, complete-looking file:\n%s", body)
	}
}
