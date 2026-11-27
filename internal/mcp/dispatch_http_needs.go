package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

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

// needsTargets reads the workspace and the refs, which every needs write
// needs. ref is a string or, as stdio accepts (cmdhelp marks it repeatable),
// an array of strings.
func needsTargets(ctx context.Context, d *HTTPHandlerDispatcher, key string, input map[string]any) (ws string, refs []string, fail *CallToolResult) {
	ws, _ = input["workspace"].(string)
	if ws == "" {
		return "", nil, noWorkspaceResult(ctx, d.Lister)
	}
	refs, err := bulkUpdateRefs(input["ref"])
	if err != nil {
		return "", nil, validationFailedResult(key, err.Error(), "Pass the item ref, e.g. ref=TASK-5, or an array of refs.")
	}
	if len(refs) == 0 {
		return "", nil, validationFailedResult(key, "ref is required", "Pass the item ref, e.g. ref=TASK-5.")
	}
	return ws, refs, nil
}

// dispatchNeedsNotMine is `needs not-mine`: with to_role or to_user it first
// reassigns each item through the existing item PATCH, guarded by the seq it
// has just read, and only then marks it not mine with an empty body. A refused
// lookup or PATCH returns before the PUT, so nothing is written for that ref.
// Refs are processed in order and processing stops at the first error.
func (d *HTTPHandlerDispatcher) dispatchNeedsNotMine(ctx context.Context, input map[string]any, user *models.User) (*CallToolResult, error) {
	const key = "needs not-mine"
	ws, refs, fail := needsTargets(ctx, d, key, input)
	if fail != nil {
		return fail, nil
	}
	toRole, _ := input["to_role"].(string)
	toUser, _ := input["to_user"].(string)
	if toRole != "" && toUser != "" {
		return validationFailedResult(key, "to_role and to_user are mutually exclusive", "Pass one of them, or neither."), nil
	}
	var give map[string]any
	var givenTo map[string]any
	if toRole != "" || toUser != "" {
		give = map[string]any{}
		if toRole != "" {
			id, err := d.lookupRoleID(ctx, user, ws, toRole)
			if err != nil {
				if res := tooLargeResultIfExceeded(key, err); res != nil {
					return res, nil
				}
				return validationFailedResult(key, "resolve to_role: "+err.Error(), "Pass an existing agent role slug."), nil
			}
			give["agent_role_id"], give["clear_assigned_user"] = id, true
			givenTo = map[string]any{"kind": "role", "id": id, "label": toRole}
		} else {
			id, err := d.lookupAssigneeID(ctx, user, ws, toUser)
			if err != nil {
				if res := tooLargeResultIfExceeded(key, err); res != nil {
					return res, nil
				}
				return validationFailedResult(key, "resolve to_user: "+err.Error(), "Pass a workspace member's name or email."), nil
			}
			give["assigned_user_id"] = id
			givenTo = map[string]any{"kind": "user", "id": id, "label": toUser}
		}
	}
	entries := make([]any, 0, len(refs))
	for i, ref := range refs {
		entry, res, err := d.needsNotMineOne(ctx, key, user, ws, ref, give, givenTo)
		if err != nil {
			return res, err
		}
		if res != nil {
			noteNeedsRefFailure(res, refs, i)
			return res, nil
		}
		entries = append(entries, entry)
	}
	return needsResults(key, entries), nil
}

