package store

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Tests for the per-user open set of workspace tabs (TASK-3256). They run on
// whichever dialect testStore gives (SQLite by default, Postgres under
// PAD_TEST_POSTGRES_URL): the store is where Postgres coverage for this unit
// lives, since internal/server's testServer is SQLite-only.

func tabsUser(t *testing.T, s *Store, name string) *models.User {
	t.Helper()
	u, err := s.CreateUser(models.UserCreate{Name: name, Email: strings.ToLower(name) + "@example.com", Username: strings.ToLower(name)})
	if err != nil {
		t.Fatalf("create user %s: %v", name, err)
	}
	return u
}

func tabsWorkspace(t *testing.T, s *Store, owner *models.User, name string) *models.Workspace {
	t.Helper()
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: name, OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("create workspace %s: %v", name, err)
	}
	if err := s.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("add owner %s: %v", name, err)
	}
	return ws
}

func tabIDs(t *testing.T, s *Store, userID string) []string {
	t.Helper()
	list, err := s.ListWorkspaceTabs(userID)
	rows := list.Rows
	if err != nil {
		t.Fatalf("list tabs: %v", err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.WorkspaceID)
	}
	return out
}

func tabRow(t *testing.T, s *Store, userID, wsID string) (WorkspaceTabRow, bool) {
	t.Helper()
	list, err := s.ListWorkspaceTabs(userID)
	rows := list.Rows
	if err != nil {
		t.Fatalf("list tabs: %v", err)
	}
	for _, r := range rows {
		if r.WorkspaceID == wsID {
			return r, true
		}
	}
	return WorkspaceTabRow{}, false
}

func sameIDs(a, b []string) bool {
	return strings.Join(a, ",") == strings.Join(b, ",")
}

// runTabsSeed re-executes the tab migration against the store's current data.
// The template database runs it before any row exists, so this is the only
// way to exercise the seed; the file is idempotent (CREATE ... IF NOT EXISTS,
// and the insert skips existing keys).
func runTabsSeed(t *testing.T, s *Store) {
	t.Helper()
	fsys, path := migrationsFS, "migrations/098_user_workspace_tabs.sql"
	if s.dialect.Driver() == DriverPostgres {
		fsys, path = pgMigrationsFS, "pgmigrations/073_user_workspace_tabs.sql"
	}
	body, err := fsys.ReadFile(path)
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if _, err := s.db.Exec(string(body)); err != nil {
		t.Fatalf("run migration: %v", err)
	}
}

