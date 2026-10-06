package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3452: in-app tutorials. Pad Cloud's server fetches the getpad.dev
// catalog and posters and serves them same-origin; a self-hosted server never
// makes an outbound call and links out instead.

type tutorialsRoundTrip func(*http.Request) (*http.Response, error)

func (f tutorialsRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func tutorialsResponse(status int, ctype, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {ctype}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

const tutorialsTestCatalog = `{
	"version": 1,
	"paths": [{"id": "getting-started", "title": "Getting started", "blurb": "b"}],
	"tutorials": [
		{"slug": "new-project", "title": "Start a new project with Pad", "path": "getting-started", "youtube_id": null, "seconds": 149,
		 "poster": "https://www.getpad.dev/learn/new-project.jpg", "chapters": [{"t": 12.9, "title": "Create your account"}],
		 "url": "https://www.getpad.dev/learn/new-project", "next": "onboard"},
		{"slug": "elsewhere", "title": "Poster off getpad.dev", "path": "getting-started", "youtube_id": null, "seconds": 10,
		 "poster": "https://evil.example/x.jpg", "chapters": [], "url": "https://www.getpad.dev/learn", "next": null},
		{"slug": "first-task", "title": "Work through your first task", "path": "getting-started", "youtube_id": null, "seconds": null,
		 "poster": null, "chapters": [], "url": "https://www.getpad.dev/learn", "next": null}
	],
	"demos": []
}`

// cloudTutorialsServer is a cloud-mode server whose outbound client answers
// from the fakes and counts what it was asked for.
func cloudTutorialsServer(t *testing.T, catalogStatus int) (*Server, *[]string) {
	t.Helper()
	srv := testServer(t)
	srv.SetCloudMode("secret-for-tutorials")
	var asked []string
	srv.tutorialsHTTP = &http.Client{Timeout: time.Second, Transport: tutorialsRoundTrip(func(r *http.Request) (*http.Response, error) {
		asked = append(asked, r.URL.String())
		switch r.URL.String() {
		case tutorialsCatalogURL:
			return tutorialsResponse(catalogStatus, "application/json", tutorialsTestCatalog), nil
		case "https://www.getpad.dev/learn/new-project.jpg":
			return tutorialsResponse(200, "image/jpeg", "JPEGBYTES"), nil
		}
		return tutorialsResponse(404, "text/plain", "no"), nil
	})}
	return srv, &asked
}

func TestTASK3452_SelfHostedNeverFetchesAndAddsNoFrameSrc(t *testing.T) {
	srv := testServer(t)
	var calls atomic.Int32
	srv.tutorialsHTTP = &http.Client{Transport: tutorialsRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, io.EOF
	})}
	for _, path := range []string{"/api/v1/tutorials", "/api/v1/tutorials/posters/new-project"} {
		if rr := doRequest(srv, "GET", path, nil); rr.Code != http.StatusNotFound {
			t.Errorf("self-hosted %s: %d %s, want 404", path, rr.Code, rr.Body.String())
		}
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("a self-hosted server made %d outbound tutorial requests, want 0", n)
	}
	srv.SetWebUI(webFS(map[string]string{"index.html": "<!doctype html><html><head></head></html>"}))
	rr := doRequest(srv, "GET", "/", nil)
	if csp := rr.Header().Get("Content-Security-Policy"); strings.Contains(csp, "frame-src") {
		t.Errorf("self-hosted page CSP carries frame-src: %s", csp)
	}
}

