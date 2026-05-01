package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// resolveAssignName rewrites a `--assign <name|email>` input into
// `assigned_user_id <uuid>` by hitting the workspace-members
// endpoint and finding a matching user. Mirrors the CLI's behaviour
// in cmd/pad/main.go's itemCreateCmd / itemUpdateCmd / itemListCmd —
// without this resolution, agents passing human-friendly assignee
// values would silently get empty results (the store filters by
// `i.assigned_user_id = ?` UUID, no name fallback).
//
// Returns the input map with `assign` replaced by `assigned_user_id`
// when a match is found, or unchanged when `assign` is missing /
// empty. Mismatches return a clear error so agents know to pass a
// different name.
//
// The returned map is always a fresh map — the caller's reference
// isn't mutated, matching the no-mutation contract of the rest of
// the dispatcher.
func (d *HTTPHandlerDispatcher) resolveAssignName(
	ctx context.Context,
	user *models.User,
	input map[string]any,
) (map[string]any, error) {
	rawAssign, present := input["assign"]
	if !present {
		return input, nil
	}
	assign, _ := rawAssign.(string)
	if assign == "" {
		return input, nil
	}
	// Already-resolved? If the caller used `--field assigned_user_id=<uuid>`
	// that's a separate input key — we don't touch it. If the caller
	// passed both `assign` and `assigned_user_id`, the explicit ID
	// wins; drop the assign value to avoid the resolution lookup.
	out := cloneStringMap(input)
	if existingID, _ := out["assigned_user_id"].(string); existingID != "" {
		delete(out, "assign")
		return out, nil
	}

	workspace, _ := input["workspace"].(string)
	if workspace == "" {
		return nil, fmt.Errorf("workspace is required to resolve --assign")
	}

	userID, err := d.lookupAssigneeID(ctx, user, workspace, assign)
	if err != nil {
		return nil, err
	}
	out["assigned_user_id"] = userID
	delete(out, "assign")
	return out, nil
}

// resolveRoleSlug rewrites a `--role <slug>` input into
// `agent_role_id <uuid>` by hitting the agent-roles endpoint and
// finding a matching role. Mirrors the CLI's behaviour in
// itemCreateCmd / itemUpdateCmd which treats `--role` as a slug or
// ID and resolves to the column UUID before sending the create/
// update — without resolution, agents passing slugs would silently
// get empty results (the store filters by `i.agent_role_id = ?`
// UUID, with slug accepted only on the LIST endpoint, not the
// item-mutation handlers).
//
// Symmetric to resolveAssignName: returns the input map with `role`
// replaced by `agent_role_id` when a match is found, or unchanged
// when `role` is missing / empty. Mismatches return a clear error.
//
// The handleGetAgentRole endpoint at /agent-roles/{roleID} accepts
// either a UUID or a slug as roleID, so this single GET resolves
// both. If the caller passed an explicit `agent_role_id` alongside
// `--role`, the explicit ID wins (matches the --assign precedence
// in resolveAssignName).
//
// The returned map is always a fresh copy — the caller's reference
// isn't mutated.
func (d *HTTPHandlerDispatcher) resolveRoleSlug(
	ctx context.Context,
	user *models.User,
	input map[string]any,
) (map[string]any, error) {
	rawRole, present := input["role"]
	if !present {
		return input, nil
	}
	role, _ := rawRole.(string)
	if role == "" {
		return input, nil
	}
	out := cloneStringMap(input)
	if existingID, _ := out["agent_role_id"].(string); existingID != "" {
		// Explicit ID wins over slug; drop the role key to avoid the
		// resolution lookup below.
		delete(out, "role")
		return out, nil
	}

	workspace, _ := input["workspace"].(string)
	if workspace == "" {
		return nil, fmt.Errorf("workspace is required to resolve --role")
	}

	roleID, err := d.lookupRoleID(ctx, user, workspace, role)
	if err != nil {
		return nil, err
	}
	out["agent_role_id"] = roleID
	delete(out, "role")
	return out, nil
}

