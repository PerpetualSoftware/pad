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
		return validationFailedResult(spec.cmdKey, "workspace is required",
			"Pass `workspace=<slug>` or set a session default via pad_set_workspace."), nil
	}

	urlRef, _ := input[spec.urlRefKey].(string)
	if urlRef == "" {
		return validationFailedResult(spec.cmdKey,
			fmt.Sprintf("%s is required", spec.urlRefKey),
			fmt.Sprintf("Pass `%s=<TASK-N>` (the source side of the link).", spec.urlRefKey)), nil
	}
	bodyRef, _ := input[spec.bodyTargetRefKey].(string)
	if bodyRef == "" {
		return validationFailedResult(spec.cmdKey,
			fmt.Sprintf("%s is required", spec.bodyTargetRefKey),
			fmt.Sprintf("Pass `%s=<TASK-N>` (the target side of the link).", spec.bodyTargetRefKey)), nil
	}

	urlItem, err := d.resolveItemRef(ctx, user, workspace, urlRef)
	if err != nil {
		return validationFailedResult(spec.cmdKey, err.Error(),
			fmt.Sprintf("Verify item %q exists in workspace %q (use pad_item search / list).", urlRef, workspace)), nil
	}
	bodyItem, err := d.resolveItemRef(ctx, user, workspace, bodyRef)
	if err != nil {
		return validationFailedResult(spec.cmdKey, err.Error(),
			fmt.Sprintf("Verify item %q exists in workspace %q (use pad_item search / list).", bodyRef, workspace)), nil
	}

	payload := map[string]any{
		"target_id": bodyItem.ID,
		"link_type": spec.linkType,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return dispatcherErrorResult(spec.cmdKey, "encode body", err), nil
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
		return validationFailedResult(spec.cmdKey, "workspace is required",
			"Pass `workspace=<slug>` or set a session default via pad_set_workspace."), nil
	}

	urlRef, _ := input[spec.urlRefKey].(string)
	if urlRef == "" {
		return validationFailedResult(spec.cmdKey,
			fmt.Sprintf("%s is required", spec.urlRefKey),
			fmt.Sprintf("Pass `%s=<TASK-N>` (the source side of the link to remove).", spec.urlRefKey)), nil
	}
	bodyRef, _ := input[spec.bodyTargetRefKey].(string)
	if bodyRef == "" {
		return validationFailedResult(spec.cmdKey,
			fmt.Sprintf("%s is required", spec.bodyTargetRefKey),
			fmt.Sprintf("Pass `%s=<TASK-N>` (the target side of the link to remove).", spec.bodyTargetRefKey)), nil
	}

	urlItem, err := d.resolveItemRef(ctx, user, workspace, urlRef)
	if err != nil {
		return validationFailedResult(spec.cmdKey, err.Error(),
			fmt.Sprintf("Verify item %q exists in workspace %q.", urlRef, workspace)), nil
	}
	bodyItem, err := d.resolveItemRef(ctx, user, workspace, bodyRef)
	if err != nil {
		return validationFailedResult(spec.cmdKey, err.Error(),
			fmt.Sprintf("Verify item %q exists in workspace %q.", bodyRef, workspace)), nil
	}

	links, err := d.listItemLinks(ctx, user, workspace, urlItem.Slug)
	if err != nil {
		return dispatcherErrorResult(spec.cmdKey, "list links", err), nil
	}

	canonicalType, normErr := models.NormalizeItemLinkType(spec.linkType)
	if normErr != nil {
		// Programming error — only canonical types belong in itemLinkSpecs.
		return dispatcherErrorResult(spec.cmdKey, "validate link type",
			fmt.Errorf("invalid link type %q", spec.linkType)), nil
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
		return NewErrorResult(ErrorPayload{
			Code: ErrNotFound,
			Message: fmt.Sprintf("%s: no %s relationship found between %s and %s",
				spec.cmdKey, spec.linkType, urlRef, bodyRef),
			Hint: fmt.Sprintf("Use pad_item action=deps to see existing relationships on %q.", urlRef),
		}), nil
	}

	urlPath := "/api/v1/workspaces/" + url.PathEscape(workspace) +
		"/links/" + url.PathEscape(linkID)
	res, err := d.executeRequest(ctx, spec.cmdKey, user, http.MethodDelete, urlPath, nil)
	if err != nil || res.IsError {
		return res, err
	}
	// Handler returns 204 No Content — packageHTTPResponse turns an
	// empty body into an empty TextContent, which is uninformative
	// for MCP clients. Mirror the CLI's `--format json` output for
	// these commands (cmd/pad/main.go's unblockCmd / lineage
	// deleteLineageLink: `{"status":"removed"}`) so MCP gets a
	// structured success signal. Goes through packageStructuredResponse
	// so the StructuredContent is a JSON-decoded `map[string]any`,
	// matching what the rest of the dispatcher emits.
	return packageStructuredResponse(spec.cmdKey, map[string]string{"status": "removed"})
}

