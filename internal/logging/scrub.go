package logging

import (
	"encoding"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// maxScrubBytes bounds the text RedactText examines. Longer text is withheld
// whole: it cannot be examined for the price of a log line (a 64 KiB string
// costs a few tens of milliseconds; a stack dump or a provider body is far
// smaller).
const maxScrubBytes = 64 << 10

// The credential path prefixes, shared by RedactPath (which masks the whole
// tail of a request path) and the text scrubber (which masks one segment).
const credentialPathPrefix = `/(?:api/v1/(?:auth/cli/sessions|invitations|s)|auth/cli|reset-password|verify-email|join|s)/`

var (
	// scheme://userinfo@host: the userinfo is a credential whether or not it
	// has a colon (a token is often the whole userinfo).
	reUserinfo = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.\-]*://)[^/\s?#"']+@`)
	// A credential-bearing HTTP header written out in text: the rest of the line.
	reHeaderLine = regexp.MustCompile(`(?i)(^|[\s"'(\[{,;])((?:proxy-)?authorization|set-cookie|cookie|private-token|api-key|apikey|x-[a-z0-9-]*(?:token|key|secret|auth|csrf|xsrf|session))[ \t]*:[ \t]*[^\r\n]+`)
	reBearer     = regexp.MustCompile(`(?i)\b(bearer)\s+([A-Za-z0-9._~+/=\-]{8,})`)
	reBasic      = regexp.MustCompile(`(?i)\b(basic)\s+([A-Za-z0-9+/]{8,}={0,2})`)
	// Credentials with a recognisable shape, whatever surrounds them: a Pad
	// personal access token, a Pad session token, a JWT, a Stripe key, a
	// GitHub token.
	reShapes = regexp.MustCompile(`\bpad_[0-9a-f]{64,}|\bpad(?:sess|res|ver|wh|app|ic|ctx)_[A-Za-z0-9_\-]{8,}|\beyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]*|\b(?:sk|pk|rk)_(?:live|test)_[A-Za-z0-9]{16,}|\bwhsec_[A-Za-z0-9]{16,}|\bgh[pousr]_[A-Za-z0-9]{36,}`)
	// A credential path that STARTS a path: after whitespace, a quote, or a
	// scheme://host. A directory called s inside a filesystem path
	// (/var/lib/pad/s/pad.db) is not one.
	reTextPath = regexp.MustCompile(`(^|[\s"'(=,\[{]|://[^\s/"']+)(` + credentialPathPrefix + `)[^\s/?#"'<>\])}]+`)
	// A URL query in text: from the ? to the first whitespace, quote, # or
	// closing bracket. RedactQuery, not this file, decides what in it is a
	// credential, so the two cannot drift apart.
	reQuerySpan = regexp.MustCompile(`\?[^\s"'#<>)\]}]+`)
	// strconv and time parse errors repeat their input in quotes, and time's
	// repeats the unparsed remainder a second time. Only those forms: "parsing
	// config.yaml" in an ordinary message is a diagnostic.
	reParsing = regexp.MustCompile(`(strconv\.[A-Za-z]+: parsing|parsing time|cannot parse|Parse[A-Za-z]*\() ?"(?:[^"\\]|\\.)*"`)
)

