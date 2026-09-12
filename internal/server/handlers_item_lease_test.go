package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Tests for the item claim/release endpoints (#1221):
// POST /api/v1/workspaces/{slug}/items/{itemSlug}/claim and /release.
// The claim is atomic (the store's conditional UPDATE is the arbiter);
// contention answers 409 code=lease_held naming the holder and expiry,
// the same structured-envelope discipline as update_conflict.

type leaseFixtureServer struct {
	srv    *Server
	wsSlug string
	item   models.Item
	tokenA string // user-a@example.com
	tokenB string // user-b@example.com
}

func setupLeaseFixture(t *testing.T) *leaseFixtureServer {
	t.Helper()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items",
		map[string]interface{}{"title": "Contended item", "fields": `{"status":"open"}`})
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed item: %d %s", rr.Code, rr.Body.String())
	}
	var item models.Item
	parseJSON(t, rr, &item)

	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil {
		t.Fatalf("GetWorkspaceBySlug: %v", err)
	}

	mint := func(email string) string {
		t.Helper()
		user, err := srv.store.CreateUser(models.UserCreate{
			Email: email, Name: email, Password: "pw-test-12345",
		})
		if err != nil {
			t.Fatalf("CreateUser %s: %v", email, err)
		}
		if err := srv.store.AddWorkspaceMember(ws.ID, user.ID, "owner"); err != nil {
			t.Fatalf("AddWorkspaceMember: %v", err)
		}
		tok, err := srv.store.CreateAPIToken(user.ID, models.APITokenCreate{
			Name: "lease-test", WorkspaceID: ws.ID,
		}, 0, 0)
		if err != nil {
			t.Fatalf("CreateAPIToken: %v", err)
		}
		return tok.Token
	}

	return &leaseFixtureServer{
		srv:    srv,
		wsSlug: slug,
		item:   item,
		tokenA: mint("user-a@example.com"),
		tokenB: mint("user-b@example.com"),
	}
}

