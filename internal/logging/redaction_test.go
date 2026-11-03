package logging

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestSetupWriterRedactsCredentials(t *testing.T) {
	for _, format := range []string{"json", "text"} {
		t.Run(format, func(t *testing.T) {
			var buf bytes.Buffer
			old := slog.Default()
			t.Cleanup(func() { slog.SetDefault(old) })
			SetupWriter(&buf, "debug", format)
			for _, key := range []string{"token", "access_token", "Password", "api-key", "webhook_secret", "Authorization", "Cookie", "Set-Cookie", "Proxy-Authorization", "X-API-Key"} {
				slog.Info("credentials", key, "credential-fixture")
			}
			slog.Default().With("refresh_token", "credential-fixture").WithGroup("request").Info("grouped",
				slog.Group("headers", "Authorization", "credential-fixture"),
				"request_headers", http.Header{"Cookie": {"credential-fixture"}})
			slog.Info("url failure", "error", fmt.Errorf("fetch failed: %w", &url.Error{Op: "Get", URL: "https://user:credential-fixture@example.com/credential-fixture", Err: errors.New("credential-fixture")}))
			slog.Info("rate limited", "path", "/api/v1/invitations/credential-fixture/preview", "status", 429)
			if strings.Contains(buf.String(), "credential-fixture") {
				t.Error("credentials reached the log")
			}
			if !strings.Contains(buf.String(), "429") || !strings.Contains(buf.String(), "rate limited") {
				t.Error("lost safe diagnostic fields")
			}
		})
	}
}

func TestRedactionHelpers(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"limit=20&%74oken=fixture", "limit=20&%74oken=REDACTED"},
		{"access_token=fixture&refresh-token=fixture&webhook_secret=fixture", "access_token=REDACTED&refresh-token=REDACTED&webhook_secret=REDACTED"},
		{"Authorization=fixture&Cookie=fixture&code=fixture", "Authorization=REDACTED&Cookie=REDACTED&code=REDACTED"},
		{"%invalid=fixture", "REDACTED"},
		{"limit=20;token=fixture", "REDACTED"},
		{"tokenizer=on&limit=20", "tokenizer=on&limit=20"},
		{"redirect=%2Fverify-email%2Ftok123&limit=20", "redirect=%2Fverify-email%2FREDACTED&limit=20"},
		{"redirect=%2Fjoin%2Fabc&next=%2Fitems", "redirect=%2Fjoin%2FREDACTED&next=%2Fitems"},
		{"redirect=%zz", "redirect=REDACTED"},
		{"next=/setup?token=abc&limit=20", "next=%2Fsetup%3Ftoken%3DREDACTED&limit=20"},
		{"redirect=%252Fverify-email%252Ftok", "redirect=%2Fverify-email%2FREDACTED"},
		{"next=%2Fsetup%23token%3Dabc", "next=%2Fsetup%23token%3DREDACTED"},
		{"state=token=abc", "state=token%3DREDACTED"},
		{"user_code=ABCD&recovery_code=x&verification_code=y&request_header=z&status_code=500", "user_code=REDACTED&recovery_code=REDACTED&verification_code=REDACTED&request_header=REDACTED&status_code=500"},
		{"invite_code=x&tokens=y&session_code=z&error_code=404", "invite_code=REDACTED&tokens=REDACTED&session_code=REDACTED&error_code=404"},
	} {
		if got := RedactQuery(tc.raw); got != tc.want {
			t.Errorf("RedactQuery(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
	for _, tc := range []struct{ raw, want string }{
		{"https://user:password@example.com:8443/private?token=fixture#fragment", "example.com:8443"},
		{"https://user:password@[::1]:8443/private", "[::1]:8443"},
		{"https://example.com/%invalid", "REDACTED"},
		{"/relative/credential", "REDACTED"},
	} {
		if got := URLHost(tc.raw); got != tc.want {
			t.Errorf("URLHost(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
	if got := RedactPath("/api/v1/workspaces/demo/items"); got != "/api/v1/workspaces/demo/items" {
		t.Errorf("changed ordinary path: %q", got)
	}
}
