package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// dispatchGitHubLink is pad_item.action=link-pr on the remote door
// (TASK-3611): the PR metadata the agent supplied, written through the typed
// github_pr member (BUG-2696) as `pad github link --pr-number ...` writes it.
// Nothing is fetched from GitHub. The server validates the number and URL.
func (d *HTTPHandlerDispatcher) dispatchGitHubLink(ctx context.Context, input map[string]any, user *models.User) (*CallToolResult, error) {
	const cmdKey = "github link"
	workspace, _ := input["workspace"].(string)
	if workspace == "" {
		return noWorkspaceResult(ctx, d.Lister), nil
	}
	ref, _ := input["item_ref"].(string)
	if ref == "" {
		return validationFailedResult(cmdKey, "ref is required", "Pass `ref=<TASK-N>`, the item to link the PR to."), nil
	}
	// Any positive whole number the CLI's --pr-number takes; the bound is
	// only where a JSON number stops being an exact integer (codex r2: a
	// MaxInt32 cap refused what the CLI accepts).
	//
	// BUG-4065: read through numericInput, so the bound is wholeInt64's
	// exclusive 2^53 (the old 'n > 1<<53' admitted 2^53, the rounded image of
	// 2^53+1), and a json.Number or a plain decimal string is read exactly
	// instead of refused for its type. Absent stays required. The message and
	// hint are unchanged.
	n, ok, err := intParam(input, "pr_number")
	if err != nil || !ok || n <= 0 {
		return validationFailedResult(cmdKey, "pr_number must be a positive whole number", "Pass `pr_number=<N>`, the PR's number."), nil
	}
	prURL, _ := input["pr_url"].(string)
	if strings.TrimSpace(prURL) == "" {
		return validationFailedResult(cmdKey, "pr_url is required", "Pass `pr_url=<https://...>`, the PR's URL."), nil
	}
	str := func(k string) string { v, _ := input[k].(string); return v }
	body, err := json.Marshal(models.ItemUpdate{GitHubPR: &models.ItemGitHubPR{
		Number: int(n),
		URL:    prURL,
		Title:  str("pr_title"),
		State:  str("pr_state"),
		Branch: str("pr_branch"),
		Repo:   str("pr_repo"),
	}})
	if err != nil {
		return dispatcherErrorResult(cmdKey, "encode body", err), nil
	}
	return d.executeRequest(ctx, cmdKey, user, http.MethodPatch, githubItemPath(workspace, ref), body)
}

// dispatchGitHubUnlink is pad_item.action=unlink-pr on the remote door: the
// typed clear_github_pr member, as `pad github unlink` sends it.
func (d *HTTPHandlerDispatcher) dispatchGitHubUnlink(ctx context.Context, input map[string]any, user *models.User) (*CallToolResult, error) {
	const cmdKey = "github unlink"
	workspace, _ := input["workspace"].(string)
	if workspace == "" {
		return noWorkspaceResult(ctx, d.Lister), nil
	}
	ref, _ := input["item_ref"].(string)
	if ref == "" {
		return validationFailedResult(cmdKey, "ref is required", "Pass `ref=<TASK-N>`, the item to unlink."), nil
	}
	// `pad github unlink` refuses an item with no PR link; so does this door
	// (TASK-3611 parity). A read that fails falls through to the PATCH, whose
	// refusal (not found, forbidden) is the server's own and is classified.
	if item, ferr := d.fetchItem(ctx, user, workspace, ref); ferr == nil {
		var fields map[string]json.RawMessage
		_ = json.Unmarshal([]byte(item.Fields), &fields)
		if _, linked := fields["github_pr"]; !linked {
			return validationFailedResult(cmdKey, "item "+ref+" has no linked PR", "There is nothing to unlink."), nil
		}
	}
	body, err := json.Marshal(models.ItemUpdate{ClearGitHubPR: true})
	if err != nil {
		return dispatcherErrorResult(cmdKey, "encode body", err), nil
	}
	return d.executeRequest(ctx, cmdKey, user, http.MethodPatch, githubItemPath(workspace, ref), body)
}

func githubItemPath(workspace, ref string) string {
	return "/api/v1/workspaces/" + url.PathEscape(workspace) + "/items/" + url.PathEscape(ref)
}

// refAsItemRef dispatches cmdPath with the catalog's `ref` under `item_ref`,
// the name `pad github link [item-ref]` / `unlink <item-ref>` give their
// positional (TASK-3611): every other pad_item action takes `ref`, so the
// catalog keeps that name and translates here, for both transports.
func refAsItemRef(cmdPath []string) ActionFn {
	return func(ctx context.Context, input map[string]any, env ActionEnv) (*CallToolResult, error) {
		out := make(map[string]any, len(input))
		for k, v := range input {
			out[k] = v
		}
		if v, ok := out["ref"]; ok {
			out["item_ref"] = v
			delete(out, "ref")
		}
		return env.Dispatch(ctx, cmdPath, out)
	}
}
