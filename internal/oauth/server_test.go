package oauth

import (
	"context"
	"errors"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ory/fosite"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// Tests for the OAuth server constructor + audience strategy +
// storage adapter (PLAN-943 TASK-951 sub-PR B). Each layer is
// covered:
//
//   - NewServer: required-field validation, default lifespan filling,
//     compile-time interface guarantees.
//   - audienceMatchingStrategy: the RFC 8707 hook that's the heart
//     of cross-server replay defense.
//   - Storage adapter: requesterToOAuthRequest + oauthRequestToFositeRequest
//     round-trip + the error mapping that bridges sub-PR A's sentinels
//     to fosite's error sentinels.
//
// The full HTTP handshake (DCR → /authorize → /token → /introspect)
// is covered by sub-PR C's handler tests; this file pins the
// constructor + adapter contracts so sub-PR C can rely on them
// without re-asserting fosite's behaviour on every handler test.

// testStoreOAuth produces a *store.Store with the migrations applied
// — same pattern internal/store/oauth_test.go uses, but importing
// /internal/store would create a test-only cycle. Inline a tiny
// helper here that opens a fresh SQLite in t.TempDir.
func testStoreOAuth(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := store.New(dbPath)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// =====================================================================
// NewServer — argument validation
// =====================================================================

func TestNewServer_RejectsMissingStore(t *testing.T) {
	_, err := NewServer(Config{
		HMACSecret:      bytes32(),
		AllowedAudience: "https://mcp.test.example/mcp",
	})
	if err == nil {
		t.Fatal("expected error when Store is nil")
	}
	if !strings.Contains(err.Error(), "Store is required") {
		t.Errorf("error doesn't mention Store: %v", err)
	}
}

func TestNewServer_RejectsShortHMACSecret(t *testing.T) {
	s := testStoreOAuth(t)
	_, err := NewServer(Config{
		Store:           s,
		HMACSecret:      []byte("too-short"),
		AllowedAudience: "https://mcp.test.example/mcp",
	})
	if err == nil {
		t.Fatal("expected error for HMAC secret < 32 bytes")
	}
	if !strings.Contains(err.Error(), "32 bytes") {
		t.Errorf("error doesn't mention 32 bytes: %v", err)
	}
}

func TestNewServer_RejectsMissingAudience(t *testing.T) {
	s := testStoreOAuth(t)
	_, err := NewServer(Config{
		Store:      s,
		HMACSecret: bytes32(),
	})
	if err == nil {
		t.Fatal("expected error when AllowedAudience is empty")
	}
	if !strings.Contains(err.Error(), "AllowedAudience") {
		t.Errorf("error doesn't mention AllowedAudience: %v", err)
	}
}

func TestNewServer_FillsDefaultLifespans(t *testing.T) {
	// Constructor pre-fills lifespans when not supplied. Pin the
	// defaults so a future bump (e.g. shortening access tokens) is
	// caught here rather than surprising downstream consumers.
	s := testStoreOAuth(t)
	srv, err := NewServer(Config{
		Store:           s,
		HMACSecret:      bytes32(),
		AllowedAudience: "https://mcp.test.example/mcp",
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if srv.Provider() == nil {
		t.Fatal("Provider() returned nil")
	}
	if srv.Storage() == nil {
		t.Fatal("Storage() returned nil")
	}
	if srv.AllowedAudience() != "https://mcp.test.example/mcp" {
		t.Errorf("AllowedAudience: got %q", srv.AllowedAudience())
	}
}

// =====================================================================
// audienceMatchingStrategy — RFC 8707 hook
// =====================================================================

func TestAudienceStrategy_RejectsEmptyNeedle(t *testing.T) {
	canonical := "https://mcp.test.example/mcp"
	strat := audienceMatchingStrategy(canonical)
	err := strat([]string{canonical}, []string{})
	if err == nil {
		t.Fatal("expected error for empty requested audience (RFC 8707 mandates resource= per PLAN-943)")
	}
	// Should be a fosite invalid_request, not internal error.
	if !errors.Is(err, fosite.ErrInvalidRequest) {
		t.Errorf("expected ErrInvalidRequest, got %v", err)
	}
}

func TestAudienceStrategy_RejectsMismatchedNeedle(t *testing.T) {
	canonical := "https://mcp.test.example/mcp"
	strat := audienceMatchingStrategy(canonical)
	err := strat([]string{canonical}, []string{"https://other.example/mcp"})
	if err == nil {
		t.Fatal("expected rejection of cross-server audience")
	}
	if !errors.Is(err, fosite.ErrInvalidRequest) {
		t.Errorf("expected ErrInvalidRequest, got %v", err)
	}
}

func TestAudienceStrategy_RejectsClientWithoutCanonical(t *testing.T) {
	// Belt-and-suspenders: client.Audience is supposed to contain
	// canonical (sub-PR C's DCR populates it), but if a stray
	// fixture / migration creates a client without it, the
	// validator must still reject. This pins the haystack-side
	// gate.
	canonical := "https://mcp.test.example/mcp"
	strat := audienceMatchingStrategy(canonical)
	err := strat([]string{} /* haystack: client has nothing */, []string{canonical})
	if err == nil {
		t.Fatal("expected rejection when client.Audience doesn't include canonical")
	}
}

func TestAudienceStrategy_AcceptsCanonicalOnly(t *testing.T) {
	canonical := "https://mcp.test.example/mcp"
	strat := audienceMatchingStrategy(canonical)
	if err := strat([]string{canonical}, []string{canonical}); err != nil {
		t.Errorf("canonical=canonical roundtrip must succeed; got %v", err)
	}
}

func TestAudienceStrategy_RejectsMultipleAudiences(t *testing.T) {
	// Even if one of the requested audiences is canonical, having
	// extras is rejected — RFC 8707 audience-restriction means
	// every issued token is scoped to ONLY one resource.
	canonical := "https://mcp.test.example/mcp"
	strat := audienceMatchingStrategy(canonical)
	err := strat([]string{canonical}, []string{canonical, "https://other.example/mcp"})
	if err == nil {
		t.Fatal("expected rejection when needle includes canonical AND another audience")
	}
}

func TestAudienceStrategy_NoCanonicalIsServerError(t *testing.T) {
	// A server constructed without a canonical audience is a
	// configuration bug; the strategy refuses validation so the
	// misconfigured handler errors at the first request rather
	// than silently issuing wide-open tokens.
	strat := audienceMatchingStrategy("")
	err := strat([]string{"anything"}, []string{"anything"})
	if err == nil {
		t.Fatal("expected ServerError when canonical is empty")
	}
	if !errors.Is(err, fosite.ErrServerError) {
		t.Errorf("expected ErrServerError, got %v", err)
	}
}

// =====================================================================
// ValidateAudienceParam — single-param helper for HTTP-handler entry
// =====================================================================

func TestValidateAudienceParam(t *testing.T) {
	canonical := "https://mcp.test.example/mcp"
	cases := []struct {
		name      string
		canonical string
		raw       string
		wantErr   bool
	}{
		{"canonical match", canonical, canonical, false},
		{"empty raw", canonical, "", true},
		{"empty canonical (server misconfig)", "", "anything", true},
		{"mismatched URL", canonical, "https://other.example/mcp", true},
		{"path mismatch", canonical, "https://mcp.test.example/other", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateAudienceParam(tc.canonical, tc.raw)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateAudienceParam(%q, %q): err=%v wantErr=%v", tc.canonical, tc.raw, err, tc.wantErr)
			}
		})
	}
}

func TestAudienceForNewClient_ProducesSingletonOrEmpty(t *testing.T) {
	// Sub-PR C's DCR handler will call this to seed every new
	// client's Audience to exactly [canonical]. Pin the shape.
	canonical := "https://mcp.test.example/mcp"
	got := audienceForNewClient(canonical)
	if len(got) != 1 || got[0] != canonical {
		t.Errorf("audienceForNewClient(%q) = %v, want [%q]", canonical, got, canonical)
	}

	if audienceForNewClient("") != nil {
		t.Error("empty canonical must produce nil to surface misconfiguration")
	}
}

// =====================================================================
// Session — cloning and accessor
// =====================================================================

func TestSession_CloneReturnsConcreteSession(t *testing.T) {
	// fosite calls Session.Clone() during refresh-token rotation
	// and other internal flows. Without our Clone override,
	// DefaultSession.Clone returns *DefaultSession; downstream
	// type-assertions to *Session in handler code would fail.
	s := NewSession("user-123")
	cloned := s.Clone()
	if cloned == nil {
		t.Fatal("Clone returned nil")
	}
	concrete, ok := cloned.(*Session)
	if !ok {
		t.Fatalf("Clone returned %T, want *Session", cloned)
	}
	if concrete.UserID() != "user-123" {
		t.Errorf("cloned subject not preserved: got %q", concrete.UserID())
	}
	// Mutating the clone must not affect the original.
	concrete.DefaultSession.Subject = "user-456"
	if s.UserID() == "user-456" {
		t.Error("Clone produced an aliased session — mutation leaked back to original")
	}
}

func TestSession_NilSafeAccessors(t *testing.T) {
	var s *Session // nil
	if s.UserID() != "" {
		t.Error("UserID() on nil session must return empty string")
	}
	if cloned := s.Clone(); cloned != nil {
		t.Error("Clone() on nil session must return nil")
	}
}

// =====================================================================
// Storage adapter — translation round-trip + error mapping
// =====================================================================

func TestStorage_AuthCodeRoundTrip(t *testing.T) {
	st := testStoreOAuth(t)
	storage := NewStorage(st)

	// Seed a client (the FK target).
	client, err := st.CreateOAuthClient(models.OAuthClientCreate{
		Name:         "Test Client",
		RedirectURIs: []string{"https://app.test/cb"},
		GrantTypes:   []string{"authorization_code"},
		Scopes:       []string{"pad:read"},
		Public:       true,
	})
	if err != nil {
		t.Fatalf("seed client: %v", err)
	}

	req := buildFositeRequest(client.ID, "user-1", []string{"pad:read"}, []string{"https://mcp.test.example/mcp"})

	const sig = "sig-1"
	if err := storage.CreateAuthorizeCodeSession(context.Background(), sig, req); err != nil {
		t.Fatalf("CreateAuthorizeCodeSession: %v", err)
	}

	// Get with a fresh empty session — fosite's handler chain hands
	// in a session pointer for hydration.
	hydrated := NewSession("")
	got, err := storage.GetAuthorizeCodeSession(context.Background(), sig, hydrated)
	if err != nil {
		t.Fatalf("GetAuthorizeCodeSession: %v", err)
	}
	if got.GetID() != req.GetID() {
		t.Errorf("ID round-trip: got %q, want %q", got.GetID(), req.GetID())
	}
	if hydrated.UserID() != "user-1" {
		t.Errorf("session subject not hydrated: got %q", hydrated.UserID())
	}
	if got.GetClient().GetID() != client.ID {
		t.Errorf("client ID lost: got %q", got.GetClient().GetID())
	}
	if !equalArguments(got.GetRequestedScopes(), fosite.Arguments{"pad:read"}) {
		t.Errorf("scopes lost: got %v", got.GetRequestedScopes())
	}
	if !equalArguments(got.GetRequestedAudience(), fosite.Arguments{"https://mcp.test.example/mcp"}) {
		t.Errorf("audience lost: got %v", got.GetRequestedAudience())
	}
}

func TestStorage_AuthCodeInvalidatedReturnsCorrectError(t *testing.T) {
	st := testStoreOAuth(t)
	storage := NewStorage(st)
	client, _ := st.CreateOAuthClient(models.OAuthClientCreate{Name: "C", Public: true})
	req := buildFositeRequest(client.ID, "u", nil, nil)
	const sig = "sig-2"
	if err := storage.CreateAuthorizeCodeSession(context.Background(), sig, req); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := storage.InvalidateAuthorizeCodeSession(context.Background(), sig); err != nil {
		t.Fatalf("invalidate: %v", err)
	}

	// fosite's contract: invalidated codes return the request
	// payload AND ErrInvalidatedAuthorizeCode so the handler can
	// run grant-family revocation.
	got, err := storage.GetAuthorizeCodeSession(context.Background(), sig, NewSession(""))
	if !errors.Is(err, fosite.ErrInvalidatedAuthorizeCode) {
		t.Fatalf("expected ErrInvalidatedAuthorizeCode, got %v", err)
	}
	if got == nil {
		t.Fatal("invalidated read must still return the request payload")
	}
}

func TestStorage_GetClient_NotFoundMappedCorrectly(t *testing.T) {
	st := testStoreOAuth(t)
	storage := NewStorage(st)
	_, err := storage.GetClient(context.Background(), "nonexistent")
	if !errors.Is(err, fosite.ErrNotFound) {
		t.Errorf("expected fosite.ErrNotFound, got %v", err)
	}
}

func TestStorage_AccessToken_InactiveMappedToFositeError(t *testing.T) {
	st := testStoreOAuth(t)
	storage := NewStorage(st)
	client, _ := st.CreateOAuthClient(models.OAuthClientCreate{Name: "C", Public: true})

	req := buildFositeRequest(client.ID, "u", nil, nil)
	const sig = "access-sig"
	if err := storage.CreateAccessTokenSession(context.Background(), sig, req); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Revoke the family (request_id-scoped); subsequent get should
	// surface fosite.ErrInactiveToken.
	if err := storage.RevokeAccessToken(context.Background(), req.GetID()); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	_, err := storage.GetAccessTokenSession(context.Background(), sig, NewSession(""))
	if !errors.Is(err, fosite.ErrInactiveToken) {
		t.Errorf("expected ErrInactiveToken after RevokeAccessToken, got %v", err)
	}
}

func TestStorage_RotateRefreshToken_RevokesEntireGrant(t *testing.T) {
	// End-to-end test of fosite's rotation expectation: after
	// RotateRefreshToken, both the refresh AND the paired access
	// token are inactive (matches fosite's reference MemoryStore
	// behaviour, locked in by sub-PR A's round-2 fix).
	st := testStoreOAuth(t)
	storage := NewStorage(st)
	client, _ := st.CreateOAuthClient(models.OAuthClientCreate{Name: "C", Public: true})

	// Build a refresh + access pair under the same request_id.
	req := buildFositeRequest(client.ID, "u", nil, nil)
	const accessSig = "rot-access"
	const refreshSig = "rot-refresh"
	if err := storage.CreateAccessTokenSession(context.Background(), accessSig, req); err != nil {
		t.Fatalf("create access: %v", err)
	}
	if err := storage.CreateRefreshTokenSession(context.Background(), refreshSig, accessSig, req); err != nil {
		t.Fatalf("create refresh: %v", err)
	}

	if err := storage.RotateRefreshToken(context.Background(), req.GetID(), refreshSig); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	// Both should now be inactive.
	if _, err := storage.GetRefreshTokenSession(context.Background(), refreshSig, NewSession("")); !errors.Is(err, fosite.ErrInactiveToken) {
		t.Errorf("refresh after rotation: expected ErrInactiveToken, got %v", err)
	}
	if _, err := storage.GetAccessTokenSession(context.Background(), accessSig, NewSession("")); !errors.Is(err, fosite.ErrInactiveToken) {
		t.Errorf("access after rotation: expected ErrInactiveToken (rotation must revoke paired access), got %v", err)
	}
}

func TestStorage_PKCERoundTrip(t *testing.T) {
	st := testStoreOAuth(t)
	storage := NewStorage(st)
	client, _ := st.CreateOAuthClient(models.OAuthClientCreate{Name: "C", Public: true})
	req := buildFositeRequest(client.ID, "u", nil, nil)
	// PKCE request carries the code_challenge in its form.
	req.Form.Set("code_challenge", "abc123")
	req.Form.Set("code_challenge_method", "S256")

	const sig = "pkce-sig"
	if err := storage.CreatePKCERequestSession(context.Background(), sig, req); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := storage.GetPKCERequestSession(context.Background(), sig, NewSession(""))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.GetRequestForm().Get("code_challenge") != "abc123" {
		t.Errorf("code_challenge lost on round-trip: %v", got.GetRequestForm())
	}

	if err := storage.DeletePKCERequestSession(context.Background(), sig); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := storage.GetPKCERequestSession(context.Background(), sig, NewSession("")); !errors.Is(err, fosite.ErrNotFound) {
		t.Errorf("after delete: expected ErrNotFound, got %v", err)
	}
}

func TestStorage_RequesterToOAuthRequest_EncodesSession(t *testing.T) {
	// Pin the JSON shape of session_data so a future Session field
	// addition (e.g. TASK-953's workspace allow-list) doesn't
	// accidentally drop fields on round-trip.
	req := buildFositeRequest("client-id", "user-42", []string{"pad:read", "pad:write"}, nil)
	out, err := requesterToOAuthRequest(req, "sig")
	if err != nil {
		t.Fatalf("requesterToOAuthRequest: %v", err)
	}
	if out.Subject != "user-42" {
		t.Errorf("Subject not denormalized: %q", out.Subject)
	}
	if !strings.Contains(out.SessionData, `"subject":"user-42"`) {
		t.Errorf("SessionData missing subject: %q", out.SessionData)
	}
	if out.Scopes != "pad:read pad:write" {
		t.Errorf("Scopes encoding: got %q, want %q", out.Scopes, "pad:read pad:write")
	}
}

func TestStorage_RequesterToOAuthRequest_RejectsMissingClient(t *testing.T) {
	// Defense-in-depth: fosite would normally guarantee a client,
	// but a malformed adapter call would surface here as a clear
	// store-input error rather than an FK constraint violation.
	req := &fosite.Request{
		ID:          "x",
		RequestedAt: time.Now().UTC(),
		Session:     NewSession("u"),
		Form:        url.Values{},
	}
	_, err := requesterToOAuthRequest(req, "sig")
	if err == nil {
		t.Error("expected error for missing client")
	}
}

// =====================================================================
// Helpers
// =====================================================================

// bytes32 returns 32 deterministic bytes for HMAC tests. Unique
// pattern keeps debug output recognizable if a test inspects the
// secret.
func bytes32() []byte {
	out := make([]byte, 32)
	for i := range out {
		out[i] = byte(i)
	}
	return out
}

// buildFositeRequest produces a fully-populated *fosite.Request so
// adapter tests can drive Create*Session methods without depending
// on fosite's HTTP entry points. session.Subject = subject so the
// adapter's denormalization path fires.
func buildFositeRequest(clientID, subject string, scopes, audience []string) *fosite.Request {
	return &fosite.Request{
		ID:          "req-" + clientID,
		RequestedAt: time.Now().UTC(),
		Client: &fosite.DefaultClient{
			ID:     clientID,
			Public: true,
		},
		RequestedScope:    fosite.Arguments(scopes),
		GrantedScope:      fosite.Arguments(scopes),
		RequestedAudience: fosite.Arguments(audience),
		GrantedAudience:   fosite.Arguments(audience),
		Form:              url.Values{},
		Session:           NewSession(subject),
	}
}

func equalArguments(a, b fosite.Arguments) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
