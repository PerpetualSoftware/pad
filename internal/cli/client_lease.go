package cli

import (
	"net/url"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Item execution lease endpoints (#1221):
// POST /workspaces/{ws}/items/{ref}/claim and .../release. A 409 with
// code "lease_held" comes back as *APIError whose message names the live
// holder and expiry; callers surface it rather than retrying blindly.

// leaseRequest is the wire body for claim and release. Zero values are
// omitted so the server applies its defaults (authenticated identity as
// holder; 15-minute TTL).
type leaseRequest struct {
	Holder     string `json:"holder,omitempty"`
	TTLSeconds int    `json:"ttl_seconds,omitempty"`
}

// claimResponse mirrors the structured success bodies: ref plus the lease
// (claim) or the released verdict (release).
type leaseResponse struct {
	Ref      string            `json:"ref"`
	Lease    *models.ItemLease `json:"lease,omitempty"`
	Released *bool             `json:"released,omitempty"`
}

// ClaimItem acquires (or refreshes, for the live holder) the execution
// lease on an item. holder and ttlSeconds may be zero for the server
// defaults.
func (c *Client) ClaimItem(wsSlug, ref, holder string, ttlSeconds int) (*models.ItemLease, error) {
	var out leaseResponse
	err := c.post("/workspaces/"+url.PathEscape(wsSlug)+"/items/"+url.PathEscape(ref)+"/claim",
		leaseRequest{Holder: holder, TTLSeconds: ttlSeconds}, &out)
	if err != nil {
		return nil, err
	}
	return out.Lease, nil
}

// ReleaseItem clears the holder's lease on an item. released=false means
// there was nothing live to release — a success, not an error.
func (c *Client) ReleaseItem(wsSlug, ref, holder string) (bool, error) {
	var out leaseResponse
	err := c.post("/workspaces/"+url.PathEscape(wsSlug)+"/items/"+url.PathEscape(ref)+"/release",
		leaseRequest{Holder: holder}, &out)
	if err != nil {
		return false, err
	}
	return out.Released != nil && *out.Released, nil
}
