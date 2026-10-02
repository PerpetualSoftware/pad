package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/watchevents"
)

// TASK-3272: one test per workspace_access_changed publisher. Each drives the
// real HTTP door (or, for the purge, the real sweep) and asserts the EXACT set
// of notifications the bus accepted for that door: which users, which change,
// which workspace, and nobody else. Delivery from the bus to a stream (the
// opt-in, the per-user addressing, replay) is pinned separately below, through
// the real stream handler.

// accessFixture is one server on one dialect with an owner who holds a
// workspace, plus helpers to add users.
type accessFixture struct {
	t        *testing.T
	srv      *Server
	owner    *models.User
	ownerTok string
	wsID     string
	wsSlug   string
}

func newAccessFixture(t *testing.T, driver store.DriverType) *accessFixture {
	t.Helper()
	srv := attachmentsServerOn(t, driver)
	srv.SetWatchEventsBus(watchevents.New())
	f := &accessFixture{t: t, srv: srv}
	f.owner = mkUser(t, srv, "owner@example.com")
	f.ownerTok = f.token(f.owner)
	rr := bearerJSON(t, srv, "POST", "/api/v1/workspaces", f.ownerTok,
		map[string]any{"name": "Access WS", "template": "startup"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create workspace: %d %s", rr.Code, rr.Body.String())
	}
	var ws models.Workspace
	parseJSON(t, rr, &ws)
	f.wsID, f.wsSlug = ws.ID, ws.Slug
	return f
}

func (f *accessFixture) token(u *models.User) string {
	f.t.Helper()
	tok, err := f.srv.store.CreateAPIToken(u.ID, models.APITokenCreate{Name: "access-test"}, 0, 0)
	if err != nil {
		f.t.Fatalf("CreateAPIToken: %v", err)
	}
	return tok.Token
}

func (f *accessFixture) member(email, role string) *models.User {
	f.t.Helper()
	u := mkUser(f.t, f.srv, email)
	if err := f.srv.store.AddWorkspaceMember(f.wsID, u.ID, role); err != nil {
		f.t.Fatalf("AddWorkspaceMember: %v", err)
	}
	return u
}

// mark returns the bus's newest id, so a later accessSince reads only what
// the door under test published.
func (f *accessFixture) mark() int64 {
	evs := f.srv.watchEvents.EventsSince(0)
	if len(evs) == 0 {
		return 0
	}
	return evs[len(evs)-1].ID
}

// accessSince renders every workspace_access_changed notification after id as
// sorted "change user-email workspace-id" lines, so an assertion names people.
func (f *accessFixture) accessSince(id int64) []string {
	f.t.Helper()
	var out []string
	for _, n := range f.srv.watchEvents.EventsSince(id) {
		if n.Kind != watchevents.KindWorkspaceAccessChanged {
			continue
		}
		who := n.TargetUserID
		if u, err := f.srv.store.GetUser(n.TargetUserID); err == nil && u != nil {
			who = u.Email
		}
		out = append(out, fmt.Sprintf("%s %s %s", n.AccessChange, who, n.WorkspaceID))
	}
	sort.Strings(out)
	return out
}

func (f *accessFixture) expect(since int64, want ...string) {
	f.t.Helper()
	sort.Strings(want)
	got := f.accessSince(since)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		f.t.Fatalf("workspace_access_changed notifications:\n got: %q\nwant: %q", got, want)
	}
}

func (f *accessFixture) line(change, email string) string {
	return fmt.Sprintf("%s %s %s", change, email, f.wsID)
}

func (f *accessFixture) do(method, path, tok string, body any) *httptest.ResponseRecorder {
	f.t.Helper()
	return bearerJSON(f.t, f.srv, method, path, tok, body)
}

func (f *accessFixture) must(rr *httptest.ResponseRecorder, code int, what string) {
	f.t.Helper()
	if rr.Code != code {
		f.t.Fatalf("%s: got %d, want %d: %s", what, rr.Code, code, rr.Body.String())
	}
}

