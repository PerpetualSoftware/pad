package server

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// enrichItemsWithParent batch-populates parent link info on a slice of items.
// Used by list endpoints where calling enrichItemForResponse per-item is too expensive.
// visibleIDs nil means an unrestricted caller; non-nil means restricted, and
// each parent is then checked per item (BUG-3366).
func (s *Server) enrichItemsWithParent(r *http.Request, workspaceID string, items []models.Item, visibleIDs ...[]string) {
	if len(items) == 0 {
		return
	}
	// A non-nil visibility filter marks a RESTRICTED caller; what each
	// restricted caller may see is then decided per item (BUG-3366), so only
	// its presence matters here.
	hasVis := len(visibleIDs) > 0 && visibleIDs[0] != nil

	// U6 hydration runs BEFORE the parent-map early return below. The two are
	// unrelated decorations, and a workspace with no parent links at all would
	// otherwise return here and silently ship every list read unhydrated.
	s.hydrateRelationTargets(r, workspaceID, items)

	parentMap, err := s.store.GetParentMap(workspaceID)
	if err != nil || len(parentMap) == 0 {
		return
	}
	// Collect only the parent IDs of the items we're actually returning —
	// not every parent link in the workspace. Scoping here keeps the batch
	// fetch below proportional to the page size, not the whole workspace
	// (BUG-2003).
	parentIDSet := make(map[string]bool, len(items))
	for i := range items {
		if pid, ok := parentMap[items[i].ID]; ok {
			parentIDSet[pid] = true
		}
	}
	if len(parentIDSet) == 0 {
		return
	}
	parentIDs := make([]string, 0, len(parentIDSet))
	for pid := range parentIDSet {
		parentIDs = append(parentIDs, pid)
	}
	// Fetch parent details (title, ref, slug, collection) for just those
	// parents in one skinny WHERE id IN (...) query — replaces the former
	// full-row GetItem per parent. Best-effort: on error, leave items
	// undecorated rather than failing the list.
	parents, err := s.store.GetItemLineageByIDs(parentIDs)
	if err != nil {
		return
	}
	// Populate items — only set parent fields when the parent resolved and
	// passed the visibility filter. A restricted caller is checked PER PARENT
	// (BUG-3366), the relation_targets policy: collection visibility is
	// navigation-lenient for item grants and would name an ungranted parent.
	var parentVisible func(id string) bool
	if hasVis {
		parentVisible = s.itemVisibleFor(r, workspaceID)
	}
	for i := range items {
		pid, ok := parentMap[items[i].ID]
		if !ok {
			continue
		}
		info, ok := parents[pid]
		if !ok {
			continue
		}
		if parentVisible != nil && !parentVisible(pid) {
			continue
		}
		items[i].ParentLinkID = pid
		items[i].ParentTitle = info.Title
		items[i].ParentRef = info.Ref
		items[i].ParentSlug = info.Slug
		items[i].ParentCollectionSlug = info.CollectionSlug
	}
}

// enrichItemForResponse populates derived closure and parent info on a single item.
// An optional non-nil visibleIDs marks a restricted caller, whose related items
// are then checked per item (BUG-3366) so ungranted ones are not named. Pass
// nil (or omit) for full access.
func (s *Server) enrichItemForResponse(r *http.Request, item *models.Item, visibleIDs ...[]string) error {
	if item == nil {
		return nil
	}

	hasVis := len(visibleIDs) > 0 && visibleIDs[0] != nil

	// A restricted caller is checked PER ITEM for everything this response
	// names (BUG-3366): see itemVisibleFor.
	var visible func(id string) bool
	if hasVis {
		visible = s.itemVisibleFor(r, item.WorkspaceID)
	}

	closure, err := s.deriveItemClosure(item, visible)
	if err != nil {
		return err
	}
	item.DerivedClosure = closure

	// U6: one item is the degenerate batch. Going through the same helper means
	// a single read and a list read cannot disagree about what `relation_targets`
	// says for the same item — including which targets collapse to id-only.
	one := []models.Item{*item}
	s.hydrateRelationTargets(r, item.WorkspaceID, one)
	item.RelationTargets = one[0].RelationTargets

	// Populate parent link info — skip if parent is in a hidden collection
	parentLink, err := s.store.GetParentForItem(item.ID)
	if err != nil {
		return err
	}
	if parentLink != nil {
		// A lookup that fails, or finds nothing, is not a visible parent
		// (BUG-3334); itemVisibleFor fails closed.
		if visible == nil || visible(parentLink.TargetID) {
			item.ParentLinkID = parentLink.TargetID
			item.ParentRef = parentLink.TargetRef
			item.ParentTitle = parentLink.TargetTitle
			item.ParentSlug = parentLink.TargetSlug
			item.ParentCollectionSlug = parentLink.TargetCollectionSlug
		}
	}

	return nil
}

