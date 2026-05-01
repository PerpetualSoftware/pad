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

// itemLinkSpec wires a CLI link-command's arg shape to the underlying
// /api/v1/workspaces/{ws}/items/{slug}/links surface.
//
// The link-command surface has an asymmetry the route table can't
// express: the URL path goes through ONE item's slug while the body
// carries the OTHER item's UUID. Block/implements/supersedes/split-from
// use (source.Slug, target.ID); blocked-by inverts to (blocker.Slug,
// source.ID). The mapper would need to know which input key drives
// which side, and would still need a Handler reference to do the
// ref→ID prefetch for the body's target_id (the body shape rejects
// a raw ref).
//
// Lives as method-bound dispatchers on HTTPHandlerDispatcher rather
// than mappers in the route table for the same reason dispatchItemUpdate
// does — needs Handler to do the ref→{slug,id} prefetches before
// building the request.
type itemLinkSpec struct {
	// cmdKey is the dotted command path, e.g. "item block" — used as
	// the prefix on every error / IsError result so MCP clients see a
	// stable identifier.
	cmdKey string

	// urlRefKey is the input key whose resolved item.Slug goes into
	// the URL path /items/{slug}/links. For `item block` that's
	// `source_ref`; for `item blocked-by` it's `blocker_ref` (the
	// blocker is the link's source per the data model).
	urlRefKey string

	// bodyTargetRefKey is the input key whose resolved item.ID
	// becomes the body's target_id. For `item block` that's
	// `target_ref`; for `item blocked-by` it's `source_ref` (the
	// blocked item is the link's target).
	bodyTargetRefKey string

	// linkType is the canonical type written on the wire — must be
	// one of the constants in models/item_links.go (blocks,
	// implements, supersedes, split_from).
	linkType string
}

// itemLinkSpecs is the lookup table for link create/delete commands.
// Build is in init() so the package's startup-cost stays small and
// the cmdKey constants are co-located with their wiring.
//
// Read-only link commands (deps, related, implemented-by) are not
// here — those just GET /items/{ref}/links and don't need the URL/body
// asymmetry; they go through dispatchGetItemLinks instead.
var itemLinkSpecs = map[string]itemLinkSpec{
	// `block`: SOURCE blocks TARGET. Link source = source_ref item,
	// link target = target_ref item.
	"item block": {
		cmdKey:           "item block",
		urlRefKey:        "source_ref",
		bodyTargetRefKey: "target_ref",
		linkType:         models.ItemLinkTypeBlocks,
	},
	// `blocked-by`: SOURCE is blocked by BLOCKER → blocker blocks
	// source. The link's source is BLOCKER, target is SOURCE.
	"item blocked-by": {
		cmdKey:           "item blocked-by",
		urlRefKey:        "blocker_ref",
		bodyTargetRefKey: "source_ref",
		linkType:         models.ItemLinkTypeBlocks,
	},
	"item unblock": {
		cmdKey:           "item unblock",
		urlRefKey:        "source_ref",
		bodyTargetRefKey: "target_ref",
		linkType:         models.ItemLinkTypeBlocks,
	},
	"item implements": {
		cmdKey:           "item implements",
		urlRefKey:        "implementer_ref",
		bodyTargetRefKey: "target_ref",
		linkType:         models.ItemLinkTypeImplements,
	},
	"item unimplements": {
		cmdKey:           "item unimplements",
		urlRefKey:        "implementer_ref",
		bodyTargetRefKey: "target_ref",
		linkType:         models.ItemLinkTypeImplements,
	},
	"item supersedes": {
		cmdKey:           "item supersedes",
		urlRefKey:        "new_ref",
		bodyTargetRefKey: "old_ref",
		linkType:         models.ItemLinkTypeSupersedes,
	},
	"item unsupersede": {
		cmdKey:           "item unsupersede",
		urlRefKey:        "new_ref",
		bodyTargetRefKey: "old_ref",
		linkType:         models.ItemLinkTypeSupersedes,
	},
	"item split-from": {
		cmdKey:           "item split-from",
		urlRefKey:        "child_ref",
		bodyTargetRefKey: "parent_ref",
		linkType:         models.ItemLinkTypeSplitFrom,
	},
	"item unsplit": {
		cmdKey:           "item unsplit",
		urlRefKey:        "child_ref",
		bodyTargetRefKey: "parent_ref",
		linkType:         models.ItemLinkTypeSplitFrom,
	},
}

// itemPrefetch is the shape resolveItemRef returns. Only exposes the
// fields the link dispatchers need so callers can't accidentally lean
// on something that's only sometimes populated.
type itemPrefetch struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
}

