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

// TASK-1809: several agents commonly share ONE account. A claim with no
// holder used to default to the account's email, so a second agent's claim
// matched the first one's label and account and silently refreshed its lease:
// both "won" (measured). The default is now the request's agent name, and
// `next` / `ready` leave out items whose live lease a claim by the caller
// would lose.

func (f *leaseFixtureServer) callAs(t *testing.T, token, agent, itemSlug, action string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest("POST",
		"/api/v1/workspaces/"+f.wsSlug+"/items/"+itemSlug+"/"+action,
		bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	if agent != "" {
		req.Header.Set("X-Pad-Agent", agent)
	}
	req.RemoteAddr = "127.0.0.1:0"
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	return rec
}

func leaseHolderOf(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var resp struct {
		Lease models.ItemLease `json:"lease"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse lease: %v (%s)", err, rr.Body.String())
	}
	return resp.Lease.Holder
}

func heldBy(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("parse error: %v (%s)", err, rr.Body.String())
	}
	if env.Error.Code != "lease_held" {
		t.Fatalf("code = %q, want lease_held (%s)", env.Error.Code, rr.Body.String())
	}
	holder, _ := env.Error.Details["holder"].(string)
	return holder
}

func TestTASK1809_TwoAgentsOnOneAccountDoNotShareALease(t *testing.T) {
	t.Parallel()
	f := setupLeaseFixture(t)
	slug := f.item.Slug

	rr := f.callAs(t, f.tokenA, "rook", slug, "claim", map[string]any{})
	if rr.Code != http.StatusOK {
		t.Fatalf("rook claim: %d %s", rr.Code, rr.Body.String())
	}
	if got := leaseHolderOf(t, rr); got != "rook" {
		t.Fatalf("default holder = %q, want the agent name rook", got)
	}

	// The defect: this answered 200, refreshing rook's lease.
	rr = f.callAs(t, f.tokenA, "wren", slug, "claim", map[string]any{})
	if rr.Code != http.StatusConflict {
		t.Fatalf("wren claim on the same account: %d %s, want 409", rr.Code, rr.Body.String())
	}
	if got := heldBy(t, rr); got != "rook" {
		t.Fatalf("409 names %q, want rook", got)
	}

	// Nor may wren end rook's claim.
	rr = f.callAs(t, f.tokenA, "wren", slug, "release", map[string]any{})
	if rr.Code != http.StatusConflict {
		t.Fatalf("wren release of rook's lease: %d %s, want 409", rr.Code, rr.Body.String())
	}

	// A caller with no agent name (a person, or a client that sends none) is
	// a different holder too: the email.
	rr = f.callAs(t, f.tokenA, "", slug, "claim", map[string]any{})
	if rr.Code != http.StatusConflict {
		t.Fatalf("no-agent claim: %d %s, want 409", rr.Code, rr.Body.String())
	}

	// The holder itself still refreshes and releases.
	if rr = f.callAs(t, f.tokenA, "rook", slug, "claim", map[string]any{}); rr.Code != http.StatusOK {
		t.Fatalf("rook re-claim: %d %s", rr.Code, rr.Body.String())
	}
	if rr = f.callAs(t, f.tokenA, "rook", slug, "release", map[string]any{}); rr.Code != http.StatusOK {
		t.Fatalf("rook release: %d %s", rr.Code, rr.Body.String())
	}
	if rr = f.callAs(t, f.tokenA, "wren", slug, "claim", map[string]any{}); rr.Code != http.StatusOK {
		t.Fatalf("wren claim after release: %d %s", rr.Code, rr.Body.String())
	}
}

func TestTASK1809_ExplicitHolderStillWins(t *testing.T) {
	t.Parallel()
	f := setupLeaseFixture(t)
	rr := f.callAs(t, f.tokenA, "rook", f.item.Slug, "claim", map[string]any{"holder": "sweep-runner"})
	if rr.Code != http.StatusOK {
		t.Fatalf("claim: %d %s", rr.Code, rr.Body.String())
	}
	if got := leaseHolderOf(t, rr); got != "sweep-runner" {
		t.Fatalf("holder = %q, want the explicit sweep-runner", got)
	}
}

// next leaves out an item another holder leased, filtered BEFORE the cap of
// three: the leased item outranks the rest (critical vs high), so filtering
// after the cap would answer two entries instead of three.
func TestTASK1809_NextLeavesOutOthersLeasedItems(t *testing.T) {
	t.Parallel()
	f := setupLeaseFixture(t)

	create := func(title, priority string) models.Item {
		t.Helper()
		rr := doRequestWithHeaders(f.srv, "POST", "/api/v1/workspaces/"+f.wsSlug+"/collections/tasks/items",
			map[string]interface{}{"title": title, "fields": `{"status":"open","priority":"` + priority + `"}`},
			map[string]string{"Authorization": "Bearer " + f.tokenA})
		if rr.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", title, rr.Code, rr.Body.String())
		}
		var it models.Item
		parseJSON(t, rr, &it)
		return it
	}
	leased := create("Leased critical", "critical")
	for _, title := range []string{"High one", "High two", "High three"} {
		create(title, "high")
	}

	next := func(token, agent string) []DashboardSuggestion {
		t.Helper()
		headers := map[string]string{"Authorization": "Bearer " + token}
		if agent != "" {
			headers["X-Pad-Agent"] = agent
		}
		rr := doRequestWithHeaders(f.srv, "GET", "/api/v1/workspaces/"+f.wsSlug+"/next", nil, headers)
		if rr.Code != http.StatusOK {
			t.Fatalf("next: %d %s", rr.Code, rr.Body.String())
		}
		var out []DashboardSuggestion
		parseJSON(t, rr, &out)
		return out
	}
	has := func(list []DashboardSuggestion, ref string) bool {
		for _, s := range list {
			if s.ItemRef == ref {
				return true
			}
		}
		return false
	}

	// Premise: unleased, the critical item leads.
	if got := next(f.tokenA, "rook"); len(got) == 0 || got[0].ItemRef != leased.Ref {
		t.Fatalf("premise: %s should lead next before it is leased, got %+v", leased.Ref, got)
	}

	if rr := f.callAs(t, f.tokenA, "wren", leased.Slug, "claim", map[string]any{}); rr.Code != http.StatusOK {
		t.Fatalf("wren claim: %d %s", rr.Code, rr.Body.String())
	}

	// Another agent on the same account: hidden, and the slot refilled.
	got := next(f.tokenA, "rook")
	if has(got, leased.Ref) {
		t.Fatalf("rook's next still offers %s, leased by wren: %+v", leased.Ref, got)
	}
	if len(got) != 3 {
		t.Fatalf("rook's next has %d entries, want 3 (the leased item's slot refilled): %+v", len(got), got)
	}

	// The holder keeps seeing its own claim.
	if !has(next(f.tokenA, "wren"), leased.Ref) {
		t.Fatalf("wren's own next lost %s, which wren holds", leased.Ref)
	}

	// Same label on ANOTHER account is a different holder (BUG-3341).
	if has(next(f.tokenB, "wren"), leased.Ref) {
		t.Fatalf("user B's 'wren' sees user A's wren lease as its own")
	}

	// An expired lease hides nothing.
	if _, err := f.srv.store.ClaimItemLease(leased.ID, "wren", userIDForEmail(t, f.srv, "user-a@example.com"), -time.Minute); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	if !has(next(f.tokenA, "rook"), leased.Ref) {
		t.Fatalf("an expired lease still hides %s", leased.Ref)
	}
}

// The filter's identity is the claim's predicate. With no signed-in user the
// claim compares lease_user_id to NULL, which a NOT(...) spelling turns into
// "not foreign"; a user-bound lease must still read as foreign there. A
// legacy lease with no user is ours on its label alone, as it is to a claim.
func TestTASK1809_ForeignLeasePredicateMatchesTheClaim(t *testing.T) {
	t.Parallel()
	f := setupLeaseFixture(t)
	st := f.srv.store
	ws, err := st.GetWorkspaceBySlug(f.wsSlug)
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	uid := userIDForEmail(t, f.srv, "user-a@example.com")

	if _, err := st.ClaimItemLease(f.item.ID, "rook", uid, time.Hour); err != nil {
		t.Fatalf("claim: %v", err)
	}
	cases := []struct {
		name, holder, user string
		foreign            bool
	}{
		{"same label, same user", "rook", uid, false},
		{"same label, no user", "rook", "", true},
		{"same label, other user", "rook", "someone-else", true},
		{"other label, same user", "wren", uid, true},
	}
	for _, c := range cases {
		ids, err := st.ListForeignItemLeaseIDs(ws.ID, c.holder, c.user)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		_, got := ids[f.item.ID]
		if got != c.foreign {
			t.Errorf("%s: foreign = %v, want %v", c.name, got, c.foreign)
		}
		// Cross-check against the claim itself: foreign iff a claim loses.
		if _, cerr := st.ClaimItemLease(f.item.ID, c.holder, c.user, time.Hour); (cerr != nil) != c.foreign {
			t.Errorf("%s: claim error = %v, but the filter says foreign = %v", c.name, cerr, c.foreign)
		}
	}

	// A legacy lease (no user recorded): ours on its label alone.
	if _, err := st.ReleaseItemLease(f.item.ID, "rook", uid); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := st.ClaimItemLease(f.item.ID, "legacy", "", time.Hour); err != nil {
		t.Fatalf("legacy claim: %v", err)
	}
	if ids, _ := st.ListForeignItemLeaseIDs(ws.ID, "legacy", uid); len(ids) != 0 {
		t.Errorf("a legacy lease on the caller's label reads as foreign")
	}
	if ids, _ := st.ListForeignItemLeaseIDs(ws.ID, "other", uid); len(ids) != 1 {
		t.Errorf("a legacy lease on another label does not read as foreign")
	}
}

func userIDForEmail(t *testing.T, srv *Server, email string) string {
	t.Helper()
	u, err := srv.store.GetUserByEmail(email)
	if err != nil || u == nil {
		t.Fatalf("GetUserByEmail %s: %v", email, err)
	}
	return u.ID
}
