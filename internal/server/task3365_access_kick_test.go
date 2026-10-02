package server

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/watchevents"
)

// TASK-3365: revoking access re-checks live connections NOW, by kicking the
// revalidation they already run on a 60s tick. These tests set that tick to
// an hour, so a close within the deadline can only come from a kick.

func TestTASK3365_KickerCoalescesAndIsNonBlocking(t *testing.T) {
	k := newAccessKicker()
	const n = 500
	chans := make([]<-chan struct{}, n)
	unregs := make([]func(), n)
	for i := range chans {
		chans[i], unregs[i] = k.register("u1", "w1")
	}
	other, _ := k.register("u2", "w2")

	done := make(chan int, 1)
	go func() {
		// Many kicks with nobody draining: must not block.
		total := 0
		for i := 0; i < 10; i++ {
			total += k.kickUser("u1")
		}
		done <- total
	}()
	select {
	case total := <-done:
		if total != 10*n {
			t.Fatalf("kickUser reached %d registrations, want %d", total, 10*n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("kicking undrained connections blocked the kicker")
	}
	for i, ch := range chans {
		select {
		case <-ch:
		default:
			t.Fatalf("registration %d was not kicked", i)
		}
		select {
		case <-ch:
			t.Fatalf("registration %d holds a second kick; kicks must coalesce", i)
		default:
		}
	}
	select {
	case <-other:
		t.Fatal("a kick for u1 reached u2")
	default:
	}

	if got := k.kickWorkspace("w1"); got != n {
		t.Fatalf("kickWorkspace reached %d, want %d", got, n)
	}
	for _, u := range unregs {
		u()
		u() // idempotent
	}
	if got := k.kickUser("u1"); got != 0 {
		t.Fatalf("after unregister, kickUser reached %d", got)
	}
	if got := k.kickWorkspace("w1"); got != 0 {
		t.Fatalf("after unregister, kickWorkspace reached %d", got)
	}
}

// LOAD-BEARING (lead): the internal kind must never reach a client, whatever
// the stream asked for and whoever it is addressed to.
func TestTASK3365_AccessInvalidatedIsNeverDelivered(t *testing.T) {
	// The caller sees everything and watches the item unconditionally, so
	// the generic path below the drop WOULD deliver any kind on this item:
	// the control proves it, and only the explicit drop refuses ours.
	watches := map[string]string{"item1": ""}
	full := watchAccessVisibility{fullAccess: true}
	control := watchevents.Notification{Kind: watchevents.KindComment, WorkspaceID: "w1", ItemID: "item1"}
	if !watchStreamDelivers(watches, full, "u1", "s1", true, true, control) {
		t.Fatal("control: an ordinary notification on a watched item should be delivered")
	}
	for _, n := range []watchevents.Notification{
		{Kind: watchevents.KindAccessInvalidated, TargetUserID: "u1", WorkspaceID: "w1"},
		{Kind: watchevents.KindAccessInvalidated, TargetUserID: "u1", WorkspaceID: "w1", ItemID: "item1"},
	} {
		for _, access := range []bool{false, true} {
			for _, armed := range []bool{false, true} {
				if watchStreamDelivers(watches, full, "u1", "s1", armed, access, n) {
					t.Fatalf("access_invalidated delivered (item=%q access=%v armed=%v)", n.ItemID, access, armed)
				}
			}
		}
	}
}

// Kicks travel the bus, so a revocation on one instance reaches connections
// held by another.
func TestTASK3365_KickCrossesInstances(t *testing.T) {
	bus := watchevents.New()
	defer bus.Close()
	a, b := testServer(t), testServer(t)
	a.SetWatchEventsBus(bus)
	b.SetWatchEventsBus(bus)
	userKick, unregUser := b.accessKicks().register("u1", "")
	defer unregUser()
	wsKick, unregWS := b.accessKicks().register("", "w1")
	defer unregWS()

	wait := func(ch <-chan struct{}, what string) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: the kick did not reach the other instance", what)
		}
	}
	// The subscriber goroutines start asynchronously; publish until seen.
	deadline := time.Now().Add(2 * time.Second)
	for {
		a.invalidateUserAccess("u1")
		select {
		case <-userKick:
			goto userDone
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("user kick never crossed instances")
		}
	}
userDone:
	a.invalidateWorkspaceAccess("w1")
	wait(wsKick, "workspace kick")
	// The existing workspace_access_changed publication kicks too, so the
	// doors that already publish it need no second call.
	if err := bus.Publish(watchevents.Notification{Kind: watchevents.KindWorkspaceAccessChanged, TargetUserID: "u1", AccessChange: watchevents.AccessLost}); err != nil {
		t.Fatal(err)
	}
	wait(userKick, "workspace_access_changed")
}

