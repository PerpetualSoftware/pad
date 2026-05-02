package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"

	"github.com/ory/fosite"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/oauth"
)

// OAuth 2.1 authorization-server HTTP handlers (PLAN-943 TASK-951).
// Sub-PR C built the /register, /authorize, /authorize/decide and
// /token endpoints + the populated discovery doc. Sub-PR D
// (TASK-1026) adds /revoke and /introspect on top, completing the
// RFC 7009 + RFC 7662 surface; sub-PR E wires MCPBearerAuth to
// OAuth introspection internally.
//
// Mounting:
//
// All routes are mounted top-level (alongside /mcp, /.well-known/*),
// outside the /api/v1 auth-required group, gated by cloud-mode + a
// non-nil oauthServer. CSRF middleware runs only on /api/* paths
// (middleware_csrf.go:36-39) so /oauth/* is naturally exempt; the
// consent-decision endpoint adds its own form-token check using the
// existing __Host-pad_csrf cookie because consent is the one POST
// here that rides a session cookie.
//
// Endpoints:
//
//   - POST /oauth/register — Dynamic Client Registration (RFC 7591).
//     Hand-written, no fosite. Public clients only.
//   - GET /oauth/authorize — start auth-code flow. Renders the
//     inline-HTML consent stub if the user is logged in;
//     otherwise 302 → /login?redirect=…
//   - POST /oauth/authorize/decide — process the consent decision.
//     Validates the form-bound CSRF token, runs fosite, redirects
//     to the client.
//   - POST /oauth/token — code exchange (PKCE + RFC 8707 verified
//     by fosite). Also handles refresh_token grant.
//   - POST /oauth/revoke — RFC 7009 token revocation. Authenticates
//     the public client by client_id, then walks the refresh-token
//     family (or single access token) to mark every chain member
//     inactive. Sub-PR D.
//   - POST /oauth/introspect — RFC 7662 token introspection.
//     Authenticates via Bearer header (an access token issued to
//     the same authorization server) — public clients only, so
//     Basic-auth is rejected by fosite as a side effect of our
//     token_endpoint_auth_methods=none policy. Sub-PR D.
//
// All six go via Server.oauthServer (set by SetOAuthServer at
// startup). When that's nil the routes don't mount — see
// registerOAuthRoutes.

// SetOAuthServer wires the OAuth 2.1 authorization server (built
// in cmd/pad/main.go via internal/oauth.NewServer) into the route
// table. Called once at startup, before the first request hits the
// router. nil disables the OAuth surface entirely.
//
// Like SetMCPTransport, this MUST be called before setupRouter
// runs (which it does on first request). Calling it later is a
// no-op because chi routes are immutable post-mount.
func (s *Server) SetOAuthServer(srv *oauth.Server) {
	s.oauthServer = srv
}

// registerOAuthRoutes mounts the OAuth endpoints on r. Called from
// setupRouter at the same level as the MCP routes. No-op when
// either cloud mode is off or SetOAuthServer was never called —
// keeps self-hosted deployments free of OAuth-server surface they
// can't use anyway (no canonical audience, no DCR clients).
//
// The discovery document at /.well-known/oauth-authorization-server
// continues to be served by registerMCPRoutes — it lives there
// because it was the 501 stub from TASK-950, and replacing it in
// place keeps the URL stable for clients that already discovered
// the chain. The OAuth-server routes added here are the four flow
// endpoints; metadata + protected-resource doc are mounted earlier.
func (s *Server) registerOAuthRoutes(r interface {
	Get(pattern string, h http.HandlerFunc)
	Post(pattern string, h http.HandlerFunc)
}) {
	if s.oauthServer == nil || !s.IsCloud() {
		return
	}
	r.Post("/oauth/register", s.handleOAuthRegister)
	r.Get("/oauth/authorize", s.handleOAuthAuthorize)
	r.Post("/oauth/authorize/decide", s.handleOAuthAuthorizeDecide)
	r.Post("/oauth/token", s.handleOAuthToken)
	r.Post("/oauth/revoke", s.handleOAuthRevoke)
	r.Post("/oauth/introspect", s.handleOAuthIntrospect)
}

// =====================================================================
// /oauth/register — Dynamic Client Registration (RFC 7591)
// =====================================================================

// dcrRequest is the wire shape DCR clients post. Only the fields
// pad cares about are declared; RFC 7591 §2 lists many more
// (logo_uri, client_uri, contacts, tos_uri, policy_uri, …) but
// they're either advisory display metadata or related to flows we
// don't run. Accept and ignore unknown fields rather than rejecting
// — RFC 7591 §3.2 explicitly says servers SHOULD allow extra fields.
type dcrRequest struct {
	ClientName              string   `json:"client_name"`
	RedirectURIs            []string `json:"redirect_uris"`
	GrantTypes              []string `json:"grant_types,omitempty"`
	ResponseTypes           []string `json:"response_types,omitempty"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method,omitempty"`
	Scopes                  string   `json:"scope,omitempty"` // RFC 7591 §2: space-delimited
	LogoURI                 string   `json:"logo_uri,omitempty"`
}

