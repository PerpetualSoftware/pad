package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/artifact"
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
		"middleware_mcp_audit.go":    "audit capture — parses the body ITSELF and binds the decoded method / params.name to mcp_audit_log.tool_name, so it is a second READER, not a pass-through. That the MCP dispatcher decodes the body again is true and says nothing about what this middleware persists — the earlier rationale here made exactly that mistake and certified it safe (codex round 20). parseMCPRequestBody now runs both caller-derived returns through sanitiseStoredText",
		"handlers_tokens.go":         "a nil/ContentLength check only — it never reads the body",
		"handlers_oauth.go":          "KNOWN GAP, tracked as BUG-2811: the OAuth handlers read FORM-encoded bodies (r.Form/FormValue), which no rule in this family covers — the transport rules see the query half of r.Form and not the body half. Listed so this test states the gap instead of being blind to it; measuring it needs a fosite-backed fixture.",
	}

	// FormValue/PostFormValue/ParseForm/ParseMultipartForm read the request
	// BODY too, and the first version of this scan looked only for .Body — so
	// it reported full coverage while the OAuth form-body handlers, which
	// BUG-2811 tracks, were entirely invisible to it (codex round 13). A
	// completeness test with a blind spot is worse than none, because it
	// reads as coverage.
	// AST, not a regex. Three defects in the lexical version, all found by
	// codex round 22, all in the direction that matters for a test whose job
	// is to say nothing is invisible:
	//
	//   - it recognised only the names `r` and `req`, so a handler holding
	//     its request as `httpReq` or `orig` was INVISIBLE;
	//   - it matched inside COMMENTS, so prose could make a file look scanned;
	//   - broadening it to any identifier (my first attempt at this fix)
	//     matched every unrelated `.Body` field — input.Body, comment.Body,
	//     fetched.Body — and the only route to green was listing five files
	//     that read no request body at all, which would then have HIDDEN real
	//     readers later added to them. A false accounting entry is worse than
	//     a missing one, so that attempt was abandoned rather than tuned.
	//
	// Keying on the TYPE fixes all three: find every identifier declared as
	// *http.Request in a function signature, then look for reader selectors on
	// exactly those identifiers. Names are irrelevant, comments are not part
	// of the AST, and `.Body` on anything else is not a match.
	readerSelectors := map[string]bool{
		"Body": true, "FormValue": true, "PostFormValue": true, "ParseForm": true,
		"ParseMultipartForm": true, "MultipartForm": true, "PostForm": true,
	}
	isRequestPtr := func(e ast.Expr) bool {
		star, ok := e.(*ast.StarExpr)
		if !ok {
			return false
		}
		sel, ok := star.X.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Request" {
			return false
		}
		pkg, ok := sel.X.(*ast.Ident)
		return ok && pkg.Name == "http"
	}
	fileReadsRequestBody := func(path string) bool {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		found := false
		ast.Inspect(f, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				return true
			}
			reqNames := map[string]bool{}
			collect := func(fl *ast.FieldList) {
				if fl == nil {
					return
				}
				for _, fld := range fl.List {
					if !isRequestPtr(fld.Type) {
						continue
					}
					for _, nm := range fld.Names {
						reqNames[nm.Name] = true
					}
				}
			}
			collect(fn.Type.Params)
			collect(fn.Recv)
			if len(reqNames) == 0 {
				return true
			}
			ast.Inspect(fn.Body, func(m ast.Node) bool {
				sel, ok := m.(*ast.SelectorExpr)
				if !ok || !readerSelectors[sel.Sel.Name] {
					return true
				}
				if id, ok := sel.X.(*ast.Ident); ok && reqNames[id.Name] {
					found = true
				}
				return true
			})
			return true
		})
		return found
	}

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
		if fileReadsRequestBody(filepath.Join(".", name)) {
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

	// KNOWN LIMITS, stated because this test's whole value is a completeness
	// claim, and an unqualified one is how the MCP audit reader came to be
	// certified safe while persisting a decoded NUL (codex round 20).
	//
	//  1. Accounting is per FILE, not per call site. Once a file is listed, a
	//     NEW reader added to it is covered by the existing entry and this
	//     test stays green. The reason strings are what a reviewer checks
	//     against; they are not permanent exemptions.
	//  2. Only signature-declared requests are seen. A request stashed in a
	//     struct field or captured by a closure is not a parameter, so this
	//     scan does not find it.
	//
	// Both want per-call-site accounting, which is a different instrument.
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
		// Depth 2 under an ORDINARY key is ACCEPTED, and that is a measured
		// decision rather than a gap: the handler parses `fields` once, so
		// the inner text is re-escaped when the blob is written and Postgres
		// never sees an escape. Probed on Postgres 17 with the check
		// disabled — the same body imports 201. Only the document Postgres
		// itself parses can carry a fatal one (codex round 7).
		{"twice-encoded under an ordinary key is safe",
			`{"fields":` + jsonEncode(t, `{"inner":`+jsonEncode(t, innerWithNUL)+`}`) + `}`, false},
		// ...but a JSON-ENCODED key INSIDE the document still recurses, so
		// this is a key rule applied at every level, not a depth limit.
		// ...and a key that LOOKS like a wire key inside caller data is not
		// one: a collection may declare a user field named `schema`, so the
		// list is consulted only outside caller data (codex round 8).
		{"a user field named like a wire key does not restart the descent",
			`{"fields":` + jsonEncode(t, `{"schema":`+jsonEncode(t, innerWithNUL)+`}`) + `}`, false},

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

// TestBodyDecodesNULDescendsExactlyOneLevel replaces an earlier depth-bound
// test. The walk no longer carries a depth counter, because it no longer
// needs one: the key list is consulted only outside caller data, so a
// document reached through a listed key is walked with the list disabled and
// nothing below it can start a second descent.
//
// This pins that property directly rather than pinning a bound, and it is the
// test that fails if someone reintroduces flag inheritance: an escape one
// level below the parsed document must be ACCEPTED (measured safe on Postgres
// — the inner text is re-escaped when the blob is written), while the same
// escape IN that document must be refused.
func TestBodyDecodesNULDescendsExactlyOneLevel(t *testing.T) {
	inner := `{"k":"a` + escNULLiteral + `b"}`

	atTheParsedLayer := `{"fields":` + jsonEncode(t, inner) + `}`
	if !bodyDecodesNUL([]byte(atTheParsedLayer)) {
		t.Error("an escape in the document Postgres parses must be refused")
	}

	oneDeeper := `{"fields":` + jsonEncode(t, `{"inner":`+jsonEncode(t, inner)+`}`) + `}`
	if bodyDecodesNUL([]byte(oneDeeper)) {
		t.Error("an escape BELOW the parsed document is safe and must be accepted")
	}

	// A user field named like a wire key must not restart the descent.
	shadowed := `{"fields":` + jsonEncode(t, `{"schema":`+jsonEncode(t, inner)+`}`) + `}`
	if bodyDecodesNUL([]byte(shadowed)) {
		t.Error("a user field named `schema` inside a fields blob is caller data, not a wire key")
	}
	natural := `{"fields":{"schema":` + jsonEncode(t, inner) + `}}`
	if bodyDecodesNUL([]byte(natural)) {
		t.Error("a user field named `schema` in a NATURAL fields object is caller data too")
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
			// And it must keep as much as the limit allows. Without this,
			// an implementation returning "" for every input passes every
			// assertion above — it is bindable, within the limit, and a
			// prefix (codex round 13).
			if len(tc.s) <= tc.limit {
				if got != tc.s {
					t.Errorf("input fits the limit and must be returned unchanged, got %q", got)
				}
			} else if lost := tc.limit - len(got); lost >= 4 {
				t.Errorf("dropped %d bytes to respect a %d-byte limit; at most one rune (max 4 bytes) "+
					"should be lost to the boundary", lost, tc.limit)
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
		return valueDecodesNUL(v, false)
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

// TestBodyDecodesNULUnderAJSONEncodedKeyChecksBothWays is codex round 9's P1,
// which was a REGRESSION this branch introduced in its round-8 restructure.
//
// Taking the JSON-encoded branch for a listed key used to skip the plain
// "does this string contain a NUL" check, so a direct NUL in a `fields` value
// — the very first door this change closed — was accepted again. The two
// questions are different and both must be asked: does the string ITSELF
// carry a NUL, and does the document it carries contain an escape.
func TestBodyDecodesNULUnderAJSONEncodedKeyChecksBothWays(t *testing.T) {
	esc := escNULLiteral

	// A direct escape in the fields STRING (not inside a nested document):
	// the outer decode turns it into a real NUL in that string.
	direct := `{"title":"x","fields":"a` + esc + `b"}`
	if !bodyDecodesNUL([]byte(direct)) {
		t.Error("a NUL in the fields string itself must be refused")
	}

	// And the nested form still is, so this is not just the plain check.
	nested := `{"title":"x","fields":` + jsonEncode(t, `{"k":"a`+esc+`b"}`) + `}`
	if !bodyDecodesNUL([]byte(nested)) {
		t.Error("an escape inside the fields document must be refused")
	}

	// Control: an ordinary fields string is accepted, so the legs above are
	// not passing because everything under a listed key is refused.
	if bodyDecodesNUL([]byte(`{"title":"x","fields":"plain"}`)) {
		t.Error("an ordinary fields string must be accepted")
	}
}

// TestArtifactBindableTextAcceptsLiteralEscapeText is codex round 9's P2: the
// artifact check searched the MARSHALLED bytes for the escape, and a value
// holding the six LITERAL characters marshals to a doubled backslash which
// still contains that sequence — so valid content was refused. Artifacts are
// documentation; text about a JSON escape is exactly what one carries.
func TestArtifactBindableTextAcceptsLiteralEscapeText(t *testing.T) {
	esc := escNULLiteral
	ok := artifact.Artifact{
		Title: "Escapes",
		Body:  "write a NUL as " + esc + " in JSON",
		Fields: map[string]any{
			"note": "also " + esc + " here",
		},
	}
	if !artifactIsBindableText(ok) {
		t.Error("literal escape TEXT in an artifact is valid content and must be accepted")
	}

	// The counterfactual: a real NUL in the same places is still refused, so
	// the leg above is not passing because the check does nothing.
	for name, bad := range map[string]artifact.Artifact{
		"title":  {Title: "a\x00b"},
		"body":   {Title: "t", Body: "a\x00b"},
		"fields": {Title: "t", Fields: map[string]any{"k": "a\x00b"}},
	} {
		if artifactIsBindableText(bad) {
			t.Errorf("a real NUL in %s must be refused", name)
		}
	}
}

// TestTestEmailRefusesUnusableBody is the handler-level leg codex round 13
// found missing. Reverting handlers_admin.go to `err != nil || input.To == ""`
// — which defaulted EVERY decode failure to the admin's own address — passed
// every other test in this file, because they only exercise decodeJSON.
//
// The distinction being pinned is between two things that used to collapse
// into one: an ABSENT body legitimately means "send it to me", while a body
// that is present and REFUSED must not be reinterpreted as that default.
func TestTestEmailRefusesUnusableBody(t *testing.T) {
	srv := testServer(t)
	token := bootstrapFirstUser(t, srv, "admin-mail@example.com", "Admin")
	mock := mockMailerooEndpoint(t, http.StatusOK, true)
	configureEmailForTest(srv, mock.URL, "http://localhost:7777")

	post := func(t *testing.T, body string) *httptest.ResponseRecorder {
		t.Helper()
		var r io.Reader
		if body != "" {
			r = strings.NewReader(body)
		}
		req := httptest.NewRequest("POST", "/api/v1/admin/test-email", r)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.RemoteAddr = "127.0.0.1:1234"
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		return rr
	}

	// Control 1: no body at all still means "send it to me".
	if rr := post(t, ""); rr.Code != http.StatusOK {
		t.Fatalf("absent body must still send, got %d: %s", rr.Code, rr.Body.String())
	}
	// Control 2: an ordinary body still works.
	if rr := post(t, `{"to":"someone@example.com"}`); rr.Code != http.StatusOK {
		t.Fatalf("ordinary body must send, got %d: %s", rr.Code, rr.Body.String())
	}
	// The case: a body that is valid JSON but carries an unusable value.
	rr := post(t, `{"to":"a`+escNULLiteral+`b@example.com"}`)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("a refused body must answer 400, not be reinterpreted as the default recipient; got %d: %s",
			rr.Code, rr.Body.String())
	}
}

// TestTextSafeHelpersAreUsedAtEveryCallSite closes the gap codex round 14
// named: reverting a single call site back to the unsafe form leaves every
// behavioural test green, because the surviving fixtures are ASCII and the
// helpers' own unit tests do not care who calls them.
//
// Testing each site behaviourally would need a fixture per site (an OAuth
// connection, a cloud login, four audit paths). This asserts the WIRING
// statically instead — the same technique as
// TestEveryRequestBodyReaderIsAccountedFor, and the same bargain: a reverted
// call site fails here immediately, and a new one has to be decided on
// deliberately rather than added invisibly.
//
// It asserts in both directions: an unsafe form anywhere fails, and finding
// none of the safe form also fails, so a scan that silently matched nothing
// cannot pass forever.
func TestTextSafeHelpersAreUsedAtEveryCallSite(t *testing.T) {
	// A plain byte slice of a caller string can split a rune and produce
	// invalid UTF-8; truncateBindableText is the only allowed form.
	unsafeTruncate := regexp.MustCompile(`\b(name|suggested|input\.Name|Name)\s*=\s*\w+(\.\w+)*\[:\d+\]`)
	// The raw header reaches a text column; requestUserAgent is the allowed
	// form. The two HASH sites are exempt and named below.
	unsafeUA := regexp.MustCompile(`(r|req)\.UserAgent\(\)|(r|req)\.Header\.Get\("User-Agent"\)`)
	// Exempt sites, with counts, so a NEW raw read in one of these files
	// still fails rather than hiding behind a blanket exemption.
	uaHashExempt := map[string]int{
		// All four reads here feed a HASH, not a text column: one
		// session-fingerprint comparison plus three CreateSession calls,
		// and store.CreateSession hashes the header (sessions.ua_hash) and
		// stores no text. sha256 over arbitrary bytes is well defined, and
		// sanitising before hashing would be actively harmful — login would
		// store sha256(sanitised) while the comparison still hashes the RAW
		// header, so every session from a client with a non-UTF-8
		// User-Agent would fail validation.
		"handlers_auth.go": 4,
		// The same fingerprint comparison on the session-validation path.
		"middleware_auth.go": 1,
		// requestUserAgent itself: this is the one read that is allowed to
		// be raw, because it is the thing doing the sanitising.
		"middleware_request_text.go": 1,
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	var safeTruncateUses, safeUAUses int
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		text := string(src)
		safeTruncateUses += strings.Count(text, "truncateBindableText(")
		safeUAUses += strings.Count(text, "requestUserAgent(")

		for _, m := range unsafeTruncate.FindAllString(text, -1) {
			t.Errorf("%s: %q slices a caller string by BYTES, which can split a rune and produce "+
				"invalid UTF-8 downstream of validation — use truncateBindableText (BUG-2803)", name, m)
		}
		found := len(unsafeUA.FindAllString(text, -1))
		if allowed := uaHashExempt[name]; found > allowed {
			t.Errorf("%s: %d raw User-Agent read(s), %d exempt. A raw header reaching a text column "+
				"must go through requestUserAgent; only the session-hash comparisons are exempt (BUG-2803)",
				name, found, allowed)
		}
	}

	if safeTruncateUses < 4 {
		t.Errorf("found only %d truncateBindableText call sites; the scan or the wiring is broken", safeTruncateUses)
	}
	if safeUAUses < 4 {
		t.Errorf("found only %d requestUserAgent call sites; the scan or the wiring is broken", safeUAUses)
	}
}

// TestOAuthRegisterRefusesNULBody is the last of codex round 14's three
// unwired-call-site findings. Reverting handlers_oauth.go to a bare
// json.NewDecoder — losing both the refusal and the size cap — would
// otherwise leave the suite green, and the static body-reader scan cannot
// catch it because that file is already listed for its FORM-body reads.
//
// It also pins the message split: a body carrying a NUL is valid JSON, so
// answering "Request body must be JSON" sends a client hunting a syntax error
// it does not have.
func TestOAuthRegisterRefusesNULBody(t *testing.T) {
	srv := testServer(t)
	srv.SetCloudMode("cloud-secret-for-test")
	o, err := newTestOAuthServer(t, srv)
	if err != nil {
		t.Fatalf("oauth.NewServer: %v", err)
	}
	srv.SetOAuthServer(o)

	post := func(t *testing.T, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest("POST", "/oauth/register", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "192.0.2.7:1234"
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		return rr
	}

	// Control: a well-formed registration must still be accepted, so a 400
	// below cannot be the endpoint refusing everything.
	control := post(t, `{"redirect_uris":["https://example.com/cb"],"client_name":"Probe"}`)
	if control.Code >= 400 {
		t.Fatalf("control registration must succeed, got %d: %s", control.Code, control.Body.String())
	}

	rr := post(t, `{"redirect_uris":["https://example.com/cb"],"client_name":"a`+escNULLiteral+`b"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("a NUL-bearing registration must answer 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "must be JSON") {
		t.Errorf("the body IS valid JSON; the message must not send the client after a syntax error: %s",
			rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "NUL") {
		t.Errorf("the refusal should name the cause, got %s", rr.Body.String())
	}
}

// TestBodyDecodesNULMatchesKeysLikeTheDecoder is codex round 16: encoding/json
// matches an incoming key to a struct field by an exact match first and a
// CASE-INSENSITIVE one otherwise, so `{"Fields":...}` lands in
// ItemCreate.Fields exactly as `{"fields":...}` does. A case-sensitive lookup
// in the walk skipped the nested check for a body the handler then accepted,
// and the database answered the original 500.
//
// Measured before the fix: `fields` refused, `Fields` and `FIELDS` accepted.
func TestBodyDecodesNULMatchesKeysLikeTheDecoder(t *testing.T) {
	inner := `{"k":"a` + escNULLiteral + `b"}`
	// The last two are Unicode simple-fold spellings: encoding/json matches
	// them to the same struct field, and lower-casing does NOT (codex round
	// 17). U+017F LONG S folds to 's'; U+212A KELVIN SIGN folds to 'k'.
	for _, key := range []string{"fields", "Fields", "FIELDS", "fIeLdS", "Schema", "TAGS",
		"\u017Fchema", "\u017FCHEMA"} {
		body := `{"title":"x","` + key + `":` + jsonEncode(t, inner) + `}`
		if !bodyDecodesNUL([]byte(body)) {
			t.Errorf("key %q reaches the same struct field as its lower-case spelling and must be "+
				"walked the same way", key)
		}
	}

	// Control: a key that is not a wire key in ANY casing stays caller data,
	// so this is case-insensitive matching rather than matching everything.
	notAKey := `{"title":"x","Notes":` + jsonEncode(t, inner) + `}`
	if bodyDecodesNUL([]byte(notAKey)) {
		t.Error("an unlisted key must not be treated as JSON-encoded whatever its casing")
	}
}

// TestBodyDecodesNULKnownMapModelDisagreements pins the four measured
// disagreements between this scan's map[string]any model and encoding/json's
// typed decode (BUG-2803 rounds 16-17; lead ruling: land-and-follow).
// They are documented on bodyDecodesNUL; this is the instrument that keeps
// that documentation and the release note from going stale.
//
// Two of the four are ACCEPTED over-refusals. The other two are KNOWN GAPS
// whose fix is the BUG-2812 token-walk unit — so those legs assert the WRONG
// answer on purpose. That is deliberate and it is the point: when BUG-2812
// lands, this test FAILS, which is the signal to update the doc comment, the
// release note and this test together rather than discovering months later
// that the note describes a version of the check that no longer exists.
func TestBodyDecodesNULKnownMapModelDisagreements(t *testing.T) {
	t.Run("accepted over-refusals", func(t *testing.T) {
		// (3) An unknown field is scanned though no handler reads it. The
		// scan has no destination type by design, so it cannot tell a
		// forward-compatible field from a real one. Observable compatibility
		// change; stated in the release note.
		unknown := `{"title":"valid","future_field":"a` + escNULLiteral + `b"}`
		if !bodyDecodesNUL([]byte(unknown)) {
			t.Error("(3) an unknown field carrying a NUL escape is refused today; if that changed, " +
				"update the disposition on bodyDecodesNUL and the release note's compatibility line")
		}

		// (4) Case-variant duplicates: the typed decode keeps the LAST
		// spelling and discards the NUL, the map scan sees both and refuses.
		// Same root as (1), opposite direction.
		caseDup := `{"title":"a` + escNULLiteral + `b","TITLE":"safe"}`
		if !bodyDecodesNUL([]byte(caseDup)) {
			t.Error("(4) a case-variant duplicate is refused today even though the decode drops the " +
				"NUL spelling; if that changed, update the disposition on bodyDecodesNUL")
		}
	})

	t.Run("known gaps owned by BUG-2812", func(t *testing.T) {
		// (1) Duplicate keys: map[string]any REPLACES, encoding/json MERGES
		// into an already-populated map field. The scan structurally cannot
		// see the shadowed first occurrence.
		dupKey := `{"fields_patch":{"orphan":"a` + escNULLiteral + `b"},"fields_patch":{"status":"open"}}`
		if bodyDecodesNUL([]byte(dupKey)) {
			t.Error("(1) now DETECTED — the map-model duplicate-key gap is closed. That is the " +
				"BUG-2812 token walk landing: update bodyDecodesNUL's disposition block, the " +
				"release note's filed-residuals line, and delete this leg")
		}

		// (2) A scan failure lets a known-bad value through: the overflowing
		// number fails the `any` unmarshal, this function returns false so the
		// caller's decode owns the "invalid JSON" message, and the typed
		// decode then SKIPS the unknown field and accepts the NUL title.
		scanFail := `{"title":"a` + escNULLiteral + `b","ignored":1e999}`
		if bodyDecodesNUL([]byte(scanFail)) {
			t.Error("(2) now DETECTED — the scan-failure passthrough is closed. That is the " +
				"BUG-2812 token walk landing: update bodyDecodesNUL's disposition block, the " +
				"release note's filed-residuals line, and delete this leg")
		}

		// Premise for both legs above: the SAME bodies with their
		// disagreement mechanism removed ARE detected. Without this, the two
		// assertions would pass against a bodyDecodesNUL that detected
		// nothing at all, and would prove nothing about the gaps they name.
		singleKey := `{"fields_patch":{"orphan":"a` + escNULLiteral + `b"}}`
		if !bodyDecodesNUL([]byte(singleKey)) {
			t.Fatal("premise failed: the duplicate-key body's payload is not detectable even when " +
				"spelled once, so the (1) leg above is vacuous")
		}
		parseable := `{"title":"a` + escNULLiteral + `b","ignored":1}`
		if !bodyDecodesNUL([]byte(parseable)) {
			t.Fatal("premise failed: the scan-failure body's payload is not detectable even when " +
				"the body parses, so the (2) leg above is vacuous")
		}
	})
}

// TestUnknownFieldRefusalThroughTheHandler is the WIRING leg for release-note
// item 10 (CONVE-19: a direct-call test vouches for the component, not its
// binding). TestBodyDecodesNULKnownMapModelDisagreements proves the scan
// RETURNS true for an unknown field carrying a NUL escape; the release note
// claims the API answers 400. Those are different claims, and only this one
// is the one an operator or client author reads.
//
// The control leg is what makes the 400 mean something: the SAME unknown
// field with an ordinary value must still be ACCEPTED, so this pins "refused
// for the NUL" rather than "refused for being unknown" — the handler does not
// reject unknown fields, and if it ever started to, the release note's
// explanation of the change would be wrong even though its status code
// stayed right.
func TestUnknownFieldRefusalThroughTheHandler(t *testing.T) {
	srv := testServer(t)

	rr := rawJSONRequest(srv, "POST", "/api/v1/workspaces/",
		`{"name":"Unknown field probe","slug":"unkprobe","template":"startup"}`)
	if rr.Code != http.StatusOK && rr.Code != http.StatusCreated {
		t.Fatalf("fixture workspace: %d %s", rr.Code, rr.Body.String())
	}
	rr = rawJSONRequest(srv, "POST", "/api/v1/workspaces/unkprobe/collections/",
		`{"name":"Probes","slug":"probes"}`)
	if rr.Code != http.StatusOK && rr.Code != http.StatusCreated {
		t.Fatalf("fixture collection: %d %s", rr.Code, rr.Body.String())
	}

	const itemsPath = "/api/v1/workspaces/unkprobe/collections/probes/items"

	// Control: an unknown field is ordinarily IGNORED, not refused. On main
	// this is the only behaviour there is — decodeJSONWithLimit unmarshals
	// straight into the typed value, which drops the key.
	control := rawJSONRequest(srv, "POST", itemsPath,
		`{"title":"ok","future_field":"harmless"}`)
	if control.Code != http.StatusCreated && control.Code != http.StatusOK {
		t.Fatalf("an unknown field with an ordinary value must still be accepted, got %d: %s",
			control.Code, control.Body.String())
	}

	// Release-note item 10: the same unknown field carrying a NUL escape is
	// now refused, though the value reaches nothing. The scan has no
	// destination type by design and cannot tell a forward-compatible field
	// from a real one.
	got := rawJSONRequest(srv, "POST", itemsPath,
		`{"title":"ok","future_field":"a`+escNULLiteral+`b"}`)
	if got.Code != http.StatusBadRequest {
		t.Fatalf("release note item 10 says an unknown field carrying a NUL escape answers 400; "+
			"got %d: %s — update the note or the check", got.Code, got.Body.String())
	}
	if !strings.Contains(got.Body.String(), "NUL") {
		t.Errorf("the 400 should name the cause, got: %s", got.Body.String())
	}
}

// TestDecodeJSONTrimsOnlyJSONWhitespace pins that the empty-body shortcut uses
// JSON's whitespace set, not Go's (codex round 22).
//
// bytes.TrimSpace uses unicode.IsSpace, which strips \v, \f, U+00A0 and more.
// encoding/json accepts none of those. So a body of just "\v" trimmed to
// EMPTY here and returned io.EOF, and an EOF-tolerant caller — playbook run
// treats errors.Is(err, io.EOF) as "no arguments supplied" and runs anyway —
// took a syntactically invalid body for an ABSENT one.
//
// The four legs matter in pairs: real JSON whitespace must still shortcut to
// EOF (or the playbook contract breaks), and non-JSON whitespace must NOT (or
// the divergence is still there).
func TestDecodeJSONTrimsOnlyJSONWhitespace(t *testing.T) {
	srv := testServer(t)

	for _, tc := range []struct {
		name    string
		body    string
		wantEOF bool
	}{
		{"space and tab and newline", " \t\r\n", true},
		{"empty", "", true},
		{"vertical tab", "\v", false},
		{"form feed", "\f", false},
		{"non-breaking space", "\u00a0", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var v map[string]any
			req := httptest.NewRequest("POST", "/x", strings.NewReader(tc.body))
			err := decodeJSON(req, &v)
			if err == nil {
				t.Fatalf("expected an error for body %q", tc.body)
			}
			gotEOF := errors.Is(err, io.EOF)
			if gotEOF != tc.wantEOF {
				if tc.wantEOF {
					t.Errorf("body %q is JSON whitespace and must read as an ABSENT body (io.EOF), "+
						"or the playbook-run empty-body contract breaks; got %v", tc.body, err)
				} else {
					t.Errorf("body %q is NOT JSON whitespace — encoding/json would reject it — so it "+
						"must NOT be reported as an absent body; an EOF-tolerant caller would proceed "+
						"on invalid input. got io.EOF", tc.body)
				}
			}
		})
	}
	_ = srv
}

// independentDecodesNUL is a SECOND implementation of the walker's contract,
// written for the test and deliberately not calling valueDecodesNUL.
//
// It exists because TestBodyDecodesNULGateAgreesWithAnUngatedWalk compares the
// gated function against an "ungated" reference that calls the SAME production
// walker (codex round 22). That comparison is valid for what it claims - it
// pins the raw-prefix GATE - but it cannot see a defect in the walker itself,
// because a walker bug is present identically on both sides and cancels.
//
// The two share encoding/json and jsonEncodedFieldKeys. They deliberately do
// not share traversal, descent, or key-matching code, which is where every
// walker defect in this unit actually lived (rounds 1, 2, 4, 16, 17).
func independentDecodesNUL(t *testing.T, raw []byte) bool {
	t.Helper()
	var root any
	if err := json.Unmarshal(raw, &root); err != nil {
		return false
	}

	hasNUL := func(s string) bool { return strings.ContainsRune(s, 0) }

	// Iterative, explicit stack - a different shape from the production
	// recursion, so a recursion-shaped bug cannot reproduce here by accident.
	type frame struct {
		v      any
		nested bool // already inside a re-parsed document; do not descend again
	}
	stack := []frame{{root, false}}
	for len(stack) > 0 {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		switch cur := f.v.(type) {
		case string:
			if hasNUL(cur) {
				return true
			}
		case []any:
			for _, e := range cur {
				stack = append(stack, frame{e, f.nested})
			}
		case map[string]any:
			for k, v := range cur {
				if hasNUL(k) {
					return true
				}
				if sv, ok := v.(string); ok {
					if hasNUL(sv) {
						return true
					}
					// One level of descent into a listed key's JSON document,
					// and only when not already nested. Matched case-folded,
					// the way encoding/json matches a wire key to a field.
					if !f.nested {
						for listed := range jsonEncodedFieldKeys {
							if !strings.EqualFold(k, listed) {
								continue
							}
							var inner any
							if json.Unmarshal([]byte(sv), &inner) == nil {
								stack = append(stack, frame{inner, true})
							}
							break
						}
					}
					continue
				}
				// A NATURAL object or array under a listed key is USER DATA:
				// the server marshals it itself and nothing re-parses it, so
				// a listed key appearing INSIDE it is an ordinary field name,
				// not a document marker. Production expresses this by passing
				// inUserData=true; this oracle must too, or the two disagree
				// on {"fields":{"schema":"...escape text..."}} — production
				// correctly says no, and an oracle that says yes would fail
				// the comparison and blame the production walker.
				//
				// Measured: before this, production=false / oracle=true on
				// exactly that body. My corpus omitted it, so the oracle
				// passed while being wrong (codex round 26). A one-sidedness
				// check that only asks whether BOTH answers appear does not
				// catch a corpus that misses a whole branch of the contract.
				nested := f.nested
				if !f.nested {
					for listed := range jsonEncodedFieldKeys {
						if strings.EqualFold(k, listed) {
							nested = true
							break
						}
					}
				}
				stack = append(stack, frame{v, nested})
			}
		}
	}
	return false
}

// TestBodyDecodesNULAgainstAnIndependentOracle checks the WALKER, which the
// gate differential structurally cannot (codex round 22: shared-oracle blind
// spot). Disagreement means one of the two is wrong and both get read - the
// point is that a single shared bug can no longer hide.
func TestBodyDecodesNULAgainstAnIndependentOracle(t *testing.T) {
	esc := escNULLiteral
	bs := string([]byte{'\\', 'u', '0', '0', '5', 'c'})
	doubled := `\\` + "u0000"

	corpus := []string{
		`{"title":"plain"}`,
		`{"title":"quotes \" and a backslash \\ but no escape"}`,
		`{"fields":"{\"k\":\"plain\"}"}`,
		`{"fields":"{\"k\":\"a` + esc + `b\"}"}`,
		`{"fields":"{\"k\":\"a` + bs + `u0000b\"}"}`,
		`{"fields":"{\"k\":\"a` + doubled + `b\"}"}`,
		`{"title":"a` + esc + `b"}`,
		`{"a` + esc + `b":"key"}`,
		`{"tags":["ok","a` + esc + `b"]}`,
		`{"fields":"[1,2,\"a` + esc + `b\"]"}`,
		`{"Fields":"{\"k\":\"a` + esc + `b\"}"}`,
		`{"content":"{\"k\":\"a` + esc + `b\"}"}`,
		`{"title":"unicode é 中"}`,
		`{"nested":{"deep":{"deeper":"a` + esc + `b"}}}`,
		// The branch the corpus used to miss entirely: a NATURAL object under
		// a listed key, carrying escape TEXT that never becomes a NUL because
		// nothing re-parses it. Both sides must answer FALSE here.
		`{"fields":{"schema":"{\"k\":\"a` + doubled + `b\"}"}}`,
		// And its counterpart, where the listed key's value IS a string and
		// the descent is correct. Both sides must answer TRUE.
		`{"fields":"{\"k\":\"a` + esc + `b\"}"}`,
		`{"title":"a control escape that is not a NUL: \\u0001"}`,
	}

	var sawTrue, sawFalse bool
	for i, body := range corpus {
		got := bodyDecodesNUL([]byte(body))
		want := independentDecodesNUL(t, []byte(body))
		if got != want {
			t.Errorf("corpus[%d] %s\n  production=%v independent=%v — one of the two walkers is "+
				"wrong; read both before assuming it is the oracle", i, body, got, want)
		}
		if want {
			sawTrue = true
		} else {
			sawFalse = true
		}
	}

	// The corpus must contain BOTH answers, or agreement proves nothing: two
	// walkers that always say false agree perfectly.
	if !sawTrue || !sawFalse {
		t.Fatalf("corpus is one-sided (sawTrue=%v sawFalse=%v); agreement over it is vacuous",
			sawTrue, sawFalse)
	}
}
