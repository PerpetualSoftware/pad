package server

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/ory/fosite"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/oauth"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// Install-client tokens (SPEC-6 U5a, TASK-3394, DOC-3371 §4 Credentials).
//
// An installed app's confidential client obtains SERVICE tokens with
// client_credentials: the subject is its install's bot, the audience is the
// app API resource only, and the store's issuance barrier persists the token
// under the install row's lock with a binding that records the install's
// epoch. The app API (U6) checks those tokens in-process through
// introspectAppToken, never through the public /oauth/introspect, which
// refuses install clients entirely (lead ruling R2).

// requestClientID returns the client id a token or introspection request
// authenticates as: HTTP Basic first (RFC 6749 §2.3.1), then the form.
func requestClientID(r *http.Request) string {
	if id, _, ok := r.BasicAuth(); ok && id != "" {
		return id
	}
	return strings.TrimSpace(r.PostForm.Get("client_id"))
}

// installClientFor returns the request's client when it is an install
// client, or nil.
func (s *Server) installClientFor(r *http.Request) *models.OAuthClient {
	id := requestClientID(r)
	if id == "" {
		return nil
	}
	c, err := s.store.GetOAuthClient(id)
	if err != nil || !c.IsInstallClient() {
		return nil
	}
	return c
}

// writeTokenError answers a token request with an RFC 6749 error before
// fosite has built a requester.
func (s *Server) writeTokenError(ctx context.Context, w http.ResponseWriter, err error) {
	s.oauthServer.Provider().WriteAccessError(ctx, w, fosite.NewAccessRequest(oauth.NewSession("")), err)
}

// prepareInstallTokenRequest applies the install-client rules to a token
// request BEFORE fosite reads it, and reports false after answering when one
// refuses:
//   - only client_credentials (delegated grants arrive with TASK-3399);
//   - `resource` is REQUIRED, with no MCP default (install clients are new,
//     so they never had the log-only phase TASK-3363 gave humans);
//   - an `audience` that names something else is refused, not reconciled.
func (s *Server) prepareInstallTokenRequest(ctx context.Context, w http.ResponseWriter, r *http.Request) bool {
	if r.PostForm.Get("grant_type") != "client_credentials" {
		s.writeTokenError(ctx, w, fosite.ErrUnsupportedGrantType.WithHint("An installed app's client uses client_credentials."))
		return false
	}
	resource := strings.TrimSpace(r.PostForm.Get("resource"))
	if resource == "" {
		s.writeTokenError(ctx, w, fosite.ErrInvalidRequest.WithHint("The resource parameter is required."))
		return false
	}
	if aud := strings.TrimSpace(r.PostForm.Get("audience")); aud != "" && oauth.NormalizeAudience(aud) != oauth.NormalizeAudience(resource) {
		s.writeTokenError(ctx, w, fosite.ErrInvalidRequest.WithHint("audience and resource name different resources."))
		return false
	}
	appAud := s.store.AppAPIAudience()
	if appAud == "" || oauth.NormalizeAudience(resource) != oauth.NormalizeAudience(appAud) {
		s.writeTokenError(ctx, w, fosite.ErrInvalidRequest.WithHint("An installed app's client holds the app API resource only."))
		return false
	}
	r.PostForm.Set("audience", resource)
	r.Form.Set("audience", resource)
	return true
}

// grantInstallServiceToken fills a validated client_credentials request: the
// app scope, the app API audience, and the install's bot as the subject.
// fosite's client_credentials handler grants nothing on its own.
func (s *Server) grantInstallServiceToken(ar fosite.AccessRequester, client *models.OAuthClient) error {
	bot, err := s.store.AppPrincipalForInstall(client.AppInstallID)
	if err != nil {
		return fosite.ErrServerError.WithWrap(err)
	}
	if bot == nil || bot.IsDisabled() {
		return fosite.ErrInvalidClient.WithHint("This app is not installed.")
	}
	sess, ok := ar.GetSession().(*oauth.Session)
	if !ok {
		return fosite.ErrServerError.WithHint("unexpected session type")
	}
	if sess.DefaultSession == nil {
		return fosite.ErrServerError.WithHint("empty session")
	}
	sess.DefaultSession.Subject = bot.ID
	ar.GrantScope(store.InstallClientScope)
	ar.GrantAudience(s.store.AppAPIAudience())
	return nil
}

