package server

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

const identityChallenge = "ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8"

func identityTestServer(t *testing.T, offset byte) *Server {
	t.Helper()
	s := testServer(t)
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i) + offset
	}
	s.store.SetEncryptionKey(key)
	return s
}

func identityResponse(t *testing.T, rr *httptest.ResponseRecorder) map[string]json.RawMessage {
	t.Helper()
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var out map[string]json.RawMessage
	parseJSON(t, rr, &out)
	return out
}

func verifyIdentityTranscript(t *testing.T, rr *httptest.ResponseRecorder) {
	t.Helper()
	out := identityResponse(t, rr)
	keys := map[string]bool{}
	for k := range out {
		keys[k] = true
	}
	if !reflect.DeepEqual(keys, map[string]bool{"version": true, "algorithm": true, "public_key": true, "signature": true}) {
		t.Fatalf("unexpected members: %v", keys)
	}
	if string(out["version"]) != "1" || string(out["algorithm"]) != `"ed25519"` {
		t.Fatalf("protocol: %s", rr.Body.String())
	}
	decode := func(name string) []byte {
		var value string
		if err := json.Unmarshal(out[name], &value); err != nil {
			t.Fatal(err)
		}
		b, err := base64.RawURLEncoding.Strict().DecodeString(value)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	pub, sig := decode("public_key"), decode("signature")
	if len(pub) != 32 || len(sig) != 64 {
		t.Fatal("wrong key or signature length")
	}
	// Build the transcript independently from the protocol's literal bytes.
	message := []byte("pad-server-identity/v1/ed25519/challenge\x00")
	message = append(message, pub...)
	message = append(message, []byte{0x20, 0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, 0x28, 0x29, 0x2a, 0x2b, 0x2c, 0x2d, 0x2e, 0x2f, 0x30, 0x31, 0x32, 0x33, 0x34, 0x35, 0x36, 0x37, 0x38, 0x39, 0x3a, 0x3b, 0x3c, 0x3d, 0x3e, 0x3f}...)
	if len(message) != 105 || !ed25519.Verify(pub, message, sig) {
		t.Fatal("invalid exact transcript signature")
	}
	if rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("missing no-store")
	}
}

func TestServerIdentitySignsExactTranscript(t *testing.T) {
	s := identityTestServer(t, 0)
	rr := doRequest(s, "GET", "/api/v1/server/identity?challenge="+identityChallenge, nil)
	verifyIdentityTranscript(t, rr)
	out := identityResponse(t, rr)
	var encoded string
	if err := json.Unmarshal(out["public_key"], &encoded); err != nil {
		t.Fatal(err)
	}
	pub, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(pub) != "a3383163144f101b00c64da51cc890afa84413255dc552b5e8e025b7389828ec" {
		t.Fatal("derivation label changed")
	}
}

func TestServerIdentitySharedAcrossInstances(t *testing.T) {
	var pubs []json.RawMessage
	for _, offset := range []byte{0, 0, 1} {
		s := identityTestServer(t, offset)
		out := identityResponse(t, doRequest(s, "GET", "/api/v1/server/identity?challenge="+identityChallenge, nil))
		pubs = append(pubs, out["public_key"])
	}
	if !bytes.Equal(pubs[0], pubs[1]) || bytes.Equal(pubs[0], pubs[2]) {
		t.Fatal("identity does not follow encryption key")
	}
}

func TestServerIdentityRejectsBadChallenge(t *testing.T) {
	s := identityTestServer(t, 0)
	cases := []string{"", "challenge=" + identityChallenge[:42], "challenge=" + identityChallenge + "A", "challenge=" + url.QueryEscape(identityChallenge+"="), "challenge=" + url.QueryEscape("+"+identityChallenge[1:]), "challenge=" + url.QueryEscape("/"+identityChallenge[1:]), "challenge=" + identityChallenge[:42] + "9", "challenge=" + url.QueryEscape(identityChallenge[:20]+"\n"+identityChallenge[21:]), "challenge=" + identityChallenge + "&challenge=" + identityChallenge, "challenge=" + url.QueryEscape(identityChallenge[:20]+"\n"+identityChallenge[20:])}
	var body string
	for i, q := range cases {
		t.Run(strings.ReplaceAll(q, "/", "_"), func(t *testing.T) {
			rr := doRequest(s, "GET", "/api/v1/server/identity?"+q, nil)
			if rr.Code != 400 {
				t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
			}
			if i == 0 {
				body = rr.Body.String()
			} else if body != rr.Body.String() {
				t.Fatal("different error body")
			}
			if !strings.Contains(body, `"invalid_challenge"`) || strings.Contains(body, `"signature"`) {
				t.Fatalf("unexpected error: %s", body)
			}
		})
	}
}

