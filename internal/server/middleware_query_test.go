package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// badQueryVal is the canonically escaped form of an invalid-UTF-8 byte, in a
// query VALUE. Unlike the path (see badPathSeg), the case of the hex digits
// is not load-bearing here — url.ParseQuery decodes "%ff" and "%FF"
// identically, and TestValidateQueryRejectsUnbindableQueryText drives both to
// pin that rather than assume it.
const badQueryVal = "bad-%FF-x"

// assertQueryVectorIntact fails if the request Go builds from target does not
// actually deliver invalid query text to a handler — i.e. if the premise of
// the assertions below has stopped holding. Without this, a future change to
// how net/url decodes a query would silently turn every case in this file
// into a test of nothing that still passes.
//
// It asserts against r.URL.Query(), because that is what handlers read and
// therefore what "reaches the store" means for the query string.
func assertQueryVectorIntact(t *testing.T, target string) {
	t.Helper()
	req := httptest.NewRequest("GET", target, nil)
	for key, values := range req.URL.Query() {
		if !bindableText(key) {
			return
		}
		for _, v := range values {
			if !bindableText(v) {
				return
			}
		}
	}
	t.Fatalf("premise broken: %q parses to a query whose keys and values are all "+
		"already valid text (%#v); this test can no longer exercise BUG-2784's vector",
		target, req.URL.Query())
}

// TestValidateQueryRejectsUnbindableQueryText drives the REAL server rather
// than ValidateQuery directly, so it vouches for the middleware's BINDING as
// well as its logic — a direct call would pass even if nobody had wired it
// into the chain (CONVE-19).
//
// It does NOT reproduce the unfixed 500; no test here can, because every test
// runs the fixed server. The pre-fix distribution is recorded on ValidateQuery
// and what keeps it reachable as evidence is the mutation matrix, since
// unwiring ValidateQuery fails TestValidateQueryPostgresNoInternalError.
func TestValidateQueryRejectsUnbindableQueryText(t *testing.T) {
	srv := testServer(t)
	ws := createWSForTest(t, srv)

	cases := []struct {
		name   string
		method string
		target string
	}{
		// The parseItemListParams funnel: a DECLARED parameter, and an
		// undeclared one that becomes a field filter only because the
		// wildcard branch exists. Both reach a text comparison.
		{"items list, declared param", "GET", "/api/v1/workspaces/" + ws + "/items?search=" + badQueryVal},
		{"items list, undeclared param", "GET", "/api/v1/workspaces/" + ws + "/items?email=" + badQueryVal},
		{"collection items", "GET", "/api/v1/workspaces/" + ws + "/collections/tasks/items?status=" + badQueryVal},
		// The discrete point-of-use sites the sweep found.
		{"search q", "GET", "/api/v1/search?q=" + badQueryVal},
		{"activity actor", "GET", "/api/v1/workspaces/" + ws + "/activity?actor=" + badQueryVal},
		{"attachments collection", "GET", "/api/v1/workspaces/" + ws + "/attachments?collection=" + badQueryVal},
		{"items-index collection", "GET", "/api/v1/workspaces/" + ws + "/items-index?collection=" + badQueryVal},
		// A NUL is valid UTF-8 and refused by Postgres, so the rule covers
		// it too; this case survives dropping the ContainsRune half of
		// bindableText only if that half is still there.
		{"NUL value", "GET", "/api/v1/workspaces/" + ws + "/items?search=bad-%00-x"},
		// The KEY half. Precautionary — no key was observed to 500 — so this
		// case is what makes the key check non-vacuous.
		{"invalid key", "GET", "/api/v1/workspaces/" + ws + "/items?bad-%FF-key=x"},
		{"NUL key", "GET", "/api/v1/workspaces/" + ws + "/items?bad-%00-key=x"},
		// Lowercase escape: same byte, and the answer must not depend on the
		// case of a hex digit.
		{"lowercase %ff", "GET", "/api/v1/workspaces/" + ws + "/items?search=bad-%ff-x"},
		// Not percent-encoded at all: a client may put the raw byte in the
		// query string. This is the case url.ParseQuery never decodes, and
		// it is caught by the raw check before the fast path returns.
		{"raw unescaped byte", "GET", "/api/v1/workspaces/" + ws + "/items?search=bad-\xff-x"},
		// Second and later values of a repeated key, and later pairs
		// generally: a check that stopped at the first pair would pass the
		// cases above and fail these.
		{"second pair", "GET", "/api/v1/workspaces/" + ws + "/items?status=open&search=" + badQueryVal},
		{"repeated key, second value", "GET", "/api/v1/workspaces/" + ws + "/items?search=fine&search=" + badQueryVal},
		// Methods other than GET route through the same chain.
		{"PATCH", "PATCH", "/api/v1/workspaces/" + ws + "/items/anything?search=" + badQueryVal},
		{"POST", "POST", "/api/v1/workspaces/" + ws + "/collections/tasks/items?search=" + badQueryVal},
		// Not an /api/v1 route: ValidateQuery is on the ROOT router. Fails if
		// someone moves the Use() into the API group.
		{"non-API route (SPA catch-all)", "GET", "/anything?search=" + badQueryVal},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertQueryVectorIntact(t, tc.target)
			rr := doRequest(srv, tc.method, tc.target, nil)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("%s %s: expected 400, got %d: %s",
					tc.method, tc.target, rr.Code, rr.Body.String())
			}
			if code := pathErrorCode(t, rr); code != "invalid_query" {
				t.Fatalf("%s %s: expected error code invalid_query, got %q",
					tc.method, tc.target, code)
			}
		})
	}
}

