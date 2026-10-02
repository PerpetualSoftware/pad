package webhooks

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3340 (codex r1 P1): a delivery that read its hook list before the
// workspace was soft-deleted must stop sending at the next boundary: the
// next retry, the next hook in the fan-out, and any redirect hop. Each test
// deletes the workspace from inside the first request the receiver sees.

func TestBUG3340_RetryAfterDeletionSendsNothing(t *testing.T) {
	var hits int32
	var st *mockStore
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		st.deleteWorkspace("ws1")
		w.WriteHeader(http.StatusInternalServerError) // transient: would retry
	}))
	defer srv.Close()
	st = newMockStore([]models.Webhook{{ID: "h1", WorkspaceID: "ws1", URL: srv.URL, Events: `["*"]`, Active: true}})
	d := NewDispatcher(st)
	d.SkipSSRF = true
	d.retryBackoff = 0

	out, err := d.DeliverEvent(Delivery{WorkspaceID: "ws1", EventID: "e", Event: "item.created", Payload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("receiver got %d POSTs, want 1 (no retry after the deletion)", n)
	}
	if out.Matched != 0 || out.Transient != 0 || out.Permanent != 0 {
		t.Errorf("outcome %+v: a suppressed delivery owes nothing and is not a failure", out)
	}
}

func TestBUG3340_LaterHookAfterDeletionSendsNothing(t *testing.T) {
	var first, second int32
	var st *mockStore
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&first, 1)
		st.deleteWorkspace("ws1")
		w.WriteHeader(http.StatusOK)
	}))
	defer a.Close()
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&second, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer b.Close()
	st = newMockStore([]models.Webhook{
		{ID: "h1", WorkspaceID: "ws1", URL: a.URL, Events: `["*"]`, Active: true},
		{ID: "h2", WorkspaceID: "ws1", URL: b.URL, Events: `["*"]`, Active: true},
	})
	d := NewDispatcher(st)
	d.SkipSSRF = true
	if _, err := d.DeliverEvent(Delivery{WorkspaceID: "ws1", EventID: "e", Event: "item.created", Payload: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&first) != 1 {
		t.Fatalf("control: the first hook should receive the event")
	}
	if n := atomic.LoadInt32(&second); n != 0 {
		t.Errorf("the second hook got %d POSTs after the workspace was deleted, want 0", n)
	}
}

func TestBUG3340_RedirectAfterDeletionIsNotFollowed(t *testing.T) {
	var target int32
	var st *mockStore
	dst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&target, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer dst.Close()
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st.deleteWorkspace("ws1")
		http.Redirect(w, r, dst.URL, http.StatusTemporaryRedirect)
	}))
	defer src.Close()
	st = newMockStore([]models.Webhook{{ID: "h1", WorkspaceID: "ws1", URL: src.URL, Events: `["*"]`, Active: true}})
	d := NewDispatcher(st)
	d.SkipSSRF = true
	if _, err := d.DeliverEvent(Delivery{WorkspaceID: "ws1", EventID: "e", Event: "item.created", Payload: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(&target); n != 0 {
		t.Errorf("the redirect target got %d POSTs after the workspace was deleted, want 0", n)
	}
}
