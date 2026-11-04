package logging

import (
	"encoding"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// maxScrubBytes bounds the text RedactText examines. Longer text is withheld
// whole: it cannot be examined for the price of a log line.
const maxScrubBytes = 256 << 10

// The credential path prefixes, shared by RedactPath (which masks the whole
// tail of a request path) and the text scrubber (which masks one segment).
const credentialPathPrefix = `/(?:api/v1/(?:auth/cli/sessions|invitations|s)|auth/cli|reset-password|verify-email|join|s)/`

var (
	// scheme://userinfo@host: the userinfo is a credential whether or not it
	// has a colon (a token is often the whole userinfo).
	reUserinfo = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.\-]*://)[^/\s?#@"']+@`)
	// A credential-bearing HTTP header written out in text: the rest of the line.
	reHeaderLine = regexp.MustCompile(`(?i)(^|[\s"'(\[{,;])((?:proxy-)?authorization|set-cookie|cookie|x-api-key)[ \t]*:[ \t]*[^\r\n]+`)
	reBearer     = regexp.MustCompile(`(?i)\b(bearer)\s+([A-Za-z0-9._~+/=\-]{8,})`)
	reBasic      = regexp.MustCompile(`(?i)\b(basic)\s+([A-Za-z0-9+/]{8,}={0,2})`)
	// Credentials with a recognisable shape, whatever surrounds them: a Pad
	// personal access token, a Pad session token, a JWT, a Stripe key, a
	// GitHub token.
	reShapes = regexp.MustCompile(`\bpad_[0-9a-f]{64}\b|\bpadsess_[A-Za-z0-9_\-]{8,}|\beyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]*|\b(?:sk|pk|rk)_(?:live|test)_[A-Za-z0-9]{16,}|\bwhsec_[A-Za-z0-9]{16,}|\bgh[pousr]_[A-Za-z0-9]{36,}`)
	// A credential path that STARTS a path: after whitespace, a quote, or a
	// scheme://host. A directory called s inside a filesystem path
	// (/var/lib/pad/s/pad.db) is not one.
	reTextPath = regexp.MustCompile(`(^|[\s"'(=,\[{]|://[^\s/"']+)(` + credentialPathPrefix + `)[^\s/?#"'<>\])}]+`)
	// strconv and time parse errors repeat their input in quotes, and time's
	// repeats the unparsed remainder a second time.
	reParsing = regexp.MustCompile(`(parsing(?: time)?|cannot parse|Parse[A-Za-z]*\() ?"(?:[^"\\]|\\.)*"`)
)

// RedactText masks credentials in free text: an error message, a response body,
// a log message. It recognises the shapes a credential takes in text (an
// Authorization or Cookie header, a bearer or basic credential, userinfo in a
// URL, a Pad, JWT, Stripe or GitHub token, a credential path, a key=value or
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
	return redactKeyValues(s)
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
// key=value anywhere, and "key": value when the key is quoted. A colon after
// an unquoted word is prose ("invalid token: expired"), not a pair. A pair
// whose key is not a credential is stepped over at the separator, so a
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
		}
		start := end
		for start > 0 && end-start < maxKeyLen && isKeyChar(s[start-1]) {
			start--
		}
		for start < end && !isLetter(s[start]) && s[start] != '%' {
			start++
		}
		if start == end || (s[i] == ':' && !quoted) {
			continue
		}
		// Inside a URL's query a bare key= and state= are credentials too.
		inQuery := start > 0 && (s[start-1] == '?' || s[start-1] == '&')
		if !(credentialKeyText(s[start:end]) || (inQuery && queryCredentialKey(s[start:end]))) {
			continue
		}
		k := i + 1
		for k < len(s) && (s[k] == ' ' || s[k] == '\t') {
			k++
		}
		if k >= len(s) {
			break
		}
		quote, ve := "", k
		switch s[k] {
		case '"':
			quote = `"`
			ve = k + 1
			for ve < len(s) && s[ve] != '"' {
				if s[ve] == '\\' {
					ve++
				}
				ve++
			}
			ve++
		case '\'':
			quote = "'"
			ve = k + 1
			for ve < len(s) && s[ve] != '\'' {
				ve++
			}
			ve++
		case '{', '[':
			// A structured value ({"token":{"value":"x"}}, "tokens":["a","b"]) is
			// masked whole, to its matching bracket.
			ve = skipBracketed(s, k)
		default:
			for ve < len(s) && !strings.ContainsRune(" \t\r\n,&;\"'}]", rune(s[ve])) {
				ve++
			}
		}
		if ve > len(s) {
			ve = len(s)
		}
		if ve == k {
			continue
		}
		out.WriteString(s[last:k])
		out.WriteString(quote + redacted + quote)
		last = ve
		i = ve - 1
	}
	out.WriteString(s[last:])
	return out.String()
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

// sanitizeValue is sanitize with a fresh node budget.
func sanitizeValue(v reflect.Value) any {
	budget := maxWalkNodes
	return sanitize(v, 0, &budget)
}

// sanitize copies v with every credential-named entry masked and every string
// scrubbed, bounded in depth, width and total nodes so a hostile, cyclic or
// fanned-out value costs a fixed amount. Unexported struct fields are left out.
// A value that marshals itself is scrubbed in its marshalled form.
func sanitize(v reflect.Value, depth int, budget *int) any {
	*budget--
	if depth > maxWalkDepth || *budget < 0 {
		return redacted
	}
	for {
		// Look for a self-rendering value at every level, pointer included: an
		// error or marshaller often has its methods on the pointer type.
		if v.CanInterface() && !(v.Kind() == reflect.Pointer && v.IsNil()) {
			switch x := v.Interface().(type) {
			case error:
				if isURLError(x) {
					return ErrorType(x)
				}
				return RedactText(safeErrorText(x))
			case json.Marshaler:
				return marshalledText(func() ([]byte, error) { return json.Marshal(x) }, true)
			case encoding.TextMarshaler:
				return marshalledText(x.MarshalText, false)
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
		return RedactText(v.String())
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		if v.CanInterface() {
			return v.Interface()
		}
		return nil
	case reflect.Slice, reflect.Array:
		if v.Type().Elem() == reflect.TypeOf(byte(0)) {
			if v.Len() > maxScrubBytes {
				return redacted
			}
			b := make([]byte, v.Len())
			reflect.Copy(reflect.ValueOf(b), v)
			if !utf8.Valid(b) {
				// Binary (a hash, a key): its bytes are not text to scrub.
				return fmt.Sprintf("<%d bytes>", len(b))
			}
			return RedactText(string(b))
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
			entries = append(entries, entry{fmt.Sprint(it.Key().Interface()), it.Value()})
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
		out := make(map[string]any, min(len(entries), maxWalkWidth+1))
		for i, e := range entries {
			if i == maxWalkWidth {
				break
			}
			if credentialKey(e.name) && !scalar(e.val) {
				out[e.name] = redacted
				continue
			}
			out[e.name] = sanitize(e.val, depth+1, budget)
		}
		if v.Len() > maxWalkWidth {
			out["…"] = fmt.Sprintf("%d more", v.Len()-maxWalkWidth)
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
			if (credentialKey(sf.Name) || credentialKey(name)) && !scalar(v.Field(i)) {
				out[name] = redacted
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

// marshalledText renders a self-marshalling value as scrubbed text. A JSON
// string literal is unquoted, so a time or an address keeps its plain form.
func marshalledText(marshal func() ([]byte, error), isJSON bool) (text any) {
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
	return RedactText(t)
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