// dcrResponse is the wire shape returned to a successful register.
// RFC 7591 §3.2 mandates client_id + client_id_issued_at; we echo
// back the validated metadata so clients can verify it matches
// what they sent.
type dcrResponse struct {
	ClientID                string   `json:"client_id"`
	ClientIDIssuedAt        int64    `json:"client_id_issued_at"`
	ClientName              string   `json:"client_name,omitempty"`
	RedirectURIs            []string `json:"redirect_uris"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	Scope                   string   `json:"scope,omitempty"`
	LogoURI                 string   `json:"logo_uri,omitempty"`
}

// dcrError matches RFC 7591 §3.2.2's error response shape.
type dcrError struct {
	Code    string `json:"error"`
	Message string `json:"error_description,omitempty"`
}

// handleOAuthRegister implements RFC 7591 Dynamic Client Registration.
// Public clients only — no client_secret is generated or returned.
// PKCE is the only authentication path (compose pattern in
// internal/oauth/server.go enforces it).
//
// fosite has no built-in DCR endpoint, so this is hand-written.
// Validation rules:
//
//   - redirect_uris MUST be non-empty (RFC 7591 §2 + OAuth 2.1's
//     exact-match requirement).
//   - Each redirect_uri MUST be an absolute URI without a fragment.
//   - grant_types defaults to ["authorization_code", "refresh_token"]
//     if absent. Only these two are accepted; any other rejected.
//   - response_types defaults to ["code"]; only "code" accepted.
//   - token_endpoint_auth_method defaults to "none" (public client).
//     "client_secret_basic" / "client_secret_post" are rejected
//     because we don't issue secrets.
//   - scope: tokens are space-delimited per RFC 6749 §3.3 and the
//     allowed set is pad:read / pad:write / pad:admin (TASK-953
//     adds the workspace allow-list scopes).
func (s *Server) handleOAuthRegister(w http.ResponseWriter, r *http.Request) {
	if !s.IsCloud() || s.oauthServer == nil {
		http.NotFound(w, r)
		return
	}

	var input dcrRequest
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeDCRError(w, http.StatusBadRequest, "invalid_client_metadata",
			"Request body must be JSON: "+err.Error())
		return
	}

	if len(input.RedirectURIs) == 0 {
		writeDCRError(w, http.StatusBadRequest, "invalid_redirect_uri",
			"redirect_uris is required and must be non-empty")
		return
	}
	for _, raw := range input.RedirectURIs {
		if err := validateRegisterRedirectURI(raw); err != nil {
			writeDCRError(w, http.StatusBadRequest, "invalid_redirect_uri", err.Error())
			return
		}
	}

	grants := input.GrantTypes
	if len(grants) == 0 {
		grants = []string{"authorization_code", "refresh_token"}
	}
	for _, g := range grants {
		if g != "authorization_code" && g != "refresh_token" {
			writeDCRError(w, http.StatusBadRequest, "invalid_client_metadata",
				"unsupported grant_type: "+g)
			return
		}
	}

	responseTypes := input.ResponseTypes
	if len(responseTypes) == 0 {
		responseTypes = []string{"code"}
	}
	for _, rt := range responseTypes {
		if rt != "code" {
			writeDCRError(w, http.StatusBadRequest, "invalid_client_metadata",
				"unsupported response_type: "+rt)
			return
		}
	}

	authMethod := input.TokenEndpointAuthMethod
	if authMethod == "" {
		authMethod = "none"
	}
	if authMethod != "none" {
		writeDCRError(w, http.StatusBadRequest, "invalid_client_metadata",
			"only token_endpoint_auth_method=none is supported (public clients only)")
		return
	}

	scopes := splitScopeString(input.Scopes)
	if len(scopes) == 0 {
		scopes = []string{"pad:read", "pad:write"}
	}
	for _, sc := range scopes {
		if !isAllowedRegisterScope(sc) {
			writeDCRError(w, http.StatusBadRequest, "invalid_client_metadata",
				"scope not allowed: "+sc)
			return
		}
	}

	created, err := s.store.CreateOAuthClient(models.OAuthClientCreate{
		Name:                    input.ClientName,
		RedirectURIs:            input.RedirectURIs,
		GrantTypes:              grants,
		ResponseTypes:           responseTypes,
		TokenEndpointAuthMethod: authMethod,
		Scopes:                  scopes,
		Public:                  true,
		LogoURL:                 input.LogoURI,
	})
	if err != nil {
		writeInternalError(w, fmt.Errorf("oauth: create client: %w", err))
		return
	}

	resp := dcrResponse{
		ClientID:                created.ID,
		ClientIDIssuedAt:        created.CreatedAt.Unix(),
		ClientName:              created.Name,
		RedirectURIs:            created.RedirectURIs,
		GrantTypes:              created.GrantTypes,
		ResponseTypes:           created.ResponseTypes,
		TokenEndpointAuthMethod: created.TokenEndpointAuthMethod,
		Scope:                   strings.Join(created.Scopes, " "),
		LogoURI:                 created.LogoURL,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(resp)
}

// validateRegisterRedirectURI enforces OAuth 2.1's exact-match
// requirement at registration time (matching is at /authorize time;
// here we just verify the URI is well-formed). Reject:
//
//   - relative URIs (no scheme)
//   - URIs with a fragment (#) — OAuth 2.1 §2.3.3 disallows
//   - http:// URIs not pointing at localhost / 127.0.0.1 — public
//     deployments can't use plain HTTP, and an attacker registering
//     http://attacker.com would intercept codes
func validateRegisterRedirectURI(raw string) error {
	if raw == "" {
		return errors.New("redirect_uri must be non-empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("redirect_uri parse: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return errors.New("redirect_uri must be an absolute URI")
	}
	if u.Fragment != "" {
		return errors.New("redirect_uri must not contain a fragment")
	}
	if u.Scheme == "http" {
		host := u.Hostname()
		if host != "localhost" && host != "127.0.0.1" && host != "::1" {
			return errors.New("redirect_uri must use https for non-loopback hosts")
		}
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		// Allow custom-scheme URIs (e.g. claude://oauth/callback)
		// because Anthropic's connector flow uses them. Block only
		// the "obviously wrong" schemes (file:, javascript:, data:).
		switch u.Scheme {
		case "file", "javascript", "data", "vbscript":
			return errors.New("redirect_uri scheme not permitted: " + u.Scheme)
		}
	}
	return nil
}

// isAllowedRegisterScope is the v1 allow-list. TASK-953 widens
// this to include workspace-scoped scopes (pad:workspaces:slug,…).
func isAllowedRegisterScope(s string) bool {
	switch s {
	case "pad:read", "pad:write", "pad:admin":
		return true
	}
	return false
}

// splitScopeString parses RFC 6749 §3.3 space-delimited scope.
// Single space is the canonical separator; multi-spaces from
// hand-typed input are tolerated.
func splitScopeString(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	out := []string{}
	for _, p := range strings.Split(raw, " ") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// writeDCRError emits the RFC 7591 §3.2.2 error response shape.
func writeDCRError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(dcrError{Code: code, Message: msg})
}

// =====================================================================
// /oauth/authorize — start auth-code flow
// =====================================================================

// translateResourceToAudience copies any RFC 8707 `resource` form
// parameters into fosite's non-standard `audience` parameter slot,
// so the request fosite parses sees the audience it expects.
//
// The two are semantically equivalent: RFC 8707 §2 calls them
// "resource indicators" and labels them "audience hints"; fosite
// (v0.49.0) reads `audience` only. Without translation, real RFC
// 8707 clients (Claude Desktop, Cursor, ChatGPT) sending only
// `resource=https://mcp.getpad.dev/mcp` would hit our custom
// audienceMatchingStrategy with an empty needle and fail every
// authorize / token request. Codex review #372 round 1.
//
// Mutates r.Form in place. Idempotent — if `audience` is already
// set we leave it (the test suite uses both keys as belt-and-
// suspenders; production clients would send only one). r MUST
// have ParseForm called before this; the OAuth handlers below
// parse before invoking fosite.
//
// Both keys land in the form because fosite preserves r.Form into
// the AuthorizeRequester, and the storage layer round-trips the
// form on token exchange — so the translated audience is what
// gets persisted + compared on the /token side too.
func translateResourceToAudience(r *http.Request) {
	if r == nil || r.Form == nil {
		return
	}
	resources := r.Form["resource"]
	if len(resources) == 0 {
		return
	}
	if existing := r.Form["audience"]; len(existing) > 0 {
		// Caller passed both — defer to audience as canonical and
		// leave it untouched. Audit-log noise about ambiguity isn't
		// worth the complexity for the v1 stub; if this becomes a
		// real source of confusion we can warn here later.
		return
	}
	r.Form["audience"] = append([]string(nil), resources...)
}

// handleOAuthAuthorize is the entry point for the authorization-code
// flow. fosite validates the request shape; if the user is logged
// in we render the inline consent stub (TODO TASK-952), otherwise we
// 302-redirect to the login page with a `redirect=` param so the
// post-login flow returns here (TASK-998 plumbed the pad-cloud
// callback to honor it).
//
// Behavior on errors:
//
//   - fosite-validation errors → fosite's WriteAuthorizeError
//     produces a redirect to the client's redirect_uri with the
//     OAuth error params.
//   - missing-user (no session): 302 → /login?redirect=<self>.
//     The current request's full URL (path + query) is
//     URL-encoded into the redirect param.
//   - canonical-audience mismatch: surfaced via fosite's
//     audienceMatchingStrategy → invalid_request.
func (s *Server) handleOAuthAuthorize(w http.ResponseWriter, r *http.Request) {
	if !s.IsCloud() || s.oauthServer == nil {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()

	// fosite reads only `audience` from the form; RFC 8707 mandates
	// `resource`. Translate before fosite parses (Codex review #372
	// round 1). ParseForm has to run first so r.Form exists.
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid query string", http.StatusBadRequest)
		return
	}
	translateResourceToAudience(r)

	ar, err := s.oauthServer.Provider().NewAuthorizeRequest(ctx, r)
	if err != nil {
		s.oauthServer.Provider().WriteAuthorizeError(ctx, w, ar, err)
		return
	}

	// User must be logged in to grant consent. SessionAuth runs
	// in the parent middleware chain and falls through gracefully
	// when no cookie is present — at this point currentUser(r)
	// returns the resolved user or nil.
	user := currentUser(r)
	if user == nil {
		// 302 → /login?redirect=/oauth/authorize?<original-query>.
		// The login page (web/src/routes/login/+page.svelte) +
		// pad-cloud's OAuth callback (TASK-998) honor `redirect=`
		// for relative paths.
		dest := "/oauth/authorize"
		if r.URL.RawQuery != "" {
			dest += "?" + r.URL.RawQuery
		}
		loginURL := "/login?redirect=" + url.QueryEscape(dest)
		http.Redirect(w, r, loginURL, http.StatusFound)
		return
	}

	// Render the consent UI (TASK-952). Loads the user's workspaces
	// + filters tier radios to the intersection of {pad:read,
	// pad:write, pad:admin} and the client's requested scopes.
	s.renderConsent(w, r, ar, user)
}

// =====================================================================
// /oauth/authorize/decide — process consent decision
// =====================================================================

// handleOAuthAuthorizeDecide processes the consent decision posted
// from the consent UI. Validates the form-bound CSRF token, parses
// the user's tier + workspace allow-list selections, then either
// runs fosite's authorize-response flow (approve) or writes an
// access_denied redirect (deny).
//
// The decision endpoint is what an attacker would target via a
// cross-origin POST trying to silently grant consent on a victim's
// behalf. The CSRF token check binds the POST to the original
// browser context — same double-submit pattern the SPA uses, with
// the token in a form field rather than a header.
//
// Approve flow:
//
//  1. Parse capability_tier ∈ {read, write, admin}; reject anything
//     else.
//  2. Parse allowed_workspaces (multi-value form field). At least
//     one entry required; "*" is the wildcard. Each non-wildcard
//     value must be a workspace the user is currently a member of
//     (defense in depth — the form was rendered with their
//     memberships, but a tampered POST could send other slugs).
//  3. Grant exactly the chosen tier scope (pad:read / pad:write /
//     pad:admin), NOT every requested scope. Selective consent is
//     the whole point of TASK-952 — granting more than the user
//     picked would defeat the UI.
//  4. Stash the workspace allow-list in session.Extra. TASK-953
//     reads it at /mcp introspection time + does the live role
//     lookup against the membership table. Storing it in Extra
//     rather than as scope strings sidesteps fosite's strict
//     "granted ⊆ requested ⊆ client.Scopes" subset check; clients
//     don't request `pad:workspaces:foo`, but the consent UI lets
//     the user pick from their workspaces regardless.
//  5. NewAuthorizeResponse persists the request + session and
//     redirects to the client's redirect_uri with the auth code.
func (s *Server) handleOAuthAuthorizeDecide(w http.ResponseWriter, r *http.Request) {
	if !s.IsCloud() || s.oauthServer == nil {
		http.NotFound(w, r)
		return
	}

	user := currentUser(r)
	if user == nil {
		// Session expired between consent render + decision POST.
		// Fall back to login redirect with the decision page's URL
		// — when the user logs back in they'll re-render the consent
		// (whose underlying request will be carried via the form
		// fields they're about to submit).
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form body", http.StatusBadRequest)
		return
	}

	// CSRF: form field must match the __Host-pad_csrf cookie. Same
	// double-submit pattern the API uses, but with the token in a
	// hidden form input rather than a header (consent is server-
	// rendered HTML, not SPA fetch).
	if err := s.validateConsentCSRFToken(r); err != nil {
		writeError(w, http.StatusForbidden, "csrf_error", err.Error())
		return
	}

	// RFC 8707 resource= → fosite audience= (Codex #372 round 1).
	translateResourceToAudience(r)

	ctx := r.Context()
	ar, err := s.oauthServer.Provider().NewAuthorizeRequest(ctx, r)
	if err != nil {
		s.oauthServer.Provider().WriteAuthorizeError(ctx, w, ar, err)
		return
	}

	decision := r.FormValue("decision")
	if decision == "deny" {
		s.oauthServer.Provider().WriteAuthorizeError(ctx, w, ar,
			fosite.ErrAccessDenied.WithHint("The user denied the consent."))
		return
	}
	if decision != "approve" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"decision must be 'approve' or 'deny'")
		return
	}

	// Parse + validate the consent payload.
	tierScope, allowedWorkspaces, vErr := s.parseConsentPayload(r, ar, user)
	if vErr != nil {
		// Validation failures are user-fixable (re-render with an
		// error message would be friendlier, but a 400 + JSON envelope
		// matches the rest of pad's API surface and the consent flow
		// is rarely re-driven manually). The OAuth client will see the
		// 400, NOT a redirect to redirect_uri, because the user never
		// reached the "real" /authorize/decide → fosite flow.
		writeError(w, http.StatusBadRequest, "invalid_request", vErr.Error())
		return
	}

	// Grant exactly the chosen tier — selective consent. No loop over
	// GetRequestedScopes; that would re-grant every scope the client
	// asked for, defeating the user's tier choice.
	ar.GrantScope(tierScope)
	for _, aud := range ar.GetRequestedAudience() {
		ar.GrantAudience(aud)
	}

	// Build the session that fosite serializes alongside the code.
	// Subject = pad user ID (used by MCPBearerAuth's introspection
	// branch to resolve the bearer to a user). Extra carries the
	// workspace allow-list; TASK-953 reads it at /mcp time + does
	// the live role lookup. Round-trips via storage.go's JSON marshal.
	session := oauth.NewSession(user.ID)
	session.SetAllowedWorkspaces(allowedWorkspaces)

	resp, err := s.oauthServer.Provider().NewAuthorizeResponse(ctx, ar, session)
	if err != nil {
		s.oauthServer.Provider().WriteAuthorizeError(ctx, w, ar, err)
		return
	}

	s.oauthServer.Provider().WriteAuthorizeResponse(ctx, w, ar, resp)
}

