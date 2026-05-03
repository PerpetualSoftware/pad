package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/ory/fosite"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// Storage is the adapter that bridges fosite's storage interfaces to
// pad's persistence layer (internal/store/oauth.go from sub-PR A).
//
// What it satisfies:
//
//   - fosite.ClientManager (GetClient, ClientAssertionJWTValid,
//     SetClientAssertionJWT)
//   - github.com/ory/fosite/handler/oauth2.AuthorizeCodeStorage
//   - github.com/ory/fosite/handler/oauth2.AccessTokenStorage
//   - github.com/ory/fosite/handler/oauth2.RefreshTokenStorage
//   - github.com/ory/fosite/handler/oauth2.TokenRevocationStorage
//   - github.com/ory/fosite/handler/pkce.PKCERequestStorage
//
// fosite's compose.Compose stuffs `storage interface{}` and type-asserts
// to fosite.Storage (which is just ClientManager) at minimum, then
// each factory type-asserts to its own narrower storage interface as
// it wires handlers. So every method declared here must be on the
// concrete *Storage receiver — no embedding shortcuts — to satisfy
// the per-factory interface set.
//
// Translation contract:
//
//   - On insert (Create*Session): convert fosite.Requester →
//     models.OAuthRequest, attach the supplied signature, and call
//     the matching store method.
//   - On read (Get*Session): fetch the stored row, hydrate session_data
//     into the supplied fosite.Session pointer, build a fosite.Request
//     using the client looked up via GetClient, and return it.
//   - Errors: map sentinel errors from internal/store/oauth.go to
//     fosite.ErrNotFound / fosite.ErrInvalidatedAuthorizeCode /
//     fosite.ErrInactiveToken so fosite's handler chain branches
//     correctly (esp. ErrInvalidatedAuthorizeCode → triggers grant-
//     family revocation in handler/oauth2/flow_authorize_code_token.go).
type Storage struct {
	store             *store.Store
	canonicalAudience string

	// onTokenRevoked is an optional observer that fires AFTER each
	// successful access-token family revocation. Wired by cmd/pad
	// from internal/metrics so the OAuth surface stays metrics-naive
	// (no Prometheus import in this package).
	//
	// kind is one of:
	//   - "user_initiated" — caller hit /oauth/revoke
	//   - "rotated"        — refresh-rotation revoked the parent family
	//   - "replayed"       — replay-detection (refresh used twice)
	//
	// ttl is wall-clock age of the oldest token in the revoked family.
	// Zero when the lookup couldn't determine an issuance time (no
	// rows, parse failure) — observers should treat zero as "no
	// observation to record."
	//
	// Best-effort: failures from the lookup or the observer must not
	// block revocation. The store's revoke-then-observe ordering means
	// the on-disk state is correct even if we crash mid-observation.
	onTokenRevoked func(kind string, ttl time.Duration)
}

// SetRevocationObserver wires a callback that fires after each
// access-token family revocation. Optional — leaving it unset is
// equivalent to wiring a no-op. Replacing a previously-set observer
// is allowed; cmd/pad calls this once at startup. Concurrent calls
// to SetRevocationObserver during traffic are not synchronized —
// callers responsible for one-shot wiring before serving requests.
func (s *Storage) SetRevocationObserver(fn func(kind string, ttl time.Duration)) {
	s.onTokenRevoked = fn
}

// observeRevocation looks up the oldest active-token issuance time
// for the family and fires the configured observer. Always called
// AFTER the family has been flagged inactive in storage so a slow
// observer never blocks revocation latency.
func (s *Storage) observeRevocation(requestID, kind string) {
	if s.onTokenRevoked == nil {
		return
	}
	issuedAt, err := s.store.OldestAccessTokenIssuedAtByRequestID(requestID)
	if err != nil || issuedAt.IsZero() {
		// No matching family (already pruned), parse failure, or
		// transient store error — record nothing rather than emit a
		// bogus zero/negative duration. Observability noise is worse
		// than a missed datapoint.
		return
	}
	ttl := time.Since(issuedAt)
	if ttl < 0 {
		// Clock skew between issuance and revocation — clamp to zero
		// rather than poison the histogram with negative observations
		// (Prometheus accepts them but the bucket math is meaningless).
		ttl = 0
	}
	s.onTokenRevoked(kind, ttl)
}

