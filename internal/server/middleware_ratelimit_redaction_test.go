package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The rate limiter logs the request path when it refuses a request, and some
// paths carry a credential (an invitation code, a CLI handoff code, a share
// token). Each limiter's log call redacts the path; this drives real limiters
// to a 429 on credential paths and checks the log.
func TestRateLimitLogsRedactCredentialPaths(t *testing.T) {
	for _, path := range []string{
		"/api/v1/invitations/credential-fixture/preview",
		"/api/v1/auth/cli/sessions/credential-fixture",
		"/api/v1/s/credential-fixture",
	} {
		t.Run(path, func(t *testing.T) {
			srv := testServer(t)
			logs := captureRedactionLogs(t)

			limited := false
			for i := 0; i < 400 && !limited; i++ {
				req := httptest.NewRequest(http.MethodGet, path, nil)
				req.RemoteAddr = "10.0.0.7:1234"
				w := httptest.NewRecorder()
				srv.ServeHTTP(w, req)
				limited = w.Code == http.StatusTooManyRequests
			}
			if !limited {
				t.Fatal("never reached a 429: the rate-limit log leg was not exercised")
			}
			if !strings.Contains(logs.String(), "rate limited") {
				t.Fatal("the limiter refused the request but logged nothing")
			}
			if strings.Contains(logs.String(), "credential-fixture") {
				t.Errorf("a credential reached the rate-limit log: %s", logs.String())
			}
		})
	}
}
