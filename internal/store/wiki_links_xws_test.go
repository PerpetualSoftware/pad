package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestWikiLinks_CrossWorkspaceRefIndexedAndQueryable is the headline
// Phase 2b test: a `[[other-ws::TASK-5]]` body in workspace B indexes
// a workspace_ref row, and querying TASK-5's backlinks from workspace
// A surfaces the cross-ws source — provided the requester has
// visibility into workspace B. PLAN-1593 / TASK-1597.
func TestWikiLinks_CrossWorkspaceRefIndexedAndQueryable(t *testing.T) {
	s := testStore(t)
	user := createTestUser(t, s, "alice@example.com", "Alice", "password123")

	// Two workspaces, both owned by `user` so the requester is an
	// admin-equivalent (owner) in each. The simpler ACL path lets us
	// focus on the index correctness in this test.
	wsA := createTestWorkspace(t, s, "Workspace A")
	wsB := createTestWorkspace(t, s, "Workspace B")
	if err := s.AddWorkspaceMember(wsA.ID, user.ID, "owner"); err != nil {
		t.Fatalf("add user to wsA: %v", err)
	}
	if err := s.AddWorkspaceMember(wsB.ID, user.ID, "owner"); err != nil {
		t.Fatalf("add user to wsB: %v", err)
	}

	colA := createTestCollection(t, s, wsA.ID, "Tasks")
	colB := createTestCollection(t, s, wsB.ID, "Notes")

	// Target lives in workspace A.
	target := createTestItem(t, s, wsA.ID, colA.ID, "Cross target", "")
	targetRef := refOf(target)

	// Source lives in workspace B, references target via `[[wsA.slug::REF]]`.
	body := "Cross-link to [[" + wsA.Slug + "::" + targetRef + "]] from B."
	source := createTestItem(t, s, wsB.ID, colB.ID, "Cross source", body)

	// Cross-ws query from A's perspective should find the source.
	got, err := s.GetCrossWorkspaceBacklinks(wsA.ID, targetRef, user.ID, 50, 0)
	if err != nil {
		t.Fatalf("GetCrossWorkspaceBacklinks: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 cross-ws backlink, got %d: %+v", len(got), got)
	}
	bl := got[0]
	if bl.SourceItemID != source.ID {
		t.Errorf("SourceItemID: got %q, want %q", bl.SourceItemID, source.ID)
	}
	if bl.SourceWorkspaceSlug != wsB.Slug {
		t.Errorf("SourceWorkspaceSlug: got %q, want %q", bl.SourceWorkspaceSlug, wsB.Slug)
	}
}

// TestWikiLinks_CrossWorkspaceNonMemberDoesNotSee covers the
// visibility contract: a user with NO access to workspace B (no
// membership, no grants) cannot see backlinks from B even when
// querying their own workspace A's target. The cross-ws traversal
// enumerates only workspaces the requester has access to via
// GetUserWorkspaces.
func TestWikiLinks_CrossWorkspaceNonMemberDoesNotSee(t *testing.T) {
	s := testStore(t)

	owner := createTestUser(t, s, "owner@example.com", "Owner", "password123")
	outsider := createTestUser(t, s, "outsider@example.com", "Outsider", "password123")

	wsA := createTestWorkspace(t, s, "Workspace A")
	wsB := createTestWorkspace(t, s, "Workspace B")
	if err := s.AddWorkspaceMember(wsA.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("owner→wsA: %v", err)
	}
	if err := s.AddWorkspaceMember(wsB.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("owner→wsB: %v", err)
	}
	// Outsider is a member of A only — has NO access to B.
	if err := s.AddWorkspaceMember(wsA.ID, outsider.ID, "editor"); err != nil {
		t.Fatalf("outsider→wsA: %v", err)
	}

	colA := createTestCollection(t, s, wsA.ID, "Tasks")
	colB := createTestCollection(t, s, wsB.ID, "Notes")

	target := createTestItem(t, s, wsA.ID, colA.ID, "Target", "")
	targetRef := refOf(target)
	createTestItem(t, s, wsB.ID, colB.ID, "Source in B",
		"See [["+wsA.Slug+"::"+targetRef+"]].")

	// Owner sees the cross-ws backlink.
	got, _ := s.GetCrossWorkspaceBacklinks(wsA.ID, targetRef, owner.ID, 50, 0)
	if len(got) != 1 {
		t.Errorf("owner should see cross-ws backlink, got %d", len(got))
	}
	// Outsider does NOT — they have no access to workspace B.
	got2, _ := s.GetCrossWorkspaceBacklinks(wsA.ID, targetRef, outsider.ID, 50, 0)
	if len(got2) != 0 {
		t.Errorf("outsider should NOT see cross-ws backlink, got %d", len(got2))
	}
}

