package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// OAuth 2.1 server storage layer (PLAN-943 TASK-951 sub-PR A).
//
// This file implements pure CRUD against the five oauth_* tables
// added by migrations/048_oauth.sql + pgmigrations/027_oauth.sql.
// The methods are intentionally fosite-agnostic: callers pass and
// receive pad-internal types (models.OAuthClient, models.OAuthRequest)
// rather than fosite.Client / fosite.Requester. Sub-PR B (TASK-1024)
// adds the fosite import + adapter wrappers in internal/oauth/.
//
// Why the boundary lives here:
//
// fosite's storage interfaces accept fosite.Requester (an interface
// with ~20 methods) and fosite.Session (another interface with custom
// claims). Implementing them directly in this package would pull
// fosite into every Pad build, including builds that don't host the
// MCP/OAuth surface (self-hosted CLI users). Keeping the storage
// layer in pure Go means:
//
//   - Self-host builds skip the fosite dep entirely (smaller binary).
//   - The schema can be tested in isolation with simple structs.
//   - Sub-PR B's adapter is the single place a Requester ⇄ DB row
//     translation happens — easier to audit + mock in tests.
//
// Concurrency:
//
// Each method runs as a single SQL statement. SQLite serializes
// writes globally (BEGIN IMMEDIATE per store.go), so the visible
// behaviour is one-at-a-time even under concurrent callers; Postgres
// allows row-level concurrent writes. Both backends are safe for the
// "rotate refresh = invalidate one row + insert two rows" pattern
// fosite uses, because fosite always issues those calls inside the
// same handler request and pad's existing /api/v1/* code paths
// don't share rows with the OAuth grant flow.

// Sentinel errors. fosite-side adapters in sub-PR B map these to
// fosite.ErrNotFound / fosite.ErrInvalidatedAuthorizeCode etc.
// Defined here so the storage layer can return them without
// importing fosite.
var (
	// ErrOAuthNotFound is returned when a row lookup misses (no
	// matching client / signature). Callers should map to fosite.ErrNotFound.
	ErrOAuthNotFound = errors.New("oauth: not found")

	// ErrOAuthInvalidatedCode is returned when GetAuthorizationCode
	// hits a row whose active flag is false. fosite expects
	// ErrInvalidatedAuthorizeCode in that case (it uses the error
	// to trigger family-revocation on the original request).
	ErrOAuthInvalidatedCode = errors.New("oauth: authorization code invalidated")

	// ErrOAuthInactiveToken signals an access/refresh row exists but
	// has been revoked or rotated out. Adapters map to
	// fosite.ErrInactiveToken.
	ErrOAuthInactiveToken = errors.New("oauth: token inactive")
)

// ============================================================
// Clients (RFC 7591 — Dynamic Client Registration)
// ============================================================

// CreateOAuthClient inserts a new client and returns its issued
// client_id (random ID + creation timestamp).
//
// Validation lives at the HTTP boundary in sub-PR C; this method
// trusts the caller. Empty slices serialize to "[]" so reads always
// produce a stable JSON shape.
func (s *Store) CreateOAuthClient(input models.OAuthClientCreate) (*models.OAuthClient, error) {
	id := newID()
	created := time.Now().UTC()
	createdStr := created.Format(time.RFC3339)

	redirects, err := jsonStringList(input.RedirectURIs)
	if err != nil {
		return nil, fmt.Errorf("encode redirect_uris: %w", err)
	}
	grants, err := jsonStringList(input.GrantTypes)
	if err != nil {
		return nil, fmt.Errorf("encode grant_types: %w", err)
	}
	respTypes, err := jsonStringList(input.ResponseTypes)
	if err != nil {
		return nil, fmt.Errorf("encode response_types: %w", err)
	}
	scopes, err := jsonStringList(input.Scopes)
	if err != nil {
		return nil, fmt.Errorf("encode scopes: %w", err)
	}

	authMethod := input.TokenEndpointAuthMethod
	if authMethod == "" {
		authMethod = "none"
	}

	// Postgres uses a BOOLEAN column for `public`; SQLite uses INTEGER.
	// Both accept Go's bool via the standard driver — no dialect-
	// specific handling needed at the call site.
	_, err = s.db.Exec(s.q(`
		INSERT INTO oauth_clients (
			id, name, redirect_uris, grant_types, response_types,
			token_endpoint_auth_method, scopes, public, logo_url, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`), id, input.Name, redirects, grants, respTypes, authMethod,
		scopes, input.Public, input.LogoURL, createdStr)
	if err != nil {
		return nil, fmt.Errorf("insert oauth client: %w", err)
	}

	return &models.OAuthClient{
		ID:                      id,
		Name:                    input.Name,
		RedirectURIs:            cloneStringSlice(input.RedirectURIs),
		GrantTypes:              cloneStringSlice(input.GrantTypes),
		ResponseTypes:           cloneStringSlice(input.ResponseTypes),
		TokenEndpointAuthMethod: authMethod,
		Scopes:                  cloneStringSlice(input.Scopes),
		Public:                  input.Public,
		LogoURL:                 input.LogoURL,
		CreatedAt:               created,
	}, nil
}