// lookupRoleID issues an in-handler GET against
// /api/v1/workspaces/{ws}/agent-roles/{slug} and returns the role's
// canonical id. The handler accepts either UUID or slug for roleID
// (see handleGetAgentRole), so callers can pass a slug like
// "implementer" or a pre-resolved UUID interchangeably.
//
// Goes through buildAuthedRequest so d.Apply (the OAuth-scope hook)
// sees this prefetch the same as a top-level dispatch — no scope
// bypass during role resolution.
//
// Errors:
//
//   - underlying handler returns 404 → "no agent role matches --role %q"
//     (clearer than the raw 404 body for agents).
//   - other non-2xx → wrapped error with body.
//   - response shape doesn't include id → error.
func (d *HTTPHandlerDispatcher) lookupRoleID(
	ctx context.Context,
	user *models.User,
	workspace string,
	role string,
) (string, error) {
	path := "/api/v1/workspaces/" + url.PathEscape(workspace) +
		"/agent-roles/" + url.PathEscape(role)
	req, err := d.buildAuthedRequest(ctx, http.MethodGet, path, nil, user)
	if err != nil {
		return "", fmt.Errorf("build agent-role request: %w", err)
	}
	rec := httptest.NewRecorder()
	d.Handler.ServeHTTP(rec, req)
	if rec.Code == http.StatusNotFound {
		return "", fmt.Errorf("no agent role matches --role %q", role)
	}
	if rec.Code >= 400 {
		body := strings.TrimSpace(rec.Body.String())
		if body == "" {
			body = http.StatusText(rec.Code)
		}
		return "", fmt.Errorf("look up agent role: %d %s", rec.Code, body)
	}

	var resp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		return "", fmt.Errorf("parse agent-role response: %w", err)
	}
	if resp.ID == "" {
		return "", fmt.Errorf("agent-role response missing id for %q", role)
	}
	return resp.ID, nil
}

