package cli

import (
	"encoding/json"
	"net/url"
)

// --- Item schedules and the agenda ---
//
// Each method returns the server's body as raw JSON so a field the server adds
// later is not dropped on the way to --format json.

func commitmentBase(wsSlug, ref string) string {
	return "/workspaces/" + url.PathEscape(wsSlug) + "/items/" + url.PathEscape(ref) + "/commitment"
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