// GetOAuthClient looks up a registered client by ID. Returns
// ErrOAuthNotFound if no row matches.
func (s *Store) GetOAuthClient(id string) (*models.OAuthClient, error) {
	var (
		c                                                models.OAuthClient
		redirectsRaw, grantsRaw, respTypesRaw, scopesRaw string
		logoURL                                          sql.NullString
		createdStr                                       string
		public                                           bool
	)
	err := s.db.QueryRow(s.q(`
		SELECT id, name, redirect_uris, grant_types, response_types,
		       token_endpoint_auth_method, scopes, public, logo_url, created_at
		FROM oauth_clients WHERE id = ?
	`), id).Scan(&c.ID, &c.Name, &redirectsRaw, &grantsRaw, &respTypesRaw,
		&c.TokenEndpointAuthMethod, &scopesRaw, &public, &logoURL, &createdStr)
	if err == sql.ErrNoRows {
		return nil, ErrOAuthNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query oauth client: %w", err)
	}

	if c.RedirectURIs, err = parseJSONStringList(redirectsRaw); err != nil {
		return nil, fmt.Errorf("decode redirect_uris: %w", err)
	}
	if c.GrantTypes, err = parseJSONStringList(grantsRaw); err != nil {
		return nil, fmt.Errorf("decode grant_types: %w", err)
	}
	if c.ResponseTypes, err = parseJSONStringList(respTypesRaw); err != nil {
		return nil, fmt.Errorf("decode response_types: %w", err)
	}
	if c.Scopes, err = parseJSONStringList(scopesRaw); err != nil {
		return nil, fmt.Errorf("decode scopes: %w", err)
	}
	c.Public = public
	c.LogoURL = logoURL.String
	c.CreatedAt = parseTime(createdStr)
	return &c, nil
}

