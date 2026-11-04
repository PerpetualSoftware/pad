package logging

import (
	"fmt"
	"log/slog"
	"net/url"
	"reflect"
	"regexp"
	"strings"
)

const redacted = "REDACTED"

var keySeparators = strings.NewReplacer("_", "", "-", "", " ", "", ".", "")

// credentialKey also recognises compound names such as access_token,
// webhook_secret and Proxy-Authorization, regardless of case or separators.
func credentialKey(key string) bool {
	// token[], access_token[0] and user[password] are bracket forms: check the
	// name and every bracketed segment.
	for _, seg := range strings.FieldsFunc(key, func(r rune) bool { return r == '[' || r == ']' }) {
		if credentialName(seg) {
			return true
		}
	}
	return credentialName(key)
}

func credentialName(key string) bool {
	key = strings.ToLower(keySeparators.Replace(key))
	// token_1, token_2 (an indexed name) name the same credential as token.
	key = strings.TrimRight(key, "0123456789")
	// A plural (tokens, passwords, cookies, recovery_codes) names the same credential.
	for _, k := range []string{key, strings.TrimSuffix(key, "s")} {
		for _, suffix := range []string{"token", "password", "secret", "apikey", "authorization", "cookie", "header",
			"signature", "jwt", "ticket", "verifier", "sessionid",
			"secretkey", "accesskey", "privatekey", "passwd", "otp", "credential", "csrf", "xsrf",
			"passphrase", "encryptionkey", "signingkey", "hmackey", "masterkey", "sessionkey", "authkey",
			"tokenvalue", "session", "sid"} {
			if strings.HasSuffix(k, suffix) {
				return true
			}
		}
		// password_confirmation, passwordConfirm: the name starts with the word;
		// private_key_pem: it names a private key somewhere inside.
		if strings.HasPrefix(k, "password") || strings.Contains(k, "privatekey") {
			return true
		}
		if exactCredentialNames[k] {
			return true
		}
	}
	// Any *code names a one-time credential (invite, recovery, verification,
	// device, user, totp...) unless it is a known diagnostic code.
	for _, k := range []string{key, strings.TrimSuffix(key, "s")} {
		if !strings.HasSuffix(k, "code") {
			continue
		}
		switch k {
		case "statuscode", "errorcode", "exitcode", "httpcode", "responsecode", "returncode", "countrycode", "languagecode",
			"decode", "encode", "unicode", "zipcode", "postcode", "barcode", "opcode", "bytecode", "sourcecode":
			continue
		}
		return true
	}
	return false
}

// exactCredentialNames are short names that are credentials only as a whole:
// a suffix or prefix test would catch author, authority, signal, passenger,
// sidecar and session_count.
var exactCredentialNames = map[string]bool{
	"auth": true, "sig": true, "sid": true, "session": true, "pwd": true, "pass": true,
	"idtokenhint": true, "xauth": true, "pin": true,
}

// queryCredentialKey is credentialKey for the parameters of a query string. A
// bare key= or state= is a credential there (an API key; an OAuth state), where
// the same words are ordinary diagnostics as slog attribute names (a field key,
// a limiter key, a workflow state).
func queryCredentialKey(key string) bool {
	if credentialKey(key) {
		return true
	}
	switch strings.ToLower(keySeparators.Replace(key)) {
	case "key", "state":
		return true
	}
	return false
}

const maxUnescapePasses = 16

var escapeSeq = regexp.MustCompile(`%[0-9A-Fa-f]{2}`)

// fullyUnescape decodes s until no escape remains. It reports false when the
// input is malformed, or is still encoded after maxUnescapePasses: text that
// cannot be examined cannot be shown, so callers redact it whole. A literal
// "%" that appears only after a decode (not a valid escape) is plain data.
func fullyUnescape(s string) (string, bool) {
	cur, err := url.QueryUnescape(s)
	if err != nil {
		return "", false
	}
	for i := 0; i < maxUnescapePasses && strings.Contains(cur, "%"); i++ {
		next, nerr := url.QueryUnescape(cur)
		if nerr != nil {
			// A bad escape after valid ones (%252F...%25zz) must not hide the
			// still-encoded text before it.
			if escapeSeq.MatchString(cur) {
				return "", false
			}
			return cur, true
		}
		if next == cur {
			return cur, true
		}
		cur = next
	}
	// The pass limit was reached; a valid escape that still decodes is opaque.
	if next, nerr := url.QueryUnescape(cur); nerr == nil && next != cur {
		return "", false
	}
	return cur, true
}