func (f *accessFixture) seedItem() models.Item {
	f.t.Helper()
	rr := f.do("POST", "/api/v1/workspaces/"+f.wsSlug+"/collections/tasks/items", f.ownerTok,
		map[string]any{"title": "Granted", "fields": `{"status":"open"}`})
	f.must(rr, http.StatusCreated, "seed item")
	var it models.Item
	parseJSON(f.t, rr, &it)
	return it
}

func (f *accessFixture) grantID(rr *httptest.ResponseRecorder) string {
	f.t.Helper()
	var g struct {
		ID string `json:"id"`
	}
	parseJSON(f.t, rr, &g)
	return g.ID
}

func forEachDialect(t *testing.T, fn func(t *testing.T, driver store.DriverType)) {
	for _, d := range []store.DriverType{store.DriverSQLite, store.DriverPostgres} {
		t.Run(string(d), func(t *testing.T) { fn(t, d) })
	}
}

func TestWorkspaceAccessChanged_Create(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		srv := attachmentsServerOn(t, d)
		srv.SetWatchEventsBus(watchevents.New())
		u := mkUser(t, srv, "creator@example.com")
		mkUser(t, srv, "bystander@example.com")
		tok, err := srv.store.CreateAPIToken(u.ID, models.APITokenCreate{Name: "t"}, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		rr := bearerJSON(t, srv, "POST", "/api/v1/workspaces", tok.Token, map[string]any{"name": "Fresh"})
		if rr.Code != http.StatusCreated {
			t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
		}
		var ws models.Workspace
		parseJSON(t, rr, &ws)
		f := &accessFixture{t: t, srv: srv, wsID: ws.ID}
		f.expect(0, f.line("gained", "creator@example.com"))
	})
}

func TestWorkspaceAccessChanged_ImportJSONAndBundle(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newAccessFixture(t, d)
		for _, format := range []string{"json", "tar"} {
			path := "/api/v1/workspaces/" + f.wsSlug + "/export"
			ctype := "application/json"
			if format == "tar" {
				path += "?format=tar"
				ctype = "application/gzip"
			}
			rr := bearerCall(t, f.srv, "GET", path, f.ownerTok, nil)
			f.must(rr, http.StatusOK, "export "+format)
			export := rr.Body.Bytes()

			since := f.mark()
			req := httptest.NewRequest("POST", "/api/v1/workspaces/import?name=Imported-"+format, bytes.NewReader(export))
			req.Header.Set("Content-Type", ctype)
			req.Header.Set("Authorization", "Bearer "+f.ownerTok)
			req.RemoteAddr = "127.0.0.1:1234"
			rr = httptest.NewRecorder()
			f.srv.ServeHTTP(rr, req)
			f.must(rr, http.StatusCreated, "import "+format)
			var ws models.Workspace
			parseJSON(t, rr, &ws)
			f.expect(since, fmt.Sprintf("gained owner@example.com %s", ws.ID))
		}
	})
}

func TestWorkspaceAccessChanged_InviteExistingUser(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newAccessFixture(t, d)
		mkUser(t, f.srv, "invitee@example.com")
		since := f.mark()
		f.must(f.do("POST", "/api/v1/workspaces/"+f.wsSlug+"/members/invite", f.ownerTok,
			map[string]any{"email": "invitee@example.com", "role": "editor"}), http.StatusCreated, "invite")
		f.expect(since, f.line("gained", "invitee@example.com"))
	})
}

func TestWorkspaceAccessChanged_InvitationAccept(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newAccessFixture(t, d)
		since := f.mark()
		rr := f.do("POST", "/api/v1/workspaces/"+f.wsSlug+"/members/invite", f.ownerTok,
			map[string]any{"email": "later@example.com", "role": "editor"})
		f.must(rr, http.StatusCreated, "invite")
		var inv struct {
			Code string `json:"code"`
		}
		parseJSON(t, rr, &inv)
		if inv.Code == "" {
			t.Fatalf("invite of an unknown email returned no code: %s", rr.Body.String())
		}
		// The invitation itself grants nothing.
		f.expect(since)

		later := mkUser(t, f.srv, "later@example.com")
		f.must(f.do("POST", "/api/v1/invitations/"+inv.Code+"/accept", f.token(later), nil), http.StatusOK, "accept")
		f.expect(since, f.line("gained", "later@example.com"))
	})
}

