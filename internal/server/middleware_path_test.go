package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// badPathSeg is the CANONICALLY escaped form of an invalid-UTF-8 byte.
//
// The case of the hex digits is load-bearing and not cosmetic: Go escapes
// 0xff as uppercase "%FF", so url.Parse leaves RawPath empty for this form
// and chi routes on the DECODED Path — which is how the raw byte reaches a
// handler and then the store. The lowercase form "%ff" is NOT canonical, so
// RawPath is populated, chi routes on it, and URLParam yields the literal
// text. Tests that used only the lowercase form would exercise the harmless
// half of the vector and pass against unfixed code.
const badPathSeg = "bad-%FF-x"

// assertPathVectorIntact fails if the request Go builds from target does not
// actually carry invalid path text — i.e. if the premise of every assertion
// below has stopped holding (a Go escaping change, a different chi routing
// choice). A test whose vector has quietly become inert passes for a reason
// that has nothing to do with the code it names.
func assertPathVectorIntact(t *testing.T, target string) {
	t.Helper()
	req := httptest.NewRequest("GET", target, nil)
	if utf8.ValidString(req.URL.Path) && !strings.ContainsRune(req.URL.Path, 0) {
		t.Fatalf("premise broken: %q parses to a path that is already valid text (%q); "+
			"this test can no longer exercise BUG-2782's vector", target, req.URL.Path)
	}
}

func pathErrorCode(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", rr.Body.String(), err)
	}
	return body.Error.Code
}

// TestValidatePathRejectsUnbindablePathText drives the REAL server (not
// ValidatePath directly) so it vouches for the middleware's BINDING as well
// as its logic — a direct call would pass even if nobody had wired it into
// the chain (CONVE-19).
//
// Every case is a 400 before any handler runs.
//
// What this test proves is that claim and no more. It does NOT reproduce
// the unfixed behaviour, and the sweep it comes from found no single
// answer to replace it with. The pre-fix distribution across 247 probes on
// Postgres, pasted from the run:
//
//	500: 191   404: 34   403: 12   401: 4   503: 4   400: 2
//
// The 56 non-500s are routes whose authorization or configuration gate
// answers before any store call is reached — admin user lookup, the
// attachment routes on a server with no storage configured. "These were
// all 500 before" would have been the tidier sentence and false for
// nearly a quarter of the table.
//
// The 500 is reproduced deliberately in one place, on the backend where it
// happens: TestValidatePathPostgresNoInternalError below.
func TestValidatePathRejectsUnbindablePathText(t *testing.T) {
	srv := testServer(t)
	ws := createWSForTest(t, srv)

	targets := []struct {
		name   string
		method string
		target string
	}{
		// One per resolver family reachable from a path segment.
		{"workspace slug", "GET", "/api/v1/workspaces/" + badPathSeg},
		{"workspace slug (subroute)", "GET", "/api/v1/workspaces/" + badPathSeg + "/activity"},
		{"item slug", "GET", "/api/v1/workspaces/" + ws + "/items/" + badPathSeg},
		{"item slug (subroute)", "GET", "/api/v1/workspaces/" + ws + "/items/" + badPathSeg + "/timeline"},
		{"collection slug", "GET", "/api/v1/workspaces/" + ws + "/collections/" + badPathSeg + "/items"},
		{"attachment id", "GET", "/api/v1/workspaces/" + ws + "/attachments/" + badPathSeg},
		{"admin user id", "GET", "/api/v1/admin/users/" + badPathSeg},
		{"invitation code", "GET", "/api/v1/invitations/" + badPathSeg + "/preview"},
		{"share token", "GET", "/api/v1/s/" + badPathSeg},
		{"collab item id", "GET", "/api/v1/collab/" + badPathSeg},
		// Methods other than GET route through the same chain.
		{"PATCH item", "PATCH", "/api/v1/workspaces/" + ws + "/items/" + badPathSeg},
		{"DELETE item", "DELETE", "/api/v1/workspaces/" + ws + "/items/" + badPathSeg},
		{"POST comment", "POST", "/api/v1/workspaces/" + ws + "/items/" + badPathSeg + "/comments"},
		// A NUL is VALID UTF-8 but Postgres refuses it in a text parameter,
		// so the rule covers it too and this case would survive dropping the
		// ContainsRune half of validPathText.
		{"NUL byte", "GET", "/api/v1/workspaces/" + ws + "/items/bad-%00-x"},
		// Non-canonical escaping of the same byte. Harmless on today's chi
		// (RawPath is populated, so the segment stays percent-encoded), and
		// answered identically anyway — the response must not depend on the
		// case of a hex digit.
		{"lowercase %ff", "GET", "/api/v1/workspaces/" + ws + "/items/bad-%ff-x"},
		// Not an /api/v1 route: ValidatePath is on the ROOT router, so the
		// SPA catch-all is covered too. Fails if someone moves the Use() into
		// the API group.
		{"non-API route (SPA catch-all)", "GET", "/" + badPathSeg},
	}

	for _, tc := range targets {
		t.Run(tc.name, func(t *testing.T) {
			assertPathVectorIntact(t, tc.target)
			rr := doRequest(srv, tc.method, tc.target, nil)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("%s %s: expected 400, got %d: %s",
					tc.method, tc.target, rr.Code, rr.Body.String())
			}
			if code := pathErrorCode(t, rr); code != "invalid_path" {
				t.Fatalf("%s %s: expected error code invalid_path, got %q",
					tc.method, tc.target, code)
			}
		})
	}
}