// RedactText masks credentials in free text: an error message, a response body,
// a log message. It recognises the shapes a credential takes in text (an
// Authorization or Cookie header, a bearer or basic credential, userinfo in a
// URL, a Pad, JWT, Stripe or GitHub token, a credential path, a URL query
// (handed to RedactQuery, so the two share one parser), a key=value or
// "key":"value" pair whose key names a credential, and the input a parse error
// repeats). A secret with none of those shapes and no credential name next to
// it cannot be recognised, so call sites that handle one still use the typed
// helpers (URLHost, ErrorType) or leave it out.
func RedactText(s string) string {
	if s == "" {
		return s
	}
	if len(s) > maxScrubBytes {
		return redacted
	}
	// json.Marshal writes & as \u0026; read it as the separator it is.
	s = strings.ReplaceAll(s, `\u0026`, "&")
	s = reUserinfo.ReplaceAllString(s, "${1}"+redacted+"@")
	s = reHeaderLine.ReplaceAllString(s, "${1}${2}: "+redacted)
	s = reBearer.ReplaceAllStringFunc(s, func(m string) string {
		sub := reBearer.FindStringSubmatch(m)
		// A word after "bearer" in prose ("bearer authentication") is not a token.
		if len(sub[2]) >= 16 || strings.ContainsAny(sub[2], "0123456789") {
			return sub[1] + " " + redacted
		}
		return m
	})
	s = reBasic.ReplaceAllStringFunc(s, func(m string) string {
		sub := reBasic.FindStringSubmatch(m)
		// Basic credentials are base64 of user:password.
		if b, err := base64.StdEncoding.DecodeString(sub[2]); err == nil && strings.Contains(string(b), ":") {
			return sub[1] + " " + redacted
		}
		if b, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(sub[2], "=")); err == nil && strings.Contains(string(b), ":") {
			return sub[1] + " " + redacted
		}
		return m
	})
	s = reShapes.ReplaceAllString(s, redacted)
	s = reTextPath.ReplaceAllString(s, "${1}${2}"+redacted)
	s = reParsing.ReplaceAllStringFunc(s, func(m string) string {
		return m[:strings.IndexByte(m, '"')] + `"` + redacted + `"`
	})
	s = reQuerySpan.ReplaceAllStringFunc(s, func(m string) string {
		// A query has parameters (a=b), a path (?/reset-password/x), or an encoded part.
		// SQL placeholders (?,?,?) and a ? in prose are neither.
		// A path inside it (/) or a percent-encoded part (%2F, %3D) can hide
		// a credential too; plain placeholders and prose cannot.
		if !strings.Contains(m, "=") && !strings.Contains(m, "/") && !escapeSeq.MatchString(m) {
			// A keyless run: placeholders (?,?,?) and short prose stay; an opaque
			// value (six or more token characters, mixed case or digits or long) goes whole.
			if opaqueRun(m[1:]) {
				return "?" + redacted
			}
			return m
		}
		return "?" + RedactQuery(m[1:])
	})
	return redactKeyValues(s)
}

func allDigits(r string) bool {
	for i := 0; i < len(r); i++ {
		if r[i] < '0' || r[i] > '9' {
			return false
		}
	}
	return r != ""
}

// opaqueRun reports a keyless query run that looks like a credential value:
// token characters only, six or more, with a digit or a capital or sixteen or
// more of them. A short lowercase word is prose or a parameter name.
func opaqueRun(r string) bool {
	if len(r) < 6 {
		return false
	}
	letter, mixed := false, false
	for i := 0; i < len(r); i++ {
		c := r[i]
		switch {
		case c >= 'a' && c <= 'z':
			letter = true
		case c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			mixed = true
			letter = letter || c > '9'
		case c == '_' || c == '-' || c == '.' || c == '~':
		default:
			return false
		}
	}
	if !letter {
		// All digits: a six-digit run is a numeric one-time code.
		return allDigits(r)
	}
	return mixed || len(r) >= 16
}

// maxMapKeyBytes bounds a map key before it is scrubbed and stored: a key is a
// name, and the walk's byte budget does not cover it.
const maxMapKeyBytes = 256

// scrubMapKey renders a map key for the log. A key holding a URL is reduced to
// its host (a path can be the credential); any other key is scrubbed as text.
func scrubMapKey(raw string) string {
	if len(raw) > maxMapKeyBytes {
		raw = raw[:maxMapKeyBytes] + "…"
	}
	if strings.Contains(raw, "://") {
		return URLHost(raw)
	}
	return RedactText(raw)
}