// redactQueryValue masks credentials nested inside a non-credential query
// value: a credential path (redirect=/verify-email/<token>), a nested query
// (next=/setup?token=x) or either of those percent-encoded more than once.
func redactQueryValue(rawValue string, depth int) (string, bool) {
	decoded, ok := fullyUnescape(rawValue)
	if !ok {
		return redacted, true
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

var credentialPath = regexp.MustCompile(`(?s)(` + credentialPathPrefix + `).+`)

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
	// A semicolon separates parameters for a legacy server, so it separates
	// them here: a credential after one is masked and the parameters before it
	// stay readable. The separators are kept in place.
	var out strings.Builder
	for len(raw) > 0 {
		i := strings.IndexAny(raw, "&;")
		part, sep := raw, ""
		if i >= 0 {
			part, sep = raw[:i], raw[i:i+1]
			raw = raw[i+1:]
		} else {
			raw = ""
		}
		masked, ok := redactQueryPart(part, depth)
		if !ok {
			return redacted
		}
		out.WriteString(masked)
		out.WriteString(sep)
	}
	return out.String()
}

// redactQueryPart masks one key=value parameter. It reports false when the
// part cannot be examined, and the whole query is then withheld.
func redactQueryPart(part string, depth int) (string, bool) {
	key, val, hasValue := strings.Cut(part, "=")
	decoded, ok := fullyUnescape(key)
	if !ok {
		return "", false
	}
	if !hasValue {
		// A bare part can be a credential path: ?/verify-email/<token>.
		if masked, changed := redactQueryValue(part, depth); changed {
			return masked, true
		}
		return part, true
	}
	if queryCredentialKey(decoded) {
		return key + "=" + redacted, true
	}
	// A value can itself carry a credential path, e.g.
	// redirect=/verify-email/<token> on the login page.
	if masked, changed := redactQueryValue(val, depth); changed {
		return key + "=" + masked, true
	}
	return part, true
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
//
// It masks, in order: an attribute (or group) whose NAME is a credential; an
// error, reduced to its type when it is a *url.Error and otherwise scrubbed as
// text; a map, struct or slice, walked entry by entry; and every string,
// including the message, scrubbed as text.
func redactAttr(groups []string, a slog.Attr) slog.Attr {
	// A number, bool, duration or time is a count or a measurement, never a
	// credential (input_tokens, access_tokens swept, http_status_code).
	switch a.Value.Kind() {
	case slog.KindInt64, slog.KindUint64, slog.KindFloat64, slog.KindBool, slog.KindDuration, slog.KindTime:
		// A one-time code or PIN is a number too: slog.Int("otp", 482913).
		if a.Value.Kind() == slog.KindInt64 && numericSecretKey(a.Key) && !smallCode(a.Key, a.Value.Int64()) ||
			a.Value.Kind() == slog.KindUint64 && numericSecretKey(a.Key) && !smallCode(a.Key, int64(min(a.Value.Uint64(), 1<<40))) {
			return slog.String(a.Key, redacted)
		}
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
	if a.Value.Kind() == slog.KindAny {
		switch v := a.Value.Any().(type) {
		case nil:
			return a
		case error:
			if isURLError(v) {
				return slog.String(a.Key, ErrorType(v))
			}
			return slog.String(a.Key, RedactText(safeErrorText(v)))
		default:
			// The source attribute is a *slog.Source, not data to walk.
			if _, isSource := v.(*slog.Source); isSource {
				return a
			}
			if walkable(v) {
				return slog.Any(a.Key, sanitizeValue(reflect.ValueOf(v)))
			}
		}
		return a
	}
	if a.Value.Kind() != slog.KindString {
		return a
	}
	if k := strings.ToLower(a.Key); strings.HasSuffix(k, "url") && len(k) > 3 {
		// webhook_url, callbackUrl, http.url: a URL attribute is reduced to its
		// host. A value with no / or ? in it (failureUrl=timeout) is plain text and stays.
		if v := a.Value.String(); strings.ContainsAny(v, "/?") {
			a.Value = slog.StringValue(URLHost(v))
		}
	}
	switch strings.ToLower(a.Key) {
	case "path", "reset_path":
		a.Value = slog.StringValue(RedactPath(a.Value.String()))
	case "query":
		a.Value = slog.StringValue(RedactQuery(a.Value.String()))
	case "url", "base_url", "source_url", "redirect_url":
		a.Value = slog.StringValue(URLHost(a.Value.String()))
	}
	// Whatever the key, a string can carry a credential in its text. This is
	// also what covers the log message, which slog passes here as "msg".
	a.Value = slog.StringValue(RedactText(a.Value.String()))
	return a
}