// NewStorage wraps a *store.Store as a fosite-compatible adapter.
// The store must be a fully-initialized pad store (migrations applied);
// no validation here because misuse would surface as a runtime panic
// the moment fosite touches an unmigrated table — fast and obvious.
//
// canonicalAudience is the RFC 8707 audience every client implicitly
// allows (PLAN-943: every client in this server is registered to one
// resource — the MCP transport URL). The adapter injects it into the
// fosite.Client.Audience field on hydration so audienceMatchingStrategy's
// haystack-side check passes for every persisted client.
//
// Why inject vs persist: pad's authorization-server is single-resource
// for v1. Storing an audience column per client would always hold the
// same value — purely write amplification — and a future move to a
// multi-resource AS will need explicit per-client policy anyway. Codex
// review #371 round 1 caught the gap where the adapter returned
// Audience=nil and every flow failed validation.
func NewStorage(s *store.Store, canonicalAudience string) *Storage {
	return &Storage{
		store:             s,
		canonicalAudience: canonicalAudience,
	}
}

// =====================================================================
// fosite.ClientManager
// =====================================================================

// GetClient looks up a registered client by ID. Returns fosite.ErrNotFound
// when the client is unknown so fosite's auth-request validator can
// reject with a proper OAuth-error response shape.
func (s *Storage) GetClient(_ context.Context, id string) (fosite.Client, error) {
	c, err := s.store.GetOAuthClient(id)
	if err != nil {
		if errors.Is(err, store.ErrOAuthNotFound) {
			return nil, fosite.ErrNotFound
		}
		return nil, fmt.Errorf("oauth: get client: %w", err)
	}
	return s.modelClientToFosite(c), nil
}

// ClientAssertionJWTValid is a no-op because pad doesn't accept JWT
// client assertions (private_key_jwt / client_secret_jwt) — public
// clients only, PKCE-authenticated. fosite calls this during JWT-
// based client auth flows we don't enable, so returning nil is safe.
//
// If a future change adds JWT client auth, populate a JTI-blocklist
// table and check it here. For now, returning nil means "nothing
// blocklisted" — equivalent to fosite's no-op.
func (s *Storage) ClientAssertionJWTValid(_ context.Context, _ string) error {
	return nil
}

// SetClientAssertionJWT is the companion no-op. Same rationale — we
// don't accept JWT client assertions.
func (s *Storage) SetClientAssertionJWT(_ context.Context, _ string, _ time.Time) error {
	return nil
}

// =====================================================================
// AuthorizeCodeStorage
// =====================================================================

// CreateAuthorizeCodeSession persists a new authorization code's
// fosite.Requester under the supplied signature. fosite calls this
// once per /authorize that yields a code; the signature is the HMAC
// of the code, so a DB read can't replay the actual code value.
func (s *Storage) CreateAuthorizeCodeSession(_ context.Context, signature string, req fosite.Requester) error {
	r, err := requesterToOAuthRequest(req, signature)
	if err != nil {
		return err
	}
	return s.store.CreateAuthorizationCode(r)
}

// GetAuthorizeCodeSession hydrates the session for an auth code. The
// caller supplies an empty fosite.Session (typically &Session{}); we
// JSON-unmarshal the stored session_data into it.
//
// Returning fosite.ErrInvalidatedAuthorizeCode (alongside the request
// payload) when the row is invalidated triggers fosite's grant-family
// revocation in flow_authorize_code_token.go — the canonical "code
// was used twice → revoke the whole grant" behaviour.
func (s *Storage) GetAuthorizeCodeSession(_ context.Context, signature string, session fosite.Session) (fosite.Requester, error) {
	stored, err := s.store.GetAuthorizationCode(signature)
	if errors.Is(err, store.ErrOAuthNotFound) {
		return nil, fosite.ErrNotFound
	}
	// ErrOAuthInvalidatedCode is special: fosite needs the request
	// payload AND the error so it can revoke the grant.
	if errors.Is(err, store.ErrOAuthInvalidatedCode) {
		req, _ := s.oauthRequestToFositeRequest(stored, session)
		return req, fosite.ErrInvalidatedAuthorizeCode
	}
	if err != nil {
		return nil, fmt.Errorf("oauth: get auth code: %w", err)
	}
	return s.oauthRequestToFositeRequest(stored, session)
}

