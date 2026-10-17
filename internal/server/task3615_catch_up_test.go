package server

import (
	"fmt"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// TASK-3615: the "catch me up" digest. Every fixture acts through the HTTP
// doors as real users, so the activity rows carry the user ids the actor
// join reads, then pins every timestamp in the workspace to one second
// (task3615At). The join is "same item, same second" by ruling, so a run
// that straddled a second boundary between a transition and its activity row
// would otherwise flake; pinning keeps the test about the rule, not the
// clock.

type task3615Fixture struct {
	srv          *Server
	ws           *models.Workspace
	alice, bob   *models.User
	aliceTok     string
	bobTok       string
	tasksID      string
	ideasID      string
	collectionID map[string]string
}

func newTask3615Fixture(t *testing.T, srv *Server) *task3615Fixture {
	t.Helper()
	slug := createWSWithCollections(t, srv)
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil || ws == nil {
		t.Fatalf("GetWorkspaceBySlug: %v", err)
	}
	f := &task3615Fixture{srv: srv, ws: ws, collectionID: map[string]string{}}
	f.alice = f.user(t, "alice", "owner")
	f.bob = f.user(t, "bob", "editor")
	f.aliceTok = f.session(t, f.alice)
	f.bobTok = f.session(t, f.bob)
	for _, c := range []string{"tasks", "ideas"} {
		coll, err := srv.store.GetCollectionBySlug(ws.ID, c)
		if err != nil || coll == nil {
			t.Fatalf("GetCollectionBySlug %s: %v", c, err)
		}
		f.collectionID[c] = coll.ID
	}
	f.tasksID, f.ideasID = f.collectionID["tasks"], f.collectionID["ideas"]
	return f
}

func (f *task3615Fixture) user(t *testing.T, name, role string) *models.User {
	t.Helper()
	u, err := f.srv.store.CreateUser(models.UserCreate{Email: name + "-3615@example.com", Name: name, Username: name + "-3615", Password: "pw-test-12345"})
	if err != nil {
		t.Fatalf("CreateUser %s: %v", name, err)
	}
	if err := f.srv.store.AddWorkspaceMember(f.ws.ID, u.ID, role); err != nil {
		t.Fatalf("AddWorkspaceMember %s: %v", name, err)
	}
	return u
}

func (f *task3615Fixture) session(t *testing.T, u *models.User) string {
	t.Helper()
	tok, err := f.srv.store.CreateSession(u.ID, "go-test", "192.0.2.1", "go-test", 24*time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return tok
}

func (f *task3615Fixture) create(t *testing.T, tok, coll, title string) models.Item {
	t.Helper()
	rr := doRequestWithCookie(f.srv, "POST", "/api/v1/workspaces/"+f.ws.Slug+"/collections/"+coll+"/items", map[string]any{"title": title}, tok)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create %q: %d %s", title, rr.Code, rr.Body.String())
	}
	var it models.Item
	parseJSON(t, rr, &it)
	return it
}

func (f *task3615Fixture) patch(t *testing.T, tok string, it models.Item, body map[string]any) {
	t.Helper()
	rr := doRequestWithCookie(f.srv, "PATCH", "/api/v1/workspaces/"+f.ws.Slug+"/items/"+it.Slug, body, tok)
	if rr.Code != http.StatusOK {
		t.Fatalf("patch %s %v: %d %s", it.Title, body, rr.Code, rr.Body.String())
	}
}

func (f *task3615Fixture) status(t *testing.T, tok string, it models.Item, status string) {
	t.Helper()
	f.patch(t, tok, it, map[string]any{"fields_patch": map[string]any{"status": status}})
}

func (f *task3615Fixture) comment(t *testing.T, tok string, it models.Item, body string) {
	t.Helper()
	rr := doRequestWithCookie(f.srv, "POST", "/api/v1/workspaces/"+f.ws.Slug+"/items/"+it.Slug+"/comments", map[string]any{"body": body}, tok)
	if rr.Code != http.StatusCreated {
		t.Fatalf("comment on %s: %d %s", it.Title, rr.Code, rr.Body.String())
	}
}

// pin sets every timestamp the digest reads in this workspace to at.
func (f *task3615Fixture) pin(t *testing.T, at time.Time) {
	t.Helper()
	ts := at.UTC().Format(time.RFC3339)
	for _, tbl := range []string{"items", "status_transitions", "activities", "comments"} {
		if _, err := f.srv.store.DB().Exec(fmt.Sprintf("UPDATE %s SET created_at = '%s' WHERE workspace_id = '%s'", tbl, ts, f.ws.ID)); err != nil {
			t.Fatalf("pin %s: %v", tbl, err)
		}
	}
}

func (f *task3615Fixture) catchUp(t *testing.T, tok string) catchUpResponse {
	t.Helper()
	rr := doRequestWithCookie(f.srv, "GET", "/api/v1/workspaces/"+f.ws.Slug+"/catch-up", nil, tok)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET catch-up: %d %s", rr.Code, rr.Body.String())
	}
	var resp catchUpResponse
	parseJSON(t, rr, &resp)
	return resp
}

