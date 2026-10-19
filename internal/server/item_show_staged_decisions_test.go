package server

import (
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/decision"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3921 F3, through the router (CONVE-19): item show carries
// needs_human_decision and none of the staged keys, while the decisions
// endpoint returns all four.
func TestItemShow_OmitsStagedDecisionKeys(t *testing.T) {
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
	raw, ok := show["decisions"]
	if !ok {
		t.Fatal("item show carries no decisions member")
	}
	got := string(raw)
	if !strings.Contains(got, decision.AttentionNeedsHuman) {
		t.Errorf("item show lost needs_human_decision: %s", got)
	}
	for _, k := range keys[1:] {
		if strings.Contains(got, `"`+k+`"`) {
			t.Errorf("item show carries staged key %s: %s", k, got)
		}
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
