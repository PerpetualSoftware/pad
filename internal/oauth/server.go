package oauth

import (
	"errors"
	"fmt"
	"time"

	"github.com/ory/fosite"
	"github.com/ory/fosite/compose"
	"github.com/ory/fosite/handler/oauth2"

	"github.com/PerpetualSoftware/pad/internal/store"
)

// Config configures NewServer. Every field is required; pass via
// the wiring in cmd/pad/main.go (sub-PR C extends that wiring with
// the HTTP handlers).
type Config struct {
	// Store is the persistence layer (sub-PR A's *store.Store).
	// NewServer wraps it in *Storage to satisfy fosite's storage
	// interfaces.
	Store *store.Store

	// HMACSecret is the 32-byte secret fosite uses to sign opaque
	// access + refresh + auth-code values. In production this is
	// derived from cfg.EncryptionKey (already required in cloud
	// mode, see cmd/pad/main.go's encryption-key bootstrap).
	//
	// Rotation: fosite supports rotating secrets via
	// Config.RotatedGlobalSecrets. We don't expose that yet —
	// rotation arrives with the operator runbook for TASK-953 /
	// TASK-954.
	HMACSecret []byte

	// AllowedAudience is the canonical resource indicator that
	// every issued token is bound to (RFC 8707). In production:
	// cfg.MCPPublicURL + "/mcp" (e.g. "https://mcp.getpad.dev/mcp").
	// /authorize and /token reject requests with mismatched
	// `resource` per the audienceMatchingStrategy in audience.go.
	AllowedAudience string

	// Optional lifespan overrides — sensible defaults below if zero.
	// Operators who need shorter access tokens (e.g. compliance
	// regimes) override via env vars in sub-PR C's wiring.
	AccessTokenLifespan   time.Duration
	RefreshTokenLifespan  time.Duration
	AuthorizeCodeLifespan time.Duration
}

// Server is pad's OAuth 2.1 authorization server. It composes
// fosite handlers over the storage adapter and exposes the
// fosite.OAuth2Provider that sub-PR C's HTTP handlers consume.
type Server struct {
	provider fosite.OAuth2Provider
	cfg      Config
	storage  *Storage
}

