package models

import "time"

// Models for the OAuth 2.1 authorization server (PLAN-943 TASK-951).
// These are pad-internal types used by internal/store/oauth.go to
// persist OAuth state. Sub-PR B (TASK-1024) introduces fosite, and
// sub-PR C wires fosite.Requester ⇄ OAuthRequest adapters in the
// HTTP handlers. The store layer never imports fosite so the schema
// boundary stays clean.

// OAuthClient is a registered OAuth client (RFC 7591 Dynamic Client
// Registration). Only public clients (no secret) are supported in
// v1 — see migrations/048_oauth.sql for why. The Public flag is a
// forward-compat toggle; future confidential-client support adds a
// secret column without changing this struct's shape.
//
// Field names use the RFC 7591 vocabulary so the JSON form can be
// served back to clients verbatim from the registration endpoint
// (sub-PR C).
type OAuthClient struct {
	ID                      string    `json:"client_id"`
	Name                    string    `json:"client_name"`
	RedirectURIs            []string  `json:"redirect_uris"`
	GrantTypes              []string  `json:"grant_types"`
	ResponseTypes           []string  `json:"response_types"`
	TokenEndpointAuthMethod string    `json:"token_endpoint_auth_method"`
	Scopes                  []string  `json:"scope"`
	Public                  bool      `json:"-"` // internal — never serialized
	LogoURL                 string    `json:"logo_uri,omitempty"`
	CreatedAt               time.Time `json:"client_id_issued_at"`
}

// OAuthRequest is the persisted form of a fosite.Requester (without
// the fosite import). Carries everything the auth-code, access-token,
// refresh-token, and PKCE storage rows need:
//
//   - Signature: the HMAC-derived lookup key (the row's primary key).
//   - RequestID: fosite's stable Requester.GetID() — preserved across
//     refresh-token rotations, so it doubles as the chain identifier
//     for theft-detection family revocation.
//   - RequestedAt: original grant timestamp; used by the cleaner.
//   - ClientID: FK to oauth_clients.
//   - Scopes / GrantedScopes: space-separated.
//   - RequestForm: URL-encoded form data from the original request
//     (PKCE handler reads code_challenge from here).
//   - SessionData: JSON-encoded session struct (subject + custom
//     claims). Sub-PR B defines pad's session type; the store layer
//     just round-trips it as bytes.
//   - Audience / GrantedAudience: space-separated RFC 8707 resource
//     indicators.
//   - Active: token-revocation toggle. RevokeRefreshToken /
//     RevokeAccessToken set this false; GetXxxSession returns
//     ErrInactiveToken (a sentinel defined in oauth.go) when active=false.
//   - Subject: denormalized from SessionData for fast subject-bound
//     queries (admin "list active tokens for user X" surfaces). Empty
//     for auth-code and PKCE rows where there is no subject yet.
//   - AccessTokenSignature: refresh-only; links the refresh row to
//     the access row issued in the same grant (or rotation step).
//     Empty for non-refresh rows.
type OAuthRequest struct {
	Signature            string
	RequestID            string
	RequestedAt          time.Time
	ClientID             string
	Scopes               string
	GrantedScopes        string
	RequestForm          string
	SessionData          string
	Audience             string
	GrantedAudience      string
	Active               bool
	Subject              string
	AccessTokenSignature string
}

// OAuthClientCreate is the input shape for the storage-level
// CreateClient method. The Postgres / SQLite migration already
// constrains required fields; this type lets callers set only the
// fields they have without populating the timestamp (the store sets
// CreatedAt from now()).
type OAuthClientCreate struct {
	Name                    string
	RedirectURIs            []string
	GrantTypes              []string
	ResponseTypes           []string
	TokenEndpointAuthMethod string
	Scopes                  []string
	Public                  bool
	LogoURL                 string
}