// uniqueKey returns name, or name#n for the first n that is free in out. The
// key is at most maxMapKeyBytes, so the probe is cheap.
func uniqueKey(out map[string]any, name string) string {
	if _, taken := out[name]; !taken {
		return name
	}
	for n := 2; ; n++ {
		if c := fmt.Sprintf("%s#%d", name, n); out[c] == nil {
			if _, taken := out[c]; !taken {
				return c
			}
		}
	}
}

// maxKeyLen bounds a key looked for to the left of "=": real names are short,
// and the bound keeps a long run of key characters from costing a rescan.
const maxKeyLen = 64

func isKeyChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '.' || c == '-' || c == '[' || c == ']' || c == '%'
}

// credentialKeyText is credentialKey for a name found in text, where it may be
// percent-encoded (?%74oken=x). An escape that cannot be decoded after valid
// ones is treated as a credential, as RedactQuery treats a key it cannot read;
// a lone % (100%=full) is just text.
func credentialKeyText(key string) bool {
	if !strings.Contains(key, "%") {
		return credentialKey(key)
	}
	decoded, ok := fullyUnescape(key)
	if !ok {
		return escapeSeq.MatchString(key)
	}
	return credentialKey(decoded)
}

func isLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// redactKeyValues masks the value of every pair whose key names a credential:
// key=value anywhere, "key": value when the key is quoted (also inside an
// escaped string, \"key\":\"value\"), and key:value with the value right
// behind the colon, as Go prints a map or struct (map[token:x], {Password:x}).
// A colon followed by a space is prose ("invalid token: expired"), not a pair.
// A pair whose key is not a credential is stepped over at the separator, so a
// credential nested in its value (error=token=x) is still found. One pass,
// linear in the text.
func redactKeyValues(s string) string {
	var out strings.Builder
	last := 0
	for i := 0; i < len(s); i++ {
		if s[i] != '=' && s[i] != ':' {
			continue
		}
		end := i
		for end > 0 && (s[end-1] == ' ' || s[end-1] == '\t') {
			end--
		}
		quoted := false
		if end > 0 && (s[end-1] == '"' || s[end-1] == '\'') {
			quoted = true
			end--
			for end > 0 && s[end-1] == '\\' { // the quote of an escaped string: \"
				end--
			}
		}
		start := end
		for start > 0 && end-start < maxKeyLen && isKeyChar(s[start-1]) {
			start--
		}
		for start < end && !isLetter(s[start]) && s[start] != '%' {
			start++
		}
		if start == end {
			continue
		}
		if s[i] == ':' && !quoted && (i+1 >= len(s) || s[i+1] == ' ' || s[i+1] == '\t' || s[i+1] == '\r' || s[i+1] == '\n') {
			continue
		}
		if !credentialKeyText(s[start:end]) {
			continue
		}
		k := i + 1
		for k < len(s) && (s[k] == ' ' || s[k] == '\t') {
			k++
		}
		if k >= len(s) {
			break
		}
		// from..to is the value to mask; ve is where scanning resumes.
		from := k
		var to, ve int
		switch s[k] {
		case '"', '\'':
			from = k + 1
			ve = from
			for ve < len(s) && s[ve] != s[k] {
				if s[k] == '"' && s[ve] == '\\' {
					ve++
				}
				ve++
			}
			to = min(ve, len(s))
			ve = min(ve+1, len(s))
		case '\\':
			// An escaped quote opens the value: \"value\"; the value ends at the next one.
			j := k
			for j < len(s) && s[j] == '\\' {
				j++
			}
			if j < len(s) && s[j] == '"' {
				from = j + 1
				c := from
				for c < len(s) && !(s[c] == '"' && s[c-1] == '\\') {
					c++
				}
				to = min(c, len(s))
				for to > from && s[to-1] == '\\' {
					to--
				}
				ve = min(c+1, len(s))
			} else {
				to = unquotedEnd(s, k)
				ve = to
			}
		case '{', '[':
			// A structured value ({"token":{"value":"x"}}, "tokens":["a","b"]) is
			// masked whole, to its matching bracket.
			to = skipBracketed(s, k)
			ve = to
		default:
			to = unquotedEnd(s, k)
			ve = to
		}
		if to <= from || tokenCount(s[start:end], s[from:to]) || benignCode(s[start:end], s[from:to]) {
			continue
		}
		out.WriteString(s[last:from])
		out.WriteString(redacted)
		last = to
		i = ve - 1
	}
	out.WriteString(s[last:])
	return out.String()
}

