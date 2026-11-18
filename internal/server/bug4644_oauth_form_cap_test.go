package server

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// BUG-4644: /oauth/token, /oauth/revoke and /oauth/introspect read form
// bodies up to net/http's 10 MiB default, and fosite parses any
// client_assertion as a JWT before it looks the client up. Pad never accepts
// JWT client authentication, so the three endpoints now cap their bodies at
// 64 KiB and refuse a client assertion before fosite runs.

// bug4644FormCap is the cap the endpoints are expected to enforce, written
// out so the tests do not move if the constant does.
const bug4644FormCap = 64 << 10

// The refusal the handlers write themselves when the form cannot be read.
// fosite never writes this text, so seeing it means fosite was not reached.
const bug4644FormRefusal = "Invalid form body"

const bug4644AssertionType = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"

// bug4644Dummy is a short, ordinary three-segment token: enough to send
// fosite down its JWT branch, nothing more.
const bug4644Dummy = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0.c2ln"

// TestBUG4644_CapConstant pins the shipped cap to the value these tests use.
func TestBUG4644_CapConstant(t *testing.T) {
	t.Parallel()
	if oauthFormBodyCap != bug4644FormCap {
		t.Fatalf("oauthFormBodyCap = %d, tests assume %d", oauthFormBodyCap, bug4644FormCap)
	}
}

// bug4644Padded encodes form and appends a padding field so the body is
// exactly size bytes.
func bug4644Padded(t *testing.T, form url.Values, size int) string {
	t.Helper()
	base := form.Encode() + "&pad="
	n := size - len(base)
	if n < 0 {
		t.Fatalf("form already %d bytes, over the %d asked for", len(base), size)
	}
	body := base + strings.Repeat("a", n)
	if len(body) != size {
		t.Fatalf("padded body is %d bytes, want %d", len(body), size)
	}
	return body
}

