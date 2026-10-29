package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type redactionImportTransport struct{}

func (redactionImportTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("redirect https://other.example/error-credential?password=error-password")
}

func TestImportLogsRedactCredentials(t *testing.T) {
	withLocalFetcher(t)
	pkgFetcher.Transport = redactionImportTransport{}
	logs := captureRedactionLogs(t)
	req := httptest.NewRequest("POST", "/api/v1/import/url", strings.NewReader(`{"url":"https://url-user:url-password@example.com/path-credential?api_key=query-credential#fragment-credential"}`))
	rr := httptest.NewRecorder()
	(&Server{}).handleImportURL(rr, req)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", rr.Code)
	}
	for _, secret := range []string{"url-user", "url-password", "path-credential", "query-credential", "fragment-credential", "error-credential", "error-password"} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("%s reached import logs", secret)
		}
	}
	if !strings.Contains(logs.String(), "import url fetch failed") {
		t.Error("missing failure diagnostic")
	}
}

func TestPasswordResetLogsRedactCredentials(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "admin@test.com", "Admin")
	logs := captureRedactionLogs(t)
	rr := doRequest(srv, "POST", "/api/v1/auth/forgot-password", map[string]string{"email": "admin@test.com"})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if strings.Contains(logs.String(), "reset_path") || strings.Contains(logs.String(), "/reset-password/") {
		t.Error("password reset credential reached the log")
	}
	if !strings.Contains(logs.String(), "email not configured") {
		t.Error("missing no-email diagnostic")
	}
}
