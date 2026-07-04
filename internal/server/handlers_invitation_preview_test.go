package server

// BUG-1934 — GET /api/v1/invitations/{code}/preview is a non-consuming,
// public (pre-auth), always-200, rate-limited endpoint that lets the /join
// page prefill the invited email read-only and pick register-vs-login mode.
// These tests pin the load-bearing properties: it never accepts the invite,
// it never 404s (enumeration safety), and it's wired to a rate limiter.

import (
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// previewResp mirrors the handler's success and not-found shapes.
type previewResp struct {
	Found         bool   `json:"found"`
	Email         string `json:"email"`
	WorkspaceName string `json:"workspace_name"`
	HasAccount    bool   `json:"has_account"`
}

const previewBadCode = "deadbeefdeadbeefdeadbeefdeadbeef" // 32 hex chars, never issued

// seedOwnerWorkspace bootstraps the first admin (so RequireAuth is active and
// users exist) and returns the owner plus a fresh workspace they own.
func seedOwnerWorkspace(t *testing.T, srv *Server, wsName string) (*models.User, *models.Workspace) {
	t.Helper()
	bootstrapFirstUser(t, srv, "owner@example.com", "Owner")
	owner, err := srv.store.GetUserByEmail("owner@example.com")
	if err != nil || owner == nil {
		t.Fatalf("get owner: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: wsName, OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	return owner, ws
}

// TestPreviewInvitation_ReturnsMetadataWithoutConsuming is the canonical
// happy path: an unauthenticated caller gets the invited email + workspace
// name, and the invitation stays pending afterward.
func TestPreviewInvitation_ReturnsMetadataWithoutConsuming(t *testing.T) {
	srv := testServer(t)
	owner, ws := seedOwnerWorkspace(t, srv, "Acme HQ")

	inv, err := srv.store.CreateInvitation(ws.ID, "invitee@example.com", "editor", owner.ID)
	if err != nil {
		t.Fatalf("create invitation: %v", err)
	}

	// Unauthenticated (no session/token), non-loopback IP.
	rr := doRequest(srv, "GET", "/api/v1/invitations/"+inv.Code+"/preview", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("preview: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp previewResp
	parseJSON(t, rr, &resp)
	if !resp.Found {
		t.Fatalf("expected found=true, got %+v", resp)
	}
	if resp.Email != "invitee@example.com" {
		t.Errorf("expected email invitee@example.com, got %q", resp.Email)
	}
	if resp.WorkspaceName != "Acme HQ" {
		t.Errorf("expected workspace_name 'Acme HQ', got %q", resp.WorkspaceName)
	}
	if resp.HasAccount {
		t.Errorf("expected has_account=false (no account for invitee), got true")
	}

	// Non-consuming: the invitation must still be pending and resolvable.
	still, err := srv.store.GetInvitationByCode(inv.Code)
	if err != nil {
		t.Fatalf("re-lookup invitation: %v", err)
	}
	if still == nil {
		t.Fatal("preview consumed the invitation (GetInvitationByCode now returns nil)")
	}
	if still.AcceptedAt != nil {
		t.Fatal("preview marked the invitation as accepted")
	}
}

// TestPreviewInvitation_HasAccountTrue flips has_account when an account
// already exists for the invited address — the signal the /join page uses to
// default to login mode.
func TestPreviewInvitation_HasAccountTrue(t *testing.T) {
	srv := testServer(t)
	owner, ws := seedOwnerWorkspace(t, srv, "Beta")

	if _, err := srv.store.CreateUser(models.UserCreate{
		Email:    "member@example.com",
		Name:     "Member",
		Password: "correct-horse-battery-staple",
	}); err != nil {
		t.Fatalf("create existing user: %v", err)
	}
	inv, err := srv.store.CreateInvitation(ws.ID, "member@example.com", "editor", owner.ID)
	if err != nil {
		t.Fatalf("create invitation: %v", err)
	}

	rr := doRequest(srv, "GET", "/api/v1/invitations/"+inv.Code+"/preview", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("preview: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp previewResp
	parseJSON(t, rr, &resp)
	if !resp.Found || !resp.HasAccount {
		t.Fatalf("expected found=true & has_account=true, got %+v", resp)
	}
}

// TestPreviewInvitation_UnknownCodeIsFoundFalse pins enumeration safety: an
// unknown code returns 200 with {found:false} and leaks no email — the same
// shape a valid-but-expired or deleted-workspace code would return, so the
// response can't be used to probe which codes are real.
func TestPreviewInvitation_UnknownCodeIsFoundFalse(t *testing.T) {
	srv := testServer(t)
	// Users exist (RequireAuth active) — the endpoint must still be public.
	bootstrapFirstUser(t, srv, "owner@example.com", "Owner")

	rr := doRequest(srv, "GET", "/api/v1/invitations/"+previewBadCode+"/preview", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("unknown code: expected 200 (never 404), got %d: %s", rr.Code, rr.Body.String())
	}
	var resp previewResp
	parseJSON(t, rr, &resp)
	if resp.Found {
		t.Errorf("expected found=false for unknown code, got %+v", resp)
	}
	if resp.Email != "" {
		t.Errorf("unknown code leaked an email: %q", resp.Email)
	}
}

// TestPreviewInvitation_RateLimited proves the endpoint is wired to its
// dedicated per-IP limiter (burst 20) so it can't be used to enumerate the
// code space at speed.
func TestPreviewInvitation_RateLimited(t *testing.T) {
	srv := testServer(t)
	const ip = "203.0.113.55:1"

	got429 := false
	for i := 0; i < 25; i++ {
		rr := doRequestFromRemoteAddr(srv, "GET", "/api/v1/invitations/"+previewBadCode+"/preview", nil, ip)
		if rr.Code == http.StatusTooManyRequests {
			got429 = true
			break
		}
		if rr.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200 or 429, got %d: %s", i, rr.Code, rr.Body.String())
		}
	}
	if !got429 {
		t.Fatal("expected the preview endpoint to rate-limit after its burst is exhausted")
	}
}