func TestTASK3365_CollabClosesOnKickWithoutWaitingForTheTick(t *testing.T) {
	orig := collabMembershipRevalInterval
	collabMembershipRevalInterval = time.Hour
	defer func() { collabMembershipRevalInterval = orig }()

	srv := testServerWithCollab(t)
	bootstrapFirstUser(t, srv, "admin@test.com", "Admin")
	ts := httptest.NewServer(srv)
	defer ts.Close()
	ws, _ := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "KickCollab"})
	col, _ := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks", Schema: `{"fields":[]}`})
	item, _ := srv.store.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: "Item", Fields: `{}`})
	u, _ := srv.store.CreateUser(models.UserCreate{Email: "m@test.com", Name: "M", Password: "correct-horse-battery-staple", Role: "member"})
	if err := srv.store.AddWorkspaceMember(ws.ID, u.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	token, _ := srv.store.CreateSession(u.ID, "go-test", "127.0.0.1", "go-test", 24*time.Hour)
	conn, resp, err := dialCollab(t, ts.URL, item.ID, []*http.Cookie{{Name: "pad_session", Value: token}}, "go-test")
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// One reader for the life of the connection: a gorilla read deadline is
	// permanent once it trips, so the control must not use one.
	closed := make(chan error, 1)
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				closed <- err
				return
			}
		}
	}()

	// Control: with no kick, the connection stays open (the tick is an hour).
	if err := srv.store.RemoveWorkspaceMember(ws.ID, u.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-closed:
		t.Fatalf("control: the connection closed before any kick: %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	srv.invalidateUserAccess(u.ID)
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("the collab connection was still open 2s after the kick")
	}
}

func TestTASK3365_SSEClosesOnKickWithoutWaitingForTheTick(t *testing.T) {
	prev := sseMembershipRevalInterval
	sseMembershipRevalInterval = time.Hour
	t.Cleanup(func() { sseMembershipRevalInterval = prev })

	srv := testServerWithEvents(t)
	bus := watchevents.New()
	defer bus.Close()
	srv.SetWatchEventsBus(bus) // the kick goes through the bus here
	ts := httptest.NewServer(srv)
	defer ts.Close()
	owner := mkUserRole(t, srv, "owner@example.com", "admin")
	member := mkUserRole(t, srv, "member@example.com", "member")
	ws, _ := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Kick Stream", OwnerID: owner.ID})
	if err := srv.store.AddWorkspaceMember(ws.ID, member.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	token, _ := srv.store.CreateSession(member.ID, "test", "127.0.0.1", testSessionUA, webSessionTTL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/v1/events?workspace="+ws.Slug, nil)
	req.Header.Set("User-Agent", testSessionUA)
	req.AddCookie(&http.Cookie{Name: sessionCookieName(srv.secureCookies), Value: token})
	resp, err := isolatedTestClient().Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("open stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	ended := make(chan struct{})
	sawUnauthorized := make(chan struct{}, 1)
	go func() {
		defer close(ended)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if strings.TrimSpace(sc.Text()) == "event: unauthorized" {
				select {
				case sawUnauthorized <- struct{}{}:
				default:
				}
			}
		}
	}()

	if err := srv.store.RemoveWorkspaceMember(ws.ID, member.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ended:
		t.Fatal("control: the stream ended before any kick")
	case <-time.After(300 * time.Millisecond):
	}
	// The subscriber starts asynchronously: re-publish until the stream ends.
	deadline := time.After(3 * time.Second)
	for {
		srv.invalidateUserAccess(member.ID)
		select {
		case <-ended:
			select {
			case <-sawUnauthorized:
			default:
				t.Fatal("the stream ended without saying why")
			}
			return
		case <-deadline:
			t.Fatal("the SSE stream was still open 3s after the kick")
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// The watch stream re-checks on a kick too, and a dead CREDENTIAL closes it
// rather than leaving it connected and silent (lead, TASK-3365 detail 3).
func TestTASK3365_WatchStreamClosesOnKickWhenItsCredentialDies(t *testing.T) {
	prev := watchListRevalInterval
	watchListRevalInterval = time.Hour
	t.Cleanup(func() { watchListRevalInterval = prev })

	srv := testServer(t)
	bus := watchevents.New()
	defer bus.Close()
	srv.SetWatchEventsBus(bus)
	ts := httptest.NewServer(srv)
	defer ts.Close()
	u := mkUserRole(t, srv, "watcher@example.com", "member")
	token, _ := srv.store.CreateSession(u.ID, "test", "127.0.0.1", testSessionUA, webSessionTTL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/v1/events/stream", nil)
	req.Header.Set("User-Agent", testSessionUA)
	req.AddCookie(&http.Cookie{Name: sessionCookieName(srv.secureCookies), Value: token})
	resp, err := isolatedTestClient().Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("open watch stream: %v (%v)", err, resp)
	}
	defer func() { _ = resp.Body.Close() }()
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
		}
	}()

	// The credential dies (a logout): with no kick, nothing notices for an hour.
	if err := srv.store.DeleteSession(token); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ended:
		t.Fatal("control: the stream ended before any kick")
	case <-time.After(300 * time.Millisecond):
	}
	deadline := time.After(3 * time.Second)
	for {
		srv.invalidateUserAccess(u.ID)
		select {
		case <-ended:
			return
		case <-deadline:
			t.Fatal("the watch stream was still open 3s after the kick")
		case <-time.After(50 * time.Millisecond):
		}
	}
}
