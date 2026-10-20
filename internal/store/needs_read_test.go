package store

import (
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Store reads behind the needs-you router (TASK-3920). Runs on both backends
// via testStore (PAD_TEST_POSTGRES_URL selects Postgres).

func TestGuestWorkspaceIDs(t *testing.T) {
	s := testStore(t)
	owner := createTestUser(t, s, "owner@example.com", "Owner", "password123")
	guest := createTestUser(t, s, "guest@example.com", "Guest", "password123")
	mk := func(name string) (*models.Workspace, *models.Collection, *models.Item) {
		ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: name, OwnerID: owner.ID})
		if err != nil {
			t.Fatal(err)
		}
		col := createTestCollection(t, s, ws.ID, "Tasks")
		return ws, col, createTestItem(t, s, ws.ID, col.ID, "Item "+name, "")
	}
	wsColl, col1, _ := mk("CollGrant")
	wsItem, _, item2 := mk("ItemGrant")
	wsMember, col3, _ := mk("Member")
	wsDeleted, col4, _ := mk("Deleted")
	mk("NoGrant")

	if _, err := s.CreateCollectionGrant(wsColl.ID, col1.ID, guest.ID, "view", owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateItemGrant(wsItem.ID, item2.ID, guest.ID, "view", owner.ID); err != nil {
		t.Fatal(err)
	}
	// A member who also holds a grant is not a guest there.
	if err := s.AddWorkspaceMember(wsMember.ID, guest.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateCollectionGrant(wsMember.ID, col3.ID, guest.ID, "view", owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateCollectionGrant(wsDeleted.ID, col4.ID, guest.ID, "view", owner.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteWorkspace(wsDeleted.Slug); err != nil {
		t.Fatal(err)
	}

	got, err := s.GuestWorkspaceIDs(guest.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{wsColl.ID, wsItem.ID}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("GuestWorkspaceIDs = %v, want %v (grant-only, member and deleted workspaces excluded)", got, want)
	}
	if none, err := s.GuestWorkspaceIDs(owner.ID); err != nil || len(none) != 0 {
		t.Fatalf("an owner with no grants is a guest nowhere: %v %v", none, err)
	}
}

func TestGetWorkspacesByIDs(t *testing.T) {
	s := testStore(t)
	owner := mentionUser(t, s, "wsowner")
	a, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "A", OwnerID: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "B", OwnerID: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteWorkspace(b.Slug); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetWorkspacesByIDs([]string{a.ID, b.ID, "nope"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != a.ID || got[0].Slug != a.Slug || got[0].OwnerUsername == "" {
		t.Fatalf("want only the live workspace with its owner's username, got %+v", got)
	}
}

type mentionFixture struct {
	s          *Store
	ws         *models.Workspace
	col        *models.Collection
	item       *models.Item
	author     *models.User
	alice, bob *models.User
}

func newMentionFixture(t *testing.T) mentionFixture {
	t.Helper()
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Mentions")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	return mentionFixture{
		s: s, ws: ws, col: col,
		item:   createTestItem(t, s, ws.ID, col.ID, "Ask", ""),
		author: mentionUser(t, s, "author"),
		alice:  mentionUser(t, s, "alice"),
		bob:    mentionUser(t, s, "bobby"),
	}
}

func (f mentionFixture) comment(t *testing.T, by *models.User, body string) *models.Comment {
	t.Helper()
	c, err := f.s.CreateComment(f.ws.ID, f.item.ID, by.ID, models.CommentCreate{Body: body, Author: by.Username, CreatedBy: "user", Source: "web"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (f mentionFixture) setAt(t *testing.T, table, id, at string) {
	t.Helper()
	col := "id"
	if table == "comment_mentions" {
		col = "comment_id"
	}
	if _, err := f.s.db.Exec(f.s.q(`UPDATE `+table+` SET created_at = ? WHERE `+col+` = ?`), at, id); err != nil {
		t.Fatal(err)
	}
}

func (f mentionFixture) open(t *testing.T) []string {
	t.Helper()
	m, err := f.s.OpenMentionsForItems([]string{f.item.ID})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, x := range m[f.item.ID] {
		out = append(out, x.UserID)
	}
	return out
}

func TestOpenMentions_ReplyClosesAndEditReopens(t *testing.T) {
	f := newMentionFixture(t)
	c := f.comment(t, f.author, "@alice @bobby please look")
	f.setAt(t, "comments", c.ID, "2001-01-01T00:00:00Z")
	f.setAt(t, "comment_mentions", c.ID, "2001-01-01T00:00:00Z")
	want := []string{f.alice.ID, f.bob.ID}
	sort.Strings(want)
	if got := f.open(t); !reflect.DeepEqual(got, want) {
		t.Fatalf("open mentions %v, want both %v", got, want)
	}
	m, _ := f.s.OpenMentionsForItems([]string{f.item.ID})
	if m[f.item.ID][0].CommentID != c.ID || m[f.item.ID][0].By != "author" || !m[f.item.ID][0].At.Equal(time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("mention carries comment, author and time: %+v", m[f.item.ID][0])
	}

	// A human reply by alice closes hers, not bob's.
	r := f.comment(t, f.alice, "on it")
	f.setAt(t, "comments", r.ID, "2001-06-01T00:00:00Z")
	if got := f.open(t); !reflect.DeepEqual(got, []string{f.bob.ID}) {
		t.Fatalf("after alice replied, open = %v, want only bob", got)
	}
	// Deleting the mentioning comment closes the rest.
	// Editing the mention away and back is a NEW mention (edit time) that
	// postdates alice's reply: it is open again.
	if _, err := f.s.UpdateComment(c.ID, "never mind"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.UpdateComment(c.ID, "@alice again"); err != nil {
		t.Fatal(err)
	}
	if got := f.open(t); !reflect.DeepEqual(got, []string{f.alice.ID}) {
		t.Fatalf("after the edit re-mentioned alice, open = %v, want only alice", got)
	}
	// A self-mention is never open, and a deleted comment holds no mention.
	self := f.comment(t, f.bob, "note to @bobby")
	if got := f.open(t); !reflect.DeepEqual(got, []string{f.alice.ID}) {
		t.Fatalf("a self-mention is open: %v (comment %s)", got, self.ID)
	}
	if err := f.s.DeleteComment(c.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.open(t); len(got) != 0 {
		t.Fatalf("a deleted comment still holds open mentions: %v", got)
	}
}

func TestNeedsCandidateItems(t *testing.T) {
	f := newMentionFixture(t)
	// A second collection: awaits_human; a system one; a plain one.
	ask, err := f.s.CreateCollection(f.ws.ID, models.CollectionCreate{Name: "Asks", Schema: `{"fields":[]}`})
	if err != nil {
		t.Fatal(err)
	}
	askItem := createTestItem(t, f.s, f.ws.ID, ask.ID, "An ask", "")
	plain := createTestItem(t, f.s, f.ws.ID, f.col.ID, "Plain", "")
	flagged := createTestItem(t, f.s, f.ws.ID, f.col.ID, "Flagged", "")
	f.comment(t, f.author, "@alice look") // f.item: open mention

	ids := func(items []models.Item) []string {
		var out []string
		for _, it := range items {
			out = append(out, it.ID)
		}
		sort.Strings(out)
		return out
	}
	sorted := func(v ...string) []string { sort.Strings(v); return v }
	all := CatchUpScope{}
	p := NeedsCandidateParams{AwaitsCollectionIDs: []string{ask.ID}, FlaggedItemIDs: []string{flagged.ID}}

	got, err := f.s.NeedsCandidateItems(f.ws.ID, all, p)
	if err != nil {
		t.Fatal(err)
	}
	if want := sorted(f.item.ID, askItem.ID, flagged.ID); !reflect.DeepEqual(ids(got), want) {
		t.Fatalf("candidates %v, want %v (not the plain item %s)", ids(got), want, plain.ID)
	}
	for _, it := range got {
		if it.ID == askItem.ID && (it.Ref == "" || it.CollectionSlug == "" || it.Title != "An ask") {
			t.Fatalf("candidate is missing its display columns: %+v", it)
		}
	}

	// Visibility is in the SQL: an item grant only, a collection only, nothing.
	if got, _ := f.s.NeedsCandidateItems(f.ws.ID, CatchUpScope{CollectionIDs: []string{}, ItemIDs: []string{flagged.ID}}, p); !reflect.DeepEqual(ids(got), sorted(flagged.ID)) {
		t.Fatalf("item-grant scope: %v", ids(got))
	}
	if got, _ := f.s.NeedsCandidateItems(f.ws.ID, CatchUpScope{CollectionIDs: []string{ask.ID}}, p); !reflect.DeepEqual(ids(got), sorted(askItem.ID)) {
		t.Fatalf("collection scope: %v", ids(got))
	}
	if got, _ := f.s.NeedsCandidateItems(f.ws.ID, CatchUpScope{CollectionIDs: []string{}}, p); len(got) != 0 {
		t.Fatalf("an empty scope sees nothing: %v", ids(got))
	}
	// ForUserID keeps the assignee's and the mentioned user's items.
	p.ForUserID = f.alice.ID
	if got, _ := f.s.NeedsCandidateItems(f.ws.ID, all, p); !reflect.DeepEqual(ids(got), sorted(f.item.ID)) {
		t.Fatalf("for alice (mentioned): %v", ids(got))
	}
	p.ForUserID = f.bob.ID
	if got, _ := f.s.NeedsCandidateItems(f.ws.ID, all, p); len(got) != 0 {
		t.Fatalf("for bob (nothing routes to him): %v", ids(got))
	}
	// A deleted item and a deleted collection are not candidates.
	if err := f.s.DeleteItem(askItem.ID); err != nil {
		t.Fatal(err)
	}
	p.ForUserID = ""
	if got, _ := f.s.NeedsCandidateItems(f.ws.ID, all, p); len(got) != 2 {
		t.Fatalf("a deleted item is a candidate: %v", ids(got))
	}
}

func TestNeedsSnoozesForItems_JoinsItemsWorkspace(t *testing.T) {
	f := newMentionFixture(t)
	other := createTestWorkspace(t, f.s, "Other")
	if _, err := f.s.SnoozeNeedsItem(f.alice.ID, f.ws.ID, f.item.ID, models.NeedsSnoozeDefer, time.Now().Add(48*time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	ids := []string{f.item.ID}
	got, err := f.s.NeedsSnoozesForItems(f.alice.ID, f.ws.ID, ids, time.Now())
	if err != nil || got[f.item.ID] == nil || !got[f.item.ID].Holds {
		t.Fatalf("a live deferral holds in the item's workspace: %+v %v", got, err)
	}
	// The row's stale workspace_id (an item that moved out leaves it behind)
	// is not trusted: the item's own workspace decides.
	if _, err := f.s.db.Exec(f.s.q(`UPDATE item_needs_snoozes SET workspace_id = ?`), other.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.s.NeedsSnoozesForItems(f.alice.ID, f.ws.ID, ids, time.Now()); got[f.item.ID] == nil {
		t.Fatal("a stale snooze.workspace_id hid the snooze from the item's own workspace")
	}
	if got, _ := f.s.NeedsSnoozesForItems(f.alice.ID, other.ID, ids, time.Now()); len(got) != 0 {
		t.Fatalf("the snooze surfaced in a workspace the item is not in: %+v", got)
	}
	// Per user, and not past a deleted item.
	if got, _ := f.s.NeedsSnoozesForItems(f.bob.ID, f.ws.ID, ids, time.Now()); len(got) != 0 {
		t.Fatalf("another user's snooze surfaced: %+v", got)
	}
	if err := f.s.DeleteItem(f.item.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.s.NeedsSnoozesForItems(f.alice.ID, f.ws.ID, ids, time.Now()); len(got) != 0 {
		t.Fatalf("a deleted item's snooze surfaced: %+v", got)
	}
}

// The batch dismissal state must agree with GetNeedsSnooze item by item.
func TestNeedsSnoozesForItems_DismissMatchesSingle(t *testing.T) {
	f := newMentionFixture(t)
	other := createTestItem(t, f.s, f.ws.ID, f.col.ID, "Second", "")
	f.comment(t, f.author, "first")
	for _, it := range []*models.Item{f.item, other} {
		if _, err := f.s.SnoozeNeedsItem(f.alice.ID, f.ws.ID, it.ID, models.NeedsSnoozeDismiss, time.Time{}, nil); err != nil {
			t.Fatal(err)
		}
	}
	check := func(label string) {
		t.Helper()
		got, err := f.s.NeedsSnoozesForItems(f.alice.ID, f.ws.ID, []string{f.item.ID, other.ID}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{f.item.ID, other.ID} {
			one, _ := f.s.GetNeedsSnooze(f.alice.ID, id, time.Now())
			if got[id] == nil || got[id].Holds != one.Holds {
				t.Fatalf("%s: batch holds=%v, single holds=%v for %s", label, got[id], one.Holds, id)
			}
		}
	}
	check("fresh dismissal holds")
	// A later comment by someone else on one item ends its hold only.
	c := f.comment(t, f.author, "second")
	f.setAt(t, "comments", c.ID, time.Now().UTC().Add(time.Hour).Format(time.RFC3339))
	check("a newer comment resurfaces")
	if got, _ := f.s.NeedsSnoozesForItems(f.alice.ID, f.ws.ID, []string{f.item.ID, other.ID}, time.Now()); got[f.item.ID].Holds || !got[other.ID].Holds {
		t.Fatalf("the comment should end only the first item's hold: %+v %+v", got[f.item.ID], got[other.ID])
	}
}

func TestLatestAssignmentTimes(t *testing.T) {
	f := newMentionFixture(t)
	// Two assignments to alice (Name = her email in this fixture) and one to bob.
	add := func(at, changes string) {
		if _, err := f.s.db.Exec(f.s.q(`INSERT INTO activities (id, workspace_id, document_id, action, actor, source, metadata, created_at)
			VALUES (?, ?, ?, 'updated', 'user', 'web', ?, ?)`), newID(), f.ws.ID, f.item.ID, `{"changes":"`+changes+`"}`, at); err != nil {
			t.Fatal(err)
		}
	}
	add("2024-01-01T00:00:00Z", "assigned:  → Alice")
	add("2024-02-01T00:00:00Z", "status: open → done; assigned: Alice → Bob")
	add("2024-03-01T00:00:00Z", "assigned: Bob → Alice")
	add("2024-04-01T00:00:00Z", "title: a → b")
	got, err := f.s.LatestAssignmentTimes(f.ws.ID, map[string]string{f.item.ID: "Alice"})
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC); !got[f.item.ID].Equal(want) {
		t.Fatalf("latest assignment to Alice = %v, want %v", got[f.item.ID], want)
	}
	got, _ = f.s.LatestAssignmentTimes(f.ws.ID, map[string]string{f.item.ID: "Carol"})
	if _, ok := got[f.item.ID]; ok {
		t.Fatal("an assignee with no assignment row must be absent (caller falls back to created_at)")
	}
}

func TestWorkspaceOwnerIDs(t *testing.T) {
	s := testStore(t)
	owner := createTestUser(t, s, "owner@example.com", "Owner", "password123")
	ed := createTestUser(t, s, "ed@example.com", "Ed", "password123")
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "W", OwnerID: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddWorkspaceMember(ws.ID, ed.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	got, err := s.WorkspaceOwnerIDs(ws.ID)
	if err != nil || !reflect.DeepEqual(got, []string{owner.ID}) {
		t.Fatalf("owners = %v %v, want only %s", got, err, owner.ID)
	}
}

func TestNeedsMarkersForItems(t *testing.T) {
	f := newMentionFixture(t)
	c := f.comment(t, f.author, "hello")
	got, err := f.s.NeedsMarkersForItems(f.alice.ID, []string{f.item.ID})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := f.s.needsItemState(f.s.db, f.alice.ID, f.item.ID)
	m := got[f.item.ID]
	if m.SeenCommentAt != want.SeenCommentAt || !reflect.DeepEqual(m.SeenCommentIDs, []string{c.ID}) || m.SeenTransitionSeq != want.SeenTransitionSeq {
		t.Fatalf("marker %+v, want %+v", m, want)
	}
	// Alice's own comment is not "other": an item with none of the others' has an empty, non-nil list.
	other := createTestItem(t, f.s, f.ws.ID, f.col.ID, "Quiet", "")
	got, _ = f.s.NeedsMarkersForItems(f.alice.ID, []string{other.ID})
	if got[other.ID].SeenCommentIDs == nil || len(got[other.ID].SeenCommentIDs) != 0 {
		t.Fatalf("quiet item marker %+v", got[other.ID])
	}
}

// A restricted scope with BOTH a collection list and an item grant returns the
// visible items and never the hidden one, even though the hidden item matches
// every candidate source (it is flagged and it carries an open mention).
func TestNeedsCandidateItems_RestrictedScopeHidesItem(t *testing.T) {
	f := newMentionFixture(t)
	ask, err := f.s.CreateCollection(f.ws.ID, models.CollectionCreate{Name: "Asks", Schema: `{"fields":[]}`})
	if err != nil {
		t.Fatal(err)
	}
	visibleByColl := createTestItem(t, f.s, f.ws.ID, ask.ID, "visible by collection", "")
	granted := createTestItem(t, f.s, f.ws.ID, f.col.ID, "visible by grant", "")
	// f.item sits in f.col, is not granted, and has an open mention.
	hidden := f.item
	p := NeedsCandidateParams{AwaitsCollectionIDs: []string{ask.ID}, FlaggedItemIDs: []string{granted.ID, hidden.ID}}
	f.comment(t, f.author, "@alice look")

	got, err := f.s.NeedsCandidateItems(f.ws.ID, CatchUpScope{CollectionIDs: []string{ask.ID}, ItemIDs: []string{granted.ID}}, p)
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, it := range got {
		have[it.ID] = true
	}
	if !have[visibleByColl.ID] || !have[granted.ID] || have[hidden.ID] || len(got) != 2 {
		t.Fatalf("restricted scope returned %v; want %s and %s, not the hidden %s", have, visibleByColl.ID, granted.ID, hidden.ID)
	}
}
