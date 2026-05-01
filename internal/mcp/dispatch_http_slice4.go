package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/PerpetualSoftware/pad/internal/collections"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// --- project standup ---

// dispatchProjectStandup reproduces `pad project standup --format
// json`: fetches the dashboard for blockers + suggested-next, lists
// items in each terminal status to find recently completed work,
// lists in-progress items separately. Mirrors cmd/pad/main.go's
// standupCmd JSON branch exactly:
//
//	{
//	  "date":          "YYYY-MM-DD",
//	  "days":          N,
//	  "completed":     [{ref, title, status},  ...],
//	  "in_progress":   [{ref, title, priority}, ...],
//	  "blockers":      [{title, reason},        ...],
//	  "suggested_next":[{title, reason},        ...]
//	}
//
// The CLI defaults --days to 1; we apply that default in-dispatcher
// since cmdhelp doesn't carry a default forward to MCP property
// schemas (registry.go strips them). Without the default, a missing
// `days` input would zero-out the cutoff and report nothing as
// "completed", surprising agents that omit the flag.
func (d *HTTPHandlerDispatcher) dispatchProjectStandup(
	ctx context.Context,
	input map[string]any,
	user *models.User,
) (*mcp.CallToolResult, error) {
	const cmdKey = "project standup"
	workspace, _ := input["workspace"].(string)
	if workspace == "" {
		return mcp.NewToolResultErrorf("%s: workspace is required", cmdKey), nil
	}

	days := 1
	if n, ok := numericInput(input["days"]); ok && n > 0 {
		days = int(n)
	}
	cutoff := time.Now().AddDate(0, 0, -days)

	dash, errRes := d.fetchDashboardJSON(ctx, input, user, cmdKey)
	if errRes != nil {
		return errRes, nil
	}

	// One list call per terminal status (matches the CLI's loop —
	// avoids OR-ing all statuses into a single query, which would
	// hit the comma-separated-status path that the broader
	// defaultActiveStatusFilter also relies on but would here treat
	// as "any of these statuses"). Filter by cutoff client-side.
	var completed []map[string]any
	for _, status := range models.DefaultTerminalStatuses {
		items, err := d.listWorkspaceItems(ctx, user, workspace, url.Values{
			"status": {status},
			"sort":   {"updated_at:desc"},
			"limit":  {"20"},
		})
		if err != nil {
			// Match the CLI: per-status errors don't abort the whole
			// command — best-effort accumulation across statuses.
			continue
		}
		for _, item := range items {
			if itemUpdatedAfter(item, cutoff) {
				completed = append(completed, item)
			}
		}
	}

	inProgress, err := d.listWorkspaceItems(ctx, user, workspace, url.Values{
		"status": {"in-progress"},
		"sort":   {"updated_at:desc"},
	})
	if err != nil {
		// Match the CLI: in-progress fetch failures are tolerated;
		// the rest of the standup still ships.
		inProgress = nil
	}

	type standupItem struct {
		Ref      string `json:"ref"`
		Title    string `json:"title"`
		Status   string `json:"status,omitempty"`
		Priority string `json:"priority,omitempty"`
		Reason   string `json:"reason,omitempty"`
	}
	completedOut := make([]standupItem, 0, len(completed))
	for _, item := range completed {
		completedOut = append(completedOut, standupItem{
			Ref:    itemRefFromMap(item),
			Title:  itemTitleFromMap(item),
			Status: extractItemFieldString(item, "status"),
		})
	}
	inProgressOut := make([]standupItem, 0, len(inProgress))
	for _, item := range inProgress {
		inProgressOut = append(inProgressOut, standupItem{
			Ref:      itemRefFromMap(item),
			Title:    itemTitleFromMap(item),
			Priority: extractItemFieldString(item, "priority"),
		})
	}

	blockers := make([]standupItem, 0)
	for _, a := range dashboardArrayField(dash, "attention") {
		blockers = append(blockers, standupItem{
			Title:  stringFromMap(a, "item_title"),
			Reason: stringFromMap(a, "reason"),
		})
	}
	suggested := make([]standupItem, 0)
	for _, s := range dashboardArrayField(dash, "suggested_next") {
		suggested = append(suggested, standupItem{
			Title:  stringFromMap(s, "item_title"),
			Reason: stringFromMap(s, "reason"),
		})
	}

	payload := map[string]any{
		"date":           time.Now().Format("2006-01-02"),
		"days":           days,
		"completed":      completedOut,
		"in_progress":    inProgressOut,
		"blockers":       blockers,
		"suggested_next": suggested,
	}
	return packageStructuredResponse(cmdKey, payload)
}