func TestWorkspaceAccessChanged_CollectionGrants(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newAccessFixture(t, d)
		guest := mkUser(t, f.srv, "guest@example.com")
		mem := f.member("member@example.com", "editor")
		base := "/api/v1/workspaces/" + f.wsSlug + "/collections/"

		since := f.mark()
		rr := f.do("POST", base+"tasks/grants", f.ownerTok, map[string]any{"user_id": guest.ID})
		f.must(rr, http.StatusCreated, "first grant")
		first := f.grantID(rr)
		f.expect(since, f.line("gained", "guest@example.com"))

		// A second grant, and a grant to a member, change nobody's reach.
		since = f.mark()
		rr = f.do("POST", base+"ideas/grants", f.ownerTok, map[string]any{"user_id": guest.ID})
		f.must(rr, http.StatusCreated, "second grant")
		second := f.grantID(rr)
		f.must(f.do("POST", base+"tasks/grants", f.ownerTok, map[string]any{"user_id": mem.ID}), http.StatusCreated, "member grant")
		f.expect(since)

		// Revoking one of two leaves the guest in; revoking the last does not.
		f.must(f.do("DELETE", base+"tasks/grants/"+first, f.ownerTok, nil), http.StatusNoContent, "revoke first")
		f.expect(since)
		f.must(f.do("DELETE", base+"ideas/grants/"+second, f.ownerTok, nil), http.StatusNoContent, "revoke last")
		f.expect(since, f.line("lost", "guest@example.com"))
	})
}

func TestWorkspaceAccessChanged_ItemGrants(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newAccessFixture(t, d)
		guest := mkUser(t, f.srv, "item-guest@example.com")
		it := f.seedItem()
		path := "/api/v1/workspaces/" + f.wsSlug + "/items/" + it.Slug + "/grants"

		since := f.mark()
		rr := f.do("POST", path, f.ownerTok, map[string]any{"user_id": guest.ID})
		f.must(rr, http.StatusCreated, "item grant")
		f.expect(since, f.line("gained", "item-guest@example.com"))

		since = f.mark()
		f.must(f.do("DELETE", path+"/"+f.grantID(rr), f.ownerTok, nil), http.StatusNoContent, "revoke item grant")
		f.expect(since, f.line("lost", "item-guest@example.com"))
	})
}

func TestWorkspaceAccessChanged_MemberRemoval(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newAccessFixture(t, d)
		gone := f.member("gone@example.com", "editor")
		demoted := f.member("demoted@example.com", "editor")
		f.must(f.do("POST", "/api/v1/workspaces/"+f.wsSlug+"/collections/tasks/grants", f.ownerTok,
			map[string]any{"user_id": demoted.ID}), http.StatusCreated, "grant before demotion")

		since := f.mark()
		f.must(f.do("DELETE", "/api/v1/workspaces/"+f.wsSlug+"/members/"+gone.ID, f.ownerTok, nil), http.StatusNoContent, "remove")
		// Removed while keeping grants: still a guest, so nothing was lost.
		f.must(f.do("DELETE", "/api/v1/workspaces/"+f.wsSlug+"/members/"+demoted.ID+"?revoke_grants=false", f.ownerTok, nil),
			http.StatusNoContent, "remove keeping grants")
		f.expect(since, f.line("lost", "gone@example.com"))
	})
}