// TestValidatePathAllowsValidText is the control leg: the rule must reject
// only what the database rejects. A middleware that answered 400 to every
// path with a percent-escape, or to anything non-ASCII, would pass the test
// above and break real clients — these cases are what tells the two apart.
func TestValidatePathAllowsValidText(t *testing.T) {
	srv := testServer(t)
	ws := createWSForTest(t, srv)

	// Valid UTF-8 segments reach the resolver and get its answer (404),
	// NOT the middleware's (400).
	for _, seg := range []string{
		"caf%C3%A9-x",         // é
		"rocket-%F0%9F%9A%80", // emoji
		"plain-ascii-miss",
	} {
		rr := doRequest(srv, "GET", "/api/v1/workspaces/"+ws+"/items/"+seg, nil)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("GET item %q: expected 404 from the resolver, got %d: %s",
				seg, rr.Code, rr.Body.String())
		}
	}

	// And a real item still resolves end to end.
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/collections/tasks/items",
		map[string]interface{}{"title": "Path control item"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create item: %d %s", rr.Code, rr.Body.String())
	}
	var it models.Item
	parseJSON(t, rr, &it)
	if rr := doRequest(srv, "GET", "/api/v1/workspaces/"+ws+"/items/"+it.Slug, nil); rr.Code != http.StatusOK {
		t.Fatalf("GET real item: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestValidPathText(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"/api/v1/workspaces/demo/items/task-5", true},
		{"/api/v1/workspaces/demo/items/café", true},
		{"/api/v1/workspaces/demo/items/\U0001F680", true},
		{"/", true},
		{"", true},
		{"/api/v1/items/bad-\xff-x", false},   // lone 0xff
		{"/api/v1/items/bad-\xc3(-x", false},  // truncated 2-byte sequence
		{"/api/v1/items/\xed\xa0\x80", false}, // surrogate half
		{"/api/v1/items/\xc0\xaf", false},     // overlong encoding
		{"/api/v1/items/bad-\x00-x", false},   // NUL: valid UTF-8, refused by Postgres
	}
	for _, c := range cases {
		if got := validPathText(c.in); got != c.want {
			t.Errorf("validPathText(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestValidatePathPostgresNoInternalError is the dialect half, and the only
// leg that reproduces the ORIGINAL symptom: on Postgres the unfixed server
// answered 500 to these requests (SQLITE answered 404, which is why the
// defect was invisible to self-hosted installs and live on Pad Cloud).
// Skips unless PAD_TEST_POSTGRES_URL is set; runs under `make test-pg`.
func TestValidatePathPostgresNoInternalError(t *testing.T) {
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
		"/api/v1/workspaces/" + badPathSeg,
		"/api/v1/workspaces/" + ws + "/items/" + badPathSeg,
		"/api/v1/workspaces/" + ws + "/collections/" + badPathSeg + "/items",
		"/api/v1/workspaces/" + ws + "/items/bad-%00-x",
	} {
		assertPathVectorIntact(t, target)
		rr := doRequest(srv, "GET", target, nil)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("GET %s on Postgres: expected 400, got %d: %s",
				target, rr.Code, rr.Body.String())
		}
	}

	// Control: the same store answers a valid request normally, so the
	// 400s above are the middleware's judgement and not a broken fixture.
	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+ws+"/items/no-such-item", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("GET valid-but-absent item on Postgres: expected 404, got %d: %s",
			rr.Code, rr.Body.String())
	}
}

// TestValidatePathRejectionLooksLikeEveryOtherAPIError pins the response
// SHAPE, not just the status.
//
// ValidatePath runs on the root router, so its rejection short-circuits
// above the /api/v1 group's cors.Handler and jsonContentType and gets
// neither for free. Before this was handled, the 400 carried a JSON body
// typed text/plain and no CORS headers at all, which on a cross-origin
// deployment means the browser will not let the page read the response —
// a debuggable 400 arrives as an opaque network error. Each header below
// is compared against the SAME request path answered normally (404), so
// the assertion is parity with the API's own errors rather than a list of
// header names copied from a spec.
func TestValidatePathRejectionLooksLikeEveryOtherAPIError(t *testing.T) {
	const allowed = "https://app.example.com"

	srv := testServer(t)
	srv.SetCORSOrigins(allowed)
	ws := createWSForTest(t, srv)

	get := func(method, target, origin string, preflight bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, nil)
		req.RemoteAddr = "10.9.9.9:1"
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if preflight {
			req.Header.Set("Access-Control-Request-Method", "GET")
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	badTarget := "/api/v1/workspaces/" + ws + "/items/" + badPathSeg
	okTarget := "/api/v1/workspaces/" + ws + "/items/no-such-item"
	compared := []string{
		"Content-Type",
		"Access-Control-Allow-Origin",
		"Access-Control-Allow-Credentials",
		"Vary",
	}

	// An allowed origin, and a disallowed one. Both directions matter: the
	// second is what would fail if the rejection echoed origins the shared
	// cors.Handler would refuse.
	for _, origin := range []string{allowed, "https://evil.example"} {
		assertPathVectorIntact(t, badTarget)
		rejected := get("GET", badTarget, origin, false)
		normal := get("GET", okTarget, origin, false)

		if rejected.Code != http.StatusBadRequest {
			t.Fatalf("origin %s: expected 400, got %d", origin, rejected.Code)
		}
		if normal.Code != http.StatusNotFound {
			t.Fatalf("origin %s: control request expected 404, got %d", origin, normal.Code)
		}
		for _, h := range compared {
			if got, want := rejected.Header().Get(h), normal.Header().Get(h); got != want {
				t.Errorf("origin %s: header %s on the 400 = %q, but the 404 for the same route carries %q",
					origin, h, got, want)
			}
		}
		if ct := rejected.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("origin %s: expected a JSON content type on the 400, got %q", origin, ct)
		}
	}

	// A genuine preflight is answered by the shared cors.Handler, exactly as
	// it is for any other path: a preflight asks whether the METHOD and
	// HEADERS are permitted, not whether the resource exists. The real
	// request that follows still gets the 400 — and can now be read.
	pre := get("OPTIONS", badTarget, allowed, true)
	if pre.Code != http.StatusOK {
		t.Fatalf("preflight for an invalid path: expected 200 from the CORS handler, got %d", pre.Code)
	}
	if got := pre.Header().Get("Access-Control-Allow-Origin"); got != allowed {
		t.Fatalf("preflight: expected Access-Control-Allow-Origin %q, got %q", allowed, got)
	}
	follow := get("GET", badTarget, allowed, false)
	if follow.Code != http.StatusBadRequest || follow.Header().Get("Access-Control-Allow-Origin") != allowed {
		t.Fatalf("request after preflight: got %d with Access-Control-Allow-Origin %q; want 400 readable cross-origin",
			follow.Code, follow.Header().Get("Access-Control-Allow-Origin"))
	}
}

// TestValidatePathRejectsBeforeAuthAndRateLimit pins an ORDERING decision,
// not an accident.
//
// ValidatePath sits on the root router, so a rejected request never reaches
// the /api/v1 group's TokenAuth, SessionAuth, RateLimit or CSRFProtect. That
// is deliberate, and the direction is the opposite of what "bypasses the
// rate limiter" usually implies: BEFORE this middleware existed the same
// request ran SessionAuth — which is a store.ValidateSession round trip —
// then the limiter, then a handler that issued a query the database refused,
// and answered 500. It now costs a UTF-8 scan over the path and a short JSON
// write, with no database contact at all, so the unmetered path is strictly
// cheaper than every path the limiter protects. There is also nothing to
// learn by flooding it: the answer is constant for all inputs of this shape,
// independent of authentication and of whether anything exists.
//
// Placing the check inside the group instead — where the limiter would meter
// it — would trade this for a real coverage hole, since the SPA catch-all
// and /api/v1/collab/{itemID} are mounted outside that group.
//
// The limiter itself is a plain token bucket per key (no escalating ban, no
// durable block), so skipping it for a rejected request defeats no state
// that outlives the request.
func TestValidatePathRejectsBeforeAuthAndRateLimit(t *testing.T) {
	srv := testServer(t)
	ws := createWSForTest(t, srv)

	const attacker = "198.51.100.7:1234"
	const bystander = "198.51.100.8:1234"
	// Comfortably above the general API limiter's burst of 60, so a metered
	// request stream would certainly have started returning 429 by the end.
	const flood = 80

	badTarget := "/api/v1/workspaces/" + ws + "/items/" + badPathSeg
	okTarget := "/api/v1/workspaces/" + ws + "/items/no-such-item"

	assertPathVectorIntact(t, badTarget)
	for i := 0; i < flood; i++ {
		rr := doRequestFromRemoteAddr(srv, "GET", badTarget, nil, attacker)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("invalid-path request %d/%d: expected 400, got %d: %s",
				i+1, flood, rr.Code, rr.Body.String())
		}
	}

	// The same IP's budget is intact: a legitimate request still gets the
	// resolver's answer rather than a 429.
	if rr := doRequestFromRemoteAddr(srv, "GET", okTarget, nil, attacker); rr.Code != http.StatusNotFound {
		t.Fatalf("valid request from an IP that just sent %d invalid paths: expected 404, got %d: %s",
			flood, rr.Code, rr.Body.String())
	}

	// PREMISE CHECK. Everything above is vacuous if the limiter is not armed
	// in this configuration — an inert limiter produces the identical
	// reading. The same volume of VALID requests from a different IP must
	// actually hit it.
	var limited bool
	for i := 0; i < flood; i++ {
		if doRequestFromRemoteAddr(srv, "GET", okTarget, nil, bystander).Code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatalf("premise broken: %d valid requests from one IP were never rate limited, "+
			"so this test cannot distinguish an unmetered rejection from a disabled limiter", flood)
	}
}