// InvalidateAuthorizeCodeSession marks a code's row inactive. Called
// by fosite once /token successfully exchanges the code; subsequent
// reads return ErrInvalidatedAuthorizeCode (which triggers family
// revocation if the same code is presented again — the OAuth 2.1
// "single-use code" anti-replay rule).
func (s *Storage) InvalidateAuthorizeCodeSession(_ context.Context, signature string) error {
	return s.store.InvalidateAuthorizationCode(signature)
}

// =====================================================================
// AccessTokenStorage
// =====================================================================

// CreateAccessTokenSession persists an access token. The row is always
// inserted with active=true (per insertOAuthRequestRow's contract);
// later RevokeAccessToken / DeleteAccessTokenSession flip or remove it.
func (s *Storage) CreateAccessTokenSession(_ context.Context, signature string, req fosite.Requester) error {
	r, err := requesterToOAuthRequest(req, signature)
	if err != nil {
		return err
	}
	return s.store.CreateAccessToken(r)
}

// GetAccessTokenSession hydrates an access token. Inactive rows
// surface as fosite.ErrInactiveToken so the introspection /
// authorization-bearer middleware can reject cleanly.
//
// Inactive rows return the request payload AND the error so callers
// (introspector, revocation handler) can read req.GetID() for grant-
// scoped follow-up actions without nil-pointer dereferencing.
// Codex review #371 round 2 caught the equivalent gap on the refresh
// path; symmetric fix here for safety even though the access path's
// callers don't currently dereference on inactive.
func (s *Storage) GetAccessTokenSession(_ context.Context, signature string, session fosite.Session) (fosite.Requester, error) {
	stored, err := s.store.GetAccessToken(signature)
	if errors.Is(err, store.ErrOAuthNotFound) {
		return nil, fosite.ErrNotFound
	}
	if errors.Is(err, store.ErrOAuthInactiveToken) {
		req, _ := s.oauthRequestToFositeRequest(stored, session)
		return req, fosite.ErrInactiveToken
	}
	if err != nil {
		return nil, fmt.Errorf("oauth: get access token: %w", err)
	}
	return s.oauthRequestToFositeRequest(stored, session)
}

// DeleteAccessTokenSession removes the row entirely. Distinct from
// RevokeAccessToken (which preserves the row marked inactive for
// audit). fosite uses Delete after successful exchange of an
// authorization code to prevent reuse.
func (s *Storage) DeleteAccessTokenSession(_ context.Context, signature string) error {
	return s.store.DeleteAccessToken(signature)
}

// =====================================================================
// RefreshTokenStorage
// =====================================================================

// CreateRefreshTokenSession persists a refresh token alongside its
// paired access token signature. The pair sharing a request_id is
// what makes family revocation work: revoking by request_id walks
// the indexed column and flips every chain member.
func (s *Storage) CreateRefreshTokenSession(_ context.Context, signature, accessSignature string, req fosite.Requester) error {
	r, err := requesterToOAuthRequest(req, signature)
	if err != nil {
		return err
	}
	r.AccessTokenSignature = accessSignature
	return s.store.CreateRefreshToken(r)
}

