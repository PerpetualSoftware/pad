package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/decision"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/needs"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// TASK-3920: GET /me/needs, the cross-workspace needs-you inbox. Every test
// runs on SQLite and, with PAD_TEST_POSTGRES_URL set, on Postgres too.
// Fixtures are built through the store, and the handler is called with the
// request context the middleware would have built, so each test is about the
// router and its visibility rules.

func needsBackends(t *testing.T, fn func(t *testing.T, srv *Server)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) { fn(t, testServer(t)) })
	t.Run("postgres", func(t *testing.T) {
		srv, _ := testServerPostgres(t)
		fn(t, srv)
	})
}

type nfx struct {
	t   *testing.T
	srv *Server
}

func newNfx(t *testing.T, srv *Server) *nfx { return &nfx{t: t, srv: srv} }

func (f *nfx) user(name string) *models.User {
	f.t.Helper()
	u, err := f.srv.store.CreateUser(models.UserCreate{Email: name + "@needs.test", Name: strings.ToUpper(name[:1]) + name[1:] + " Person", Username: name + "-nx", Password: "pw-test-12345"})
	if err != nil {
		f.t.Fatalf("CreateUser %s: %v", name, err)
	}
	return u
}

// workspace makes a workspace owned (and so a member as owner) by owner.
func (f *nfx) workspace(name string, owner *models.User) *models.Workspace {
	f.t.Helper()
	ws, err := f.srv.store.CreateWorkspace(models.WorkspaceCreate{Name: name, OwnerID: owner.ID})
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.srv.store.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		f.t.Fatal(err)
	}
	return ws
}

func (f *nfx) member(ws *models.Workspace, u *models.User, role string) {
	f.t.Helper()
	if err := f.srv.store.AddWorkspaceMember(ws.ID, u.ID, role); err != nil {
		f.t.Fatal(err)
	}
}

const nfxSchema = `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","done"],"default":"open","required":true}]}`

func (f *nfx) collection(ws *models.Workspace, name string, asks bool) *models.Collection {
	f.t.Helper()
	in := models.CollectionCreate{Name: name, Schema: nfxSchema}
	if asks {
		yes := true
		in.AwaitsHuman = &yes
	}
	c, err := f.srv.store.CreateCollection(ws.ID, in)
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

// item creates an open item; assignee may be nil. created pins created_at.
func (f *nfx) item(ws *models.Workspace, c *models.Collection, title string, assignee *models.User, created time.Time) *models.Item {
	f.t.Helper()
	in := models.ItemCreate{Title: title, Fields: `{"status":"open"}`}
	if assignee != nil {
		in.AssignedUserID = &assignee.ID
	}
	it, err := f.srv.store.CreateItem(ws.ID, c.ID, in)
	if err != nil {
		f.t.Fatal(err)
	}
	f.setCreated(it, created)
	return it
}

// forceAssign assigns an item to a user who is not a member (a guest): the
// store refuses that on write, but a person's membership can end after the
// assignment, which is the shape this reproduces.
func (f *nfx) forceAssign(it *models.Item, u *models.User) {
	f.t.Helper()
	if _, err := f.srv.store.DB().Exec(fmt.Sprintf("UPDATE items SET assigned_user_id = '%s' WHERE id = '%s'", u.ID, it.ID)); err != nil {
		f.t.Fatal(err)
	}
}

func (f *nfx) setCreated(it *models.Item, at time.Time) {
	f.t.Helper()
	if _, err := f.srv.store.DB().Exec(fmt.Sprintf("UPDATE items SET created_at = '%s' WHERE id = '%s'", at.UTC().Format(time.RFC3339), it.ID)); err != nil {
		f.t.Fatal(err)
	}
}

func (f *nfx) comment(ws *models.Workspace, it *models.Item, by *models.User, body string) *models.Comment {
	f.t.Helper()
	c, err := f.srv.store.CreateComment(ws.ID, it.ID, by.ID, models.CommentCreate{Body: body, Author: by.Name, CreatedBy: "user", Source: "web"})
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

// get calls the handler as user, with the context mods a token would add.
func (f *nfx) get(u *models.User, query string, mods ...func(context.Context) context.Context) *httptest.ResponseRecorder {
	f.t.Helper()
	r := httptest.NewRequest("GET", "/api/v1/me/needs"+query, nil)
	ctx := r.Context()
	if u != nil {
		ctx = WithCurrentUser(ctx, u)
	}
	for _, m := range mods {
		ctx = m(ctx)
	}
	rr := httptest.NewRecorder()
	f.srv.handleMeNeeds(rr, r.WithContext(ctx))
	return rr
}

func (f *nfx) need(u *models.User, query string, mods ...func(context.Context) context.Context) needsResponse {
	f.t.Helper()
	rr := f.get(u, query, mods...)
	if rr.Code != http.StatusOK {
		f.t.Fatalf("GET /me/needs%s as %v: %d %s", query, u != nil, rr.Code, rr.Body.String())
	}
	var resp needsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		f.t.Fatalf("parse: %v\n%s", err, rr.Body.String())
	}
	return resp
}

func titles(rows []*needs.Row) []string {
	out := []string{}
	for _, r := range rows {
		out = append(out, r.Item.Title)
	}
	return out
}

func wantTitles(t *testing.T, label string, rows []*needs.Row, want ...string) {
	t.Helper()
	got := titles(rows)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("%s: rows %v, want %v", label, got, want)
	}
}

var nfxT0 = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

func nfxDay(d int) time.Time { return nfxT0.AddDate(0, 0, d) }