func (f *leaseFixtureServer) call(t *testing.T, token, action string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest("POST",
		"/api/v1/workspaces/"+f.wsSlug+"/items/"+f.item.Slug+"/"+action,
		bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.RemoteAddr = "127.0.0.1:0"
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	return rec
}

// Claim → contended claim → release → re-claim: the full lifecycle, with
// the 409 envelope naming the live holder and expiry.
func TestItemLease_ClaimContendReleaseReclaim(t *testing.T) {
	f := setupLeaseFixture(t)

	// A claims with an explicit holder string.
	rr := f.call(t, f.tokenA, "claim", map[string]any{"holder": "sweep-runner", "ttl_seconds": 600})
	if rr.Code != http.StatusOK {
		t.Fatalf("claim: expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	var claimResp struct {
		Ref   string           `json:"ref"`
		Lease models.ItemLease `json:"lease"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &claimResp); err != nil {
		t.Fatalf("parse claim response: %v", err)
	}
	if claimResp.Ref != f.item.Ref {
		t.Errorf("ref = %q, want %q", claimResp.Ref, f.item.Ref)
	}
	if claimResp.Lease.Holder != "sweep-runner" {
		t.Errorf("holder = %q, want sweep-runner", claimResp.Lease.Holder)
	}
	if !claimResp.Lease.ExpiresAt.After(time.Now().UTC().Add(9 * time.Minute)) {
		t.Errorf("expiry %v not ~10m out", claimResp.Lease.ExpiresAt)
	}

	// B's claim while A's is live: 409 lease_held naming holder + expiry.
	rr = f.call(t, f.tokenB, "claim", map[string]any{"holder": "other-runner"})
	if rr.Code != http.StatusConflict {
		t.Fatalf("contended claim: expected 409, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	var conflict struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &conflict); err != nil {
		t.Fatalf("parse conflict: %v", err)
	}
	if conflict.Error.Code != "lease_held" {
		t.Errorf("code = %q, want lease_held", conflict.Error.Code)
	}
	if conflict.Error.Details["holder"] != "sweep-runner" {
		t.Errorf("details.holder = %v, want sweep-runner", conflict.Error.Details["holder"])
	}
	if conflict.Error.Details["expires_at"] == nil {
		t.Error("details.expires_at missing — the caller can't decide to wait or skip without it")
	}

	// The holder releases; released=true.
	rr = f.call(t, f.tokenA, "release", map[string]any{"holder": "sweep-runner"})
	if rr.Code != http.StatusOK {
		t.Fatalf("release: expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	var relResp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &relResp); err != nil {
		t.Fatalf("parse release response: %v", err)
	}
	if relResp["released"] != true {
		t.Errorf("released = %v, want true", relResp["released"])
	}

	// B claims successfully now.
	rr = f.call(t, f.tokenB, "claim", map[string]any{"holder": "other-runner"})
	if rr.Code != http.StatusOK {
		t.Fatalf("post-release claim: expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}

// An empty holder defaults to the authenticated user's email — the #879
// tie-in: the identity the token carries is the identity the lease
// records unless the caller says otherwise.
func TestItemLease_DefaultHolderIsAuthenticatedIdentity(t *testing.T) {
	f := setupLeaseFixture(t)

	rr := f.call(t, f.tokenA, "claim", map[string]any{})
	if rr.Code != http.StatusOK {
		t.Fatalf("claim: expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	var resp struct {
		Lease models.ItemLease `json:"lease"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.Lease.Holder != "user-a@example.com" {
		t.Errorf("default holder = %q, want the authenticated user's email", resp.Lease.Holder)
	}
}

// ttl_seconds outside (0, 24h] is refused with 400 — a zero body means
// the 15-minute default, not an instant or eternal lease.
func TestItemLease_TTLBounds(t *testing.T) {
	f := setupLeaseFixture(t)

	for _, ttl := range []int{-5, 86401} {
		rr := f.call(t, f.tokenA, "claim", map[string]any{"ttl_seconds": ttl})
		if rr.Code != http.StatusBadRequest {
			t.Errorf("ttl_seconds=%d: expected 400, got %d (body: %s)", ttl, rr.Code, rr.Body.String())
		}
	}

	// Default: no ttl_seconds → ~15 minutes.
	rr := f.call(t, f.tokenA, "claim", map[string]any{})
	if rr.Code != http.StatusOK {
		t.Fatalf("default-ttl claim: expected 200, got %d", rr.Code)
	}
	var resp struct {
		Lease models.ItemLease `json:"lease"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	until := time.Until(resp.Lease.ExpiresAt)
	if until < 14*time.Minute || until > 16*time.Minute {
		t.Errorf("default TTL = %v, want ~15m", until)
	}
}

// Claiming requires authentication — a lease with no accountable identity
// behind it is exactly the out-of-band lock the feature replaces. An
// unauthenticated cookie-less POST is refused upstream of the handler
// (the CSRF middleware answers 403 before auth resolves), so accept
// either refusal shape; the invariant is that NO lease gets written.
func TestItemLease_ClaimRequiresAuth(t *testing.T) {
	f := setupLeaseFixture(t)

	rr := f.call(t, "", "claim", map[string]any{})
	if rr.Code != http.StatusUnauthorized && rr.Code != http.StatusForbidden {
		t.Errorf("unauthenticated claim: expected 401/403, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	lease, err := f.srv.store.GetItemLease(f.item.ID)
	if err != nil {
		t.Fatalf("GetItemLease: %v", err)
	}
	if lease != nil {
		t.Errorf("unauthenticated claim wrote a lease: %+v", lease)
	}
}

// The single-item GET carries the live lease and omits it once expired —
// expiry is absence on the read path, with no reaper involved.
func TestItemLease_GetItemCarriesLiveLeaseOmitsExpired(t *testing.T) {
	f := setupLeaseFixture(t)

	get := func() map[string]any {
		t.Helper()
		req := httptest.NewRequest("GET", "/api/v1/workspaces/"+f.wsSlug+"/items/"+f.item.Slug, nil)
		req.Header.Set("Authorization", "Bearer "+f.tokenA)
		req.RemoteAddr = "127.0.0.1:0"
		rec := httptest.NewRecorder()
		f.srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("get item: %d %s", rec.Code, rec.Body.String())
		}
		var m map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatalf("parse item: %v", err)
		}
		return m
	}

	if _, present := get()["lease"]; present {
		t.Error("unclaimed item must not carry a lease key")
	}

	if rr := f.call(t, f.tokenA, "claim", map[string]any{"holder": "runner"}); rr.Code != http.StatusOK {
		t.Fatalf("claim: %d", rr.Code)
	}
	leaseVal, present := get()["lease"]
	if !present {
		t.Fatal("live lease missing from item GET")
	}
	leaseMap, _ := leaseVal.(map[string]any)
	if leaseMap["holder"] != "runner" {
		t.Errorf("lease.holder = %v, want runner", leaseMap["holder"])
	}

	// Expire it directly at the store (a crashed holder), then re-read:
	// the key must be gone with no reaper having run.
	if _, err := f.srv.store.ClaimItemLease(f.item.ID, "runner", -2*time.Second); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	if _, present := get()["lease"]; present {
		t.Error("expired lease leaked into the item GET")
	}
}

// The workspace item list decorates leased items and only leased items.
func TestItemLease_ListDecoratesLeasedItems(t *testing.T) {
	f := setupLeaseFixture(t)

	if rr := f.call(t, f.tokenA, "claim", map[string]any{"holder": "runner"}); rr.Code != http.StatusOK {
		t.Fatalf("claim: %d", rr.Code)
	}

	req := httptest.NewRequest("GET", "/api/v1/workspaces/"+f.wsSlug+"/items", nil)
	req.Header.Set("Authorization", "Bearer "+f.tokenA)
	req.RemoteAddr = "127.0.0.1:0"
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list items: %d %s", rec.Code, rec.Body.String())
	}
	var items []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil {
		t.Fatalf("parse list: %v", err)
	}
	found := false
	for _, it := range items {
		if it["id"] == f.item.ID {
			lease, present := it["lease"].(map[string]any)
			if !present {
				t.Fatal("leased item's list row carries no lease")
			}
			if lease["holder"] != "runner" {
				t.Errorf("list lease.holder = %v, want runner", lease["holder"])
			}
			found = true
		} else if _, present := it["lease"]; present {
			t.Errorf("unleased item %v carries a lease key", it["id"])
		}
	}
	if !found {
		t.Fatal("seeded item missing from the list")
	}
}
