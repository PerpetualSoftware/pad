package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestSSEEntryRequiresMembership is the TASK-264 regression: the SSE
// entry gate must reject an authenticated NON-member for BOTH the
// ?workspace=<slug> and ?workspace=<uuid> forms with a 404 (not 403 —
// a 403 would itself be a workspace-existence oracle), while a
// legitimate member still connects (200 + the initial "connected"
// event).
//
// The UUID form is the one the pre-fix handler leaked: resolveWorkspace
// resolves a UUID ?workspace= param via the global, unscoped
// GetWorkspaceByID, so a non-member reached SubscribeIfAllowed with a
// fully-resolved workspace (a connection-slot DoS + existence oracle).
// The slug form was already membership-scoped
// (GetWorkspacesBySlugForUser → nil → 404); we assert it too so the two
// forms can never silently drift apart.
//
// Every subtest drives handleSSE with a short-deadline context so the
// live-stream select loop unblocks and the synchronous call returns
// even when the handler admits the connection.
func TestSSEEntryRequiresMembership(t *testing.T) {
	srv := testServerWithEvents(t)

	owner, err := srv.store.CreateUser(models.UserCreate{
		Email: "owner@example.com", Name: "Owner", Password: "correct-horse-battery-staple", Role: "member",
	})
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	outsider, err := srv.store.CreateUser(models.UserCreate{
		Email: "outsider@example.com", Name: "Outsider", Password: "correct-horse-battery-staple", Role: "member",
	})
	if err != nil {
		t.Fatalf("create outsider: %v", err)
	}

	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "PrivateWS"})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("add owner member: %v", err)
	}

	// mkReq builds a GET /api/v1/events request carrying the given user
	// in context (mirroring what the auth middleware would set) and a
	// bounded deadline so an admitted stream returns instead of hanging.
	mkReq := func(u *models.User, wsParam string) (*http.Request, context.CancelFunc) {
		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		if u != nil {
			ctx = context.WithValue(ctx, ctxCurrentUser, u)
		}
		req := httptest.NewRequest(http.MethodGet, "/api/v1/events?workspace="+wsParam, nil)
		return req.WithContext(ctx), cancel
	}

	// Non-member via SLUG → 404. resolveWorkspace's membership-scoped
	// slug lookup returns nil for an outsider, so the existing not-found
	// branch already covers this form.
	t.Run("non-member via slug rejected with 404", func(t *testing.T) {
		req, cancel := mkReq(outsider, ws.Slug)
		defer cancel()
		rr := httptest.NewRecorder()
		srv.handleSSE(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("non-member via slug: got %d, want 404; body=%q", rr.Code, rr.Body.String())
		}
		if strings.Contains(rr.Body.String(), "connected") {
			t.Fatalf("non-member via slug must not receive a connected event; body=%q", rr.Body.String())
		}
	})

	// Non-member via UUID → 404. This is the TASK-264 gap: resolveWorkspace
	// resolves the UUID globally via GetWorkspaceByID, so without the
	// explicit entry-gate the outsider would get 200 + connected.
	t.Run("non-member via uuid rejected with 404", func(t *testing.T) {
		req, cancel := mkReq(outsider, ws.ID)
		defer cancel()
		rr := httptest.NewRecorder()
		srv.handleSSE(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("non-member via uuid: got %d, want 404; body=%q", rr.Code, rr.Body.String())
		}
		if strings.Contains(rr.Body.String(), "connected") {
			t.Fatalf("non-member via uuid must not receive a connected event; body=%q", rr.Body.String())
		}
		// No connection slot may be consumed by a rejected non-member.
		if got := srv.events.SubscriberCount(); got != 0 {
			t.Fatalf("rejected non-member must not subscribe; SubscriberCount=%d, want 0", got)
		}
	})

	// Legitimate member → 200 + connected. Drives the streaming handler
	// under the bounded deadline; the initial connected event is written
	// before the select loop, so it is present by the time the call
	// returns.
	t.Run("member connects with 200 + connected", func(t *testing.T) {
		req, cancel := mkReq(owner, ws.Slug)
		defer cancel()
		rr := httptest.NewRecorder()
		srv.handleSSE(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("member: got %d, want 200; body=%q", rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), "event: connected") {
			t.Fatalf("member should receive the initial connected event; body=%q", rr.Body.String())
		}
	})
}