func task3615Titles(sec store.CatchUpSection) []string {
	out := []string{}
	for _, l := range sec.Lines {
		out = append(out, l.Title)
	}
	sort.Strings(out)
	return out
}

func task3615Want(t *testing.T, name string, sec store.CatchUpSection, want ...string) {
	t.Helper()
	sort.Strings(want)
	got := task3615Titles(sec)
	if fmt.Sprint(got) != fmt.Sprint(want) || sec.Total != len(want) {
		t.Errorf("%s: lines %v total %d, want %v total %d", name, got, sec.Total, want, len(want))
	}
}

var task3615At = time.Now().UTC().Add(-time.Hour).Truncate(time.Second)

func TestTASK3615_CatchUpSections(t *testing.T) {
	t.Parallel()
	bothBackends(t, func(t *testing.T, srv *Server) {
		f := newTask3615Fixture(t, srv)

		mine := f.create(t, f.aliceTok, "tasks", "Alice's task")
		f.comment(t, f.bobTok, mine, "bob on alice's task")
		f.comment(t, f.aliceTok, mine, "alice on her own task")

		bobDone := f.create(t, f.bobTok, "tasks", "Bob finished")
		f.status(t, f.bobTok, bobDone, "done")

		aliceDone := f.create(t, f.aliceTok, "tasks", "Alice finished")
		f.status(t, f.aliceTok, aliceDone, "done")

		// Alice finished it, but Bob wrote to it in the same second: the
		// actor is ambiguous, so the line stays.
		shared := f.create(t, f.aliceTok, "tasks", "Shared second")
		f.status(t, f.aliceTok, shared, "done")
		f.patch(t, f.bobTok, shared, map[string]any{"title": "Shared second (bob)"})

		// Bob's alone: pinning puts every row of an item in one second, so
		// a reopen by someone other than the finisher would make the finish
		// ambiguous too.
		reopened := f.create(t, f.bobTok, "tasks", "Bob reopened")
		f.status(t, f.bobTok, reopened, "done")
		f.status(t, f.bobTok, reopened, "open")

		f.create(t, f.bobTok, "ideas", "Bob's idea")

		// Alice's own item, reopened by Bob: news as a reopen, not as a
		// creation (only Bob's rows touch its status).
		bobReopensMine := f.create(t, f.aliceTok, "tasks", "Bob reopened mine")
		f.status(t, f.bobTok, bobReopensMine, "done")
		f.status(t, f.bobTok, bobReopensMine, "open")

		// Cancelled is terminal but abandoned: not a finish.
		cancelled := f.create(t, f.aliceTok, "tasks", "Bob cancelled")
		f.status(t, f.bobTok, cancelled, "cancelled")

		// Comments that are not Alice's news: her own, alone, on her item;
		// and Bob's on his own item, which is neither hers nor assigned to her.
		ownComment := f.create(t, f.aliceTok, "tasks", "Only Alice commented")
		f.comment(t, f.aliceTok, ownComment, "note to self")
		bobsOwn := f.create(t, f.bobTok, "tasks", "Bob's own thread")
		f.comment(t, f.bobTok, bobsOwn, "bob talking to himself")

		f.pin(t, task3615At)
		if _, err := srv.store.MarkCaughtUp(f.alice.ID, f.ws.ID, task3615At.Add(-time.Hour)); err != nil {
			t.Fatalf("MarkCaughtUp: %v", err)
		}

		got := f.catchUp(t, f.aliceTok)
		task3615Want(t, "finished", got.Finished, "Bob finished", "Bob reopened", "Bob reopened mine", "Shared second (bob)")
		task3615Want(t, "new_and_reopened", got.NewAndReopened, "Bob finished", "Bob's idea", "Bob reopened", "Bob reopened mine", "Bob's own thread")
		task3615Want(t, "comments", got.Comments, "Alice's task")
		for _, l := range got.NewAndReopened.Lines {
			if l.Title == "Bob reopened mine" && l.What != "reopened" {
				t.Errorf("%q: what %q, want reopened", l.Title, l.What)
			}
		}
		if !got.Window.HasCursor || got.Window.CappedAtCeiling {
			t.Errorf("window %+v: want a cursor inside the ceiling", got.Window)
		}
		for _, l := range got.Finished.Lines {
			if l.Actor != "bob" {
				t.Errorf("finished %q: actor %q, want bob", l.Title, l.Actor)
			}
		}

		// Bob's view is the mirror: Alice's finishes are his news, his own
		// are not.
		bobView := f.catchUp(t, f.bobTok)
		task3615Want(t, "bob finished", bobView.Finished, "Alice finished", "Shared second (bob)")
	})
}

