package server

import (
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestAgendaAgentRoleFeeds(t *testing.T) {
	w := newAgendaWorld(t, "tasks")
	w.srv.agendaClock = func() time.Time { return meAgendaNow }
	sc := oneShot()
	sc.Audience = "agent"
	owned := w.item(t, "tasks", "Owned reminder", `{}`, sc)
	unowned := w.item(t, "tasks", "Unowned reminder", `{}`, sc)
	role, err := w.srv.store.CreateAgentRole(w.ws.ID, models.AgentRoleCreate{Name: "Atlas", Slug: "atlas"})
	if err != nil {
		t.Fatal(err)
	}
	feed := createTestCalendarFeed(t, w.srv, w.owner, "agent")
	before := fetchTestCalendarFeed(w.srv, feed.Token)
	if before.Code != 200 || !strings.Contains(before.Body.String(), "Owned reminder") {
		t.Fatalf("initial ICS status %d, missing reminder", before.Code)
	}
	meAgendaExec(t, w, `UPDATE items SET agent_role_id = ? WHERE id = ?`, role.ID, owned.ID)
	for _, item := range []*models.Item{owned, unowned} {
		if err := w.srv.store.SetForecast(w.ws.ID, item.ID, w.owner.ID, "user", "2031-04-02", "hand estimate"); err != nil {
			t.Fatal(err)
		}
	}
	for _, endpoint := range []string{"workspace", "me"} {
		t.Run(endpoint, func(t *testing.T) {
			var response map[string]any
			if endpoint == "workspace" {
				response = w.ok(t, w.owner, agendaWindow)
			} else {
				response = meAgendaOK(t, w, w.owner, agendaWindow)
			}
			entries := entriesOf(response)
			if endpoint == "workspace" {
				for _, value := range response["priority_list"].([]any) {
					entry := value.(map[string]any)
					if entry["item_ref"] == owned.Ref && (entry["agent_role_name"] != "Atlas" || entry["agent_role_slug"] != "atlas" || entry["audience"] != "agent") {
						t.Errorf("attention outside the loaded window lacks owner/audience: %v", entry)
					}
				}
			}
			if len(entries) != 2 {
				t.Fatalf("got %d entries, want 2", len(entries))
			}
			for _, entry := range entries {
				if entry["item_ref"] == owned.Ref {
					if entry["agent_role_name"] != "Atlas" || entry["agent_role_slug"] != "atlas" {
						t.Errorf("owned schedule lacks role: %v", entry)
					}
				} else {
					for _, field := range []string{"agent_role_name", "agent_role_slug"} {
						if _, exists := entry[field]; exists {
							t.Errorf("unowned schedule exposes %s", field)
						}
					}
				}
			}
			if endpoint == "workspace" {
				response = w.ok(t, w.owner, forecastApril)
			} else {
				response = meAgendaOK(t, w, w.owner, forecastApril)
			}
			if len(entriesOf(response)) != 0 {
				t.Fatalf("forecast window contains occurrences: %v", response["entries"])
			}
			forecasts := response["forecasts"].([]any)
			if len(forecasts) != 2 {
				t.Fatalf("got %d forecasts, want 2", len(forecasts))
			}
			for _, value := range forecasts {
				row := value.(map[string]any)
				if row["audience"] != "agent" {
					t.Errorf("forecast lacks agent audience: %v", row)
				}
				if row["item_ref"] == owned.Ref {
					if row["agent_role_name"] != "Atlas" || row["agent_role_slug"] != "atlas" {
						t.Errorf("owned forecast lacks role: %v", row)
					}
				} else {
					if row["item_ref"] != unowned.Ref {
						t.Fatalf("unexpected forecast: %v", row)
					}
					for _, field := range []string{"agent_role_name", "agent_role_slug"} {
						if _, exists := row[field]; exists {
							t.Errorf("unowned forecast exposes %s", field)
						}
					}
				}
			}
		})
	}
	t.Run("ics", func(t *testing.T) {
		after := fetchTestCalendarFeed(w.srv, feed.Token)
		if after.Code != 200 || after.Body.String() != before.Body.String() {
			t.Fatal("assigning an agent role and setting forecasts changed ICS bytes")
		}
		for _, value := range []string{"agent_role_name", "agent_role_slug", "Atlas", "atlas"} {
			if strings.Contains(after.Body.String(), value) {
				t.Errorf("ICS exposes role metadata %q", value)
			}
		}
	})
}