// deriveItemClosure computes derived closure (superseded, implemented, split)
// from item links. When visible is non-nil, a link whose other side it
// refuses is excluded (BUG-3366: per item, not per collection).
func (s *Server) deriveItemClosure(item *models.Item, visible func(id string) bool) (*models.ItemDerivedClosure, error) {
	links, err := s.store.GetItemLinks(item.ID)
	if err != nil {
		return nil, err
	}

	var supersededBy []models.ItemRelationRef
	var implementedBy []models.ItemRelationRef
	var splitChildren []models.ItemRelationRef
	allSplitChildrenDone := true

	for _, link := range links {
		// If visibility is restricted, check that the "other side" is visible
		if visible != nil {
			otherID := link.SourceID
			if otherID == item.ID {
				otherID = link.TargetID
			}
			// A lookup that fails, or finds nothing, is not a visible item
			// (BUG-3334); itemVisibleFor fails closed.
			if !visible(otherID) {
				continue
			}
		}

		linkType, err := models.NormalizeItemLinkType(link.LinkType)
		if err != nil {
			continue
		}
		switch linkType {
		case models.ItemLinkTypeSupersedes:
			if link.TargetID == item.ID && models.IsTerminalStatusDefault(link.SourceStatus) {
				supersededBy = append(supersededBy, relationRefFromLink(link, true))
			}
		case models.ItemLinkTypeImplements:
			if link.TargetID == item.ID && models.IsTerminalStatusDefault(link.SourceStatus) {
				implementedBy = append(implementedBy, relationRefFromLink(link, true))
			}
		case models.ItemLinkTypeSplitFrom:
			if link.TargetID == item.ID {
				splitChildren = append(splitChildren, relationRefFromLink(link, true))
				if !models.IsTerminalStatusDefault(link.SourceStatus) {
					allSplitChildrenDone = false
				}
			}
		}
	}

	if len(supersededBy) > 0 {
		return &models.ItemDerivedClosure{
			IsClosed:     true,
			Kind:         "superseded_by",
			Summary:      "Superseded by " + summarizeRelationRefs(supersededBy),
			RelatedItems: supersededBy,
		}, nil
	}
	if len(implementedBy) > 0 {
		return &models.ItemDerivedClosure{
			IsClosed:     true,
			Kind:         "implemented_by",
			Summary:      "Implemented by " + summarizeRelationRefs(implementedBy),
			RelatedItems: implementedBy,
		}, nil
	}
	// NOTE: split_into does NOT auto-close the original item. Splitting work
	// out doesn't mean the original is done — it still stands on its own.
	if len(splitChildren) > 0 && allSplitChildrenDone {
		return &models.ItemDerivedClosure{
			IsClosed:     false,
			Kind:         "split_into",
			Summary:      "Split into completed items " + summarizeRelationRefs(splitChildren),
			RelatedItems: splitChildren,
		}, nil
	}

	return nil, nil
}

func relationRefFromLink(link models.ItemLink, useSource bool) models.ItemRelationRef {
	if useSource {
		return models.ItemRelationRef{
			ID:             link.SourceID,
			Slug:           link.SourceSlug,
			Ref:            link.SourceRef,
			Title:          link.SourceTitle,
			CollectionSlug: link.SourceCollectionSlug,
			Status:         link.SourceStatus,
		}
	}
	return models.ItemRelationRef{
		ID:             link.TargetID,
		Slug:           link.TargetSlug,
		Ref:            link.TargetRef,
		Title:          link.TargetTitle,
		CollectionSlug: link.TargetCollectionSlug,
		Status:         link.TargetStatus,
	}
}

func summarizeRelationRefs(items []models.ItemRelationRef) string {
	labels := make([]string, 0, len(items))
	for _, item := range items {
		labels = append(labels, relationRefLabel(item))
	}
	switch len(labels) {
	case 0:
		return ""
	case 1:
		return labels[0]
	case 2:
		return labels[0] + " and " + labels[1]
	default:
		return fmt.Sprintf("%s and %d more", strings.Join(labels[:2], ", "), len(labels)-2)
	}
}

func relationRefLabel(item models.ItemRelationRef) string {
	if item.Ref != "" && item.Title != "" {
		return item.Ref + " " + item.Title
	}
	if item.Ref != "" {
		return item.Ref
	}
	if item.Title != "" {
		return item.Title
	}
	return item.ID
}