// DeleteOAuthClient removes a client AND every dependent row (auth
// codes, access tokens, refresh tokens, PKCE sessions) atomically.
//
// Why explicit cascade in code rather than ON DELETE CASCADE on the
// FKs: keeping the FK as a plain reference means a stray
// `DELETE FROM oauth_clients` from a future migration / admin tool
// errors loudly instead of silently nuking grants. The intentional
// cascade lives here, in one named method, so its blast radius is
// obvious and auditable. Codex review #370 round 3 caught the
// previous behaviour where DeleteOAuthClient errored on FK violation
// for any client that had ever issued a grant.
//
// Postgres concurrency note (Codex review #370 round 4): without the
// row-level lock below, a concurrent grant/token insert on the same
// client_id could land between our child-row deletes and the parent
// delete, producing a fresh FK reference that fails the parent
// delete. We take SELECT … FOR UPDATE on the client row as the very
// first statement in the tx, which blocks any concurrent insert
// trying to read the client (which fosite does as part of FK
// resolution on grant insert) until our tx commits. SQLite serializes
// writes via BEGIN IMMEDIATE (set globally in store.go's DSN), so
// the race doesn't exist there — we skip the FOR UPDATE on SQLite
// because the syntax isn't universally supported by the driver.
//
// Idempotent: calling on a non-existent client is a no-op (every
// dependent table's WHERE matches nothing; the FOR UPDATE locks no
// row but doesn't error). Atomic: if any DELETE fails, the whole
// transaction rolls back so we never leave a half-deleted client.
func (s *Store) DeleteOAuthClient(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin delete oauth client tx: %w", err)
	}
	defer tx.Rollback() // no-op after Commit

	// Postgres: lock the parent row first to serialize against
	// concurrent grant/token inserts. Skipped on SQLite because
	// (a) BEGIN IMMEDIATE already serializes writes, and (b) the
	// FOR UPDATE syntax isn't recognized by every SQLite driver.
	if s.dialect.Driver() == DriverPostgres {
		var locked sql.NullString
		err := tx.QueryRow(s.q(`SELECT id FROM oauth_clients WHERE id = ? FOR UPDATE`), id).Scan(&locked)
		if err != nil && err != sql.ErrNoRows {
			return fmt.Errorf("lock oauth client row: %w", err)
		}
		// sql.ErrNoRows is fine: the client doesn't exist, so the
		// subsequent DELETEs match nothing and the call stays
		// idempotent. We still proceed through the deletes for
		// uniform code paths.
	}

	// Order: child rows first, parent (oauth_clients) last. Required
	// for the FK constraint to be satisfied at commit time when ON
	// DELETE CASCADE isn't set. The order between the four child
	// tables doesn't matter — none reference each other.
	deletes := []string{
		`DELETE FROM oauth_pkce_requests WHERE client_id = ?`,
		`DELETE FROM oauth_refresh_tokens WHERE client_id = ?`,
		`DELETE FROM oauth_access_tokens WHERE client_id = ?`,
		`DELETE FROM oauth_authorization_codes WHERE client_id = ?`,
		`DELETE FROM oauth_clients WHERE id = ?`,
	}
	for _, q := range deletes {
		if _, err := tx.Exec(s.q(q), id); err != nil {
			return fmt.Errorf("delete oauth client cascade (%q): %w", q, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delete oauth client: %w", err)
	}
	return nil
}

// ============================================================
// Authorization codes (handler/oauth2/storage.go AuthorizeCodeStorage)
// ============================================================

// CreateAuthorizationCode persists a code's session under its
// signature. The signature is fosite's HMAC of the code value;
// the code itself is never stored, so a DB read can't replay it.
//
// The row's `active` column is always set TRUE on insert; callers
// flip it via InvalidateAuthorizationCode after the code is
// exchanged. Pre-seeding an inactive row isn't a supported flow —
// fosite never does it.
func (s *Store) CreateAuthorizationCode(req models.OAuthRequest) error {
	return s.insertOAuthRequestRow("oauth_authorization_codes", req)
}

// GetAuthorizationCode hydrates the code's session. Returns
// ErrOAuthInvalidatedCode if the row exists but active=false (fosite
// triggers grant-family revocation on this); ErrOAuthNotFound if the
// row is missing entirely.
func (s *Store) GetAuthorizationCode(signature string) (*models.OAuthRequest, error) {
	req, active, err := s.queryOAuthRequestRow("oauth_authorization_codes", signature)
	if err != nil {
		return nil, err
	}
	if !active {
		// Per fosite contract: still return the request payload
		// alongside the invalidation error so the caller can run
		// family revocation against the now-known request_id.
		return req, ErrOAuthInvalidatedCode
	}
	return req, nil
}

// InvalidateAuthorizationCode marks a code inactive (single-use).
// Idempotent: invalidating an already-invalid or missing row is a
// no-op (fosite's contract here is "make it invalid", not "fail if
// it's already invalid").
func (s *Store) InvalidateAuthorizationCode(signature string) error {
	_, err := s.db.Exec(s.q(`
		UPDATE oauth_authorization_codes SET active = ? WHERE signature = ?
	`), false, signature)
	if err != nil {
		return fmt.Errorf("invalidate auth code: %w", err)
	}
	return nil
}

// ============================================================
// Access tokens (handler/oauth2/storage.go AccessTokenStorage)
// ============================================================

// CreateAccessToken persists an access token row. The row's
// `active` column is always set TRUE on insert; callers flip it
// via DeleteAccessToken (drop) or RevokeAccessTokenFamily (mark
// inactive but keep the row for introspection auditing). The
// caller's `req.Active` field is ignored on insert — fosite never
// issues pre-revoked tokens, so honoring it would only enable
// silently-broken adapters. Codex review #370 round 1 caught the
// previous bug where zero-value `req.Active=false` collided with
// the default and silently issued inactive tokens.
func (s *Store) CreateAccessToken(req models.OAuthRequest) error {
	return s.insertOAuthRequestRow("oauth_access_tokens", req)
}

// GetAccessToken hydrates an access token by its HMAC signature.
// Returns ErrOAuthInactiveToken if the row exists but has been
// revoked / rotated out, ErrOAuthNotFound if missing.
func (s *Store) GetAccessToken(signature string) (*models.OAuthRequest, error) {
	req, active, err := s.queryOAuthRequestRow("oauth_access_tokens", signature)
	if err != nil {
		return nil, err
	}
	if !active {
		return req, ErrOAuthInactiveToken
	}
	return req, nil
}

// DeleteAccessToken removes a row entirely (fosite uses Delete after
// successful exchange to prevent reuse). Distinct from RevokeAccessToken
// which preserves the row for introspection auditing.
func (s *Store) DeleteAccessToken(signature string) error {
	_, err := s.db.Exec(s.q(`DELETE FROM oauth_access_tokens WHERE signature = ?`), signature)
	if err != nil {
		return fmt.Errorf("delete access token: %w", err)
	}
	return nil
}

// ============================================================
// Refresh tokens (handler/oauth2/storage.go RefreshTokenStorage)
// ============================================================

// CreateRefreshToken persists a refresh token. accessSignature links
// the refresh to the access token issued in the same grant; pass
// empty string when there is no paired access token (rare — only
// during error recovery flows). Same active-on-insert contract as
// CreateAccessToken — the caller's req.Active field is ignored.
func (s *Store) CreateRefreshToken(req models.OAuthRequest) error {
	return s.insertOAuthRequestRow("oauth_refresh_tokens", req)
}

// GetRefreshToken hydrates a refresh token. Returns
// ErrOAuthInactiveToken when active=false, which fosite uses to
// trigger family revocation on the matching request_id (the OAuth
// 2.1 BCP "revoke the whole family on replay" rule). Sub-PR D wires
// the family-revocation walk via RevokeRefreshTokenFamily.
func (s *Store) GetRefreshToken(signature string) (*models.OAuthRequest, error) {
	req, active, err := s.queryOAuthRequestRow("oauth_refresh_tokens", signature)
	if err != nil {
		return nil, err
	}
	if !active {
		return req, ErrOAuthInactiveToken
	}
	return req, nil
}

// DeleteRefreshToken removes a refresh row entirely. Used by the
// rotation flow to drop the previous-step's refresh after the new
// one is in place.
func (s *Store) DeleteRefreshToken(signature string) error {
	_, err := s.db.Exec(s.q(`DELETE FROM oauth_refresh_tokens WHERE signature = ?`), signature)
	if err != nil {
		return fmt.Errorf("delete refresh token: %w", err)
	}
	return nil
}

// RotateRefreshToken revokes the entire grant — both the refresh
// family AND the paired access family for the given requestID —
// then fosite immediately issues a new refresh + access pair via
// CreateRefreshTokenSession + CreateAccessTokenSession.
//
// This matches fosite's reference MemoryStore.RotateRefreshToken
// (storage/memory.go:497-504), which delegates to
// RevokeRefreshToken + RevokeAccessToken: the simple "revoke the
// whole grant on rotation" model rather than the more complex
// graceful-rotation pattern fosite mentions but doesn't implement.
//
// Why both families: every token in a refresh-rotation chain shares
// the same request_id (fosite preserves it via flow_refresh.go:86),
// so the new access + new refresh fosite issues immediately after
// this call inherit the same request_id. They get inserted with
// active=TRUE per insertOAuthRequestRow's hardcode, so the net
// state is "all old rows in this chain inactive; the new pair
// active." Without revoking the access family here, the previously-
// issued access token would remain active until its TTL expired —
// the bug Codex review #370 round 2 caught.
//
// signatureToRotate is fosite's hint about which specific refresh
// row triggered the rotation; we ignore it because revoking by
// request_id catches the entire chain whether we flip one row at
// a time or all at once.
func (s *Store) RotateRefreshToken(requestID, _ /*signatureToRotate*/ string) error {
	if err := s.RevokeRefreshTokenFamily(requestID); err != nil {
		return fmt.Errorf("rotate refresh: revoke refresh family: %w", err)
	}
	if err := s.RevokeAccessTokenFamily(requestID); err != nil {
		return fmt.Errorf("rotate refresh: revoke access family: %w", err)
	}
	return nil
}

// ============================================================
// Token revocation (handler/oauth2/revocation_storage.go)
// ============================================================

// RevokeRefreshTokenFamily marks every refresh token sharing the
// given requestID inactive — the OAuth 2.1 BCP §4.14
// (refresh_token_replay rule) "revoke the whole family on a replayed
// refresh" behaviour. fosite's RevokeRefreshToken adapter in sub-PR
// B calls this when GetRefreshToken returns ErrOAuthInactiveToken.
//
// The walk uses the request_id index (oauth_refresh_request_id_idx)
// added by the migration, so it stays O(family-size) even when the
// table grows. Idempotent — re-revoking an already-inactive family
// is a no-op.
func (s *Store) RevokeRefreshTokenFamily(requestID string) error {
	_, err := s.db.Exec(s.q(`
		UPDATE oauth_refresh_tokens SET active = ? WHERE request_id = ?
	`), false, requestID)
	if err != nil {
		return fmt.Errorf("revoke refresh family: %w", err)
	}
	return nil
}

// CountActiveOAuthAccessTokens returns the number of access-token rows
// with active=1. Backs the pad_oauth_active_tokens gauge (TASK-961).
//
// Caveat: the schema does not store an expires_at column — fosite
// computes expiry from session_data at introspection time. Counting
// active=1 therefore over-reports: tokens that have aged past their
// TTL but haven't been pruned (and haven't been re-introspected since
// expiry) still appear active here. For an operational gauge that's
// fine — "tokens we still consider valid storage-side" is exactly the
// signal ops want for capacity / abuse alerting. A future expiry
// sweeper would reduce the gap; until then the count is an upper
// bound rather than ground truth.
func (s *Store) CountActiveOAuthAccessTokens() (int64, error) {
	var n int64
	err := s.db.QueryRow(s.q(`
		SELECT COUNT(*) FROM oauth_access_tokens WHERE active = ?
	`), true).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count active oauth access tokens: %w", err)
	}
	return n, nil
}