// dispatchItemDeps handles `pad item deps <ref>` — the simplest of
// the three read-only link queries. CLI parity: `deps --format json`
// returns the raw `/links` array, so we just GET-and-package.
func (d *HTTPHandlerDispatcher) dispatchItemDeps(
	ctx context.Context,
	input map[string]any,
	user *models.User,
) (*mcp.CallToolResult, error) {
	const cmdKey = "item deps"
	workspace, _ := input["workspace"].(string)
	if workspace == "" {
		return validationFailedResult(cmdKey, "workspace is required",
			"Pass `workspace=<slug>` or set a session default via pad_set_workspace."), nil
	}
	ref, _ := input["ref"].(string)
	if ref == "" {
		return validationFailedResult(cmdKey, "ref is required",
			"Pass `ref=<TASK-N>` (the item whose dependencies you're querying)."), nil
	}

	urlPath := "/api/v1/workspaces/" + url.PathEscape(workspace) +
		"/items/" + url.PathEscape(ref) + "/links"
	return d.executeRequest(ctx, cmdKey, user, http.MethodGet, urlPath, nil)
}

// relatedEntry / relatedGroup mirror the CLI's `--format json` output
// for `item related` / `item implemented-by` (cmd/pad/query.go's
// types of the same name). Reproduced here because the CLI types are
// in package main and not importable; field shapes match exactly so
// MCP clients see the same JSON they'd see through the CLI.
type relatedEntry struct {
	Ref            string `json:"ref,omitempty"`
	Title          string `json:"title"`
	CollectionSlug string `json:"collection_slug,omitempty"`
	Status         string `json:"status,omitempty"`
}

type relatedGroup struct {
	Key     string         `json:"key"`
	Label   string         `json:"label"`
	Entries []relatedEntry `json:"entries"`
}

// dispatchItemRelated handles `pad item related <ref>` and emits the
// grouped response shape the CLI's `--format json` output uses
// (cmd/pad/query.go relatedCmd):
//
//	{"item_ref":..., "item_title":..., "collection":...,
//	 "group_count": N, "groups": [{"key", "label", "entries":[...]}, ...]}
//
// Codex review on PR #346 caught the original raw-links shape as a
// behavioural divergence from the CLI; fixing it preserves the
// "transport-equivalent to ExecDispatcher" contract this dispatcher
// is built on.
func (d *HTTPHandlerDispatcher) dispatchItemRelated(
	ctx context.Context,
	input map[string]any,
	user *models.User,
) (*mcp.CallToolResult, error) {
	const cmdKey = "item related"
	workspace, _ := input["workspace"].(string)
	if workspace == "" {
		return validationFailedResult(cmdKey, "workspace is required",
			"Pass `workspace=<slug>` or set a session default via pad_set_workspace."), nil
	}
	ref, _ := input["ref"].(string)
	if ref == "" {
		return validationFailedResult(cmdKey, "ref is required",
			"Pass `ref=<TASK-N>` (the item whose related items you're querying)."), nil
	}

	item, err := d.fetchItem(ctx, user, workspace, ref)
	if err != nil {
		return validationFailedResult(cmdKey, err.Error(),
			fmt.Sprintf("Verify item %q exists in workspace %q.", ref, workspace)), nil
	}
	links, err := d.listItemLinks(ctx, user, workspace, item.Slug)
	if err != nil {
		return dispatcherErrorResult(cmdKey, "list links", err), nil
	}

	groups := buildRelatedGroups(item, links)
	payload := map[string]any{
		"item_ref":    itemRefString(item),
		"item_title":  item.Title,
		"collection":  item.CollectionSlug,
		"group_count": len(groups),
		"groups":      groups,
	}
	return packageStructuredResponse(cmdKey, payload)
}

