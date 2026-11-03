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
	"time"
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

// A hostile query that nests "?" thousands of times must cost one bounded
// pass, not one regexp scan and one stack frame per level.
func TestRedactQueryNestingIsBounded(t *testing.T) {
	raw := strings.Repeat("a=?", 20000)
	done := make(chan string, 1)
	go func() { done <- RedactQuery(raw) }()
	select {
	case got := <-done:
		if !strings.Contains(got, "REDACTED") {
			t.Errorf("deeply nested query was not cut off: %.60q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RedactQuery on a deeply nested query did not return in 5s")
	}
}

func TestRedactQueryOpaqueEncodingIsRedacted(t *testing.T) {
	enc := func(s string, n int) string {
		for i := 0; i < n; i++ {
			s = url.QueryEscape(s)
		}
		return s
	}
	// A reset path encoded past the decode limit, as a value and as a key.
	val := "next=" + enc("/reset-password/SECRET", 20)
	if got := RedactQuery(val); strings.Contains(got, "SECRET") || got != "next=REDACTED" {
		t.Errorf("over-encoded value not redacted: %.60q", got)
	}
	key := enc("token", 20) + "=SECRET"
	if got := RedactQuery(key); strings.Contains(got, "SECRET") {
		t.Errorf("over-encoded key leaked: %.60q", got)
	}
	// Within the limit it is still decoded and the diagnostics survive.
	if got := RedactQuery("next=" + enc("/reset-password/SECRET", 5)); strings.Contains(got, "SECRET") {
		t.Errorf("5x-encoded value leaked: %q", got)
	}
}

func TestRedactQueryBadEscapeAfterValidEncoding(t *testing.T) {
	for _, raw := range []string{
		"next=%252Freset-password%252FSECRET%25zz",
		"next=%253Ftoken%253DSECRET%25zz",
	} {
		if got := RedactQuery(raw); strings.Contains(got, "SECRET") {
			t.Errorf("RedactQuery(%q) leaked: %q", raw, got)
		}
	}
}

func TestCredentialKeyForms(t *testing.T) {
	for _, k := range []string{"user[password]", "filter[token]", "recovery_codes", "invite_codes", "token[]", "access_token[0]", "Backup-Codes"} {
		if !credentialKey(k) {
			t.Errorf("credentialKey(%q) = false", k)
		}
	}
	for _, k := range []string{"status_code", "error_codes", "limit", "tokenizer", "items[]"} {
		if credentialKey(k) {
			t.Errorf("credentialKey(%q) = true", k)
		}
	}
}

func TestRedactAttrKeepsNumericCounts(t *testing.T) {
	for _, a := range []slog.Attr{slog.Int("input_tokens", 12), slog.Int("access_tokens", 3), slog.Int("http_status_code", 502)} {
		if got := redactAttr(nil, a); got.Value.Kind() != slog.KindInt64 || got.Value.Int64() != a.Value.Int64() {
			t.Errorf("%s was redacted: %v", a.Key, got)
		}
	}
	if got := redactAttr(nil, slog.String("access_token", "x")); got.Value.String() != "REDACTED" {
		t.Errorf("string credential kept: %v", got)
	}
}

func TestRedactionHelpers(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"limit=20&%74oken=fixture", "limit=20&%74oken=REDACTED"},
		{"access_token=fixture&refresh-token=fixture&webhook_secret=fixture", "access_token=REDACTED&refresh-token=REDACTED&webhook_secret=REDACTED"},
		{"Authorization=fixture&Cookie=fixture&code=fixture", "Authorization=REDACTED&Cookie=REDACTED&code=REDACTED"},
		{"%invalid=fixture", "REDACTED"},
		{"limit=20;token=fixture", "limit=20;token=REDACTED"},
		{"tokenizer=on&limit=20", "tokenizer=on&limit=20"},
		{"redirect=%2Fverify-email%2Ftok123&limit=20", "redirect=%2Fverify-email%2FREDACTED&limit=20"},
		{"redirect=%2Fjoin%2Fabc&next=%2Fitems", "redirect=%2Fjoin%2FREDACTED&next=%2Fitems"},
		{"redirect=%zz", "redirect=REDACTED"},
		{"next=/setup?token=abc&limit=20", "next=%2Fsetup%3Ftoken%3DREDACTED&limit=20"},
		{"redirect=%252Fverify-email%252Ftok", "redirect=%2Fverify-email%2FREDACTED"},
		{"next=%2Fsetup%23token%3Dabc", "next=%2Fsetup%23token%3DREDACTED"},
		{"next=%2Fx%23%2Freset-password%2FSECRET", "next=%2Fx%23%2Freset-password%2FREDACTED"},
		{"next=%2Fx%3F%2Fjoin%2Fabc", "next=%2Fx%3F%2Fjoin%2FREDACTED"},
		{"redirect=%25252525252Fverify-email%25252525252Ftok", "redirect=%2Fverify-email%2FREDACTED"},
		{"secret_key=a&access_key=b&otp=c&passwd=d&credentials=e", "secret_key=REDACTED&access_key=REDACTED&otp=REDACTED&passwd=REDACTED&credentials=REDACTED"},
		{"return_to=token=abc", "return_to=token%3DREDACTED"},
		{"%2Fverify-email%2Ftok", "%2Fverify-email%2FREDACTED"},
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