// OldestAccessTokenIssuedAtByRequestID returns the requested_at of the
// earliest access token in the family identified by requestID. Used
// by the OAuth revocation path (TASK-961) to observe token TTL —
// "how long was this token alive before it got revoked?"
//
// Why MIN(requested_at) rather than per-row reporting: a single grant
// family typically holds one access token at a time (the refresh-
// rotation flow revokes the prior one before issuing the new one),
// but the "rotated" code path briefly stages two during the swap.
// Using the oldest row's timestamp gives a meaningful "lifetime of
// the original grant" measurement that doesn't oscillate based on
// rotation timing.
//
// Returns ErrOAuthNotFound when the family has no rows (already
// pruned, or revoke called with an unknown request_id). Callers may
// safely treat that as "no observation to record."
func (s *Store) OldestAccessTokenIssuedAtByRequestID(requestID string) (time.Time, error) {
	if requestID == "" {
		return time.Time{}, fmt.Errorf("oauth: requestID required")
	}
	var raw sql.NullString
	err := s.db.QueryRow(s.q(`
		SELECT MIN(requested_at) FROM oauth_access_tokens WHERE request_id = ?
	`), requestID).Scan(&raw)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return time.Time{}, ErrOAuthNotFound
		}
		return time.Time{}, fmt.Errorf("query oldest access token: %w", err)
	}
	if !raw.Valid {
		// MIN returns NULL when the family has zero rows. SQLite drops
		// the row through a single QueryRow.Scan rather than ErrNoRows
		// because the aggregate "produces" exactly one (null) row.
		return time.Time{}, ErrOAuthNotFound
	}
	return parseTime(raw.String), nil
}