func TestNeedsMe_NoAuth401(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newNfx(t, srv)
		f.user("someone") // once a user exists, the API needs a credential
		if rr := doRequest(srv, "GET", "/api/v1/me/needs", nil); rr.Code != http.StatusUnauthorized {
			t.Fatalf("no credential: %d", rr.Code)
		}
		if rr := f.get(nil, ""); rr.Code != http.StatusUnauthorized {
			t.Fatalf("handler with no user: %d", rr.Code)
		}
	})
}

func TestNeedsMe_AppContextRefused(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newNfx(t, srv)
		u := f.user("alice")
		r := httptest.NewRequest("GET", "/api/v1/me/needs", nil)
		ctx := context.WithValue(WithCurrentUser(r.Context(), u), appCtxKey{}, &appContext{})
		rr := httptest.NewRecorder()
		srv.handleMeNeeds(rr, r.WithContext(ctx))
		if rr.Code != http.StatusNotFound {
			t.Fatalf("an app context got %d, want 404", rr.Code)
		}
	})
}

func TestNeedsMe_CrossWorkspace(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newNfx(t, srv)
		owner, alice := f.user("owner"), f.user("alice")
		w1, w2, w3 := f.workspace("One", owner), f.workspace("Two", owner), f.workspace("Three", owner)
		f.member(w1, alice, "editor")
		f.member(w2, alice, "editor")
		a1, a2, a3 := f.collection(w1, "Asks", true), f.collection(w2, "Asks", true), f.collection(w3, "Asks", true)
		f.item(w1, a1, "w1 late", alice, nfxDay(10))
		f.item(w2, a2, "w2 early", alice, nfxDay(1))
		f.item(w1, a1, "w1 mid", alice, nfxDay(5))
		g := f.item(w3, a3, "guest item", nil, nfxDay(3))
		f.forceAssign(g, alice)
		// alice is only a GUEST in three: a collection grant, no membership.
		if _, err := srv.store.CreateCollectionGrant(w3.ID, a3.ID, alice.ID, "view", owner.ID); err != nil {
			t.Fatal(err)
		}
		_ = g

		resp := f.need(alice, "")
		wantTitles(t, "alice", resp.Rows, "w2 early", "guest item", "w1 mid", "w1 late")
		if resp.Total != 4 || resp.Hidden != 0 || len(resp.Degraded) != 0 {
			t.Fatalf("total %d hidden %d degraded %v", resp.Total, resp.Hidden, resp.Degraded)
		}
		counts := map[string]int{}
		for _, c := range resp.Workspaces {
			counts[c.Slug] = c.Count
			if c.OwnerUsername != "owner-nx" || c.Name == "" {
				t.Fatalf("workspace label %+v", c)
			}
		}
		if counts[w1.Slug] != 2 || counts[w2.Slug] != 1 || counts[w3.Slug] != 1 || len(counts) != 3 {
			t.Fatalf("workspace counts %v", counts)
		}
		for _, r := range resp.Rows {
			if r.Why.Rule != needs.RuleAsked || r.Workspace.Slug == "" || !strings.HasPrefix(r.Item.Path, "/owner-nx/") || r.Marker == nil {
				t.Fatalf("row %+v", r)
			}
		}
		if got := f.need(alice, "?workspace="+w1.Slug); got.Total != 2 || len(got.Workspaces) != 1 {
			t.Fatalf("?workspace filter: %+v", got)
		}
		// Not for the owner: nothing is assigned to or mentions them, and an
		// ask is routed to its assignee, never "everyone".
		if got := f.need(owner, ""); got.Total != 0 {
			t.Fatalf("owner saw alice's asks: %v", titles(got.Rows))
		}
	})
}

func TestNeedsMe_RestrictedMemberNoLeak(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newNfx(t, srv)
		owner, alice := f.user("owner"), f.user("alice")
		ws := f.workspace("One", owner)
		f.member(ws, alice, "editor")
		open, hidden := f.collection(ws, "Open asks", true), f.collection(ws, "Hidden asks", true)
		plain := f.collection(ws, "Hidden tasks", false)
		if err := srv.store.SetMemberCollectionAccess(ws.ID, alice.ID, "specific", []string{open.ID}); err != nil {
			t.Fatal(err)
		}
		f.item(ws, open, "visible ask", alice, nfxDay(1))
		f.item(ws, hidden, "hidden ask", alice, nfxDay(2))
		secret := f.item(ws, plain, "hidden mention", nil, nfxDay(3))
		f.comment(ws, secret, owner, "@alice-nx please look")
		f.item(ws, hidden, "hidden ask 2", alice, nfxDay(4))

		resp := f.need(alice, "")
		wantTitles(t, "restricted page 1", resp.Rows, "visible ask")
		if resp.Total != 1 || resp.Hidden != 0 || len(resp.Workspaces) != 1 || resp.Workspaces[0].Count != 1 {
			t.Fatalf("counts leak hidden items: %+v", resp)
		}
		// The owner sees them as the asks' assignee? No: alice is the assignee.
		// A cursor minted for the owner's view is just a position for alice.
		ownerPage := f.need(owner, "?limit=1")
		_ = ownerPage
		cur := needs.NewCursor(needs.SortAge, needsViewActive, "", time.Now(), &needs.Row{ItemID: "x", WaitingSince: nfxDay(0), Item: needs.ItemInfo{CreatedAt: nfxDay(0)}}).Encode()
		page := f.need(alice, "?cursor="+cur)
		wantTitles(t, "restricted cursor page", page.Rows, "visible ask")
		if page.Total != 1 {
			t.Fatalf("cursor page total %d", page.Total)
		}
		// The hidden collection's snooze view is empty too.
		if h := f.need(alice, "?view=hidden"); h.Total != 0 || h.Hidden != 0 {
			t.Fatalf("hidden view: %+v", h)
		}
	})
}