// --- project changelog ---

// dispatchProjectChangelog reproduces `pad project changelog
// --format json`: lists items in each terminal status, filters by
// date (--since YYYY-MM-DD takes precedence over --days), filters by
// parent ref/slug/title, then groups by collection.
//
//	{
//	  "period": "<human label>",
//	  "since":  "YYYY-MM-DD",
//	  "total":  N,
//	  "groups": [{collection, icon?, count, items:[{ref,title,status}]}]
//	}
//
// CLI defaults: --days 7, --since "" (overrides days), --parent "".
// The dispatcher applies the same defaults in-process so MCP agents
// don't need to know the cmdhelp-stripped defaults.
func (d *HTTPHandlerDispatcher) dispatchProjectChangelog(
	ctx context.Context,
	input map[string]any,
	user *models.User,
) (*mcp.CallToolResult, error) {
	const cmdKey = "project changelog"
	workspace, _ := input["workspace"].(string)
	if workspace == "" {
		return mcp.NewToolResultErrorf("%s: workspace is required", cmdKey), nil
	}

	since, _ := input["since"].(string)
	since = strings.TrimSpace(since)
	days := 7
	if n, ok := numericInput(input["days"]); ok && n > 0 {
		days = int(n)
	}

	var cutoff time.Time
	if since != "" {
		parsed, err := time.Parse("2006-01-02", since)
		if err != nil {
			return mcp.NewToolResultErrorf(
				"%s: invalid --since date %q (expected YYYY-MM-DD): %s",
				cmdKey, since, err.Error(),
			), nil
		}
		cutoff = parsed
	} else {
		cutoff = time.Now().AddDate(0, 0, -days)
	}

	parentFilter, _ := input["parent"].(string)
	parentFilter = strings.TrimSpace(parentFilter)

	var allItems []map[string]any
	for _, status := range models.DefaultTerminalStatuses {
		items, err := d.listWorkspaceItems(ctx, user, workspace, url.Values{
			"status": {status},
			"sort":   {"updated_at:desc"},
			"limit":  {"100"},
		})
		if err != nil {
			continue
		}
		for _, item := range items {
			if !itemUpdatedAfter(item, cutoff) {
				continue
			}
			if parentFilter != "" && !itemMatchesParent(item, parentFilter) {
				continue
			}
			allItems = append(allItems, item)
		}
	}

	// Group by collection slug, preserving first-seen ordering so
	// the output is stable across runs (matches the CLI's
	// groupOrder slice).
	type collectionGroup struct {
		Name  string
		Icon  string
		Items []map[string]any
	}
	groupOrder := []string{}
	groups := map[string]*collectionGroup{}
	for _, item := range allItems {
		key := stringFromMap(item, "collection_slug")
		if key == "" {
			key = "other"
		}
		if _, exists := groups[key]; !exists {
			name := stringFromMap(item, "collection_name")
			if name == "" {
				name = key
			}
			groups[key] = &collectionGroup{
				Name: name,
				Icon: stringFromMap(item, "collection_icon"),
			}
			groupOrder = append(groupOrder, key)
		}
		groups[key].Items = append(groups[key].Items, item)
	}

	periodLabel := fmt.Sprintf("last %d days", days)
	if since != "" {
		periodLabel = "since " + since
	}
	if parentFilter != "" {
		periodLabel += " (parent: " + parentFilter + ")"
	}

	type changelogItem struct {
		Ref    string `json:"ref"`
		Title  string `json:"title"`
		Status string `json:"status"`
	}
	type changelogGroup struct {
		Collection string          `json:"collection"`
		Icon       string          `json:"icon,omitempty"`
		Count      int             `json:"count"`
		Items      []changelogItem `json:"items"`
	}

	outGroups := make([]changelogGroup, 0, len(groupOrder))
	for _, key := range groupOrder {
		g := groups[key]
		cg := changelogGroup{
			Collection: g.Name,
			Icon:       g.Icon,
			Count:      len(g.Items),
			Items:      make([]changelogItem, 0, len(g.Items)),
		}
		for _, item := range g.Items {
			cg.Items = append(cg.Items, changelogItem{
				Ref:    itemRefFromMap(item),
				Title:  itemTitleFromMap(item),
				Status: extractItemFieldString(item, "status"),
			})
		}
		outGroups = append(outGroups, cg)
	}

	payload := map[string]any{
		"period": periodLabel,
		"since":  cutoff.Format("2006-01-02"),
		"total":  len(allItems),
		"groups": outGroups,
	}
	return packageStructuredResponse(cmdKey, payload)
}

