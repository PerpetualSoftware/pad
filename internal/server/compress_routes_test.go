package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// TASK-2225, lead condition: the GET responses compressSkips leaves alone
// because they carry a secret are a DENY list, and a deny list rots: the next
// GET route that returns a secret would be compressed silently. This census
// holds every GET route the router serves, each classified compress or skip
// by a person, and fails when the router has a route the census does not, or
// the other way round, or when compressSkips disagrees with the census.
//
// A new GET route: decide whether its response can carry a live secret
// (a token, a code, a signed URL, an unmasked secret). If it can, add it to
// compressSkips; then regenerate with
//
//	PAD_UPDATE_COMPRESS_ROUTES=1 go test ./internal/server -run TestCompressRouteCensus
//
// and review the diff of testdata/compress_get_routes.txt as the decision it
// is.
const compressCensusFile = "testdata/compress_get_routes.txt"

var routeParam = regexp.MustCompile(`\{[^}]*\}`)

// sampleRequest is a GET for a concrete path the pattern matches.
func sampleRequest(pattern string) *http.Request {
	p := routeParam.ReplaceAllString(pattern, "x")
	p = strings.TrimSuffix(p, "/*") + "/x"
	if !strings.HasSuffix(pattern, "/*") {
		p = strings.TrimSuffix(p, "/x")
	}
	return httptest.NewRequest(http.MethodGet, p, nil)
}

func TestCompressRouteCensus(t *testing.T) {
	srv := testServer(t)
	// Built lazily: a server that has served nothing has a nil router.
	srv.ensureRouter()
	var routes []string
	if err := chi.Walk(srv.router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if method == http.MethodGet {
			routes = append(routes, route)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(routes)
	if len(routes) < 50 {
		t.Fatalf("walked only %d GET routes: re-point the census", len(routes))
	}

	actual := make([]string, 0, len(routes))
	for _, rt := range routes {
		class := "compress"
		if compressSkips(sampleRequest(rt)) {
			class = "skip"
		}
		actual = append(actual, class+" "+rt)
	}

	if os.Getenv("PAD_UPDATE_COMPRESS_ROUTES") == "1" {
		body := "# Every GET route the router serves, with what compressSkips does to it\n" +
			"# (TASK-2225). Reviewed, not generated blindly: see compress_routes_test.go.\n" +
			strings.Join(actual, "\n") + "\n"
		if err := os.WriteFile(compressCensusFile, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}

	raw, err := os.ReadFile(compressCensusFile)
	if err != nil {
		t.Fatalf("read %s: %v", compressCensusFile, err)
	}
	reviewed := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		class, route, ok := strings.Cut(line, " ")
		if !ok {
			t.Fatalf("bad census line %q", line)
		}
		reviewed[route] = class
	}
	seen := map[string]bool{}
	for _, a := range actual {
		class, route, _ := strings.Cut(a, " ")
		seen[route] = true
		want, ok := reviewed[route]
		switch {
		case !ok:
			t.Errorf("GET %s is not in the census: decide whether its response can carry a secret, then regenerate (see the top of this file)", route)
		case want != class:
			t.Errorf("GET %s: the census says %s, compressSkips does %s", route, want, class)
		}
	}
	for route := range reviewed {
		if !seen[route] {
			t.Errorf("GET %s is in the census but the router no longer serves it: regenerate", route)
		}
	}
}
