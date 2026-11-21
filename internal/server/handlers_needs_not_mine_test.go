package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// "Not mine" (TASK-4643): PUT|DELETE .../needs/not-mine and the hold it puts
// on GET /me/needs. The router tests run on SQLite and, with
// PAD_TEST_POSTGRES_URL set, on Postgres.

// at pins a comment and its mentions to one second.
func (f *nfx) at(c *models.Comment, at time.Time) {
	f.t.Helper()
	ts := at.UTC().Format(time.RFC3339)
	for _, q := range []string{
		fmt.Sprintf("UPDATE comments SET created_at = '%s' WHERE id = '%s'", ts, c.ID),
		fmt.Sprintf("UPDATE comment_mentions SET created_at = '%s' WHERE comment_id = '%s'", ts, c.ID),
	} {
		if _, err := f.srv.store.DB().Exec(q); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *nfx) exec(q string) {
	f.t.Helper()
	if _, err := f.srv.store.DB().Exec(q); err != nil {
		f.t.Fatal(err)
	}
}

func (f *nfx) notMine(u *models.User, ws *models.Workspace, it *models.Item, m *models.NeedsSnoozeMarker) {
	f.t.Helper()
	if _, err := f.srv.store.MarkNeedsNotMine(u.ID, ws.ID, it.ID, models.NeedsSourceWeb, m); err != nil {
		f.t.Fatal(err)
	}
}

// held asserts the item is hidden from u's active view and shown, with its
// not-mine, in the hidden view.
func (f *nfx) held(label string, u *models.User, title string) {
	f.t.Helper()
	a := f.need(u, "")
	for _, r := range a.Rows {
		if r.Item.Title == title {
			f.t.Fatalf("%s: %q is in the active view", label, title)
		}
	}
	h := f.need(u, "?view=hidden")
	for _, r := range h.Rows {
		if r.Item.Title == title {
			if r.NotMine == nil {
				f.t.Fatalf("%s: hidden row has no not_mine", label)
			}
			if a.Hidden != h.Total {
				f.t.Fatalf("%s: hidden count %d, hidden view %d", label, a.Hidden, h.Total)
			}
			return
		}
	}
	f.t.Fatalf("%s: %q is not in the hidden view (%v)", label, title, titles(h.Rows))
}

// released asserts the item is back in u's active view with no not-mine.
func (f *nfx) released(label string, u *models.User, title string) {
	f.t.Helper()
	for _, r := range f.need(u, "").Rows {
		if r.Item.Title == title {
			if r.NotMine != nil {
				f.t.Fatalf("%s: released row still carries not_mine", label)
			}
			return
		}
	}
	f.t.Fatalf("%s: %q is not in the active view", label, title)
}

func notMineSchemaWaiting() string {
	return `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","doing","needs-answer","done"],"default":"open","required":true,"awaits_human_options":["needs-answer"]}]}`
}

func TestNotMine_HoldsThroughAgentComments(t *testing.T) {
	t.Parallel()
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newNfx(t, srv)
		owner, alice, bob := f.user("owner"), f.user("alice"), f.user("bob")
		ws := f.workspace("One", owner)
		f.member(ws, alice, "editor")
		f.member(ws, bob, "editor")
		asks := f.collection(ws, "Asks", true)
		it := f.item(ws, asks, "asked of alice", alice, nfxDay(1))
		wantTitles(t, "before", f.need(alice, "").Rows, "asked of alice")
		f.notMine(alice, ws, it, nil)
		f.held("fresh", alice, "asked of alice")

		later := time.Now().Add(time.Hour)
		agent, err := srv.store.CreateComment(ws.ID, it.ID, owner.ID, models.CommentCreate{Body: "status: working on it", Author: "Seat", CreatedBy: "agent", Source: "cli"})
		if err != nil {
			t.Fatal(err)
		}
		f.at(agent, later)
		f.at(f.comment(ws, it, bob, "@owner-nx this one is yours"), later.Add(time.Second))
		mine, err := srv.store.CreateComment(ws.ID, it.ID, alice.ID, models.CommentCreate{Body: "agent note", Author: "Alice", CreatedBy: "agent", Source: "cli"})
		if err != nil {
			t.Fatal(err)
		}
		f.at(mine, later.Add(2*time.Second))
		f.held("after agent and other comments", alice, "asked of alice")
	})
}