// listWorkspaceItems issues an in-process GET against
// /api/v1/workspaces/{ws}/items with the supplied query params and
// returns the response decoded into []map[string]any. Used by
// standup/changelog where iterating per-status keeps the dispatcher
// from having to re-decode into a typed slice for every call.
//
// Returns a non-nil error on 4xx/5xx — the caller decides whether
// to swallow (per-status best-effort, matching the CLI) or surface.
func (d *HTTPHandlerDispatcher) listWorkspaceItems(
	ctx context.Context,
	user *models.User,
	workspace string,
	values url.Values,
) ([]map[string]any, error) {
	path := "/api/v1/workspaces/" + url.PathEscape(workspace) + "/items"
	if encoded := values.Encode(); encoded != "" {
		path += "?" + encoded
	}
	req, err := d.buildAuthedRequest(ctx, http.MethodGet, path, nil, user)
	if err != nil {
		return nil, fmt.Errorf("build items request: %w", err)
	}
	rec := httptest.NewRecorder()
	d.Handler.ServeHTTP(rec, req)
	if rec.Code >= 400 {
		body := strings.TrimSpace(rec.Body.String())
		if body == "" {
			body = http.StatusText(rec.Code)
		}
		return nil, fmt.Errorf("list items: %d %s", rec.Code, body)
	}
	var items []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil {
		return nil, fmt.Errorf("parse items response: %w", err)
	}
	return items, nil
}

// itemUpdatedAfter compares an item's updated_at against cutoff,
// returning true if the item is newer. The CLI parses updated_at as
// RFC3339; we accept the same format. Items with malformed or
// missing timestamps are excluded so a stale or partial response
// can't fall through and pollute "completed" with everything.
func itemUpdatedAfter(item map[string]any, cutoff time.Time) bool {
	ts := stringFromMap(item, "updated_at")
	if ts == "" {
		return false
	}
	parsed, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		// Accept the alternate "no fractional seconds" RFC3339Nano
		// shape that some Go encoders emit. If this also fails,
		// the item is excluded.
		parsed, err = time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			return false
		}
	}
	return parsed.After(cutoff)
}

// itemMatchesParent mirrors the CLI's parent-filter matching:
// case-insensitive comparison against the item's parent_link_id,
// parent_ref, OR parent_title. Useful so an agent can pass either a
// UUID, an issue ref like "PLAN-3", or the human-readable title and
// have the filter match.
func itemMatchesParent(item map[string]any, parent string) bool {
	for _, key := range []string{"parent_link_id", "parent_ref", "parent_title"} {
		if v := stringFromMap(item, key); v != "" && strings.EqualFold(v, parent) {
			return true
		}
	}
	return false
}

// extractItemFieldString pulls a value out of an item's `fields`
// JSON-string. Mirrors cmd/pad/main.go's extractFieldFromJSON helper.
// Used by standup/changelog to surface status / priority into the
// flattened output without forcing the agent to parse the fields
// blob themselves.
func extractItemFieldString(item map[string]any, key string) string {
	raw := stringFromMap(item, "fields")
	if raw == "" || raw == "{}" {
		return ""
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return ""
	}
	if v, ok := fields[key].(string); ok {
		return v
	}
	return ""
}

// itemRefFromMap returns "TASK-5"-style refs for items that carry
// collection_prefix + item_number, matching cli.ItemRef. Used in
// changelog/standup output where the user-visible identifier is
// preferred over the slug.
func itemRefFromMap(item map[string]any) string {
	prefix := stringFromMap(item, "collection_prefix")
	if prefix == "" {
		return ""
	}
	switch v := item["item_number"].(type) {
	case nil:
		return ""
	case float64:
		return fmt.Sprintf("%s-%d", prefix, int64(v))
	case int:
		return fmt.Sprintf("%s-%d", prefix, v)
	case int64:
		return fmt.Sprintf("%s-%d", prefix, v)
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return ""
		}
		return fmt.Sprintf("%s-%d", prefix, n)
	case string:
		// Some encoders stringify integers; treat empty as missing.
		if v == "" {
			return ""
		}
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return fmt.Sprintf("%s-%d", prefix, n)
		}
		return ""
	default:
		return ""
	}
}