func TestTASK3452_CloudServesTheCatalogSameOrigin(t *testing.T) {
	srv, asked := cloudTutorialsServer(t, 200)
	rr := doRequest(srv, "GET", "/api/v1/tutorials", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /tutorials: %d %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Available bool            `json:"available"`
		Tutorials []tutorialEntry `json:"tutorials"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Available || len(body.Tutorials) != 3 {
		t.Fatalf("available=%v tutorials=%d, want true/3", body.Available, len(body.Tutorials))
	}
	if p := body.Tutorials[0].Poster; p == nil || *p != "/api/v1/tutorials/posters/new-project" {
		t.Errorf("poster not rewritten same-origin: %v", p)
	}
	if body.Tutorials[2].Poster != nil {
		t.Errorf("a tutorial with no poster gained one: %v", *body.Tutorials[2].Poster)
	}
	// Cached: a second read fetches nothing.
	before := len(*asked)
	doRequest(srv, "GET", "/api/v1/tutorials", nil)
	if len(*asked) != before {
		t.Errorf("a second read refetched the catalog: %v", *asked)
	}

	rr = doRequest(srv, "GET", "/api/v1/tutorials/posters/new-project", nil)
	if rr.Code != http.StatusOK || rr.Body.String() != "JPEGBYTES" || rr.Header().Get("Content-Type") != "image/jpeg" {
		t.Errorf("poster proxy: %d %q %q", rr.Code, rr.Header().Get("Content-Type"), rr.Body.String())
	}
	// A poster the catalog places off getpad.dev is never fetched.
	rr = doRequest(srv, "GET", "/api/v1/tutorials/posters/elsewhere", nil)
	if rr.Code == http.StatusOK {
		t.Errorf("a poster off getpad.dev was served: %d", rr.Code)
	}
	for _, u := range *asked {
		if strings.Contains(u, "evil.example") {
			t.Errorf("the server fetched a poster off getpad.dev: %s", u)
		}
	}

	srv.SetWebUI(webFS(map[string]string{"index.html": "<!doctype html><html><head></head></html>"}))
	page := doRequest(srv, "GET", "/", nil)
	if csp := page.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-src https://www.youtube-nocookie.com;") {
		t.Errorf("cloud page CSP lacks the youtube-nocookie frame-src: %s", csp)
	}
}

func TestTASK3452_CloudCatalogUnavailableFallsBackToLinks(t *testing.T) {
	srv, _ := cloudTutorialsServer(t, http.StatusServiceUnavailable)
	rr := doRequest(srv, "GET", "/api/v1/tutorials", nil)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"available":false`) {
		t.Errorf("unavailable catalog: %d %s, want 200 available:false", rr.Code, rr.Body.String())
	}
}

func TestTASK3452_UIDismissals(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		u, err := srv.store.CreateUser(models.UserCreate{Email: "dismiss-3452@example.com", Name: "D", Username: "dismiss-3452", Password: "pw-test-12345"})
		if err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		token, err := srv.store.CreateSession(u.ID, "go-test", "192.0.2.1", "go-test", 24*time.Hour)
		if err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		list := func() string {
			rr := doRequestWithCookie(srv, "GET", "/api/v1/me/ui-dismissals", nil, token)
			if rr.Code != http.StatusOK {
				t.Fatalf("list: %d %s", rr.Code, rr.Body.String())
			}
			return strings.TrimSpace(rr.Body.String())
		}
		if got := list(); got != `{"dismissed":[]}` {
			t.Errorf("fresh user: %s", got)
		}
		for i := 0; i < 2; i++ { // idempotent
			if rr := doRequestWithCookie(srv, "PUT", "/api/v1/me/ui-dismissals/tutorials.launchpad", nil, token); rr.Code != http.StatusOK {
				t.Fatalf("dismiss: %d %s", rr.Code, rr.Body.String())
			}
		}
		doRequestWithCookie(srv, "PUT", "/api/v1/me/ui-dismissals/tutorials.console", nil, token)
		if got := list(); got != `{"dismissed":["tutorials.console","tutorials.launchpad"]}` {
			t.Errorf("after two dismissals: %s", got)
		}
		if rr := doRequestWithCookie(srv, "PUT", "/api/v1/me/ui-dismissals/anything-else", nil, token); rr.Code != http.StatusBadRequest {
			t.Errorf("unknown key: %d, want 400", rr.Code)
		}
		if got := list(); strings.Contains(got, "anything-else") {
			t.Errorf("an unknown key was stored: %s", got)
		}
	})
}

