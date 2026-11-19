package server

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/decision"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3921 F3 and TASK-4643, through the router (CONVE-19): item show carries
// no attention key (no surface reads the set), while the decisions endpoint
// returns all four.
func TestItemShow_OmitsAttentionDecisionKeys(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	reg, err := decision.ProductionRegistry()
	if err != nil {
		t.Fatal(err)
	}
	srv.SetDecisionRunner(decision.NewRunner(srv.store, &stubProvider{}, reg))

	slug := createWSWithCollections(t, srv)
	item := createItem(t, srv, slug, "tasks", map[string]interface{}{"title": "Staged", "fields": `{"status":"open"}`})
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil || ws == nil {
		t.Fatalf("workspace: %v", err)
	}
	keys := []string{decision.AttentionNeedsHuman, decision.AttentionEffort, "effort_medium", decision.AttentionHighStakes}
	var rows []models.ItemDecision
	for _, k := range keys {
		rows = append(rows, models.ItemDecision{
			ItemID: item.ID, QuestionSet: decision.AttentionSetName, QuestionKey: k, Kind: "noul",
			Answer: []byte(`{"type":"noul","noul":0.9}`), Provider: "fake", Model: "m",
			StateHash: "h", QuestionHash: "q-" + k, ItemSeq: 1,
		})
	}
	if err := srv.store.InsertItemDecisions(ws.ID, rows); err != nil {
		t.Fatal(err)
	}

	show := itemJSONKeys(t, srv, slug, item.Slug)
	if raw, ok := show["decisions"]; ok {
		t.Errorf("item show carries a decisions member with only attention answers stored: %s", raw)
	}

	all := getDecisions(t, srv, slug, item.Slug)
	seen := map[string]bool{}
	for _, d := range all.Decisions {
		seen[d.QuestionKey] = true
	}
	for _, k := range keys {
		if !seen[k] {
			t.Errorf("the decisions endpoint is missing %s (it is RAW by design)", k)
		}
	}
}
