package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDeleteAttachment_SendsDeleteToAttachmentPath(t *testing.T) {
	var method, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.EscapedPath()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	c := isolatedClient(t, srv.URL)

	if err := c.DeleteAttachment("demo", "a/1"); err != nil {
		t.Fatalf("DeleteAttachment: %v", err)
	}
	if method != http.MethodDelete || path != "/api/v1/workspaces/demo/attachments/a%2F1" {
		t.Errorf("got %s %s, want DELETE /api/v1/workspaces/demo/attachments/a%%2F1", method, path)
	}
}

func TestDeleteAttachment_NotFoundIsNamed(t *testing.T) {
	for name, body := range map[string]string{
		"json envelope": `{"error":{"code":"not_found","message":"nope"}}`,
		"empty body":    ``,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(body))
		}))
		c := isolatedClient(t, srv.URL)
		err := c.DeleteAttachment("demo", "gone")
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), "attachment gone not found in workspace demo") {
			t.Errorf("%s: err = %v, want a named not-found", name, err)
		}
		if HTTPStatus(err) != http.StatusNotFound {
			t.Errorf("%s: HTTPStatus = %d, want 404", name, HTTPStatus(err))
		}
	}
}

func TestDeleteAttachment_OtherErrorsPassThrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"not yours"}}`))
	}))
	t.Cleanup(srv.Close)
	c := isolatedClient(t, srv.URL)
	if err := c.DeleteAttachment("demo", "a-1"); err == nil || err.Error() != "not yours" {
		t.Errorf("err = %v, want the server's message", err)
	}
}

func TestDeleteAttachment_RefusesDotSegments(t *testing.T) {
	srv, seen := recordingServer(t)
	c := isolatedClient(t, srv.URL)
	for _, id := range []string{".", ".."} {
		if err := c.DeleteAttachment("demo", id); err == nil {
			t.Errorf("DeleteAttachment(%q) = nil, want a refusal", id)
		}
	}
	if got := seen(); len(got) != 0 {
		t.Errorf("sent %v for a dot-segment id", got)
	}
}
