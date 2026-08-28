package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// rawJSONRequest sends a RAW body string. A test cannot marshal a Go map
// here: marshalling would escape the backslash and send the literal text
// instead of the escape, so every probe would be testing the harmless case.
func rawJSONRequest(srv *Server, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

// escNULLiteral is the six-character JSON escape that decodes to a NUL,
// assembled from bytes. Written as a Go literal it is one backslash away from
// being the NUL itself, and every layer between an editor and the compiler is
// a chance for that to happen silently — which is the whole subject here.
var escNULLiteral = string([]byte{'\\', 'u', '0', '0', '0', '0'})

func TestBodyDecodesNUL(t *testing.T) {
	esc := escNULLiteral
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"plain body, no escape anywhere", `{"title":"hello"}`, false},
		{"escape in a string value", `{"title":"a` + esc + `b"}`, true},
		{"escape as the whole value", `{"title":"` + esc + `"}`, true},
		{"escape in an OBJECT KEY", `{"a` + esc + `b":"v"}`, true},
		{"escape nested in a fields map", `{"fields":{"k":"a` + esc + `b"}}`, true},
		{"escape inside an array element", `{"tags":["ok","a` + esc + `b"]}`, true},

		// The reason this is not a substring search. A doubled backslash is
		// an escaped BACKSLASH followed by the literal characters u0000, so
		// the decoded string holds no NUL. This product stores markdown, and
		// a document explaining JSON escapes is an ordinary thing to write.
		{"doubled backslash decodes to literal text, NOT a NUL",
			`{"content":"write it as \\` + "u0000" + ` in JSON"}`, false},
		{"doubled backslash twice", `{"a":"\\` + "u0000" + `","b":"\\` + "u0000" + `"}`, false},

		// A NUL escape that follows an escaped backslash IS real: the first
		// two backslashes consume each other, so the third begins a fresh
		// escape. The doubled-backslash cases above must not be read as
		// "any preceding backslash makes it safe".
		{"escaped backslash then a REAL escape", `{"a":"\\` + esc + `"}`, true},

		{"other escapes are untouched", `{"a":"tab\there é \n"}`, false},
		{"base64-looking payload with no escape", `{"b":"AQACAAAA"}`, false},

		// Malformed input answers false: the caller's decode reports the JSON
		// error, so this function never has to phrase one.
		{"malformed JSON carrying the escape", `{"a":"` + esc, false},
		{"empty body", ``, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := bodyDecodesNUL([]byte(tc.body)); got != tc.want {
				t.Errorf("bodyDecodesNUL(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

// TestBodyDecodesNULAgreesWithTheDecoder is the differential check: for every
// case above, the verdict must match what actually lands in a decoded Go
// value. Asserting the function against hand-written expectations pins my
// reading of JSON escaping; asserting it against encoding/json pins the
// property the fix depends on.
func TestBodyDecodesNULAgreesWithTheDecoder(t *testing.T) {
	esc := escNULLiteral
	bodies := []string{
		`{"title":"hello"}`,
		`{"title":"a` + esc + `b"}`,
		`{"a` + esc + `b":"v"}`,
		`{"fields":{"k":"a` + esc + `b"}}`,
		`{"tags":["ok","a` + esc + `b"]}`,
		`{"content":"write it as \\` + "u0000" + ` in JSON"}`,
		`{"a":"\\` + esc + `"}`,
		`{"a":"tab\there é \n"}`,
	}
	for _, body := range bodies {
		var v any
		if err := json.Unmarshal([]byte(body), &v); err != nil {
			t.Fatalf("fixture %q does not decode: %v", body, err)
		}
		want := anyHasNUL(v)
		if got := bodyDecodesNUL([]byte(body)); got != want {
			t.Errorf("bodyDecodesNUL(%q) = %v, but the decoded value %s a NUL",
				body, got, map[bool]string{true: "CONTAINS", false: "does not contain"}[want])
		}
	}
}

func anyHasNUL(v any) bool {
	switch t := v.(type) {
	case string:
		return strings.ContainsRune(t, 0)
	case map[string]any:
		for k, sub := range t {
			if strings.ContainsRune(k, 0) || anyHasNUL(sub) {
				return true
			}
		}
	case []any:
		for _, sub := range t {
			if anyHasNUL(sub) {
				return true
			}
		}
	}
	return false
}

// TestDecodeJSONRefusesNULThroughTheHandler exercises the BINDING, not the
// predicate: a real request through the real router, so the test has an
// opinion about whether decodeJSON is what handlers actually call. Its
// control leg is the same request with an ordinary value — without one, a 400
// proves nothing, since a malformed body would produce the same status
// (this filing's original measurement made exactly that mistake).
//
// Runs on SQLite, where the underlying insert would SUCCEED with a truncated
// value rather than error. That is deliberate: on SQLite the refusal can only
// come from this fix, so a green here cannot be the database doing the work.
func TestDecodeJSONRefusesNULThroughTheHandler(t *testing.T) {
	srv := testServer(t)

	rr := rawJSONRequest(srv, "POST", "/api/v1/workspaces/",
		`{"name":"NUL probe","slug":"nulprobe","template":"startup"}`)
	if rr.Code != http.StatusOK && rr.Code != http.StatusCreated {
		t.Fatalf("fixture workspace: %d %s", rr.Code, rr.Body.String())
	}
	rr = rawJSONRequest(srv, "POST", "/api/v1/workspaces/nulprobe/collections/",
		`{"name":"Probes","slug":"probes"}`)
	if rr.Code != http.StatusOK && rr.Code != http.StatusCreated {
		t.Fatalf("fixture collection: %d %s", rr.Code, rr.Body.String())
	}

	const itemsPath = "/api/v1/workspaces/nulprobe/collections/probes/items"
	esc := escNULLiteral

	control := rawJSONRequest(srv, "POST", itemsPath, `{"title":"a-plain-b"}`)
	if control.Code != http.StatusCreated && control.Code != http.StatusOK {
		t.Fatalf("control leg must succeed, got %d: %s", control.Code, control.Body.String())
	}

	for _, tc := range []struct {
		name string
		body string
	}{
		{"title", `{"title":"a` + esc + `b"}`},
		{"content", `{"title":"ok","content":"a` + esc + `b"}`},
		{"fields value", `{"title":"ok","fields":{"k":"a` + esc + `b"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := rawJSONRequest(srv, "POST", itemsPath, tc.body)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), "NUL") {
				t.Errorf("400 body should name the cause, got: %s", rr.Body.String())
			}
		})
	}

	// The doubled-backslash body must still be ACCEPTED end to end. This is
	// the leg that fails if anyone ever "simplifies" the check into a
	// substring search.
	ok := rawJSONRequest(srv, "POST", itemsPath,
		`{"title":"escapes","content":"write it as \\`+"u0000"+` in JSON"}`)
	if ok.Code != http.StatusCreated && ok.Code != http.StatusOK {
		t.Fatalf("literal escape text must be accepted, got %d: %s", ok.Code, ok.Body.String())
	}
}

// TestDecodeJSONKeepsEmptyBodyEOF pins a contract a caller depends on:
// handlers_playbooks.go treats errors.Is(err, io.EOF) as "no arguments
// supplied" and runs the playbook anyway. json.Decoder answered io.EOF on an
// empty body; json.Unmarshal answers a SyntaxError, which that check cannot
// see. Without this, an empty-body playbook run starts returning 400.
func TestDecodeJSONKeepsEmptyBodyEOF(t *testing.T) {
	for _, body := range []string{"", "   ", "\n\t "} {
		req := httptest.NewRequest("POST", "/", strings.NewReader(body))
		var v map[string]any
		err := decodeJSON(req, &v)
		if err == nil {
			t.Fatalf("empty body %q: expected an error", body)
		}
		if !errors.Is(err, io.EOF) {
			t.Errorf("empty body %q: error must wrap io.EOF, got %v", body, err)
		}
	}
}

// TestEveryRequestBodyReaderIsAccountedFor is the completeness claim, made
// ENFORCEABLE rather than asserted — and made honest after codex round 3
// showed the first version could not see past two exact call shapes.
//
// The fix works because a JSON request body reaches the store through
// decodeJSON/decodeJSONWithLimit, which refuse a decoded NUL and apply a size
// cap. The first version of this test scanned for `json.NewDecoder(r.Body)`
// and `io.ReadAll(r.Body)` only, so it was blind to
// `io.ReadAll(io.LimitReader(r.Body, n))` — a shape ALREADY PRESENT in the
// package — and to any alias, helper or future spelling. A completeness test
// that misses a live example is worse than none: it reads as coverage.
//
// So it now scans for the thing that cannot be spelled around — a reference to
// the request body at all — and requires every FILE that touches one to be
// accounted for here with a reason. A new body reader fails this test until
// someone writes down what it does, which is the point: the decision becomes
// deliberate instead of invisible.
//
// It asserts BOTH directions. An unaccounted file fails, because a door may
// have opened. An accounted file that no longer touches a body ALSO fails, so
// the list cannot rot into a set of stale excuses that quietly permits a
// future reader added to the same file.
func TestEveryRequestBodyReaderIsAccountedFor(t *testing.T) {
	accounted := map[string]string{
		"middleware_request_text.go": "the chokepoint itself: readBodyForDecode reads the body under the caller's cap so bodyDecodesNUL can scan it",
		"handlers_import_bundle.go":  "tar.gz bundle import — streams the body through gzip, and its pad-export.json is checked with bodyDecodesNUL before ImportWorkspace",
		"handlers_attachments.go":    "multipart upload — the body is binary blob content, not text, and must NOT be scanned for text validity",
		"artifact_import.go":         "raw artifact TEXT (not JSON) — checked with bindableText, the same predicate ValidatePath and ValidateQuery apply",
		"handlers_cloud.go":          "bodyHasCloudSecret PEEKS at the body and restores it; the real decode still happens through decodeJSON downstream",
		"middleware_mcp_audit.go":    "audit capture — records the body for the MCP audit log and restores it; decoding still happens in the MCP dispatcher",
		"handlers_tokens.go":         "a nil/ContentLength check only — it never reads the body",
	}

	pattern := regexp.MustCompile(`\b(r|req)\.Body\b`)

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	touches := map[string]bool{}
	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		scanned++
		src, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if pattern.Match(src) {
			touches[name] = true
		}
	}

	// The scan must have looked at something, and must have FOUND something.
	// A pattern that silently matched nothing would pass forever.
	if scanned < 20 {
		t.Fatalf("scan looked at only %d non-test .go files; the package is much larger, so the scan is broken", scanned)
	}
	if len(touches) < 3 {
		t.Fatalf("scan found only %d files touching a request body; the pattern is broken", len(touches))
	}

	for name := range touches {
		if _, ok := accounted[name]; !ok {
			t.Errorf("%s reads the request body but is not accounted for in this test. "+
				"A body carrying JSON must go through decodeJSON/decodeJSONWithLimit, which apply "+
				"BUG-2803's NUL refusal and the size cap. If this reader is legitimate, add it here WITH "+
				"the reason it is safe.", name)
		}
	}
	for name, why := range accounted {
		if !touches[name] {
			t.Errorf("%s is accounted for here (%q) but no longer reads a request body — remove the entry "+
				"so it cannot silently cover a future reader added to that file", name, why)
		}
	}
}

// jsonEncode returns s as a JSON string literal — the wire form of a field
// that crosses as a JSON-ENCODED STRING (an item's fields, a collection's
// schema, a workspace's settings). Using encoding/json to build it, rather
// than hand-writing the backslashes, is deliberate: the escaping rules are
// the subject under test and hand-written fixtures would pin my reading of
// them instead of the real ones.
func jsonEncode(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("encode %q: %v", s, err)
	}
	return string(b)
}

// TestBodyDecodesNULNestedDocuments covers codex round 1's P1 on BUG-2803:
// several fields arrive as JSON-ENCODED STRINGS, so the escape survives the
// OUTER decode as literal text and reappears when the destination re-parses
// the string as JSON. Postgres refuses that with SQLSTATE 22P05 (unsupported
// Unicode escape sequence) rather than the 22021 the rest of this family
// produces — a different error precisely because the outer string is pure
// ASCII and never trips the text-encoding check.
func TestBodyDecodesNULNestedDocuments(t *testing.T) {
	esc := escNULLiteral

	innerWithNUL := `{"k":"a` + esc + `b"}`       // decodes to a NUL when parsed
	innerLiteral := `{"k":"a\\` + "u0000" + `b"}` // a doubled backslash: literal text
	innerPlain := `{"k":"plain"}`

	cases := []struct {
		name string
		body string
		want bool
	}{
		{"fields as a JSON-encoded string carrying the escape",
			`{"title":"x","fields":` + jsonEncode(t, innerWithNUL) + `}`, true},
		{"schema as a JSON-encoded string carrying the escape",
			`{"name":"c","schema":` + jsonEncode(t, innerWithNUL) + `}`, true},
		{"settings as a JSON-encoded string carrying the escape",
			`{"name":"w","settings":` + jsonEncode(t, innerWithNUL) + `}`, true},
		{"a JSON-encoded ARRAY carrying the escape",
			`{"tags":` + jsonEncode(t, `["ok","a`+esc+`b"]`) + `}`, true},
		// codex round 4: the BACKSLASH itself can be written as an escape, so
		// the raw body carries no literal six-character sequence anywhere
		// while the outer decode manufactures one inside the nested document.
		// This is the case the old substring fast path let through — measured
		// as a 201 through the real router before the gate became a backslash
		// check.
		{"escape spelled obliquely, via an escaped backslash",
			`{"fields":"{\"k\":\"a` + string([]byte{'\\', 'u', '0', '0', '5', 'c'}) + `u0000b\"}"}`, true},
		{"twice-encoded — a document inside a document",
			`{"fields":` + jsonEncode(t, `{"inner":`+jsonEncode(t, innerWithNUL)+`}`) + `}`, true},

		// Controls. These must stay ACCEPTED: the recursion must not turn
		// "contains the six characters somewhere" into a refusal.
		{"fields as a JSON-encoded string, ordinary content",
			`{"title":"x","fields":` + jsonEncode(t, innerPlain) + `}`, false},
		{"nested document whose escape is a DOUBLED backslash",
			`{"fields":` + jsonEncode(t, innerLiteral) + `}`, false},
		// codex round 6: these fields also accept their NATURAL shape, in
		// which the elements are ordinary strings the server marshals itself.
		// Nothing re-parses them, so an element that merely LOOKS like a
		// document must not be treated as one — this refused a free-form tag
		// whose whole value happened to be JSON.
		{"tags as a natural ARRAY whose element is a JSON document",
			`{"title":"x","tags":["release",` + jsonEncode(t, innerWithNUL) + `]}`, false},
		{"fields as a natural OBJECT whose value is a JSON document",
			`{"title":"x","fields":{"k":` + jsonEncode(t, innerWithNUL) + `}}`, false},
		// ...while the JSON-ENCODED spelling of the same field still is.
		{"tags as a JSON-encoded STRING carrying the escape",
			`{"title":"x","tags":` + jsonEncode(t, `["a`+esc+`b"]`) + `}`, true},
		{"a string that starts like JSON but does not parse",
			`{"content":` + jsonEncode(t, `{"k":"a`+esc+`b"`) + `}`, false},
		{"prose mentioning the escape is not a JSON document",
			`{"content":` + jsonEncode(t, `write a NUL as `+esc+` in JSON`) + `}`, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := bodyDecodesNUL([]byte(tc.body)); got != tc.want {
				t.Errorf("bodyDecodesNUL(%s) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

// TestBodyDecodesNULLeavesTextFieldsAlone is codex round 2's finding on
// BUG-2803, pinned so it cannot come back.
//
// The first version of the nesting check recursed into ANY string that parsed
// as a JSON document. That refused a plain-text `content` value holding a
// JSON snippet which merely MENTIONS the escape — a value this server
// accepted before the fix, stores in a text column that has no problem with
// it, and emits again in an export. Refusing input the server itself produced
// is a worse failure than the narrow door the unscoped recursion closed, so
// the recursion is now scoped to the keys that actually carry JSON documents.
func TestBodyDecodesNULLeavesTextFieldsAlone(t *testing.T) {
	doc := `{"k":"a` + escNULLiteral + `b"}`
	for _, key := range []string{"content", "title", "summary", "body", "description"} {
		body := `{"` + key + `":` + jsonEncode(t, doc) + `}`
		if bodyDecodesNUL([]byte(body)) {
			t.Errorf("%s is a text field: a JSON document in its value must not be re-parsed "+
				"(codex round 2 — the server emits such values in exports and must accept them back)", key)
		}
	}
	// The same document under a JSON-ENCODED key is still refused, so the
	// legs differ only in the key and this is not just "recursion removed".
	if !bodyDecodesNUL([]byte(`{"fields":` + jsonEncode(t, doc) + `}`)) {
		t.Error("under a JSON-encoded key the same document must still be refused")
	}
}

// TestJSONEncodedFieldKeysCoversTheModels keeps jsonEncodedFieldKeys from
// going stale in silence, which is the whole objection to a list-based rule.
// It derives the set from the wire model — a Go string field with a json tag
// whose comment says it holds JSON — and fails when one is not covered. A key
// missing from the list reopens a door; an extra key is harmless (see the
// over-inclusion note at jsonEncodedFieldKeys), so this asserts one direction
// only, deliberately.
func TestJSONEncodedFieldKeysCoversTheModels(t *testing.T) {
	entries, err := os.ReadDir("../models")
	if err != nil {
		t.Fatalf("read models dir: %v", err)
	}
	// A string field, a json tag, and a trailing comment mentioning JSON.
	decl := regexp.MustCompile("string\\s+`json:\"([a-z_]+)[\",][^`]*`\\s*//[^\\n]*JSON")
	found := map[string]string{}
	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		scanned++
		src, err := os.ReadFile(filepath.Join("../models", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, m := range decl.FindAllStringSubmatch(string(src), -1) {
			found[m[1]] = name
		}
	}
	if scanned < 10 || len(found) < 5 {
		t.Fatalf("derivation looks broken: scanned %d files, found %d JSON-encoded fields", scanned, len(found))
	}
	for key, file := range found {
		if !jsonEncodedFieldKeys[key] {
			t.Errorf("%s declares %q as a JSON-encoded string, but it is not in jsonEncodedFieldKeys — "+
				"a NUL escape nested inside it would reach the database unchecked (BUG-2803)", file, key)
		}
	}
}

// TestBodyDecodesNULDepthBound pins the behaviour AT the recursion limit:
// past it the escape is known to be present and the walk has stopped looking,
// so the body is refused rather than passed uninspected.
func TestBodyDecodesNULDepthBound(t *testing.T) {
	// Wrap a NUL-bearing document deeper than the limit allows.
	// Nested under a JSON-ENCODED key at every level, since that is the only
	// path the walk descends: nesting under an ordinary key would never start
	// the recursion and the test would pass for the wrong reason.
	doc := `{"k":"a` + escNULLiteral + `b"}`
	for i := 0; i < maxJSONDocumentNesting+2; i++ {
		doc = `{"fields":` + jsonEncode(t, doc) + `}`
	}
	doc = `{"fields":` + jsonEncode(t, doc) + `}`
	if !bodyDecodesNUL([]byte(doc)) {
		t.Error("a body nested past maxJSONDocumentNesting must be refused, not passed uninspected")
	}
}

// TestDecodeJSONRefusesNestedNULThroughTheHandler is the wiring leg for the
// nested case, on SQLite for the same reason as the single-layer one: there
// the write would SUCCEED, so a green cannot be the database doing the work.
func TestDecodeJSONRefusesNestedNULThroughTheHandler(t *testing.T) {
	srv := testServer(t)

	rr := rawJSONRequest(srv, "POST", "/api/v1/workspaces/",
		`{"name":"Nested","slug":"nested","template":"startup"}`)
	if rr.Code != http.StatusOK && rr.Code != http.StatusCreated {
		t.Fatalf("fixture workspace: %d %s", rr.Code, rr.Body.String())
	}
	rr = rawJSONRequest(srv, "POST", "/api/v1/workspaces/nested/collections/",
		`{"name":"Probes","slug":"probes"}`)
	if rr.Code != http.StatusOK && rr.Code != http.StatusCreated {
		t.Fatalf("fixture collection: %d %s", rr.Code, rr.Body.String())
	}

	const itemsPath = "/api/v1/workspaces/nested/collections/probes/items"

	control := rawJSONRequest(srv, "POST", itemsPath,
		`{"title":"nested control","fields":`+jsonEncode(t, `{"k":"plain"}`)+`}`)
	if control.Code != http.StatusCreated && control.Code != http.StatusOK {
		t.Fatalf("control leg must succeed, got %d: %s", control.Code, control.Body.String())
	}

	bad := rawJSONRequest(srv, "POST", itemsPath,
		`{"title":"nested probe","fields":`+jsonEncode(t, `{"k":"a`+escNULLiteral+`b"}`)+`}`)
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a JSON-encoded fields string carrying the escape, got %d: %s",
			bad.Code, bad.Body.String())
	}
}

// TestTruncateBindableText covers codex round 5's second class on BUG-2803:
// a value that PASSED the body check becomes unbindable on its way to the
// store, because a plain s[:n] can split a rune and leave a partial sequence.
//
// The failure is invisible with ASCII fixtures, which is why four review
// rounds aimed at the input side did not reach it — so these cases are built
// from multi-byte runes deliberately, and each asserts the OUTPUT is valid
// UTF-8 rather than merely short.
func TestTruncateBindableText(t *testing.T) {
	// é is 2 bytes, 中 is 3, 𝄞 is 4 — one case per continuation length, so a
	// walk-back that is off by one cannot pass them all.
	for _, tc := range []struct {
		name  string
		s     string
		limit int
	}{
		{"two-byte rune straddling the cut", strings.Repeat("é", 80), 121},
		{"three-byte rune straddling the cut", strings.Repeat("中", 80), 121},
		{"four-byte rune straddling the cut", strings.Repeat("𝄞", 80), 121},
		{"cut exactly on a boundary", strings.Repeat("é", 80), 120},
		{"ascii", strings.Repeat("a", 300), 120},
		{"shorter than the limit", "café", 120},
		{"limit of zero", "café", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateBindableText(tc.s, tc.limit)
			if len(got) > tc.limit {
				t.Errorf("result is %d bytes, over the %d limit", len(got), tc.limit)
			}
			// The point of the whole helper: the RESULT must still be
			// storable. A plain slice fails exactly here.
			if !bindableText(got) {
				t.Errorf("result is not bindable text: %q", got)
			}
			if !strings.HasPrefix(tc.s, got) {
				t.Errorf("result %q is not a prefix of the input", got)
			}
		})
	}

	// The counterfactual, stated as a test rather than as a comment: the
	// naive slice this replaces really does produce unbindable output for the
	// same input. Without this leg the cases above would pass against an
	// implementation that simply returned the input unchanged when short —
	// they would never demonstrate that anything was wrong.
	s := strings.Repeat("é", 80)
	if bindableText(s[:121]) {
		t.Fatal("the fixture cannot reproduce the defect: a plain byte slice of it is still valid UTF-8, " +
			"so this test would pass against a broken implementation")
	}
}

// TestRequestUserAgentIsBindableText covers codex round 5's third class on
// BUG-2803: the User-Agent header reaches activities.user_agent and
// sessions.user_agent as text, and no rule in this file sees a header.
//
// The disposition here is SANITISE, not refuse — a header is metadata this
// server chose to record, not something the caller asked for, so a malformed
// one must not turn a fine request into a 400. That difference from every
// other check in this file is the thing worth pinning.
func TestRequestUserAgentIsBindableText(t *testing.T) {
	for _, tc := range []struct {
		name string
		ua   string
		want string
	}{
		{"ordinary", "pad-cli/1.0", "pad-cli/1.0"},
		{"non-ascii is preserved", "café/1.0 中", "café/1.0 中"},
		{"invalid UTF-8 is dropped", "pad\xffcli", "padcli"},
		{"NUL is dropped", "pad\x00cli", "padcli"},
		{"empty stays empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			req.Header.Set("User-Agent", tc.ua)
			got := requestUserAgent(req)
			if got != tc.want {
				t.Errorf("requestUserAgent = %q, want %q", got, tc.want)
			}
			if !bindableText(got) {
				t.Errorf("result is not bindable text: %q", got)
			}
		})
	}

	// The counterfactual: the raw header really is unbindable, so these cases
	// would not pass against a helper that simply returned it unchanged.
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("User-Agent", "pad\xffcli")
	if bindableText(req.Header.Get("User-Agent")) {
		t.Fatal("the fixture cannot reproduce the defect: the raw header is already bindable text")
	}
}

// TestBodyDecodesNULGateAgreesWithAnUngatedWalk is the instrument that keeps
// the fast path honest.
//
// The gate is an argument, not an observation: to manufacture the
// six-character NUL escape inside a decoded string, every one of its
// characters must arrive either literally from the raw bytes — in which case
// the raw contains the escape, which begins with \u00 — or from a \u escape
// of its own, and the three characters involved (backslash U+005C, 'u'
// U+0075, '0' U+0030) all sit below U+0100, so every such escape also begins
// with \u00. Hence: no \u00 in the raw bytes, no NUL at any depth, however
// spelled.
//
// The FIRST version of that argument was wrong in exactly this way — it said
// "the escape has only one spelling", which is true inside a decoded string
// and false of the raw bytes, and codex round 4 turned that into a live
// bypass. So the argument does not get to stand on its own reasoning: this
// test runs the gated function against an UNGATED walk over a corpus built to
// attack it, and any disagreement is a bypass.
func TestBodyDecodesNULGateAgreesWithAnUngatedWalk(t *testing.T) {
	ungated := func(raw []byte) bool {
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return false
		}
		return valueDecodesNUL(v, false, 0)
	}

	esc := escNULLiteral                                     // the six-character NUL escape
	bs := string([]byte{'\\', 'u', '0', '0', '5', 'c'})      // escapes to a BACKSLASH
	bsUpper := string([]byte{'\\', 'u', '0', '0', '5', 'C'}) // same, upper-case hex
	uEsc := string([]byte{'\\', 'u', '0', '0', '7', '5'})    // escapes to the letter u
	zero := string([]byte{'\\', 'u', '0', '0', '3', '0'})    // escapes to the digit 0
	doubled := `\\` + "u0000"                                // literal text, no NUL

	corpus := []string{
		`{"title":"plain"}`,
		`{"title":"quotes \" and a backslash \\ but no escape"}`,
		`{"fields":"{\"k\":\"plain\"}"}`,
		`{"fields":"{\"k\":\"a` + esc + `b\"}"}`,
		`{"fields":"{\"k\":\"a` + bs + `u0000b\"}"}`,
		`{"fields":"{\"k\":\"a` + bsUpper + `u0000b\"}"}`,
		`{"fields":"{\"k\":\"a\\` + uEsc + `0000b\"}"}`,
		`{"fields":"{\"k\":\"a\\u00` + zero + `0b\"}"}`,
		`{"fields":"{\"k\":\"a` + doubled + `b\"}"}`,
		`{"content":"{\"k\":\"a` + esc + `b\"}"}`,
		`{"title":"a` + esc + `b"}`,
		`{"a` + esc + `b":"key"}`,
		`{"tags":["ok","a` + esc + `b"]}`,
		`{"fields":"[1,2,\"a` + esc + `b\"]"}`,
		`{"title":"unicode é 中"}`,
		`{"title":"a control escape that is not a NUL: \\u0001"}`,
		`{"fields":"{\"k\":\"ab\"}"}`,
	}

	for i, body := range corpus {
		got, want := bodyDecodesNUL([]byte(body)), ungated([]byte(body))
		if got != want {
			t.Errorf("corpus[%d] %s\n  gated=%v ungated=%v — the fast path is a BYPASS for this input",
				i, body, got, want)
		}
	}

	// The corpus must contain both answers, or agreement proves nothing.
	var trues, falses int
	for _, body := range corpus {
		if ungated([]byte(body)) {
			trues++
		} else {
			falses++
		}
	}
	if trues < 3 || falses < 3 {
		t.Fatalf("corpus is one-sided (%d refuse, %d accept); agreement would be vacuous", trues, falses)
	}
}

// TestDocumentActivityStoresBindableUserAgent is the WIRING leg for the
// User-Agent sanitiser (CONVE-19, and codex round 6's second finding: a unit
// test vouches for the helper, not for anything calling it).
//
// It drives a real request through the router with a malformed header and
// reads the STORED value out of the activities row, so reverting the
// production call site fails this even though the helper still works.
//
// It targets the ACTIVITY sink deliberately. The round-5 enumeration also
// named "sessions.user_agent", and I wired the three login paths to the
// sanitiser before reading store.CreateSession — which HASHES the header and
// stores no text at all. That change was reverted: login would have stored
// sha256(sanitised) while middleware_auth still compares sha256(RAW),
// breaking session validation for any client with a non-UTF-8 User-Agent.
// A sink named in a review is a pointer to verify, not a finding.
func TestDocumentActivityStoresBindableUserAgent(t *testing.T) {
	srv := testServer(t)
	ws := createWSForTest(t, srv)

	post := func(t *testing.T, ua, title string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"title": title})
		req := httptest.NewRequest("POST", "/api/v1/workspaces/"+ws+"/documents/", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", ua)
		req.RemoteAddr = "192.0.2.1:1234"
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK && rr.Code != http.StatusCreated {
			t.Fatalf("create document = %d: %s", rr.Code, rr.Body.String())
		}
	}

	// Control first: an ordinary header must be stored VERBATIM, so a
	// sanitiser that mangled everything would fail here rather than pass.
	post(t, "pad-cli/1.0 café", "control doc")
	post(t, "pad-cli/1.0 bad\xffbyte\x00here", "probe doc")

	rows, err := srv.store.DB().Query(`SELECT user_agent FROM activities WHERE user_agent IS NOT NULL`)
	if err != nil {
		t.Fatalf("read activities: %v", err)
	}
	defer rows.Close()

	var seenControl, seenSanitised int
	for rows.Next() {
		var ua sql.NullString
		if err := rows.Scan(&ua); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if !ua.Valid {
			continue
		}
		if !bindableText(ua.String) {
			t.Errorf("a stored activity user_agent is not bindable text: %q", ua.String)
		}
		switch ua.String {
		case "pad-cli/1.0 café":
			seenControl++
		case "pad-cli/1.0 badbytehere":
			seenSanitised++
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if seenControl == 0 {
		t.Error("the control header was not stored verbatim — the sanitiser is doing too much")
	}
	if seenSanitised == 0 {
		t.Error("no activity carries the sanitised header; the call site is not wired to requestUserAgent")
	}
}