// needsNotMineOne gives one item away when give is set, then marks it not
// mine. It returns the result entry, or the error result to stop on.
func (d *HTTPHandlerDispatcher) needsNotMineOne(ctx context.Context, key string, user *models.User, ws, ref string, give, givenTo map[string]any) (any, *CallToolResult, error) {
	if give != nil {
		req, err := d.buildAuthedRequest(ctx, http.MethodGet, needsItemURL(ws, ref), nil, user)
		if err != nil {
			return nil, buildRequestErrorResult(key, err), nil
		}
		resp, exceeded := d.serveCapped(req)
		if exceeded {
			return nil, responseTooLargeResult(key, d.responseMaxBytes()), nil
		}
		if resp.StatusCode >= 400 {
			res, err := packageHTTPResponse(req.Context(), key, resp, d.Lister)
			return nil, res, err
		}
		bodyBytes, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return nil, dispatcherErrorResult(key, "read item", err), nil
		}
		var item struct {
			Seq int64 `json:"seq"`
		}
		if err := json.Unmarshal(bodyBytes, &item); err != nil {
			return nil, dispatcherErrorResult(key, "decode item", err), nil
		}
		patch := make(map[string]any, len(give)+1)
		for k, v := range give {
			patch[k] = v
		}
		patch["expected_seq"] = item.Seq
		raw, err := json.Marshal(patch)
		if err != nil {
			return nil, dispatcherErrorResult(key, "encode request", err), nil
		}
		res, err := d.executeRequest(ctx, key, user, http.MethodPatch, needsItemURL(ws, ref), raw)
		if err != nil || res.IsError {
			return nil, res, err
		}
	}
	res, err := d.executeRequest(ctx, key, user, http.MethodPut, needsItemURL(ws, ref)+"/needs/not-mine", []byte(`{}`))
	if err != nil {
		return nil, res, err
	}
	if res.IsError {
		if givenTo != nil {
			notePartialGive(res, givenTo["label"].(string))
		}
		return nil, res, nil
	}
	// The write has landed, so an unexpected body shape is passed through
	// rather than reported as a failure.
	entry := res.StructuredContent
	if m, ok := entry.(map[string]any); ok && m != nil && givenTo != nil {
		m["given_to"] = givenTo
	}
	return entry, nil, nil
}

// notePartialGive prefixes the error message of a failed not-mine PUT that
// followed a successful reassignment, so the caller knows the item has
// already changed hands (the CLI says the same). Retrying the call is safe.
func notePartialGive(res *CallToolResult, label string) {
	prefix := "the item was already given to " + label + ", but marking it not mine failed: "
	rewriteErrorMessage(res, func(msg string) string { return prefix + msg })
}

// noteNeedsRefFailure names, on a call with several refs, the ref that failed
// and the ones already done, as the CLI does. A single-ref error is unchanged.
func noteNeedsRefFailure(res *CallToolResult, refs []string, failed int) {
	if len(refs) < 2 {
		return
	}
	rewriteErrorMessage(res, func(msg string) string {
		msg = refs[failed] + ": " + msg
		if failed > 0 {
			msg += " (already done: " + strings.Join(refs[:failed], ", ") + ")"
		}
		return msg
	})
}

// rewriteErrorMessage applies edit to the message of an error result: the
// envelope's error.message when the text is one, else the whole text.
func rewriteErrorMessage(res *CallToolResult, edit func(string) string) {
	for i, c := range res.Content {
		tc, ok := c.(mcp.TextContent)
		if !ok {
			continue
		}
		var env map[string]any
		if json.Unmarshal([]byte(tc.Text), &env) == nil {
			if e, ok := env["error"].(map[string]any); ok {
				msg, _ := e["message"].(string)
				e["message"] = edit(msg)
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
		tc.Text = edit(tc.Text)
		res.Content[i] = tc
		return
	}
}

// dispatchNeedsRestore is `needs restore`: for each ref in order it removes
// the caller's not-mine mark and then their snooze, stopping at the first
// error. Both DELETEs are idempotent.
func (d *HTTPHandlerDispatcher) dispatchNeedsRestore(ctx context.Context, input map[string]any, user *models.User) (*CallToolResult, error) {
	const key = "needs restore"
	ws, refs, fail := needsTargets(ctx, d, key, input)
	if fail != nil {
		return fail, nil
	}
	entries := make([]any, 0, len(refs))
	for i, ref := range refs {
		for _, kind := range []string{"not-mine", "snooze"} {
			res, err := d.executeRequest(ctx, key, user, http.MethodDelete, needsItemURL(ws, ref)+"/needs/"+kind, nil)
			if err != nil {
				return res, err
			}
			if res.IsError {
				noteNeedsRefFailure(res, refs, i)
				return res, nil
			}
		}
		entries = append(entries, map[string]any{"ref": ref, "not_mine": nil, "snooze": nil})
	}
	return needsResults(key, entries), nil
}

// needsResults gives the remote result the CLI's JSON shape: {results: [...]},
// one entry per ref, with given_to when the item was reassigned.
func needsResults(key string, entries []any) *CallToolResult {
	payload := map[string]any{"results": entries}
	raw, err := json.Marshal(payload)
	if err != nil {
		return dispatcherErrorResult(key, "encode result", err)
	}
	return structuredResult(payload, string(raw))
}