func TestNeedsMe_GuestItemGrantOnly(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newNfx(t, srv)
		owner, guest := f.user("owner"), f.user("guest")
		ws := f.workspace("One", owner)
		asks := f.collection(ws, "Asks", true)
		granted := f.item(ws, asks, "granted", nil, nfxDay(1))
		notGranted := f.item(ws, asks, "not granted", nil, nfxDay(2))
		f.forceAssign(granted, guest)
		f.forceAssign(notGranted, guest)
		if _, err := srv.store.CreateItemGrant(ws.ID, granted.ID, guest.ID, "view", owner.ID); err != nil {
			t.Fatal(err)
		}
		wantTitles(t, "guest", f.need(guest, "").Rows, "granted")
	})
}

func TestNeedsMe_UnknownVsHiddenWorkspaceIdentical(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newNfx(t, srv)
		owner, alice, stranger := f.user("owner"), f.user("alice"), f.user("stranger")
		ws := f.workspace("Private", owner)
		f.member(ws, alice, "editor")
		f.item(ws, f.collection(ws, "Asks", true), "secret ask", alice, nfxDay(1))
		other := f.workspace("Strangers", stranger)

		unknown := f.get(stranger, "?workspace=does-not-exist").Body.Bytes()
		hiddenWS := f.get(stranger, "?workspace="+ws.Slug).Body.Bytes()
		ownEmpty := f.get(stranger, "?workspace="+other.Slug).Body.Bytes()
		if !bytes.Equal(unknown, hiddenWS) {
			t.Fatalf("unknown %s\nhidden  %s", unknown, hiddenWS)
		}
		if !bytes.Equal(unknown, ownEmpty) {
			t.Fatalf("a known empty workspace differs from unknown: %s vs %s", ownEmpty, unknown)
		}
		if strings.Contains(string(hiddenWS), "secret") || strings.Contains(string(hiddenWS), ws.Slug) {
			t.Fatalf("hidden workspace leaked: %s", hiddenWS)
		}
	})
}

func TestNeedsMe_TokenAllowlist(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newNfx(t, srv)
		owner, alice := f.user("owner"), f.user("alice")
		w1, w2 := f.workspace("One", owner), f.workspace("Two", owner)
		for _, w := range []*models.Workspace{w1, w2} {
			f.member(w, alice, "editor")
			f.item(w, f.collection(w, "Asks", true), "ask in "+w.Name, alice, nfxDay(1))
		}
		allow := func(slugs ...string) func(context.Context) context.Context {
			slugs = append([]string{}, slugs...) // empty, not nil: nil means no gate
			return func(c context.Context) context.Context { return WithTokenAllowedWorkspaces(c, slugs) }
		}
		wantTitles(t, "fan-out", f.need(alice, "", allow(w1.Slug)).Rows, "ask in One")
		wantTitles(t, "?workspace= allowed", f.need(alice, "?workspace="+w1.Slug, allow(w1.Slug)).Rows, "ask in One")
		excluded := f.get(alice, "?workspace="+w2.Slug, allow(w1.Slug)).Body.Bytes()
		unknown := f.get(alice, "?workspace=nope", allow(w1.Slug)).Body.Bytes()
		if !bytes.Equal(excluded, unknown) {
			t.Fatalf("an allow-list-excluded workspace differs from unknown: %s vs %s", excluded, unknown)
		}
		if got := f.need(alice, "", allow()); got.Total != 0 {
			t.Fatalf("an empty allow-list admitted %v", titles(got.Rows))
		}
	})
}

// A user-bound token scoped to one workspace stays in it (lead ruling Q5,
// stricter than search; BUG-3982).
func TestNeedsMe_WorkspaceBoundTokenStaysInIt(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newNfx(t, srv)
		owner, alice := f.user("owner"), f.user("alice")
		w1, w2 := f.workspace("One", owner), f.workspace("Two", owner)
		for i, w := range []*models.Workspace{w1, w2} {
			f.member(w, alice, "editor")
			f.item(w, f.collection(w, "Asks", true), "ask in "+w.Name, alice, nfxDay(1+i))
		}
		bound := func(c context.Context) context.Context { return WithTokenWorkspaceID(c, w1.ID) }
		wantTitles(t, "unscoped session", f.need(alice, "").Rows, "ask in One", "ask in Two")
		wantTitles(t, "bound fan-out", f.need(alice, "", bound).Rows, "ask in One")
		other := f.get(alice, "?workspace="+w2.Slug, bound).Body.Bytes()
		unknown := f.get(alice, "?workspace=nope", bound).Body.Bytes()
		if !bytes.Equal(other, unknown) {
			t.Fatalf("a workspace outside the token binding differs from unknown: %s vs %s", other, unknown)
		}
	})
}