// lookupAssigneeID issues an in-handler GET against
// /api/v1/workspaces/{ws}/members and returns the user_id whose
// name OR email matches `assign`. Case-insensitive on both fields.
//
// Errors:
//
//   - underlying handler returns non-2xx → wrapped error with body.
//   - response shape doesn't match expected {members:[...]} → error.
//   - no member matches → "no workspace member matches --assign %q".
func (d *HTTPHandlerDispatcher) lookupAssigneeID(
	ctx context.Context,
	user *models.User,
	workspace string,
	assign string,
) (string, error) {
	path := "/api/v1/workspaces/" + url.PathEscape(workspace) + "/members"
	// Goes through buildAuthedRequest so d.Apply (the OAuth-scope
	// hook) sees this prefetch the same as a top-level dispatch —
	// no scope bypass during assignee resolution.
	req, err := d.buildAuthedRequest(ctx, http.MethodGet, path, nil, user)
	if err != nil {
		return "", fmt.Errorf("build members request: %w", err)
	}
	rec := httptest.NewRecorder()
	d.Handler.ServeHTTP(rec, req)
	if rec.Code >= 400 {
		body := strings.TrimSpace(rec.Body.String())
		if body == "" {
			body = http.StatusText(rec.Code)
		}
		return "", fmt.Errorf("list workspace members: %d %s", rec.Code, body)
	}

	// Response shape: {"members":[{user_id, user_name, user_email, ...}, ...], "invitations":[...]}
	var resp struct {
		Members []struct {
			UserID    string `json:"user_id"`
			UserName  string `json:"user_name"`
			UserEmail string `json:"user_email"`
		} `json:"members"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		return "", fmt.Errorf("parse members response: %w", err)
	}

	for _, m := range resp.Members {
		if strings.EqualFold(m.UserName, assign) || strings.EqualFold(m.UserEmail, assign) {
			return m.UserID, nil
		}
	}
	return "", fmt.Errorf("no workspace member matches --assign %q", assign)
}

// dispatchItemUpdate handles `pad item update <ref>` with full CLI
// parity, including the read-modify-write merge of the fields JSON.
//
// The handler at handleUpdateItem treats input.Fields as a complete
// replacement (json_extract-friendly), but the CLI does a GET first
// to read existing fields, merges in new --status / --priority /
// --field overrides, then PATCHes the merged result. Without this
// dispatch path, an MCP `item.update --status done` would erase
// every other field the schema set — Codex caught the equivalent
// shape regression on item.create in PR #343.
//
// Sequence:
//
//  1. GET /api/v1/workspaces/{ws}/items/{ref} — read current state.
//  2. Merge: existing.fields + input.{status, priority, category,
//     parent} + parsed --field key=value pairs. Last-write-wins per
//     key (matches CLI; --field can override --status).
//  3. PATCH /api/v1/workspaces/{ws}/items/{ref} with the merged
//     payload.
//
// Returns the PATCH response packaged like any other dispatch result
// (structured JSON if 2xx + JSON body, IsError-flagged if non-2xx).
func (d *HTTPHandlerDispatcher) dispatchItemUpdate(
	ctx context.Context,
	input map[string]any,
	user *models.User,
) (*mcp.CallToolResult, error) {
	const cmdKey = "item update"

	workspace, _ := input["workspace"].(string)
	ref, _ := input["ref"].(string)
	if workspace == "" {
		return mcp.NewToolResultErrorf("%s: workspace is required", cmdKey), nil
	}
	if ref == "" {
		return mcp.NewToolResultErrorf("%s: ref is required", cmdKey), nil
	}

	itemPath := "/api/v1/workspaces/" + url.PathEscape(workspace) +
		"/items/" + url.PathEscape(ref)

	// `--role` is now resolved at the dispatcher level (TASK-968):
	// Dispatch's preprocess step rewrites it to `agent_role_id`
	// before reaching this method, so by the time we get here a slug
	// has already been resolved to a UUID. The `--field
	// agent_role_id=<uuid>` workaround that the older rejection
	// pointed at still works (lifted via liftFieldsToColumns below)
	// and is preserved as the explicit-ID escape hatch when an agent
	// already knows the UUID and wants to skip the slug lookup.

	// Step 1: GET the existing item so we can read fields for the
	// read-modify-write merge. If the GET fails (item not found,
	// permission error, …), surface that to the caller — there's no
	// point trying to PATCH an item we can't read.
	//
	// Goes through buildAuthedRequest so d.Apply (the OAuth-scope
	// hook) sees this prefetch the same as a top-level dispatch.
	prefetchReq, err := d.buildAuthedRequest(ctx, http.MethodGet, itemPath, nil, user)
	if err != nil {
		return mcp.NewToolResultErrorf("%s: build prefetch request: %s", cmdKey, err.Error()), nil
	}
	prefetchRec := httptest.NewRecorder()
	d.Handler.ServeHTTP(prefetchRec, prefetchReq)
	if prefetchRec.Code >= 400 {
		// Mirror the CLI's "not found" UX — the handler's 404 body
		// already contains a clear message; package it the same way
		// any other tool error would be packaged.
		return packageHTTPResponse(ctx, cmdKey, prefetchRec.Result())
	}
	var existing struct {
		Fields string `json:"fields"`
	}
	if err := json.Unmarshal(prefetchRec.Body.Bytes(), &existing); err != nil {
		return mcp.NewToolResultErrorf("%s: parse current item: %s", cmdKey, err.Error()), nil
	}

	// Step 2: Build the PATCH payload.
	payload := map[string]any{}
	for _, key := range []string{"title", "content", "comment", "tags"} {
		if v, ok := input[key].(string); ok && v != "" {
			payload[key] = v
		}
	}
	if v, ok := input["assigned_user_id"].(string); ok && v != "" {
		payload["assigned_user_id"] = v
	}
	if v, ok := input["agent_role_id"].(string); ok && v != "" {
		payload["agent_role_id"] = v
	}
	if b, ok := input["pinned"].(bool); ok {
		payload["pinned"] = b
	}

	// Field merging — the actual reason this command needs a custom
	// dispatcher rather than a routeSpec entry. Match the CLI's
	// last-write-wins precedence: existing fields, then named flags
	// (status / priority / category / parent), then --field entries.
	if hasFieldChanges(input) {
		merged := map[string]any{}
		if existing.Fields != "" && existing.Fields != "{}" {
			if err := json.Unmarshal([]byte(existing.Fields), &merged); err != nil {
				return mcp.NewToolResultErrorf(
					"%s: parse existing fields JSON: %s", cmdKey, err.Error()), nil
			}
		}
		for _, key := range []string{"status", "priority", "category", "parent"} {
			if v, ok := input[key].(string); ok && v != "" {
				merged[key] = v
			}
		}
		if rawFields, ok := input["field"]; ok {
			extra, err := parseFieldKVP(rawFields)
			if err != nil {
				return mcp.NewToolResultErrorf("%s: parse --field: %s", cmdKey, err.Error()), nil
			}
			for k, v := range extra {
				merged[k] = v
			}
		}
		// Lift recognized column keys (agent_role_id, assigned_user_id)
		// out of the merged fields blob onto the top-level payload so
		// the handler writes the column instead of stuffing the value
		// inert in the JSON. Same shape mapItemCreate uses; matches
		// the workaround the --role rejection points at.
		liftFieldsToColumns(merged, payload)
		fieldsJSON, err := json.Marshal(merged)
		if err != nil {
			return mcp.NewToolResultErrorf("%s: encode merged fields: %s", cmdKey, err.Error()), nil
		}
		fieldsStr := string(fieldsJSON)
		payload["fields"] = fieldsStr
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return mcp.NewToolResultErrorf("%s: encode body: %s", cmdKey, err.Error()), nil
	}

	// Step 3: PATCH.
	return d.executeRequest(ctx, cmdKey, user, http.MethodPatch, itemPath, body)
}

// hasFieldChanges reports whether the input has any value that
// should trigger field-merging on update. Mirrors the CLI's check
// at cmd/pad/main.go itemUpdateCmd around the `hasFieldChanges`
// boolean — without this guard, dispatching `item update TASK-1
// --content "x"` would do an unnecessary GET-merge-PATCH of
// fields, churning the audit log entry for no reason.
func hasFieldChanges(input map[string]any) bool {
	for _, key := range []string{"status", "priority", "category", "parent"} {
		if v, ok := input[key].(string); ok && v != "" {
			return true
		}
	}
	if rawFields, ok := input["field"]; ok && rawFields != nil {
		switch x := rawFields.(type) {
		case string:
			return x != ""
		case []any:
			return len(x) > 0
		case []string:
			return len(x) > 0
		}
	}
	return false
}
