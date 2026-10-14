package server

import (
	"strings"
	"testing"
)

// BUG-3669: default-src does not govern form submission, so the SPA policy
// let a form that reached rendered content post to any origin. The share page
// is served by the same SPA handler, so its response must carry
// form-action 'self'.
func TestBUG3669_SharePageCSPRestrictsFormAction(t *testing.T) {
	srv := testServer(t)
	srv.SetWebUI(webFS(map[string]string{"index.html": "<!doctype html><html><head></head></html>"}))
	for _, path := range []string{"/s/some-token", "/"} {
		rr := doRequest(srv, "GET", path, nil)
		csp := rr.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "script-src-attr 'none'") {
			t.Fatalf("%s: premise: not the SPA policy (%d): %q", path, rr.Code, csp)
		}
		if !strings.Contains(csp, "form-action 'self'") {
			t.Errorf("%s: SPA CSP has no form-action 'self': %q", path, csp)
		}
	}
}