func TestTASK3615_CursorAndCeilingBoundTheWindow(t *testing.T) {
	t.Parallel()
	bothBackends(t, func(t *testing.T, srv *Server) {
		f := newTask3615Fixture(t, srv)
		it := f.create(t, f.bobTok, "tasks", "Bob finished")
		f.status(t, f.bobTok, it, "done")
		f.pin(t, task3615At)

		// No cursor: the window is the 14-day ceiling, and reading it does
		// not create one.
		got := f.catchUp(t, f.aliceTok)
		task3615Want(t, "no cursor", got.Finished, "Bob finished")
		if got.Window.HasCursor || !got.Window.CappedAtCeiling {
			t.Errorf("window %+v: want no cursor, capped at the ceiling", got.Window)
		}
		if _, found, err := srv.store.ReadCursor(f.alice.ID, f.ws.ID); err != nil || found {
			t.Fatalf("GET wrote a cursor (found=%v, err=%v)", found, err)
		}

		// A press in the same second as the event does NOT hide it: the
		// event is stamped to the second and may have come after the press.
		if _, err := srv.store.MarkCaughtUp(f.alice.ID, f.ws.ID, task3615At.Add(500*time.Millisecond)); err != nil {
			t.Fatalf("MarkCaughtUp: %v", err)
		}
		task3615Want(t, "same-second cursor", f.catchUp(t, f.aliceTok).Finished, "Bob finished")
		// A press in the next second does.
		if _, err := srv.store.MarkCaughtUp(f.alice.ID, f.ws.ID, task3615At.Add(time.Second)); err != nil {
			t.Fatalf("MarkCaughtUp: %v", err)
		}
		task3615Want(t, "next-second cursor", f.catchUp(t, f.aliceTok).Finished)

		// The ceiling: an event 15 days old is out even with no cursor.
		bobView := f.catchUp(t, f.bobTok)
		task3615Want(t, "bob, own work", bobView.Finished)
		f.pin(t, time.Now().UTC().Add(-15*24*time.Hour))
		other := f.user(t, "carol", "editor")
		task3615Want(t, "past the ceiling", f.catchUp(t, f.session(t, other)).Finished)

		// A cursor older than the ceiling does not reach past it.
		dave := f.user(t, "dave", "editor")
		if _, err := srv.store.MarkCaughtUp(dave.ID, f.ws.ID, time.Now().UTC().Add(-20*24*time.Hour)); err != nil {
			t.Fatalf("MarkCaughtUp: %v", err)
		}
		old := f.catchUp(t, f.session(t, dave))
		task3615Want(t, "old cursor, past the ceiling", old.Finished)
		if !old.Window.HasCursor || !old.Window.CappedAtCeiling {
			t.Errorf("old cursor window %+v: want a cursor, capped at the ceiling", old.Window)
		}
	})
}

func TestTASK3615_MarkCaughtUpNeverMovesBackwards(t *testing.T) {
	t.Parallel()
	bothBackends(t, func(t *testing.T, srv *Server) {
		f := newTask3615Fixture(t, srv)
		rr := doRequestWithCookie(srv, "PUT", "/api/v1/workspaces/"+f.ws.Slug+"/me/seen", nil, f.aliceTok)
		if rr.Code != http.StatusOK {
			t.Fatalf("PUT me/seen: %d %s", rr.Code, rr.Body.String())
		}
		first, found, err := srv.store.ReadCursor(f.alice.ID, f.ws.ID)
		if err != nil || !found {
			t.Fatalf("cursor after PUT: found=%v err=%v", found, err)
		}
		if d := time.Since(first); d < 0 || d > time.Minute {
			t.Fatalf("cursor %v is not now", first)
		}
		back, err := srv.store.MarkCaughtUp(f.alice.ID, f.ws.ID, first.Add(-time.Hour))
		if err != nil {
			t.Fatalf("MarkCaughtUp: %v", err)
		}
		if !back.Equal(first) {
			t.Fatalf("cursor moved backwards: %v, was %v", back, first)
		}
		// Per user: Bob has no cursor.
		if _, found, _ := srv.store.ReadCursor(f.bob.ID, f.ws.ID); found {
			t.Fatalf("Alice's press created Bob's cursor")
		}
	})
}