// NewServer constructs an OAuth 2.1 authorization server backed by
// fosite v0.49.0.
//
// Compliance posture (PLAN-943 TASK-951):
//
//   - PKCE required (S256 only). Config.EnforcePKCE = true,
//     EnablePKCEPlainChallengeMethod stays false (the default), so
//     `plain` is rejected. fosite's PKCE handler enforces this on
//     /authorize + /token.
//   - Refresh tokens rotate single-use. compose.OAuth2RefreshTokenGrantFactory
//     wires fosite's standard rotation flow which calls
//     Storage.RotateRefreshToken (revokes the entire grant family
//     per the round-2 fix in sub-PR A) before issuing the new pair.
//   - Audience-bound tokens (RFC 8707). Custom AudienceMatchingStrategy
//     from audience.go rejects any audience that isn't the canonical
//     MCP resource URL.
//   - Opaque HMAC tokens (not JWT). compose.NewOAuth2HMACStrategy
//     produces opaque values that can only be validated by us +
//     introspected per RFC 7662 — easier to revoke than JWT.
//   - HTTPS-only enforcement at the HTTP boundary (sub-PR C's job).
//
// Factories included:
//   - OAuth2AuthorizeExplicitFactory — auth-code grant
//   - OAuth2RefreshTokenGrantFactory — refresh-token rotation
//   - OAuth2TokenIntrospectionFactory — RFC 7662 introspection
//   - OAuth2TokenRevocationFactory — RFC 7009 revocation
//   - OAuth2PKCEFactory — PKCE (S256 enforced)
//
// Excluded by design:
//   - OAuth2ClientCredentialsGrantFactory (server-to-server, not
//     applicable for the public-clients-only model)
//   - OAuth2AuthorizeImplicitFactory (deprecated in OAuth 2.1)
//   - OAuth2ResourceOwnerPasswordCredentialsFactory (deprecated in
//     OAuth 2.1)
//   - OpenID factories (we're not an OIDC IdP — yet)
//   - PushedAuthorizeHandlerFactory (PAR — not needed for v1)
func NewServer(cfg Config) (*Server, error) {
	if cfg.Store == nil {
		return nil, errors.New("oauth: NewServer: Store is required")
	}
	if len(cfg.HMACSecret) < 32 {
		return nil, errors.New("oauth: NewServer: HMACSecret must be at least 32 bytes (256 bits)")
	}
	if cfg.AllowedAudience == "" {
		return nil, errors.New("oauth: NewServer: AllowedAudience is required (RFC 8707)")
	}

	// Sensible lifespans. Tunable via Config because TASK-959 may
	// want shorter access tokens once observability lands and we can
	// see realistic refresh frequency. Defaults are conservative —
	// short-lived enough to bound replay damage, long enough to
	// avoid excess refresh churn.
	access := cfg.AccessTokenLifespan
	if access == 0 {
		access = time.Hour
	}
	refresh := cfg.RefreshTokenLifespan
	if refresh == 0 {
		refresh = 30 * 24 * time.Hour
	}
	authCode := cfg.AuthorizeCodeLifespan
	if authCode == 0 {
		authCode = 15 * time.Minute
	}

	fcfg := &fosite.Config{
		// Spec compliance.
		EnforcePKCE:                    true,
		EnablePKCEPlainChallengeMethod: false, // S256 only; plain rejected
		// fosite is content with the default ScopeStrategy
		// (HierarchicScopeStrategy). PLAN-943 ships simple
		// pad:read / pad:write / pad:admin scopes — the hierarchic
		// strategy treats them as opaque, which is what we want.
		// TASK-953's allow-list scopes plug in via an
		// AccessTokenIssuer / RequestValidator hook; sub-PR E adds
		// the required custom strategy when we layer those in.

		// Token shapes.
		AccessTokenLifespan:   access,
		RefreshTokenLifespan:  refresh,
		AuthorizeCodeLifespan: authCode,

		// HMAC signing material. fosite uses the secret to derive
		// the signature half of opaque tokens (the value the user
		// sees is the public half + a "." + signature).
		GlobalSecret: cfg.HMACSecret,

		// Custom audience strategy (RFC 8707). Rejects any audience
		// that isn't the canonical MCP resource URL. See audience.go.
		AudienceMatchingStrategy: audienceMatchingStrategy(cfg.AllowedAudience),

		// Strategies fosite needs to introspect:
		// (no extra config — defaults handle these)
	}

	// Storage carries the canonical audience so hydrated clients
	// pass audienceMatchingStrategy's haystack-side check. Single-
	// resource AS for v1 — every client implicitly allowed for the
	// configured audience. See storage.go modelClientToFosite.
	storage := NewStorage(cfg.Store, cfg.AllowedAudience)
	strategy := compose.NewOAuth2HMACStrategy(fcfg)

	provider := compose.Compose(
		fcfg,
		storage,
		strategy,
		// Auth-code grant (the only authorize-side flow we run).
		compose.OAuth2AuthorizeExplicitFactory,
		// Refresh-token rotation. The factory's flow_refresh.go
		// calls Storage.RotateRefreshToken before issuing the new
		// pair, which under our adapter revokes the entire grant
		// family — matching fosite's reference MemoryStore behaviour.
		compose.OAuth2RefreshTokenGrantFactory,
		// RFC 7662 introspection (sub-PR D wires the endpoint).
		compose.OAuth2TokenIntrospectionFactory,
		// RFC 7009 revocation (sub-PR D wires the endpoint).
		compose.OAuth2TokenRevocationFactory,
		// PKCE (S256 enforced).
		compose.OAuth2PKCEFactory,
	)

	// Sanity check that compose actually built a usable provider.
	// Defensive — fosite's compose returns a non-nil provider in all
	// public paths, but the type-assertions inside compose.Compose
	// silently drop unrecognized factory return types and we don't
	// want to mask that.
	if provider == nil {
		return nil, fmt.Errorf("oauth: compose.Compose returned nil provider")
	}

	return &Server{
		provider: provider,
		cfg:      cfg,
		storage:  storage,
	}, nil
}

// Provider returns the fosite.OAuth2Provider that sub-PR C's HTTP
// handlers will call (NewAuthorizeRequest, NewAccessRequest,
// WriteAuthorizeResponse, etc.). Exposed as a method rather than a
// public field so the field can stay unexported and a future change
// (e.g. adding a tracing wrapper) only mutates Server.provider once.
func (s *Server) Provider() fosite.OAuth2Provider {
	return s.provider
}

// Storage returns the storage adapter. Exposed for sub-PR C's
// /oauth/register handler (which calls Storage.store directly to
// CreateOAuthClient) and for tests that want to seed rows under
// pad-internal types rather than constructing fosite.Requester.
func (s *Server) Storage() *Storage {
	return s.storage
}

// AllowedAudience returns the canonical resource URL the server is
// configured to accept. Sub-PR C's /authorize handler reads this to
// reject mismatched `resource=` query params at request entry,
// before fosite's downstream validation runs.
func (s *Server) AllowedAudience() string {
	return s.cfg.AllowedAudience
}

// Compile-time guard: NewStorage produces a value that satisfies the
// minimal fosite.Storage interface (which is just ClientManager).
// Ensures storage.go's interface coverage doesn't drift.
var _ fosite.Storage = (*Storage)(nil)

// Compile-time guard: also assert the per-handler interfaces
// individually so a future rename / removal in fosite surfaces here
// at build time rather than at runtime.
var (
	_ oauth2.AuthorizeCodeStorage   = (*Storage)(nil)
	_ oauth2.AccessTokenStorage     = (*Storage)(nil)
	_ oauth2.RefreshTokenStorage    = (*Storage)(nil)
	_ oauth2.TokenRevocationStorage = (*Storage)(nil)
)
