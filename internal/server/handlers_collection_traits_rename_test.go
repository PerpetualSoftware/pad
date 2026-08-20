package server

import (
	"net/http"
	"testing"
)

// TestConventionsCollectionSurvivesRename locks BUG-2702's conventions half.
//
// Before collection traits, bootstrap queried the literal slug "conventions".
// Store.UpdateCollection re-slugs on any name change, and renaming a
// collection is a documented onboarding step (TASK-1510; the onboard playbook
// tells agents to adapt collection names to the project), so a rename
// silently emptied every agent's always-on rule set with no error anywhere.
//
// OBSERVED FAILING on origin/main 6e7d34c6 before the fix: conventions 1 -> 0
// and convention_index 1 -> 0 after the rename, with the item still present in
// the renamed collection. The control leg below is what makes the failure
// meaningful — it proves the payload was populated before the rename, so a
// zero afterwards is the rename's doing and not an empty fixture.
func TestConventionsCollectionSurvivesRename(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// Seed one active, always-on convention — the exact shape bootstrap
	// promises to hand every agent at boot.
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/conventions/items", map[string]interface{}{
		"title":   "Use conventional commits",
		"content": "Commit messages follow the conventional-commit format.",
		"fields":  map[string]string{"status": "active", "trigger": "always"},
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create convention: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	// Control leg: before the rename, the convention is in the bootstrap.
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/agent/bootstrap", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("bootstrap (before): expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var before AgentBootstrap
	parseJSON(t, rr, &before)
	if len(before.Conventions) != 1 {
		t.Fatalf("control leg failed: conventions before rename = %d, want 1", len(before.Conventions))
	}
	if len(before.ConventionIndex) != 1 {
		t.Fatalf("control leg failed: convention_index before rename = %d, want 1", len(before.ConventionIndex))
	}

	// The documented onboarding mutation: rename the collection to fit the
	// project's vocabulary (TASK-1510; the onboard playbook tells agents to
	// do exactly this). UpdateCollection re-slugs from the new name.
	rr = doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/collections/conventions", map[string]interface{}{
		"name": "Rules",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("rename collection: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// The item is untouched — still active, still always-on, still there.
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/collections/rules/items", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list renamed collection: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var items []map[string]interface{}
	parseJSON(t, rr, &items)
	if len(items) != 1 {
		t.Fatalf("renamed collection lost its items: got %d, want 1", len(items))
	}

	// The defect: bootstrap queries the literal slug "conventions", which no
	// longer resolves, so the agent's always-on rules silently vanish.
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/agent/bootstrap", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("bootstrap (after): expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var after AgentBootstrap
	parseJSON(t, rr, &after)
	if len(after.Conventions) != 1 {
		t.Errorf("regression: conventions after rename = %d, want 1", len(after.Conventions))
	}
	if len(after.ConventionIndex) != 1 {
		t.Errorf("regression: convention_index after rename = %d, want 1", len(after.ConventionIndex))
	}
}

// TestPlaybooksCollectionSurvivesRename locks BUG-2702's playbooks half, which
// is the worse of the two: a rename unregistered every invokable playbook, so
// `/pad ship` fell through to natural-language routing with no sign that the
// playbook still existed.
//
// OBSERVED FAILING on origin/main 6e7d34c6 before the fix: playbooks 1 -> 0
// and GET /playbooks/ship 200 -> 404.
func TestPlaybooksCollectionSurvivesRename(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/playbooks/items", map[string]interface{}{
		"title":   "Ship a list of tasks",
		"content": "## Steps\n\n1. Do the thing.",
		"fields":  map[string]string{"status": "active", "invocation_slug": "ship"},
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create playbook: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	// Control leg: before the rename, the playbook is both listed in
	// bootstrap and resolvable by its invocation slug.
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/agent/bootstrap", nil)
	var before AgentBootstrap
	parseJSON(t, rr, &before)
	if len(before.Playbooks) != 1 {
		t.Fatalf("control leg failed: playbooks before rename = %d, want 1", len(before.Playbooks))
	}
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/playbooks/ship", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("control leg failed: resolve playbook before rename = %d, want 200", rr.Code)
	}

	rr = doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/collections/playbooks", map[string]interface{}{
		"name": "Procedures",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("rename collection: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/agent/bootstrap", nil)
	var after AgentBootstrap
	parseJSON(t, rr, &after)
	if len(after.Playbooks) != 1 {
		t.Errorf("regression: playbooks after rename = %d, want 1", len(after.Playbooks))
	}

	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/playbooks/ship", nil)
	if rr.Code != http.StatusOK {
		t.Errorf("regression: resolve playbook after rename = %d, want 200", rr.Code)
	}
}
