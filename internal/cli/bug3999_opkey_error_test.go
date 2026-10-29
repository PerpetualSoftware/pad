package cli

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// BUG-3999: a failed write that sent an op_key carries it in an OpKeyError,
// without changing what errors.As / the retry classifiers see.
func TestBUG3999_FailedCopyCarriesItsKey(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"copy_failed","message":"m"}}`))
	}))
	defer ts.Close()
	c := newCopyTestClient(t, ts)
	_, _, err := c.CopyItem("ws", "TASK-1", ItemCopyRequest{TargetWorkspace: "b", TargetCollection: "tasks", OpKey: "k-copy"})
	var oe *OpKeyError
	if !errors.As(err, &oe) || oe.Key != "k-copy" {
		t.Fatalf("want OpKeyError with key k-copy, got %v", err)
	}
	var api *APIError
	if !errors.As(err, &api) || !CopyOutcomeUnknown(err) {
		t.Fatalf("the wrap must not hide the APIError or the unknown outcome: %v", err)
	}
}

func TestBUG3999_NoKeyNoWrap(t *testing.T) {
	base := errors.New("boom")
	if got := WrapOpKey([]byte(`{"a":1}`), base); got != base {
		t.Fatalf("a body without op_key must not wrap: %v", got)
	}
	if got := WrapOpKey([]byte(`{"op_key":""}`), base); got != base {
		t.Fatalf("an empty op_key must not wrap: %v", got)
	}
	if WrapOpKey([]byte(`{"op_key":"k"}`), nil) != nil {
		t.Fatal("nil stays nil")
	}
}

// Chat's key is the caller's own: a failed send is not wrapped.
func TestBUG3999_ChatSendIsNotWrapped(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"x","message":"m"}}`))
	}))
	defer ts.Close()
	c := newCopyTestClient(t, ts)
	k := "mine"
	_, err := c.SendChatMessage(1, ChatSendInput{Body: "hi", OpKey: &k})
	if err == nil {
		t.Fatal("a 500 must fail")
	}
	var oe *OpKeyError
	if errors.As(err, &oe) {
		t.Fatalf("chat send must not carry an OpKeyError: %v", err)
	}
}