// unquotedEnd is the end of a bare value: up to whitespace or a delimiter.
func unquotedEnd(s string, k int) int {
	e := k
	for e < len(s) && !strings.ContainsRune(" \t\r\n,&;\"'}]", rune(s[e])) {
		e++
	}
	return e
}

// grpcCodes are the gRPC status names: a word-shaped one-time code ("Abcdef")
// must not pass as one, so these are listed, not matched by shape.
var grpcCodes = map[string]bool{
	"OK": true, "Ok": true, "Canceled": true, "Cancelled": true, "Unknown": true, "InvalidArgument": true,
	"DeadlineExceeded": true, "NotFound": true, "AlreadyExists": true, "PermissionDenied": true,
	"ResourceExhausted": true, "FailedPrecondition": true, "Aborted": true, "OutOfRange": true,
	"Unimplemented": true, "Internal": true, "Unavailable": true, "DataLoss": true, "Unauthenticated": true,
}

// benignCode reports a bare code that is a status and not a one-time code: a
// number of up to three digits (404, -1, an exit code), a gRPC status name
// (NotFound). One-time codes are four or more digits or any other string; the OAuth ?code= in a URL query
// is decided by RedactQuery, not here. Punctuation that ends a sentence or
// closes a bracket is not part of the value.
func benignCode(key, val string) bool {
	if strings.ToLower(keySeparators.Replace(key)) != "code" {
		return false
	}
	val = strings.TrimRight(val, ").,]}!?;:")
	if val == "" || len(val) > 40 {
		return false
	}
	if digits := strings.TrimPrefix(val, "-"); len(digits) <= 3 && digits != "" && strings.Trim(digits, "0123456789") == "" {
		return true
	}
	return grpcCodes[val]
}

// tokenCount reports a count of tokens (max_tokens=4096, input_tokens=5): a
// plural name with a bare number. A singular token with a number is a credential.
func tokenCount(key, val string) bool {
	k := strings.ToLower(keySeparators.Replace(key))
	if !strings.HasSuffix(k, "tokens") || val == "" {
		return false
	}
	for i := 0; i < len(val); i++ {
		if val[i] < '0' || val[i] > '9' {
			return false
		}
	}
	return true
}

