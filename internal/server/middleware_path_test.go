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
// Every case is a 400 before any handler runs. Against unfixed code these
// are 500 on Postgres and 404 on SQLite; either way, not 400.
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
