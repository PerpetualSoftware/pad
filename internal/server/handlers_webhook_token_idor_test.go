package server

import (
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestWebhookTokenCrossWorkspaceIDOR is the TASK-266 regression guard: an owner
// of one workspace must not be able to delete or test another workspace's
// webhook, nor revoke its API token, by object ID reached through their OWN
// workspace's URL. Before the fix, handleDeleteWebhook / handleTestWebhook /
// handleDeleteToken looked the object up by ID with no workspace-ownership
// predicate, so any owner of any workspace could destroy another workspace's
// integrations given the object ID (cross-workspace IDOR).
func TestWebhookTokenCrossWorkspaceIDOR(t *testing.T) {
	srv := testServer(t)

	// Admin owns the VICTIM workspace (A) and its webhook + API token.
	adminToken := bootstrapFirstUser(t, srv, "victim-owner@test.com", "Victim")

	rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces",
		map[string]string{"name": "Victim WS"}, adminToken)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create victim ws: %d %s", rr.Code, rr.Body.String())
	}
	var wsA models.Workspace
	parseJSON(t, rr, &wsA)

	// Attacker is a separate user who legitimately OWNS a different workspace (B),
	// but is NOT a member of A.
	rr = doRequestWithCookie(srv, "POST", "/api/v1/auth/register", map[string]string{
		"email":    "attacker@test.com",
		"name":     "Attacker",
		"password": "correct-horse-battery-staple",
	}, adminToken)
	if rr.Code != http.StatusCreated {
		t.Fatalf("register attacker: %d %s", rr.Code, rr.Body.String())
	}
	attacker, err := srv.store.GetUserByEmail("attacker@test.com")
	if err != nil || attacker == nil {
		t.Fatalf("find attacker user: %v", err)
	}

	rr = doRequestWithCookie(srv, "POST", "/api/v1/workspaces",
		map[string]string{"name": "Attacker WS"}, adminToken)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create attacker ws: %d %s", rr.Code, rr.Body.String())
	}
	var wsB models.Workspace
	parseJSON(t, rr, &wsB)
	if err := srv.store.AddWorkspaceMember(wsB.ID, attacker.ID, "owner"); err != nil {
		t.Fatalf("add attacker as owner of B: %v", err)
	}
	attackerToken := loginUser(t, srv, "attacker@test.com", "correct-horse-battery-staple")

	// Victim webhook in workspace A. 8.8.8.8 is a public, non-reserved literal
	// IP so ValidateWebhookURL passes without a DNS lookup.
	rr = doRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+wsA.Slug+"/webhooks",
		map[string]interface{}{"url": "https://8.8.8.8/hook"}, adminToken)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create victim webhook: %d %s", rr.Code, rr.Body.String())
	}
	var hook models.Webhook
	parseJSON(t, rr, &hook)

	// Victim API token in workspace A.
	rr = doRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+wsA.Slug+"/tokens",
		map[string]interface{}{"name": "victim-token"}, adminToken)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create victim token: %d %s", rr.Code, rr.Body.String())
	}
	var tok models.APIToken
	parseJSON(t, rr, &tok)

	// --- Attack: reach A's objects through the attacker's OWN workspace B URL. ---
	// Each must 404 (object not found in workspace B), not act on A's object.
	t.Run("delete webhook cross-workspace", func(t *testing.T) {
		rr := doRequestWithCookie(srv, "DELETE",
			"/api/v1/workspaces/"+wsB.Slug+"/webhooks/"+hook.ID, nil, attackerToken)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d: %s", rr.Code, rr.Body.String())
		}
	})
	t.Run("test webhook cross-workspace", func(t *testing.T) {
		rr := doRequestWithCookie(srv, "POST",
			"/api/v1/workspaces/"+wsB.Slug+"/webhooks/"+hook.ID+"/test", nil, attackerToken)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d: %s", rr.Code, rr.Body.String())
		}
	})
	t.Run("delete token cross-workspace", func(t *testing.T) {
		rr := doRequestWithCookie(srv, "DELETE",
			"/api/v1/workspaces/"+wsB.Slug+"/tokens/"+tok.ID, nil, attackerToken)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d: %s", rr.Code, rr.Body.String())
		}
	})

	// --- The victim's objects must still exist (the attack mutated nothing). ---
	rr = doRequestWithCookie(srv, "GET", "/api/v1/workspaces/"+wsA.Slug+"/webhooks", nil, adminToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("list victim webhooks: %d %s", rr.Code, rr.Body.String())
	}
	var hooks []models.Webhook
	parseJSON(t, rr, &hooks)
	if len(hooks) != 1 || hooks[0].ID != hook.ID {
		t.Fatalf("victim webhook should survive the attack; got %+v", hooks)
	}

	rr = doRequestWithCookie(srv, "GET", "/api/v1/workspaces/"+wsA.Slug+"/tokens", nil, adminToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("list victim tokens: %d %s", rr.Code, rr.Body.String())
	}
	var toks []models.APIToken
	parseJSON(t, rr, &toks)
	found := false
	for _, x := range toks {
		if x.ID == tok.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("victim token should survive the attack; got %+v", toks)
	}

	// --- Control: the legitimate owner CAN act within their own workspace, so
	// the fix rejects only the cross-workspace case, not all deletes. ---
	t.Run("owner deletes own webhook", func(t *testing.T) {
		rr := doRequestWithCookie(srv, "DELETE",
			"/api/v1/workspaces/"+wsA.Slug+"/webhooks/"+hook.ID, nil, adminToken)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("expected 204, got %d: %s", rr.Code, rr.Body.String())
		}
	})
	t.Run("owner deletes own token", func(t *testing.T) {
		rr := doRequestWithCookie(srv, "DELETE",
			"/api/v1/workspaces/"+wsA.Slug+"/tokens/"+tok.ID, nil, adminToken)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("expected 204, got %d: %s", rr.Code, rr.Body.String())
		}
	})
}