// GetRefreshTokenSession hydrates a refresh token. Inactive rows
// surface as fosite.ErrInactiveToken — fosite's refresh-flow handler
// (handler/oauth2/flow_refresh.go) treats this as a replay signal
// and triggers RevokeRefreshToken / RevokeAccessToken on the
// request_id, which under our adapter walks the entire family and
// revokes it (the OAuth 2.1 BCP §4.14 rule).
//
// Crucially, the inactive-row path returns the request payload
// ALONGSIDE the error: fosite's handleRefreshTokenReuse
// (flow_refresh.go:178-204) calls req.GetID() on the returned value
// to drive RevokeRefreshToken(requestID) + RevokeAccessToken(requestID).
// Returning a nil request would nil-deref the family-revocation
// flow, defeating replay detection. Codex review #371 round 2 caught
// this. The symmetric pattern is used by the auth-code invalidation
// path (GetAuthorizeCodeSession above).
func (s *Storage) GetRefreshTokenSession(_ context.Context, signature string, session fosite.Session) (fosite.Requester, error) {
	stored, err := s.store.GetRefreshToken(signature)
	if errors.Is(err, store.ErrOAuthNotFound) {
		return nil, fosite.ErrNotFound
	}
	if errors.Is(err, store.ErrOAuthInactiveToken) {
		// fosite needs req.GetID() to revoke the family; hydrate even
		// on the failure path. Hydration may itself fail (e.g. the
		// client was deleted) — in that case return the underlying
		// error rather than masking it; replay detection still loses
		// but at least the failure is observable.
		req, hydrateErr := s.oauthRequestToFositeRequest(stored, session)
		if hydrateErr != nil {
			return nil, hydrateErr
		}
		return req, fosite.ErrInactiveToken
	}
	if err != nil {
		return nil, fmt.Errorf("oauth: get refresh token: %w", err)
	}
	return s.oauthRequestToFositeRequest(stored, session)
}

// DeleteRefreshTokenSession removes a refresh row entirely. Used by
// fosite's rotation flow to drop the previous-step's refresh after
// the new one has been issued + the old one rotated.
func (s *Storage) DeleteRefreshTokenSession(_ context.Context, signature string) error {
	return s.store.DeleteRefreshToken(signature)
}

// RotateRefreshToken matches fosite's reference MemoryStore
// (storage/memory.go:497-504): revoke the entire grant — both the
// refresh family AND the access family — for the request_id, then
// fosite immediately issues a fresh pair via CreateAccessTokenSession
// + CreateRefreshTokenSession (which inherit the same request_id and
// land active=TRUE per the store's hardcode).
//
// signatureToRotate is fosite's hint about which row triggered the
// rotation; the store layer ignores it and revokes by request_id.
func (s *Storage) RotateRefreshToken(_ context.Context, requestID, signatureToRotate string) error {
	if err := s.store.RotateRefreshToken(requestID, signatureToRotate); err != nil {
		return err
	}
	// TASK-961: report rotation-induced revocation to the observability
	// hook. Distinct kind label so dashboards can separate "user clicked
	// revoke" from "client refreshed and we naturally rolled the family."
	s.observeRevocation(requestID, "rotated")
	return nil
}

// =====================================================================
// TokenRevocationStorage (RFC 7009)
// =====================================================================

// RevokeRefreshToken walks the chain of refresh tokens sharing the
// given requestID and marks every one inactive. Called by fosite's
// /oauth/revoke handler (sub-PR D wires the endpoint) and by the
// rotation flow's replay-detection branch.
//
// No metrics observation here: when fosite revokes a grant family
// from /oauth/revoke or replay-detection, it pairs RevokeRefreshToken
// with RevokeAccessToken — emitting from both would double-count.
// We let the access-side emitter own the reporting and keep this
// path lean.
func (s *Storage) RevokeRefreshToken(_ context.Context, requestID string) error {
	return s.store.RevokeRefreshTokenFamily(requestID)
}

