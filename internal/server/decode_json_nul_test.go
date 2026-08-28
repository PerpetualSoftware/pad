package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
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

// TestNoJSONBodyDecoderOutsideTheChokepoint is the completeness claim, made
// ENFORCEABLE rather than asserted.
//
// The fix works because every JSON request body in this package reaches the
// store through decodeJSON/decodeJSONWithLimit. That was true of 65 call
// sites and false of six, which decoded straight off r.Body and inherited
// neither the NUL check nor the size cap decodeJSON has always applied. A
// seventh added later would silently reopen both, and nothing in a diff would
// point at it — so the invariant is checked here instead of remembered.
func TestNoJSONBodyDecoderOutsideTheChokepoint(t *testing.T) {
	// The chokepoint itself reads the body; nothing else in the package may.
	allowed := map[string]bool{"middleware_request_text.go": true}

	pattern := regexp.MustCompile(`json\.NewDecoder\((r|req)\.Body\)|io\.ReadAll\((r|req)\.Body\)`)

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	var offenders []string
	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		scanned++
		if allowed[name] {
			continue
		}
		src, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if pattern.MatchString(line) {
				offenders = append(offenders, name+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
			}
		}
	}

	// Assert the scan actually looked at something. A test whose search
	// silently matched no files would pass forever.
	if scanned < 20 {
		t.Fatalf("scan looked at only %d non-test .go files; the package is much larger, so the scan is broken", scanned)
	}
	if len(offenders) > 0 {
		t.Errorf("request bodies must be decoded through decodeJSON/decodeJSONWithLimit "+
			"(BUG-2803: they apply the NUL refusal and the size cap). Found %d direct decoder(s):\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}