func TestWorkspaceAccessChanged_SoftDeleteRestorePurge(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newAccessFixture(t, d)
		f.member("member@example.com", "viewer")
		guest := mkUser(t, f.srv, "guest@example.com")
		mkUser(t, f.srv, "outsider@example.com")
		f.must(f.do("POST", "/api/v1/workspaces/"+f.wsSlug+"/collections/tasks/grants", f.ownerTok,
			map[string]any{"user_id": guest.ID}), http.StatusCreated, "guest grant")
		itemGuest := mkUser(t, f.srv, "item-guest@example.com")
		it := f.seedItem()
		f.must(f.do("POST", "/api/v1/workspaces/"+f.wsSlug+"/items/"+it.Slug+"/grants", f.ownerTok,
			map[string]any{"user_id": itemGuest.ID}), http.StatusCreated, "item-guest grant")
		everyone := func(change string) []string {
			return []string{f.line(change, "owner@example.com"), f.line(change, "member@example.com"),
				f.line(change, "guest@example.com"), f.line(change, "item-guest@example.com")}
		}

		since := f.mark()
		f.must(f.do("DELETE", "/api/v1/workspaces/"+f.wsSlug, f.ownerTok, nil), http.StatusNoContent, "soft delete")
		f.expect(since, everyone("deleted")...)

		since = f.mark()
		f.must(f.do("POST", "/api/v1/workspaces/"+f.wsSlug+"/restore", f.ownerTok, nil), http.StatusOK, "restore")
		f.expect(since, everyone("restored")...)

		f.must(f.do("DELETE", "/api/v1/workspaces/"+f.wsSlug, f.ownerTok, nil), http.StatusNoContent, "soft delete again")
		since = f.mark()
		res, err := f.srv.runWorkspacePurgeSweep(context.Background(), time.Now().UTC().Add(time.Minute))
		if err != nil || res.Purged != 1 {
			t.Fatalf("purge sweep: res=%+v err=%v", res, err)
		}
		f.expect(since, everyone("purged")...)
	})
}

func TestWorkspaceAccessChanged_AccountDeletion(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newAccessFixture(t, d)
		f.member("member@example.com", "editor")
		// A workspace the deleted account only BELONGS to is not deleted,
		// so it publishes nothing.
		other := mkUser(t, f.srv, "other-owner@example.com")
		rr := f.do("POST", "/api/v1/workspaces", f.token(other), map[string]any{"name": "Not Theirs"})
		f.must(rr, http.StatusCreated, "other workspace")
		var otherWS models.Workspace
		parseJSON(t, rr, &otherWS)
		if err := f.srv.store.AddWorkspaceMember(otherWS.ID, f.owner.ID, "editor"); err != nil {
			t.Fatal(err)
		}

		// An interactive session, not the fixture's PAT: an API token cannot
		// delete its own account (BUG-3336).
		sess, err := f.srv.store.CreateSession(f.owner.ID, "cli-browser-auth", "192.0.2.1", "", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		since := f.mark()
		f.must(f.do("POST", "/api/v1/auth/delete-account", sess,
			map[string]any{"password": "correct-horse-battery-staple"}), http.StatusOK, "delete account")
		got := f.accessSince(since)
		// The deleted owner is still in the recipient set (read before the
		// delete); its id no longer resolves to an email, so match the member.
		var sawMember bool
		for _, l := range got {
			if !strings.HasPrefix(l, "deleted ") || !strings.HasSuffix(l, " "+f.wsID) {
				t.Fatalf("unexpected notification %q (want only deletions of %s)", l, f.wsID)
			}
			if l == f.line("deleted", "member@example.com") {
				sawMember = true
			}
		}
		if !sawMember {
			t.Fatalf("the owned workspace's member was not told: %q", got)
		}
	})
}

// --- delivery, through the real stream handler ---

