package store

import (
	"errors"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Tests for the OAuth 2.1 storage layer (PLAN-943 TASK-951 sub-PR A).
// The store layer is fosite-agnostic by design — sub-PR B introduces
// the fosite import + adapter wrappers. These tests exercise the
// storage primitives directly so the schema + CRUD shape is verified
// in isolation, before fosite types add semantic checks on top.
//
// Both backends share the same test bodies via testStore(t), which
// switches to Postgres when PAD_TEST_POSTGRES_URL is set in CI.
// The test client used everywhere (newTestClient) seeds an
// oauth_clients row first because every other table FKs into it.

func newTestClient(t *testing.T, s *Store) *models.OAuthClient {
	t.Helper()
	c, err := s.CreateOAuthClient(models.OAuthClientCreate{
		Name:                    "Test Client",
		RedirectURIs:            []string{"https://example.test/callback"},
		GrantTypes:              []string{"authorization_code", "refresh_token"},
		ResponseTypes:           []string{"code"},
		TokenEndpointAuthMethod: "none",
		Scopes:                  []string{"pad:read", "pad:write"},
		Public:                  true,
	})
	if err != nil {
		t.Fatalf("CreateOAuthClient: %v", err)
	}
	return c
}

// newTestRequest builds an OAuthRequest with sensible defaults; tests
// override fields they care about. signature uniqueness is the
// caller's responsibility — pass distinct strings per row.
func newTestRequest(clientID, signature, requestID string) models.OAuthRequest {
	return models.OAuthRequest{
		Signature:       signature,
		RequestID:       requestID,
		RequestedAt:     time.Now().UTC(),
		ClientID:        clientID,
		Scopes:          "pad:read pad:write",
		GrantedScopes:   "pad:read pad:write",
		RequestForm:     "client_id=" + clientID + "&code_challenge=abc&code_challenge_method=S256",
		SessionData:     `{"subject":"user-123"}`,
		Audience:        "https://mcp.test.example/mcp",
		GrantedAudience: "https://mcp.test.example/mcp",
		Active:          true,
		Subject:         "user-123",
	}
}

// ------------------------------------------------------------
// Clients
// ------------------------------------------------------------

func TestOAuth_ClientCRUD(t *testing.T) {
	s := testStore(t)

	created, err := s.CreateOAuthClient(models.OAuthClientCreate{
		Name:                    "MyApp",
		RedirectURIs:            []string{"https://app.test/cb", "http://localhost:3000/cb"},
		GrantTypes:              []string{"authorization_code", "refresh_token"},
		ResponseTypes:           []string{"code"},
		TokenEndpointAuthMethod: "none",
		Scopes:                  []string{"pad:read", "pad:write", "pad:admin"},
		Public:                  true,
		LogoURL:                 "https://app.test/logo.png",
	})
	if err != nil {
		t.Fatalf("CreateOAuthClient: %v", err)
	}
	if created.ID == "" {
		t.Error("expected non-empty client_id")
	}
	if created.CreatedAt.IsZero() {
		t.Error("expected non-zero CreatedAt")
	}

	got, err := s.GetOAuthClient(created.ID)
	if err != nil {
		t.Fatalf("GetOAuthClient: %v", err)
	}
	if got.Name != "MyApp" {
		t.Errorf("Name: got %q, want %q", got.Name, "MyApp")
	}
	if len(got.RedirectURIs) != 2 || got.RedirectURIs[1] != "http://localhost:3000/cb" {
		t.Errorf("RedirectURIs round-trip lost order or values: %v", got.RedirectURIs)
	}
	if !got.Public {
		t.Error("Public flag did not round-trip true")
	}
	if got.LogoURL != "https://app.test/logo.png" {
		t.Errorf("LogoURL: got %q", got.LogoURL)
	}
}

func TestOAuth_GetOAuthClient_NotFound(t *testing.T) {
	s := testStore(t)
	_, err := s.GetOAuthClient("does-not-exist")
	if !errors.Is(err, ErrOAuthNotFound) {
		t.Errorf("expected ErrOAuthNotFound, got %v", err)
	}
}

func TestOAuth_DeleteOAuthClient_Idempotent(t *testing.T) {
	s := testStore(t)
	c := newTestClient(t, s)
	if err := s.DeleteOAuthClient(c.ID); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	// Second delete must not error — idempotency is part of the
	// contract so /oauth/register failure-recovery flows can retry.
	if err := s.DeleteOAuthClient(c.ID); err != nil {
		t.Errorf("second delete: %v", err)
	}
	// Get must report not-found after delete.
	if _, err := s.GetOAuthClient(c.ID); !errors.Is(err, ErrOAuthNotFound) {
		t.Errorf("expected ErrOAuthNotFound after delete, got %v", err)
	}
}

func TestOAuth_CreateClient_EmptySlicesNormalize(t *testing.T) {
	// nil / empty slices must round-trip as empty (non-nil) so
	// callers can range without nil-checking. Pinning this prevents
	// a regression where Postgres's JSONB column returns null for
	// missing fields — the store would have to map null→[].
	s := testStore(t)
	c, err := s.CreateOAuthClient(models.OAuthClientCreate{
		Name: "Minimal",
	})
	if err != nil {
		t.Fatalf("CreateOAuthClient minimal: %v", err)
	}
	got, err := s.GetOAuthClient(c.ID)
	if err != nil {
		t.Fatalf("GetOAuthClient: %v", err)
	}
	for name, slice := range map[string][]string{
		"RedirectURIs":  got.RedirectURIs,
		"GrantTypes":    got.GrantTypes,
		"ResponseTypes": got.ResponseTypes,
		"Scopes":        got.Scopes,
	} {
		if slice == nil {
			t.Errorf("%s: expected empty slice, got nil", name)
		}
		if len(slice) != 0 {
			t.Errorf("%s: expected empty, got %v", name, slice)
		}
	}
	// Default token endpoint auth method.
	if got.TokenEndpointAuthMethod != "none" {
		t.Errorf("TokenEndpointAuthMethod default: got %q, want %q", got.TokenEndpointAuthMethod, "none")
	}
}

// ------------------------------------------------------------
// Authorization codes
// ------------------------------------------------------------

func TestOAuth_AuthorizationCode_CRUDAndInvalidate(t *testing.T) {
	s := testStore(t)
	c := newTestClient(t, s)

	req := newTestRequest(c.ID, "code-sig-1", "request-1")
	if err := s.CreateAuthorizationCode(req); err != nil {
		t.Fatalf("CreateAuthorizationCode: %v", err)
	}

	got, err := s.GetAuthorizationCode("code-sig-1")
	if err != nil {
		t.Fatalf("GetAuthorizationCode active: %v", err)
	}
	if got.ClientID != c.ID || got.RequestID != "request-1" {
		t.Errorf("round-trip mismatch: got=%+v", got)
	}
	if got.SessionData != `{"subject":"user-123"}` {
		t.Errorf("session_data round-trip lost: %q", got.SessionData)
	}

	if err := s.InvalidateAuthorizationCode("code-sig-1"); err != nil {
		t.Fatalf("InvalidateAuthorizationCode: %v", err)
	}

	got2, err := s.GetAuthorizationCode("code-sig-1")
	if !errors.Is(err, ErrOAuthInvalidatedCode) {
		t.Fatalf("expected ErrOAuthInvalidatedCode after invalidate, got %v", err)
	}
	// Per fosite contract, the request payload is still returned
	// alongside the error so the caller can run family revocation.
	if got2 == nil {
		t.Fatal("expected request payload returned alongside ErrOAuthInvalidatedCode, got nil")
	}
	if got2.RequestID != "request-1" {
		t.Errorf("invalidate-then-get must still surface request_id (caller revokes family by it); got %q", got2.RequestID)
	}
}

func TestOAuth_GetAuthorizationCode_NotFound(t *testing.T) {
	s := testStore(t)
	_, err := s.GetAuthorizationCode("nope")
	if !errors.Is(err, ErrOAuthNotFound) {
		t.Errorf("expected ErrOAuthNotFound, got %v", err)
	}
}

func TestOAuth_InvalidateAuthorizationCode_Idempotent(t *testing.T) {
	// Invalidating an absent / already-invalid row must not error.
	// fosite's contract is "make it invalid"; our store doesn't
	// distinguish "wasn't there" from "was already invalid".
	s := testStore(t)
	if err := s.InvalidateAuthorizationCode("never-existed"); err != nil {
		t.Errorf("expected nil on absent code, got %v", err)
	}
}

// ------------------------------------------------------------
// Access tokens
// ------------------------------------------------------------

func TestOAuth_AccessToken_CRUDAndDelete(t *testing.T) {
	s := testStore(t)
	c := newTestClient(t, s)

	req := newTestRequest(c.ID, "access-sig-1", "request-1")
	if err := s.CreateAccessToken(req); err != nil {
		t.Fatalf("CreateAccessToken: %v", err)
	}

	got, err := s.GetAccessToken("access-sig-1")
	if err != nil {
		t.Fatalf("GetAccessToken: %v", err)
	}
	if got.Subject != "user-123" {
		t.Errorf("Subject not denormalized correctly: %q", got.Subject)
	}

	if err := s.DeleteAccessToken("access-sig-1"); err != nil {
		t.Fatalf("DeleteAccessToken: %v", err)
	}

	if _, err := s.GetAccessToken("access-sig-1"); !errors.Is(err, ErrOAuthNotFound) {
		t.Errorf("expected ErrOAuthNotFound after delete, got %v", err)
	}
}

// ------------------------------------------------------------
// Refresh tokens — rotation + family revocation (the security-critical part)
// ------------------------------------------------------------

func TestOAuth_RefreshToken_CRUD(t *testing.T) {
	s := testStore(t)
	c := newTestClient(t, s)

	req := newTestRequest(c.ID, "refresh-sig-1", "request-1")
	req.AccessTokenSignature = "access-sig-1"
	if err := s.CreateRefreshToken(req); err != nil {
		t.Fatalf("CreateRefreshToken: %v", err)
	}

	got, err := s.GetRefreshToken("refresh-sig-1")
	if err != nil {
		t.Fatalf("GetRefreshToken: %v", err)
	}
	if got.AccessTokenSignature != "access-sig-1" {
		t.Errorf("AccessTokenSignature did not round-trip: got %q", got.AccessTokenSignature)
	}
	if got.RequestID != "request-1" {
		t.Errorf("RequestID round-trip: got %q", got.RequestID)
	}
}

func TestOAuth_RotateRefreshToken_RevokesEntireGrant(t *testing.T) {
	// Per fosite's reference MemoryStore.RotateRefreshToken
	// (storage/memory.go:497-504), rotation revokes BOTH the refresh
	// family AND the access family for the grant's request_id, then
	// fosite immediately issues a fresh pair via CreateAccessTokenSession
	// + CreateRefreshTokenSession (which inherit the same request_id
	// per flow_refresh.go:86 and get inserted active=TRUE per our
	// insertOAuthRequestRow contract).
	//
	// Codex review #370 round 2 caught the previous implementation
	// that flipped only the named refresh row, leaving the previously-
	// issued access token active until TTL expired. The test below
	// pins the corrected behavior: after rotation, the OLD refresh +
	// OLD access in the grant are inactive, and unrelated grants are
	// untouched.
	s := testStore(t)
	c := newTestClient(t, s)
	chain := "request-rotate-1"
	other := "request-other-1"

	// Seed the grant: one refresh + one access token sharing
	// request_id = chain. (fosite never has multiple active
	// refreshes in the same chain at once — the chain is sequential —
	// so a single pair is the realistic input shape.)
	rreq := newTestRequest(c.ID, "refresh-1", chain)
	rreq.AccessTokenSignature = "access-1"
	if err := s.CreateRefreshToken(rreq); err != nil {
		t.Fatalf("create refresh: %v", err)
	}
	if err := s.CreateAccessToken(newTestRequest(c.ID, "access-1", chain)); err != nil {
		t.Fatalf("create access: %v", err)
	}

	// Seed an unrelated grant to verify rotation is scoped by request_id.
	if err := s.CreateRefreshToken(newTestRequest(c.ID, "other-refresh", other)); err != nil {
		t.Fatalf("create other refresh: %v", err)
	}
	if err := s.CreateAccessToken(newTestRequest(c.ID, "other-access", other)); err != nil {
		t.Fatalf("create other access: %v", err)
	}

	if err := s.RotateRefreshToken(chain, "refresh-1"); err != nil {
		t.Fatalf("RotateRefreshToken: %v", err)
	}

	// Both old refresh AND old access in the rotated grant must be inactive.
	if got, err := s.GetRefreshToken("refresh-1"); !errors.Is(err, ErrOAuthInactiveToken) {
		t.Errorf("refresh-1 must be inactive after rotation, got err=%v got=%+v", err, got)
	}
	if got, err := s.GetAccessToken("access-1"); !errors.Is(err, ErrOAuthInactiveToken) {
		t.Errorf("access-1 must be inactive after rotation (matches fosite's reference); got err=%v got=%+v", err, got)
	}

	// Unrelated grant must be untouched — rotation is request_id-scoped.
	if got, err := s.GetRefreshToken("other-refresh"); err != nil || !got.Active {
		t.Errorf("other-chain refresh touched by rotation: err=%v got=%+v", err, got)
	}
	if got, err := s.GetAccessToken("other-access"); err != nil || !got.Active {
		t.Errorf("other-chain access touched by rotation: err=%v got=%+v", err, got)
	}
}

func TestOAuth_RevokeRefreshTokenFamily_RevokesEntireChain(t *testing.T) {
	// The OAuth 2.1 BCP §4.14 "revoke the whole family on a replayed
	// refresh" rule. fosite triggers this when GetRefreshToken on a
	// previously-rotated (inactive) row signals replay.
	s := testStore(t)
	c := newTestClient(t, s)

	chain := "request-family-1"
	other := "request-other-1"

	for _, sig := range []string{"r1", "r2", "r3"} {
		if err := s.CreateRefreshToken(newTestRequest(c.ID, sig, chain)); err != nil {
			t.Fatalf("create %s: %v", sig, err)
		}
	}
	// Other-chain row must NOT be touched by the revocation.
	if err := s.CreateRefreshToken(newTestRequest(c.ID, "other-r1", other)); err != nil {
		t.Fatalf("create other-r1: %v", err)
	}

	if err := s.RevokeRefreshTokenFamily(chain); err != nil {
		t.Fatalf("RevokeRefreshTokenFamily: %v", err)
	}

	for _, sig := range []string{"r1", "r2", "r3"} {
		_, err := s.GetRefreshToken(sig)
		if !errors.Is(err, ErrOAuthInactiveToken) {
			t.Errorf("%s: expected ErrOAuthInactiveToken after family revoke, got %v", sig, err)
		}
	}

	otherGot, err := s.GetRefreshToken("other-r1")
	if err != nil {
		t.Fatalf("other-chain r1 should still be readable, got %v", err)
	}
	if !otherGot.Active {
		t.Error("RevokeRefreshTokenFamily(chain) must NOT touch rows in a different request_id chain")
	}
}

func TestOAuth_RevokeAccessTokenFamily_RevokesEntireChain(t *testing.T) {
	// Symmetric to RevokeRefreshTokenFamily — fosite revokes both
	// access and refresh families when the user clicks "log out
	// everywhere" or POSTs /oauth/revoke (sub-PR D).
	s := testStore(t)
	c := newTestClient(t, s)

	chain := "request-access-family-1"
	for _, sig := range []string{"a1", "a2"} {
		if err := s.CreateAccessToken(newTestRequest(c.ID, sig, chain)); err != nil {
			t.Fatalf("create %s: %v", sig, err)
		}
	}

	if err := s.RevokeAccessTokenFamily(chain); err != nil {
		t.Fatalf("RevokeAccessTokenFamily: %v", err)
	}

	for _, sig := range []string{"a1", "a2"} {
		_, err := s.GetAccessToken(sig)
		if !errors.Is(err, ErrOAuthInactiveToken) {
			t.Errorf("%s: expected ErrOAuthInactiveToken after family revoke, got %v", sig, err)
		}
	}
}

// ------------------------------------------------------------
// PKCE
// ------------------------------------------------------------

func TestOAuth_PKCE_CRUD(t *testing.T) {
	s := testStore(t)
	c := newTestClient(t, s)

	req := newTestRequest(c.ID, "pkce-sig-1", "request-1")
	if err := s.CreatePKCERequest(req); err != nil {
		t.Fatalf("CreatePKCERequest: %v", err)
	}

	got, err := s.GetPKCERequest("pkce-sig-1")
	if err != nil {
		t.Fatalf("GetPKCERequest: %v", err)
	}
	// PKCE row carries the same request_form fosite stored at /authorize
	// time; sub-PR B's adapter parses code_challenge out of it.
	if got.RequestForm == "" {
		t.Error("RequestForm must round-trip (carries code_challenge for verification)")
	}

	if err := s.DeletePKCERequest("pkce-sig-1"); err != nil {
		t.Fatalf("DeletePKCERequest: %v", err)
	}

	if _, err := s.GetPKCERequest("pkce-sig-1"); !errors.Is(err, ErrOAuthNotFound) {
		t.Errorf("expected ErrOAuthNotFound after delete, got %v", err)
	}
}

// ------------------------------------------------------------
// Validation
// ------------------------------------------------------------

// TestOAuth_Insert_AlwaysActive pins the active-on-insert contract
// the round-1 Codex finding flushed out. Before the fix,
// insertOAuthRequestRow used `if req.Active != defaultActive` to
// detect "explicit override" — but a zero-value bool is
// indistinguishable from "the caller forgot to set it," so any
// adapter that built an OAuthRequest without explicitly setting
// Active=true would silently store an immediately-revoked token.
//
// The fix hardcodes active=TRUE on insert; this test enforces that
// invariant by passing zero-value Active and asserting the row is
// readable as active. Without the fix, GetAccessToken would return
// ErrOAuthInactiveToken here.
func TestOAuth_Insert_AlwaysActive(t *testing.T) {
	s := testStore(t)
	c := newTestClient(t, s)

	// Deliberately omit Active — caller used the zero value.
	req := models.OAuthRequest{
		Signature:   "active-default-1",
		RequestID:   "req-1",
		ClientID:    c.ID,
		Subject:     "user-x",
		RequestedAt: time.Now().UTC(),
		// Active: not set; zero value is false.
	}
	if err := s.CreateAccessToken(req); err != nil {
		t.Fatalf("CreateAccessToken: %v", err)
	}

	got, err := s.GetAccessToken("active-default-1")
	if err != nil {
		t.Fatalf("GetAccessToken (must succeed because insert hardcodes active=true): %v", err)
	}
	if !got.Active {
		t.Error("row stored with active=false despite hardcoded active=true on insert; the round-1 fix regressed")
	}

	// Same for refresh tokens.
	rreq := models.OAuthRequest{
		Signature:   "active-default-2",
		RequestID:   "req-2",
		ClientID:    c.ID,
		Subject:     "user-x",
		RequestedAt: time.Now().UTC(),
	}
	if err := s.CreateRefreshToken(rreq); err != nil {
		t.Fatalf("CreateRefreshToken: %v", err)
	}
	rgot, err := s.GetRefreshToken("active-default-2")
	if err != nil {
		t.Fatalf("GetRefreshToken: %v", err)
	}
	if !rgot.Active {
		t.Error("refresh row stored inactive despite zero-value Active in input")
	}

	// Same for auth codes.
	creq := models.OAuthRequest{
		Signature:   "active-default-3",
		RequestID:   "req-3",
		ClientID:    c.ID,
		RequestedAt: time.Now().UTC(),
	}
	if err := s.CreateAuthorizationCode(creq); err != nil {
		t.Fatalf("CreateAuthorizationCode: %v", err)
	}
	cgot, err := s.GetAuthorizationCode("active-default-3")
	if err != nil {
		t.Fatalf("GetAuthorizationCode (must succeed; insert is active=true): %v", err)
	}
	if !cgot.Active {
		t.Error("auth code stored inactive despite zero-value Active in input")
	}
}

func TestOAuth_Insert_RejectsEmptyRequiredFields(t *testing.T) {
	// The store-level guards exist as defense in depth — fosite's
	// adapter in sub-PR B will populate these fields, but the SQL
	// schema's NOT NULL constraints would otherwise produce
	// confusing "constraint failed" errors. The store rejects
	// upstream with a clear message.
	s := testStore(t)
	c := newTestClient(t, s)

	cases := map[string]models.OAuthRequest{
		"missing signature": {
			RequestID: "r1", ClientID: c.ID,
		},
		"missing request_id": {
			Signature: "sig", ClientID: c.ID,
		},
		"missing client_id": {
			Signature: "sig", RequestID: "r1",
		},
	}
	for name, req := range cases {
		if err := s.CreateAccessToken(req); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}
