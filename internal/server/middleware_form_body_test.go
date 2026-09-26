package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/oauth"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// oauthFormBodyServer is oauthEnabledTestServer on a chosen dialect. The
// defect BUG-2811 closes answered differently on SQLite and Postgres, so the
// wired test has to run on both.
func oauthFormBodyServer(t *testing.T, driver store.DriverType) *Server {
	t.Helper()
	var s *store.Store
	if driver == store.DriverPostgres {
		s = storetest.NewPostgres(t) // skips when PAD_TEST_POSTGRES_URL is unset
	} else {
		s = storetest.NewSQLite(t)
	}
	if got := s.D().Driver(); got != driver {
		t.Fatalf("wanted a %s store, got %s", driver, got)
	}
	srv := New(s)
	t.Cleanup(func() { srv.Stop() })
	srv.SetCloudMode("test-secret")
	stub := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {})
	srv.SetMCPTransport(stub, testCanonicalAudience, testAuthServerURL, nil)
	o, err := oauth.NewServer(oauth.Config{
		Store:           srv.store,
		HMACSecret:      bytes32ForTest(),
		AllowedAudience: testCanonicalAudience,
	})
	if err != nil {
		t.Fatalf("oauth.NewServer: %v", err)
	}
	srv.SetOAuthServer(o)
	return srv
}

// TestOAuthFormBodyRefusesUnbindableText drives the three sinks measured to
// answer 500 (BUG-2811 checkpoint 1) through the real router. The control
// leg of each asserts the status it answered BEFORE the fix, so a
// validator that refused everything, or that broke the body handed on to
// the handler, fails here rather than passing as "400".
func TestOAuthFormBodyRefusesUnbindableText(t *testing.T) {
	for _, driver := range []store.DriverType{store.DriverSQLite, store.DriverPostgres} {
		t.Run(string(driver), func(t *testing.T) {
			srv := oauthFormBodyServer(t, driver)
			user, sess := loginTestUser(t, srv)
			clientID := registerTestClient(t, srv, "https://app.test/cb")
			csrf := readCSRFFromCookie(t, srv, sess)
			mustSeedWorkspaceWithRole(t, srv, user.ID, "Alpha", "alpha-2811", "owner")
			const verifier = "verifier-2811-quick-brown-fox-1234567890-abcdef"

			decide := func(name string) *httptest.ResponseRecorder {
				return postFormWithCookie(srv, "/oauth/authorize/decide", url.Values{
					"client_id": {clientID}, "response_type": {"code"}, "redirect_uri": {"https://app.test/cb"},
					"code_challenge": {s256Challenge(verifier)}, "code_challenge_method": {"S256"},
					"scope": {"pad:read"}, "audience": {testCanonicalAudience}, "state": {"state-2811-abc"},
					"decision": {"approve"}, "csrf_token": {csrf}, "capability_tier": {"read"},
					"connection_name": {name}, "workspace_access": {"specific"},
					"allowed_workspaces": {"alpha-2811"},
				}, sess, csrf)
			}
			token := func(form url.Values) *httptest.ResponseRecorder {
				req := httptest.NewRequest("POST", "/oauth/token", strings.NewReader(form.Encode()))
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				req.RemoteAddr = "192.0.2.1:1234"
				rr := httptest.NewRecorder()
				srv.ServeHTTP(rr, req)
				return rr
			}
			code := func(v string) *httptest.ResponseRecorder {
				return token(url.Values{
					"grant_type": {"authorization_code"}, "code": {v}, "client_id": {clientID},
					"redirect_uri": {"https://app.test/cb"}, "code_verifier": {verifier},
				})
			}
			refresh := func(v string) *httptest.ResponseRecorder {
				return token(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {v}, "client_id": {clientID}})
			}

			legs := []struct {
				name string
				send func(string) *httptest.ResponseRecorder
				ctrl string
				want int // the control's status, unchanged by the fix
			}{
				{"decide connection_name", decide, "Cursor on laptop", http.StatusSeeOther},
				{"token code", code, "junk.code", http.StatusBadRequest},
				{"token refresh_token", refresh, "junk.rt", http.StatusBadRequest},
			}
			for _, leg := range legs {
				if rr := leg.send(leg.ctrl); rr.Code != leg.want {
					t.Fatalf("%s control: got %d, want %d (body %s)", leg.name, rr.Code, leg.want, rr.Body.String())
				}
				for _, bad := range []string{"a\x00b", "a\xffb"} {
					rr := leg.send(leg.ctrl + bad)
					if rr.Code != http.StatusBadRequest {
						t.Fatalf("%s %q: got %d, want 400 (body %s)", leg.name, bad, rr.Code, rr.Body.String())
					}
					var body map[string]string
					if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil || body["error"] != "invalid_request" ||
						!strings.Contains(body["error_description"], "invalid UTF-8 or a NUL") {
						t.Fatalf("%s %q: body %s is not the form-body refusal", leg.name, bad, rr.Body.String())
					}
				}
			}
		})
	}
}