func TestNeedsMe_BearerAdminNotUnrestricted(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newNfx(t, srv)
		admin, alice, owner := f.user("boss"), f.user("alice"), f.user("owner")
		if _, err := srv.store.DB().Exec(fmt.Sprintf("UPDATE users SET role = 'admin' WHERE id = '%s'", admin.ID)); err != nil {
			t.Fatal(err)
		}
		admin.Role = "admin"
		mine, foreign := f.workspace("Mine", owner), f.workspace("Foreign", owner)
		f.member(mine, admin, "editor")
		f.member(mine, alice, "editor")
		f.member(foreign, alice, "editor")
		f.item(mine, f.collection(mine, "Asks", true), "mine", admin, nfxDay(1))
		f.item(foreign, f.collection(foreign, "Asks", true), "foreign", alice, nfxDay(1))
		fa := f.item(foreign, f.collection(foreign, "Other asks", true), "foreign admin ask", nil, nfxDay(1))
		f.forceAssign(fa, admin)

		tok, err := srv.store.CreateAPIToken(admin.ID, models.APITokenCreate{Name: "pat"}, 30, 365)
		if err != nil {
			t.Fatal(err)
		}
		var body []byte
		for _, q := range []string{"", "?workspace=" + foreign.Slug, "?workspace=nope"} {
			rr := doRequestWithBearer(srv, "GET", "/api/v1/me/needs"+q, tok.Token, nil)
			if rr.Code != http.StatusOK {
				t.Fatalf("bearer GET %q: %d %s", q, rr.Code, rr.Body.String())
			}
			if q == "" {
				var resp needsResponse
				parseJSON(t, rr, &resp)
				wantTitles(t, "bearer admin", resp.Rows, "mine")
			} else if body == nil {
				body = rr.Body.Bytes()
			} else if !bytes.Equal(body, rr.Body.Bytes()) {
				t.Fatalf("a foreign workspace differs from unknown for a bearer admin: %s vs %s", body, rr.Body.Bytes())
			}
		}
		// The same admin on the cookie door: still membership only.
		wantTitles(t, "cookie admin", f.need(admin, "").Rows, "mine")
	})
}

func TestNeedsMe_PaginationTotalOrder(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newNfx(t, srv)
		owner, alice := f.user("owner"), f.user("alice")
		ws := f.workspace("One", owner)
		f.member(ws, alice, "editor")
		asks := f.collection(ws, "Asks", true)
		var want []string
		type k struct {
			at time.Time
			id string
		}
		var keys []k
		for i := 0; i < 7; i++ {
			// Three items tie on created_at exactly, two more on another second.
			day := []int{1, 1, 1, 2, 2, 3, 4}[i]
			it := f.item(ws, asks, fmt.Sprintf("item %d", i), alice, nfxDay(day))
			keys = append(keys, k{nfxDay(day), it.ID})
		}
		sort.Slice(keys, func(a, b int) bool {
			if !keys[a].at.Equal(keys[b].at) {
				return keys[a].at.Before(keys[b].at)
			}
			return keys[a].id < keys[b].id
		})
		for _, x := range keys {
			want = append(want, x.id)
		}
		var got []string
		cursor := ""
		pages := 0
		for {
			q := "?limit=3"
			if cursor != "" {
				q += "&cursor=" + cursor
			}
			resp := f.need(alice, q)
			pages++
			if resp.Total != 7 && cursor == "" {
				t.Fatalf("total %d", resp.Total)
			}
			for _, r := range resp.Rows {
				// ItemID is internal; recover it from the title's creation order.
				got = append(got, itemIDByTitle(t, srv, ws, r.Item.Title))
			}
			if resp.NextCursor == "" {
				if len(resp.Rows) == 0 {
					t.Fatal("an empty last page")
				}
				break
			}
			cursor = resp.NextCursor
			if pages > 5 {
				t.Fatal("pagination does not end")
			}
		}
		if strings.Join(got, ",") != strings.Join(want, ",") || pages != 3 {
			t.Fatalf("pages %d, order\n got %v\nwant %v", pages, got, want)
		}
		for _, bad := range []string{"?cursor=garbage", "?cursor=" + cursor + "&view=hidden", "?limit=0", "?limit=x", "?sort=bogus", "?view=other"} {
			rr := f.get(alice, bad)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("%s: %d, want 400", bad, rr.Code)
			}
		}
		if rr := f.get(alice, "?cursor=garbage"); !strings.Contains(rr.Body.String(), "invalid_cursor") {
			t.Fatalf("bad cursor body: %s", rr.Body.String())
		}
	})
}

func itemIDByTitle(t *testing.T, srv *Server, ws *models.Workspace, title string) string {
	t.Helper()
	var id string
	row := srv.store.DB().QueryRow(fmt.Sprintf("SELECT id FROM items WHERE workspace_id = '%s' AND title = '%s'", ws.ID, title))
	if err := row.Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestNeedsMe_DismissOnlyForCaller(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newNfx(t, srv)
		owner, alice, bob := f.user("owner"), f.user("alice"), f.user("bob")
		ws := f.workspace("One", owner)
		f.member(ws, alice, "editor")
		f.member(ws, bob, "editor")
		tasks := f.collection(ws, "Tasks", false)
		it := f.item(ws, tasks, "both asked", nil, nfxDay(1))
		f.comment(ws, it, owner, "@alice-nx and @bob-nx please decide")
		if _, err := srv.store.SnoozeNeedsItem(alice.ID, ws.ID, it.ID, models.NeedsSnoozeDismiss, time.Time{}, nil); err != nil {
			t.Fatal(err)
		}
		a := f.need(alice, "")
		if a.Total != 0 || a.Hidden != 1 {
			t.Fatalf("alice active: total %d hidden %d", a.Total, a.Hidden)
		}
		h := f.need(alice, "?view=hidden")
		wantTitles(t, "alice hidden view", h.Rows, "both asked")
		if h.Rows[0].Snooze == nil || h.Rows[0].Snooze.Kind != models.NeedsSnoozeDismiss || !h.Rows[0].Snooze.Holds {
			t.Fatalf("hidden row has no holding snooze: %+v", h.Rows[0].Snooze)
		}
		b := f.need(bob, "")
		wantTitles(t, "bob active", b.Rows, "both asked")
		if b.Hidden != 0 || b.Rows[0].Why.Rule != needs.RuleMentioned {
			t.Fatalf("bob: hidden %d row %+v", b.Hidden, b.Rows[0].Why)
		}
		// A new comment by someone else brings it back for alice.
		f.comment(ws, it, owner, "any news?")
		time.Sleep(1100 * time.Millisecond)
		f.comment(ws, it, owner, "ping")
		wantTitles(t, "alice after a new comment", f.need(alice, "").Rows, "both asked")
	})
}