// TestValidateQueryAllowsValidText is the control leg, and it is the half
// that matters most here: the objection to validating the query string at
// the transport was that its legitimate value domain is much wider than the
// path's. It is — and every one of these must still work.
//
// A middleware that answered 400 to any query with a percent-escape, or to
// anything non-ASCII, would pass the test above and break real clients.
func TestValidateQueryAllowsValidText(t *testing.T) {
	srv := testServer(t)
	ws := createWSForTest(t, srv)

	for _, tc := range []struct {
		name   string
		target string
	}{
		{"no query at all", "/api/v1/workspaces/" + ws + "/items"},
		{"empty query", "/api/v1/workspaces/" + ws + "/items?"},
		{"plain ascii", "/api/v1/workspaces/" + ws + "/items?search=hello"},
		{"escaped accented text", "/api/v1/workspaces/" + ws + "/items?search=caf%C3%A9"},
		{"unescaped accented text", "/api/v1/workspaces/" + ws + "/items?search=café"},
		{"escaped emoji", "/api/v1/workspaces/" + ws + "/items?search=%F0%9F%9A%80"},
		{"plus as space", "/api/v1/workspaces/" + ws + "/items?search=two+words"},
		{"escaped space", "/api/v1/workspaces/" + ws + "/items?search=two%20words"},
		{"escaped ampersand in value", "/api/v1/workspaces/" + ws + "/items?search=a%26b"},
		{"escaped equals in value", "/api/v1/workspaces/" + ws + "/items?search=a%3Db"},
		{"empty value", "/api/v1/workspaces/" + ws + "/items?search="},
		{"key with no equals", "/api/v1/workspaces/" + ws + "/items?search"},
		{"repeated key", "/api/v1/workspaces/" + ws + "/items?search=a&search=b"},
		{"non-ascii key", "/api/v1/workspaces/" + ws + "/items?caf%C3%A9=x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := doRequest(srv, "GET", tc.target, nil)
			if rr.Code != http.StatusOK {
				t.Fatalf("GET %s: expected 200, got %d: %s", tc.target, rr.Code, rr.Body.String())
			}
		})
	}
}

// TestValidateQueryDeliversEscapedUnicodeToTheHandler is the control the
// status-code table above cannot give: a 200 proves the request was not
// refused, not that the parameter still means what the client sent. This
// asserts the decoded value reached the handler intact by requiring it to
// MATCH an item it can only match if it did.
func TestValidateQueryDeliversEscapedUnicodeToTheHandler(t *testing.T) {
	srv := testServer(t)
	ws := createWSForTest(t, srv)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/collections/tasks/items",
		map[string]interface{}{"title": "Le café serves 🚀 rockets"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create item: %d %s", rr.Code, rr.Body.String())
	}
	var created models.Item
	parseJSON(t, rr, &created)

	// NOT an emoji case, deliberately, though the status-code table above has
	// one. `?search=%F0%9F%9A%80` matches zero items — and so does the same
	// emoji sent UNESCAPED, against the same item, which is what identifies
	// the cause: the search index does not tokenize it. That is a property of
	// search, not of percent-decoding, so an emoji here would fail for a
	// reason this test is not about. Multi-byte coverage comes from café,
	// whose escape sequence is exactly as multi-byte as the emoji's.
	for _, tc := range []struct{ name, query string }{
		{"escaped accented text", "search=caf%C3%A9"},
		{"escaped space in a phrase", "search=serves%20rockets"},
		{"plus-encoded space in a phrase", "search=serves+rockets"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := doRequest(srv, "GET", "/api/v1/workspaces/"+ws+"/items?"+tc.query, nil)
			if rr.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
			}
			var got []models.Item
			parseJSON(t, rr, &got)
			found := false
			for _, it := range got {
				if it.ID == created.ID {
					found = true
				}
			}
			if !found {
				t.Fatalf("?%s matched %d items but not the one it names — the decoded "+
					"value did not reach the handler intact", tc.query, len(got))
			}
		})
	}
}