// dispatchItemImplementedBy handles `pad item implemented-by <ref>`
// with the CLI's filtered shape (cmd/pad/query.go implementedByCmd):
//
//	{"item_ref":..., "item_title":..., "count": N,
//	 "results": [{"ref","title","collection_slug","status"}, ...]}
//
// Filters to INCOMING `implements` links only — outgoing implements
// links are excluded because they describe what THIS item implements,
// not what implements it.
func (d *HTTPHandlerDispatcher) dispatchItemImplementedBy(
	ctx context.Context,
	input map[string]any,
	user *models.User,
) (*mcp.CallToolResult, error) {
	const cmdKey = "item implemented-by"
	workspace, _ := input["workspace"].(string)
	if workspace == "" {
		return validationFailedResult(cmdKey, "workspace is required",
			"Pass `workspace=<slug>` or set a session default via pad_set_workspace."), nil
	}
	ref, _ := input["ref"].(string)
	if ref == "" {
		return validationFailedResult(cmdKey, "ref is required",
			"Pass `ref=<TASK-N>` (the item whose implementers you're querying)."), nil
	}

	item, err := d.fetchItem(ctx, user, workspace, ref)
	if err != nil {
		return validationFailedResult(cmdKey, err.Error(),
			fmt.Sprintf("Verify item %q exists in workspace %q.", ref, workspace)), nil
	}
	links, err := d.listItemLinks(ctx, user, workspace, item.Slug)
	if err != nil {
		return dispatcherErrorResult(cmdKey, "list links", err), nil
	}

	results := incomingImplementedBy(item, links)
	payload := map[string]any{
		"item_ref":   itemRefString(item),
		"item_title": item.Title,
		"count":      len(results),
		"results":    results,
	}
	return packageStructuredResponse(cmdKey, payload)
}

// packageStructuredResponse encodes payload to JSON, then decodes it
// back to a generic any so the StructuredContent surface matches the
// shape MCP clients see over the wire — `map[string]any` / `[]any` /
// JSON-decoded primitives — rather than the originally-typed Go
// struct slices.
//
// This matches packageHTTPResponse's pattern: that helper json-decodes
// the handler's response body into `any` for the structured channel,
// so synthesized responses use the same path here for shape parity.
func packageStructuredResponse(cmdKey string, payload any) (*mcp.CallToolResult, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return dispatcherErrorResult(cmdKey, "encode response", err), nil
	}
	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		// Should be unreachable — we just marshalled a known-good
		// payload — but if it ever happens, fall back to the typed
		// payload + raw body so the caller still gets something
		// usable instead of an error.
		return mcp.NewToolResultStructured(payload, string(body)), nil
	}
	return mcp.NewToolResultStructured(decoded, string(body)), nil
}