type recordingScorer struct {
	mu     sync.Mutex
	calls  []needs.Scope
	failIn map[string]bool
}

func (s *recordingScorer) Name() string { return "importance" }
func (s *recordingScorer) Score(_ context.Context, v needs.Viewer, sc needs.Scope, rows []*needs.Row) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, sc)
	for _, r := range rows {
		r.Importance = &needs.Score{Value: 50}
	}
	return s.failIn[sc.WorkspaceID], nil
}

func TestNeedsMe_DegradedPerWorkspace(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newNfx(t, srv)
		owner, alice := f.user("owner"), f.user("alice")
		w1, w2 := f.workspace("One", owner), f.workspace("Two", owner)
		f.member(w1, alice, "editor")
		f.member(w2, alice, "editor")
		f.item(w1, f.collection(w1, "Asks", true), "in one", alice, nfxDay(1))
		a2 := f.collection(w2, "Asks", true)
		f.item(w2, a2, "in two", alice, nfxDay(2))
		sc := &recordingScorer{failIn: map[string]bool{w1.ID: true}}
		srv.needsScorers = []needs.Scorer{sc}

		resp := f.need(alice, "")
		wantTitles(t, "rows from both", resp.Rows, "in one", "in two")
		if len(resp.Degraded) != 1 || resp.Degraded[0] != w1.Slug {
			t.Fatalf("degraded = %v, want [%s]", resp.Degraded, w1.Slug)
		}
		if resp.Rows[0].Importance == nil || resp.Rows[0].Importance.Value != 50 {
			t.Fatalf("score not set: %+v", resp.Rows[0].Importance)
		}
		if len(sc.calls) != 2 {
			t.Fatalf("scorer called %d times, want once per workspace", len(sc.calls))
		}
		for _, c := range sc.calls {
			if !c.Unrestricted || c.CollectionIDs == nil || c.ItemIDs == nil {
				t.Fatalf("scope %+v: lists must arrive as [] not nil", c)
			}
		}

		// A guest with only an item grant: restricted scope, nil collections as [].
		guest := f.user("guest")
		it := f.item(w2, a2, "granted", nil, nfxDay(3))
		f.forceAssign(it, guest)
		if _, err := srv.store.CreateItemGrant(w2.ID, it.ID, guest.ID, "view", owner.ID); err != nil {
			t.Fatal(err)
		}
		sc.calls = nil
		f.need(guest, "")
		if len(sc.calls) != 1 || sc.calls[0].Unrestricted || sc.calls[0].CollectionIDs == nil || len(sc.calls[0].CollectionIDs) != 0 || len(sc.calls[0].ItemIDs) != 1 {
			t.Fatalf("guest scope %+v", sc.calls)
		}
	})
}

func TestNeedsMe_DegradedOnlyOnProviderError(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newNfx(t, srv)
		owner, alice := f.user("owner"), f.user("alice")
		ws := f.workspace("One", owner)
		f.member(ws, alice, "editor")
		open, hidden := f.collection(ws, "Open asks", true), f.collection(ws, "Hidden tasks", false)
		if err := srv.store.SetMemberCollectionAccess(ws.ID, alice.ID, "specific", []string{open.ID}); err != nil {
			t.Fatal(err)
		}
		f.item(ws, open, "visible ask", alice, nfxDay(1))

		// A provider that FAILS: an item in a collection alice cannot see
		// gets a failing job, so the workspace-wide failing bit is set.
		p := attachAttentionRunner(t, srv)
		p.fail = errors.New("upstream down")
		f.item(ws, hidden, "hidden failing item", nil, nfxDay(2))
		runDecisionTick(t, srv)
		if failing, err := srv.store.DecisionJobsFailing(ws.ID, decision.AttentionSetName); err != nil || !failing {
			t.Fatalf("the fixture must have a failing job: %v %v", failing, err)
		}
		resp := f.need(alice, "")
		wantTitles(t, "rows", resp.Rows, "visible ask")
		if len(resp.Degraded) != 0 {
			t.Fatalf("a failing item alice cannot see set degraded: %v", resp.Degraded)
		}

		// A provider ERROR (the set is not registered) does degrade, by slug.
		srv.SetDecisionRunner(decision.NewRunner(srv.store, p, decision.NewRegistry()))
		resp = f.need(alice, "")
		if len(resp.Degraded) != 1 || resp.Degraded[0] != ws.Slug {
			t.Fatalf("degraded = %v, want [%s]", resp.Degraded, ws.Slug)
		}
		wantTitles(t, "rows still return", resp.Rows, "visible ask")
	})
}