// skipBracketed returns the index just past the bracket that closes the one at
// s[k], honouring JSON strings, or len(s) when it never closes.
func skipBracketed(s string, k int) int {
	depth, inStr := 0, false
	for i := k; i < len(s); i++ {
		c := s[i]
		if inStr {
			if c == '\\' {
				i++
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return len(s)
}

const (
	maxWalkNodes = 2000
	maxMapRead   = 4 * maxWalkWidth
	maxWalkDepth = 6
	maxWalkWidth = 256
)

// walkable reports whether v is a composite (or a named string) that
// redactAttr should inspect.
func walkable(v any) bool {
	switch reflect.ValueOf(v).Kind() {
	case reflect.Map, reflect.Struct, reflect.Slice, reflect.Array, reflect.Pointer, reflect.String:
		return true
	}
	return false
}

// numericSecretKey reports a name that holds a one-time code or a PIN, which
// is a number: slog.Int("otp", 482913) is a credential. A bare "code" counts
// too, unless its value is a status (see smallCode).
func numericSecretKey(key string) bool {
	switch strings.ToLower(keySeparators.Replace(key)) {
	case "otp", "totp", "pin", "passcode", "code", "verificationcode", "authcode", "mfacode", "smscode", "resetcode", "securitycode":
		return true
	}
	return false
}

// statusCodeNames are the names that hold a status or exit code, not a secret.
var statusCodeNames = []string{"statuscode", "exitcode", "returncode", "errorcode", "errcode", "grpccode"}

// smallCode reports a status or exit code: the bare key code, or a name on
// statusCodeNames, with a value of up to three digits (as benignCode accepts in
// text). verification_code=123 is not one: a one-time code keeps the credential rule.
func smallCode(key string, n int64) bool {
	if n <= -1000 || n >= 1000 {
		return false
	}
	k := strings.ToLower(keySeparators.Replace(key))
	if k == "code" {
		return true
	}
	for _, c := range statusCodeNames {
		if strings.HasSuffix(k, c) {
			return true
		}
	}
	return false
}

// scalar reports whether v is a number or bool: a count or a measurement,
// never a credential, whatever its field is called (Tokens int, input_tokens).
func scalar(v reflect.Value) bool {
	for v.Kind() == reflect.Interface || v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return false
		}
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	}
	return false
}

// numericSecret reports a number or bool under a credential name: slog.Int("otp", 482913),
// {"password": 123456}, Secret bool. Two exceptions keep diagnostics: a count
// of tokens (input_tokens=5) and a bare code that is a status (code=404).
func numericSecret(key string, v reflect.Value) bool {
	for v.Kind() == reflect.Interface || v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return false
		}
		v = v.Elem()
	}
	var n int64
	isInt := false
	switch v.Kind() {
	case reflect.Bool:
	case reflect.Float32, reflect.Float64:
		// A JSON-decoded number is a float64: a whole one is a count or a code.
		if f := v.Float(); f == float64(int64(f)) && f > -1<<40 && f < 1<<40 {
			n, isInt = int64(f), true
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, isInt = v.Int(), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, isInt = int64(min(v.Uint(), 1<<40)), true
	default:
		return false
	}
	if !credentialKey(key) && !numericSecretKey(key) {
		return false
	}
	if isInt && (smallCode(key, n) || pluralName(key)) {
		return false
	}
	return true
}

// countNames are the plurals of the credential nouns the name rule knows. A
// number under one of them is the size of a collection (sessions=3), because a
// plural names many credentials and holds none; the singular (token, password,
// secret, api_key, pin) is not in the list and still redacts a number. The list
// is closed: a name that is not on it keeps the credential rule.
var countNames = []string{"tokens", "passwords", "secrets", "apikeys", "cookies", "sessions", "headers",
	"credentials", "tickets", "signatures", "otps", "pins"}

// pluralName reports a name whose last word is one of countNames.
func pluralName(key string) bool {
	k := strings.ToLower(keySeparators.Replace(key))
	for _, c := range countNames {
		if strings.HasSuffix(k, c) {
			return true
		}
	}
	return false
}

// stringValue returns the string v holds (through interfaces and pointers).
func stringValue(v reflect.Value) (string, bool) {
	for v.Kind() == reflect.Interface || v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return "", false
		}
		v = v.Elem()
	}
	if v.Kind() == reflect.String {
		return v.String(), true
	}
	return "", false
}

// maxWalkBytes bounds the text a walk scrubs in total: 2000 nodes of 64 KiB
// each would cost half a gigabyte of scanning for one log line.
const maxWalkBytes = 512 << 10

// walkBudget is what one walk may still spend.
type walkBudget struct{ nodes, bytes int }

// text scrubs one string out of the walk's byte budget; past it, a string is
// withheld.
func (b *walkBudget) text(s string) string {
	if len(s) > b.bytes {
		b.bytes = 0
		return redacted
	}
	b.bytes -= len(s)
	return RedactText(s)
}

// urlText is text for a url-named value: reduced to its host, out of the same
// byte budget as any other string.
func (b *walkBudget) urlText(s string) string {
	if len(s) > b.bytes {
		b.bytes = 0
		return redacted
	}
	b.bytes -= len(s)
	return RedactText(reduceURLText(s))
}