// RevokeAccessToken mirrors RevokeRefreshToken for access tokens.
// fosite calls these in pairs when revoking a grant (the unified
// "revoke the whole family" behaviour).
//
// TASK-961: emits an observability signal AFTER the family is
// flagged inactive in storage. The default kind here is
// "user_initiated" because /oauth/revoke is the dominant caller —
// fosite's replay-detection path also routes through this method,
// so the label is best-effort rather than ground truth (Codex
// could later differentiate via a context-carried hint, but the
// gross signal "tokens are getting revoked" matters more than the
// per-cause split for v1 alerting).
func (s *Storage) RevokeAccessToken(_ context.Context, requestID string) error {
	if err := s.store.RevokeAccessTokenFamily(requestID); err != nil {
		return err
	}
	s.observeRevocation(requestID, "user_initiated")
	return nil
}

// =====================================================================
// PKCERequestStorage
// =====================================================================

// CreatePKCERequestSession persists the PKCE session keyed by the
// auth-code's signature. fosite stores the original /authorize
// request (which carries code_challenge + code_challenge_method) so
// the verifier from /token can be checked against it.
func (s *Storage) CreatePKCERequestSession(_ context.Context, signature string, req fosite.Requester) error {
	r, err := requesterToOAuthRequest(req, signature)
	if err != nil {
		return err
	}
	return s.store.CreatePKCERequest(r)
}

// GetPKCERequestSession hydrates the PKCE session. Returns
// fosite.ErrNotFound when missing (e.g. someone replayed an old
// auth code that's already been deleted post-exchange).
func (s *Storage) GetPKCERequestSession(_ context.Context, signature string, session fosite.Session) (fosite.Requester, error) {
	stored, err := s.store.GetPKCERequest(signature)
	if errors.Is(err, store.ErrOAuthNotFound) {
		return nil, fosite.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("oauth: get pkce request: %w", err)
	}
	return s.oauthRequestToFositeRequest(stored, session)
}

// DeletePKCERequestSession removes the row after a successful /token
// exchange. fosite's PKCE lifecycle is delete-on-use, distinct from
// auth codes (which are flagged inactive so a replay can be
// distinguished from a missing row).
func (s *Storage) DeletePKCERequestSession(_ context.Context, signature string) error {
	return s.store.DeletePKCERequest(signature)
}

// =====================================================================
// Translation helpers
// =====================================================================

// requesterToOAuthRequest converts a fosite.Requester to the flat
// model the store layer accepts. Session and form data are JSON-
// encoded so the storage layer never imports fosite types.
//
// Consumes: req.GetID, req.GetClient.GetID, req.GetRequestedAt,
// req.GetRequestedScopes, req.GetGrantedScopes, req.GetRequestForm,
// req.GetSession, req.GetRequestedAudience, req.GetGrantedAudience.
//
// signature is the HMAC of the token / code value (provided by
// fosite at insert time), separate from the requester so the same
// requester can be persisted under multiple signatures during a
// single grant flow.
func requesterToOAuthRequest(req fosite.Requester, signature string) (models.OAuthRequest, error) {
	if req == nil {
		return models.OAuthRequest{}, fmt.Errorf("oauth: nil requester")
	}
	if req.GetClient() == nil || req.GetClient().GetID() == "" {
		return models.OAuthRequest{}, fmt.Errorf("oauth: requester missing client")
	}

	sessionBytes := []byte("{}")
	if sess := req.GetSession(); sess != nil {
		var err error
		sessionBytes, err = json.Marshal(sess)
		if err != nil {
			return models.OAuthRequest{}, fmt.Errorf("oauth: encode session: %w", err)
		}
	}

	subject := ""
	if sess := req.GetSession(); sess != nil {
		subject = sess.GetSubject()
	}

	return models.OAuthRequest{
		Signature:       signature,
		RequestID:       req.GetID(),
		RequestedAt:     req.GetRequestedAt(),
		ClientID:        req.GetClient().GetID(),
		Scopes:          strings.Join(req.GetRequestedScopes(), " "),
		GrantedScopes:   strings.Join(req.GetGrantedScopes(), " "),
		RequestForm:     req.GetRequestForm().Encode(),
		SessionData:     string(sessionBytes),
		Audience:        strings.Join(req.GetRequestedAudience(), " "),
		GrantedAudience: strings.Join(req.GetGrantedAudience(), " "),
		Subject:         subject,
	}, nil
}