// The seed rule (PLAN-3002 §3, Q8): each user's first six live member
// workspaces, in (sort_order, name) order, as durable tabs; nothing for a
// guest; a soft-deleted workspace is skipped rather than taking a slot.
func TestWorkspaceTabs_MigrationSeed(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	owner := tabsUser(t, s, "Seedowner")
	member := tabsUser(t, s, "Seedmember")
	guest := tabsUser(t, s, "Seedguest")

	// Eight memberships, plus one soft-deleted one ordered FIRST, so a seed
	// that forgot deleted_at would put it in slot 0.
	var want []string
	for i := 0; i < 8; i++ {
		ws := tabsWorkspace(t, s, owner, fmt.Sprintf("Seed %d", i))
		if err := s.AddWorkspaceMember(ws.ID, member.ID, "editor"); err != nil {
			t.Fatalf("add member: %v", err)
		}
		// Reverse the name order with sort_order, so a seed that sorted by
		// name alone gets the wrong six.
		if err := s.UpdateWorkspaceSortOrder(member.ID, ws.ID, 10-i); err != nil {
			t.Fatalf("sort order: %v", err)
		}
		want = append([]string{ws.ID}, want...)
	}
	want = want[:6]
	gone := tabsWorkspace(t, s, owner, "Seed gone")
	if err := s.AddWorkspaceMember(gone.ID, member.ID, "editor"); err != nil {
		t.Fatalf("add member: %v", err)
	}
	if err := s.UpdateWorkspaceSortOrder(member.ID, gone.ID, -5); err != nil {
		t.Fatalf("sort order: %v", err)
	}
	if err := s.DeleteWorkspace(gone.Slug); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	// The guest reaches one workspace through a collection grant only.
	gws := tabsWorkspace(t, s, owner, "Seed guested")
	coll, err := s.CreateCollection(gws.ID, models.CollectionCreate{Name: "Tasks", Slug: "tasks", Prefix: "TSK", Schema: `{"fields":[]}`})
	if err != nil {
		t.Fatalf("create collection: %v", err)
	}
	if _, err := s.CreateCollectionGrant(gws.ID, coll.ID, guest.ID, "view", owner.ID); err != nil {
		t.Fatalf("grant: %v", err)
	}

	if _, err := s.db.Exec(`DELETE FROM user_workspace_tabs`); err != nil {
		t.Fatalf("clear tabs: %v", err)
	}
	runTabsSeed(t, s)

	got := tabIDs(t, s, member.ID)
	if !sameIDs(got, want) {
		t.Fatalf("member seed:\n got %v\nwant %v", got, want)
	}
	list, _ := s.ListWorkspaceTabs(member.ID)
	rows := list.Rows
	for i, r := range rows {
		if r.Position != i || r.Ephemeral || r.LastRoute != "" {
			t.Fatalf("seeded row %d = %+v, want position %d, durable, no route", i, r, i)
		}
	}
	if got := tabIDs(t, s, guest.ID); len(got) != 0 {
		t.Fatalf("guest was seeded %v, want nothing", got)
	}

	// Re-running it changes nothing: the insert skips existing keys.
	runTabsSeed(t, s)
	if got := tabIDs(t, s, member.ID); !sameIDs(got, want) {
		t.Fatalf("seed not idempotent: %v", got)
	}
}

