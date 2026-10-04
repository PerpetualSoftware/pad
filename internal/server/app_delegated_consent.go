package server

import (
	"fmt"
	"html/template"
	"net/http"
	"net/url"

	"github.com/ory/fosite"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/oauth"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// Delegated app sign-in (SPEC-6 U5b-1, TASK-3399; DOC-3371 §4 "Delegated
// tokens"): authorization code + PKCE for an install's client, so a Pad user
// can act through the app. The consent page's workspace is FIXED to the
// install: no workspace choice, no wildcard, no creation flag. The person
// chooses the access, read-only by default, and write only when the manifest
// offers it (lead ruling Q2). No oauth_connections row is written (lead
// ruling Q1): the install fixes the workspace, and the binding the issuance
// barrier writes carries the kind, epoch and access.

// installClientOf returns the install client an authorize request names, or
// nil for any other client.
func (s *Server) installClientOf(ar fosite.AuthorizeRequester) (*models.OAuthClient, error) {
	c, err := s.store.GetOAuthClient(ar.GetClient().GetID())
	if err != nil || c == nil || !c.IsInstallClient() {
		return nil, err
	}
	return c, nil
}

// appConsentRefusal is why a delegated sign-in cannot proceed: answered as
// an authorize error to the app (a registered redirect), except when apps
// are off, which answers the same 404 every app route gives.
type appConsentRefusal struct {
	notFound bool
	err      *fosite.RFC6749Error
}

// appConsentGate is every check the consent render AND the decision make,
// read fresh each time: apps on, the install active and offering delegated
// access, PKCE with S256, the app API audience, and the person a human
// member of the install's workspace.
func (s *Server) appConsentGate(ar fosite.AuthorizeRequester, user *models.User, client *models.OAuthClient) (*store.InstallConsentState, *appConsentRefusal) {
	if !s.appsAvailable() {
		return nil, &appConsentRefusal{notFound: true}
	}
	// Closed until U5b-2 (TASK-3399): the app API does not accept delegated
	// tokens yet, so no grant is issued that it could not honour.
	if !s.delegatedSignInOpen {
		return nil, &appConsentRefusal{err: fosite.ErrAccessDenied.WithHint("Signing in to installed apps is not yet available.")}
	}
	st, err := s.store.GetInstallConsentState(client.AppInstallID)
	if err != nil {
		return nil, &appConsentRefusal{err: fosite.ErrServerError.WithWrap(err)}
	}
	if st == nil || st.State != "active" {
		return nil, &appConsentRefusal{err: fosite.ErrAccessDenied.WithHint("This app is not available.")}
	}
	if st.DelegatedAccess != "read" && st.DelegatedAccess != "write" {
		return nil, &appConsentRefusal{err: fosite.ErrAccessDenied.WithHint("This app does not offer sign-in.")}
	}
	// Second layer: fosite already refuses a missing or plain PKCE for every
	// client (oauth/server.go EnforcePKCE, S256 only), so this check stays
	// for the case a future config loosens that (U5b-1 mutant C2 survives).
	form := ar.GetRequestForm()
	if form.Get("code_challenge") == "" || form.Get("code_challenge_method") != "S256" {
		return nil, &appConsentRefusal{err: fosite.ErrInvalidRequest.WithHint("An installed app signs people in with PKCE (S256).")}
	}
	// Second layer: an install client's fosite audiences are its own
	// AllowedAudiences, the app API only (oauth/storage.go), so fosite
	// already refuses any other resource (U5b-1 mutant C3 survives).
	appAud := s.store.AppAPIAudience()
	audOK := false
	for _, a := range ar.GetRequestedAudience() {
		if appAud != "" && oauth.NormalizeAudience(a) == oauth.NormalizeAudience(appAud) {
			audOK = true
		} else {
			audOK = false
			break
		}
	}
	if !audOK {
		return nil, &appConsentRefusal{err: fosite.ErrInvalidRequest.WithHint("An installed app's client holds the app API resource only.")}
	}
	m, err := s.store.GetWorkspaceMember(st.WorkspaceID, user.ID)
	if err != nil {
		return nil, &appConsentRefusal{err: fosite.ErrServerError.WithWrap(err)}
	}
	if m == nil || user.IsApp() {
		return nil, &appConsentRefusal{err: fosite.ErrAccessDenied.WithHint("You are not a member of the workspace this app is installed in.")}
	}
	return st, nil
}

func (s *Server) writeAppConsentRefusal(w http.ResponseWriter, r *http.Request, ar fosite.AuthorizeRequester, ref *appConsentRefusal) {
	s.recordOAuthFlow("failed")
	if ref.notFound {
		http.NotFound(w, r)
		return
	}
	s.oauthServer.Provider().WriteAuthorizeError(r.Context(), s.authorizeResponseWriter(w), ar, ref.err)
}

var appConsentTmpl = template.Must(template.New("app-consent").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Sign in to {{.AppName}}</title>
<style>
body{font-family:system-ui,sans-serif;max-width:32rem;margin:3rem auto;padding:0 1rem;color:#1f2328;background:#fff}
.card{border:1px solid #d0d7de;border-radius:8px;padding:1.5rem}
.muted{color:#59636e;font-size:.9rem}
fieldset{border:0;padding:0;margin:1rem 0}
label{display:block;margin:.4rem 0}
.actions{display:flex;gap:.5rem;justify-content:flex-end;margin-top:1.25rem}
button{padding:.5rem 1rem;border-radius:6px;border:1px solid #d0d7de;background:#f6f8fa;cursor:pointer}
button.primary{background:#1f6feb;color:#fff;border-color:#1f6feb}
@media (prefers-color-scheme:dark){body{background:#0d1117;color:#e6edf3}.card{border-color:#30363d}.muted{color:#9198a1}button{background:#21262d;color:#e6edf3;border-color:#30363d}}
</style></head><body>
<div class="card">
<h1>Sign in to {{.AppName}}</h1>
<p class="muted">{{.Origin}}</p>
<p><strong>{{.AppName}}</strong> will act as you, <strong>{{.Username}}</strong>, in the workspace <strong>{{.WorkspaceName}}</strong> only, inside the collections the app was installed with.</p>
<form method="post" action="/oauth/authorize/decide">
  <input type="hidden" name="csrf_token" value="{{.CSRF}}">
  {{range $k, $vs := .HiddenFields}}{{range $vs}}<input type="hidden" name="{{$k}}" value="{{.}}">{{end}}{{end}}
  <fieldset><legend>What may it do as you?</legend>
  <label><input type="radio" name="app_access" value="read" checked> Read only</label>
  {{if .OfferWrite}}<label><input type="radio" name="app_access" value="write"> Read and write</label>{{end}}
  </fieldset>
  <p class="muted">You can revoke this at any time from Connected apps.</p>
  <div class="actions">
    <button type="submit" name="decision" value="deny">Deny</button>
    <button type="submit" name="decision" value="approve" class="primary">Allow</button>
  </div>
</form>
</div></body></html>`))

type appConsentData struct {
	AppName, Origin, Username, WorkspaceName, CSRF string
	OfferWrite                                     bool
	HiddenFields                                   url.Values
}

// renderAppConsent renders the delegated consent page.
func (s *Server) renderAppConsent(w http.ResponseWriter, r *http.Request, ar fosite.AuthorizeRequester, user *models.User, client *models.OAuthClient) {
	st, ref := s.appConsentGate(ar, user, client)
	if ref != nil {
		s.writeAppConsentRefusal(w, r, ar, ref)
		return
	}
	ws, err := s.store.GetWorkspaceByID(st.WorkspaceID)
	if err != nil || ws == nil {
		s.writeAppConsentRefusal(w, r, ar, &appConsentRefusal{err: fosite.ErrAccessDenied.WithHint("This app is not available.")})
		return
	}
	s.recordOAuthFlow("started")
	csrf := readOrSetConsentCSRF(w, r, s.secureCookies)
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := appConsentTmpl.Execute(w, appConsentData{
		AppName: st.AppName, Origin: st.Origin, Username: user.Name, WorkspaceName: ws.Name, CSRF: csrf,
		OfferWrite:   st.DelegatedAccess == "write",
		HiddenFields: allowlistedAuthorizeParams(r.URL.Query()),
	}); err != nil {
		writeInternalError(w, fmt.Errorf("oauth: render app consent: %w", err))
	}
}

// decideAppConsent completes an approved delegated sign-in: the gate again,
// the person's access choice, and the authorization code, whose session
// carries the install's epoch (read now, as the code is minted; the store's
// barrier re-checks it at every persistence after this one), the kind and
// the access. Deny was answered by the caller.
func (s *Server) decideAppConsent(w http.ResponseWriter, r *http.Request, ar fosite.AuthorizeRequester, user *models.User, client *models.OAuthClient) {
	st, ref := s.appConsentGate(ar, user, client)
	if ref != nil {
		s.writeAppConsentRefusal(w, r, ar, ref)
		return
	}
	access := r.PostForm.Get("app_access")
	if access == "" {
		access = "read" // least privilege (lead ruling Q2)
	}
	if access != "read" && !(access == "write" && st.DelegatedAccess == "write") {
		s.recordOAuthFlow("failed")
		writeError(w, http.StatusBadRequest, "invalid_request", "app_access must be read, or write when the app offers it")
		return
	}
	epoch, err := s.store.InstallEpoch(client.AppInstallID)
	if err != nil {
		s.writeAppConsentRefusal(w, r, ar, &appConsentRefusal{err: fosite.ErrAccessDenied.WithHint("This app is not available.")})
		return
	}
	ar.GrantScope(store.InstallClientScope)
	ar.GrantAudience(s.store.AppAPIAudience())
	session := oauth.NewSession(user.ID)
	session.DefaultSession.Extra[store.InstallEpochSessionKey] = epoch
	session.DefaultSession.Extra[store.InstallAuthKindSessionKey] = "delegated"
	session.DefaultSession.Extra[store.InstallAccessSessionKey] = access
	// The credentials this request's session resolved the person under: the
	// barrier refuses the code if they changed since (a disable destroyed
	// this session, then a re-enable), codex U5b-1 r7.
	session.DefaultSession.Extra[store.InstallPersonEpochSessionKey] = user.CredentialEpoch
	resp, err := s.oauthServer.Provider().NewAuthorizeResponse(r.Context(), ar, session)
	if err != nil {
		s.recordOAuthFlow("failed")
		s.oauthServer.Provider().WriteAuthorizeError(r.Context(), s.authorizeResponseWriter(w), ar, err)
		return
	}
	s.recordOAuthFlow("completed")
	s.oauthServer.Provider().WriteAuthorizeResponse(r.Context(), s.authorizeResponseWriter(w), ar, resp)
}

// appSignInAvailable reports whether a request at the authorize or decide
// door names an installed app's client while apps are available: those doors
// serve that sign-in with MCP off too. It reads client_id from the query or
// the form; the full rules run later, in appConsentGate.
func (s *Server) appSignInAvailable(r *http.Request) bool {
	if !s.appsAvailable() || s.oauthServer == nil {
		return false
	}
	if err := r.ParseForm(); err != nil {
		return false
	}
	c, err := s.store.GetOAuthClient(r.Form.Get("client_id"))
	return err == nil && c != nil && c.IsInstallClient()
}
