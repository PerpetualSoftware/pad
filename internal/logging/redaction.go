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
	// A plural (tokens, passwords, cookies) names the same credential.
	for _, k := range []string{key, strings.TrimSuffix(key, "s")} {
		for _, suffix := range []string{"token", "password", "secret", "apikey", "authorization", "cookie", "header",
			"signature", "jwt", "ticket", "verifier", "sessionid",
			"secretkey", "accesskey", "privatekey", "passwd", "otp", "credential"} {
			if strings.HasSuffix(k, suffix) {
				return true
			}
		}
	}
	// Any *code names a one-time credential (invite, recovery, verification,
	// device, user, totp...) unless it is a known diagnostic code.
	if strings.HasSuffix(key, "code") {
		switch key {
		case "statuscode", "errorcode", "exitcode", "httpcode", "responsecode", "returncode", "countrycode", "languagecode":
			return false
		}
		return true
	}
	return false
}

// redactQueryValue masks credentials nested inside a non-credential query
// value: a credential path (redirect=/verify-email/<token>), a nested query
// (next=/setup?token=x) or either of those percent-encoded more than once.
func redactQueryValue(rawValue string, depth int) (string, bool) {
	decoded, err := url.QueryUnescape(rawValue)
	if err != nil {
		return redacted, true
	}
	for i := 0; i < 16 && strings.Contains(decoded, "%"); i++ {
		next, nerr := url.QueryUnescape(decoded)
		if nerr != nil {
			break
		}
		decoded = next
	}
	// The value may be a path with a nested query (?) or fragment (#) whose
	// key=value pairs are redacted like a query, or a bare key=value pair.
	var out string
	if q := strings.IndexAny(decoded, "?#"); q >= 0 {
		out = RedactPath(decoded[:q]) + decoded[q:q+1] + redactQueryDepth(RedactPath(decoded[q+1:]), depth+1)
	} else if strings.Contains(decoded, "=") {
		out = redactQueryDepth(RedactPath(decoded), depth+1)
	} else {
		out = RedactPath(decoded)
	}
	if out == decoded {
		return rawValue, false
	}
	return url.QueryEscape(out), true
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
	return redactQueryDepth(raw, 0)
}

// maxQueryNesting bounds the recursion on nested query values. Real redirect
// values nest once or twice; deeper input is hostile, and redacting it whole
// keeps the cost of an unauthenticated request linear.
const maxQueryNesting = 4

func redactQueryDepth(raw string, depth int) string {
	if depth > maxQueryNesting {
		return redacted
	}
	parts := strings.Split(raw, "&")
	for i, part := range parts {
		key, _, hasValue := strings.Cut(part, "=")
		decoded, err := url.QueryUnescape(key)
		if err != nil || strings.Contains(part, ";") {
			return redacted
		}
		if !hasValue {
			// A bare part can be a credential path: ?/verify-email/<token>.
			if masked, changed := redactQueryValue(part, depth); changed {
				parts[i] = masked
			}
			continue
		}
		if credentialKey(decoded) {
			parts[i] = key + "=" + redacted
			continue
		}
		// A value can itself carry a credential path, e.g.
		// redirect=/verify-email/<token> on the login page.
		_, val, _ := strings.Cut(part, "=")
		if masked, changed := redactQueryValue(val, depth); changed {
			parts[i] = key + "=" + masked
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
	// A number, bool, duration or time is a count or a measurement, never a
	// credential (input_tokens, access_tokens swept, http_status_code).
	switch a.Value.Kind() {
	case slog.KindInt64, slog.KindUint64, slog.KindFloat64, slog.KindBool, slog.KindDuration, slog.KindTime:
		return a
	}
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