// Codex r1 P2: the attention read evaluates only items the caller can see, so
// an item whose state cannot be built, hidden from the caller, sets no bit on
// their response; the same failure on a visible item does degrade.
func TestNeedsMe_DegradedNotLeakedByHiddenItemFailure(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newNfx(t, srv)
		owner, alice := f.user("owner"), f.user("alice")
		ws := f.workspace("One", owner)
		f.member(ws, alice, "editor")
		open, hidden := f.collection(ws, "Open asks", true), f.collection(ws, "Hidden tasks", false)
		if err := srv.store.SetMemberCollectionAccess(ws.ID, alice.ID, "specific", []string{open.ID}); err != nil {
			t.Fatal(err)
		}
		f.item(ws, open, "visible ask", alice, nfxDay(1))
		attachAttentionRunner(t, srv) // answers HUMAN-HIGH titles above the threshold
		hid := f.item(ws, hidden, "HUMAN-HIGH hidden", nil, nfxDay(2))
		vis := f.item(ws, open, "HUMAN-HIGH visible", nil, nfxDay(3))
		runDecisionTick(t, srv)

		corrupt := func(id string) {
			t.Helper()
			if _, err := srv.store.DB().Exec(fmt.Sprintf(`UPDATE items SET fields = '[1]' WHERE id = '%s'`, id)); err != nil {
				t.Fatal(err)
			}
		}
		// The hidden item's state can no longer be built: alice learns nothing.
		corrupt(hid.ID)
		resp := f.need(alice, "")
		if len(resp.Degraded) != 0 {
			t.Fatalf("a failure on an item alice cannot see set degraded: %v", resp.Degraded)
		}
		// The same failure on an item alice can see degrades the workspace.
		corrupt(vis.ID)
		resp = f.need(alice, "")
		if len(resp.Degraded) != 1 || resp.Degraded[0] != ws.Slug {
			t.Fatalf("degraded = %v, want [%s]", resp.Degraded, ws.Slug)
		}
	})
}

func TestNeedsMe_RoutingRulesAndWaitingSince(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newNfx(t, srv)
		owner, alice, bob := f.user("owner"), f.user("alice"), f.user("bob")
		f.user("outsider") // a real user, but not a member of the workspace
		ws := f.workspace("One", owner)
		f.member(ws, alice, "editor")
		f.member(ws, bob, "editor")
		tasks := f.collection(ws, "Tasks", false)

		// assigned: a mention of someone who cannot see the item routes nothing
		// to them, so it falls to the assignee. WaitingSince is the latest
		// assignment, not the item's creation.
		assigned := f.item(ws, tasks, "assigned old", alice, nfxDay(0))
		f.comment(ws, assigned, bob, "@outsider-nx can you look?")
		if _, err := srv.store.DB().Exec(fmt.Sprintf(`INSERT INTO activities (id, workspace_id, document_id, action, actor, source, metadata, created_at)
			VALUES ('act-1', '%s', '%s', 'updated', 'user', 'web', '{"changes":"assigned: Bob Person → Alice Person"}', '%s')`,
			ws.ID, assigned.ID, nfxDay(40).Format(time.RFC3339))); err != nil {
			t.Fatal(err)
		}
		// Same, no assignment row: falls back to created_at.
		noRow := f.item(ws, tasks, "assigned no row", alice, nfxDay(2))
		f.comment(ws, noRow, bob, "@outsider-nx hello")

		resp := f.need(alice, "")
		wantTitles(t, "alice", resp.Rows, "assigned no row", "assigned old")
		for _, r := range resp.Rows {
			if r.Why.Rule != needs.RuleAssigned || strings.Contains(r.Why.Text, "utsider") {
				t.Fatalf("why = %+v", r.Why)
			}
		}
		if !resp.Rows[0].WaitingSince.Equal(nfxDay(2)) || !resp.Rows[1].WaitingSince.Equal(nfxDay(40)) {
			t.Fatalf("WaitingSince: %v, %v", resp.Rows[0].WaitingSince, resp.Rows[1].WaitingSince)
		}

		// admin_fallback: nobody eligible is named, so only an owner is routed.
		orphan := f.item(ws, tasks, "orphan", nil, nfxDay(3))
		f.comment(ws, orphan, bob, "@outsider-nx ?")
		wantTitles(t, "owner", f.need(owner, "").Rows, "orphan")
		if got := f.need(bob, ""); got.Total != 0 {
			t.Fatalf("an editor was routed the owners' fallback: %v", titles(got.Rows))
		}
		if r := f.need(owner, "").Rows[0]; r.Why.Rule != needs.RuleAdminFallback {
			t.Fatalf("owner why %+v", r.Why)
		}

		// mentioned: a reference-collection mention counts too (lead ruling Q2).
		docs, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Notes", Schema: `{"fields":[]}`})
		if err != nil {
			t.Fatal(err)
		}
		note := f.item(ws, docs, "a note", nil, nfxDay(4))
		f.comment(ws, note, owner, "@bob-nx thoughts?")
		wantTitles(t, "bob", f.need(bob, "").Rows, "a note")
		// A reply closes it.
		f.comment(ws, note, bob, "done")
		if got := f.need(bob, ""); got.Total != 0 {
			t.Fatalf("a replied mention is still open: %v", titles(got.Rows))
		}
	})
}