// RevokeAccessTokenFamily mirrors RevokeRefreshTokenFamily for
// access tokens. fosite calls these in pairs when revoking by
// request_id (the unified RevokeRefreshToken / RevokeAccessToken
// pair in TokenRevocationStorage).
func (s *Store) RevokeAccessTokenFamily(requestID string) error {
	_, err := s.db.Exec(s.q(`
		UPDATE oauth_access_tokens SET active = ? WHERE request_id = ?
	`), false, requestID)
	if err != nil {
		return fmt.Errorf("revoke access family: %w", err)
	}
	return nil
}

// ============================================================
// PKCE request sessions (handler/pkce/storage.go PKCERequestStorage)
// ============================================================

// CreatePKCERequest persists a PKCE session keyed by the auth code's
// signature. fosite stores the original /authorize request here so
// the code_challenge can be re-validated against the code_verifier
// on /token exchange.
func (s *Store) CreatePKCERequest(req models.OAuthRequest) error {
	// PKCE rows have no `active` column (lifecycle is "exists or
	// deleted" rather than "active or revoked"). insertOAuthRequestRow
	// with table=oauth_pkce_requests skips the active column write.
	return s.insertOAuthRequestRow("oauth_pkce_requests", req)
}

// GetPKCERequest hydrates the PKCE session. Returns ErrOAuthNotFound
// if the row is missing.
func (s *Store) GetPKCERequest(signature string) (*models.OAuthRequest, error) {
	req, _, err := s.queryOAuthRequestRow("oauth_pkce_requests", signature)
	return req, err
}

