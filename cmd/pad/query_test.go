package main

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/server"
)

func TestFilterAgentAttentionKeepsOnlyActionableBuckets(t *testing.T) {
	input := []server.DashboardAttention{
		{Type: "stalled", ItemRef: "TASK-1"},
		{Type: "blocked", ItemRef: "TASK-2"},
		{Type: "phase_completion", ItemRef: "PHASE-1"},
		{Type: "orphaned_task", ItemRef: "TASK-3"},
	}

	got := filterAgentAttention(input)
	if len(got) != 3 {
		t.Fatalf("expected 3 attention items, got %#v", got)
	}
	for _, item := range got {
		if item.Type == "phase_completion" {
			t.Fatalf("did not expect phase_completion in %#v", got)
		}
	}
}

func TestIncomingImplementedByReturnsIncomingImplementersOnly(t *testing.T) {
	item := &models.Item{ID: "target"}
	links := []models.ItemLink{
		{SourceID: "task-a", TargetID: "target", LinkType: models.ItemLinkTypeImplements, SourceRef: "TASK-1", SourceTitle: "Implement auth", SourceStatus: "done"},
		{SourceID: "target", TargetID: "idea-b", LinkType: models.ItemLinkTypeImplements, TargetRef: "IDEA-2", TargetTitle: "Auth idea"},
	}

	got := incomingImplementedBy(item, links)
	if len(got) != 1 {
		t.Fatalf("expected 1 incoming implementer, got %#v", got)
	}
	if got[0].Ref != "TASK-1" || got[0].Title != "Implement auth" {
		t.Fatalf("unexpected implementer %#v", got[0])
	}
}

func TestBuildRelatedGroupsIncludesLineageAndDependencySections(t *testing.T) {
	item := &models.Item{ID: "task-1"}
	links := []models.ItemLink{
		{SourceID: "task-1", TargetID: "task-2", LinkType: models.ItemLinkTypeBlocks, TargetRef: "TASK-2", TargetTitle: "Blocked task"},
		{SourceID: "task-3", TargetID: "task-1", LinkType: models.ItemLinkTypeImplements, SourceRef: "TASK-3", SourceTitle: "Implementation task"},
		{SourceID: "task-1", TargetID: "doc-1", LinkType: models.ItemLinkTypeWikiLink, TargetRef: "DOC-1", TargetTitle: "Context doc"},
	}

	got := buildRelatedGroups(item, links)
	if len(got) != 3 {
		t.Fatalf("expected 3 groups, got %#v", got)
	}
	if got[0].Key != "blocks" {
		t.Fatalf("expected first group blocks, got %q", got[0].Key)
	}
	if got[1].Key != "links_to" {
		t.Fatalf("expected second group links_to, got %q", got[1].Key)
	}
	if got[2].Key != "implemented_by" {
		t.Fatalf("expected third group implemented_by, got %q", got[2].Key)
	}
}