// isOwnInstallBot reports whether user is the bot of client's install: the
// one subject an install client may hold a token for (lead ruling R1).
func (s *Server) isOwnInstallBot(client *models.OAuthClient, user *models.User) bool {
	if client == nil || !client.IsInstallClient() || user == nil || !user.IsApp() {
		return false
	}
	bot, err := s.store.AppPrincipalForInstall(client.AppInstallID)
	return err == nil && bot != nil && bot.ID == user.ID
}

// Errors introspectAppToken refuses with. Every one is "not a usable app
// token"; the app API answers them all with the same 401.
var (
	errAppTokenInactive  = errors.New("app token: not an active access token")
	errAppTokenAudience  = errors.New("app token: wrong audience")
	errAppTokenUnbound   = errors.New("app token: no binding")
	errAppTokenMismatch  = errors.New("app token: binding does not match the token")
	errAppTokenInstall   = errors.New("app token: the install is not active")
	errAppTokenEpoch     = errors.New("app token: issued under an earlier epoch")
	errAppTokenDisabled  = errors.New("app token: the client is disabled")
	errAppTokenNoBackend = errors.New("app token: no OAuth server")
)

// AppTokenGrant is a verified app token: who it binds to, for the app API
// middleware (U6) to finish with the workspace, access and ceiling steps.
type AppTokenGrant struct {
	RequestID   string
	ClientID    string
	InstallID   string
	WorkspaceID string
	AuthEpoch   int64
	AuthKind    string // "service" or "delegated"
	Subject     string
}

// introspectAppToken is the app API's token check, in-process (DOC-3371 §4
// "Token middleware", steps 1–3). In order: an ACTIVE ACCESS token (a refresh
// token is refused); its audience is the app API resource; it has a binding
// whose client and request id are the token's; the install is active; the
// client is not disabled; and the binding's epoch is the install's current
// epoch, so a disable or rotate that bumped the epoch ends it, even one that
// raced its issuance.
func (s *Server) introspectAppToken(ctx context.Context, token string) (*AppTokenGrant, error) {
	if s.oauthServer == nil {
		return nil, errAppTokenNoBackend
	}
	ar, tokenUse, err := s.oauthServer.IntrospectToken(ctx, token)
	if err != nil || ar == nil || tokenUse != fosite.AccessToken {
		return nil, errAppTokenInactive
	}
	appAud := s.store.AppAPIAudience()
	aud := ar.GetGrantedAudience()
	if appAud == "" || len(aud) != 1 || oauth.NormalizeAudience(aud[0]) != oauth.NormalizeAudience(appAud) {
		return nil, errAppTokenAudience
	}
	st, err := s.store.GetAppTokenState(ar.GetID())
	if err != nil {
		return nil, err
	}
	if st == nil {
		return nil, errAppTokenUnbound
	}
	if st.Binding.ClientID != ar.GetClient().GetID() {
		return nil, errAppTokenMismatch
	}
	if st.InstallState != "active" {
		return nil, errAppTokenInstall
	}
	if st.ClientDisabled {
		return nil, errAppTokenDisabled
	}
	if st.Binding.AuthEpoch != st.InstallEpoch {
		return nil, errAppTokenEpoch
	}
	return &AppTokenGrant{
		RequestID: st.Binding.RequestID, ClientID: st.Binding.ClientID, InstallID: st.Binding.InstallID,
		WorkspaceID: st.Binding.WorkspaceID, AuthEpoch: st.Binding.AuthEpoch, AuthKind: st.Binding.AuthKind,
		Subject: ar.GetSession().GetSubject(),
	}, nil
}

// introspectionCallerIsInstall reports whether a public /oauth/introspect
// request authenticates as an install client, by Basic credentials or by an
// install client's access token as the Bearer (lead ruling R2: install
// clients do not use the public endpoint).
func (s *Server) introspectionCallerIsInstall(r *http.Request) bool {
	if s.installClientFor(r) != nil {
		return true
	}
	auth := r.Header.Get("Authorization")
	if len(auth) > 7 && strings.EqualFold(auth[:7], "bearer ") {
		if ar, _, err := s.oauthServer.IntrospectToken(r.Context(), strings.TrimSpace(auth[7:])); err == nil && ar != nil {
			if c, cerr := s.store.GetOAuthClient(ar.GetClient().GetID()); cerr == nil && c.IsInstallClient() {
				return true
			}
		}
	}
	return false
}
