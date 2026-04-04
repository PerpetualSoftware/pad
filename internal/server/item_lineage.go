package server

import (
	"fmt"
	"strings"

	"github.com/xarmian/pad/internal/models"
)

func (s *Server) enrichItemForResponse(item *models.Item) error {
	if item == nil {
		return nil
	}
	closure, err := s.deriveItemClosure(item)
	if err != nil {
		return err
	}
	item.DerivedClosure = closure
	return nil
}

func (s *Server) deriveItemClosure(item *models.Item) (*models.ItemDerivedClosure, error) {
	links, err := s.store.GetItemLinks(item.ID)
	if err != nil {
		return nil, err
	}

	var supersededBy []models.ItemRelationRef
	var implementedBy []models.ItemRelationRef
	var splitChildren []models.ItemRelationRef
	allSplitChildrenDone := true

	for _, link := range links {
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
	if len(splitChildren) > 0 && allSplitChildrenDone {
		return &models.ItemDerivedClosure{
			IsClosed:     true,
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
