package logging

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
)

const redacted = "REDACTED"

var keySeparators = strings.NewReplacer("_", "", "-", "", " ", "")

// credentialKey also recognises compound names such as access_token,
// webhook_secret and Proxy-Authorization, regardless of case or separators.
func credentialKey(key string) bool {
	key = strings.ToLower(keySeparators.Replace(key))
	for _, suffix := range []string{"token", "password", "secret", "apikey", "authorization", "cookie", "headers"} {
		if strings.HasSuffix(key, suffix) {
			return true
		}
	}
	return key == "code" || key == "headers" || key == "header"
}

var credentialPath = regexp.MustCompile(`(?s)(/(?:api/v1/(?:auth/cli/sessions|invitations|s)|auth/cli|reset-password|verify-email|join|s)/).+`)

// RedactPath removes credential-bearing path tails, including malformed and
// unmatched requests for which a router has no route template yet.
// URL.Path is already decoded: escaped ?, # and newlines are path data too.
func RedactPath(path string) string {
	return credentialPath.ReplaceAllString(path, "${1}"+redacted)
}

// RedactQuery preserves diagnostic parameters and their order. Decode keys
// before checking them so percent-encoding cannot bypass credential redaction.
func RedactQuery(raw string) string {
	parts := strings.Split(raw, "&")
	for i, part := range parts {
		key, _, hasValue := strings.Cut(part, "=")
		decoded, err := url.QueryUnescape(key)
		if err != nil || strings.Contains(part, ";") {
			return redacted
		}
		if hasValue && credentialKey(decoded) {
			parts[i] = key + "=" + redacted
		}
	}
	return strings.Join(parts, "&")
}

// URLHost deliberately drops userinfo, path, query and fragment: URL secrets
// need not have recognisable names (a webhook path is often the credential).
// Never return parse errors here; they can contain the original input.
func URLHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return redacted
	}
	return u.Host
}

// ErrorType retains the failure's type without rendering untrusted error text.
// HTTP and conversion errors can repeat URLs, response bodies or credentials.
func ErrorType(err error) string {
	return fmt.Sprintf("%T", err)
}

// redactAttr is a backstop for structured application logs, including With
// attributes and groups. Call sites handling external input should still use
// the helpers so they are safe with a custom slog handler as well.
func redactAttr(groups []string, a slog.Attr) slog.Attr {
	for _, group := range groups {
		if credentialKey(group) {
			return slog.String(a.Key, redacted)
		}
	}
	if credentialKey(a.Key) {
		return slog.String(a.Key, redacted)
	}
	if err, ok := a.Value.Any().(error); ok {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return slog.String(a.Key, ErrorType(err))
		}
	}
	if a.Value.Kind() != slog.KindString {
		return a
	}
	switch strings.ToLower(a.Key) {
	case "path", "reset_path":
		a.Value = slog.StringValue(RedactPath(a.Value.String()))
	case "query":
		a.Value = slog.StringValue(RedactQuery(a.Value.String()))
	case "url", "base_url", "source_url", "redirect_url":
		a.Value = slog.StringValue(URLHost(a.Value.String()))
	}
	return a
}