// formBodyEcho is a handler that reports what ParseForm gave it, so the
// unit tests below can assert the middleware hands the handler the body it
// would have read without the middleware.
func formBodyEcho(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "parse: "+err.Error(), http.StatusTeapot)
		return
	}
	_ = json.NewEncoder(w).Encode(r.PostForm)
}

func serveFormBody(h http.HandlerFunc, ct string, body io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/", body)
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	rr := httptest.NewRecorder()
	h(rr, req)
	return rr
}

func TestValidateFormBody(t *testing.T) {
	h := ValidateFormBody(formBodyEcho)
	const form = "application/x-www-form-urlencoded"

	t.Run("valid body reaches the handler unchanged", func(t *testing.T) {
		rr := serveFormBody(h, form, strings.NewReader("a=x+y&b=%C3%A9&c=1&c=2"))
		if rr.Code != http.StatusOK || strings.TrimSpace(rr.Body.String()) != `{"a":["x y"],"b":["é"],"c":["1","2"]}` {
			t.Fatalf("got %d %s", rr.Code, rr.Body.String())
		}
	})

	for _, tc := range []struct{ name, ct, body string }{
		{"escaped NUL value", form, "a=%00"},
		{"escaped invalid UTF-8 value", form, "a=%FF"},
		{"raw NUL", form, "a=x\x00y"},
		{"raw invalid UTF-8", form, "a=\xff"},
		{"bad key", form, "%FF=1"},
		// The media type is compared after parameters are stripped, as
		// parsePostForm compares it.
		{"charset parameter", form + "; charset=UTF-8", "a=%00"},
		{"upper-case media type", "Application/X-WWW-Form-URLEncoded", "a=%00"},
	} {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			if rr := serveFormBody(h, tc.ct, strings.NewReader(tc.body)); rr.Code != http.StatusBadRequest {
				t.Fatalf("got %d %s, want 400", rr.Code, rr.Body.String())
			}
		})
	}

	t.Run("other content types are not read", func(t *testing.T) {
		for _, ct := range []string{"application/json", "multipart/form-data; boundary=x", ""} {
			rr := serveFormBody(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				_, _ = w.Write(b)
			}, ct, strings.NewReader(`{"a":"\u0000"}`+"\xff"))
			if rr.Code != http.StatusOK || rr.Body.String() != `{"a":"\u0000"}`+"\xff" {
				t.Fatalf("%q: got %d %q", ct, rr.Code, rr.Body.String())
			}
		}
	})

	// A body over net/http's cap is not checked: the handler's own
	// ParseForm refuses it as too large, exactly as without the middleware.
	t.Run("over-cap body is left to ParseForm", func(t *testing.T) {
		big := "a=" + strings.Repeat("x", formBodyParseCap) + "%00"
		rr := serveFormBody(h, form, strings.NewReader(big))
		if rr.Code != http.StatusTeapot || !strings.Contains(rr.Body.String(), "too large") {
			t.Fatalf("got %d %.80s", rr.Code, rr.Body.String())
		}
	})

	t.Run("read error is replayed to the handler", func(t *testing.T) {
		boom := errors.New("client went away")
		rr := serveFormBody(h, form, io.MultiReader(strings.NewReader("a=1&"), errReader{boom}))
		if rr.Code != http.StatusTeapot || !strings.Contains(rr.Body.String(), boom.Error()) {
			t.Fatalf("got %d %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("GET is not read", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", strings.NewReader("a=%00"))
		req.Header.Set("Content-Type", form)
		rr := httptest.NewRecorder()
		h(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("got %d", rr.Code)
		}
	})
}
