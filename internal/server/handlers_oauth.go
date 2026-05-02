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

// OAuth 2.1 authorization-server HTTP handlers (PLAN-943 TASK-951
// sub-PR C). The four endpoints below + the populated
// /.well-known/oauth-authorization-server discovery doc complete
// the flow-driving half of the OAuth surface — sub-PR D adds
// /revoke and /introspect, sub-PR E wires MCPBearerAuth to OAuth
// introspection.
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
//
// All four go via Server.oauthServer (set by SetOAuthServer at
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

	// Render the inline consent stub. TODO(TASK-952): replace with
	// the SvelteKit /oauth/consent page that surfaces workspace
	// allow-list selection (TASK-953) + per-app branding.
	s.renderConsentStub(w, r, ar, user)
}

// =====================================================================
// /oauth/authorize/decide — process consent decision
// =====================================================================

// handleOAuthAuthorizeDecide processes the consent decision posted
// from the inline consent stub. Validates a form-bound CSRF token
// (the same __Host-pad_csrf cookie the SPA uses, but read from a
// hidden form field instead of a header), then either runs fosite's
// authorize-response flow (approve) or writes an access_denied
// redirect (deny).
//
// The decision endpoint is what an attacker would target via a
// cross-origin POST trying to silently grant consent on a victim's
// behalf. The CSRF token check binds the POST to the original
// browser context. Same defense the SPA uses elsewhere, just
// rendered via form field rather than header because the consent
// stub is server-rendered HTML, not SPA fetch().
//
// On approve: ar.GrantScope() each requested scope (auto-approve
// all in the v1 stub), ar.GrantAudience(canonical), then run
// fosite. On deny: WriteAuthorizeError with ErrAccessDenied.
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

	// v1 stub: auto-approve every requested scope. TASK-952's real
	// consent UI lets the user toggle per-scope, and TASK-953 adds
	// the workspace allow-list selection that gates which workspaces
	// the issued token can reach.
	for _, sc := range ar.GetRequestedScopes() {
		ar.GrantScope(sc)
	}
	for _, aud := range ar.GetRequestedAudience() {
		ar.GrantAudience(aud)
	}

	// Build the session that fosite serializes alongside the code.
	// The opaque token's signature ties back to this; introspection
	// (sub-PR D) reads back the same Subject for the bearer-auth
	// gate on /mcp (sub-PR E).
	session := oauth.NewSession(user.ID)

	resp, err := s.oauthServer.Provider().NewAuthorizeResponse(ctx, ar, session)
	if err != nil {
		s.oauthServer.Provider().WriteAuthorizeError(ctx, w, ar, err)
		return
	}

	s.oauthServer.Provider().WriteAuthorizeResponse(ctx, w, ar, resp)
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

	// Mirror /authorize's grant: the access response derives from
	// the granted scope/audience set on the request. fosite preserves
	// these from the underlying auth-code grant for the refresh path.
	for _, sc := range ar.GetRequestedScopes() {
		ar.GrantScope(sc)
	}
	for _, aud := range ar.GetRequestedAudience() {
		ar.GrantAudience(aud)
	}

	resp, err := s.oauthServer.Provider().NewAccessResponse(ctx, ar)
	if err != nil {
		s.oauthServer.Provider().WriteAccessError(ctx, w, ar, err)
		return
	}

	s.oauthServer.Provider().WriteAccessResponse(ctx, w, ar, resp)
}

// =====================================================================
// Inline consent stub — TODO(TASK-952): replace with real UI
// =====================================================================