// TestValidateQueryRunsAfterValidatePath pins the ORDERING decision in
// setupRouter. A request bad in both places is answered for its path, which
// is the more specific fault; swapping the two Use() calls flips the code and
// fails here.
func TestValidateQueryRunsAfterValidatePath(t *testing.T) {
	srv := testServer(t)
	target := "/api/v1/workspaces/" + badPathSeg + "/items?search=" + badQueryVal
	assertPathVectorIntact(t, target)
	assertQueryVectorIntact(t, target)

	rr := doRequest(srv, "GET", target, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if code := pathErrorCode(t, rr); code != "invalid_path" {
		t.Fatalf("a request bad in both path and query must be answered for the path: got %q", code)
	}
}

// TestValidateQueryRejectionLooksLikeEveryOtherAPIError pins the response
// SHAPE, not just the status: this rejection short-circuits above the
// /api/v1 group's cors.Handler and jsonContentType, so a naive
// implementation answers a JSON body typed text/plain and, on a
// cross-origin deployment, with no CORS headers — which the browser refuses
// to let the page read at all.
// Each header is compared against the SAME endpoint answered normally, so
// the assertion is parity with the API's own responses rather than a list of
// header names copied from a spec — the same construction the path half uses.
func TestValidateQueryRejectionLooksLikeEveryOtherAPIError(t *testing.T) {
	const allowed = "https://app.example.com"

	srv := testServer(t)
	srv.SetCORSOrigins(allowed)
	ws := createWSForTest(t, srv)

	get := func(target, origin string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", target, nil)
		req.RemoteAddr = "10.9.9.9:1"
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	badTarget := "/api/v1/workspaces/" + ws + "/items?search=" + badQueryVal
	okTarget := "/api/v1/workspaces/" + ws + "/items?search=ordinary-value"
	compared := []string{
		"Content-Type",
		"Access-Control-Allow-Origin",
		"Access-Control-Allow-Credentials",
		"Vary",
	}

	for _, origin := range []string{allowed, "https://evil.example.com"} {
		bad := get(badTarget, origin)
		ok := get(okTarget, origin)
		if bad.Code != http.StatusBadRequest {
			t.Fatalf("origin %s: expected 400, got %d: %s", origin, bad.Code, bad.Body.String())
		}
		if ok.Code != http.StatusOK {
			t.Fatalf("origin %s: control expected 200, got %d: %s", origin, ok.Code, ok.Body.String())
		}
		for _, h := range compared {
			if bad.Header().Get(h) != ok.Header().Get(h) {
				t.Errorf("origin %s: %s differs between the rejection and an ordinary "+
					"response: rejection %q, ordinary %q",
					origin, h, bad.Header().Get(h), ok.Header().Get(h))
			}
		}
	}

	// Sanity: the parity above is worthless if the ordinary response carries
	// no CORS header to match in the first place.
	if got := get(okTarget, allowed).Header().Get("Access-Control-Allow-Origin"); got != allowed {
		t.Fatalf("premise broken: an ordinary response to an allowed origin carries "+
			"Access-Control-Allow-Origin %q, so header parity proves nothing", got)
	}
}

// TestValidateQueryPostgresNoInternalError is the dialect half: it drives the
// vector on the backend where the 500 occurred, against a real Postgres
// store, and requires 400. Unwiring ValidateQuery fails this test, which is
// what keeps the measured 500 reachable as evidence.
//
// Skips unless PAD_TEST_POSTGRES_URL is set; runs under `make test-pg`.
func TestValidateQueryPostgresNoInternalError(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set — the 500 only reproduces on Postgres")
	}
	s := storetest.NewPostgres(t)
	srv := New(s)
	t.Cleanup(func() { srv.Stop() })
	if srv.store.D().Driver() != store.DriverPostgres {
		t.Fatalf("expected a Postgres store, got %s", srv.store.D().Driver())
	}

	ws := createWSForTest(t, srv)
	for _, target := range []string{
		"/api/v1/workspaces/" + ws + "/items?search=" + badQueryVal,
		"/api/v1/workspaces/" + ws + "/items?email=" + badQueryVal,
		"/api/v1/workspaces/" + ws + "/items?search=bad-%00-x",
		"/api/v1/workspaces/" + ws + "/collections/tasks/items?status=" + badQueryVal,
		"/api/v1/search?q=" + badQueryVal,
		"/api/v1/workspaces/" + ws + "/activity?actor=" + badQueryVal,
		"/api/v1/workspaces/" + ws + "/attachments?collection=" + badQueryVal,
		"/api/v1/workspaces/" + ws + "/items-index?collection=" + badQueryVal,
	} {
		assertQueryVectorIntact(t, target)
		rr := doRequest(srv, "GET", target, nil)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("GET %s on Postgres: expected 400, got %d: %s",
				target, rr.Code, rr.Body.String())
		}
	}

	// Control: the same store answers the same endpoints normally, so the
	// 400s above are the middleware's judgement and not a broken fixture.
	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+ws+"/items?search=ordinary-value", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET valid search on Postgres: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestValidQueryText(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"empty", "", true},
		{"plain", "search=hello&status=open", true},
		{"escaped utf8 value", "search=caf%C3%A9", true},
		{"literal utf8 value", "search=café", true},
		{"escaped emoji", "search=%F0%9F%9A%80", true},
		{"plus", "search=two+words", true},
		{"escaped separators", "search=a%26b%3Dc", true},
		{"escaped key", "caf%C3%A9=x", true},
		{"no equals", "search", true},

		{"escaped lone 0xff value", "search=bad-%FF-x", false},
		{"escaped lone 0xff value, lowercase", "search=bad-%ff-x", false},
		{"raw lone 0xff value", "search=bad-\xff-x", false},
		{"escaped NUL value", "search=bad-%00-x", false},
		{"raw NUL value", "search=bad-\x00-x", false},
		{"escaped truncated sequence", "search=bad-%C3(-x", false},
		{"escaped surrogate half", "search=%ED%A0%80", false},
		{"escaped overlong", "search=%C0%AF", false},
		{"bad key", "bad-%FF-key=x", false},
		{"bad value in a later pair", "status=open&search=bad-%FF-x", false},
		{"bad second value of a repeated key", "search=fine&search=bad-%FF-x", false},

		// A malformed escape is dropped by url.ParseQuery, and by
		// r.URL.Query(), so no handler can see it. The pairs that DO parse
		// still decide the answer — both directions.
		{"malformed escape, rest fine", "search=100%off&status=open", true},
		{"malformed escape, rest bad", "search=100%off&status=bad-%FF-x", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := validQueryText(c.in); got != c.want {
				t.Errorf("validQueryText(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

// TestValidQueryTextFastPathAgreesWithTheSlowPath is the differential check
// behind the '%' fast path. The fast path returns true for any raw query
// with no escapes that is itself bindableText; the claim is that this can
// never disagree with decoding every pair and checking each one. Any
// disagreement here means the reasoning in validQueryText's comment is
// wrong, not merely that a case was missed.
func TestValidQueryTextFastPathAgreesWithTheSlowPath(t *testing.T) {
	slow := func(rawQuery string) bool {
		q, _ := url.ParseQuery(rawQuery)
		for key, values := range q {
			if !bindableText(key) {
				return false
			}
			for _, v := range values {
				if !bindableText(v) {
					return false
				}
			}
		}
		return true
	}
	for _, raw := range []string{
		"", "a=b", "a=b&c=d", "search=café", "search=two+words", "a", "a=", "=b",
		"a=b&a=c", "&&", "a=b&", "search=a b", "x=\xff", "\xff=x", "x=a\x00b",
		"a=b&c=\xc3", "søk=svar", "a=%", "s=100%off",
	} {
		if !strings.Contains(raw, "%") {
			if got, want := validQueryText(raw), slow(raw); got != want {
				t.Errorf("fast path disagrees for %q: validQueryText=%v, per-pair=%v", raw, got, want)
			}
		}
	}
}