func TestTASK3615_VisibilityBoundsLinesAndTotals(t *testing.T) {
	t.Parallel()
	bothBackends(t, func(t *testing.T, srv *Server) {
		f := newTask3615Fixture(t, srv)
		task := f.create(t, f.bobTok, "tasks", "Visible task")
		granted := f.create(t, f.bobTok, "ideas", "Granted idea")
		hidden := f.create(t, f.bobTok, "ideas", "Hidden idea")
		f.status(t, f.bobTok, task, "done")
		f.status(t, f.bobTok, granted, "implemented")
		f.status(t, f.bobTok, hidden, "implemented")
		f.pin(t, task3615At)

		restricted := f.user(t, "restricted", "editor")
		if err := srv.store.SetMemberCollectionAccess(f.ws.ID, restricted.ID, "specific", []string{f.tasksID}); err != nil {
			t.Fatalf("SetMemberCollectionAccess: %v", err)
		}
		tok := f.session(t, restricted)

		// Control: the owner sees all three, so the fixture is what it says.
		task3615Want(t, "owner", f.catchUp(t, f.aliceTok).Finished, "Visible task", "Granted idea", "Hidden idea")

		got := f.catchUp(t, tok)
		task3615Want(t, "restricted finished", got.Finished, "Visible task")
		task3615Want(t, "restricted new", got.NewAndReopened, "Visible task")

		if _, err := srv.store.CreateItemGrant(f.ws.ID, granted.ID, restricted.ID, "view", f.alice.ID); err != nil {
			t.Fatalf("CreateItemGrant: %v", err)
		}
		got = f.catchUp(t, tok)
		task3615Want(t, "granted finished", got.Finished, "Visible task", "Granted idea")
		task3615Want(t, "granted new", got.NewAndReopened, "Visible task", "Granted idea")
	})
}

func TestTASK3615_NeedsYouListsBlockedItemsAssignedToYou(t *testing.T) {
	t.Parallel()
	bothBackends(t, func(t *testing.T, srv *Server) {
		f := newTask3615Fixture(t, srv)
		blocked := f.create(t, f.bobTok, "tasks", "Assigned and blocked")
		f.patch(t, f.bobTok, blocked, map[string]any{"assigned_user_id": f.alice.ID})
		notMine := f.create(t, f.bobTok, "tasks", "Blocked, not mine")
		freed := f.create(t, f.bobTok, "tasks", "Assigned, blocker done")
		f.patch(t, f.bobTok, freed, map[string]any{"assigned_user_id": f.alice.ID})
		doneButBlocked := f.create(t, f.bobTok, "tasks", "Assigned, done, link left")
		f.patch(t, f.bobTok, doneButBlocked, map[string]any{"assigned_user_id": f.alice.ID})
		f.status(t, f.bobTok, doneButBlocked, "done")
		// A doc is reference, not open work (PLAN-3535): assigned and
		// blocked, it is still not something that needs Alice.
		refDoc := f.create(t, f.bobTok, "docs", "Assigned blocked doc")
		f.patch(t, f.bobTok, refDoc, map[string]any{"assigned_user_id": f.alice.ID})
		blocker := f.create(t, f.bobTok, "tasks", "Blocker")
		doneBlocker := f.create(t, f.bobTok, "tasks", "Done blocker")
		f.status(t, f.bobTok, doneBlocker, "done")
		for _, l := range []struct{ src, tgt models.Item }{{blocker, blocked}, {blocker, notMine}, {doneBlocker, freed}, {blocker, doneButBlocked}, {blocker, refDoc}} {
			if _, err := srv.store.CreateItemLink(f.ws.ID, models.ItemLinkCreate{TargetID: l.tgt.ID, LinkType: "blocks"}, l.src.ID); err != nil {
				t.Fatalf("CreateItemLink: %v", err)
			}
		}
		got := f.catchUp(t, f.aliceTok)
		task3615Want(t, "needs_you", got.NeedsYou, "Assigned and blocked")
		if len(got.NeedsYou.Lines) == 1 && got.NeedsYou.Lines[0].What != "blocked" {
			t.Errorf("needs_you what = %q, want blocked", got.NeedsYou.Lines[0].What)
		}
	})
}

