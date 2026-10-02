package server

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/PerpetualSoftware/pad/internal/accesskick"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/redisns"
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

// deterministicFirstTick makes a connection's first revalidation land a full
// interval out instead of anywhere inside it (codex r1): with the interval at
// an hour, nothing but a kick can re-check during a test.
func deterministicFirstTick(t *testing.T) {
	t.Helper()
	prev := revalFirstDelay
	revalFirstDelay = func(interval time.Duration) time.Duration { return interval }
	t.Cleanup(func() { revalFirstDelay = prev })
}

func miniredisTransport(t *testing.T) (*miniredis.Miniredis, func() accesskick.Transport) {
	t.Helper()
	mr := miniredis.RunT(t)
	return mr, func() accesskick.Transport {
		client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		t.Cleanup(func() { _ = client.Close() })
		return accesskick.NewRedisTransport(client, redisns.Default)
	}
}

func waitKick(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("%s: no kick within 2s", what)
	}
}

// Kicks travel their own transport, so a revocation on one instance reaches
// connections held by another, including a workspace kick reaching a watch
// stream through the wildcard.
func TestTASK3365_KickCrossesInstances(t *testing.T) {
	_, newTransport := miniredisTransport(t)
	a, b := testServer(t), testServer(t)
	a.SetAccessKickTransport(newTransport())
	b.SetAccessKickTransport(newTransport())
	t.Cleanup(func() { a.SetAccessKickTransport(nil); b.SetAccessKickTransport(nil) })

	userKick, unregUser := b.accessKicks().register("u1", "")
	defer unregUser()
	wsKick, unregWS := b.accessKicks().register("", "w1")
	defer unregWS()
	watchKick, unregWatch := b.accessKicks().register("u9", workspaceWildcard)
	defer unregWatch()

	a.invalidateUserAccess("u1")
	waitKick(t, userKick, "user kick")
	a.invalidateWorkspaceAccess("w1")
	waitKick(t, wsKick, "workspace kick")
	waitKick(t, watchKick, "workspace kick reaching a watch stream")
	// The existing access-changed helper kicks too.
	a.publishWorkspaceAccessChanged("w1", watchevents.AccessLost, []string{"u1"}, "", "")
	waitKick(t, userKick, "workspace_access_changed")
}

// Kicks never touch the client watch bus (codex r1): no sequence id, no
// replay slot, no entry in any client's queue.
func TestTASK3365_KicksDoNotUseTheClientWatchBus(t *testing.T) {
	_, newTransport := miniredisTransport(t)
	srv := testServer(t)
	bus := watchevents.New()
	defer bus.Close()
	srv.SetWatchEventsBus(bus)
	srv.SetAccessKickTransport(newTransport())
	t.Cleanup(func() { srv.SetAccessKickTransport(nil) })
	kick, unreg := srv.accessKicks().register("u1", "w1")
	defer unreg()
	for i := 0; i < 100; i++ {
		srv.invalidateUserAccess("u1")
		srv.invalidateWorkspaceAccess("w1")
	}
	waitKick(t, kick, "kick")
	if got := bus.EventsSince(0); len(got) != 0 {
		t.Fatalf("kicks put %d notifications on the client watch bus", len(got))
	}
}

type countingTransport struct {
	mu                sync.Mutex
	subscribes, stops int
	handle            func(accesskick.Message)
}

func (c *countingTransport) Publish(_ context.Context, m accesskick.Message) error {
	c.mu.Lock()
	h := c.handle
	c.mu.Unlock()
	if h != nil {
		h(m)
	}
	return nil
}

func (c *countingTransport) Subscribe(h func(accesskick.Message)) func() {
	c.mu.Lock()
	c.subscribes++
	c.handle = h
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		c.stops++
		c.handle = nil
		c.mu.Unlock()
	}
}

// Installing the same transport twice subscribes once; replacing it stops
// the previous subscription (codex r1).
func TestTASK3365_TransportInstallIsIdempotent(t *testing.T) {
	srv := testServer(t)
	first, second := &countingTransport{}, &countingTransport{}
	srv.SetAccessKickTransport(first)
	srv.SetAccessKickTransport(first)
	if first.subscribes != 1 {
		t.Fatalf("same transport installed twice subscribed %d times", first.subscribes)
	}
	srv.SetAccessKickTransport(second)
	if first.stops != 1 || second.subscribes != 1 {
		t.Fatalf("replacement: first stops=%d, second subscribes=%d", first.stops, second.subscribes)
	}
	srv.SetAccessKickTransport(nil)
	if second.stops != 1 {
		t.Fatalf("nil did not stop the subscription (stops=%d)", second.stops)
	}
}

func TestTASK3365_CollabClosesOnKickWithoutWaitingForTheTick(t *testing.T) {
	orig := collabMembershipRevalInterval
	collabMembershipRevalInterval = time.Hour
	defer func() { collabMembershipRevalInterval = orig }()
	deterministicFirstTick(t)

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
	deterministicFirstTick(t)

	srv := testServerWithEvents(t)
	_, newTransport := miniredisTransport(t)
	srv.SetAccessKickTransport(newTransport()) // the kick goes through Redis here
	t.Cleanup(func() { srv.SetAccessKickTransport(nil) })
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

// A removal that leaves the user SOME access (a grant) still narrows it, so
// the user's connections are kicked even though "lost" is not published.
func TestTASK3365_PartialRevokeStillKicks(t *testing.T) {
	f := newAccessFixture(t, "sqlite")
	u := f.member("partial@example.com", "editor")
	it := f.seedItem()
	if _, err := f.srv.store.CreateItemGrant(f.wsID, it.ID, u.ID, "view", f.owner.ID); err != nil {
		t.Fatal(err)
	}
	kick, unreg := f.srv.accessKicks().register(u.ID, "")
	defer unreg()
	f.must(f.do("DELETE", "/api/v1/workspaces/"+f.wsSlug+"/members/"+u.ID+"?revoke_grants=false", f.ownerTok, nil), http.StatusNoContent, "remove member, keep grant")
	waitKick(t, kick, "partial revoke")
}