func TestWorkspaceTabs_OpenCloseReorderUpdate(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	u := tabsUser(t, s, "Tabber")
	a := tabsWorkspace(t, s, u, "A")
	b := tabsWorkspace(t, s, u, "B")
	c := tabsWorkspace(t, s, u, "C")
	d := tabsWorkspace(t, s, u, "D")
	if _, err := s.db.Exec(s.q(`DELETE FROM user_workspace_tabs WHERE user_id = ?`), u.ID); err != nil {
		t.Fatalf("clear: %v", err)
	}

	must := func(_ WorkspaceTabList, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.OpenWorkspaceTab(u.ID, a.ID, false))
	must(s.OpenWorkspaceTab(u.ID, b.ID, true))
	must(s.OpenWorkspaceTab(u.ID, c.ID, false))
	if got := tabIDs(t, s, u.ID); !sameIDs(got, []string{a.ID, b.ID, c.ID}) {
		t.Fatalf("after opens: %v", got)
	}

	// A second ephemeral open REPLACES the first, in its position.
	must(s.OpenWorkspaceTab(u.ID, d.ID, true))
	if got := tabIDs(t, s, u.ID); !sameIDs(got, []string{a.ID, d.ID, c.ID}) {
		t.Fatalf("ephemeral replace: %v", got)
	}
	if r, _ := tabRow(t, s, u.ID, d.ID); !r.Ephemeral {
		t.Fatalf("d should be ephemeral: %+v", r)
	}

	// An ephemeral open of a durable tab never demotes it.
	must(s.OpenWorkspaceTab(u.ID, a.ID, true))
	if r, _ := tabRow(t, s, u.ID, a.ID); r.Ephemeral {
		t.Fatalf("durable tab was demoted: %+v", r)
	}
	if r, _ := tabRow(t, s, u.ID, d.ID); !r.Ephemeral {
		t.Fatalf("re-opening a durable tab touched the ephemeral one: %+v", r)
	}

	// A durable open of the ephemeral tab pins it in place.
	must(s.OpenWorkspaceTab(u.ID, d.ID, false))
	if r, _ := tabRow(t, s, u.ID, d.ID); r.Ephemeral {
		t.Fatalf("durable open did not pin: %+v", r)
	}
	if got := tabIDs(t, s, u.ID); !sameIDs(got, []string{a.ID, d.ID, c.ID}) {
		t.Fatalf("pin moved the tab: %v", got)
	}

	// Reorder: listed first in that order, unknown ids ignored, unlisted
	// open tabs kept after in their old order.
	must(s.ReorderWorkspaceTabs(u.ID, []string{c.ID, "no-such-id", b.ID}))
	if got := tabIDs(t, s, u.ID); !sameIDs(got, []string{c.ID, a.ID, d.ID}) {
		t.Fatalf("reorder: %v", got)
	}
	// The positions are rewritten, not merely sorted into place: a tie
	// broken by created_at would hide a reorder that left rows unwritten.
	reorderedList, _ := s.ListWorkspaceTabs(u.ID)
	reordered := reorderedList.Rows
	for i, r := range reordered {
		if r.Position != i {
			t.Fatalf("after reorder, row %d (%s) has position %d", i, r.WorkspaceID, r.Position)
		}
	}

	// Update: pin and route; an unopened workspace is ErrNoRows.
	must(s.OpenWorkspaceTab(u.ID, b.ID, true))
	route := "/tabber/b/tasks"
	must(s.UpdateWorkspaceTab(u.ID, b.ID, WorkspaceTabUpdate{Pin: true, LastRoute: &route}))
	if r, _ := tabRow(t, s, u.ID, b.ID); r.Ephemeral || r.LastRoute != route {
		t.Fatalf("update: %+v", r)
	}
	empty := ""
	must(s.UpdateWorkspaceTab(u.ID, b.ID, WorkspaceTabUpdate{LastRoute: &empty}))
	if r, _ := tabRow(t, s, u.ID, b.ID); r.LastRoute != "" {
		t.Fatalf("route not cleared: %+v", r)
	}
	must(s.CloseWorkspaceTab(u.ID, b.ID))
	if _, err := s.UpdateWorkspaceTab(u.ID, b.ID, WorkspaceTabUpdate{Pin: true}); err == nil || !strings.Contains(err.Error(), "no rows") {
		t.Fatalf("update of a closed tab: err %v, want sql.ErrNoRows", err)
	}

	// Close is idempotent.
	must(s.CloseWorkspaceTab(u.ID, b.ID))
	if got := tabIDs(t, s, u.ID); !sameIDs(got, []string{c.ID, a.ID, d.ID}) {
		t.Fatalf("after closes: %v", got)
	}
}

// At most one ephemeral tab per user, under concurrent opens of different
// workspaces (PLAN-3002 U1's proving test).
func TestWorkspaceTabs_AtMostOneEphemeralUnderConcurrency(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	u := tabsUser(t, s, "Racer")
	var wss []*models.Workspace
	for i := 0; i < 8; i++ {
		wss = append(wss, tabsWorkspace(t, s, u, fmt.Sprintf("Race %d", i)))
	}
	if _, err := s.db.Exec(s.q(`DELETE FROM user_workspace_tabs WHERE user_id = ?`), u.ID); err != nil {
		t.Fatalf("clear: %v", err)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, len(wss))
	for _, ws := range wss {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			<-start
			_, err := s.OpenWorkspaceTab(u.ID, id, true)
			errs <- err
		}(ws.ID)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent open: %v", err)
		}
	}
	list, err := s.ListWorkspaceTabs(u.ID)
	rows := list.Rows
	if err != nil {
		t.Fatal(err)
	}
	ephemeral := 0
	for _, r := range rows {
		if r.Ephemeral {
			ephemeral++
		}
	}
	if len(rows) != 1 || ephemeral != 1 {
		t.Fatalf("after %d concurrent ephemeral opens: %d rows, %d ephemeral; want 1 and 1", len(wss), len(rows), ephemeral)
	}
}

