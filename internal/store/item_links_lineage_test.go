package store

import (
	"testing"

	"github.com/xarmian/pad/internal/models"
)

func TestCreateItemLinkNormalizesLineageTypes(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	child := createTestItem(t, s, ws.ID, col.ID, "Child Task", "")
	parent := createTestItem(t, s, ws.ID, col.ID, "Parent Task", "")

	link, err := s.CreateItemLink(ws.ID, models.ItemLinkCreate{
		TargetID: parent.ID,
		LinkType: "split-from",
	}, child.ID)
	if err != nil {
		t.Fatalf("CreateItemLink error: %v", err)
	}
	if link.LinkType != models.ItemLinkTypeSplitFrom {
		t.Fatalf("expected normalized link type %q, got %q", models.ItemLinkTypeSplitFrom, link.LinkType)
	}
}

func TestCreateItemLinkRejectsInvalidLineageType(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	item1 := createTestItem(t, s, ws.ID, col.ID, "Task A", "")
	item2 := createTestItem(t, s, ws.ID, col.ID, "Task B", "")

	_, err := s.CreateItemLink(ws.ID, models.ItemLinkCreate{
		TargetID: item2.ID,
		LinkType: "implemented-by",
	}, item1.ID)
	if err == nil {
		t.Fatal("expected invalid lineage link type to fail")
	}
}

func TestCreateItemLinkRejectsSelfLinks(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	item := createTestItem(t, s, ws.ID, col.ID, "Task A", "")

	_, err := s.CreateItemLink(ws.ID, models.ItemLinkCreate{
		TargetID: item.ID,
		LinkType: models.ItemLinkTypeSupersedes,
	}, item.ID)
	if err == nil {
		t.Fatal("expected self-link creation to fail")
	}
}
