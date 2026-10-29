package server

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func captureRedactionLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

func TestRequestLogsRedactCredentials(t *testing.T) {
	for _, path := range []string{
		"/s/credential-fixture", "/api/v1/s/credential-fixture/attachments/id",
		"/reset-password/credential-fixture", "/verify-email/credential-fixture",
		"/auth/cli/credential-fixture", "/api/v1/auth/cli/sessions/credential-fixture/approve",
		"/join/credential-fixture", "/api/v1/invitations/credential-fixture/preview",
		"/s/%63redential-fixture/not-found",
		"/s/%3Fcredential-fixture", "/s/%23credential-fixture", "/s/%0Acredential-fixture",
	} {
		t.Run(path, func(t *testing.T) {
			logs := captureRedactionLogs(t)
			req := httptest.NewRequest("GET", path+"?access_token=credential-fixture&%74oken=credential-fixture&webhook_secret=credential-fixture&Authorization=credential-fixture&Cookie=credential-fixture&limit=20", nil)
			req.Header.Set("Authorization", "Bearer credential-fixture")
			req.Header.Set("Cookie", "session=credential-fixture")
			StructuredLogger(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })).ServeHTTP(httptest.NewRecorder(), req)
			if strings.Contains(logs.String(), "credential-fixture") {
				t.Error("request credentials reached the log")
			}
			if !strings.Contains(logs.String(), `"status":404`) || !strings.Contains(logs.String(), "limit=20") {
				t.Error("lost safe request diagnostics")
			}
		})
	}
}
