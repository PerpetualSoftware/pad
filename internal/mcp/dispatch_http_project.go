package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// dispatchProjectReady reproduces `pad project ready --format json` —
// the CLI returns `{count, results}` extracted from the dashboard's
// SuggestedNext slice, NOT the full dashboard payload. (Compare with
// `pad project next` which returns the raw dashboard JSON; both surface
// the same suggestions but with different framing.)
//
// Aliasing to /dashboard would be a behavioural divergence: the
// agent would see an unexpected wrapper shape and have to know to dig
// into `suggested_next`. Mirroring the CLI's `{count, results}` shape
// keeps the MCP transport equivalent to ExecDispatcher.
func (d *HTTPHandlerDispatcher) dispatchProjectReady(
	ctx context.Context,
	input map[string]any,
	user *models.User,
) (*mcp.CallToolResult, error) {
	const cmdKey = "project ready"
	dash, errRes := d.fetchDashboardJSON(ctx, input, user, cmdKey)
	if errRes != nil {
		return errRes, nil
	}
	suggestions := dashboardArrayField(dash, "suggested_next")
	return packageStructuredResponse(cmdKey, map[string]any{
		"count":   len(suggestions),
		"results": suggestions,
	})
}

// dispatchProjectStale reproduces `pad project stale --format json` —
// CLI filters the dashboard's Attention slice to "interesting" types
// (stalled / blocked / overdue / orphaned_task) before returning
// `{count, results}`. Sorting matches cmd/pad/query.go's
// filterAgentAttention: type, ItemRef, ItemTitle.
//
// Operates on the raw map[string]any decoded from the dashboard JSON
// so any field server.DashboardAttention adds in future versions
// (collection, plus anything not yet wired) flows through unchanged.
// Codex review on PR #348 round 1 caught the previous typed-struct
// approach dropping `collection` from the response.
func (d *HTTPHandlerDispatcher) dispatchProjectStale(
	ctx context.Context,
	input map[string]any,
	user *models.User,
) (*mcp.CallToolResult, error) {
	const cmdKey = "project stale"
	dash, errRes := d.fetchDashboardJSON(ctx, input, user, cmdKey)
	if errRes != nil {
		return errRes, nil
	}
	attention := filterAgentAttention(dashboardArrayField(dash, "attention"))
	return packageStructuredResponse(cmdKey, map[string]any{
		"count":   len(attention),
		"results": attention,
	})
}

// fetchDashboardJSON hits the workspace dashboard endpoint and decodes
// the response into a generic map[string]any so the dispatcher
// preserves every field the server emits — no maintenance burden when
// new fields land on DashboardAttention / DashboardSuggestion.
//
// Returns the raw object so callers can pull specific fields
// (suggested_next, attention) via dashboardArrayField without the
// typed-struct round-trip.
func (d *HTTPHandlerDispatcher) fetchDashboardJSON(
	ctx context.Context,
	input map[string]any,
	user *models.User,
	cmdKey string,
) (map[string]any, *mcp.CallToolResult) {
	workspace, _ := input["workspace"].(string)
	if workspace == "" {
		return nil, mcp.NewToolResultErrorf("%s: workspace is required", cmdKey)
	}
	path := "/api/v1/workspaces/" + url.PathEscape(workspace) + "/dashboard"
	req, err := d.buildAuthedRequest(ctx, http.MethodGet, path, nil, user)
	if err != nil {
		return nil, mcp.NewToolResultErrorf("%s: build dashboard request: %s", cmdKey, err.Error())
	}
	rec := httptest.NewRecorder()
	d.Handler.ServeHTTP(rec, req)
	if rec.Code >= 400 {
		body := strings.TrimSpace(rec.Body.String())
		if body == "" {
			body = http.StatusText(rec.Code)
		}
		return nil, mcp.NewToolResultErrorf("%s: %d %s", cmdKey, rec.Code, body)
	}
	var dash map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &dash); err != nil {
		return nil, mcp.NewToolResultErrorf("%s: parse dashboard: %s", cmdKey, err.Error())
	}
	return dash, nil
}

