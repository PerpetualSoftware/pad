package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/PerpetualSoftware/pad/internal/artifact"
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
//   - The target collection, resolved from each entry's artifact kind via the
//     collection that DECLARES it (SPEC-5), falling back to the canonical slug
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
		target, terr := d.libraryTargetCollection(ctx, user, workspace, string(artifact.KindConvention), "conventions")
		if terr != nil {
			return dispatcherErrorResult(cmdKey, "resolve target collection", terr), nil
		}
		return d.postLibraryItem(ctx, user, workspace, target, cmdKey, conv.Title, conv.Content, fieldsJSON)
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
		target, terr := d.libraryTargetCollection(ctx, user, workspace, string(artifact.KindPlaybook), "playbooks")
		if terr != nil {
			return dispatcherErrorResult(cmdKey, "resolve target collection", terr), nil
		}
		return d.postLibraryItem(ctx, user, workspace, target, cmdKey, pb.Title, pb.Content, string(fieldsJSON))
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

// libraryTargetCollection resolves where a library entry of the given artifact
// kind should be activated: whichever collection DECLARES that kind (SPEC-5
// artifact_kind), not the one that happens to be named "conventions" or
// "playbooks". Activating into a workspace whose collection was renamed used
// to fail not-found with the collection sitting right there ([[BUG-2702]]).
//
// A LOOKUP FAILURE IS NOT A FALLBACK CASE. Falling back on an error means
// writing to a slug we never confirmed anything about, and a workspace may
// legitimately have an ordinary collection sitting on the canonical slug, so
// the guess can land a library entry somewhere unrelated. Errors are returned;
// the fallback applies ONLY when the read SUCCEEDED and no collection declares
// the kind, which is the genuine pre-backfill case. Codex round 5.
func (d *HTTPHandlerDispatcher) libraryTargetCollection(
	ctx context.Context,
	user *models.User,
	workspace, kind, fallback string,
) (string, error) {
	req, err := d.buildAuthedRequest(ctx, http.MethodGet,
		"/api/v1/workspaces/"+url.PathEscape(workspace)+"/collections", nil, user)
	if err != nil {
		return "", fmt.Errorf("resolve target collection: %w", err)
	}
	rec := httptest.NewRecorder()
	d.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return "", fmt.Errorf("resolve target collection: listing collections returned %d", rec.Code)
	}
	var colls []models.Collection
	if err := json.Unmarshal(rec.Body.Bytes(), &colls); err != nil {
		return "", fmt.Errorf("resolve target collection: decode collections: %w", err)
	}
	if slug := collections.SlugForArtifactKind(colls, kind); slug != "" {
		return slug, nil
	}
	return fallback, nil
}

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