// parseConsentPayload extracts and validates the user's tier choice
// + workspace allow-list from the consent form. Returns the chosen
// tier as a "pad:<tier>" scope string ready to GrantScope, and the
// allow-list as either ["*"] (wildcard) or a list of slugs.
//
// Validation rules:
//
//   - capability_tier MUST be one of {read, write, admin}.
//   - capability_tier MUST also be in the client's requested scopes
//     (fosite's grant-time check would reject otherwise — fail fast
//     here with a cleaner error).
//   - allowed_workspaces MUST be non-empty (after coalescing).
//   - If "*" appears, the result is exactly ["*"] regardless of any
//     other slugs sent (matches the UI's mutual-exclusion behaviour
//   - defends against tampered forms that send both).
//   - Each non-wildcard slug MUST be a workspace the user is
//     currently a member of (defense in depth).
//
// Returns wrapped errors with WithMessage strings safe to surface to
// the user.
func (s *Server) parseConsentPayload(r *http.Request, ar fosite.AuthorizeRequester, user *models.User) (tierScope string, allowedWorkspaces []string, err error) {
	tier := r.FormValue("capability_tier")
	switch tier {
	case "read", "write", "admin":
		// ok
	default:
		return "", nil, errors.New("capability_tier must be 'read', 'write', or 'admin'")
	}
	tierScope = "pad:" + tier

	// Mirror the consent UI's tier-radio constraint server-side. The
	// UI hides radios for tiers the client didn't request, but a
	// tampered form could send any value — fosite's grant-time check
	// would catch this with a less-obvious error, so we reject early.
	requestedTier := false
	for _, sc := range ar.GetRequestedScopes() {
		if sc == tierScope {
			requestedTier = true
			break
		}
	}
	if !requestedTier {
		return "", nil, fmt.Errorf("capability_tier %q is not among the scopes the client requested", tier)
	}

	raw := r.PostForm["allowed_workspaces"]
	if len(raw) == 0 {
		return "", nil, errors.New("at least one workspace must be selected")
	}

	// Wildcard wins if present — the UI enforces mutual exclusion,
	// and a tampered form sending both should still resolve to the
	// safer wildcard interpretation (rather than partial allow-list).
	for _, w := range raw {
		if w == "*" {
			return tierScope, []string{"*"}, nil
		}
	}

	// Validate every slug against the user's current memberships.
	// Tampered submissions land here.
	memberships, mErr := s.store.GetUserWorkspaceMemberships(user.ID)
	if mErr != nil {
		return "", nil, fmt.Errorf("load memberships: %w", mErr)
	}
	memberSet := make(map[string]struct{}, len(memberships))
	for _, m := range memberships {
		memberSet[m.WorkspaceSlug] = struct{}{}
	}
	out := make([]string, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for _, slug := range raw {
		if _, ok := memberSet[slug]; !ok {
			return "", nil, fmt.Errorf("you are not a member of workspace %q", slug)
		}
		if _, dup := seen[slug]; dup {
			continue // de-dupe; defensive against form tampering
		}
		seen[slug] = struct{}{}
		out = append(out, slug)
	}
	return tierScope, out, nil
}

// =====================================================================
// /oauth/token — code exchange + refresh-token rotation
// =====================================================================

// handleOAuthToken handles the token endpoint for both
// authorization_code and refresh_token grant types. fosite's
// NewAccessRequest does the heavy lifting: validates client_id,
// looks up the auth code (or refresh token), verifies the PKCE
// code_verifier (S256-required by Config.EnforcePKCE +
// EnablePKCEPlainChallengeMethod=false in NewServer), confirms the
// audience matches (custom strategy from audience.go).
//
// Refresh-token rotation is implicit: fosite's flow_refresh.go
// calls Storage.RotateRefreshToken before issuing the new pair,
// which under our adapter revokes the entire grant family
// (matches fosite's reference MemoryStore behaviour, locked in
// by sub-PR A round 2 and sub-PR B round 2 of Codex review).
//
// On error fosite's WriteAccessError writes the JSON OAuth error
// body; on success WriteAccessResponse writes
// {access_token, token_type, expires_in, refresh_token, scope}.
func (s *Server) handleOAuthToken(w http.ResponseWriter, r *http.Request) {
	if !s.IsCloud() || s.oauthServer == nil {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()

	// RFC 8707 resource= → fosite audience= before fosite parses.
	// Codex review #372 round 1 caught this — without translation
	// real RFC 8707 token-exchange requests fail audience matching.
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form body", http.StatusBadRequest)
		return
	}
	translateResourceToAudience(r)

	// Empty session for fosite to populate from storage. The auth
	// code's stored session_data carries the user's Subject; fosite
	// hydrates this via Storage.GetAuthorizationCodeSession during
	// /token exchange.
	session := oauth.NewSession("")

	ar, err := s.oauthServer.Provider().NewAccessRequest(ctx, r, session)
	if err != nil {
		s.oauthServer.Provider().WriteAccessError(ctx, w, ar, err)
		return
	}

	// fosite's auth-code + refresh-token handlers
	// (handler/oauth2/flow_authorize_code_token.go:134-138 +
	// flow_refresh.go:91-103) copy GrantedScope + GrantedAudience
	// from the persisted upstream request to the new access request
	// automatically. We deliberately do NOT loop over RequestedScope
	// here — that would expand the granted set on every /token
	// exchange to include scopes the user explicitly chose NOT to
	// grant at consent time (TASK-952's selective-consent rule).
	// The earlier auto-approve stub coincidentally got away with the
	// loop because granted == requested for every grant; the new
	// consent UI breaks that equivalence.

	resp, err := s.oauthServer.Provider().NewAccessResponse(ctx, ar)
	if err != nil {
		s.oauthServer.Provider().WriteAccessError(ctx, w, ar, err)
		return
	}

	s.oauthServer.Provider().WriteAccessResponse(ctx, w, ar, resp)
}

// =====================================================================
// /oauth/revoke — RFC 7009 token revocation
// =====================================================================

// handleOAuthRevoke implements RFC 7009 token revocation. Public
// clients authenticate by sending only `client_id` in the form body
// (fosite's AuthenticateClient accepts that for clients with
// token_endpoint_auth_method=none, which is every client we
// register).
//
// Why this is interesting:
//
// fosite's revocation handler (handler/oauth2/revocation.go) calls
// our adapter's TokenRevocationStorage methods (RevokeRefreshToken /
// RevokeAccessToken) keyed by request_id — the chain identifier sub-
// PR A wires through every persisted row in a grant. Both methods
// delegate to store.RevokeRefreshTokenFamily / RevokeAccessTokenFamily,
// which walk the request_id index and mark every row inactive in a
// single statement. So revoking a single refresh token kills the
// whole grant — every paired access token, every rotated refresh
// — in one shot. Matches OAuth 2.1 BCP §4.14's "revoke the family"
// rule.
//
// Wire shape:
//
//   - 200 OK on success or unknown token (RFC 7009 §2.2: "invalid
//     tokens do not cause an error response since the client cannot
//     handle such an error in a reasonable way"; revocation is
//     idempotent from the client's POV).
//   - 400 invalid_request on malformed input (missing token, wrong
//     HTTP method, unparseable body).
//   - 401 invalid_client on client-auth failure.
//
// Response body is empty for the 200 success path; the OAuth error
// envelope for the others.
//
// fosite v0.49 already handles RFC 7009 §2.2 idempotency natively:
// handler/oauth2/revocation.go's RevokeToken returns nil for
// unknown / already-revoked tokens via storeErrorsToRevocationError
// (which collapses ErrNotFound + ErrInactiveToken to nil). So
// NewRevocationRequest returns nil and WriteRevocationResponse
// writes 200. The only spec gap we patch is missing-token: RFC
// 7009 §2.1 marks `token` REQUIRED, but fosite doesn't enforce that
// — it passes the empty string into the revocation handlers and
// returns nil (because both signatures look up empty and miss).
// Without the pre-check below, "client_id but no token" would
// silently 200 instead of 400. Codex review #373 round 3.
func (s *Server) handleOAuthRevoke(w http.ResponseWriter, r *http.Request) {
	if !s.IsCloud() || s.oauthServer == nil {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()

	// Pre-check: RFC 7009 §2.1 marks `token` REQUIRED. Parse the
	// form ourselves (fosite would do it inside NewRevocationRequest
	// anyway) so we can fail-fast with a clean 400 + spec-shaped
	// error envelope instead of fosite's silent 200-on-empty-token
	// behavior. ParseForm on a x-www-form-urlencoded body populates
	// r.PostForm; multipart bodies are extremely rare for OAuth
	// endpoints, and ParseForm refuses to handle them — for those
	// we let fosite handle the rejection downstream.
	if err := r.ParseForm(); err == nil && r.PostForm.Get("token") == "" {
		s.oauthServer.Provider().WriteRevocationResponse(ctx, w,
			fosite.ErrInvalidRequest.WithHint("The `token` parameter is required (RFC 7009 §2.1)."))
		return
	}

	err := s.oauthServer.Provider().NewRevocationRequest(ctx, r)
	s.oauthServer.Provider().WriteRevocationResponse(ctx, w, err)
}

// =====================================================================
// /oauth/introspect — RFC 7662 token introspection
// =====================================================================

// handleOAuthIntrospect implements RFC 7662 token introspection.
// Returns one of two JSON shapes:
//
//   - {"active": true, "client_id": ..., "scope": ..., "sub": ...,
//     "aud": [...], "exp": ..., "iat": ...}  — for active tokens.
//   - {"active": false}  — for any other case (unknown, expired,
//     revoked, or the caller isn't allowed to know about this
//     token). RFC 7662 §2.2 explicitly mandates the empty-body
//     shape on the inactive path so an attacker can't enumerate
//     token state from the response shape.
//
// Authentication (RFC 7662 §2.1: "the endpoint MUST also require
// some form of authorization to access this endpoint"):
//
// fosite's NewIntrospectionRequest accepts either Basic auth
// (client_id + client_secret) or Bearer auth (a separate access
// token). Pad's clients are public-only — no secrets — so the
// Basic-auth branch will always fail; callers MUST send Bearer
// auth using a different access token they hold. fosite checks the
// bearer is itself active and not the same token being introspected
// (avoid trivial "introspect yourself with yourself" auth).
//
// In practice the consumer of this endpoint is sub-PR E's
// MCPBearerAuth — but that integration uses fosite.IntrospectToken
// directly (server-side, no HTTP roundtrip), so this public
// endpoint primarily satisfies the RFC 7662 + RFC 8414 contract
// for clients that follow the discovery chain. Internal call sites
// stay efficient.
//
// Why a fresh empty oauth.Session: fosite's IntrospectToken hydrates
// the session from the stored token via Storage.GetAccessTokenSession
// / GetRefreshTokenSession — we just need a typed session pointer
// to receive the JSON unmarshal. Using oauth.NewSession("") matches
// the pattern in /oauth/token.
func (s *Server) handleOAuthIntrospect(w http.ResponseWriter, r *http.Request) {
	if !s.IsCloud() || s.oauthServer == nil {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()

	// Empty session pointer — fosite hydrates from storage. The
	// session type must match what's stored (oauth.Session, written
	// via oauth.NewSession in /authorize/decide), or the JSON
	// unmarshal in oauthRequestToFositeRequest would silently drop
	// pad-specific fields.
	session := oauth.NewSession("")

	ir, err := s.oauthServer.Provider().NewIntrospectionRequest(ctx, r, session)
	if err != nil {
		// fosite's WriteIntrospectionError handles the RFC 7662 §2.2
		// distinction: for ErrInactiveToken / general validation
		// failures it emits 200 + {active: false}; for auth failures
		// (bad bearer, missing auth header) it emits 401 with the
		// OAuth error envelope. Don't second-guess that — the spec
		// is finicky about which failure category gets which response.
		s.oauthServer.Provider().WriteIntrospectionError(ctx, w, err)
		return
	}
	s.oauthServer.Provider().WriteIntrospectionResponse(ctx, w, ir)
}

// =====================================================================
// Consent UI (TASK-952)
// =====================================================================

// consentTmpl is the server-rendered consent page that drives /oauth/authorize.
//
// Why server-rendered HTML (not a SvelteKit page):
//
//   - Security-critical: this is where a user grants long-lived bearer
//     tokens to a third-party application. A purely-static HTML page
//     with no client-side hydration has a much smaller attack surface
//     than a SPA route that loads bundled JavaScript, runtime hooks,
//     and an API client.
//   - Pre-form-submit failure modes are simpler: if the user has
//     JavaScript disabled or it fails to load, the form still works
//     (the JS below is purely UX polish, not a security gate). The
//     security-relevant validations all run on the server in
//     handleOAuthAuthorizeDecide.
//   - Performance: zero hydration latency on a one-shot page that's
//     never revisited.
//
// What's on the page:
//
//   - Client name + logo (from /oauth/register's metadata).
//   - Logged-in user's email/name as a "you are signed in as X" line.
//   - Workspace multi-select: every workspace the user is a member of,
//     with their role shown next to each row.
//   - "Any workspace I currently or later have access to" wildcard
//     checkbox (mutually exclusive with the per-workspace boxes).
//   - Capability-tier radio: read / write / admin. Constrained to
//     the intersection of {pad:read, pad:write, pad:admin} and the
//     client's requested scopes — fosite's grant-time check rejects
//     scopes not in the request, so only-requested tiers can be
//     granted.
//   - Cancel + Allow buttons. Allow disabled until at least one
//     workspace (or the wildcard) is checked.
//
// Form action posts to /oauth/authorize/decide with a hidden
// __Host-pad_csrf token + every original /authorize query param
// preserved. handleOAuthAuthorizeDecide rebuilds the AuthorizeRequest
// from those hidden fields, then layers the user's tier + workspace
// selections on top.
//
// CSRF: the form-bound hex token is rendered escaped (default html/
// template behaviour is fine — generateCSRFToken outputs hex which
// is safe in HTML attributes). validateConsentCSRFToken does the
// constant-time comparison server-side.
//
// JS gracefully degrades:
//
//   - When wildcard is checked, per-workspace checkboxes are disabled.
//   - When any per-workspace checkbox is checked, wildcard is
//     unchecked.
//   - Allow button stays disabled until ≥1 workspace OR wildcard
//     selected. Server-side validation in handleOAuthAuthorizeDecide
//     repeats these checks (the JS is convenience only).
var consentTmpl = template.Must(template.New("consent").Parse(`<!doctype html>
<html lang="en"><head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Authorize {{.ClientName}} · Pad</title>
<style>
:root { color-scheme: light dark; }
* { box-sizing: border-box; }
body {
  font: 15px/1.5 -apple-system, BlinkMacSystemFont, system-ui, sans-serif;
  max-width: 540px; margin: 2em auto; padding: 0 1em; color: #1a1a1a;
}
@media (prefers-color-scheme: dark) {
  body { color: #e8e8e8; background: #1a1a1a; }
  fieldset, .client-card { border-color: #444; background: #222; }
  .role { color: #aaa; }
  .hint, .footer { color: #999; }
  button { background: #333; color: #e8e8e8; border-color: #555; }
  button.primary { background: #2563eb; color: #fff; border-color: #2563eb; }
  a { color: #6ea8fe; }
}
h1 { font-size: 1.5em; margin: 0 0 .25em; }
.client-card {
  display: flex; align-items: center; gap: 1em; margin: 1.5em 0;
  padding: 1em; border: 1px solid #ddd; border-radius: 10px; background: #fafafa;
}
.client-card img.logo {
  width: 56px; height: 56px; border-radius: 10px;
  object-fit: cover; background: #fff;
}
.client-card .meta strong { font-size: 1.05em; }
.client-card .meta small { display: block; color: #666; margin-top: .15em; }
fieldset {
  border: 1px solid #ddd; border-radius: 10px; padding: 1em 1.25em;
  margin: 1.25em 0;
}
legend { font-weight: 600; padding: 0 .4em; }
.ws-row, .tier-row {
  display: flex; align-items: baseline; gap: .6em;
  padding: .35em 0; cursor: pointer;
}
.ws-row input, .tier-row input { flex: 0 0 auto; margin-top: 2px; }
.ws-name { flex: 1 1 auto; }
.role { color: #666; font-size: .9em; }
.tier-row .desc { color: #555; font-size: .92em; margin-left: .4em; }
.wildcard-warning {
  display: none; margin: .5em 0 0; padding: .6em .8em;
  background: #fff3cd; border-left: 3px solid #f0c000;
  font-size: .9em; border-radius: 4px;
}
.wildcard-warning.show { display: block; }
@media (prefers-color-scheme: dark) {
  .wildcard-warning { background: #3a2f00; border-left-color: #d4a400; color: #f0d873; }
}
.hint { color: #555; font-size: .9em; margin: .75em 0; }
.actions { margin-top: 2em; display: flex; gap: .75em; justify-content: flex-end; }
button {
  padding: .65em 1.4em; border-radius: 8px; border: 1px solid #ccc;
  font-size: 1em; font-weight: 500; cursor: pointer; background: #f5f5f5;
}
button.primary { background: #2563eb; color: #fff; border-color: #2563eb; }
button:disabled { opacity: 0.5; cursor: not-allowed; }
button.primary:hover:not(:disabled) { background: #1e54d4; }
.footer { margin-top: 2em; font-size: .85em; color: #888; }
.footer a { color: inherit; }
.no-ws { color: #b00; }
</style>
</head>
<body>
<h1>Authorize {{.ClientName}}</h1>
<p>Signed in as <strong>{{.Username}}</strong>.</p>

<div class="client-card">
  {{if .ClientLogoURL}}<img class="logo" src="{{.ClientLogoURL}}" alt="">{{end}}
  <div class="meta">
    <strong>{{.ClientName}}</strong>
    <small>is requesting access to your Pad workspaces.</small>
  </div>
</div>

<form method="POST" action="/oauth/authorize/decide" id="consent-form">
  <input type="hidden" name="csrf_token" value="{{.CSRF}}">
  {{range $k, $vs := .HiddenFields}}{{range $vs}}<input type="hidden" name="{{$k}}" value="{{.}}">{{end}}{{end}}

  <fieldset>
    <legend>Workspaces this connection can access</legend>
    {{if .Workspaces}}
      {{range .Workspaces}}
      <label class="ws-row">
        <input type="checkbox" name="allowed_workspaces" value="{{.Slug}}" class="ws-checkbox">
        <span class="ws-name">{{.Name}}</span>
        <span class="role">(you are {{.Role}})</span>
      </label>
      {{end}}
    {{else}}
      <p class="no-ws">You are not a member of any workspaces yet. Create or join one to authorize this app.</p>
    {{end}}
    <label class="ws-row">
      <input type="checkbox" name="allowed_workspaces" value="*" id="ws-wildcard">
      <span class="ws-name">Allow access to any workspace I currently or later have access to</span>
    </label>
    <div class="wildcard-warning" id="wildcard-warning">
      This connection will be able to access any workspace you join in the future, without
      asking again. Only choose this if you trust the app fully.
    </div>
  </fieldset>

  <fieldset>
    <legend>Access level</legend>
    {{if .CanRead}}
      <label class="tier-row">
        <input type="radio" name="capability_tier" value="read"{{if eq .DefaultTier "read"}} checked{{end}}>
        <span><strong>Read only</strong><span class="desc">— list, show, search, dashboard</span></span>
      </label>
    {{end}}
    {{if .CanWrite}}
      <label class="tier-row">
        <input type="radio" name="capability_tier" value="write"{{if eq .DefaultTier "write"}} checked{{end}}>
        <span><strong>Read and write</strong><span class="desc">— create + update items, comments, links</span></span>
      </label>
    {{end}}
    {{if .CanAdmin}}
      <label class="tier-row">
        <input type="radio" name="capability_tier" value="admin"{{if eq .DefaultTier "admin"}} checked{{end}}>
        <span><strong>Full access</strong><span class="desc">— read, write, admin (workspace settings, members)</span></span>
      </label>
    {{end}}
  </fieldset>

  <p class="hint">
    Effective permissions are also limited by your role in each workspace —
    e.g. if you're a Viewer in a workspace, you can read but not write there
    even at "Read and write" level. If your membership changes, this
    connection's permissions change immediately.
  </p>

  <p class="hint">
    You can revoke access any time from
    <a href="/console/connected-apps">app.getpad.dev/console/connected-apps</a>.
  </p>

  <div class="actions">
    <button type="submit" name="decision" value="deny">Cancel</button>
    <button type="submit" name="decision" value="approve" class="primary" id="allow-btn" disabled>Allow access</button>
  </div>
</form>

<script>
(function(){
  var wildcard = document.getElementById('ws-wildcard');
  var wsBoxes = document.querySelectorAll('input.ws-checkbox');
  var allowBtn = document.getElementById('allow-btn');
  var warning = document.getElementById('wildcard-warning');

  function refresh() {
    var anySpecific = false;
    for (var i = 0; i < wsBoxes.length; i++) {
      if (wsBoxes[i].checked) { anySpecific = true; break; }
    }
    if (wildcard.checked) {
      // Wildcard wins: disable per-workspace, show warning.
      for (var j = 0; j < wsBoxes.length; j++) {
        wsBoxes[j].checked = false;
        wsBoxes[j].disabled = true;
      }
      warning.classList.add('show');
      allowBtn.disabled = false;
      return;
    }
    // Wildcard off: re-enable per-workspace, hide warning.
    for (var k = 0; k < wsBoxes.length; k++) wsBoxes[k].disabled = false;
    warning.classList.remove('show');
    allowBtn.disabled = !anySpecific;
  }
  wildcard.addEventListener('change', refresh);
  for (var n = 0; n < wsBoxes.length; n++) {
    wsBoxes[n].addEventListener('change', refresh);
  }
  refresh();
})();
</script>
</body></html>`))

// consentData is the template's data shape. Field names match the
// template's references; adding a new field requires updating both.
type consentData struct {
	ClientName    string
	ClientLogoURL string
	Username      string
	Workspaces    []consentWorkspaceRow // user's workspace memberships
	CanRead       bool                  // tier radios — true iff client requested the corresponding pad:* scope
	CanWrite      bool
	CanAdmin      bool
	DefaultTier   string // "read" / "write" / "admin" — initially-checked radio
	CSRF          string
	HiddenFields  url.Values
}

// consentWorkspaceRow renders one workspace checkbox row with the
// user's role for that workspace.
type consentWorkspaceRow struct {
	Slug string
	Name string
	Role string // "owner" / "editor" / "viewer"
}

// renderConsent draws the consent UI. CSRF token comes from the
// existing __Host-pad_csrf cookie (or is minted here if absent —
// rare on the post-login path, but defensive). Workspaces come from
// the user's membership table; the live role lookup at TASK-953
// enforcement time will re-check current role per call, so the role
// we render here is informational ("at the moment of consent, you
// were an Editor in foo"). If the role changes later the token's
// effective permissions change immediately.
//
// Tier radios are constrained to the intersection of {pad:read,
// pad:write, pad:admin} and the client's requested scopes — fosite's
// grant-time check rejects granting any scope outside the request
// (subset rule, RFC 6749 §3.3), so we must only let the user pick
// scopes the client opted into. Default radio is the highest tier
// the client requested (admin > write > read), which matches what
// most clients ask for and lets users dial down if they want.
func (s *Server) renderConsent(w http.ResponseWriter, r *http.Request, ar fosite.AuthorizeRequester, user *models.User) {
	csrf := readOrSetConsentCSRF(w, r, s.secureCookies)

	clientName := ar.GetClient().GetID()
	logo := ""
	if c, err := s.store.GetOAuthClient(ar.GetClient().GetID()); err == nil {
		if c.Name != "" {
			clientName = c.Name
		}
		logo = c.LogoURL
	}

	// Load the user's workspace memberships to populate the multi-
	// select. GetUserWorkspaceMemberships sorts by name. If a user
	// has no workspaces, the template renders a clear "create or
	// join one first" message and the Allow button stays disabled
	// (server validation enforces this regardless).
	memberships, err := s.store.GetUserWorkspaceMemberships(user.ID)
	if err != nil {
		writeInternalError(w, fmt.Errorf("oauth: load workspaces for consent: %w", err))
		return
	}
	rows := make([]consentWorkspaceRow, 0, len(memberships))
	for _, m := range memberships {
		rows = append(rows, consentWorkspaceRow{
			Slug: m.WorkspaceSlug,
			Name: m.WorkspaceName,
			Role: m.Role,
		})
	}

	// Determine which tiers the client requested. fosite's auth-code
	// flow refuses to grant scopes that weren't requested, so the UI
	// must only offer tiers the request includes.
	canRead, canWrite, canAdmin := false, false, false
	for _, sc := range ar.GetRequestedScopes() {
		switch sc {
		case "pad:read":
			canRead = true
		case "pad:write":
			canWrite = true
		case "pad:admin":
			canAdmin = true
		}
	}
	// Default to the highest-power tier the client requested. Users
	// who want to grant less can still pick a lower radio. If the
	// client requested neither read/write/admin, none of the radios
	// renders — that flow is rare (a client requesting only
	// pad:user:* etc.) and would also fail the form submission's
	// tier validation.
	defaultTier := ""
	switch {
	case canAdmin:
		defaultTier = "admin"
	case canWrite:
		defaultTier = "write"
	case canRead:
		defaultTier = "read"
	}

	// Round-trip ONLY the OAuth-request params fosite needs to
	// rebuild the AuthorizeRequest at /authorize/decide time. An
	// allowlist (rather than a "round-trip everything" pattern with
	// blocklisted names) is critical here because the consent UI
	// also has form fields named capability_tier / allowed_workspaces
	// — without the allowlist a malicious client could craft
	//   /oauth/authorize?...&capability_tier=admin&allowed_workspaces=*
	// and the hidden inputs (rendered in DOM order BEFORE the
	// user-controlled radios + checkboxes) would override the user's
	// selection on submit. r.FormValue returns the first matching
	// value, and r.PostForm["allowed_workspaces"] sees the wildcard
	// scan match before the user's slug list. Codex review #376
	// round 1 caught the gap.
	hidden := allowlistedAuthorizeParams(r.URL.Query())

	data := consentData{
		ClientName:    clientName,
		ClientLogoURL: logo,
		Username:      user.Name,
		Workspaces:    rows,
		CanRead:       canRead,
		CanWrite:      canWrite,
		CanAdmin:      canAdmin,
		DefaultTier:   defaultTier,
		CSRF:          csrf,
		HiddenFields:  hidden,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Cache-Control: no-store — the page carries a CSRF token tied
	// to the user's session; serving a cached copy to a different
	// user would let them complete the flow with someone else's
	// identity. Plus the workspace list is per-user.
	w.Header().Set("Cache-Control", "no-store")
	if err := consentTmpl.Execute(w, data); err != nil {
		writeInternalError(w, fmt.Errorf("oauth: render consent: %w", err))
	}
}

// allowlistedAuthorizeParams returns a copy of in containing ONLY
// the keys an OAuth /authorize request is allowed to round-trip
// through the consent form. Defends against URL parameter pollution
// where a malicious client crafts /oauth/authorize with extra params
// (e.g. capability_tier, allowed_workspaces, decision) that would
// otherwise be rendered as hidden inputs and override the user's
// consent selections on submit.
//
// The allowlist covers the standard OAuth 2.1 + PKCE + RFC 8707
// authorize-request parameters fosite reads from r.Form when it
// rebuilds the AuthorizeRequest at /authorize/decide time. Any
// param not in this list is silently dropped — the consent flow
// doesn't use it, and the AuthorizeRequest reconstruction won't
// miss it.
//
// Note `csrf_token` is also dropped: that value belongs in the
// __Host-pad_csrf cookie, not the URL, and we always render a
// fresh one from the cookie.
func allowlistedAuthorizeParams(in url.Values) url.Values {
	allowed := map[string]struct{}{
		"client_id":             {},
		"response_type":         {},
		"redirect_uri":          {},
		"scope":                 {},
		"state":                 {},
		"audience":              {},
		"resource":              {},
		"code_challenge":        {},
		"code_challenge_method": {},
		"nonce":                 {}, // OIDC-compatible clients may send; harmless to round-trip
	}
	out := make(url.Values, len(in))
	for k, vs := range in {
		if _, ok := allowed[k]; !ok {
			continue
		}
		out[k] = append([]string(nil), vs...)
	}
	return out
}

// readOrSetConsentCSRF reads the current __Host-pad_csrf cookie or
// mints a new one if absent. The cookie's value is what gets
// rendered into the consent form's hidden field; validateConsentCSRFToken
// reads it back from both cookie + form to close the double-submit
// loop.
func readOrSetConsentCSRF(w http.ResponseWriter, r *http.Request, secure bool) string {
	if c, err := r.Cookie(csrfCookieName(secure)); err == nil && c.Value != "" {
		return c.Value
	}
	// generateCSRFToken from middleware_csrf.go.
	token := generateCSRFToken()
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName(secure),
		Value:    token,
		Path:     "/",
		MaxAge:   int(webSessionTTL.Seconds()),
		HttpOnly: false,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
	return token
}

// validateConsentCSRFToken implements the form-bound double-submit
// pattern. Reads the __Host-pad_csrf cookie, reads the csrf_token
// form field, compares them in constant time. Both must be the
// expected hex length so an attacker can't flood with mismatched
// equal-prefix values.
//
// Why duplicate middleware_csrf.go's logic instead of reusing it:
// the existing CSRFProtect middleware reads from the X-CSRF-Token
// header (SPA convention), but the consent stub is server-rendered
// HTML where the natural carrier is a form field. Same security
// model, different transport.
func (s *Server) validateConsentCSRFToken(r *http.Request) error {
	cookie, err := r.Cookie(csrfCookieName(s.secureCookies))
	if err != nil || cookie.Value == "" {
		return errors.New("missing CSRF cookie")
	}
	form := r.FormValue("csrf_token")
	if form == "" {
		return errors.New("missing csrf_token form field")
	}
	const expectedLen = csrfTokenLen * 2 // hex
	if len(cookie.Value) != expectedLen || len(form) != expectedLen {
		return errors.New("CSRF token mismatch")
	}
	if subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(form)) != 1 {
		return errors.New("CSRF token mismatch")
	}
	return nil
}

// =====================================================================
// Helpers shared with handlers_well_known.go for the discovery doc
// =====================================================================

// authServerIssuerURL returns the canonical issuer URL for this
// authorization server — what /.well-known/oauth-authorization-server
// emits as `issuer`. Sourced from cfg.AuthServerURL (the
// PAD_AUTH_SERVER_URL env var); falls back to the request host
// for local dev. See handlers_well_known.go's existing fallback.
func (s *Server) authServerIssuerURL(r *http.Request) string {
	if s.mcpAuthServerURL != "" {
		return strings.TrimRight(s.mcpAuthServerURL, "/")
	}
	if r != nil && r.Host != "" {
		return "https://" + r.Host
	}
	return ""
}

// Compile-time guard: oauth.NewSession returns a fosite-compatible
// type so we can pass it to NewAccessRequest / NewAuthorizeResponse
// without the call sites importing fosite.Session.
var _ fosite.Session = (*oauth.Session)(nil)

// dummyContextSilencer keeps the context import live in case future
// adds (e.g. cancellation propagation through fosite calls) drop it.
var _ = context.Background
