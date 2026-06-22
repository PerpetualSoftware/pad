package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// dispatchItemImport reproduces `pad item import <file>` over the HTTP
// MCP transport. It POSTs the raw artifact text (YAML frontmatter +
// Markdown body) to the workspace's import-artifact endpoint.
//
// Why a special-case method instead of a routeTable entry:
//
// The import endpoint (handleImportArtifact → parseArtifactRequest)
// reads the request body as raw bytes — NOT as JSON — and the CLI
// sends it with Content-Type text/markdown (internal/cli/client.go's
// ImportArtifact). The RouteMapper contract carries only a []byte body
// that buildHTTPRequest unconditionally stamps with
// `application/json`. There's no hook in that path to (a) send a
// verbatim, un-marshalled body or (b) override the content type, so
// the artifact would have to ride the wire as a JSON string — which
// parseArtifactRequest's ReadAll would then see double-quoted and
// reject as malformed frontmatter. Routing it through a dispatcher
// method lets us build the request, set the right header, and serve it
// through the same authed/scoped path executeRequest uses.
//
// The artifact arrives in the `artifact` input param (NOT `content`):
// `content` is just the item's Markdown body, whereas the artifact
// carries the frontmatter the server needs to reconstruct the item's
// collection + typed fields. This mirrors the stdio/exec catalog
// action (actionItemImport), which spills the same `artifact` string
// to a temp file and hands the CLI a path.
//
// Mutating but not destructive — the server imports the artifact as a
// DRAFT item, returning {ref, slug, warnings}.
func (d *HTTPHandlerDispatcher) dispatchItemImport(
	ctx context.Context,
	input map[string]any,
	user *models.User,
) (*mcp.CallToolResult, error) {
	const cmdKey = "item import"

	workspace, _ := input["workspace"].(string)
	if workspace == "" {
		return validationFailedResult(cmdKey, "workspace is required",
			"Pass `workspace=<slug>` or set a session default via pad_set_workspace."), nil
	}
	artifact, _ := input["artifact"].(string)
	if strings.TrimSpace(artifact) == "" {
		return validationFailedResult(cmdKey, "artifact is required",
			"Pass `artifact=<full artifact text>` — the YAML-frontmatter + Markdown body a prior `export` produced."), nil
	}

	urlPath := "/api/v1/workspaces/" + url.PathEscape(workspace) + "/import-artifact"

	// Build through buildAuthedRequest so the API-token scope check
	// (TokenScopeAllows) runs on this POST exactly like every other
	// synthesized write. The body is the raw artifact bytes.
	req, err := d.buildAuthedRequest(ctx, http.MethodPost, urlPath, []byte(artifact), user)
	if err != nil {
		if strings.HasPrefix(err.Error(), "permission_denied:") {
			return NewErrorResult(ErrorPayload{
				Code:    ErrPermissionDenied,
				Message: cmdKey + ": " + err.Error(),
				Hint:    "Token scope does not permit this operation. Re-issue with a write scope (POST).",
			}), nil
		}
		return dispatcherErrorResult(cmdKey, "build request", err), nil
	}

	// Override the JSON content type buildHTTPRequest stamps on bodies.
	// parseArtifactRequest reads raw bytes and the CLI sends
	// text/markdown; matching it keeps the two transports' wire shape
	// identical.
	req.Header.Set("Content-Type", "text/markdown; charset=utf-8")

	rec := httptest.NewRecorder()
	d.Handler.ServeHTTP(rec, req)
	return packageHTTPResponse(req.Context(), cmdKey, rec.Result(), d.Lister)
}