// resolveItemRef does a GET /api/v1/workspaces/{ws}/items/{ref} and
// returns the resolved id+slug. Used by the link dispatchers to
// translate user-friendly refs (TASK-5, item slugs, UUIDs) into the
// {slug for URL, id for body} pair the /links surface expects.
//
// Goes through buildAuthedRequest so any OAuth-scope context attached
// at dispatch time (d.Apply) applies to the prefetch the same way it
// applies to the main request — same scope-bypass-prevention reasoning
// behind dispatchItemUpdate's prefetch (Codex review #345 round 1).
func (d *HTTPHandlerDispatcher) resolveItemRef(
	ctx context.Context,
	user *models.User,
	workspace, ref string,
) (*itemPrefetch, error) {
	path := "/api/v1/workspaces/" + url.PathEscape(workspace) +
		"/items/" + url.PathEscape(ref)
	req, err := d.buildAuthedRequest(ctx, http.MethodGet, path, nil, user)
	if err != nil {
		return nil, fmt.Errorf("build prefetch request for %q: %w", ref, err)
	}
	rec := httptest.NewRecorder()
	d.Handler.ServeHTTP(rec, req)
	if rec.Code >= 400 {
		body := strings.TrimSpace(rec.Body.String())
		if body == "" {
			body = http.StatusText(rec.Code)
		}
		return nil, fmt.Errorf("resolve %q: %d %s", ref, rec.Code, body)
	}
	var out itemPrefetch
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		return nil, fmt.Errorf("parse item prefetch for %q: %w", ref, err)
	}
	if out.ID == "" || out.Slug == "" {
		return nil, fmt.Errorf("resolve %q: response missing id or slug", ref)
	}
	return &out, nil
}

// dispatchCreateItemLink handles the create-side of the link surface
// (block, blocked-by, implements, supersedes, split-from). Resolves
// both refs, then POSTs to /items/{urlSlug}/links with body
// {target_id: <UUID>, link_type: <type>}.
//
// Mirrors the CLI's createLineageLink (cmd/pad/lineage.go) and
// blocksCmd / blockedByCmd (cmd/pad/main.go). The behaviour is
// identical: prefetch source + target, then create the link in the
// canonical direction the data model expects.
func (d *HTTPHandlerDispatcher) dispatchCreateItemLink(
	ctx context.Context,
	input map[string]any,
	user *models.User,
	spec itemLinkSpec,
) (*mcp.CallToolResult, error) {
	workspace, _ := input["workspace"].(string)
	if workspace == "" {
		return mcp.NewToolResultErrorf("%s: workspace is required", spec.cmdKey), nil
	}

	urlRef, _ := input[spec.urlRefKey].(string)
	if urlRef == "" {
		return mcp.NewToolResultErrorf("%s: %s is required", spec.cmdKey, spec.urlRefKey), nil
	}
	bodyRef, _ := input[spec.bodyTargetRefKey].(string)
	if bodyRef == "" {
		return mcp.NewToolResultErrorf("%s: %s is required", spec.cmdKey, spec.bodyTargetRefKey), nil
	}

	urlItem, err := d.resolveItemRef(ctx, user, workspace, urlRef)
	if err != nil {
		return mcp.NewToolResultErrorf("%s: %s", spec.cmdKey, err.Error()), nil
	}
	bodyItem, err := d.resolveItemRef(ctx, user, workspace, bodyRef)
	if err != nil {
		return mcp.NewToolResultErrorf("%s: %s", spec.cmdKey, err.Error()), nil
	}

	payload := map[string]any{
		"target_id": bodyItem.ID,
		"link_type": spec.linkType,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return mcp.NewToolResultErrorf("%s: encode body: %s", spec.cmdKey, err.Error()), nil
	}

	urlPath := "/api/v1/workspaces/" + url.PathEscape(workspace) +
		"/items/" + url.PathEscape(urlItem.Slug) + "/links"
	return d.executeRequest(ctx, spec.cmdKey, user, http.MethodPost, urlPath, body)
}

