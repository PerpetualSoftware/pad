package cli

import (
	"net/url"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// User-scoped API-token endpoints (GET/POST /auth/tokens,
// DELETE /auth/tokens/{id}). These are the mint/list/revoke calls behind
// `pad token` — the CLI counterpart to the web settings page, so a headless
// agent setup can mint from a terminal (`pad auth login -i` prompts for
// email/password rather than opening a browser).
//
// NOT usable end-to-end when PAD_TOKEN carries a `pad_` API token, which this
// comment used to claim without that qualifier: #879 added the override and
// left minting web-only, #1237 added this group so the CLI could mint at all,
// and #1267 then gated minting on a session. The server refuses a mint
// authenticated by an API token with 403 `session_required`, because the tokens
// such a mint produces outlive the revocation of the token that made them.
// `CreateUserToken` therefore needs a session — a cookie, or a `padsess_`
// bearer, which PAD_TOKEN also accepts — while list and revoke stay reachable
// by a PAT. See `handlers_tokens.go::requireInteractiveSession`.

// ListUserTokens returns the caller's API tokens. Metadata only — the
// server never returns secret material on list.
func (c *Client) ListUserTokens() ([]models.APIToken, error) {
	var out []models.APIToken
	if err := c.get("/auth/tokens", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateUserToken mints a new API token owned by the authenticated user.
// The response carries the plaintext secret exactly once; it is never
// retrievable again.
func (c *Client) CreateUserToken(input models.APITokenCreate) (*models.APITokenWithSecret, error) {
	var out models.APITokenWithSecret
	if err := c.post("/auth/tokens", input, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RevokeUserToken deletes an API token by id. The server verifies the
// token belongs to the caller; an unknown or foreign id is a 404.
func (c *Client) RevokeUserToken(id string) error {
	return c.delete("/auth/tokens/" + url.PathEscape(id))
}

// RotateUserToken generates a new secret for an existing token, keeping
// its metadata (name, scopes, workspace). The OLD secret dies with the
// UPDATE — RotateAPIToken replaces the hash in place, so there is no
// grace window. expiresIn > 0 sets a new expiry in days (capped by the
// platform max lifetime); 0 preserves the original. Like minting, this
// requires an interactive session (#1267) — a PAT-authenticated call is
// refused 403 session_required.
func (c *Client) RotateUserToken(id string, expiresIn int) (*models.APITokenWithSecret, error) {
	input := struct {
		ExpiresIn int `json:"expires_in,omitempty"`
	}{ExpiresIn: expiresIn}
	var out models.APITokenWithSecret
	if err := c.post("/auth/tokens/"+url.PathEscape(id)+"/rotate", input, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
