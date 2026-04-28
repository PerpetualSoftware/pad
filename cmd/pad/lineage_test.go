package main

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestFindMatchingLinkIDMatchesCanonicalType(t *testing.T) {
	links := []models.ItemLink{
		{ID: "one", SourceID: "a", TargetID: "b", LinkType: "split-from"},
		{ID: "two", SourceID: "a", TargetID: "c", LinkType: models.ItemLinkTypeSplitFrom},
	}

	got := findMatchingLinkID(links, models.ItemLinkTypeSplitFrom, "a", "b")
	if got != "one" {
		t.Fatalf("expected canonical matcher to find link 'one', got %q", got)
	}
}

func TestFindMatchingLinkIDSkipsOtherRelationships(t *testing.T) {
	links := []models.ItemLink{
		{ID: "one", SourceID: "a", TargetID: "b", LinkType: models.ItemLinkTypeBlocks},
	}

	got := findMatchingLinkID(links, models.ItemLinkTypeImplements, "a", "b")
	if got != "" {
		t.Fatalf("expected no match, got %q", got)
	}
}