// fetchItem retrieves a full models.Item via the
// /api/v1/workspaces/{ws}/items/{ref} surface. Used by the related /
// implemented-by dispatchers which need title + collection_slug +
// computed ref for the response wrapper. Goes through buildAuthedRequest
// so d.Apply (OAuth scope context) applies to the prefetch — same
// scope-bypass-prevention reasoning behind dispatchItemUpdate's GET.
func (d *HTTPHandlerDispatcher) fetchItem(
	ctx context.Context,
	user *models.User,
	workspace, ref string,
) (*models.Item, error) {
	path := "/api/v1/workspaces/" + url.PathEscape(workspace) +
		"/items/" + url.PathEscape(ref)
	req, err := d.buildAuthedRequest(ctx, http.MethodGet, path, nil, user)
	if err != nil {
		return nil, fmt.Errorf("build item request for %q: %w", ref, err)
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
	var item models.Item
	if err := json.Unmarshal(rec.Body.Bytes(), &item); err != nil {
		return nil, fmt.Errorf("parse item response for %q: %w", ref, err)
	}
	return &item, nil
}

// itemRefString returns "TASK-5"-style refs for non-nil items, or
// the empty string when the collection prefix or item number is
// missing. Mirrors cli.ItemRef but operates on a pointer so the
// dispatchers can pass *models.Item without dereferencing.
func itemRefString(item *models.Item) string {
	if item == nil || item.CollectionPrefix == "" || item.ItemNumber == nil {
		return ""
	}
	return fmt.Sprintf("%s-%d", item.CollectionPrefix, *item.ItemNumber)
}

// buildRelatedGroups mirrors cmd/pad/query.go's function of the same
// name. Reproduced here because the CLI version is in package main.
// Groups every link touching `item` by canonical type + direction
// (split_from vs split_into, supersedes vs superseded_by, etc.) and
// returns a stable-ordered list.
func buildRelatedGroups(item *models.Item, links []models.ItemLink) []relatedGroup {
	if item == nil || len(links) == 0 {
		return []relatedGroup{}
	}

	type groupDef struct{ label string }
	definitions := map[string]groupDef{
		"blocks":         {label: "Blocks"},
		"blocked_by":     {label: "Blocked by"},
		"links_to":       {label: "Links to"},
		"referenced_by":  {label: "Referenced by"},
		"split_from":     {label: "Split from"},
		"split_into":     {label: "Split into"},
		"supersedes":     {label: "Supersedes"},
		"superseded_by":  {label: "Superseded by"},
		"implements":     {label: "Implements"},
		"implemented_by": {label: "Implemented by"},
		"related":        {label: "Related"},
	}
	order := []string{
		"blocks", "blocked_by",
		"links_to", "referenced_by",
		"split_from", "split_into",
		"supersedes", "superseded_by",
		"implements", "implemented_by",
		"related",
	}

	grouped := map[string][]relatedEntry{}
	for _, link := range links {
		linkType, err := models.NormalizeItemLinkType(link.LinkType)
		if err != nil {
			linkType = models.ItemLinkTypeRelated
		}
		isSource := link.SourceID == item.ID

		switch linkType {
		case models.ItemLinkTypeBlocks:
			if isSource {
				grouped["blocks"] = append(grouped["blocks"], relatedEntryFromLink(link, false))
			} else {
				grouped["blocked_by"] = append(grouped["blocked_by"], relatedEntryFromLink(link, true))
			}
		case models.ItemLinkTypeWikiLink:
			if isSource {
				grouped["links_to"] = append(grouped["links_to"], relatedEntryFromLink(link, false))
			} else {
				grouped["referenced_by"] = append(grouped["referenced_by"], relatedEntryFromLink(link, true))
			}
		case models.ItemLinkTypeSplitFrom:
			if isSource {
				grouped["split_from"] = append(grouped["split_from"], relatedEntryFromLink(link, false))
			} else {
				grouped["split_into"] = append(grouped["split_into"], relatedEntryFromLink(link, true))
			}
		case models.ItemLinkTypeSupersedes:
			if isSource {
				grouped["supersedes"] = append(grouped["supersedes"], relatedEntryFromLink(link, false))
			} else {
				grouped["superseded_by"] = append(grouped["superseded_by"], relatedEntryFromLink(link, true))
			}
		case models.ItemLinkTypeImplements:
			if isSource {
				grouped["implements"] = append(grouped["implements"], relatedEntryFromLink(link, false))
			} else {
				grouped["implemented_by"] = append(grouped["implemented_by"], relatedEntryFromLink(link, true))
			}
		default:
			grouped["related"] = append(grouped["related"], relatedEntryFromLink(link, !isSource))
		}
	}

	results := make([]relatedGroup, 0, len(order))
	for _, key := range order {
		entries := grouped[key]
		if len(entries) == 0 {
			continue
		}
		results = append(results, relatedGroup{
			Key:     key,
			Label:   definitions[key].label,
			Entries: entries,
		})
	}
	return results
}

// incomingImplementedBy mirrors cmd/pad/query.go's helper. Filters
// the link list to incoming `implements` links only — outgoing
// implements describe what THIS item implements, which is the
// reverse of what callers want.
func incomingImplementedBy(item *models.Item, links []models.ItemLink) []relatedEntry {
	if item == nil {
		return []relatedEntry{}
	}
	results := make([]relatedEntry, 0, len(links))
	for _, link := range links {
		linkType, err := models.NormalizeItemLinkType(link.LinkType)
		if err != nil {
			continue
		}
		if linkType != models.ItemLinkTypeImplements || link.TargetID != item.ID {
			continue
		}
		results = append(results, relatedEntryFromLink(link, true))
	}
	return results
}

// relatedEntryFromLink projects a link's source-side or target-side
// metadata into a relatedEntry. Mirrors cmd/pad/query.go's helper.
func relatedEntryFromLink(link models.ItemLink, useSource bool) relatedEntry {
	if useSource {
		return relatedEntry{
			Ref:            link.SourceRef,
			Title:          link.SourceTitle,
			CollectionSlug: link.SourceCollectionSlug,
			Status:         link.SourceStatus,
		}
	}
	return relatedEntry{
		Ref:            link.TargetRef,
		Title:          link.TargetTitle,
		CollectionSlug: link.TargetCollectionSlug,
		Status:         link.TargetStatus,
	}
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
