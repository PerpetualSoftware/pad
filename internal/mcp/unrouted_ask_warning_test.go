package mcp

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// The unrouted-ask warning reaches the agent through item update: an item
// with only an agent role, moved into a waiting status, reaches no person.
func TestMCPItemUpdate_UnroutedAskWarningPassesThrough(t *testing.T) {
	f := newParentFixture(t)
	coll, err := f.store.CreateCollection(f.workspace.ID, models.CollectionCreate{
		Name: "Questions", Slug: "questions", Prefix: "Q",
		Schema: `{"fields":[{"key":"status","type":"select","options":["open","needs-answer","done"],"default":"open","awaits_human_options":["needs-answer"]}]}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	role, err := f.store.CreateAgentRole(f.workspace.ID, models.AgentRoleCreate{Name: "Seat"})
	if err != nil {
		t.Fatal(err)
	}
	it, err := f.store.CreateItem(f.workspace.ID, coll.ID, models.ItemCreate{Title: "For the seat", Fields: `{"status":"open"}`, AgentRoleID: &role.ID})
	if err != nil {
		t.Fatal(err)
	}
	res := f.dispatchResult(t, map[string]any{"workspace": f.workspace.Slug, "ref": it.Slug, "status": "needs-answer"})
	if res.IsError {
		t.Fatalf("update refused: %s", textOf(res))
	}
	m, _ := res.StructuredContent.(map[string]any)
	w, _ := m["warnings"].(map[string]any)
	if w["unrouted_ask"] != true {
		t.Fatalf("warnings = %v, want unrouted_ask true", m["warnings"])
	}
}