// DeletePKCERequest removes the row after a successful /token
// exchange (fosite's lifecycle for PKCE is delete-on-use, unlike
// auth codes which are flagged inactive).
func (s *Store) DeletePKCERequest(signature string) error {
	_, err := s.db.Exec(s.q(`DELETE FROM oauth_pkce_requests WHERE signature = ?`), signature)
	if err != nil {
		return fmt.Errorf("delete pkce request: %w", err)
	}
	return nil
}

// ============================================================
// Helpers — shared by the three "request-row" tables
// ============================================================

// insertOAuthRequestRow writes a request payload into the named
// table. The four request-row tables (auth codes, access tokens,
// refresh tokens, pkce) share the same column set with three
// exceptions:
//
//   - pkce has no `active` column (no revocation lifecycle).
//   - access + refresh have a `subject` column for fast subject-
//     bound lookups; auth codes have a subject column too (set when
//     consent fires); pkce sets it to the empty string for now.
//   - refresh has an `access_token_signature` column linking to the
//     access row issued in the same grant.
//
// Active-on-insert contract: the three flagged tables (codes /
// access / refresh) ALWAYS get `active=TRUE`. The caller's
// req.Active field is ignored — fosite never issues pre-revoked
// tokens, and honoring zero-value Active=false would silently
// produce immediately-revoked tokens for any adapter that didn't
// remember to set it. Codex review #370 round 1.
//
// Switch on the table name to pick the right INSERT — uglier than
// per-table methods but keeps the call sites uniform and lets the
// helpers below share the same row-decoding code on read. The
// per-table methods above (CreateAccessToken etc.) are the public
// surface; this is the private worker.
func (s *Store) insertOAuthRequestRow(table string, req models.OAuthRequest) error {
	if req.Signature == "" {
		return fmt.Errorf("oauth: signature required")
	}
	if req.RequestID == "" {
		return fmt.Errorf("oauth: request_id required")
	}
	if req.ClientID == "" {
		return fmt.Errorf("oauth: client_id required")
	}

	requestedAt := req.RequestedAt
	if requestedAt.IsZero() {
		requestedAt = time.Now().UTC()
	}
	requestedStr := requestedAt.UTC().Format(time.RFC3339)

	// Always TRUE on insert for the three flagged tables. Callers
	// drop a row to inactive via Invalidate / Rotate /
	// RevokeXxxFamily; pre-seeding inactive isn't a supported flow.
	const active = true

	switch table {
	case "oauth_authorization_codes":
		_, err := s.db.Exec(s.q(`
			INSERT INTO oauth_authorization_codes (
				signature, request_id, requested_at, client_id,
				scopes, granted_scopes, request_form, session_data,
				audience, granted_audience, active
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`), req.Signature, req.RequestID, requestedStr, req.ClientID,
			req.Scopes, req.GrantedScopes, req.RequestForm, req.SessionData,
			req.Audience, req.GrantedAudience, active)
		if err != nil {
			return fmt.Errorf("insert auth code: %w", err)
		}
	case "oauth_access_tokens":
		_, err := s.db.Exec(s.q(`
			INSERT INTO oauth_access_tokens (
				signature, request_id, requested_at, client_id,
				scopes, granted_scopes, request_form, session_data,
				audience, granted_audience, active, subject
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`), req.Signature, req.RequestID, requestedStr, req.ClientID,
			req.Scopes, req.GrantedScopes, req.RequestForm, req.SessionData,
			req.Audience, req.GrantedAudience, active, req.Subject)
		if err != nil {
			return fmt.Errorf("insert access token: %w", err)
		}
	case "oauth_refresh_tokens":
		var accessSig interface{}
		if req.AccessTokenSignature != "" {
			accessSig = req.AccessTokenSignature
		}
		_, err := s.db.Exec(s.q(`
			INSERT INTO oauth_refresh_tokens (
				signature, request_id, access_token_signature, requested_at,
				client_id, scopes, granted_scopes, request_form, session_data,
				audience, granted_audience, active, subject
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`), req.Signature, req.RequestID, accessSig, requestedStr,
			req.ClientID, req.Scopes, req.GrantedScopes, req.RequestForm, req.SessionData,
			req.Audience, req.GrantedAudience, active, req.Subject)
		if err != nil {
			return fmt.Errorf("insert refresh token: %w", err)
		}
	case "oauth_pkce_requests":
		// PKCE rows have no active column — fosite's lifecycle is
		// "exists or deleted", not "active or revoked".
		_, err := s.db.Exec(s.q(`
			INSERT INTO oauth_pkce_requests (
				signature, request_id, requested_at, client_id,
				scopes, granted_scopes, request_form, session_data,
				audience, granted_audience
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`), req.Signature, req.RequestID, requestedStr, req.ClientID,
			req.Scopes, req.GrantedScopes, req.RequestForm, req.SessionData,
			req.Audience, req.GrantedAudience)
		if err != nil {
			return fmt.Errorf("insert pkce request: %w", err)
		}
	default:
		return fmt.Errorf("oauth: unknown table %q", table)
	}
	return nil
}

