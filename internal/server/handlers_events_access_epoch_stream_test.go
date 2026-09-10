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

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestSSEStream_AnnouncesAnAccessChangeButStaysQuietOtherwise is the WIRING
// half of IDEA-2898's server signal (CONVE-19): the epoch's correctness is
// pinned by the unit test next door, and that test says nothing about whether
// the revalidation tick ever WRITES anything. A version of this change that
// computed the epoch perfectly and never emitted would pass every other test.
//
// Both legs are load-bearing, and the quiet one is the sharper:
//
//   - QUIET: a tick that emitted unconditionally would send a client into a
//     full authoritative resync once a minute, forever, on every open tab.
//     That is worse than the defect being closed, and it is indistinguishable
//     from correct behaviour if you only assert that the signal arrives.
//   - ANNOUNCED: without it the client's warm index holds the revoked
//     collection's rows until it next bootstraps.
//
// Through the router on a live server, not by calling the handler.
func TestSSEStream_AnnouncesAnAccessChangeButStaysQuietOtherwise(t *testing.T) {
	// A real minute is not a test. The var exists to be shrunk; restore it so
	// a parallel-package run isn't left with a 25ms cadence.
	prev := sseMembershipRevalInterval
	sseMembershipRevalInterval = 25 * time.Millisecond
	t.Cleanup(func() { sseMembershipRevalInterval = prev })

	srv := testServerWithEvents(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	owner, err := srv.store.CreateUser(models.UserCreate{
		Email: "owner@example.com", Name: "Owner", Password: "correct-horse-battery-staple", Role: "admin",
	})
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	member, err := srv.store.CreateUser(models.UserCreate{
		Email: "member@example.com", Name: "Member", Password: "correct-horse-battery-staple", Role: "member",
	})
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "EpochStream", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("add owner: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, member.ID, "editor"); err != nil {
		t.Fatalf("add member: %v", err)
	}
	schema := `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"}]}`
	kept, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Kept", Slug: "kept", Prefix: "KEP", Schema: schema})
	if err != nil {
		t.Fatalf("create kept: %v", err)
	}
	revoked, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Revoked", Slug: "revoked", Prefix: "REV", Schema: schema})
	if err != nil {
		t.Fatalf("create revoked: %v", err)
	}
	if err := srv.store.SetMemberCollectionAccess(ws.ID, member.ID, "specific", []string{kept.ID, revoked.ID}); err != nil {
		t.Fatalf("grant both: %v", err)
	}
	// The session is IP- and UA-bound; over a live server the request really
	// does arrive from loopback, so bind it there rather than at the
	// 192.0.2.1 the ResponseRecorder helpers can fake.
	token, err := srv.store.CreateSession(member.ID, "test", "127.0.0.1", testSessionUA, webSessionTTL)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/v1/events?workspace="+ws.Slug, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("User-Agent", testSessionUA)
	req.AddCookie(&http.Cookie{Name: sessionCookieName(srv.secureCookies), Value: token})
	resp, err := isolatedTestClient().Do(req)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200", resp.StatusCode)
	}

	var mu sync.Mutex
	var syncRequired int
	connected := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(resp.Body)
		closedConnected := false
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "event: connected" && !closedConnected {
				closedConnected = true
				close(connected)
			}
			if line == "event: sync_required" {
				mu.Lock()
				syncRequired++
				mu.Unlock()
			}
		}
	}()

	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		t.Fatal("stream never announced `connected`")
	}

	// QUIET LEG. Several revalidation ticks with nothing changing.
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	quiet := syncRequired
	mu.Unlock()
	if quiet != 0 {
		t.Fatalf("tick emitted %d sync_required with no access change; a per-tick resync is worse than the defect", quiet)
	}

	// ANNOUNCED LEG. A revocation that writes no item — the case the delta
	// stream cannot carry.
	if err := srv.store.SetMemberCollectionAccess(ws.ID, member.ID, "specific", []string{kept.ID}); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		got := syncRequired
		mu.Unlock()
		if got > 0 {
			return
		}
		select {
		case <-deadline:
			t.Fatal("no sync_required after the caller's collection access was narrowed")
		case <-time.After(10 * time.Millisecond):
		}
	}
}