// consentTmpl is the inline-HTML consent screen rendered by
// /oauth/authorize when the user is logged in. Deliberately ugly —
// TASK-952 replaces this with the SvelteKit /oauth/consent page
// that surfaces workspace allow-list selection (TASK-953) + proper
// branding. The stub is just enough to drive the auth-code flow
// end-to-end for testing during the OAuth-server build-out.
//
// Form action POSTs to /oauth/authorize/decide with a hidden CSRF
// token + a hidden copy of every original /authorize query param.
// The decision is "approve" (the primary button) or "deny".
//
// Style note: any future caller that updates this template MUST
// keep the {{.CSRF}} field's auto-escape disabled — it's pre-
// validated as hex in setCSRFCookie / validateConsentCSRFToken,
// and the html/template package's default escaping would otherwise
// double-escape the hex chars. We use {{.CSRF}} (escaped) since
// hex IS safe for HTML attributes.
var consentTmpl = template.Must(template.New("consent").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>Authorize {{.ClientName}}</title>
<style>
body { font: 14px system-ui; max-width: 480px; margin: 4em auto; padding: 0 1em; color: #333; }
h1 { font-size: 1.4em; }
.client { display: flex; align-items: center; gap: .75em; margin: 1em 0; padding: 1em;
          border: 1px solid #ddd; border-radius: 8px; background: #fafafa; }
.client img { width: 48px; height: 48px; border-radius: 8px; }
ul { padding-left: 1.4em; } li { margin: .25em 0; }
.actions { margin-top: 2em; display: flex; gap: 1em; }
button { padding: .75em 1.5em; border-radius: 6px; border: 1px solid #888;
         font-size: 1em; cursor: pointer; }
button.primary { background: #2563eb; color: white; border-color: #2563eb; }
.footer { margin-top: 2em; font-size: .85em; color: #888; }
</style></head>
<body>
<h1>Authorize {{.ClientName}}?</h1>
<div class="client">
  {{if .LogoURL}}<img src="{{.LogoURL}}" alt="">{{end}}
  <div>
    <strong>{{.ClientName}}</strong><br>
    <small>Wants to access your Pad account as {{.Username}}</small>
  </div>
</div>
<p>Permissions requested:</p>
<ul>{{range .Scopes}}<li><code>{{.}}</code></li>{{end}}</ul>
<form method="POST" action="/oauth/authorize/decide">
  <input type="hidden" name="csrf_token" value="{{.CSRF}}">
  {{range $k, $vs := .HiddenFields}}{{range $vs}}<input type="hidden" name="{{$k}}" value="{{.}}">{{end}}{{end}}
  <div class="actions">
    <button type="submit" name="decision" value="deny">Deny</button>
    <button type="submit" name="decision" value="approve" class="primary">Approve</button>
  </div>
</form>
<p class="footer">Inline consent stub — TASK-952 replaces with the production UI.</p>
</body></html>`))

type consentData struct {
	ClientName   string
	Username     string
	LogoURL      string
	Scopes       []string
	CSRF         string
	HiddenFields url.Values
}

// renderConsentStub draws the inline approval page. The CSRF token
// is the existing __Host-pad_csrf cookie value; the consent decision
// handler validates the form-submitted copy against the cookie via
// validateConsentCSRFToken.
//
// If the cookie isn't set yet (rare — the user is logged in, so the
// session creation path has already issued one), we mint one here
// so the form always carries a valid token.
func (s *Server) renderConsentStub(w http.ResponseWriter, r *http.Request, ar fosite.AuthorizeRequester, user *models.User) {
	csrf := readOrSetConsentCSRF(w, r, s.secureCookies)

	logo := ""
	if dc, ok := ar.GetClient().(*fosite.DefaultClient); ok && dc != nil {
		// fosite.DefaultClient doesn't expose LogoURL; fetch the row
		// directly so we can show the logo if registered.
		if c, err := s.store.GetOAuthClient(ar.GetClient().GetID()); err == nil {
			logo = c.LogoURL
		}
	}

	clientName := ar.GetClient().GetID()
	if c, err := s.store.GetOAuthClient(ar.GetClient().GetID()); err == nil && c.Name != "" {
		clientName = c.Name
	}

	// Mirror every original query param into the form so the POST
	// can re-validate via fosite.NewAuthorizeRequest with the same
	// inputs. r.URL.Query() returns a parsed map with the original
	// values intact.
	hidden := r.URL.Query()
	hidden.Del("csrf_token") // never round-trip this; we render fresh

	data := consentData{
		ClientName:   clientName,
		Username:     user.Name,
		LogoURL:      logo,
		Scopes:       []string(ar.GetRequestedScopes()),
		CSRF:         csrf,
		HiddenFields: hidden,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Cache-Control: no-store — the consent page carries a CSRF
	// token tied to the user's session; serving a cached copy to a
	// different user would let them complete the flow with someone
	// else's identity.
	w.Header().Set("Cache-Control", "no-store")
	if err := consentTmpl.Execute(w, data); err != nil {
		// Already streaming HTML; logging is the most we can do.
		writeInternalError(w, fmt.Errorf("oauth: render consent: %w", err))
	}
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
