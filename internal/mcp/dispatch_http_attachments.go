package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// dispatchAttachmentList handles `pad attachment list` — pure metadata
// query against /api/v1/workspaces/{ws}/attachments.
//
// Custom dispatcher (rather than a routeSpec) for two reasons:
//
//  1. `--item <ref>` needs ref→UUID resolution. The handler reads
//     `item_id` (UUID); the CLI resolves the ref via GetItem before
//     calling the API. Without resolution, an agent passing
//     `item=TASK-5` would get an empty result silently.
//  2. `--attached` / `--unattached` are mutex booleans on the CLI
//     side that fold into a single `item=attached|unattached` query
//     param. The dispatcher does the same fold (and rejects the
//     mutex violation) so MCP behaviour matches the CLI's
//     pre-flight validation.
//
// Also returns the same `{attachments, total, limit, offset}` shape
// the handler emits — that's the parity contract.
func (d *HTTPHandlerDispatcher) dispatchAttachmentList(
	ctx context.Context,
	input map[string]any,
	user *models.User,
) (*mcp.CallToolResult, error) {
	const cmdKey = "attachment list"
	workspace, _ := input["workspace"].(string)
	if workspace == "" {
		return mcp.NewToolResultErrorf("%s: workspace is required", cmdKey), nil
	}

	attached, _ := input["attached"].(bool)
	unattached, _ := input["unattached"].(bool)
	if attached && unattached {
		return mcp.NewToolResultErrorf(
			"%s: --attached and --unattached are mutually exclusive", cmdKey,
		), nil
	}
	itemRef, _ := input["item"].(string)
	if itemRef != "" && unattached {
		return mcp.NewToolResultErrorf(
			"%s: --item and --unattached are mutually exclusive", cmdKey,
		), nil
	}

	q := url.Values{}
	if s, _ := input["category"].(string); s != "" {
		q.Set("category", s)
	}
	if s, _ := input["collection"].(string); s != "" {
		q.Set("collection", s)
	}
	if s, _ := input["sort"].(string); s != "" {
		q.Set("sort", s)
	}
	if n, ok := numericInput(input["limit"]); ok && n > 0 {
		q.Set("limit", strconv.FormatInt(n, 10))
	}
	if n, ok := numericInput(input["offset"]); ok && n > 0 {
		q.Set("offset", strconv.FormatInt(n, 10))
	}
	switch {
	case attached:
		q.Set("item", "attached")
	case unattached:
		q.Set("item", "unattached")
	}

	// Resolve --item ref → item_id UUID. Goes through the existing
	// resolveItemRef helper that the link dispatchers use, so the
	// Apply (OAuth-scope) hook applies uniformly.
	if itemRef != "" {
		resolved, err := d.resolveItemRef(ctx, user, workspace, itemRef)
		if err != nil {
			return mcp.NewToolResultErrorf("%s: resolve --item: %s", cmdKey, err.Error()), nil
		}
		q.Set("item_id", resolved.ID)
	}

	urlPath := "/api/v1/workspaces/" + url.PathEscape(workspace) + "/attachments"
	if encoded := q.Encode(); encoded != "" {
		urlPath += "?" + encoded
	}
	return d.executeRequest(ctx, cmdKey, user, http.MethodGet, urlPath, nil)
}

// dispatchAttachmentShow handles `pad attachment show <attachment-id>
// [--variant ...]` — metadata-only HEAD request.
//
// The HEAD response has no body; the metadata lives in headers
// (Content-Type, Content-Length, Content-Disposition, ETag,
// Last-Modified). We extract those into a JSON object that mirrors
// the CLI's `--format json` output (cmd/pad/main.go attachmentShowCmd):
//
//	{id, mime, size, filename?, etag?, last_modified?}
//
// Custom dispatcher because packageHTTPResponse expects a JSON body —
// HEAD responses always have an empty body, so we'd otherwise return
// empty TextContent which is uninformative.
func (d *HTTPHandlerDispatcher) dispatchAttachmentShow(
	ctx context.Context,
	input map[string]any,
	user *models.User,
) (*mcp.CallToolResult, error) {
	const cmdKey = "attachment show"
	workspace, _ := input["workspace"].(string)
	if workspace == "" {
		return mcp.NewToolResultErrorf("%s: workspace is required", cmdKey), nil
	}
	attachmentID, _ := input["attachment_id"].(string)
	if attachmentID == "" {
		return mcp.NewToolResultErrorf("%s: attachment_id is required", cmdKey), nil
	}

	urlPath := "/api/v1/workspaces/" + url.PathEscape(workspace) +
		"/attachments/" + url.PathEscape(attachmentID)
	if v, _ := input["variant"].(string); v != "" {
		urlPath += "?variant=" + url.QueryEscape(v)
	}

	req, err := d.buildAuthedRequest(ctx, http.MethodHead, urlPath, nil, user)
	if err != nil {
		return mcp.NewToolResultErrorf("%s: build request: %s", cmdKey, err.Error()), nil
	}
	rec := httptest.NewRecorder()
	d.Handler.ServeHTTP(rec, req)

	if rec.Code >= 400 {
		body := strings.TrimSpace(rec.Body.String())
		if body == "" {
			body = http.StatusText(rec.Code)
		}
		return mcp.NewToolResultErrorf("pad %s failed: %s", cmdKey, body), nil
	}

	headers := rec.Result().Header
	out := map[string]any{
		"id":   attachmentID,
		"mime": headers.Get("Content-Type"),
	}
	if cl := headers.Get("Content-Length"); cl != "" {
		if n, err := strconv.ParseInt(cl, 10, 64); err == nil {
			out["size"] = n
		}
	}
	if filename := parseAttachmentFilename(headers.Get("Content-Disposition")); filename != "" {
		out["filename"] = filename
	}
	if etag := headers.Get("ETag"); etag != "" {
		out["etag"] = etag
	}
	if lm := headers.Get("Last-Modified"); lm != "" {
		out["last_modified"] = lm
	}
	return packageStructuredResponse(cmdKey, out)
}

// parseAttachmentFilename extracts the filename from a
// Content-Disposition header. Mirrors the CLI's helper of the same
// name (cmd/pad/main.go) — handles both the bare `filename=value` and
// the URL-encoded `filename*=UTF-8”value` forms.
//
// Reproduced here rather than imported because the CLI's helper is
// in package main and not callable from internal/mcp.
func parseAttachmentFilename(header string) string {
	if header == "" {
		return ""
	}
	// Prefer the RFC 5987 `filename*=UTF-8''<urlencoded>` form when
	// present — it's the spec-compliant carrier for non-ASCII names.
	const filenameStarPrefix = "filename*=UTF-8''"
	for _, part := range strings.Split(header, ";") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, filenameStarPrefix) {
			raw := part[len(filenameStarPrefix):]
			decoded, err := url.QueryUnescape(raw)
			if err == nil {
				return decoded
			}
		}
	}
	for _, part := range strings.Split(header, ";") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "filename=") {
			value := part[len("filename="):]
			value = strings.TrimSpace(value)
			value = strings.TrimPrefix(value, `"`)
			value = strings.TrimSuffix(value, `"`)
			return value
		}
	}
	return ""
}
