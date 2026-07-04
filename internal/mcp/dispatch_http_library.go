package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/PerpetualSoftware/pad/internal/collections"
	"github.com/PerpetualSoftware/pad/internal/models"
)

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
		return validationFailedResult(cmdKey, "workspace is required",
			"Pass `workspace=<slug>` or set a session default via pad_set_workspace."), nil
	}
	title, _ := input["title"].(string)
	if title == "" {
		return validationFailedResult(cmdKey, "title is required",
			"Pass `title=<library-item-title>` matching an entry in the convention or playbook library."), nil
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
			return dispatcherErrorResult(cmdKey, "build convention fields", err), nil
		}
		return d.postLibraryItem(ctx, user, workspace, "conventions", cmdKey, conv.Title, conv.Content, fieldsJSON)
	}

	if pb := collections.GetLibraryPlaybook(title); pb != nil {
		// Forward invocation_slug + arguments only when set so legacy
		// library entries (none of which carry them) seed with the
		// original three-field shape. Mirrors ShipPlaybook() and the
		// CLI activate path in cmd/pad/main.go's libraryActivate.
		fields := map[string]any{
			"status":  "active",
			"trigger": pb.Trigger,
			"scope":   pb.Scope,
		}
		if pb.InvocationSlug != "" {
			fields["invocation_slug"] = pb.InvocationSlug
		}
		if len(pb.Arguments) > 0 {
			fields["arguments"] = pb.Arguments
		}
		fieldsJSON, err := json.Marshal(fields)
		if err != nil {
			return dispatcherErrorResult(cmdKey, "encode playbook fields", err), nil
		}
		return d.postLibraryItem(ctx, user, workspace, "playbooks", cmdKey, pb.Title, pb.Content, string(fieldsJSON))
	}

	return NewErrorResult(ErrorPayload{
		Code:    ErrNotFound,
		Message: fmt.Sprintf("%s: %q not found in convention or playbook library", cmdKey, title),
		Hint:    "Use `pad_library action=list` to enumerate available titles.",
	}), nil
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
		return dispatcherErrorResult(cmdKey, "encode body", err), nil
	}
	urlPath := "/api/v1/workspaces/" + url.PathEscape(workspace) +
		"/collections/" + url.PathEscape(collection) + "/items"
	return d.executeRequest(ctx, cmdKey, user, http.MethodPost, urlPath, body)
}
