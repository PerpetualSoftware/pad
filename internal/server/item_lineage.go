package server

import (
	"fmt"
	"strings"

	"github.com/xarmian/pad/internal/models"
)

// enrichItemsWithParent batch-populates parent link info on a slice of items.
// Used by list endpoints where calling enrichItemForResponse per-item is too expensive.
// visibleIDs controls which parent collections are allowed; nil means all visible.
func (s *Server) enrichItemsWithParent(workspaceID string, items []models.Item, visibleIDs ...[]string) {
	if len(items) == 0 {
		return
	}
	// Extract the optional visibility filter
	var vis []string
	hasVis := false
	if len(visibleIDs) > 0 && visibleIDs[0] != nil {
		vis = visibleIDs[0]
		hasVis = true
	}

	parentMap, err := s.store.GetParentMap(workspaceID)
	if err != nil || len(parentMap) == 0 {
		return
	}
	// Collect unique parent IDs
	parentIDs := make(map[string]bool)
	for _, pid := range parentMap {
		parentIDs[pid] = true
	}
	// Fetch parent item details (title, ref) in bulk
	type parentInfo struct {
		title          string
		ref            string
		slug           string
		collectionSlug string
	}
	parents := make(map[string]parentInfo)
	for pid := range parentIDs {
		item, err := s.store.GetItem(pid)
		if err != nil || item == nil {
			continue
		}
		// Skip parents from hidden collections
		if hasVis && !isCollectionVisible(item.CollectionID, vis) {
			continue
		}
		ref := ""
		if item.CollectionPrefix != "" && item.ItemNumber != nil {
			ref = fmt.Sprintf("%s-%d", item.CollectionPrefix, *item.ItemNumber)
		}
		parents[pid] = parentInfo{title: item.Title, ref: ref, slug: item.Slug, collectionSlug: item.CollectionSlug}
	}
	// Populate items — only set parent fields when the parent passed
	// the visibility filter (i.e. is in the parents map)
	for i := range items {
		pid, ok := parentMap[items[i].ID]
		if !ok {
			continue
		}
		if info, ok := parents[pid]; ok {
			items[i].ParentLinkID = pid
			items[i].ParentTitle = info.title
			items[i].ParentRef = info.ref
			items[i].ParentSlug = info.slug
			items[i].ParentCollectionSlug = info.collectionSlug
		}
	}
}

// enrichItemForResponse populates derived closure and parent info on a single item.
// An optional visibleIDs slice filters related items so hidden-collection metadata
// is not leaked. Pass nil (or omit) for full access.
func (s *Server) enrichItemForResponse(item *models.Item, visibleIDs ...[]string) error {
	if item == nil {
		return nil
	}

	var vis []string
	hasVis := len(visibleIDs) > 0 && visibleIDs[0] != nil
	if hasVis {
		vis = visibleIDs[0]
	}

	closure, err := s.deriveItemClosure(item, vis)
	if err != nil {
		return err
	}
	item.DerivedClosure = closure

	// Populate parent link info — skip if parent is in a hidden collection
	parentLink, err := s.store.GetParentForItem(item.ID)
	if err != nil {
		return err
	}
	if parentLink != nil {
		parentVisible := true
		if hasVis {
			if parent, perr := s.store.GetItem(parentLink.TargetID); perr == nil && parent != nil {
				parentVisible = isCollectionVisible(parent.CollectionID, vis)
			}
		}
		if parentVisible {
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
// from item links. When vis is non-nil, links to items in hidden collections
// are excluded.
func (s *Server) deriveItemClosure(item *models.Item, vis []string) (*models.ItemDerivedClosure, error) {
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
		if vis != nil {
			otherID := link.SourceID
			if otherID == item.ID {
				otherID = link.TargetID
			}
			if other, oerr := s.store.GetItem(otherID); oerr == nil && other != nil {
				if !isCollectionVisible(other.CollectionID, vis) {
					continue
				}
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
