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
)

// BUG-3273: an open workspace stream (GET /api/v1/events) survived a soft
// delete of its workspace. The entry check refuses a deleted workspace, but
// the per-tick recheck, sseSubscriberStillHasAccess, asked only about the
// principal, and a soft delete keeps membership and grant rows so a restore
// can bring them back. The two disagreed for as long as the tab stayed open.

// TestSSESubscriberStillHasAccess_SoftDeletedWorkspace asks the recheck about
// every principal it admits, before the delete (the control: each IS admitted)
// and after it (each must be refused).
func TestSSESubscriberStillHasAccess_SoftDeletedWorkspace(t *testing.T) {
	srv := testServer(t)
	admin := mkUserRole(t, srv, "admin@example.com", "admin")
	member := mkUserRole(t, srv, "member@example.com", "member")
	guest := mkUserRole(t, srv, "guest@example.com", "member")

	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Doomed", OwnerID: admin.ID})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, member.ID, "editor"); err != nil {
		t.Fatalf("add member: %v", err)
	}
	coll, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Things", Slug: "things", Prefix: "THG",
		Schema: `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"}]}`,
	})
	if err != nil {
		t.Fatalf("create collection: %v", err)
	}
	if _, err := srv.store.CreateCollectionGrant(ws.ID, coll.ID, guest.ID, "view", admin.ID); err != nil {
		t.Fatalf("guest grant: %v", err)
	}

	userReq := func(u *models.User) *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/events?workspace="+ws.Slug, nil)
		return req.WithContext(context.WithValue(req.Context(), ctxCurrentUser, u))
	}
	tokenReq := httptest.NewRequest(http.MethodGet, "/api/v1/events?workspace="+ws.Slug, nil)
	tokenReq = tokenReq.WithContext(context.WithValue(tokenReq.Context(), ctxTokenWorkspaceID, ws.ID))

	principals := map[string]*http.Request{
		"member":              userReq(member),
		"grant-holding guest": userReq(guest),
		"cookie admin":        userReq(admin),
		"legacy ws token":     tokenReq,
	}
	for name, req := range principals {
		if !srv.sseSubscriberStillHasAccess(req, ws.ID) {
			t.Fatalf("control: %s should have access to a LIVE workspace", name)
		}
	}

	if err := srv.store.DeleteWorkspace(ws.Slug); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	for name, req := range principals {
		if srv.sseSubscriberStillHasAccess(req, ws.ID) {
			t.Errorf("%s still has access to a SOFT-DELETED workspace", name)
		}
	}
}

// TestSSEStream_ClosesWhenItsWorkspaceIsSoftDeleted is the same question
// asked through the router on a live server: a member's open stream answers
// `unauthorized` and ends on the first tick after the delete, and not before.
func TestSSEStream_ClosesWhenItsWorkspaceIsSoftDeleted(t *testing.T) {
	prev := sseMembershipRevalInterval
	sseMembershipRevalInterval = 25 * time.Millisecond
	t.Cleanup(func() { sseMembershipRevalInterval = prev })

	srv := testServerWithEvents(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	owner := mkUserRole(t, srv, "owner@example.com", "admin")
	member := mkUserRole(t, srv, "member@example.com", "member")
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Doomed Stream", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, member.ID, "editor"); err != nil {
		t.Fatalf("add member: %v", err)
	}
	token, err := srv.store.CreateSession(member.ID, "test", "127.0.0.1", testSessionUA, webSessionTTL)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/v1/events?workspace="+ws.Slug, nil)
	if err != nil {
		t.Fatal(err)
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

	events := make(chan string, 64)
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if line := strings.TrimSpace(sc.Text()); strings.HasPrefix(line, "event: ") {
				events <- strings.TrimPrefix(line, "event: ")
			}
		}
	}()

	// Control: many ticks pass on a live workspace and the stream is not
	// closed, so the close below is caused by the delete.
	liveWindow := time.After(300 * time.Millisecond)
control:
	for {
		select {
		case ev := <-events:
			if ev == "unauthorized" {
				t.Fatal("control: the stream was refused while its workspace was live")
			}
		case <-ended:
			t.Fatal("control: the stream ended while its workspace was live")
		case <-liveWindow:
			break control
		}
	}

	if err := srv.store.DeleteWorkspace(ws.Slug); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	deadline := time.After(3 * time.Second)
	sawUnauthorized := false
	for {
		select {
		case ev := <-events:
			if ev == "unauthorized" {
				sawUnauthorized = true
			}
		case <-ended:
			if !sawUnauthorized {
				t.Fatal("the stream ended without saying why")
			}
			return
		case <-deadline:
			t.Fatalf("the stream was still open 3s after its workspace was soft-deleted (unauthorized seen: %v)", sawUnauthorized)
		}
	}
}

func mkUserRole(t *testing.T, srv *Server, email, role string) *models.User {
	t.Helper()
	u, err := srv.store.CreateUser(models.UserCreate{
		Email: email, Name: email, Password: "correct-horse-battery-staple", Role: role,
	})
	if err != nil {
		t.Fatalf("CreateUser %s: %v", email, err)
	}
	return u
}
