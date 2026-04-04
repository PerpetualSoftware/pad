package models

import (
	"fmt"
	"strings"
)

const (
	ItemLinkTypeRelated    = "related"
	ItemLinkTypeBlocks     = "blocks"
	ItemLinkTypeWikiLink   = "wiki_link"
	ItemLinkTypeSplitFrom  = "split_from"
	ItemLinkTypeSupersedes = "supersedes"
	ItemLinkTypeImplements = "implements"
	ItemLinkTypePhase     = "phase"
)

var itemLinkTypeAliases = map[string]string{
	"":           ItemLinkTypeRelated,
	"related":    ItemLinkTypeRelated,
	"blocks":     ItemLinkTypeBlocks,
	"wiki_link":  ItemLinkTypeWikiLink,
	"wiki-link":  ItemLinkTypeWikiLink,
	"split_from": ItemLinkTypeSplitFrom,
	"split-from": ItemLinkTypeSplitFrom,
	"supersedes": ItemLinkTypeSupersedes,
	"implements": ItemLinkTypeImplements,
	"phase":      ItemLinkTypePhase,
}

// NormalizeItemLinkType canonicalizes supported link types and returns an error
// for unknown values.
func NormalizeItemLinkType(linkType string) (string, error) {
	normalized := strings.TrimSpace(strings.ToLower(linkType))
	if canonical, ok := itemLinkTypeAliases[normalized]; ok {
		return canonical, nil
	}
	return "", fmt.Errorf("invalid link type %q", linkType)
}

func IsLineageItemLinkType(linkType string) bool {
	normalized, err := NormalizeItemLinkType(linkType)
	if err != nil {
		return false
	}
	switch normalized {
	case ItemLinkTypeSplitFrom, ItemLinkTypeSupersedes, ItemLinkTypeImplements:
		return true
	default:
		return false
	}
}