func TestNotMine_ReleasesOnMention(t *testing.T) {
	t.Parallel()
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newNfx(t, srv)
		owner, alice := f.user("owner"), f.user("alice")
		ws := f.workspace("One", owner)
		f.member(ws, alice, "editor")
		asks := f.collection(ws, "Asks", true)
		it := f.item(ws, asks, "asked of alice", alice, nfxDay(1))
		f.notMine(alice, ws, it, nil)
		f.held("fresh", alice, "asked of alice")
		f.at(f.comment(ws, it, owner, "@alice-nx it really is yours"), time.Now().Add(time.Hour))
		f.released("after a mention", alice, "asked of alice")
	})
}

func TestNotMine_SameSecondMentionInMarkerHolds(t *testing.T) {
	t.Parallel()
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newNfx(t, srv)
		owner, alice, bob := f.user("owner"), f.user("alice"), f.user("bob")
		ws := f.workspace("One", owner)
		f.member(ws, alice, "editor")
		f.member(ws, bob, "editor")
		tasks := f.collection(ws, "Tasks", false)
		it := f.item(ws, tasks, "mentioned", nil, nfxDay(1))
		s := nfxDay(5)
		c1 := f.comment(ws, it, owner, "@alice-nx thoughts?")
		f.at(c1, s)
		stamp := s.Format(time.RFC3339)
		f.notMine(alice, ws, it, &models.NeedsSnoozeMarker{SeenCommentAt: stamp, SeenCommentIDs: []string{c1.ID}, SeenAt: stamp, SeenCollectionID: tasks.ID})
		f.held("the mention in the marker, at seen_at's second", alice, "mentioned")
		// Another mention at that same second, not in the marker, releases.
		f.at(f.comment(ws, it, bob, "@alice-nx me too"), s)
		f.released("a same-second mention outside the marker", alice, "mentioned")
	})
}

func TestNotMine_ReleasesOnWaitingTransition(t *testing.T) {
	t.Parallel()
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newNfx(t, srv)
		owner, alice := f.user("owner"), f.user("alice")
		ws := f.workspace("One", owner)
		f.member(ws, alice, "editor")
		yes := true
		asks, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Asks", Schema: notMineSchemaWaiting(), AwaitsHuman: &yes})
		if err != nil {
			t.Fatal(err)
		}
		it := f.item(ws, asks, "asked of alice", alice, nfxDay(1))
		f.notMine(alice, ws, it, nil)
		set := func(status string) {
			t.Helper()
			fields := `{"status":"` + status + `"}`
			if _, err := srv.store.UpdateItem(it.ID, models.ItemUpdate{Fields: &fields}); err != nil {
				t.Fatal(err)
			}
		}
		set("doing")
		f.held("a transition to an ordinary status", alice, "asked of alice")
		set("needs-answer")
		f.released("a transition into a waiting status", alice, "asked of alice")
	})
}

func TestNotMine_ReleasesOnMoveToAsk(t *testing.T) {
	t.Parallel()
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newNfx(t, srv)
		owner, alice := f.user("owner"), f.user("alice")
		ws := f.workspace("One", owner)
		f.member(ws, alice, "editor")
		tasks := f.collection(ws, "Tasks", false)
		asks := f.collection(ws, "Asks", true)
		it := f.item(ws, tasks, "mentioned", nil, nfxDay(1))
		f.at(f.comment(ws, it, owner, "@alice-nx thoughts?"), nfxDay(2))
		f.notMine(alice, ws, it, nil)
		f.held("fresh", alice, "mentioned")
		f.exec(fmt.Sprintf("UPDATE items SET collection_id = '%s' WHERE id = '%s'", asks.ID, it.ID))
		f.released("moved into an ask collection", alice, "mentioned")
	})
}

