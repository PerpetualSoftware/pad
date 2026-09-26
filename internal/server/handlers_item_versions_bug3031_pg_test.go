package server

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3031 on Postgres. A refused restore returns an error from its commit, and
// on Postgres ForceRefreshRoom hands every commit error to the reconcile read,
// which asks whether the restore landed anyway. The read is AMBIGUOUS exactly
// when the version being restored already equals the current body: content
// matches, but the seq never advanced. That takes the UNCERTAIN arm, which
// plain-closes every connected peer. The handler short-circuits reconcile for a
// refusal, since a restore that wrote nothing cannot have landed. This leg drives
// that exact case and asserts the peer's socket survives. It skips unless
// PAD_TEST_POSTGRES_URL is set, and runs under `make test-pg`.
func TestRestoreRefusalOnPostgresLeavesPeersConnected(t *testing.T) {
	srv, _ := testServerPostgres(t)
	if srv.store.D().Driver() != store.DriverPostgres {
		t.Fatalf("expected a Postgres store, got %s", srv.store.D().Driver())
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	slug := createWSWithCollections(t, srv)

	// A → B → A, each write under a different source so the per-(actor, source)
	// version throttle cannot swallow one. The version bracketing the first A
	// then resolves to the same text as the current body.
	it := reconcileNewItem(t, srv, slug, "same body")
	for _, step := range []struct{ content, source string }{{"other body", "web"}, {"same body", "skill"}} {
		c := step.content
		if _, err := srv.store.UpdateItem(it.ID, models.ItemUpdate{Content: &c, LastModifiedBy: "user", Source: step.source}); err != nil {
			t.Fatalf("update to %q: %v", c, err)
		}
	}
	versions, err := srv.store.ListItemVersionsResolved(it.ID, "same body")
	if err != nil {
		t.Fatal(err)
	}
	var sameID string
	for _, v := range versions {
		if v.Content == "same body" {
			sameID = v.ID
			break
		}
	}
	if sameID == "" {
		t.Fatalf("premise: no version resolving to the current body among %d", len(versions))
	}

	peer, resp, err := dialCollab(t, ts.URL, it.ID, nil, "")
	if err != nil {
		status := ""
		if resp != nil {
			status = resp.Status
		}
		t.Fatalf("dialCollab: %v (%s)", err, status)
	}
	t.Cleanup(func() { _ = peer.Close() })
	_ = peer.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, _, err := peer.ReadMessage(); err != nil {
		t.Fatalf("waiting for the initial collab frame: %v", err)
	}

	if _, err := srv.store.AppendYjsUpdate(it.ID, pendingEditFrame, "1"); err != nil {
		t.Fatalf("AppendYjsUpdate: %v", err)
	}

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+it.Slug+"/versions/"+sameID+"/restore", nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d %s", rr.Code, rr.Body.String())
	}
	if code, _ := decodeErrorCode(t, rr); code != "content_pending_flush" {
		t.Fatalf("code: want content_pending_flush, got %q", code)
	}

	// The peer must still be connected: a read that times out is the pass, and
	// a read that returns anything else (a close, EOF, a frame) is the fail.
	_ = peer.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	_, data, err := peer.ReadMessage()
	var ne net.Error
	if err == nil {
		t.Fatalf("a refused restore sent the peer a frame: %q", data)
	}
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("a refused restore disconnected the peer: %v", err)
	}
}