func TestTASK3615_AgentsGroupedByName(t *testing.T) {
	t.Parallel()
	bothBackends(t, func(t *testing.T, srv *Server) {
		f := newTask3615Fixture(t, srv)
		a := f.create(t, f.bobTok, "tasks", "Agent target A")
		b := f.create(t, f.bobTok, "tasks", "Agent target B")
		seed := func(doc models.Item, meta string) {
			if _, err := srv.store.CreateActivity(models.Activity{WorkspaceID: f.ws.ID, DocumentID: doc.ID, Action: "updated", Actor: "agent", Source: "cli", Metadata: meta, UserID: f.alice.ID}); err != nil {
				t.Fatalf("CreateActivity: %v", err)
			}
		}
		seed(a, `{"agent":"wren"}`)
		seed(a, `{"agent":"wren"}`)
		seed(b, `{"agent":"wren"}`)
		seed(b, `{"agent":"rook"}`)
		// Malformed metadata can only exist on SQLite: Postgres's JSONB
		// refuses it at write time.
		malformed := srv.store.D().Driver() != store.DriverPostgres
		if malformed {
			seed(b, `not json`)
		}
		f.pin(t, task3615At)

		got := f.catchUp(t, f.aliceTok).Agents
		byName := map[string]store.CatchUpAgentGroup{}
		for _, g := range got.Groups {
			byName[g.Agent] = g
		}
		// The viewer's own agents stay in: these rows carry Alice's user id.
		if g := byName["wren"]; g.Changes != 3 || g.Items != 2 {
			t.Errorf("wren: %+v, want 3 changes on 2 items", g)
		}
		if g := byName["rook"]; g.Changes != 1 || g.Items != 1 {
			t.Errorf("rook: %+v, want 1 change on 1 item", g)
		}
		wantTotal := 2
		if malformed {
			wantTotal = 3
			if g := byName["agent"]; g.Changes != 1 {
				t.Errorf("malformed metadata: %+v, want one unnamed change", g)
			}
		}
		if got.Total != wantTotal {
			t.Errorf("agents total %d, want %d", got.Total, wantTotal)
		}
	})
}

func TestTASK3615_SignedOutIsRefused(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	f := newTask3615Fixture(t, srv)
	for _, m := range []string{"GET /catch-up", "PUT /me/seen"} {
		method, path := m[:3], m[4:]
		rr := doRequest(srv, method, "/api/v1/workspaces/"+f.ws.Slug+path, nil)
		// A signed-out write is refused by middleware (403) before the
		// handler's own 401; either way it is refused.
		if rr.Code != http.StatusUnauthorized && rr.Code != http.StatusForbidden {
			t.Errorf("%s signed out: %d, want 401 or 403", m, rr.Code)
		}
	}
}

// CatchUpItemLines is the only visibility gate on needs_you's attention-set
// ids (the decision runner's map is workspace-wide), so it is pinned here
// directly rather than through a provider.
func TestTASK3615_NeedsYouLinesAreScoped(t *testing.T) {
	t.Parallel()
	bothBackends(t, func(t *testing.T, srv *Server) {
		f := newTask3615Fixture(t, srv)
		task := f.create(t, f.bobTok, "tasks", "Visible")
		idea := f.create(t, f.bobTok, "ideas", "Hidden")
		what := map[string]string{task.ID: "needs a decision", idea.ID: "needs a decision"}
		ids := []string{idea.ID, task.ID}

		all, err := srv.store.CatchUpItemLines(f.ws.ID, ids, what, store.CatchUpScope{})
		if err != nil {
			t.Fatalf("CatchUpItemLines: %v", err)
		}
		task3615Want(t, "unscoped control", all, "Visible", "Hidden")
		if all.Lines[0].Title != "Hidden" {
			t.Errorf("order not kept: %v", task3615TitlesInOrder(all))
		}

		scoped, err := srv.store.CatchUpItemLines(f.ws.ID, ids, what, store.CatchUpScope{CollectionIDs: []string{f.tasksID}})
		if err != nil {
			t.Fatalf("CatchUpItemLines: %v", err)
		}
		task3615Want(t, "scoped", scoped, "Visible")

		granted, err := srv.store.CatchUpItemLines(f.ws.ID, ids, what, store.CatchUpScope{CollectionIDs: []string{}, ItemIDs: []string{idea.ID}})
		if err != nil {
			t.Fatalf("CatchUpItemLines: %v", err)
		}
		task3615Want(t, "grant only", granted, "Hidden")

		none, err := srv.store.CatchUpItemLines(f.ws.ID, ids, what, store.CatchUpScope{CollectionIDs: []string{}})
		if err != nil {
			t.Fatalf("CatchUpItemLines: %v", err)
		}
		task3615Want(t, "sees nothing", none)
	})
}

func task3615TitlesInOrder(sec store.CatchUpSection) []string {
	out := []string{}
	for _, l := range sec.Lines {
		out = append(out, l.Title)
	}
	return out
}
