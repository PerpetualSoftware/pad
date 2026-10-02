package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// The CLI's history summary (stdio MCP's pad_item.history) carries the
// editor's display name, as the remote transport does (TASK-3321, 0.63).
func TestItemHistorySummaryCarriesActorName(t *testing.T) {
	got := toItemVersionSummaries([]models.Version{
		{ID: "v2", CreatedBy: "user", Source: "web", ActorName: "Olivia Owner"},
		{ID: "v1", CreatedBy: "system", Source: "recovery"},
	})
	b, _ := json.Marshal(got)
	if !strings.Contains(string(b), `"actor_name":"Olivia Owner"`) {
		t.Errorf("summary lacks the editor's name: %s", b)
	}
	if strings.Count(string(b), "actor_name") != 1 {
		t.Errorf("a row with no user should omit actor_name: %s", b)
	}
}