// codex round 1: an allowed getpad.dev poster URL that redirects elsewhere is
// not followed.
func TestTASK3452_PosterRedirectIsNotFollowed(t *testing.T) {
	srv := testServer(t)
	srv.SetCloudMode("secret-for-tutorials")
	var asked []string
	srv.tutorialsHTTP = &http.Client{Timeout: time.Second, Transport: tutorialsRoundTrip(func(r *http.Request) (*http.Response, error) {
		asked = append(asked, r.URL.String())
		switch r.URL.String() {
		case tutorialsCatalogURL:
			return tutorialsResponse(200, "application/json", tutorialsTestCatalog), nil
		case "https://www.getpad.dev/learn/new-project.jpg":
			resp := tutorialsResponse(http.StatusFound, "text/plain", "")
			resp.Header.Set("Location", "http://169.254.169.254/latest/meta-data")
			return resp, nil
		}
		return tutorialsResponse(200, "image/jpeg", "SHOULD-NOT-BE-FETCHED"), nil
	})}
	rr := doRequest(srv, "GET", "/api/v1/tutorials/posters/new-project", nil)
	if rr.Code == http.StatusOK {
		t.Errorf("a redirected poster was served: %q", rr.Body.String())
	}
	for _, u := range asked {
		if strings.Contains(u, "169.254") {
			t.Errorf("the server followed a poster redirect to %s", u)
		}
	}
}

// codex round 1: a stale catalog is served at once while a slow refresh runs;
// no request waits on the network when a copy is in hand.
func TestTASK3452_StaleCatalogServedWhileUpstreamHangs(t *testing.T) {
	srv := testServer(t)
	srv.SetCloudMode("secret-for-tutorials")
	release := make(chan struct{})
	defer close(release)
	srv.tutorialsHTTP = &http.Client{Timeout: 10 * time.Second, Transport: tutorialsRoundTrip(func(r *http.Request) (*http.Response, error) {
		<-release
		return tutorialsResponse(503, "text/plain", ""), nil
	})}
	var cat tutorialCatalog
	if err := json.Unmarshal([]byte(tutorialsTestCatalog), &cat); err != nil {
		t.Fatal(err)
	}
	srv.tutorials.catalog = &cat
	srv.tutorials.fetchedAt = time.Now().Add(-2 * tutorialsTTL)

	start := time.Now()
	rr := doRequest(srv, "GET", "/api/v1/tutorials", nil)
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("a request with a stale copy waited %s on the upstream", took)
	}
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"available":true`) {
		t.Errorf("stale catalog not served: %d %s", rr.Code, rr.Body.String())
	}
}

// codex round 1: two dismissals racing from two devices both land, on both
// backends (SQLite serializes writers with BEGIN IMMEDIATE; Postgres takes
// the users row FOR NO KEY UPDATE).
func TestTASK3452_ConcurrentDismissalsBothLand(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		for round := 0; round < 5; round++ {
			u, err := srv.store.CreateUser(models.UserCreate{Email: fmt.Sprintf("race-%d-3452@example.com", round), Name: "R", Username: fmt.Sprintf("race-%d-3452", round), Password: "pw-test-12345"})
			if err != nil {
				t.Fatalf("CreateUser: %v", err)
			}
			errs := make(chan error, 2)
			for _, k := range models.UIDismissalKeys {
				go func(k string) { _, err := srv.store.DismissUI(u.ID, k); errs <- err }(k)
			}
			for range models.UIDismissalKeys {
				if err := <-errs; err != nil {
					t.Fatalf("round %d: DismissUI: %v", round, err)
				}
			}
			got, err := srv.store.ListUIDismissals(u.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(models.UIDismissalKeys) {
				t.Fatalf("round %d: %v, want both keys", round, got)
			}
		}
	})
}