// dispatchDeleteItemLink handles the un-* side of the link surface
// (unblock, unimplements, unsupersede, unsplit). Mirrors the CLI's
// deleteLineageLink + unblockCmd: resolve both refs, list links on
// the source item, find the one matching (source.ID, target.ID,
// link_type), and DELETE it by id.
//
// Returns IsError when no matching link exists — same UX the CLI
// surfaces ("no <type> relationship found"). Surfacing the same
// missing-link error keeps the behaviour identical across transports.
func (d *HTTPHandlerDispatcher) dispatchDeleteItemLink(
	ctx context.Context,
	input map[string]any,
	user *models.User,
	spec itemLinkSpec,
) (*mcp.CallToolResult, error) {
	workspace, _ := input["workspace"].(string)
	if workspace == "" {
		return mcp.NewToolResultErrorf("%s: workspace is required", spec.cmdKey), nil
	}

	urlRef, _ := input[spec.urlRefKey].(string)
	if urlRef == "" {
		return mcp.NewToolResultErrorf("%s: %s is required", spec.cmdKey, spec.urlRefKey), nil
	}
	bodyRef, _ := input[spec.bodyTargetRefKey].(string)
	if bodyRef == "" {
		return mcp.NewToolResultErrorf("%s: %s is required", spec.cmdKey, spec.bodyTargetRefKey), nil
	}

	urlItem, err := d.resolveItemRef(ctx, user, workspace, urlRef)
	if err != nil {
		return mcp.NewToolResultErrorf("%s: %s", spec.cmdKey, err.Error()), nil
	}
	bodyItem, err := d.resolveItemRef(ctx, user, workspace, bodyRef)
	if err != nil {
		return mcp.NewToolResultErrorf("%s: %s", spec.cmdKey, err.Error()), nil
	}

	links, err := d.listItemLinks(ctx, user, workspace, urlItem.Slug)
	if err != nil {
		return mcp.NewToolResultErrorf("%s: %s", spec.cmdKey, err.Error()), nil
	}

	canonicalType, normErr := models.NormalizeItemLinkType(spec.linkType)
	if normErr != nil {
		// Programming error — only canonical types belong in itemLinkSpecs.
		return mcp.NewToolResultErrorf("%s: invalid link type %q", spec.cmdKey, spec.linkType), nil
	}

	var linkID string
	for _, link := range links {
		if link.SourceID != urlItem.ID || link.TargetID != bodyItem.ID {
			continue
		}
		got, err := models.NormalizeItemLinkType(link.LinkType)
		if err != nil {
			continue
		}
		if got == canonicalType {
			linkID = link.ID
			break
		}
	}
	if linkID == "" {
		return mcp.NewToolResultErrorf(
			"%s: no %s relationship found between %s and %s",
			spec.cmdKey, spec.linkType, urlRef, bodyRef,
		), nil
	}

	urlPath := "/api/v1/workspaces/" + url.PathEscape(workspace) +
		"/links/" + url.PathEscape(linkID)
	return d.executeRequest(ctx, spec.cmdKey, user, http.MethodDelete, urlPath, nil)
}

// dispatchGetItemLinks handles the read-only link queries (deps,
// related, implemented-by). Each takes a single ref input and lists
// every link touching that item — the differing CLI presentations
// (deps groups by direction, related groups by type, implemented-by
// filters to incoming implements) are pure rendering on top of the
// same payload, so the dispatcher returns the raw links and lets the
// agent group however it wants.
func (d *HTTPHandlerDispatcher) dispatchGetItemLinks(
	ctx context.Context,
	input map[string]any,
	user *models.User,
	cmdKey string,
) (*mcp.CallToolResult, error) {
	workspace, _ := input["workspace"].(string)
	if workspace == "" {
		return mcp.NewToolResultErrorf("%s: workspace is required", cmdKey), nil
	}
	ref, _ := input["ref"].(string)
	if ref == "" {
		return mcp.NewToolResultErrorf("%s: ref is required", cmdKey), nil
	}

	urlPath := "/api/v1/workspaces/" + url.PathEscape(workspace) +
		"/items/" + url.PathEscape(ref) + "/links"
	return d.executeRequest(ctx, cmdKey, user, http.MethodGet, urlPath, nil)
}

// listItemLinks issues an in-handler GET against
// /api/v1/workspaces/{ws}/items/{slug}/links and decodes the response
// into models.ItemLink so the un-* dispatchers can find the matching
// link to delete.
func (d *HTTPHandlerDispatcher) listItemLinks(
	ctx context.Context,
	user *models.User,
	workspace, itemSlug string,
) ([]models.ItemLink, error) {
	path := "/api/v1/workspaces/" + url.PathEscape(workspace) +
		"/items/" + url.PathEscape(itemSlug) + "/links"
	req, err := d.buildAuthedRequest(ctx, http.MethodGet, path, nil, user)
	if err != nil {
		return nil, fmt.Errorf("build links request: %w", err)
	}
	rec := httptest.NewRecorder()
	d.Handler.ServeHTTP(rec, req)
	if rec.Code >= 400 {
		body := strings.TrimSpace(rec.Body.String())
		if body == "" {
			body = http.StatusText(rec.Code)
		}
		return nil, fmt.Errorf("list item links: %d %s", rec.Code, body)
	}
	var links []models.ItemLink
	if err := json.Unmarshal(rec.Body.Bytes(), &links); err != nil {
		return nil, fmt.Errorf("parse links response: %w", err)
	}
	return links, nil
}