// One test per loss path: the tab row is gone after it, and only the user who
// lost access loses the tab.
func TestWorkspaceTabs_LossPathsDeleteTheRow(t *testing.T) {
	t.Parallel()

	type fixture struct {
		s      *Store
		owner  *models.User
		member *models.User
		ws     *models.Workspace
		coll   *models.Collection
	}
	setup := func(t *testing.T) fixture {
		s := testStore(t)
		owner := tabsUser(t, s, "Lossowner")
		member := tabsUser(t, s, "Lossmember")
		ws := tabsWorkspace(t, s, owner, "Loss")
		coll, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks", Slug: "tasks", Prefix: "TSK", Schema: `{"fields":[]}`})
		if err != nil {
			t.Fatalf("create collection: %v", err)
		}
		for _, uid := range []string{owner.ID, member.ID} {
			if _, err := s.OpenWorkspaceTab(uid, ws.ID, false); err != nil {
				t.Fatalf("open: %v", err)
			}
		}
		return fixture{s, owner, member, ws, coll}
	}
	has := func(t *testing.T, f fixture, userID string) bool {
		_, ok := tabRow(t, f.s, userID, f.ws.ID)
		return ok
	}
	grant := func(t *testing.T, f fixture) *models.CollectionGrant {
		g, err := f.s.CreateCollectionGrant(f.ws.ID, f.coll.ID, f.member.ID, "view", f.owner.ID)
		if err != nil {
			t.Fatalf("grant: %v", err)
		}
		return g
	}
	itemGrant := func(t *testing.T, f fixture) *models.ItemGrant {
		it, err := f.s.CreateItem(f.ws.ID, f.coll.ID, models.ItemCreate{Title: "shared", Fields: `{}`})
		if err != nil {
			t.Fatalf("create item: %v", err)
		}
		g, err := f.s.CreateItemGrant(f.ws.ID, it.ID, f.member.ID, "view", f.owner.ID)
		if err != nil {
			t.Fatalf("item grant: %v", err)
		}
		return g
	}

	t.Run("member removal", func(t *testing.T) {
		t.Parallel()
		f := setup(t)
		if err := f.s.AddWorkspaceMember(f.ws.ID, f.member.ID, "editor"); err != nil {
			t.Fatal(err)
		}
		if err := f.s.RemoveWorkspaceMember(f.ws.ID, f.member.ID); err != nil {
			t.Fatal(err)
		}
		if has(t, f, f.member.ID) {
			t.Fatal("tab survived member removal")
		}
		if !has(t, f, f.owner.ID) {
			t.Fatal("owner's tab went too")
		}
	})
	t.Run("member removal keeps the tab of a member who stays a guest", func(t *testing.T) {
		t.Parallel()
		f := setup(t)
		if err := f.s.AddWorkspaceMember(f.ws.ID, f.member.ID, "editor"); err != nil {
			t.Fatal(err)
		}
		grant(t, f)
		if err := f.s.RemoveWorkspaceMember(f.ws.ID, f.member.ID); err != nil {
			t.Fatal(err)
		}
		if !has(t, f, f.member.ID) {
			t.Fatal("tab deleted although a grant keeps the user in the workspace")
		}
	})
	t.Run("member removal with grant revoke", func(t *testing.T) {
		t.Parallel()
		f := setup(t)
		if err := f.s.AddWorkspaceMember(f.ws.ID, f.member.ID, "editor"); err != nil {
			t.Fatal(err)
		}
		grant(t, f)
		if err := f.s.RemoveWorkspaceMemberAndRevokeGrants(f.ws.ID, f.member.ID); err != nil {
			t.Fatal(err)
		}
		if has(t, f, f.member.ID) {
			t.Fatal("tab survived member removal with grant revoke")
		}
	})
	t.Run("guest's last collection grant revoked", func(t *testing.T) {
		t.Parallel()
		f := setup(t)
		g := grant(t, f)
		if err := f.s.DeleteCollectionGrant(g.ID, f.ws.ID); err != nil {
			t.Fatal(err)
		}
		if has(t, f, f.member.ID) {
			t.Fatal("tab survived the guest's last grant")
		}
	})
	t.Run("guest's collection grant revoked while an item grant remains", func(t *testing.T) {
		t.Parallel()
		f := setup(t)
		g := grant(t, f)
		itemGrant(t, f)
		if err := f.s.DeleteCollectionGrant(g.ID, f.ws.ID); err != nil {
			t.Fatal(err)
		}
		if !has(t, f, f.member.ID) {
			t.Fatal("tab deleted although an item grant remains")
		}
	})
	t.Run("guest's last item grant revoked", func(t *testing.T) {
		t.Parallel()
		f := setup(t)
		g := itemGrant(t, f)
		if err := f.s.DeleteItemGrant(g.ID, f.ws.ID); err != nil {
			t.Fatal(err)
		}
		if has(t, f, f.member.ID) {
			t.Fatal("tab survived the guest's last item grant")
		}
	})
	t.Run("revoke all grants", func(t *testing.T) {
		t.Parallel()
		f := setup(t)
		grant(t, f)
		itemGrant(t, f)
		if err := f.s.RevokeAllUserGrants(f.ws.ID, f.member.ID); err != nil {
			t.Fatal(err)
		}
		if has(t, f, f.member.ID) {
			t.Fatal("tab survived revoke-all")
		}
	})
	t.Run("soft delete, and restore does not bring it back", func(t *testing.T) {
		t.Parallel()
		f := setup(t)
		if err := f.s.DeleteWorkspace(f.ws.Slug); err != nil {
			t.Fatal(err)
		}
		if has(t, f, f.member.ID) || has(t, f, f.owner.ID) {
			t.Fatal("a tab survived soft delete")
		}
		if err := f.s.RestoreWorkspace(f.ws.Slug); err != nil {
			t.Fatal(err)
		}
		if has(t, f, f.member.ID) || has(t, f, f.owner.ID) {
			t.Fatal("restore brought a tab back")
		}
	})
	t.Run("purge", func(t *testing.T) {
		t.Parallel()
		f := setup(t)
		// Purge refuses a live workspace; soft-delete by hand so the tab
		// rows are still there when the purge runs.
		if _, err := f.s.db.Exec(f.s.q(`UPDATE workspaces SET deleted_at = ? WHERE id = ?`), now(), f.ws.ID); err != nil {
			t.Fatal(err)
		}
		if !has(t, f, f.member.ID) {
			t.Fatal("precondition: the row must exist before the purge")
		}
		if err := f.s.PurgeWorkspaceData(f.ws.ID); err != nil {
			t.Fatal(err)
		}
		if has(t, f, f.member.ID) {
			t.Fatal("tab survived purge")
		}
	})
	t.Run("owner deletes their account", func(t *testing.T) {
		t.Parallel()
		f := setup(t)
		if err := f.s.DeleteAccountAtomic(f.owner.ID); err != nil {
			t.Fatal(err)
		}
		if has(t, f, f.member.ID) {
			t.Fatal("another user's tab survived the owner's account deletion")
		}
	})
}

// The read-side invariant: a stored row whose workspace is not in the visible
// set is never served, whatever left it there.
func TestVisibleWorkspaceTabs_FiltersToTheVisibleSet(t *testing.T) {
	t.Parallel()
	rows := []WorkspaceTabRow{
		{WorkspaceID: "w1", Position: 0},
		{WorkspaceID: "gone", Position: 1},
		{WorkspaceID: "w2", Position: 2, Ephemeral: true, LastRoute: "/o/two/tasks"},
	}
	visible := []models.Workspace{
		{ID: "w2", Slug: "two", Name: "Two", OwnerUsername: "o", IsGuest: true},
		{ID: "w1", Slug: "one", Name: "One", OwnerUsername: "o"},
	}
	got := VisibleWorkspaceTabs(rows, visible)
	if len(got) != 2 || got[0].Slug != "one" || got[1].Slug != "two" {
		t.Fatalf("got %+v", got)
	}
	if !got[1].IsGuest || !got[1].Ephemeral || got[1].LastRoute != "/o/two/tasks" {
		t.Fatalf("fields not carried: %+v", got[1])
	}
	if out := VisibleWorkspaceTabs(rows, nil); len(out) != 0 {
		t.Fatalf("empty visible set served %v", out)
	}
}