func TestServerIdentityOutsideAuthStack(t *testing.T) {
	s := identityTestServer(t, 0)
	if _, err := s.store.CreateUser(models.UserCreate{Email: "identity@example.com", Name: "Identity", Password: "pw-test-12345"}); err != nil {
		t.Fatal(err)
	}
	// The /api/v1 mount requires authentication once a user exists.
	if rr := doRequest(s, "GET", "/api/v1/workspaces", nil); rr.Code != 401 {
		t.Fatalf("auth stack control: %d", rr.Code)
	}
	var body string
	for i, credential := range []string{"", "bearer", "cookie"} {
		req := httptest.NewRequest("GET", "/api/v1/server/identity?challenge="+identityChallenge, nil)
		req.RemoteAddr = "192.0.2.1:1234"
		req.Header.Set("Origin", "https://untrusted.example")
		if credential == "bearer" {
			req.Header.Set("Authorization", "Bearer invalid")
		}
		if credential == "cookie" {
			req.AddCookie(&http.Cookie{Name: "__Host-pad_session", Value: "invalid"})
		}
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, req)
		verifyIdentityTranscript(t, rr)
		if i == 0 {
			body = rr.Body.String()
		} else if rr.Body.String() != body {
			t.Fatal("credential affected response")
		}
		if len(rr.Header().Values("Set-Cookie")) != 0 {
			t.Fatal("sets cookie")
		}
		for k := range rr.Header() {
			if strings.HasPrefix(strings.ToLower(k), "access-control-") {
				t.Fatalf("CORS header %s", k)
			}
		}
	}
}

func TestServerIdentityNoDatabase(t *testing.T) {
	s := identityTestServer(t, 0)
	verifyIdentityTranscript(t, doRequest(s, "GET", "/api/v1/server/identity?challenge="+identityChallenge, nil))
	if err := s.store.DB().Close(); err != nil {
		t.Fatal(err)
	}
	verifyIdentityTranscript(t, doRequest(s, "GET", "/api/v1/server/identity?challenge="+identityChallenge, nil))
}

func TestServerIdentityAnyHost(t *testing.T) {
	s := identityTestServer(t, 0)
	req := httptest.NewRequest("GET", "http://unconfigured.example/api/v1/server/identity?challenge="+identityChallenge, nil)
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	verifyIdentityTranscript(t, rr)
}

func TestServerIdentityRateLimited(t *testing.T) {
	s := identityTestServer(t, 0)
	for i := 0; i < 120; i++ {
		rr := doRequest(s, "GET", "/api/v1/server/identity?challenge="+identityChallenge, nil)
		if rr.Code != 200 {
			t.Fatalf("request %d: %d", i, rr.Code)
		}
	}
	rr := doRequest(s, "GET", "/api/v1/server/identity", nil)
	if rr.Code != 429 {
		t.Fatalf("burst+1: %d", rr.Code)
	}
	verifyIdentityTranscript(t, doRequestFromRemoteAddr(s, "GET", "/api/v1/server/identity?challenge="+identityChallenge, nil, "192.0.2.2:1234"))
}

func TestServerIdentityNoEncryptionKey(t *testing.T) {
	s := testServer(t)
	s.store.SetEncryptionKey(nil)
	rr := doRequest(s, "GET", "/api/v1/server/identity?challenge="+identityChallenge, nil)
	if rr.Code != 503 || !strings.Contains(rr.Body.String(), `"server_identity_unavailable"`) || strings.Contains(rr.Body.String(), `"signature"`) {
		t.Fatalf("%d: %s", rr.Code, rr.Body.String())
	}
}
