package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/mark3labs/mcp-go/mcp"
)

// Every hop of the needs actions goes through buildAuthedRequest (directly or
// via executeRequest), so the scope checks apply to each one. The not-mine
// PUT and the snooze DELETE carry no source: the server takes it from how the
// request authenticated.

func needsItemURL(ws, ref string) string {
	return "/api/v1/workspaces/" + url.PathEscape(ws) + "/items/" + url.PathEscape(ref)
}

// needsTarget reads ref and the workspace, which every needs write needs.
func needsTarget(ctx context.Context, d *HTTPHandlerDispatcher, key string, input map[string]any) (ws, ref string, fail *CallToolResult) {
	ws, _ = input["workspace"].(string)
	if ws == "" {
		return "", "", noWorkspaceResult(ctx, d.Lister)
	}
	ref, _ = input["ref"].(string)
	if ref == "" {
		return "", "", validationFailedResult(key, "ref is required", "Pass the item ref, e.g. ref=TASK-5.")
	}
	return ws, ref, nil
}

// dispatchNeedsNotMine is `needs not-mine`: with to_role or to_user it first
// reassigns the item through the existing item PATCH, guarded by the seq it
// has just read, and only then marks it not mine with an empty body. A refused
// lookup or PATCH returns before the PUT, so nothing is written.
func (d *HTTPHandlerDispatcher) dispatchNeedsNotMine(ctx context.Context, input map[string]any, user *models.User) (*CallToolResult, error) {
	const key = "needs not-mine"
	ws, ref, fail := needsTarget(ctx, d, key, input)
	if fail != nil {
		return fail, nil
	}
	toRole, _ := input["to_role"].(string)
	toUser, _ := input["to_user"].(string)
	if toRole != "" && toUser != "" {
		return validationFailedResult(key, "to_role and to_user are mutually exclusive", "Pass one of them, or neither."), nil
	}
	var givenTo map[string]any
	if toRole != "" || toUser != "" {
		patch := map[string]any{}
		if toRole != "" {
			id, err := d.lookupRoleID(ctx, user, ws, toRole)
			if err != nil {
				return validationFailedResult(key, "resolve to_role: "+err.Error(), "Pass an existing agent role slug."), nil
			}
			patch["agent_role_id"], patch["clear_assigned_user"] = id, true
			givenTo = map[string]any{"kind": "role", "id": id, "label": toRole}
		} else {
			id, err := d.lookupAssigneeID(ctx, user, ws, toUser)
			if err != nil {
				return validationFailedResult(key, "resolve to_user: "+err.Error(), "Pass a workspace member's name or email."), nil
			}
			patch["assigned_user_id"] = id
			givenTo = map[string]any{"kind": "user", "id": id, "label": toUser}
		}
		req, err := d.buildAuthedRequest(ctx, http.MethodGet, needsItemURL(ws, ref), nil, user)
		if err != nil {
			return buildRequestErrorResult(key, err), nil
		}
		rec := httptest.NewRecorder()
		d.Handler.ServeHTTP(rec, req)
		if rec.Code >= 400 {
			return packageHTTPResponse(req.Context(), key, rec.Result(), d.Lister)
		}
		var item struct {
			Seq int64 `json:"seq"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &item); err != nil {
			return dispatcherErrorResult(key, "decode item", err), nil
		}
		patch["expected_seq"] = item.Seq
		raw, err := json.Marshal(patch)
		if err != nil {
			return dispatcherErrorResult(key, "encode request", err), nil
		}
		res, err := d.executeRequest(ctx, key, user, http.MethodPatch, needsItemURL(ws, ref), raw)
		if err != nil || res.IsError {
			return res, err
		}
	}
	res, err := d.executeRequest(ctx, key, user, http.MethodPut, needsItemURL(ws, ref)+"/needs/not-mine", []byte(`{}`))
	if err != nil {
		return res, err
	}
	if res.IsError {
		if givenTo != nil {
			notePartialGive(res, givenTo["label"].(string))
		}
		return res, nil
	}
	return needsResults(res, givenTo), nil
}

// notePartialGive prefixes the error message of a failed not-mine PUT that
// followed a successful reassignment, so the caller knows the item has
// already changed hands (the CLI says the same). Retrying the call is safe.
func notePartialGive(res *CallToolResult, label string) {
	prefix := "the item was already given to " + label + ", but marking it not mine failed: "
	for i, c := range res.Content {
		tc, ok := c.(mcp.TextContent)
		if !ok {
			continue
		}
		var env map[string]any
		if json.Unmarshal([]byte(tc.Text), &env) == nil {
			if e, ok := env["error"].(map[string]any); ok {
				msg, _ := e["message"].(string)
				e["message"] = prefix + msg
				if raw, err := json.Marshal(env); err == nil {
					tc.Text = string(raw)
					res.Content[i] = tc
					if res.StructuredContent != nil {
						res.StructuredContent = env
					}
					return
				}
			}
		}
		tc.Text = prefix + tc.Text
		res.Content[i] = tc
		return
	}
}

// dispatchNeedsRestore is `needs restore`: it removes the caller's not-mine
// mark and then their snooze. Both DELETEs are idempotent.
func (d *HTTPHandlerDispatcher) dispatchNeedsRestore(ctx context.Context, input map[string]any, user *models.User) (*CallToolResult, error) {
	const key = "needs restore"
	ws, ref, fail := needsTarget(ctx, d, key, input)
	if fail != nil {
		return fail, nil
	}
	for _, kind := range []string{"not-mine", "snooze"} {
		res, err := d.executeRequest(ctx, key, user, http.MethodDelete, needsItemURL(ws, ref)+"/needs/"+kind, nil)
		if err != nil || res.IsError {
			return res, err
		}
	}
	payload := map[string]any{"results": []any{map[string]any{"ref": ref, "not_mine": nil, "snooze": nil}}}
	raw, _ := json.Marshal(payload)
	return structuredResult(payload, string(raw)), nil
}

// needsResults gives the remote result the CLI's JSON shape: {results: [...]},
// one entry per ref, with given_to when the item was reassigned.
func needsResults(res *CallToolResult, givenTo map[string]any) *CallToolResult {
	entry, ok := res.StructuredContent.(map[string]any)
	if !ok || entry == nil {
		return res
	}
	if givenTo != nil {
		entry["given_to"] = givenTo
	}
	payload := map[string]any{"results": []any{entry}}
	raw, err := json.Marshal(payload)
	if err != nil {
		return dispatcherErrorResult("needs not-mine", "encode result", err)
	}
	return structuredResult(payload, string(raw))
}
