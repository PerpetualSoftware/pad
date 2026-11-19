package cli

import (
	"encoding/json"
	"net/url"
	"strconv"
)

// ListNeeds reads the caller's cross-workspace needs-you inbox
// (GET /me/needs, TASK-3920). Empty arguments are omitted, so the server's
// defaults apply; workspace narrows the inbox to one workspace.
func (c *Client) ListNeeds(view, workspace, cursor string, limit int) (json.RawMessage, error) {
	q := url.Values{}
	if view != "" {
		q.Set("view", view)
	}
	if workspace != "" {
		q.Set("workspace", workspace)
	}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	path := "/me/needs"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var result json.RawMessage
	err := c.get(path, &result)
	return result, err
}

func needsItemPath(ws, ref, kind string) string {
	return "/workspaces/" + url.PathEscape(ws) + "/items/" + url.PathEscape(ref) + "/needs/" + kind
}

// NotMineNeeds marks the item not mine for the caller (TASK-4643). The body is
// empty: with no marker the server records the item as it is now, and the
// source comes from how the request authenticated, never from the body.
func (c *Client) NotMineNeeds(ws, ref string) (json.RawMessage, error) {
	var result json.RawMessage
	err := c.put(needsItemPath(ws, ref, "not-mine"), map[string]any{}, &result)
	return result, err
}

// UnNotMineNeeds removes the caller's not-mine mark. Idempotent.
func (c *Client) UnNotMineNeeds(ws, ref string) (json.RawMessage, error) {
	var result json.RawMessage
	err := c.deleteWithResult(needsItemPath(ws, ref, "not-mine"), &result)
	return result, err
}

// UnsnoozeNeeds removes the caller's dismissal or deferral. Idempotent.
func (c *Client) UnsnoozeNeeds(ws, ref string) (json.RawMessage, error) {
	var result json.RawMessage
	err := c.deleteWithResult(needsItemPath(ws, ref, "snooze"), &result)
	return result, err
}