// itemTitleFromMap returns the item's human-readable title, falling
// back to slug if title is unset (matches CLI behaviour for items
// that may have only a slug populated in legacy paths).
func itemTitleFromMap(item map[string]any) string {
	if t := stringFromMap(item, "title"); t != "" {
		return t
	}
	return stringFromMap(item, "slug")
}

// stringFromMap is a tiny helper that does the type-assert dance.
// Returns the empty string for missing keys or non-string values so
// callers can compose conditionals cleanly without nested asserts.
func stringFromMap(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// --- library activate ---

// dispatchLibraryActivate reproduces `pad library activate <title>`:
// looks up a library entry by title (conventions first, then
// playbooks — same precedence the CLI uses), builds the right
// fields blob, and POSTs an item into the workspace's
// conventions/playbooks collection.
//
// Library data is sourced from internal/collections directly rather
// than via the /convention-library / /playbook-library endpoints.
// Both paths return the same data (the handlers wrap the same
// constants), and the in-process accessor avoids two extra HTTP
// round-trips per activate. The OAuth-scope hook (d.Apply) still
// runs on the eventual POST, so this isn't a scope bypass.
//
// Two minor divergences from the CLI:
//
//   - The CLI uses `models.BuildConventionItemFields` for
//     conventions (deals with surfaces/enforcement/commands metadata)
//     but builds the playbook fields by hand. We match exactly.
//   - The CLI's "conventions" / "playbooks" target collection slugs
//     are hardcoded; we do the same. Workspaces from non-software
//     templates may not have these collections, in which case the
//     POST will 404 — same UX the CLI delivers.
func (d *HTTPHandlerDispatcher) dispatchLibraryActivate(
	ctx context.Context,
	input map[string]any,
	user *models.User,
) (*mcp.CallToolResult, error) {
	const cmdKey = "library activate"
	workspace, _ := input["workspace"].(string)
	if workspace == "" {
		return mcp.NewToolResultErrorf("%s: workspace is required", cmdKey), nil
	}
	title, _ := input["title"].(string)
	if title == "" {
		return mcp.NewToolResultErrorf("%s: title is required", cmdKey), nil
	}

	if conv := collections.GetLibraryConvention(title); conv != nil {
		fieldsJSON, err := models.BuildConventionItemFields("active", &models.ItemConventionMetadata{
			Category:    conv.Category,
			Trigger:     conv.Trigger,
			Surfaces:    conv.Surfaces,
			Enforcement: conv.Enforcement,
			Commands:    conv.Commands,
		})
		if err != nil {
			return mcp.NewToolResultErrorf("%s: build convention fields: %s", cmdKey, err.Error()), nil
		}
		return d.postLibraryItem(ctx, user, workspace, "conventions", cmdKey, conv.Title, conv.Content, fieldsJSON)
	}

	if pb := collections.GetLibraryPlaybook(title); pb != nil {
		fields := map[string]any{
			"status":  "active",
			"trigger": pb.Trigger,
			"scope":   pb.Scope,
		}
		fieldsJSON, err := json.Marshal(fields)
		if err != nil {
			return mcp.NewToolResultErrorf("%s: encode playbook fields: %s", cmdKey, err.Error()), nil
		}
		return d.postLibraryItem(ctx, user, workspace, "playbooks", cmdKey, pb.Title, pb.Content, string(fieldsJSON))
	}

	return mcp.NewToolResultErrorf(
		"%s: not found in convention or playbook library: %q", cmdKey, title,
	), nil
}

// postLibraryItem POSTs an ItemCreate body into the named
// collection's items endpoint. Shared between conventions /
// playbooks branches of dispatchLibraryActivate so the URL +
// envelope shape stays in lockstep.
func (d *HTTPHandlerDispatcher) postLibraryItem(
	ctx context.Context,
	user *models.User,
	workspace, collection, cmdKey, title, content, fieldsJSON string,
) (*mcp.CallToolResult, error) {
	payload := map[string]any{
		"title":  title,
		"fields": fieldsJSON,
	}
	if content != "" {
		payload["content"] = content
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return mcp.NewToolResultErrorf("%s: encode body: %s", cmdKey, err.Error()), nil
	}
	urlPath := "/api/v1/workspaces/" + url.PathEscape(workspace) +
		"/collections/" + url.PathEscape(collection) + "/items"
	return d.executeRequest(ctx, cmdKey, user, http.MethodPost, urlPath, body)
}
