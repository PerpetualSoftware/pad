package server

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/store"
)

// /api/v1/oauth/clients/{id}/public-info — non-sensitive OAuth client
// metadata for the consent screen (TASK-952) and the OAuth-intent
// banner on /login + /register (TASK-1001, already shipped).
//
// Why a separate endpoint vs. reusing the DCR registration response:
//
// DCR's response (sub-PR C's handleOAuthRegister) is the one-shot
// document a registering client receives at registration time. The
// consent UI later needs the same fields — name, logo, redirect URIs
// — but on a different device / browser session, AFTER registration.
// A read endpoint is the only sane way to fetch the row without
// re-running registration (which would create a new row with a new
// client_id every time and break the OAuth flow).
//
// Auth + scope:
//
//   - Logged-in users only. No Bearer-token bypass — the consent
//     screen runs with a session cookie, the OAuth-intent banner
//     runs after a login, and the metadata is non-sensitive enough
//     that requiring auth (vs. open-public) primarily prevents
//     unauthenticated enumeration of the oauth_clients table.
//   - Any logged-in user can read any client's public-info. The
//     four fields here (client_id, client_name, logo_uri, redirect_uris)
//     are deliberately the only ones — no scopes, no grant types,
//     no created_at, nothing that could fingerprint OAuth-server
//     behaviour or leak when an integration was set up.
//   - Cloud-mode-only: self-hosted deployments don't host an OAuth
//     surface, so this endpoint serves no purpose there. The route
//     is mounted under requireCloudMode at the registration site
//     (server.go), which 404s outside cloud mode.
//
// Wire shape:
//
//	GET /api/v1/oauth/clients/{id}/public-info
//	200 OK
//	{
//	  "client_id":     "abc-...",
//	  "client_name":   "Claude Desktop",
//	  "logo_uri":      "https://anthropic.com/logo.png",
//	  "redirect_uris": ["claude://oauth/callback", "https://app.claude.ai/cb"]
//	}
//	404 — unknown client
//
// Why explicit fields rather than embedding the full models.OAuthClient:
// the model carries grant_types / response_types / token_endpoint_auth_method
// / scope / created_at, which are operational details of the OAuth
// server and not relevant to consent UX. A typed response struct
// pins the leak surface; a future model field (e.g. a client_secret
// for confidential clients) won't accidentally appear here.
type oauthClientPublicInfo struct {
	ClientID     string   `json:"client_id"`
	ClientName   string   `json:"client_name"`
	LogoURI      string   `json:"logo_uri,omitempty"`
	RedirectURIs []string `json:"redirect_uris"`
}

// handleOAuthClientPublicInfo serves the public metadata for a
// registered OAuth client. Auth-required at the route level (see
// the /api/v1 group in server.go); cloud-mode-gated at the route
// level (requireCloudMode wraps the registration).
//
// Returns 404 if the client is unknown — explicitly, not 401, so
// authorized callers can distinguish "this client doesn't exist"
// from "you're not allowed to see this client" (the latter never
// happens — every logged-in user can see every client's public
// info).
func (s *Server) handleOAuthClientPublicInfo(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "client id is required")
		return
	}

	client, err := s.store.GetOAuthClient(id)
	if err != nil {
		if errors.Is(err, store.ErrOAuthNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "client not found")
			return
		}
		writeInternalError(w, err)
		return
	}

	resp := oauthClientPublicInfo{
		ClientID:     client.ID,
		ClientName:   client.Name,
		LogoURI:      client.LogoURL,
		RedirectURIs: append([]string(nil), client.RedirectURIs...),
	}
	writeJSON(w, http.StatusOK, resp)
}
