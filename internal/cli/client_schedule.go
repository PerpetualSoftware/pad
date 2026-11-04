package cli

import (
	"encoding/json"
	"net/url"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// --- Item schedules and the agenda ---
//
// Each method returns the server's body as raw JSON so a field the server adds
// later is not dropped on the way to --format json.

func scheduleBase(wsSlug, ref string) string {
	return "/workspaces/" + url.PathEscape(wsSlug) + "/items/" + url.PathEscape(ref) + "/schedule"
}

// ScheduleResolve is the body of POST .../schedule/resolve.
type ScheduleResolve struct {
	OccurrenceAt string `json:"occurrence_at"`
	State        string `json:"state"`
	Comment      string `json:"comment,omitempty"`
	Until        string `json:"until,omitempty"`
}

// GetItemSchedule returns an item's schedule view.
func (c *Client) GetItemSchedule(wsSlug, ref string) (json.RawMessage, error) {
	var result json.RawMessage
	return result, c.get(scheduleBase(wsSlug, ref), &result)
}

// CreateItemSchedule attaches a schedule to an item.
func (c *Client) CreateItemSchedule(wsSlug, ref string, in models.ScheduleCreate) (json.RawMessage, error) {
	var result json.RawMessage
	return result, c.post(scheduleBase(wsSlug, ref), in, &result)
}

// UpdateItemSchedule changes an item's schedule.
func (c *Client) UpdateItemSchedule(wsSlug, ref string, in models.ScheduleUpdate) (json.RawMessage, error) {
	var result json.RawMessage
	return result, c.patch(scheduleBase(wsSlug, ref), in, &result)
}

// DeleteItemSchedule removes an item's schedule and its managed reminder.
func (c *Client) DeleteItemSchedule(wsSlug, ref string) (json.RawMessage, error) {
	var result json.RawMessage
	return result, c.deleteWithResult(scheduleBase(wsSlug, ref), &result)
}

// ResolveItemSchedule resolves the outstanding occurrence.
func (c *Client) ResolveItemSchedule(wsSlug, ref string, in ScheduleResolve) (json.RawMessage, error) {
	var result json.RawMessage
	return result, c.post(scheduleBase(wsSlug, ref)+"/resolve", in, &result)
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
