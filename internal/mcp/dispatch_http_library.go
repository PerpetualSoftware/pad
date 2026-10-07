package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/PerpetualSoftware/pad/internal/collections"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// --- library activate ---

// dispatchLibraryActivate reproduces `pad library activate <title>`. The
// server's POST /workspaces/{ws}/library/activate does the work since
// TASK-3462: it builds the fields, picks the collection that declares the
// entry's artifact kind (BUG-2702), and records the item's built-in origin, so
// this door, the CLI and the web create the same item.
//
// The title is checked against the libraries in-process first, so an unknown
// one answers with the not-found payload this tool has always returned rather
// than the server's 404. The OAuth-scope hook (d.Apply) still runs on the
// POST, so this isn't a scope bypass.
func (d *HTTPHandlerDispatcher) dispatchLibraryActivate(
	ctx context.Context,
	input map[string]any,
	user *models.User,
) (*CallToolResult, error) {
	const cmdKey = "library activate"
	workspace, _ := input["workspace"].(string)
	if workspace == "" {
		return noWorkspaceResult(ctx, d.Lister), nil
	}
	title, _ := input["title"].(string)
	if title == "" {
		return validationFailedResult(cmdKey, "title is required",
			"Pass `title=<library-item-title>` matching an entry in the convention or playbook library."), nil
	}
	if collections.GetLibraryConvention(title) == nil && collections.GetLibraryPlaybook(title) == nil {
		return NewErrorResult(ErrorPayload{
			Code:    ErrNotFound,
			Message: fmt.Sprintf("%s: %q not found in convention or playbook library", cmdKey, title),
			Hint:    "Use `pad_library action=list` to enumerate available titles.",
		}), nil
	}
	body, err := json.Marshal(map[string]string{"title": title})
	if err != nil {
		return dispatcherErrorResult(cmdKey, "encode body", err), nil
	}
	urlPath := "/api/v1/workspaces/" + url.PathEscape(workspace) + "/library/activate"
	return d.executeRequest(ctx, cmdKey, user, http.MethodPost, urlPath, body)
}