// sanitizeValue is sanitize with a fresh budget.
func sanitizeValue(v reflect.Value) any {
	budget := walkBudget{nodes: maxWalkNodes, bytes: maxWalkBytes}
	return sanitize(v, 0, &budget)
}

// sanitize copies v with every credential-named entry masked and every string
// scrubbed, bounded in depth, width and total nodes so a hostile, cyclic or
// fanned-out value costs a fixed amount. Unexported struct fields are left out.
// A value that marshals itself is scrubbed in its marshalled form.
func sanitize(v reflect.Value, depth int, budget *walkBudget) any {
	budget.nodes--
	if depth > maxWalkDepth || budget.nodes < 0 {
		return redacted
	}
	for {
		// Look for a self-rendering value at every level, pointer included: an
		// error or marshaller often has its methods on the pointer type.
		if v.CanInterface() && !(v.Kind() == reflect.Pointer && v.IsNil()) {
			switch x := v.Interface().(type) {
			case *url.URL:
				// A webhook path is often the credential: keep the host, as the
				// url-named string attributes do.
				return URLHost(x.String())
			case url.URL:
				return URLHost(x.String())
			case *http.Cookie:
				return x.Name + "=" + redacted
			case http.Cookie:
				return x.Name + "=" + redacted
			case error:
				if isURLError(x) {
					return ErrorType(x)
				}
				return budget.text(safeErrorText(x))
			case json.Marshaler:
				return budget.text(marshalledText(func() ([]byte, error) { return json.Marshal(x) }, true))
			case encoding.TextMarshaler:
				return budget.text(marshalledText(x.MarshalText, false))
			case fmt.Stringer:
				// A type that chose how it prints (a masked key, a clock time) keeps it.
				return budget.text(safeStringText(x))
			}
		}
		if v.Kind() != reflect.Interface && v.Kind() != reflect.Pointer {
			break
		}
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
		depth++
		if depth > maxWalkDepth {
			return redacted
		}
	}
	switch v.Kind() {
	case reflect.String:
		return budget.text(v.String())
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		if v.CanInterface() {
			return v.Interface()
		}
		return nil
	case reflect.Slice, reflect.Array:
		if v.Type().Elem() == reflect.TypeOf(byte(0)) {
			if v.Len() > maxScrubBytes || v.Len() > budget.bytes {
				return redacted
			}
			b := make([]byte, v.Len())
			reflect.Copy(reflect.ValueOf(b), v)
			if !utf8.Valid(b) {
				// Binary (a hash, a key): its bytes are not text to scrub.
				return fmt.Sprintf("<%d bytes>", len(b))
			}
			return budget.text(string(b))
		}
		n := v.Len()
		if n > maxWalkWidth {
			n = maxWalkWidth
		}
		out := make([]any, 0, n+1)
		for i := 0; i < n; i++ {
			out = append(out, sanitize(v.Index(i), depth+1, budget))
		}
		if v.Len() > n {
			out = append(out, fmt.Sprintf("… %d more", v.Len()-n))
		}
		return out
	case reflect.Map:
		type entry struct {
			name string
			val  reflect.Value
		}
		// Read a bounded prefix: a map of a million entries is not
		// materialised and sorted to log its first few hundred.
		entries := make([]entry, 0, min(v.Len(), maxMapRead))
		for it := v.MapRange(); it.Next() && len(entries) < maxMapRead; {
			// fmt reports a key whose String method panics rather than panicking.
			// A string key is cut before it is formatted; fmt would copy it whole.
			var raw string
			k := it.Key()
			for k.Kind() == reflect.Interface && !k.IsNil() {
				k = k.Elem()
			}
			// A plain string, or a named string too long to be a name (its String
			// method is not consulted), is cut here; a short named string keeps
			// its String form through fmt.
			if k.Kind() == reflect.String && (k.Type() == reflect.TypeOf("") || k.Len() > maxMapKeyBytes) {
				raw = k.String()
				if len(raw) > maxMapKeyBytes {
					raw = raw[:maxMapKeyBytes+1]
				}
			} else if k.Type().Size() > maxMapKeyBytes {
				// An array or struct key too large to be a name: its type stands for it.
				raw = "<" + k.Type().String() + ">"
			} else {
				raw = fmt.Sprint(it.Key().Interface())
			}
			// A key's bytes count toward the walk, as a value's do.
			cost := min(len(raw), maxMapKeyBytes)
			if budget.bytes < cost {
				break
			}
			budget.bytes -= cost
			entries = append(entries, entry{scrubMapKey(raw), it.Value()})
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
		out := make(map[string]any, min(len(entries), maxWalkWidth+1))
		for i, e := range entries {
			if i == maxWalkWidth {
				break
			}
			// Scrubbing can make two keys equal; keep both entries.
			name := uniqueKey(out, e.name)
			if credentialKey(e.name) && !scalar(e.val) || numericSecret(e.name, e.val) {
				out[name] = redacted
				continue
			}
			if sv, ok := stringValue(e.val); ok && urlNamed(e.name) {
				out[name] = budget.urlText(sv)
				continue
			}
			out[name] = sanitize(e.val, depth+1, budget)
		}
		if v.Len() > maxWalkWidth {
			out[uniqueKey(out, "…")] = fmt.Sprintf("%d more", v.Len()-maxWalkWidth)
		}
		return out
	case reflect.Struct:
		t := v.Type()
		out := make(map[string]any, t.NumField())
		for i := 0; i < t.NumField(); i++ {
			sf := t.Field(i)
			if !sf.IsExported() {
				continue
			}
			name, tag := sf.Name, strings.Split(sf.Tag.Get("json"), ",")[0]
			if tag == "-" {
				continue
			}
			if tag != "" {
				name = tag
			}
			f := v.Field(i)
			if (credentialKey(sf.Name) || credentialKey(name)) && !scalar(f) || numericSecret(name, f) || numericSecret(sf.Name, f) {
				out[name] = redacted
				continue
			}
			if sv, ok := stringValue(f); ok && (urlNamed(name) || urlNamed(sf.Name)) {
				out[name] = budget.urlText(sv)
				continue
			}
			out[name] = sanitize(v.Field(i), depth+1, budget)
		}
		return out
	default:
		// A func, chan or unsafe pointer has no value to show.
		return "<" + v.Type().String() + ">"
	}
}

// marshalledText renders a self-marshalling value as text (not yet scrubbed).
// A JSON string literal is unquoted, so a time or an address keeps its plain form.
func marshalledText(marshal func() ([]byte, error), isJSON bool) (text string) {
	defer func() {
		if recover() != nil {
			text = "<unmarshalable>"
		}
	}()
	b, err := marshal()
	if err != nil || len(b) > maxScrubBytes {
		return "<unmarshalable>"
	}
	t := string(b)
	if isJSON && strings.HasPrefix(t, `"`) {
		if u, uerr := strconv.Unquote(t); uerr == nil {
			t = u
		}
	}
	return t
}

// safeStringText is x.String() that survives a nil receiver or a panicking method.
func safeStringText(x fmt.Stringer) (text string) {
	defer func() {
		if recover() != nil {
			text = "<unprintable>"
		}
	}()
	return x.String()
}

// isURLError reports whether err's chain holds a *url.Error. A typed-nil error
// whose Unwrap dereferences its receiver must not take a log call down.
func isURLError(err error) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	var u *url.Error
	return errors.As(err, &u)
}

// safeErrorText is err.Error() that survives a nil receiver or a panicking
// method: a log call must never take the process down.
func safeErrorText(err error) (text string) {
	defer func() {
		if recover() != nil {
			text = "<error text unavailable>"
		}
	}()
	return err.Error()
}