// queryOAuthRequestRow reads a row by signature from the named
// table and returns (request, active, err).
//
//   - For oauth_pkce_requests, active is always true (no column).
//   - Returns ErrOAuthNotFound when the row is missing; callers map
//     that to fosite.ErrNotFound.
//   - Active=false rows are returned alongside no error; the caller
//     decides whether to treat that as ErrInvalidatedCode (auth
//     codes) or ErrInactiveToken (access/refresh).
func (s *Store) queryOAuthRequestRow(table, signature string) (*models.OAuthRequest, bool, error) {
	if signature == "" {
		return nil, false, fmt.Errorf("oauth: signature required")
	}

	var req models.OAuthRequest
	req.Signature = signature
	var requestedStr string

	switch table {
	case "oauth_authorization_codes":
		err := s.db.QueryRow(s.q(`
			SELECT request_id, requested_at, client_id, scopes, granted_scopes,
			       request_form, session_data, audience, granted_audience, active
			FROM oauth_authorization_codes WHERE signature = ?
		`), signature).Scan(&req.RequestID, &requestedStr, &req.ClientID,
			&req.Scopes, &req.GrantedScopes, &req.RequestForm, &req.SessionData,
			&req.Audience, &req.GrantedAudience, &req.Active)
		if err == sql.ErrNoRows {
			return nil, false, ErrOAuthNotFound
		}
		if err != nil {
			return nil, false, fmt.Errorf("query auth code: %w", err)
		}
	case "oauth_access_tokens":
		err := s.db.QueryRow(s.q(`
			SELECT request_id, requested_at, client_id, scopes, granted_scopes,
			       request_form, session_data, audience, granted_audience, active, subject
			FROM oauth_access_tokens WHERE signature = ?
		`), signature).Scan(&req.RequestID, &requestedStr, &req.ClientID,
			&req.Scopes, &req.GrantedScopes, &req.RequestForm, &req.SessionData,
			&req.Audience, &req.GrantedAudience, &req.Active, &req.Subject)
		if err == sql.ErrNoRows {
			return nil, false, ErrOAuthNotFound
		}
		if err != nil {
			return nil, false, fmt.Errorf("query access token: %w", err)
		}
	case "oauth_refresh_tokens":
		var accessSig sql.NullString
		err := s.db.QueryRow(s.q(`
			SELECT request_id, access_token_signature, requested_at, client_id,
			       scopes, granted_scopes, request_form, session_data,
			       audience, granted_audience, active, subject
			FROM oauth_refresh_tokens WHERE signature = ?
		`), signature).Scan(&req.RequestID, &accessSig, &requestedStr, &req.ClientID,
			&req.Scopes, &req.GrantedScopes, &req.RequestForm, &req.SessionData,
			&req.Audience, &req.GrantedAudience, &req.Active, &req.Subject)
		if err == sql.ErrNoRows {
			return nil, false, ErrOAuthNotFound
		}
		if err != nil {
			return nil, false, fmt.Errorf("query refresh token: %w", err)
		}
		req.AccessTokenSignature = accessSig.String
	case "oauth_pkce_requests":
		err := s.db.QueryRow(s.q(`
			SELECT request_id, requested_at, client_id, scopes, granted_scopes,
			       request_form, session_data, audience, granted_audience
			FROM oauth_pkce_requests WHERE signature = ?
		`), signature).Scan(&req.RequestID, &requestedStr, &req.ClientID,
			&req.Scopes, &req.GrantedScopes, &req.RequestForm, &req.SessionData,
			&req.Audience, &req.GrantedAudience)
		if err == sql.ErrNoRows {
			return nil, false, ErrOAuthNotFound
		}
		if err != nil {
			return nil, false, fmt.Errorf("query pkce: %w", err)
		}
		// PKCE rows are always "active" for the purposes of this method —
		// lifecycle is exists/deleted, not flagged.
		req.Active = true
	default:
		return nil, false, fmt.Errorf("oauth: unknown table %q", table)
	}

	req.RequestedAt = parseTime(requestedStr)
	return &req, req.Active, nil
}

// jsonStringList encodes a []string as a JSON array string suitable
// for both SQLite (TEXT column) and Postgres (JSONB column — pgx
// accepts a JSON-shaped string and validates it server-side). nil
// input encodes to "[]" so reads always parse cleanly.
func jsonStringList(in []string) (string, error) {
	if in == nil {
		in = []string{}
	}
	b, err := json.Marshal(in)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// parseJSONStringList decodes a JSON array string back to []string.
// Empty / null inputs produce an empty (non-nil) slice so callers
// can range without nil-checking.
func parseJSONStringList(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return []string{}, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = []string{}
	}
	return out, nil
}

// cloneStringSlice returns a copy of in so callers holding the
// returned model can't mutate the input the caller still owns.
// nil → nil so the model's slice fields stay falsy when the caller
// passed nil; sub-PR B's adapter relies on that.
func cloneStringSlice(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}