func TestNotMine_ReleasesOnAssignToViewer(t *testing.T) {
	t.Parallel()
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newNfx(t, srv)
		owner, alice, bob := f.user("owner"), f.user("alice"), f.user("bob")
		ws := f.workspace("One", owner)
		f.member(ws, alice, "editor")
		f.member(ws, bob, "editor")
		tasks := f.collection(ws, "Tasks", false)
		it := f.item(ws, tasks, "mentioned", bob, nfxDay(1))
		f.at(f.comment(ws, it, owner, "@alice-nx thoughts?"), nfxDay(2))
		f.notMine(alice, ws, it, nil)
		f.exec(fmt.Sprintf("UPDATE items SET assigned_user_id = '%s' WHERE id = '%s'", owner.ID, it.ID))
		f.held("reassigned to someone else", alice, "mentioned")
		f.exec(fmt.Sprintf("UPDATE items SET assigned_user_id = '%s' WHERE id = '%s'", alice.ID, it.ID))
		f.released("assigned to the viewer", alice, "mentioned")
	})
}

// An item assigned to the viewer when they marked it, then assigned away and
// back: the assignee id matches the marker, so only the assignment's time
// (the latest "assigned:" activity) can release it.
func TestNotMine_ReleasesOnReassignBackToViewer(t *testing.T) {
	t.Parallel()
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newNfx(t, srv)
		owner, alice := f.user("owner"), f.user("alice")
		ws := f.workspace("One", owner)
		f.member(ws, alice, "editor")
		tasks := f.collection(ws, "Tasks", false)
		it := f.item(ws, tasks, "back to me", alice, nfxDay(1))
		f.at(f.comment(ws, it, owner, "@alice-nx thoughts?"), nfxDay(2))
		f.notMine(alice, ws, it, nil)
		f.exec(fmt.Sprintf("UPDATE item_needs_not_mine SET seen_at = '%s' WHERE user_id = '%s' AND item_id = '%s'",
			nfxDay(10).Format(time.RFC3339), alice.ID, it.ID))
		assigned := func(id string, at time.Time) {
			t.Helper()
			f.exec(fmt.Sprintf(`INSERT INTO activities (id, workspace_id, document_id, action, actor, source, metadata, created_at)
				VALUES ('%s', '%s', '%s', 'updated', 'user', 'web', '{"changes":"assigned: Owner Person → Alice Person"}', '%s')`,
				id, ws.ID, it.ID, at.Format(time.RFC3339)))
		}
		assigned("act-before", nfxDay(5))
		f.held("assigned to her before the mark", alice, "back to me")
		assigned("act-same", nfxDay(10))
		f.held("assigned at the mark's second", alice, "back to me")
		assigned("act-after", nfxDay(20))
		f.released("assigned away and back after the mark", alice, "back to me")
	})
}

func TestNotMine_ReleasesOnDueReminder(t *testing.T) {
	t.Parallel()
	// The agenda clock is in the future (a schedule must start after the
	// real now), so seen_at is set on the row directly: the store would
	// clamp a marker's seen_at to the real now.
	w := newBootAgendaWorld(t, "2031-03-10T12:00:00Z")
	f := newNfx(t, w.srv)
	viewer := f.user("recipient")
	f.member(w.ws, viewer, "editor")
	it := w.sched(t, viewer, "Due item", "2031-03-10T09:00", "human")
	wantTitles(t, "before", f.need(viewer, "").Rows, "Due item")
	f.notMine(viewer, w.ws, it, nil)
	seen := func(at string) {
		t.Helper()
		f.exec(fmt.Sprintf("UPDATE item_needs_not_mine SET seen_at = '%s' WHERE user_id = '%s' AND item_id = '%s'", at, viewer.ID, it.ID))
	}
	seen("2031-03-10T10:00:00Z")
	f.held("marked after the reminder came due", viewer, "Due item")
	seen("2031-03-10T09:00:00Z")
	f.held("marked the second it came due", viewer, "Due item")
	seen("2031-03-10T08:59:59Z")
	f.released("a reminder that came due after the mark", viewer, "Due item")
}