// dashboardArrayField pulls a named array out of a decoded dashboard
// payload, returning a typed []map[string]any so the callers can
// filter / sort by string fields without the json.Number / interface{}
// dance per element. Missing/empty/non-array values normalize to an
// empty slice so the {count, results} responses always emit a usable
// shape.
func dashboardArrayField(dash map[string]any, key string) []map[string]any {
	raw, ok := dash[key].([]any)
	if !ok {
		return []map[string]any{}
	}
	out := make([]map[string]any, 0, len(raw))
	for _, e := range raw {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// filterAgentAttention mirrors cmd/pad/query.go's helper of the same
// name — keeps only the attention types agents care about (stalled,
// blocked, overdue, orphaned_task) and sorts deterministically by
// (type, item_ref, item_title). Same stable ordering as the CLI so
// `--format json` outputs match between transports.
//
// Operates on map[string]any (not a typed struct) so attention
// entries pass through to the response with EVERY field the server
// emitted, not just the ones we knew to declare. Codex review on PR
// #348 caught the previous typed approach dropping `collection`.
func filterAgentAttention(attention []map[string]any) []map[string]any {
	interesting := map[string]bool{
		"stalled":       true,
		"blocked":       true,
		"overdue":       true,
		"orphaned_task": true,
	}
	results := make([]map[string]any, 0, len(attention))
	for _, item := range attention {
		typ, _ := item["type"].(string)
		if interesting[typ] {
			results = append(results, item)
		}
	}
	sort.SliceStable(results, func(i, j int) bool {
		ti, _ := results[i]["type"].(string)
		tj, _ := results[j]["type"].(string)
		if ti != tj {
			return ti < tj
		}
		ri, _ := results[i]["item_ref"].(string)
		rj, _ := results[j]["item_ref"].(string)
		if ri != rj {
			return ri < rj
		}
		titI, _ := results[i]["item_title"].(string)
		titJ, _ := results[j]["item_title"].(string)
		return titI < titJ
	})
	return results
}

// --- item bulk-update ---

// dispatchItemBulkUpdate iterates the input's `ref` array and applies
// --status / --priority via the same read-modify-write semantics the
// item.update path uses (so existing fields survive). Mirrors the
// CLI's bulkUpdateCmd: at-least-one-of-status-or-priority gating, per-
// item GET → field merge → PATCH, and a per-item success/error report.
//
// The cmdhelp surface marks `ref` as required AND repeatable — agents
// pass it as either []any (typical JSON array) or []string. Anything
// else is rejected so the dispatcher doesn't silently iterate over
// nothing.
//
// Per-item failures don't abort the bulk operation; they get
// individually reported in the response so an agent can inspect what
// succeeded vs. failed without having to retry the whole batch.
func (d *HTTPHandlerDispatcher) dispatchItemBulkUpdate(
	ctx context.Context,
	input map[string]any,
	user *models.User,
) (*mcp.CallToolResult, error) {
	const cmdKey = "item bulk-update"

	workspace, _ := input["workspace"].(string)
	if workspace == "" {
		return mcp.NewToolResultErrorf("%s: workspace is required", cmdKey), nil
	}

	refs, err := bulkUpdateRefs(input["ref"])
	if err != nil {
		return mcp.NewToolResultErrorf("%s: %s", cmdKey, err.Error()), nil
	}
	if len(refs) == 0 {
		return mcp.NewToolResultErrorf("%s: at least one ref is required", cmdKey), nil
	}

	status, _ := input["status"].(string)
	priority, _ := input["priority"].(string)
	if status == "" && priority == "" {
		return mcp.NewToolResultErrorf("%s: at least one of --status or --priority is required", cmdKey), nil
	}

	type bulkResult struct {
		Ref     string `json:"ref"`
		Updated bool   `json:"updated"`
		Error   string `json:"error,omitempty"`
	}
	results := make([]bulkResult, 0, len(refs))
	successes := 0

	for _, ref := range refs {
		// Per-item RMW: GET, merge fields, PATCH. Same shape
		// dispatchItemUpdate uses, but inlined here so a per-item
		// failure produces a {ref, error} entry instead of aborting.
		itemPath := "/api/v1/workspaces/" + url.PathEscape(workspace) +
			"/items/" + url.PathEscape(ref)

		getReq, err := d.buildAuthedRequest(ctx, http.MethodGet, itemPath, nil, user)
		if err != nil {
			results = append(results, bulkResult{Ref: ref, Error: fmt.Sprintf("build request: %s", err.Error())})
			continue
		}
		getRec := httptest.NewRecorder()
		d.Handler.ServeHTTP(getRec, getReq)
		if getRec.Code >= 400 {
			results = append(results, bulkResult{
				Ref:   ref,
				Error: fmt.Sprintf("read item: %d %s", getRec.Code, strings.TrimSpace(getRec.Body.String())),
			})
			continue
		}
		var existing struct {
			Fields string `json:"fields"`
		}
		if err := json.Unmarshal(getRec.Body.Bytes(), &existing); err != nil {
			results = append(results, bulkResult{Ref: ref, Error: fmt.Sprintf("parse item: %s", err.Error())})
			continue
		}

		merged := map[string]any{}
		if existing.Fields != "" && existing.Fields != "{}" {
			if err := json.Unmarshal([]byte(existing.Fields), &merged); err != nil {
				results = append(results, bulkResult{Ref: ref, Error: fmt.Sprintf("parse existing fields: %s", err.Error())})
				continue
			}
		}
		if status != "" {
			merged["status"] = status
		}
		if priority != "" {
			merged["priority"] = priority
		}
		fieldsJSON, err := json.Marshal(merged)
		if err != nil {
			results = append(results, bulkResult{Ref: ref, Error: fmt.Sprintf("encode fields: %s", err.Error())})
			continue
		}
		fieldsStr := string(fieldsJSON)
		patchBody, err := json.Marshal(map[string]any{"fields": fieldsStr})
		if err != nil {
			results = append(results, bulkResult{Ref: ref, Error: fmt.Sprintf("encode body: %s", err.Error())})
			continue
		}

		patchReq, err := d.buildAuthedRequest(ctx, http.MethodPatch, itemPath, patchBody, user)
		if err != nil {
			results = append(results, bulkResult{Ref: ref, Error: fmt.Sprintf("build PATCH: %s", err.Error())})
			continue
		}
		patchRec := httptest.NewRecorder()
		d.Handler.ServeHTTP(patchRec, patchReq)
		if patchRec.Code >= 400 {
			results = append(results, bulkResult{
				Ref:   ref,
				Error: fmt.Sprintf("update: %d %s", patchRec.Code, strings.TrimSpace(patchRec.Body.String())),
			})
			continue
		}

		results = append(results, bulkResult{Ref: ref, Updated: true})
		successes++
	}

	payload := map[string]any{
		"updated": successes,
		"total":   len(refs),
		"results": results,
	}
	return packageStructuredResponse(cmdKey, payload)
}

// bulkUpdateRefs canonicalizes the `ref` input — accepts repeatable
// shapes the cmdhelp registry generates (string for a single value,
// []any from JSON arrays, []string from typed callers) into a clean
// []string. Empty / non-string entries are rejected so we don't
// silently skip elements an agent expected to be processed.
func bulkUpdateRefs(raw any) ([]string, error) {
	switch v := raw.(type) {
	case nil:
		return nil, nil
	case string:
		if v == "" {
			return nil, nil
		}
		return []string{v}, nil
	case []string:
		out := make([]string, 0, len(v))
		for i, s := range v {
			if s == "" {
				return nil, fmt.Errorf("ref[%d] is empty", i)
			}
			out = append(out, s)
		}
		return out, nil
	case []any:
		out := make([]string, 0, len(v))
		for i, e := range v {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("ref[%d] must be a string, got %T", i, e)
			}
			if s == "" {
				return nil, fmt.Errorf("ref[%d] is empty", i)
			}
			out = append(out, s)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("ref must be a string or array of strings, got %T", raw)
	}
}

// --- item note + decide (RMW append) ---

// dispatchItemNote handles `pad item note <ref> <summary>
// [--details ...]` — appends an implementation-note entry to the
// item's structured-fields blob, then PATCHes.
//
// Same RMW shape as dispatchItemUpdate but using
// models.AppendImplementationNote so the entry gets the right shape
// + ID + timestamp the CLI applies.
//
// Emits the updated item (the PATCH response) like every other
// dispatcher — agents see the same shape they'd get from a follow-up
// `item show`.
func (d *HTTPHandlerDispatcher) dispatchItemNote(
	ctx context.Context,
	input map[string]any,
	user *models.User,
) (*mcp.CallToolResult, error) {
	const cmdKey = "item note"

	workspace, _ := input["workspace"].(string)
	ref, _ := input["ref"].(string)
	summary, _ := input["summary"].(string)
	if workspace == "" {
		return mcp.NewToolResultErrorf("%s: workspace is required", cmdKey), nil
	}
	if ref == "" {
		return mcp.NewToolResultErrorf("%s: ref is required", cmdKey), nil
	}
	if summary == "" {
		return mcp.NewToolResultErrorf("%s: summary is required", cmdKey), nil
	}
	details, _ := input["details"].(string)
	details = strings.TrimSpace(details)

	itemPath := "/api/v1/workspaces/" + url.PathEscape(workspace) +
		"/items/" + url.PathEscape(ref)
	currentFields, errRes := d.prefetchItemFields(ctx, user, cmdKey, itemPath)
	if errRes != nil {
		return errRes, nil
	}

	updated, err := models.AppendImplementationNote(currentFields, models.ItemImplementationNote{
		ID:        newStructuredEntryID("note"),
		Summary:   strings.TrimSpace(summary),
		Details:   details,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		CreatedBy: userActorLabel(user),
	})
	if err != nil {
		return mcp.NewToolResultErrorf("%s: append note: %s", cmdKey, err.Error()), nil
	}

	body, err := json.Marshal(map[string]any{"fields": updated})
	if err != nil {
		return mcp.NewToolResultErrorf("%s: encode body: %s", cmdKey, err.Error()), nil
	}
	return d.executeRequest(ctx, cmdKey, user, http.MethodPatch, itemPath, body)
}

// dispatchItemDecide is the decision-log analogue of
// dispatchItemNote — same RMW shape, just using
// AppendDecisionLogEntry on a different fields slot.
func (d *HTTPHandlerDispatcher) dispatchItemDecide(
	ctx context.Context,
	input map[string]any,
	user *models.User,
) (*mcp.CallToolResult, error) {
	const cmdKey = "item decide"

	workspace, _ := input["workspace"].(string)
	ref, _ := input["ref"].(string)
	decision, _ := input["decision"].(string)
	if workspace == "" {
		return mcp.NewToolResultErrorf("%s: workspace is required", cmdKey), nil
	}
	if ref == "" {
		return mcp.NewToolResultErrorf("%s: ref is required", cmdKey), nil
	}
	if decision == "" {
		return mcp.NewToolResultErrorf("%s: decision is required", cmdKey), nil
	}
	rationale, _ := input["rationale"].(string)
	rationale = strings.TrimSpace(rationale)

	itemPath := "/api/v1/workspaces/" + url.PathEscape(workspace) +
		"/items/" + url.PathEscape(ref)
	currentFields, errRes := d.prefetchItemFields(ctx, user, cmdKey, itemPath)
	if errRes != nil {
		return errRes, nil
	}

	updated, err := models.AppendDecisionLogEntry(currentFields, models.ItemDecisionLogEntry{
		ID:        newStructuredEntryID("decision"),
		Decision:  strings.TrimSpace(decision),
		Rationale: rationale,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		CreatedBy: userActorLabel(user),
	})
	if err != nil {
		return mcp.NewToolResultErrorf("%s: append decision: %s", cmdKey, err.Error()), nil
	}

	body, err := json.Marshal(map[string]any{"fields": updated})
	if err != nil {
		return mcp.NewToolResultErrorf("%s: encode body: %s", cmdKey, err.Error()), nil
	}
	return d.executeRequest(ctx, cmdKey, user, http.MethodPatch, itemPath, body)
}

// prefetchItemFields GETs the item at itemPath and returns its
// `fields` JSON string. Surfaces 404s and parse errors as
// IsError-flagged tool results so the dispatcher's caller can return
// them directly without further wrapping.
//
// Used by note/decide which append into the existing fields blob —
// they need the current value so AppendImplementationNote /
// AppendDecisionLogEntry can preserve other entries.
func (d *HTTPHandlerDispatcher) prefetchItemFields(
	ctx context.Context,
	user *models.User,
	cmdKey, itemPath string,
) (string, *mcp.CallToolResult) {
	req, err := d.buildAuthedRequest(ctx, http.MethodGet, itemPath, nil, user)
	if err != nil {
		return "", mcp.NewToolResultErrorf("%s: build prefetch: %s", cmdKey, err.Error())
	}
	rec := httptest.NewRecorder()
	d.Handler.ServeHTTP(rec, req)
	if rec.Code >= 400 {
		body := strings.TrimSpace(rec.Body.String())
		if body == "" {
			body = http.StatusText(rec.Code)
		}
		return "", mcp.NewToolResultErrorf("%s: prefetch: %d %s", cmdKey, rec.Code, body)
	}
	var existing struct {
		Fields string `json:"fields"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &existing); err != nil {
		return "", mcp.NewToolResultErrorf("%s: parse current item: %s", cmdKey, err.Error())
	}
	return existing.Fields, nil
}

// newStructuredEntryID mirrors the CLI's helper for note/decision
// IDs (cmd/pad/notes.go). The actual collision-avoidance is handled
// by combining the prefix + a unix-nano timestamp — same shape so
// CLI-created and MCP-created entries are indistinguishable in
// downstream consumers.
func newStructuredEntryID(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UTC().UnixNano())
}

// userActorLabel produces a stable string label for the actor that
// created a structured entry. Mirrors the CLI's "user" label for
// CLI-driven entries; for MCP we use the requesting user's name (or
// email fallback) so audit-log review can tell who appended what
// when multiple users share the same MCP server.
func userActorLabel(user *models.User) string {
	if user == nil {
		return "user"
	}
	if user.Name != "" {
		return user.Name
	}
	if user.Email != "" {
		return user.Email
	}
	return "user"
}

// dispatchLibraryList composes the /convention-library and
// /playbook-library endpoints to mirror `pad library list --format
// json`. The CLI's JSON output shape varies on --type:
//
//   - --type conventions    → returns the convention library (lib).
//   - --type playbooks      → returns the playbook library (plib).
//   - (no --type)           → returns {conventions: lib, playbooks: plib}.
//
// `--category` is intentionally not applied here — the CLI also
// doesn't filter the JSON output by category (it's purely a
// human-readable rendering filter). Agents that want category
// filtering can apply it client-side over the returned categories[].
//
// The endpoints are global (no workspace), so we don't read
// `workspace` from input. Both endpoints require an authenticated
// user; the route table-level Apply hook handles that uniformly.
func (d *HTTPHandlerDispatcher) dispatchLibraryList(
	ctx context.Context,
	input map[string]any,
	user *models.User,
) (*mcp.CallToolResult, error) {
	const cmdKey = "library list"
	typ, _ := input["type"].(string)
	typ = strings.ToLower(strings.TrimSpace(typ))

	wantConventions := typ == "" || typ == "conventions"
	wantPlaybooks := typ == "" || typ == "playbooks"
	if !wantConventions && !wantPlaybooks {
		return mcp.NewToolResultErrorf(
			"%s: unknown --type %q (expected: conventions, playbooks, or empty for both)",
			cmdKey, typ,
		), nil
	}

	var conventions any
	var playbooks any

	if wantConventions {
		v, errRes := d.fetchLibraryEndpoint(ctx, user, cmdKey, "/api/v1/convention-library")
		if errRes != nil {
			return errRes, nil
		}
		conventions = v
	}
	if wantPlaybooks {
		v, errRes := d.fetchLibraryEndpoint(ctx, user, cmdKey, "/api/v1/playbook-library")
		if errRes != nil {
			return errRes, nil
		}
		playbooks = v
	}

	// Single-type mode returns the library payload directly (matches
	// the CLI). Both-types mode wraps in {conventions, playbooks}.
	switch {
	case wantConventions && wantPlaybooks:
		return packageStructuredResponse(cmdKey, map[string]any{
			"conventions": conventions,
			"playbooks":   playbooks,
		})
	case wantConventions:
		return packageStructuredResponse(cmdKey, conventions)
	default:
		return packageStructuredResponse(cmdKey, playbooks)
	}
}

// fetchLibraryEndpoint GETs one of the library endpoints and decodes
// the JSON body into a generic any so the caller can stuff it into
// the composed response without losing the wire shape.
func (d *HTTPHandlerDispatcher) fetchLibraryEndpoint(
	ctx context.Context,
	user *models.User,
	cmdKey, path string,
) (any, *mcp.CallToolResult) {
	req, err := d.buildAuthedRequest(ctx, http.MethodGet, path, nil, user)
	if err != nil {
		return nil, mcp.NewToolResultErrorf("%s: build %s: %s", cmdKey, path, err.Error())
	}
	rec := httptest.NewRecorder()
	d.Handler.ServeHTTP(rec, req)
	if rec.Code >= 400 {
		body := strings.TrimSpace(rec.Body.String())
		if body == "" {
			body = http.StatusText(rec.Code)
		}
		return nil, mcp.NewToolResultErrorf("%s: %s: %d %s", cmdKey, path, rec.Code, body)
	}
	var decoded any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		return nil, mcp.NewToolResultErrorf("%s: parse %s: %s", cmdKey, path, err.Error())
	}
	return decoded, nil
}
