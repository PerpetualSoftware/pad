package cli

import (
	"net/url"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// User-scoped API-token endpoints (GET/POST /auth/tokens,
// DELETE /auth/tokens/{id}). These are the mint/list/revoke calls behind
// `pad token` — the CLI counterpart to the web settings page, so the
// PAD_TOKEN override (#879) is usable end-to-end without a browser.

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
