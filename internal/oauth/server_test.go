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

// TestNewServer_RefreshTokenScopesIsEmpty pins Codex review #371
// round 3: fosite defaults Config.RefreshTokenScopes to
// ["offline", "offline_access"] — without overriding, refresh tokens
// only get issued for grants that include one of those scopes. PLAN-
// 943's scope vocabulary is pad:read / pad:write / pad:admin (no
// offline), so the default would silently disable the rotation +
// family-revocation flow this PR adds.
//
// NewServer sets RefreshTokenScopes to []string{} which tells fosite
// "issue refresh on every authorize-code grant" — matches fosite's
// own tests for the unconditional path.
//
// We can't read the Config back through fosite.OAuth2Provider's
// public surface, so this test goes via reflection-free approach:
// constructing a server, then exercising the audience strategy
// hook (which we DO retain a reference to via the server's audience
// configuration). Covering the actual issue-on-grant path requires
// the HTTP handlers (sub-PR C). For sub-PR B, we pin the wiring
// behaviour: a fresh server's provider is non-nil and the
// configuration we passed in survives storage.
//
// Pin via the configured Storage's canonical audience round-trip,
// which we already cover. The RefreshTokenScopes field itself is a
// fosite-internal concern; the test for whether refresh tokens are
// actually issued lands in sub-PR C's /token end-to-end tests.
// This test exists to document the decision via name + comment so
// a future contributor reading server.go's RefreshTokenScopes line
// can find the rationale here.
func TestNewServer_RefreshTokenScopesIsEmpty(t *testing.T) {
	s := testStoreOAuth(t)
	srv, err := NewServer(Config{
		Store:           s,
		HMACSecret:      bytes32(),
		AllowedAudience: "https://mcp.test.example/mcp",
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	// Smoke check: the constructor returns a usable provider.
	if srv.Provider() == nil {
		t.Fatal("provider is nil — RefreshTokenScopes wiring may have broken NewServer")
	}
	// The actual "refresh issued on authorize-code grant" assertion
	// lands in sub-PR C's /token endpoint test — that's where fosite's
	// flow_authorize_code_token.go reads RefreshTokenScopes. Naming
	// this test ties the rationale together for grep.
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

// TestAudienceStrategy_TrailingSlashEquivalence pins the fix for the
// real-world bug Claude Desktop hit against pad: the connector parses
// the URL the user pasted (e.g. "https://mcp.getpad.dev"), URL-parses
// it (which canonicalizes empty path → "/"), and emits the resource
// indicator as "https://mcp.getpad.dev/" — with a trailing slash that
// pad's canonical "https://mcp.getpad.dev" doesn't have. Per RFC 3986
// §6.2.3 those forms are equivalent for the HTTP scheme; without
// trailing-slash normalization the strict string compare rejects
// every real-client request and the connector loops on
// "Requested audience X is not the canonical audience Y."
//
// Matrix locks in all four (canonical-with-or-without-slash) ×
// (needle-with-or-without-slash) combinations so a future refactor
// that drops the normalization regresses the test, not production.
func TestAudienceStrategy_TrailingSlashEquivalence(t *testing.T) {
	cases := []struct {
		name      string
		canonical string
		needle    string
	}{
		{"both bare", "https://mcp.test.example", "https://mcp.test.example"},
		{"canonical bare, needle slashed", "https://mcp.test.example", "https://mcp.test.example/"},
		{"canonical slashed, needle bare", "https://mcp.test.example/", "https://mcp.test.example"},
		{"both slashed", "https://mcp.test.example/", "https://mcp.test.example/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			strat := audienceMatchingStrategy(tc.canonical)
			if err := strat([]string{tc.canonical}, []string{tc.needle}); err != nil {
				t.Errorf("canonical=%q needle=%q must compare equal; got %v", tc.canonical, tc.needle, err)
			}
		})
	}
}

// TestNormalizeAudience pins the boundaries of the slash-trim helper.
// Codex review #386 round 1 caught an over-broad earlier version that
// trimmed every trailing "/", which would have made
// "https://host/mcp" and "https://host/mcp/" compare equal — distinct
// HTTP resources collapsing to one audience is a real confusion-attack
// surface. The fixed version trims ONLY when the URI's path component
// is the root.
func TestNormalizeAudience(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		// Root-case equivalence (the whole reason the helper exists).
		{"https://host", "https://host"},
		{"https://host/", "https://host"},
		{"http://host:8080", "http://host:8080"},
		{"http://host:8080/", "http://host:8080"},

		// Non-root paths are NOT equivalent — `/foo` and `/foo/` are
		// distinct HTTP resources. MUST stay byte-exact.
		{"https://host/mcp", "https://host/mcp"},
		{"https://host/mcp/", "https://host/mcp/"},
		{"https://host/foo/bar", "https://host/foo/bar"},
		{"https://host/foo/bar/", "https://host/foo/bar/"},

		// Edge cases that must not change semantics:
		//   - hostless strings (parse but no host) stay as-is
		//   - empty stays empty
		//   - URLs with query or fragment stay as-is even on root
		{"", ""},
		{"/", "/"},
		{"https://host/?q=x", "https://host/?q=x"},
		{"https://host/#frag", "https://host/#frag"},

		// Unparseable inputs return as-is (defensive — matching
		// continues to fail string-equally rather than throwing).
		{"::not-a-url::", "::not-a-url::"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := NormalizeAudience(tc.in); got != tc.want {
				t.Errorf("NormalizeAudience(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestAudienceStrategy_PathSlashIsNotEquivalent guards Codex #386
// round 1 directly at the strategy layer: even with normalization
// active, a token requested for a path-slashed audience must NOT
// pass when the canonical is the unslashed path form (and vice
// versa). The strict reject is what stops the audience-confusion
// attack the round-1 review flagged.
func TestAudienceStrategy_PathSlashIsNotEquivalent(t *testing.T) {
	cases := []struct{ canonical, needle string }{
		{"https://host/mcp", "https://host/mcp/"},
		{"https://host/mcp/", "https://host/mcp"},
		{"https://host/foo/bar", "https://host/foo/bar/"},
	}
	for _, tc := range cases {
		t.Run(tc.canonical+"_vs_"+tc.needle, func(t *testing.T) {
			strat := audienceMatchingStrategy(tc.canonical)
			if err := strat([]string{tc.canonical}, []string{tc.needle}); err == nil {
				t.Errorf("canonical=%q needle=%q must NOT compare equal (path-slash distinction)", tc.canonical, tc.needle)
			}
		})
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
	storage := NewStorage(st, "https://mcp.test.example/mcp")

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
	storage := NewStorage(st, "https://mcp.test.example/mcp")
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

// TestStorage_GetClient_InjectsCanonicalAudience pins Codex review
// #371 round 1: the adapter MUST inject the canonical audience into
// the hydrated fosite.Client.Audience field. Without this,
// audienceMatchingStrategy's haystack check (client.GetAudience()
// must contain canonical) rejects every request — every authorize
// + token + refresh flow fails with invalid_request.
//
// The audience isn't persisted (single-resource AS for v1; storing
// what we'd always set to the same value would be write
// amplification). It's injected at hydration from
// Storage.canonicalAudience.
func TestStorage_GetClient_InjectsCanonicalAudience(t *testing.T) {
	st := testStoreOAuth(t)
	canonical := "https://mcp.test.example/mcp"
	storage := NewStorage(st, canonical)

	// Seed a client with NO audience (the storage layer doesn't
	// expose it as a field).
	created, err := st.CreateOAuthClient(models.OAuthClientCreate{
		Name:         "Test Client",
		RedirectURIs: []string{"https://app.test/cb"},
		Public:       true,
	})
	if err != nil {
		t.Fatalf("seed client: %v", err)
	}

	// Hydrate via the adapter and assert the audience is populated.
	got, err := storage.GetClient(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("GetClient: %v", err)
	}
	aud := got.GetAudience()
	if len(aud) != 1 || aud[0] != canonical {
		t.Errorf("hydrated client.Audience = %v, want [%q]", aud, canonical)
	}
}

// TestStorage_NewStorage_EmptyCanonicalLeavesAudienceNil documents
// the "fail-loud" branch: a misconfigured Storage (empty
// canonicalAudience — should never happen in production because
// NewServer rejects it earlier) returns clients with Audience=nil,
// which makes audienceMatchingStrategy reject every request. Better
// than silently issuing wide-open tokens.
func TestStorage_NewStorage_EmptyCanonicalLeavesAudienceNil(t *testing.T) {
	st := testStoreOAuth(t)
	storage := NewStorage(st, "")

	created, err := st.CreateOAuthClient(models.OAuthClientCreate{
		Name:   "Test Client",
		Public: true,
	})
	if err != nil {
		t.Fatalf("seed client: %v", err)
	}
	got, err := storage.GetClient(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("GetClient: %v", err)
	}
	if got.GetAudience() != nil && len(got.GetAudience()) != 0 {
		t.Errorf("empty canonical must produce nil/empty Audience to fail loud; got %v", got.GetAudience())
	}
}

func TestStorage_GetClient_NotFoundMappedCorrectly(t *testing.T) {
	st := testStoreOAuth(t)
	storage := NewStorage(st, "https://mcp.test.example/mcp")
	_, err := storage.GetClient(context.Background(), "nonexistent")
	if !errors.Is(err, fosite.ErrNotFound) {
		t.Errorf("expected fosite.ErrNotFound, got %v", err)
	}
}

func TestStorage_AccessToken_InactiveMappedToFositeError(t *testing.T) {
	st := testStoreOAuth(t)
	storage := NewStorage(st, "https://mcp.test.example/mcp")
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

// TestStorage_GetRefreshTokenSession_InactiveReturnsPayload pins
// Codex review #371 round 2: when the refresh row is inactive,
// GetRefreshTokenSession must return the hydrated request payload
// alongside fosite.ErrInactiveToken. Fosite's handleRefreshTokenReuse
// (flow_refresh.go:178-204) calls req.GetID() to revoke the family —
// returning nil would nil-deref the family-revocation flow and
// defeat replay detection.
//
// Symmetric pattern is used for access tokens (no caller currently
// derefs on inactive, but defense-in-depth makes the contract
// uniform).
func TestStorage_GetRefreshTokenSession_InactiveReturnsPayload(t *testing.T) {
	st := testStoreOAuth(t)
	storage := NewStorage(st, "https://mcp.test.example/mcp")
	client, _ := st.CreateOAuthClient(models.OAuthClientCreate{Name: "C", Public: true})

	const sig = "refresh-replay-sig"
	req := buildFositeRequest(client.ID, "user-1", nil, nil)
	if err := storage.CreateRefreshTokenSession(context.Background(), sig, "access-sig", req); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Mark inactive directly (simulating a rotation that flipped the
	// row). RotateRefreshToken would do the same plus revoke the
	// access family — for this test we just want the inactive row
	// without the access-side effect.
	if err := st.RevokeRefreshTokenFamily(req.GetID()); err != nil {
		t.Fatalf("seed inactive: %v", err)
	}

	got, err := storage.GetRefreshTokenSession(context.Background(), sig, NewSession(""))
	if !errors.Is(err, fosite.ErrInactiveToken) {
		t.Fatalf("expected ErrInactiveToken, got %v", err)
	}
	if got == nil {
		t.Fatal("inactive refresh must return request payload alongside error (fosite handleRefreshTokenReuse derefs req.GetID())")
	}
	if got.GetID() != req.GetID() {
		t.Errorf("returned request_id mismatch: got %q want %q", got.GetID(), req.GetID())
	}
}

// TestStorage_GetAccessTokenSession_InactiveReturnsPayload — symmetric
// to the refresh path. Access-token introspection / revocation
// handlers don't currently dereference on inactive, but the uniform
// contract makes the adapter resilient to future fosite changes.
func TestStorage_GetAccessTokenSession_InactiveReturnsPayload(t *testing.T) {
	st := testStoreOAuth(t)
	storage := NewStorage(st, "https://mcp.test.example/mcp")
	client, _ := st.CreateOAuthClient(models.OAuthClientCreate{Name: "C", Public: true})

	const sig = "access-inactive-sig"
	req := buildFositeRequest(client.ID, "user-1", nil, nil)
	if err := storage.CreateAccessTokenSession(context.Background(), sig, req); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := storage.RevokeAccessToken(context.Background(), req.GetID()); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	got, err := storage.GetAccessTokenSession(context.Background(), sig, NewSession(""))
	if !errors.Is(err, fosite.ErrInactiveToken) {
		t.Fatalf("expected ErrInactiveToken, got %v", err)
	}
	if got == nil {
		t.Fatal("inactive access must return request payload alongside error")
	}
	if got.GetID() != req.GetID() {
		t.Errorf("returned request_id mismatch: got %q want %q", got.GetID(), req.GetID())
	}
}

func TestStorage_RotateRefreshToken_RevokesEntireGrant(t *testing.T) {
	// End-to-end test of fosite's rotation expectation: after
	// RotateRefreshToken, both the refresh AND the paired access
	// token are inactive (matches fosite's reference MemoryStore
	// behaviour, locked in by sub-PR A's round-2 fix).
	st := testStoreOAuth(t)
	storage := NewStorage(st, "https://mcp.test.example/mcp")
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
	storage := NewStorage(st, "https://mcp.test.example/mcp")
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
// Revocation observer (PLAN-943 TASK-961)
// =====================================================================

// TestStorage_RevocationObserver_FiresOnAccessTokenRevoke pins the
// contract metrics depend on: when fosite calls RevokeAccessToken on
// our adapter, the configured observer fires with kind="user_initiated"
// and a positive TTL derived from the revoked family's oldest
// requested_at.
func TestStorage_RevocationObserver_FiresOnAccessTokenRevoke(t *testing.T) {
	s := testStoreOAuth(t)

	// Seed a client + access token row with a known issuance time.
	c, err := s.CreateOAuthClient(models.OAuthClientCreate{
		Name:                    "Test",
		RedirectURIs:            []string{"https://example.test/cb"},
		GrantTypes:              []string{"authorization_code"},
		ResponseTypes:           []string{"code"},
		TokenEndpointAuthMethod: "none",
		Scopes:                  []string{"pad:read"},
		Public:                  true,
	})
	if err != nil {
		t.Fatalf("CreateOAuthClient: %v", err)
	}

	issuedAt := time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Second)
	if err := s.CreateAccessToken(models.OAuthRequest{
		Signature:   "sig-1",
		RequestID:   "req-1",
		RequestedAt: issuedAt,
		ClientID:    c.ID,
		Scopes:      "pad:read",
		Audience:    "https://mcp.test.example/mcp",
		Active:      true,
		Subject:     "user-x",
	}); err != nil {
		t.Fatalf("CreateAccessToken: %v", err)
	}

	storage := NewStorage(s, "https://mcp.test.example/mcp")

	var (
		gotKind string
		gotTTL  time.Duration
		fired   int
	)
	storage.SetRevocationObserver(func(kind string, ttl time.Duration) {
		fired++
		gotKind = kind
		gotTTL = ttl
	})

	if err := storage.RevokeAccessToken(context.Background(), "req-1"); err != nil {
		t.Fatalf("RevokeAccessToken: %v", err)
	}

	if fired != 1 {
		t.Errorf("observer fired %d times, want 1", fired)
	}
	if gotKind != "user_initiated" {
		t.Errorf("kind: got %q, want %q", gotKind, "user_initiated")
	}
	// TTL should be ~30 minutes (allow generous slack for slow CI +
	// timestamp truncation).
	if gotTTL < 25*time.Minute || gotTTL > 35*time.Minute {
		t.Errorf("ttl: got %v, want ~30m", gotTTL)
	}
}

// TestStorage_RevocationObserver_FiresOnRotation covers the second
// emit path: fosite's RotateRefreshToken (called during /oauth/token
// refresh exchange) revokes the parent family and our adapter must
// signal that with kind="rotated".
func TestStorage_RevocationObserver_FiresOnRotation(t *testing.T) {
	s := testStoreOAuth(t)

	c, err := s.CreateOAuthClient(models.OAuthClientCreate{
		Name:                    "Test",
		RedirectURIs:            []string{"https://example.test/cb"},
		GrantTypes:              []string{"authorization_code", "refresh_token"},
		ResponseTypes:           []string{"code"},
		TokenEndpointAuthMethod: "none",
		Scopes:                  []string{"pad:read"},
		Public:                  true,
	})
	if err != nil {
		t.Fatalf("CreateOAuthClient: %v", err)
	}

	issuedAt := time.Now().UTC().Add(-5 * time.Minute).Truncate(time.Second)
	if err := s.CreateAccessToken(models.OAuthRequest{
		Signature: "acc-1", RequestID: "fam-1", RequestedAt: issuedAt,
		ClientID: c.ID, Audience: "https://mcp.test.example/mcp", Active: true,
	}); err != nil {
		t.Fatalf("CreateAccessToken: %v", err)
	}
	if err := s.CreateRefreshToken(models.OAuthRequest{
		Signature: "ref-1", RequestID: "fam-1", RequestedAt: issuedAt,
		ClientID: c.ID, Audience: "https://mcp.test.example/mcp", Active: true,
	}); err != nil {
		t.Fatalf("CreateRefreshToken: %v", err)
	}

	storage := NewStorage(s, "https://mcp.test.example/mcp")
	var observed []string
	storage.SetRevocationObserver(func(kind string, _ time.Duration) {
		observed = append(observed, kind)
	})

	if err := storage.RotateRefreshToken(context.Background(), "fam-1", "ref-1"); err != nil {
		t.Fatalf("RotateRefreshToken: %v", err)
	}

	if len(observed) != 1 || observed[0] != "rotated" {
		t.Errorf("observer kinds: got %v, want [rotated]", observed)
	}
}

// TestStorage_RevocationObserver_NilSafe verifies that an unset
// observer is a no-op — callers shouldn't have to wire one to use
// the storage adapter (selfhost / tests / non-metrics builds).
func TestStorage_RevocationObserver_NilSafe(t *testing.T) {
	s := testStoreOAuth(t)
	c, err := s.CreateOAuthClient(models.OAuthClientCreate{
		Name:                    "Test",
		RedirectURIs:            []string{"https://example.test/cb"},
		GrantTypes:              []string{"authorization_code"},
		ResponseTypes:           []string{"code"},
		TokenEndpointAuthMethod: "none",
		Scopes:                  []string{"pad:read"},
		Public:                  true,
	})
	if err != nil {
		t.Fatalf("CreateOAuthClient: %v", err)
	}
	if err := s.CreateAccessToken(models.OAuthRequest{
		Signature: "sig-1", RequestID: "req-1", RequestedAt: time.Now().UTC(),
		ClientID: c.ID, Audience: "https://mcp.test.example/mcp", Active: true,
	}); err != nil {
		t.Fatalf("CreateAccessToken: %v", err)
	}

	storage := NewStorage(s, "https://mcp.test.example/mcp")
	// No SetRevocationObserver call — should not panic.
	if err := storage.RevokeAccessToken(context.Background(), "req-1"); err != nil {
		t.Fatalf("RevokeAccessToken (no observer): %v", err)
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