func TestNeedsMe_NeedsHumanClaim(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newNfx(t, srv)
		owner := f.user("owner")
		ws := f.workspace("One", owner)
		tasks, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks", Schema: nfxSchema})
		if err != nil {
			t.Fatal(err)
		}
		attachAttentionRunner(t, srv)
		it := f.item(ws, tasks, "Pick vendor HUMAN-HIGH", nil, nfxDay(0))
		f.item(ws, tasks, "Pick vendor HUMAN-LOW", nil, nfxDay(0))
		f.setCreated(it, time.Now().UTC().Add(-48*time.Hour))
		runDecisionTick(t, srv)

		resp := f.need(owner, "")
		wantTitles(t, "owner", resp.Rows, "Pick vendor HUMAN-HIGH")
		r := resp.Rows[0]
		if r.NeedsHuman == nil || len(r.Because) != 1 || r.Because[0].Kind != needs.BecauseNeedsHuman || r.Why.Rule != needs.RuleAdminFallback {
			t.Fatalf("row %+v because %+v", r, r.Because)
		}
		if !r.WaitingSince.Equal(r.Item.CreatedAt) {
			t.Fatalf("WaitingSince %v, want the item's creation %v (the earlier claim)", r.WaitingSince, r.Item.CreatedAt)
		}
	})
}

// Eligibility loads the user first: asking the visibility resolver about a
// user that does not exist answers (nil, nil, nil), which is also its
// "unrestricted" sentinel. A missing user must get nothing.
func TestNeedsEligibility_MissingUserGetsNothing(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newNfx(t, srv)
		owner := f.user("owner")
		ws := f.workspace("One", owner)
		asks := f.collection(ws, "Asks", true)
		it := f.item(ws, asks, "x", owner, nfxDay(1))
		disabled := f.user("gone")
		f.member(ws, disabled, "owner")
		if _, err := srv.store.DB().Exec(fmt.Sprintf("UPDATE users SET disabled_at = '2024-01-01T00:00:00Z' WHERE id = '%s'", disabled.ID)); err != nil {
			t.Fatal(err)
		}
		e := &needsEligibility{s: srv, wsID: ws.ID, cache: map[string]*needsVisibility{}}
		if !e.Eligible(owner.ID, asks.ID, it.ID) {
			t.Fatal("a live owner is eligible")
		}
		if e.Eligible("no-such-user", asks.ID, it.ID) {
			t.Fatal("a user that does not exist was treated as unrestricted")
		}
		if e.Eligible(disabled.ID, asks.ID, it.ID) {
			t.Fatal("a disabled user is eligible")
		}
		// The resolver's own sentinel, for the record: nil, nil for a ghost.
		full, granted, err := srv.store.ResolveBacklinksVisibility("no-such-user", ws.ID, false, true)
		if err != nil || full != nil || granted != nil {
			t.Fatalf("resolver for a ghost: %v %v %v (the guard in needsEligibility exists because of this)", full, granted, err)
		}
	})
}

func TestNeedsMe_SnoozeMovedItem(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newNfx(t, srv)
		owner, alice := f.user("owner"), f.user("alice")
		w1, w2 := f.workspace("One", owner), f.workspace("Two", owner)
		f.member(w1, alice, "editor")
		f.member(w2, alice, "editor")
		a1, a2 := f.collection(w1, "Asks", true), f.collection(w2, "Asks", true)
		src := f.item(w1, a1, "moving", alice, nfxDay(1))
		if _, err := srv.store.SnoozeNeedsItem(alice.ID, w1.ID, src.ID, models.NeedsSnoozeDefer, time.Now().Add(72*time.Hour), nil); err != nil {
			t.Fatal(err)
		}
		if got := f.need(alice, ""); got.Total != 0 || got.Hidden != 1 {
			t.Fatalf("before the move: total %d hidden %d", got.Total, got.Hidden)
		}
		// Move it: copy to workspace two and archive the source. The snooze
		// stays with the archived source.
		res, err := srv.store.CopyItemAcrossWorkspaces(store.CrossWorkspaceCopyRequest{
			SourceItemID: src.ID, TargetWorkspaceID: w2.ID, TargetCollectionID: a2.ID,
			ArchiveSource: true, Actor: alice.ID, ActorUserID: alice.ID,
		})
		if err != nil {
			t.Fatalf("copy: %v", err)
		}
		if res == nil {
			t.Fatal("no copy result")
		}
		resp := f.need(alice, "")
		if resp.Hidden != 0 {
			t.Fatalf("the archived source's snooze was counted: hidden %d", resp.Hidden)
		}
		wantTitles(t, "the clone is not hidden by the source's snooze", resp.Rows, "moving")
		if resp.Rows[0].Workspace.Slug != w2.Slug || resp.Rows[0].Snooze != nil {
			t.Fatalf("clone row %+v", resp.Rows[0])
		}
	})
}

func TestNeedsMe_SnoozeLostAccess(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newNfx(t, srv)
		owner, alice, carol := f.user("owner"), f.user("alice"), f.user("carol")
		ws := f.workspace("One", owner)
		keep, asks := f.collection(ws, "Keep", false), f.collection(ws, "Asks", true)
		for _, u := range []*models.User{alice, carol} {
			f.member(ws, u, "editor")
			if err := srv.store.SetMemberCollectionAccess(ws.ID, u.ID, "specific", []string{keep.ID, asks.ID}); err != nil {
				t.Fatal(err)
			}
		}
		it := f.item(ws, asks, "ask", alice, nfxDay(1))
		if _, err := srv.store.SnoozeNeedsItem(alice.ID, ws.ID, it.ID, models.NeedsSnoozeDefer, time.Now().Add(72*time.Hour), nil); err != nil {
			t.Fatal(err)
		}
		if got := f.need(alice, ""); got.Hidden != 1 {
			t.Fatalf("before losing access: hidden %d", got.Hidden)
		}
		// alice loses the collection; the snooze row stays.
		if err := srv.store.SetMemberCollectionAccess(ws.ID, alice.ID, "specific", []string{keep.ID}); err != nil {
			t.Fatal(err)
		}
		after := f.get(alice, "").Body.Bytes()
		afterHidden := f.get(alice, "?view=hidden").Body.Bytes()
		never := f.get(carol, "").Body.Bytes() // same access, no snooze, nothing assigned
		neverHidden := f.get(carol, "?view=hidden").Body.Bytes()
		if !bytes.Equal(after, never) || !bytes.Equal(afterHidden, neverHidden) {
			t.Fatalf("a snooze on an item the caller lost access to shows:\n%s\nvs\n%s", after, never)
		}
		var resp needsResponse
		if err := json.Unmarshal(after, &resp); err != nil || resp.Hidden != 0 || resp.Total != 0 {
			t.Fatalf("hidden/total count it: %+v %v", resp, err)
		}
	})
}