// TestWikiLinks_CrossWorkspaceGuestSeesGrantedCollectionOnly: a guest
// in workspace B (only access via a collection_grant on one
// collection) should see backlinks from that collection but not from
// other collections in B. Mirrors the same-ws Phase 1 visibility
// model — Codex round-1/2 P1 — across the cross-ws boundary.
func TestWikiLinks_CrossWorkspaceGuestSeesGrantedCollectionOnly(t *testing.T) {
	s := testStore(t)

	owner := createTestUser(t, s, "owner@example.com", "Owner", "password123")
	guest := createTestUser(t, s, "guest@example.com", "Guest", "password123")

	wsA := createTestWorkspace(t, s, "Workspace A")
	wsB := createTestWorkspace(t, s, "Workspace B")
	for _, ws := range []*models.Workspace{wsA, wsB} {
		if err := s.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
			t.Fatalf("owner→%s: %v", ws.Slug, err)
		}
	}
	// Guest is a member of A (so they can ask for A's backlinks),
	// but has only a guest-style collection_grant in B.
	if err := s.AddWorkspaceMember(wsA.ID, guest.ID, "editor"); err != nil {
		t.Fatalf("guest→wsA: %v", err)
	}

	colA := createTestCollection(t, s, wsA.ID, "Tasks")
	visibleColB := createTestCollection(t, s, wsB.ID, "Visible") // guest will get grant on this
	hiddenColB := createTestCollection(t, s, wsB.ID, "Hidden")   // guest CANNOT see

	// Guest's collection grant in B — on Visible only.
	if _, err := s.CreateCollectionGrant(wsB.ID, visibleColB.ID, guest.ID, "view", owner.ID); err != nil {
		t.Fatalf("grant visible collection: %v", err)
	}

	target := createTestItem(t, s, wsA.ID, colA.ID, "Target", "")
	targetRef := refOf(target)
	visibleSrc := createTestItem(t, s, wsB.ID, visibleColB.ID, "Visible source",
		"See [["+wsA.Slug+"::"+targetRef+"]].")
	hiddenSrc := createTestItem(t, s, wsB.ID, hiddenColB.ID, "Hidden source",
		"Also see [["+wsA.Slug+"::"+targetRef+"]].")
	_ = hiddenSrc

	// Guest sees ONLY the visible-collection source.
	got, err := s.GetCrossWorkspaceBacklinks(wsA.ID, targetRef, guest.ID, 50, 0)
	if err != nil {
		t.Fatalf("GetCrossWorkspaceBacklinks: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("guest should see exactly 1 cross-ws backlink (visible-collection only), got %d: %+v", len(got), got)
	}
	if got[0].SourceItemID != visibleSrc.ID {
		t.Errorf("guest saw wrong source: got %q want %q", got[0].SourceItemID, visibleSrc.ID)
	}
	if got[0].SourceWorkspaceSlug != wsB.Slug {
		t.Errorf("SourceWorkspaceSlug: got %q want %q", got[0].SourceWorkspaceSlug, wsB.Slug)
	}
}

