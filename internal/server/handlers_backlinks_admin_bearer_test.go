package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestCrossWorkspaceBacklinks_AdminBearer_OnlySeesMembershipWorkspaces
// is the BUG-1617 HTTP integration test. It exercises the full chain
// (handler → guestResourceFilterCore → Store.GetCrossWorkspaceBacklinks
// → Store.ResolveBacklinksVisibility) with both auth surfaces an admin
// user can present:
//
//   - Cookie session admin → sees cross-ws backlinks from EVERY
//     workspace (BUG-1616's preserved web-UI affordance).
//   - PAT-bearer admin → sees cross-ws backlinks ONLY from workspaces
//     they're a member of. The non-member workspace's source row is
//     stripped at the GetUserWorkspaces enumeration step.
//
// Setup: two workspaces — wsA owned by the admin (target lives here),
// wsB owned by a different user, with a source item that wiki-links
// the target via `[[wsA-slug::TASK-N]]`.
func TestCrossWorkspaceBacklinks_AdminBearer_OnlySeesMembershipWorkspaces(t *testing.T) {
	srv := testServer(t)

	admin, err := srv.store.CreateUser(models.UserCreate{
		Email: "admin@example.com", Name: "Admin", Password: "correct-horse-battery-staple", Role: "admin",
	})
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	owner, err := srv.store.CreateUser(models.UserCreate{
		Email: "owner@example.com", Name: "Owner", Password: "correct-horse-battery-staple", Role: "member",
	})
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	// wsA: admin owns. Target lives here. Admin IS a member.
	wsA, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "AdminHomeWS", OwnerID: admin.ID})
	if err != nil {
		t.Fatalf("create wsA: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(wsA.ID, admin.ID, "owner"); err != nil {
		t.Fatalf("add admin to wsA: %v", err)
	}
	// wsB: owner owns. Admin is NOT a member. Source with cross-ws
	// wiki-link lives here.
	wsB, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "OtherUserWS", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("create wsB: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(wsB.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("add owner to wsB: %v", err)
	}

	tasksA, err := srv.store.CreateCollection(wsA.ID, models.CollectionCreate{
		Name: "Tasks", Slug: "tasks", Prefix: "TASK",
	})
	if err != nil {
		t.Fatalf("create tasks (wsA): %v", err)
	}
	notesB, err := srv.store.CreateCollection(wsB.ID, models.CollectionCreate{
		Name: "Notes", Slug: "notes", Prefix: "NOTE",
	})
	if err != nil {
		t.Fatalf("create notes (wsB): %v", err)
	}

	target, err := srv.store.CreateItem(wsA.ID, tasksA.ID, models.ItemCreate{
		Title: "Cross target", CreatedBy: "user", Source: "test",
	})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	// Source in wsB referencing wsA's target via [[wsA-slug::TASK-N]].
	// item_number is the canonical ref form the cross-ws resolver
	// expects; ItemCreate doesn't set it directly, so we derive it
	// from the target after creation.
	if target.ItemNumber == nil || *target.ItemNumber <= 0 {
		t.Fatalf("target item_number missing/zero (need it for ref): %+v", target)
	}
	targetRef := "TASK-" + strconv.Itoa(*target.ItemNumber)
	if _, err := srv.store.CreateItem(wsB.ID, notesB.ID, models.ItemCreate{
		Title:     "Cross source",
		Content:   "See [[" + wsA.Slug + "::" + targetRef + "]] for context.",
		CreatedBy: "user", Source: "test",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}

	// Issue an admin PAT. Token's workspace_id satisfies the NOT
	// NULL constraint; the user-owned-token shape (apiToken.UserID
	// set) is what RequireWorkspaceAccess actually keys on.
	tok, err := srv.store.CreateAPIToken(admin.ID, models.APITokenCreate{
		Name: "admin-bearer-test", WorkspaceID: wsA.ID,
	}, 0, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	// Cookie session for the same admin — exercises the preserved
	// admin bypass.
	sessTok, err := srv.store.CreateSession(admin.ID, "web-test", "192.0.2.1", "", webSessionTTL)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Endpoint under test: backlinks for the target item in wsA.
	// guestResourceFilterCore and GetCrossWorkspaceBacklinks both
	// run inside the handler so this exercises the full BUG-1617
	// fix chain.
	backlinksURL := "/api/v1/workspaces/" + wsA.Slug + "/items/" + target.Slug + "/backlinks"

	t.Run("cookie session admin sees cross-ws backlink from non-member workspace", func(t *testing.T) {
		rr := doRequestWithCookie(srv, "GET", backlinksURL, nil, sessTok)
		if rr.Code != http.StatusOK {
			t.Fatalf("cookie admin: got %d, want 200; body=%s", rr.Code, rr.Body.String())
		}
		var rows []models.Backlink
		if err := json.Unmarshal(rr.Body.Bytes(), &rows); err != nil {
			t.Fatalf("decode: %v body=%s", err, rr.Body.String())
		}
		// Expect at least one cross-ws row (SourceWorkspaceSlug
		// populated only for cross-ws hits — see models/backlink.go).
		var foundCross bool
		for _, r := range rows {
			if r.SourceWorkspaceSlug == wsB.Slug {
				foundCross = true
			}
		}
		if !foundCross {
			t.Errorf("cookie admin should see cross-ws backlink from wsB; got %d rows: %+v", len(rows), rows)
		}
	})

	t.Run("bearer admin does NOT see cross-ws backlink from non-member workspace (BUG-1617)", func(t *testing.T) {
		rr := doRequestWithBearer(srv, "GET", backlinksURL, tok.Token, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("bearer admin: got %d, want 200; body=%s", rr.Code, rr.Body.String())
		}
		var rows []models.Backlink
		if err := json.Unmarshal(rr.Body.Bytes(), &rows); err != nil {
			t.Fatalf("decode: %v body=%s", err, rr.Body.String())
		}
		for _, r := range rows {
			if r.SourceWorkspaceSlug == wsB.Slug {
				t.Errorf("bearer admin must NOT see cross-ws backlink from wsB (non-member workspace); leaked row=%+v", r)
			}
		}
	})
}