func bug4644Post(srv *Server, path, contentType, body, bearer string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

const bug4644FormCT = "application/x-www-form-urlencoded"

// bug4644Multipart encodes form as multipart/form-data, padded with a field
// so the body is at least minSize bytes (0 for no padding).
func bug4644Multipart(t *testing.T, form url.Values, minSize int) (string, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, vs := range form {
		for _, v := range vs {
			if err := mw.WriteField(k, v); err != nil {
				t.Fatal(err)
			}
		}
	}
	if minSize > 0 {
		if err := mw.WriteField("pad", strings.Repeat("a", minSize)); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String(), mw.FormDataContentType()
}

// bug4644Active reports whether token introspects active for bearer.
func bug4644Active(t *testing.T, srv *Server, token, bearer string) bool {
	t.Helper()
	rr := postOAuthFormBearer(srv, "/oauth/introspect", url.Values{"token": {token}}, bearer)
	if rr.Code != http.StatusOK {
		t.Fatalf("introspect: %d %s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	parseJSON(t, rr, &resp)
	active, _ := resp["active"].(bool)
	return active
}

func bug4644TokenForm(clientID, code, verifier string) url.Values {
	return url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {clientID},
		"redirect_uri":  {"https://app.test/cb"},
		"code_verifier": {verifier},
		"audience":      {testCanonicalAudience},
	}
}

// TestBUG4644_Token_BodyCap: a token request one byte over the cap is
// refused by the handler's own form-parse path (not fosite), and the code
// it carried still exchanges in a request exactly at the cap.
func TestBUG4644_Token_BodyCap(t *testing.T) {
	t.Parallel()
	srv, _ := oauthEnabledTestServer(t)
	_, sess := loginTestUser(t, srv)
	csrf := readCSRFFromCookie(t, srv, sess)
	clientID := registerTestClient(t, srv, "https://app.test/cb")
	verifier := "verifier-bug4644-token-cap-quick-brown-fox-0123456789"
	code := authCodeFor(t, srv, sess, csrf, clientID, verifier, testCanonicalAudience)
	form := bug4644TokenForm(clientID, code, verifier)

	over := bug4644Post(srv, "/oauth/token", bug4644FormCT, bug4644Padded(t, form, bug4644FormCap+1), "")
	if over.Code != http.StatusBadRequest || !strings.Contains(over.Body.String(), bug4644FormRefusal) {
		t.Fatalf("over-cap token request: got %d %.200s, want 400 %q", over.Code, over.Body.String(), bug4644FormRefusal)
	}

	at := bug4644Post(srv, "/oauth/token", bug4644FormCT, bug4644Padded(t, form, bug4644FormCap), "")
	if at.Code != http.StatusOK {
		t.Fatalf("token request at the cap: got %d %.300s, want 200", at.Code, at.Body.String())
	}
	var resp map[string]any
	parseJSON(t, at, &resp)
	if tok, _ := resp["access_token"].(string); tok == "" {
		t.Fatalf("no access_token at the cap: %v", resp)
	}
}

// TestBUG4644_Token_BodyCapMultipart: the cap governs a multipart token
// body too, which ValidateFormBody does not read.
func TestBUG4644_Token_BodyCapMultipart(t *testing.T) {
	t.Parallel()
	srv, _ := oauthEnabledTestServer(t)
	_, sess := loginTestUser(t, srv)
	csrf := readCSRFFromCookie(t, srv, sess)
	clientID := registerTestClient(t, srv, "https://app.test/cb")
	verifier := "verifier-bug4644-token-mp-quick-brown-fox-0123456789"
	code := authCodeFor(t, srv, sess, csrf, clientID, verifier, testCanonicalAudience)
	form := bug4644TokenForm(clientID, code, verifier)

	body, ct := bug4644Multipart(t, form, bug4644FormCap)
	over := bug4644Post(srv, "/oauth/token", ct, body, "")
	if over.Code != http.StatusBadRequest || !strings.Contains(over.Body.String(), bug4644FormRefusal) {
		t.Fatalf("over-cap multipart token request: got %d %.200s, want 400 %q", over.Code, over.Body.String(), bug4644FormRefusal)
	}
	// The code was not consumed: it still exchanges.
	if rr := postOAuthForm(srv, "/oauth/token", form); rr.Code != http.StatusOK {
		t.Fatalf("exchange after refused multipart: got %d %.300s", rr.Code, rr.Body.String())
	}
}

// TestBUG4644_Revoke_BodyCap: an over-cap revoke is refused before fosite
// and revokes nothing; the same revoke at the cap succeeds.
func TestBUG4644_Revoke_BodyCap(t *testing.T) {
	t.Parallel()
	srv, _ := oauthEnabledTestServer(t)
	_, sess := loginTestUser(t, srv)
	csrf := readCSRFFromCookie(t, srv, sess)
	clientID := registerTestClient(t, srv, "https://app.test/cb")
	target, _ := runAuthCodeFlow(t, srv, sess, csrf, clientID, "verifier-bug4644-rev-tgt-quick-brown-fox-012345")["access_token"].(string)
	bearer, _ := runAuthCodeFlow(t, srv, sess, csrf, clientID, "verifier-bug4644-rev-brr-quick-brown-fox-012345")["access_token"].(string)
	if target == "" || bearer == "" {
		t.Fatal("missing tokens")
	}
	form := url.Values{"token": {target}, "token_type_hint": {"access_token"}, "client_id": {clientID}}

	over := bug4644Post(srv, "/oauth/revoke", bug4644FormCT, bug4644Padded(t, form, bug4644FormCap+1), "")
	if over.Code != http.StatusBadRequest || !strings.Contains(over.Body.String(), bug4644FormRefusal) {
		t.Fatalf("over-cap revoke: got %d %.200s, want 400 %q", over.Code, over.Body.String(), bug4644FormRefusal)
	}
	if !bug4644Active(t, srv, target, bearer) {
		t.Fatal("over-cap revoke revoked the token")
	}

	at := bug4644Post(srv, "/oauth/revoke", bug4644FormCT, bug4644Padded(t, form, bug4644FormCap), "")
	if at.Code != http.StatusOK {
		t.Fatalf("revoke at the cap: got %d %.200s, want 200", at.Code, at.Body.String())
	}
	if bug4644Active(t, srv, target, bearer) {
		t.Fatal("revoke at the cap left the token active")
	}
}

// TestBUG4644_Introspect_BodyCap: an over-cap introspection is refused by
// the handler's own form-parse path; the same request at the cap answers.
func TestBUG4644_Introspect_BodyCap(t *testing.T) {
	t.Parallel()
	srv, _ := oauthEnabledTestServer(t)
	_, sess := loginTestUser(t, srv)
	csrf := readCSRFFromCookie(t, srv, sess)
	clientID := registerTestClient(t, srv, "https://app.test/cb")
	target, _ := runAuthCodeFlow(t, srv, sess, csrf, clientID, "verifier-bug4644-int-tgt-quick-brown-fox-012345")["access_token"].(string)
	bearer, _ := runAuthCodeFlow(t, srv, sess, csrf, clientID, "verifier-bug4644-int-brr-quick-brown-fox-012345")["access_token"].(string)
	if target == "" || bearer == "" {
		t.Fatal("missing tokens")
	}
	form := url.Values{"token": {target}}

	over := bug4644Post(srv, "/oauth/introspect", bug4644FormCT, bug4644Padded(t, form, bug4644FormCap+1), bearer)
	if over.Code != http.StatusBadRequest || !strings.Contains(over.Body.String(), bug4644FormRefusal) {
		t.Fatalf("over-cap introspect: got %d %.200s, want 400 %q", over.Code, over.Body.String(), bug4644FormRefusal)
	}

	mpBody, mpCT := bug4644Multipart(t, form, bug4644FormCap)
	overMP := bug4644Post(srv, "/oauth/introspect", mpCT, mpBody, bearer)
	if overMP.Code != http.StatusBadRequest || !strings.Contains(overMP.Body.String(), bug4644FormRefusal) {
		t.Fatalf("over-cap multipart introspect: got %d %.200s, want 400 %q", overMP.Code, overMP.Body.String(), bug4644FormRefusal)
	}

	at := bug4644Post(srv, "/oauth/introspect", bug4644FormCT, bug4644Padded(t, form, bug4644FormCap), bearer)
	if at.Code != http.StatusOK {
		t.Fatalf("introspect at the cap: got %d %.200s, want 200", at.Code, at.Body.String())
	}
	var resp map[string]any
	parseJSON(t, at, &resp)
	if active, _ := resp["active"].(bool); !active {
		t.Fatalf("introspect at the cap: want active, got %v", resp)
	}
}

// bug4644AssertionVariants are the client-assertion shapes refused: both
// fields (fosite's JWT branch), and each alone.
var bug4644AssertionVariants = []struct {
	name   string
	fields url.Values
}{
	{"type and assertion", url.Values{"client_assertion_type": {bug4644AssertionType}, "client_assertion": {bug4644Dummy}}},
	{"assertion only", url.Values{"client_assertion": {bug4644Dummy}}},
	{"type only", url.Values{"client_assertion_type": {bug4644AssertionType}}},
}

func bug4644With(base, extra url.Values) url.Values {
	out := url.Values{}
	for k, v := range base {
		out[k] = append([]string(nil), v...)
	}
	for k, v := range extra {
		out[k] = append(out[k], v...)
	}
	return out
}

// bug4644WantInvalidClient asserts a 401 RFC 6749 invalid_client and, when
// hint is set, that the description carries the handler's own hint (which
// fosite's JWT failure never produces).
func bug4644WantInvalidClient(t *testing.T, rr *httptest.ResponseRecorder, hint bool) {
	t.Helper()
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("got %d %.300s, want 401 invalid_client", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	parseJSON(t, rr, &resp)
	if e, _ := resp["error"].(string); e != "invalid_client" {
		t.Fatalf("error = %q, want invalid_client (%v)", e, resp)
	}
	if hint {
		if d, _ := resp["error_description"].(string); !strings.Contains(d, "does not accept client assertions") {
			t.Fatalf("error_description %q lacks the client-assertion hint", d)
		}
	}
}

// TestBUG4644_Token_RefusesClientAssertion: an otherwise valid code
// exchange carrying a client assertion is refused invalid_client before
// fosite, and the code is left unused.
func TestBUG4644_Token_RefusesClientAssertion(t *testing.T) {
	t.Parallel()
	srv, _ := oauthEnabledTestServer(t)
	_, sess := loginTestUser(t, srv)
	csrf := readCSRFFromCookie(t, srv, sess)
	clientID := registerTestClient(t, srv, "https://app.test/cb")
	for i, v := range bug4644AssertionVariants {
		t.Run(v.name, func(t *testing.T) {
			verifier := "verifier-bug4644-assert-" + string(rune('a'+i)) + "-quick-brown-fox-0123456789"
			code := authCodeFor(t, srv, sess, csrf, clientID, verifier, testCanonicalAudience)
			form := bug4644TokenForm(clientID, code, verifier)
			bug4644WantInvalidClient(t, postOAuthForm(srv, "/oauth/token", bug4644With(form, v.fields)), true)
			if rr := postOAuthForm(srv, "/oauth/token", form); rr.Code != http.StatusOK {
				t.Fatalf("exchange after refusal: got %d %.300s", rr.Code, rr.Body.String())
			}
		})
	}
	t.Run("multipart assertion only", func(t *testing.T) {
		verifier := "verifier-bug4644-assert-mp-quick-brown-fox-0123456789"
		code := authCodeFor(t, srv, sess, csrf, clientID, verifier, testCanonicalAudience)
		form := bug4644TokenForm(clientID, code, verifier)
		body, ct := bug4644Multipart(t, bug4644With(form, url.Values{"client_assertion": {bug4644Dummy}}), 0)
		bug4644WantInvalidClient(t, bug4644Post(srv, "/oauth/token", ct, body, ""), true)
		if rr := postOAuthForm(srv, "/oauth/token", form); rr.Code != http.StatusOK {
			t.Fatalf("exchange after refusal: got %d %.300s", rr.Code, rr.Body.String())
		}
	})
}

// TestBUG4644_Revoke_RefusesClientAssertion: a revoke of a real token
// carrying a client assertion is refused invalid_client and revokes nothing.
func TestBUG4644_Revoke_RefusesClientAssertion(t *testing.T) {
	t.Parallel()
	srv, _ := oauthEnabledTestServer(t)
	_, sess := loginTestUser(t, srv)
	csrf := readCSRFFromCookie(t, srv, sess)
	clientID := registerTestClient(t, srv, "https://app.test/cb")
	target, _ := runAuthCodeFlow(t, srv, sess, csrf, clientID, "verifier-bug4644-rva-tgt-quick-brown-fox-012345")["access_token"].(string)
	bearer, _ := runAuthCodeFlow(t, srv, sess, csrf, clientID, "verifier-bug4644-rva-brr-quick-brown-fox-012345")["access_token"].(string)
	if target == "" || bearer == "" {
		t.Fatal("missing tokens")
	}
	form := url.Values{"token": {target}, "token_type_hint": {"access_token"}, "client_id": {clientID}}
	for _, v := range bug4644AssertionVariants {
		t.Run(v.name, func(t *testing.T) {
			bug4644WantInvalidClient(t, postOAuthForm(srv, "/oauth/revoke", bug4644With(form, v.fields)), false)
			if !bug4644Active(t, srv, target, bearer) {
				t.Fatal("refused revoke revoked the token")
			}
		})
	}
}

// TestBUG4644_Introspect_RefusesClientAssertion: an introspection carrying
// a client assertion is refused invalid_client, with or without a bearer.
func TestBUG4644_Introspect_RefusesClientAssertion(t *testing.T) {
	t.Parallel()
	srv, _ := oauthEnabledTestServer(t)
	_, sess := loginTestUser(t, srv)
	csrf := readCSRFFromCookie(t, srv, sess)
	clientID := registerTestClient(t, srv, "https://app.test/cb")
	target, _ := runAuthCodeFlow(t, srv, sess, csrf, clientID, "verifier-bug4644-ina-tgt-quick-brown-fox-012345")["access_token"].(string)
	bearer, _ := runAuthCodeFlow(t, srv, sess, csrf, clientID, "verifier-bug4644-ina-brr-quick-brown-fox-012345")["access_token"].(string)
	if target == "" || bearer == "" {
		t.Fatal("missing tokens")
	}
	form := url.Values{"token": {target}}
	for _, v := range bug4644AssertionVariants {
		t.Run(v.name, func(t *testing.T) {
			bug4644WantInvalidClient(t, postOAuthFormBearer(srv, "/oauth/introspect", bug4644With(form, v.fields), bearer), true)
		})
		t.Run(v.name+" no bearer", func(t *testing.T) {
			bug4644WantInvalidClient(t, postOAuthForm(srv, "/oauth/introspect", bug4644With(form, v.fields)), true)
		})
	}
}