// Hunter r1 F1 end to end: with two open mentions on one item, each person's
// row carries only their own mention.
func TestNeedsMe_MentionedRowIsPerCaller(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newNfx(t, srv)
		owner, alice, bob, carol := f.user("owner"), f.user("alice"), f.user("bob"), f.user("carol")
		ws := f.workspace("One", owner)
		f.member(ws, alice, "editor")
		f.member(ws, bob, "editor")
		f.member(ws, carol, "editor")
		tasks := f.collection(ws, "Tasks", false)
		it := f.item(ws, tasks, "shared", nil, nfxDay(0))
		at := func(c *models.Comment, day int) string {
			if _, err := srv.store.DB().Exec(fmt.Sprintf(`UPDATE comment_mentions SET created_at = '%s' WHERE comment_id = '%s'`,
				nfxDay(day).UTC().Format("2006-01-02T15:04:05Z"), c.ID)); err != nil {
				t.Fatal(err)
			}
			return c.ID
		}
		bc := at(f.comment(ws, it, carol, "@bob-nx first"), 1)
		ac := at(f.comment(ws, it, owner, "@alice-nx later"), 5)
		resp := f.need(alice, "")
		wantTitles(t, "alice", resp.Rows, "shared")
		r := resp.Rows[0]
		if !r.WaitingSince.Equal(nfxDay(5)) || r.Why.CommentID != ac || r.Why.By == "" || len(r.Because) != 1 || r.Because[0].CommentID != ac {
			t.Fatalf("alice's row = %+v because %+v", r.Why, r.Because)
		}
		// The marker is the item's own comment state (every other-author comment id),
		// not a routing claim; the routing fields must carry no trace of bob's.
		raw, _ := json.Marshal([]any{r.Why, r.Because, r.WaitingSince})
		if strings.Contains(string(raw), bc) || strings.Contains(string(raw), "arol") {
			t.Fatalf("alice's row leaks bob's mention: %s", raw)
		}
		b := f.need(bob, "").Rows[0]
		if !b.WaitingSince.Equal(nfxDay(1)) || b.Why.CommentID != bc {
			t.Fatalf("bob's row = %+v", b.Why)
		}
	})
}

// A needs_human row that did not surface (below threshold) must not touch a
// candidate that reached the list another way, even when the workspace has a
// truly flagged item (which is what makes the decision rows get read at all).
func TestNeedsMe_UnsurfacedNeedsHumanRowIgnored(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newNfx(t, srv)
		owner, alice := f.user("owner"), f.user("alice")
		ws := f.workspace("One", owner)
		f.member(ws, alice, "editor")
		asks := f.collection(ws, "Asks", true)
		attachAttentionRunner(t, srv)
		f.item(ws, asks, "flagged HUMAN-HIGH", nil, nfxDay(0))
		mine := f.item(ws, asks, "mine HUMAN-LOW", alice, nfxDay(1))
		runDecisionTick(t, srv)

		resp := f.need(alice, "")
		wantTitles(t, "alice", resp.Rows, "mine HUMAN-LOW")
		r := resp.Rows[0]
		if r.NeedsHuman != nil {
			t.Fatalf("an unsurfaced needs_human row set NeedsHuman: %+v", r.NeedsHuman)
		}
		for _, b := range r.Because {
			if b.Kind == needs.BecauseNeedsHuman {
				t.Fatalf("because carries needs_human: %+v", r.Because)
			}
		}
		if !r.WaitingSince.Equal(mine.CreatedAt) && !r.WaitingSince.Equal(nfxDay(1)) {
			t.Fatalf("WaitingSince %v, want its own claim time", r.WaitingSince)
		}
	})
}

// A forged as_of in the future is clamped to now: a defer still in force is
// still hiding its item.
func TestNeedsMe_ForgedFutureAsOfClamped(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newNfx(t, srv)
		owner, alice := f.user("owner"), f.user("alice")
		ws := f.workspace("One", owner)
		f.member(ws, alice, "editor")
		asks := f.collection(ws, "Asks", true)
		it := f.item(ws, asks, "deferred", alice, nfxDay(1))
		f.item(ws, asks, "visible", alice, nfxDay(2))
		if _, err := srv.store.SnoozeNeedsItem(alice.ID, ws.ID, it.ID, models.NeedsSnoozeDefer, time.Now().Add(72*time.Hour), nil); err != nil {
			t.Fatal(err)
		}
		row := &needs.Row{ItemID: "x", WaitingSince: nfxDay(0), Item: needs.ItemInfo{CreatedAt: nfxDay(0)}}
		tok := needs.NewCursor(needs.SortAge, "active", "", time.Now().Add(10*365*24*time.Hour), row).Encode()
		resp := f.need(alice, "?cursor="+tok)
		if resp.Hidden != 1 {
			t.Fatalf("forged as_of un-hid a live defer: hidden %d rows %v", resp.Hidden, titles(resp.Rows))
		}
	})
}
