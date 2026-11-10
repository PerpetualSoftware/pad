package cli

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestServerIdentityProbeOmitsCredentials(t *testing.T) {
	priv := ed25519.NewKeyFromSeed(make([]byte, 32))
	pub := priv.Public().(ed25519.PublicKey)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Pad-Agent") != "" {
			t.Error("probe carried credentials")
		}
		challenge, err := base64.RawURLEncoding.Strict().DecodeString(r.URL.Query().Get("challenge"))
		if err != nil || len(challenge) != 32 {
			t.Error("invalid challenge")
		}
		message := append([]byte("pad-server-identity/v1/ed25519/challenge\x00"), pub...)
		message = append(message, challenge...)
		_ = json.NewEncoder(w).Encode(map[string]any{"version": 1, "algorithm": "ed25519", "public_key": base64.RawURLEncoding.EncodeToString(pub), "signature": base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, message))})
	}))
	defer srv.Close()
	c := NewClientFromURLWithAgentName(srv.URL, "test-agent")
	c.httpClient = srv.Client()
	c.authToken = "test-token"
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	jar.SetCookies(u, []*http.Cookie{{Name: "session", Value: "test-cookie"}})
	c.httpClient.Jar = jar
	fingerprint, err := c.ServerIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if fingerprint != "139e-3940-e64b-5491-7220-88d9-a0d7-4162" || calls != 1 {
		t.Fatalf("fingerprint=%s calls=%d", fingerprint, calls)
	}
	c.baseURL = "http://user:password@" + u.Host + "/api/v1"
	if _, err := c.ServerIdentity(); err == nil || calls != 1 {
		t.Fatal("userinfo credentials were not rejected before request")
	}
}