// A Not mine marked while the reminder was snoozed releases when the snooze
// ends and the reminder re-fires (TASK-4711).
func TestNotMine_ReleasesOnSnoozeRefire(t *testing.T) {
	t.Parallel()
	w := newBootAgendaWorld(t, "2031-03-10T12:00:00Z")
	f := newNfx(t, w.srv)
	viewer := f.user("recipient")
	f.member(w.ws, viewer, "editor")
	it := w.sched(t, viewer, "Snoozed item", "2031-03-10T09:00", "human")
	refire := "2031-03-11T09:00:00Z"
	f.exec(fmt.Sprintf("UPDATE item_schedules SET next_override_at = '%s' WHERE item_id = '%s'", refire, it.ID))
	wantTitles(t, "snoozed", f.need(viewer, "").Rows)
	f.notMine(viewer, w.ws, it, nil)
	f.exec(fmt.Sprintf("UPDATE item_needs_not_mine SET seen_at = '%s' WHERE user_id = '%s' AND item_id = '%s'", "2031-03-10T12:00:00Z", viewer.ID, it.ID))
	clock := time.Date(2031, 3, 11, 10, 0, 0, 0, time.UTC)
	w.srv.agendaClock = func() time.Time { return clock }
	f.released("the snoozed reminder re-fired after the mark", viewer, "Snoozed item")
}

func TestNotMine_PerUser(t *testing.T) {
	t.Parallel()
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newNfx(t, srv)
		owner, alice, bob := f.user("owner"), f.user("alice"), f.user("bob")
		ws := f.workspace("One", owner)
		f.member(ws, alice, "editor")
		f.member(ws, bob, "editor")
		tasks := f.collection(ws, "Tasks", false)
		it := f.item(ws, tasks, "both asked", nil, nfxDay(1))
		f.at(f.comment(ws, it, owner, "@alice-nx and @bob-nx please decide"), nfxDay(2))
		f.notMine(alice, ws, it, nil)
		f.held("alice", alice, "both asked")
		b := f.need(bob, "")
		wantTitles(t, "bob", b.Rows, "both asked")
		if b.Hidden != 0 || b.Rows[0].NotMine != nil {
			t.Fatalf("bob sees alice's mark: hidden %d row %+v", b.Hidden, b.Rows[0].NotMine)
		}
	})
}

// notMineRoute seeds one workspace through the router for the HTTP tests.
type notMineRoute struct {
	srv   *Server
	ws    *models.Workspace
	item  models.Item
	path  string
	owner *models.User
}