// TestWikiLinks_CrossWorkspaceGuestItemGrantOnlyVisible: a guest with
// an item-level grant on ONE source item in workspace B sees only that
// item, even if other items in B's collections reference the same
// target. This is the cross-ws equivalent of
// TestWikiLinks_ItemGrantPagination from Phase 1.
func TestWikiLinks_CrossWorkspaceGuestItemGrantOnlyVisible(t *testing.T) {
	s := testStore(t)

	owner := createTestUser(t, s, "owner@example.com", "Owner", "password123")
	guest := createTestUser(t, s, "guest@example.com", "Guest", "password123")

	wsA := createTestWorkspace(t, s, "Workspace A")
	wsB := createTestWorkspace(t, s, "Workspace B")
	for _, ws := range []*models.Workspace{wsA, wsB} {
		if err := s.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
			t.Fatalf("owner→%s: %v", ws.Slug, err)
		}
	}
	if err := s.AddWorkspaceMember(wsA.ID, guest.ID, "editor"); err != nil {
		t.Fatalf("guest→wsA: %v", err)
	}

	colA := createTestCollection(t, s, wsA.ID, "Tasks")
	colB := createTestCollection(t, s, wsB.ID, "Notes")

	target := createTestItem(t, s, wsA.ID, colA.ID, "Target", "")
	targetRef := refOf(target)

	// Two sources in workspace B's same collection, both referencing
	// the target. Guest gets an item-grant on src1 only.
	src1 := createTestItem(t, s, wsB.ID, colB.ID, "Granted source",
		"See [["+wsA.Slug+"::"+targetRef+"]].")
	src2 := createTestItem(t, s, wsB.ID, colB.ID, "Ungranted source",
		"Also see [["+wsA.Slug+"::"+targetRef+"]].")
	_ = src2

	if _, err := s.CreateItemGrant(wsB.ID, src1.ID, guest.ID, "view", owner.ID); err != nil {
		t.Fatalf("grant item: %v", err)
	}

	got, _ := s.GetCrossWorkspaceBacklinks(wsA.ID, targetRef, guest.ID, 50, 0)
	if len(got) != 1 {
		t.Fatalf("guest should see exactly 1 cross-ws backlink (the granted item), got %d: %+v", len(got), got)
	}
	if got[0].SourceItemID != src1.ID {
		t.Errorf("got %q want granted src1 %q", got[0].SourceItemID, src1.ID)
	}
}

// TestWikiLinks_CrossWorkspaceUnknownSlugBroken: a `[[unknown-ws::TASK-1]]`
// body where the workspace slug doesn't exist persists as a row with
// target_workspace_id = NULL. The cross-ws query for that ref against
// the unknown workspace returns nothing (the FK column doesn't match
// any wsID). Documents the broken-link semantics consistent with the
// renderer's resolver-route 404 path.
func TestWikiLinks_CrossWorkspaceUnknownSlugBroken(t *testing.T) {
	s := testStore(t)
	user := createTestUser(t, s, "alice@example.com", "Alice", "password123")
	ws := createTestWorkspace(t, s, "Workspace A")
	if err := s.AddWorkspaceMember(ws.ID, user.ID, "owner"); err != nil {
		t.Fatalf("add user: %v", err)
	}
	col := createTestCollection(t, s, ws.ID, "Tasks")
	// Source references a workspace slug that doesn't exist.
	createTestItem(t, s, ws.ID, col.ID, "Source",
		"Cross [[nonexistent-ws::TASK-1]] over.")

	// Some other workspace exists too so the cross-ws query has
	// something to iterate. The query for "nonexistent-ws"'s ref
	// against our real wsB obviously misses.
	wsB := createTestWorkspace(t, s, "Workspace B")
	if err := s.AddWorkspaceMember(wsB.ID, user.ID, "owner"); err != nil {
		t.Fatalf("add user wsB: %v", err)
	}

	got, _ := s.GetCrossWorkspaceBacklinks(wsB.ID, "TASK-1", user.ID, 50, 0)
	if len(got) != 0 {
		t.Errorf("broken cross-ws ref should not surface in cross-ws backlinks, got %d", len(got))
	}
}

