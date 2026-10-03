package cli

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// BUG-3350: the CLI marks its requests, so its sign-ins mint a CLI session,
// which the server never accepts as a browser cookie.
func TestBUG3350_CLIMarksItsSignIn(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Pad-Client")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"padsess_x","user":{"id":"u"}}`))
	}))
	defer srv.Close()
	c := NewClientFromURL(srv.URL)
	if _, err := c.Login("a@example.com", "pw"); err != nil {
		t.Fatal(err)
	}
	if got != "cli" {
		t.Fatalf("login sent X-Pad-Client %q, want \"cli\"", got)
	}
}