func newNotMineRoute(t *testing.T) *notMineRoute {
	t.Helper()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items",
		map[string]interface{}{"title": "Asked", "fields": `{"status":"open"}`})
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed item: %d %s", rr.Code, rr.Body.String())
	}
	r := &notMineRoute{srv: srv}
	parseJSON(t, rr, &r.item)
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil {
		t.Fatal(err)
	}
	r.ws = ws
	r.path = "/api/v1/workspaces/" + slug + "/items/" + r.item.Slug + "/needs/not-mine"
	r.owner = r.user(t, "owner@example.com")
	if err := srv.store.AddWorkspaceMember(ws.ID, r.owner.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *notMineRoute) user(t *testing.T, email string) *models.User {
	t.Helper()
	u, err := r.srv.store.CreateUser(models.UserCreate{Email: email, Name: email, Password: "pw-test-12345"})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func (r *notMineRoute) token(t *testing.T, u *models.User) string {
	t.Helper()
	tok, err := r.srv.store.CreateAPIToken(u.ID, models.APITokenCreate{Name: "t"}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return tok.Token
}

func (r *notMineRoute) row(t *testing.T, u *models.User) *models.NeedsNotMine {
	t.Helper()
	nm, err := r.srv.store.GetNeedsNotMine(u.ID, r.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	return nm
}

func TestNotMine_InvisibleItem404(t *testing.T) {
	t.Parallel()
	r := newNotMineRoute(t)
	ideas, err := r.srv.store.GetCollectionBySlug(r.ws.ID, "ideas")
	if err != nil || ideas == nil {
		t.Fatalf("ideas collection: %v", err)
	}
	restricted := r.user(t, "restricted@example.com")
	if err := r.srv.store.AddWorkspaceMember(r.ws.ID, restricted.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	if err := r.srv.store.SetMemberCollectionAccess(r.ws.ID, restricted.ID, "specific", []string{ideas.ID}); err != nil {
		t.Fatal(err)
	}
	tok := r.token(t, restricted)
	missing := "/api/v1/workspaces/" + r.ws.Slug + "/items/TASK-99999/needs/not-mine"
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		hidden := bearerCall(t, r.srv, method, r.path, tok, []byte(`{}`))
		absent := bearerCall(t, r.srv, method, missing, tok, []byte(`{}`))
		if hidden.Code != http.StatusNotFound || absent.Code != http.StatusNotFound {
			t.Fatalf("%s: invisible %d, nonexistent %d; want 404 both", method, hidden.Code, absent.Code)
		}
		if hidden.Body.String() != absent.Body.String() {
			t.Fatalf("%s: invisible and nonexistent differ:\n%s\n%s", method, hidden.Body.String(), absent.Body.String())
		}
	}
	if r.row(t, restricted) != nil {
		t.Fatal("a refused PUT wrote a not-mine")
	}
}

func TestNotMine_ReplacesSnooze(t *testing.T) {
	t.Parallel()
	r := newNotMineRoute(t)
	tok := r.token(t, r.owner)
	snoozePath := "/api/v1/workspaces/" + r.ws.Slug + "/items/" + r.item.Slug + "/needs/snooze"
	snoozed := func() bool {
		sn, err := r.srv.store.GetNeedsSnooze(r.owner.ID, r.item.ID, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return sn != nil
	}
	if rr := bearerCall(t, r.srv, http.MethodPut, snoozePath, tok, []byte(`{"kind":"dismiss"}`)); rr.Code != http.StatusOK {
		t.Fatalf("snooze: %d %s", rr.Code, rr.Body.String())
	}
	if rr := bearerCall(t, r.srv, http.MethodPut, r.path, tok, nil); rr.Code != http.StatusOK {
		t.Fatalf("not mine: %d %s", rr.Code, rr.Body.String())
	}
	if snoozed() || r.row(t, r.owner) == nil {
		t.Fatal("a not-mine did not replace the snooze")
	}
	if rr := bearerCall(t, r.srv, http.MethodPut, snoozePath, tok, []byte(`{"kind":"dismiss"}`)); rr.Code != http.StatusOK {
		t.Fatalf("snooze: %d %s", rr.Code, rr.Body.String())
	}
	if !snoozed() || r.row(t, r.owner) != nil {
		t.Fatal("a snooze did not replace the not-mine")
	}
}

func TestNotMine_DeleteIdempotent(t *testing.T) {
	t.Parallel()
	r := newNotMineRoute(t)
	tok := r.token(t, r.owner)
	del := func(label string) {
		t.Helper()
		rr := bearerCall(t, r.srv, http.MethodDelete, r.path, tok, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", label, rr.Code, rr.Body.String())
		}
		var body map[string]any
		parseJSON(t, rr, &body)
		if v, ok := body["not_mine"]; !ok || v != nil || body["ref"] != r.item.Ref {
			t.Fatalf("%s: body %v", label, body)
		}
	}
	del("with none")
	rr := bearerCall(t, r.srv, http.MethodPut, r.path, tok, []byte(`{}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", rr.Code, rr.Body.String())
	}
	var put struct {
		Ref     string              `json:"ref"`
		NotMine models.NeedsNotMine `json:"not_mine"`
	}
	parseJSON(t, rr, &put)
	if put.Ref != r.item.Ref || put.NotMine.ItemID != r.item.ID || put.NotMine.Marker.SeenCollectionID != r.item.CollectionID {
		t.Fatalf("PUT body %+v", put)
	}
	del("with one")
	if r.row(t, r.owner) != nil {
		t.Fatal("the row survived DELETE")
	}
	del("again")
}

func TestNotMine_SourceFromAuth(t *testing.T) {
	t.Parallel()
	r := newNotMineRoute(t)
	// A token is "cli", whatever the body claims.
	rr := bearerCall(t, r.srv, http.MethodPut, r.path, r.token(t, r.owner), []byte(`{"source":"web"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("bearer PUT: %d %s", rr.Code, rr.Body.String())
	}
	if nm := r.row(t, r.owner); nm == nil || nm.Source != models.NeedsSourceCLI {
		t.Fatalf("bearer source: %+v", nm)
	}
	// A cookie session is "web", whatever the body claims.
	session, err := r.srv.store.CreateSession(r.owner.ID, "web-test", "192.0.2.1", "", webSessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut, r.path, bytes.NewReader([]byte(`{"source":"cli"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.1:1234"
	req.AddCookie(&http.Cookie{Name: "pad_session", Value: session})
	const csrf = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	req.AddCookie(&http.Cookie{Name: "pad_csrf", Value: csrf})
	req.Header.Set("X-CSRF-Token", csrf)
	rec := httptest.NewRecorder()
	r.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("cookie PUT: %d %s", rec.Code, rec.Body.String())
	}
	if nm := r.row(t, r.owner); nm == nil || nm.Source != models.NeedsSourceWeb {
		t.Fatalf("cookie source: %+v", nm)
	}
}

// Row.CanEdit is the viewer's own edit right: a role that reaches the
// collection, else the item grant, else the collection grant.
func TestNotMine_RowCanEdit(t *testing.T) {
	t.Parallel()
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newNfx(t, srv)
		owner, editor, viewer, viewerEdit, guestEdit, guestView := f.user("owner"), f.user("editor"), f.user("viewer"), f.user("viewedit"), f.user("guestedit"), f.user("guestview")
		ws := f.workspace("One", owner)
		f.member(ws, editor, "editor")
		f.member(ws, viewer, "viewer")
		f.member(ws, viewerEdit, "viewer")
		tasks := f.collection(ws, "Tasks", false)
		it := f.item(ws, tasks, "everyone asked", nil, nfxDay(1))
		for _, g := range []struct {
			u    *models.User
			perm string
		}{{viewerEdit, "edit"}, {guestEdit, "edit"}, {guestView, "view"}} {
			if _, err := srv.store.CreateItemGrant(ws.ID, it.ID, g.u.ID, g.perm, owner.ID); err != nil {
				t.Fatal(err)
			}
		}
		f.at(f.comment(ws, it, owner, "@editor-nx @viewer-nx @viewedit-nx @guestedit-nx @guestview-nx please look"), nfxDay(2))
		for _, tc := range []struct {
			u    *models.User
			want bool
		}{{editor, true}, {viewer, false}, {viewerEdit, true}, {guestEdit, true}, {guestView, false}} {
			resp := f.need(tc.u, "")
			wantTitles(t, tc.u.Name, resp.Rows, "everyone asked")
			if got := resp.Rows[0].CanEdit; got != tc.want {
				t.Errorf("%s can_edit = %v, want %v", tc.u.Name, got, tc.want)
			}
			raw := f.get(tc.u, "").Body.String()
			if want := fmt.Sprintf(`"can_edit":%v`, tc.want); !bytes.Contains([]byte(raw), []byte(want)) {
				t.Errorf("%s: wire lacks %s", tc.u.Name, want)
			}
		}
	})
}

// The row's marker carries what a not-mine needs: when the list was read, the
// item's collection, and the viewer's own id only when it is theirs.
func TestNotMine_RowMarker(t *testing.T) {
	t.Parallel()
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newNfx(t, srv)
		owner, alice, bob := f.user("owner"), f.user("alice"), f.user("bob")
		ws := f.workspace("One", owner)
		f.member(ws, alice, "editor")
		f.member(ws, bob, "editor")
		asks := f.collection(ws, "Asks", true)
		tasks := f.collection(ws, "Tasks", false)
		f.item(ws, asks, "asked of alice", alice, nfxDay(1))
		other := f.item(ws, tasks, "bob's, mentions alice", bob, nfxDay(2))
		f.at(f.comment(ws, other, owner, "@alice-nx thoughts?"), nfxDay(3))
		resp := f.need(alice, "")
		wantTitles(t, "alice", resp.Rows, "asked of alice", "bob's, mentions alice")
		mine, theirs := resp.Rows[0].Marker, resp.Rows[1].Marker
		if mine.SeenAssigneeID != alice.ID || mine.SeenCollectionID != asks.ID || mine.SeenAt == "" {
			t.Fatalf("own row marker %+v", mine)
		}
		if theirs.SeenAssigneeID != "" || theirs.SeenCollectionID != tasks.ID {
			t.Fatalf("another's row marker %+v", theirs)
		}
		if bytes.Contains(f.get(alice, "").Body.Bytes(), []byte(bob.ID)) {
			t.Fatal("a row names another person's id")
		}
		// Echoing the row's marker back holds the item.
		b, _ := json.Marshal(mine)
		var m models.NeedsSnoozeMarker
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		it := itemIDByTitle(t, srv, ws, "asked of alice")
		if _, err := srv.store.MarkNeedsNotMine(alice.ID, ws.ID, it, models.NeedsSourceWeb, &m); err != nil {
			t.Fatal(err)
		}
		f.held("the row's own marker", alice, "asked of alice")
	})
}

// A not-mine moves like a snooze: a cross-workspace move leaves it on the
// archived source, so the clone is not hidden by it.
func TestNotMine_MovedItem(t *testing.T) {
	t.Parallel()
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newNfx(t, srv)
		owner, alice := f.user("owner"), f.user("alice")
		w1, w2 := f.workspace("One", owner), f.workspace("Two", owner)
		f.member(w1, alice, "editor")
		f.member(w2, alice, "editor")
		a1, a2 := f.collection(w1, "Asks", true), f.collection(w2, "Asks", true)
		src := f.item(w1, a1, "moving", alice, nfxDay(1))
		f.notMine(alice, w1, src, nil)
		f.held("before the move", alice, "moving")
		if _, err := srv.store.CopyItemAcrossWorkspaces(store.CrossWorkspaceCopyRequest{
			SourceItemID: src.ID, TargetWorkspaceID: w2.ID, TargetCollectionID: a2.ID,
			ArchiveSource: true, Actor: alice.ID, ActorUserID: alice.ID,
		}); err != nil {
			t.Fatalf("copy: %v", err)
		}
		resp := f.need(alice, "")
		if resp.Hidden != 0 {
			t.Fatalf("the archived source's not-mine was counted: hidden %d", resp.Hidden)
		}
		wantTitles(t, "the clone", resp.Rows, "moving")
		if resp.Rows[0].Workspace.Slug != w2.Slug || resp.Rows[0].NotMine != nil {
			t.Fatalf("clone row %+v", resp.Rows[0])
		}
	})
}