// accessPayloads drains ch until a status-change notification for sentinelRef
// arrives, returning every workspace_access_changed payload seen before it.
// The sentinel is published AFTER the events under test, so "none before the
// sentinel" is a positive observation, not a timeout.
func accessPayloadsUntil(t *testing.T, ch <-chan watchSSEEvent, sentinelRef string) []watchEventPayload {
	t.Helper()
	var got []watchEventPayload
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("stream closed before the sentinel")
			}
			if ev.Type != "notification" {
				continue
			}
			var p watchEventPayload
			if err := json.Unmarshal([]byte(ev.Data), &p); err != nil {
				t.Fatalf("parse payload: %v", err)
			}
			if p.Kind == watchevents.KindWorkspaceAccessChanged {
				got = append(got, p)
				continue
			}
			if p.ItemRef == sentinelRef {
				return got
			}
		case <-deadline:
			t.Fatal("timed out waiting for the sentinel")
		}
	}
}

// streamFixture is an access fixture whose member holds a watched item, so a
// status change on it can serve as the sentinel on the member's streams.
func streamFixture(t *testing.T) (*accessFixture, *models.User, string, models.Item) {
	t.Helper()
	f := newAccessFixture(t, store.DriverSQLite)
	it := f.seedItem()
	watcher := f.member("watcher@example.com", "editor")
	wtok := f.token(watcher)
	f.must(bearerCall(t, f.srv, "POST", "/api/v1/workspaces/"+f.wsSlug+"/items/"+it.Slug+"/watch", wtok, nil),
		http.StatusOK, "watch")
	return f, watcher, wtok, it
}

func (f *accessFixture) sentinel(it models.Item, status string) {
	f.t.Helper()
	f.must(f.do("PATCH", "/api/v1/workspaces/"+f.wsSlug+"/items/"+it.Slug, f.ownerTok,
		map[string]any{"fields": `{"status":"` + status + `"}`}), http.StatusOK, "sentinel update")
}

// TestWorkspaceAccessChanged_StreamOptInAndAddressing: the event reaches the
// affected user's access=true stream with its workspace_id and change, and
// no other stream: not the same user's stream without access=true (armed or
// not), and not another user's access=true stream.
func TestWorkspaceAccessChanged_StreamOptInAndAddressing(t *testing.T) {
	f, _, wtok, it := streamFixture(t)
	target := mkUser(t, f.srv, "target@example.com")
	ttok := f.token(target)
	// The sentinel rides the watcher's streams (it watches the item); the
	// target watches nothing, so its stream is asserted on its first event.
	ts := httptest.NewServer(f.srv)
	defer ts.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	targetAccess := connectWatchStreamWithHeadersAndQuery(ctx, t, ts.URL, ttok, nil, "access=true")
	watcherAccess := connectWatchStreamWithHeadersAndQuery(ctx, t, ts.URL, wtok, nil, "access=true")
	watcherPlain := connectWatchStream(ctx, t, ts.URL, wtok)
	watcherArmed := connectArmedWatchStream(ctx, t, ts.URL, wtok)
	for _, ch := range []<-chan watchSSEEvent{targetAccess, watcherAccess, watcherPlain, watcherArmed} {
		waitForWatchEvent(t, ch, 3*time.Second) // connected
	}

	f.must(f.do("POST", "/api/v1/workspaces/"+f.wsSlug+"/members/invite", f.ownerTok,
		map[string]any{"email": "target@example.com", "role": "viewer"}), http.StatusCreated, "invite target")
	ev := waitForWatchEvent(t, targetAccess, 3*time.Second)
	var p watchEventPayload
	if err := json.Unmarshal([]byte(ev.Data), &p); err != nil {
		t.Fatal(err)
	}
	if p.Kind != watchevents.KindWorkspaceAccessChanged || p.Change != "gained" || p.WorkspaceID != f.wsID || p.Workspace != f.wsSlug {
		t.Fatalf("target's access stream got %+v", p)
	}

	f.sentinel(it, "done")
	if got := accessPayloadsUntil(t, watcherAccess, it.Ref); len(got) != 0 {
		t.Fatalf("another user's access=true stream received the target's event: %+v", got)
	}

	// Now events addressed to the watcher itself: only its access stream may
	// carry them.
	watcher, _ := f.srv.store.GetUserByEmail("watcher@example.com")
	f.srv.publishWorkspaceAccessChanged(f.wsID, watchevents.AccessGained, []string{watcher.ID}, "user", "")
	f.srv.publishWorkspaceAccessChanged(f.wsID, watchevents.AccessDeleted, []string{watcher.ID}, "user", "")
	f.sentinel(it, "open")
	if got := accessPayloadsUntil(t, watcherAccess, it.Ref); len(got) != 2 {
		t.Fatalf("watcher's access stream: got %d access events, want 2: %+v", len(got), got)
	}
	for name, ch := range map[string]<-chan watchSSEEvent{"plain": watcherPlain, "armed": watcherArmed} {
		// Both sentinels are in these streams' buffers; drain through both.
		if got := accessPayloadsUntil(t, ch, it.Ref); len(got) != 0 {
			t.Fatalf("%s stream (no access=true) received access events: %+v", name, got)
		}
		if got := accessPayloadsUntil(t, ch, it.Ref); len(got) != 0 {
			t.Fatalf("%s stream (no access=true) received access events: %+v", name, got)
		}
	}
}

