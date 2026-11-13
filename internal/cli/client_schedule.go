package cli

import (
	"encoding/json"
	"net/url"
	"strings"
)

// --- Item schedules and the agenda ---
//
// Each method returns the server's body as raw JSON so a field the server adds
// later is not dropped on the way to --format json.

func commitmentBase(wsSlug, ref string) string {
	return "/workspaces/" + url.PathEscape(wsSlug) + "/items/" + url.PathEscape(ref) + "/commitment"
}

// IsCommitmentRoute identifies commitment API paths for the CLI and MCP error
// contracts. The escaped path may include /api/v1, a mount prefix before it
// (a server URL such as https://host/pad), and a query string.
func IsCommitmentRoute(path string) bool {
	path, _, _ = strings.Cut(path, "?")
	// Slugs and refs are path-escaped, so the first /api/v1/ is the API root.
	if _, rest, ok := strings.Cut(path, "/api/v1/"); ok {
		path = "/" + rest
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) < 4 || parts[0] != "workspaces" || parts[1] == "" || parts[3] == "" {
		return false
	}
	if parts[2] == "items" {
		return len(parts) >= 5 && parts[4] == "commitment"
	}
	return len(parts) == 4 && (parts[2] == "commitment-links" || parts[2] == "commitment-gates")
}

// GetItemCommitment returns an item's schedule view.
func (c *Client) GetItemCommitment(wsSlug, ref string) (json.RawMessage, error) {
	var result json.RawMessage
	return result, c.get(commitmentBase(wsSlug, ref), &result)
}

// CreateItemCommitment attaches a schedule to an item.
func (c *Client) CreateItemCommitment(wsSlug, ref string, in map[string]any) (json.RawMessage, error) {
	var result json.RawMessage
	return result, c.post(commitmentBase(wsSlug, ref), in, &result)
}

// UpdateItemCommitment changes an item's schedule.
func (c *Client) UpdateItemCommitment(wsSlug, ref string, in map[string]any) (json.RawMessage, error) {
	var result json.RawMessage
	return result, c.patch(commitmentBase(wsSlug, ref), in, &result)
}

// DeleteItemCommitment removes an item's schedule and its managed reminder.
func (c *Client) DeleteItemCommitment(wsSlug, ref string) (json.RawMessage, error) {
	var result json.RawMessage
	return result, c.deleteWithResult(commitmentBase(wsSlug, ref), &result)
}

// ResolveItemCommitment resolves the outstanding occurrence.
func (c *Client) ResolveItemCommitment(wsSlug, ref string, in any) (json.RawMessage, error) {
	var result json.RawMessage
	return result, c.post(commitmentBase(wsSlug, ref)+"/resolve", in, &result)
}

// DismissEndedCommitment dismisses an ended schedule from Outstanding while
// its item stays open (TASK-4537).
func (c *Client) DismissEndedCommitment(wsSlug, ref string) (json.RawMessage, error) {
	var result json.RawMessage
	return result, c.post(commitmentBase(wsSlug, ref)+"/dismiss-ended", map[string]any{}, &result)
}

// RenegotiateItemCommitment previews (in.preview true) or applies a deadline
// change. A preview answers {preview, revision}; an apply answers the schedule view.
func (c *Client) RenegotiateItemCommitment(wsSlug, ref string, in map[string]any) (json.RawMessage, error) {
	var result json.RawMessage
	return result, c.post(commitmentBase(wsSlug, ref)+"/renegotiate", in, &result)
}

// GetAgenda returns the workspace agenda for [from, to). tz is optional.
func (c *Client) GetAgenda(wsSlug, from, to, tz string) (json.RawMessage, error) {
	q := url.Values{"from": {from}, "to": {to}}
	if tz != "" {
		q.Set("tz", tz)
	}
	var result json.RawMessage
	return result, c.get("/workspaces/"+url.PathEscape(wsSlug)+"/agenda?"+q.Encode(), &result)
}

// GetStaleAgenda returns the occurrences unresolved for more than 14 days, oldest
// first (TASK-4237). from (an RFC3339 instant) and tz are optional.
func (c *Client) GetStaleAgenda(wsSlug, from, tz string) (json.RawMessage, error) {
	q := url.Values{"stale": {"true"}}
	if from != "" {
		q.Set("from", from)
	}
	if tz != "" {
		q.Set("tz", tz)
	}
	var result json.RawMessage
	return result, c.get("/workspaces/"+url.PathEscape(wsSlug)+"/agenda?"+q.Encode(), &result)
}

// ListCommitmentLinks returns the readable links of an item.
func (c *Client) ListCommitmentLinks(ws, ref string) (json.RawMessage, error) {
	var raw json.RawMessage
	return raw, c.get(commitmentBase(ws, ref)+"/links", &raw)
}

// AddCommitmentLink previews or creates a link from the URL's parent item.
func (c *Client) AddCommitmentLink(ws, ref string, body map[string]any) (json.RawMessage, error) {
	var raw json.RawMessage
	return raw, c.post(commitmentBase(ws, ref)+"/links", body, &raw)
}

func commitmentGraphPath(ws, kind, id string) string {
	return "/workspaces/" + url.PathEscape(ws) + "/commitment-" + kind + "/" + url.PathEscape(id)
}

// EditCommitmentLink previews or applies an offset or cancellation policy edit.
func (c *Client) EditCommitmentLink(ws, id string, body map[string]any) (json.RawMessage, error) {
	var raw json.RawMessage
	return raw, c.patch(commitmentGraphPath(ws, "links", id), body, &raw)
}

// RemoveCommitmentLink removes a link without deleting either commitment.
func (c *Client) RemoveCommitmentLink(ws, id string) (json.RawMessage, error) {
	return nil, c.delete(commitmentGraphPath(ws, "links", id))
}

// MoveLinkedCommitment previews or applies a cascade of linked dates.
func (c *Client) MoveLinkedCommitment(ws, ref string, preview bool, body map[string]any) (json.RawMessage, error) {
	path := "/move"
	if preview {
		path = "/move-preview"
	}
	var raw json.RawMessage
	return raw, c.post(commitmentBase(ws, ref)+path, body, &raw)
}

// ListCommitmentGates returns the item's gate state and readable blockers.
func (c *Client) ListCommitmentGates(ws, ref string) (json.RawMessage, error) {
	var raw json.RawMessage
	return raw, c.get(commitmentBase(ws, ref)+"/gates", &raw)
}

// AddCommitmentGate waits for the blocker to succeed.
func (c *Client) AddCommitmentGate(ws, ref, blocker string) (json.RawMessage, error) {
	var raw json.RawMessage
	return raw, c.post(commitmentBase(ws, ref)+"/gates", map[string]any{"blocker": blocker}, &raw)
}

// RemoveCommitmentGate removes the ordering constraint.
func (c *Client) RemoveCommitmentGate(ws, id string) (json.RawMessage, error) {
	return nil, c.delete(commitmentGraphPath(ws, "gates", id))
}