// oauthRequestToFositeRequest hydrates a stored row into a fresh
// fosite.Request, fetching the client by ID and unmarshalling the
// session bytes into the caller-supplied session pointer.
//
// fosite's contract is that the session pointer (passed by fosite to
// every Get*Session call) gets populated in-place — it's how fosite
// flows the session through the handler chain. We JSON-unmarshal
// directly into it so the caller's concrete type (e.g. *Session)
// stays intact.
//
// The form is parsed back from URL-encoded; any decode error means
// the storage row was tampered with or written by an incompatible
// adapter version, and we surface as an internal error rather than
// silently dropping fields.
func (s *Storage) oauthRequestToFositeRequest(stored *models.OAuthRequest, session fosite.Session) (fosite.Requester, error) {
	if stored == nil {
		return nil, fosite.ErrNotFound
	}

	client, err := s.store.GetOAuthClient(stored.ClientID)
	if err != nil {
		if errors.Is(err, store.ErrOAuthNotFound) {
			// The client was deleted while this token was active —
			// e.g. an admin-driven revocation in between issuance and
			// use. Treat as not-found so the calling handler returns
			// a clean OAuth error.
			return nil, fosite.ErrNotFound
		}
		return nil, fmt.Errorf("oauth: hydrate client: %w", err)
	}

	if session != nil && stored.SessionData != "" {
		if err := json.Unmarshal([]byte(stored.SessionData), session); err != nil {
			return nil, fmt.Errorf("oauth: decode session: %w", err)
		}
	}

	form, err := url.ParseQuery(stored.RequestForm)
	if err != nil {
		return nil, fmt.Errorf("oauth: parse request form: %w", err)
	}

	return &fosite.Request{
		ID:                stored.RequestID,
		RequestedAt:       stored.RequestedAt,
		Client:            s.modelClientToFosite(client),
		RequestedScope:    splitSpaceSeparated(stored.Scopes),
		GrantedScope:      splitSpaceSeparated(stored.GrantedScopes),
		Form:              form,
		Session:           session,
		RequestedAudience: splitSpaceSeparated(stored.Audience),
		GrantedAudience:   splitSpaceSeparated(stored.GrantedAudience),
	}, nil
}

// modelClientToFosite turns a stored client row into the
// fosite.DefaultClient fosite expects. Public clients only — Secret
// stays empty; PKCE is the only auth path.
//
// Audience: every client in this server is implicitly authorized
// for the canonical audience (PLAN-943: single-resource AS). The
// adapter injects Storage.canonicalAudience into the hydrated
// client's Audience field so audienceMatchingStrategy's haystack
// check (client.GetAudience() must contain canonical) passes for
// every persisted client. Codex review #371 round 1 caught the bug
// where this returned Audience=nil and every flow failed validation.
//
// If canonicalAudience is empty (a misconfigured Storage — should
// never happen in production because NewServer rejects empty
// AllowedAudience), Audience stays nil and the strategy will reject
// every request with ServerError, surfacing the misconfiguration
// fast.
func (s *Storage) modelClientToFosite(c *models.OAuthClient) fosite.Client {
	var audience []string
	if s.canonicalAudience != "" {
		audience = []string{s.canonicalAudience}
	}
	return &fosite.DefaultClient{
		ID:            c.ID,
		Secret:        nil, // public client
		RedirectURIs:  append([]string(nil), c.RedirectURIs...),
		GrantTypes:    append([]string(nil), c.GrantTypes...),
		ResponseTypes: append([]string(nil), c.ResponseTypes...),
		Scopes:        append([]string(nil), c.Scopes...),
		Audience:      audience,
		Public:        true,
	}
}

// splitSpaceSeparated decodes the "a b c" form back to fosite.Arguments
// (a []string alias). Empty input → empty slice (non-nil) so range
// is safe.
func splitSpaceSeparated(s string) fosite.Arguments {
	s = strings.TrimSpace(s)
	if s == "" {
		return fosite.Arguments{}
	}
	parts := strings.Split(s, " ")
	out := make(fosite.Arguments, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