// TestWikiLinks_CrossWorkspaceSourceWorkspaceSlugPopulated double-
// checks the wire-shape promise: cross-ws Backlink rows MUST set
// SourceWorkspaceSlug so the renderer can route the link to the
// foreign workspace. Same-ws rows leave it empty.
func TestWikiLinks_CrossWorkspaceSourceWorkspaceSlugPopulated(t *testing.T) {
	s := testStore(t)
	user := createTestUser(t, s, "alice@example.com", "Alice", "password123")

	wsA := createTestWorkspace(t, s, "Workspace A")
	wsB := createTestWorkspace(t, s, "Workspace B")
	for _, ws := range []*models.Workspace{wsA, wsB} {
		if err := s.AddWorkspaceMember(ws.ID, user.ID, "owner"); err != nil {
			t.Fatalf("member: %v", err)
		}
	}
	colA := createTestCollection(t, s, wsA.ID, "Tasks")
	colB := createTestCollection(t, s, wsB.ID, "Notes")

	target := createTestItem(t, s, wsA.ID, colA.ID, "Cross target", "")
	targetRef := refOf(target)
	createTestItem(t, s, wsB.ID, colB.ID, "Cross source",
		"See [["+wsA.Slug+"::"+targetRef+"]].")

	got, _ := s.GetCrossWorkspaceBacklinks(wsA.ID, targetRef, user.ID, 50, 0)
	if len(got) != 1 {
		t.Fatalf("expected 1 cross-ws row, got %d", len(got))
	}
	if got[0].SourceWorkspaceSlug != wsB.Slug {
		t.Errorf("cross-ws row missing SourceWorkspaceSlug: got %q want %q", got[0].SourceWorkspaceSlug, wsB.Slug)
	}

	// Same-ws GetBacklinks should leave SourceWorkspaceSlug empty.
	// (Add a same-ws source so we have something to compare.)
	createTestItem(t, s, wsA.ID, colA.ID, "Same-ws source", "Link [["+targetRef+"]].")
	sameBls, _ := s.GetBacklinks(target.ID, wsA.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if len(sameBls) != 1 {
		t.Fatalf("expected 1 same-ws backlink, got %d", len(sameBls))
	}
	if sameBls[0].SourceWorkspaceSlug != "" {
		t.Errorf("same-ws row leaked SourceWorkspaceSlug: got %q want empty", sameBls[0].SourceWorkspaceSlug)
	}
}

// TestResolveBacklinksVisibility_RoleMatrix exercises the request-
// independent ACL helper across the four caller-visible role
// shapes: admin, full-access member, restricted member with grants,
// guest. Each case asserts the (fullCollIDs, grantedItemIDs) shape
// against the documented contract. PLAN-1593 / TASK-1597.
func TestResolveBacklinksVisibility_RoleMatrix(t *testing.T) {
	s := testStore(t)
	owner := createTestUser(t, s, "owner@example.com", "Owner", "password123")
	ws := createTestWorkspace(t, s, "Test")
	if err := s.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("add owner: %v", err)
	}

	t.Run("admin user returns unrestricted (nil/nil)", func(t *testing.T) {
		admin, err := s.CreateUser(models.UserCreate{
			Email: "admin@example.com", Name: "Admin", Password: "password123",
			Role: "admin",
		})
		if err != nil {
			t.Fatalf("create admin: %v", err)
		}
		full, granted, err := s.ResolveBacklinksVisibility(admin.ID, ws.ID, false)
		if err != nil {
			t.Fatalf("ResolveBacklinksVisibility: %v", err)
		}
		if full != nil || granted != nil {
			t.Errorf("admin should be unrestricted (nil/nil), got %v / %v", full, granted)
		}
	})

	t.Run("full-access member returns unrestricted", func(t *testing.T) {
		full, granted, err := s.ResolveBacklinksVisibility(owner.ID, ws.ID, false)
		if err != nil {
			t.Fatalf("ResolveBacklinksVisibility: %v", err)
		}
		if full != nil || granted != nil {
			t.Errorf("full-access member should be unrestricted, got %v / %v", full, granted)
		}
	})

	t.Run("guest with grants returns grant-only lists", func(t *testing.T) {
		guest := createTestUser(t, s, "guest@example.com", "Guest", "password123")
		col := createTestCollection(t, s, ws.ID, "GuestColl")
		if _, err := s.CreateCollectionGrant(ws.ID, col.ID, guest.ID, "view", owner.ID); err != nil {
			t.Fatalf("grant collection: %v", err)
		}
		full, granted, err := s.ResolveBacklinksVisibility(guest.ID, ws.ID, false)
		if err != nil {
			t.Fatalf("ResolveBacklinksVisibility: %v", err)
		}
		if len(full) != 1 || full[0] != col.ID {
			t.Errorf("guest fullCollIDs: got %v want [%q]", full, col.ID)
		}
		if len(granted) != 0 {
			t.Errorf("guest grantedItemIDs: got %v want []", granted)
		}
	})

	t.Run("non-member non-grant user returns empty lists", func(t *testing.T) {
		stranger := createTestUser(t, s, "stranger@example.com", "Stranger", "password123")
		full, granted, err := s.ResolveBacklinksVisibility(stranger.ID, ws.ID, false)
		if err != nil {
			t.Fatalf("ResolveBacklinksVisibility: %v", err)
		}
		// Both lists empty (not nil-nil) — caller interprets as
		// "no visibility" via BacklinksVisibility.Unrestricted=false
		// + both lists empty.
		if len(full) != 0 || len(granted) != 0 {
			t.Errorf("stranger should have empty visibility, got %v / %v", full, granted)
		}
	})
}