// TestWorkspaceAccessChanged_ReplayHonoursOptIn: a Last-Event-ID resume
// replays the event to an access=true stream and withholds it from one
// without, the same rule as live delivery.
func TestWorkspaceAccessChanged_ReplayHonoursOptIn(t *testing.T) {
	f, watcher, wtok, it := streamFixture(t)
	// A resume needs a positive Last-Event-ID; this baseline supplies one
	// without depending on what the fixture happened to publish.
	f.sentinel(it, "in-progress")
	since := f.mark()
	if since == 0 {
		t.Fatal("the baseline sentinel did not reach the bus")
	}
	f.srv.publishWorkspaceAccessChanged(f.wsID, watchevents.AccessLost, []string{watcher.ID}, "user", "")
	f.sentinel(it, "done")

	ts := httptest.NewServer(f.srv)
	defer ts.Close()
	for _, tc := range []struct {
		query string
		want  int
	}{{"access=true", 1}, {"", 0}, {"armed=true", 0}} {
		ctx, cancel := context.WithCancel(context.Background())
		ch := connectWatchStreamWithHeadersAndQuery(ctx, t, ts.URL, wtok,
			map[string]string{"Last-Event-ID": fmt.Sprint(since)}, tc.query)
		waitForWatchEvent(t, ch, 3*time.Second) // connected
		got := accessPayloadsUntil(t, ch, it.Ref)
		cancel()
		if len(got) != tc.want {
			t.Fatalf("replay with %q: got %d access events, want %d: %+v", tc.query, len(got), tc.want, got)
		}
		if tc.want == 1 && (got[0].Change != "lost" || got[0].WorkspaceID != f.wsID) {
			t.Fatalf("replayed payload: %+v", got[0])
		}
	}
}

// TestWatchEventPayload_ExistingKindsAreByteIdentical pins every other kind's
// payload to its exact pre-TASK-3272 bytes, even when the notification
// carries an AccessChange, so DOC-2479's shape cannot move under existing
// consumers.
func TestWatchEventPayload_ExistingKindsAreByteIdentical(t *testing.T) {
	srv := testServerWithWatchEvents(t)
	for _, kind := range []string{watchevents.KindStatusChange, watchevents.KindAssignment, watchevents.KindComment, watchevents.KindPush, watchevents.KindAsk} {
		b, err := json.Marshal(watchEventPayloadFor(srv, watchevents.Notification{
			ID: 7, Timestamp: 1700000000, WorkspaceID: "no-such-ws", Kind: kind, AccessChange: "gained",
			ItemRef: "TASK-1", Actor: "user", ActorName: "Dave", Summary: "open → done",
		}))
		if err != nil {
			t.Fatal(err)
		}
		want := `{"id":7,"ts":1700000000,"workspace":"no-such-ws","item_ref":"TASK-1","kind":"` + kind + `","actor":"Dave","summary":"open → done"}`
		if string(b) != want {
			t.Fatalf("%s payload bytes changed:\n got: %s\nwant: %s", kind, b, want)
		}
	}
}
