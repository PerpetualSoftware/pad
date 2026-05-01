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

	// `--role` parity with mapItemCreate: until the next route-table
	// expansion adds slug → ID resolution, reject loudly so agents
	// don't get a successful update response while their requested
	// role assignment is silently dropped (Codex review #345 round 1).
	// The workaround pointed at — passing `agent_role_id=<uuid>`
	// directly in the input — is the genuinely-supported path; we
	// pass it through to ItemUpdate.AgentRoleID below. (Codex
	// review #345 round 2 caught the original message pointing at
	// `--field agent_role_id=...` which writes to the fields JSON
	// blob, not the column, and would have silently no-op'd the
	// role assignment.)
	if v, ok := input["role"].(string); ok && v != "" {
		return mcp.NewToolResultErrorf(
			"%s: --role is not yet supported by HTTPHandlerDispatcher; "+
				"slug → role-ID resolution lands in the next route-table "+
				"expansion. For now, pass `agent_role_id=<uuid>` directly "+
				"in the tool input (use `role list` to find the UUID).",
			cmdKey,
		), nil
	}

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
		return packageHTTPResponse(cmdKey, prefetchRec.Result())
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
