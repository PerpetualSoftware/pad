package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestPushDeliverySymmetry_CountAgreesWithWhatTheStreamReceives is the
// end-to-end statement of what BUG-2725 is actually about: the count and
// the delivery must not disagree.
//
// Every other test in this unit asserts the COUNT, with delivery's
// predicate reproduced rather than run. That leaves the claim
// "delivered_sessions now matches what delivery does" resting on the two
// call sites applying the same rule — which is an argument, not a
// measurement, and a mutation that makes the STREAM read a constant
// transport instead of its own connection's survives all of them.
//
// So this drives a REAL armed stream, opened over a bearer token, and
// checks both directions against it:
//
//   - a push to an item in a workspace the admin does not belong to is
//     counted 0 AND never arrives;
//   - a push to an item in a workspace the admin DOES belong to is
//     counted 1 AND arrives on that same stream.
//
// One stream serves both because /api/v1/events/stream is user-scoped
// and spans every workspace the caller can see. The second leg is the
// control: without it, a stream that was simply dead — never armed,
// never registered, disconnected — would satisfy the first leg's
// non-arrival and prove nothing.
func TestPushDeliverySymmetry_CountAgreesWithWhatTheStreamReceives(t *testing.T) {
	srv := testServerWithWatchEvents(t)
	srv.SetSessionPresence(NewMemorySessionPresence())

	// Fresh-install window: both workspaces and both items first.
	outsideSlug := createWSWithCollections(t, srv)
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+outsideSlug+"/collections/tasks/items",
		map[string]interface{}{"title": "Outside", "fields": `{"status":"open"}`})
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed outside item: %d %s", rr.Code, rr.Body.String())
	}
	var outsideItem models.Item
	parseJSON(t, rr, &outsideItem)

	memberSlug := createWSWithCollections(t, srv)
	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+memberSlug+"/collections/tasks/items",
		map[string]interface{}{"title": "Inside", "fields": `{"status":"open"}`})
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed member item: %d %s", rr.Code, rr.Body.String())
	}
	var memberItem models.Item
	parseJSON(t, rr, &memberItem)

	adminSessionToken := bootstrapFirstUser(t, srv, "sym-admin@example.com", "Admin")
	admin, err := srv.store.GetUserByEmail("sym-admin@example.com")
	if err != nil || admin == nil {
		t.Fatalf("resolve admin: %v", err)
	}
	if admin.Role != "admin" {
		t.Fatalf("fixture premise broken: expected a platform admin, got role %q", admin.Role)
	}

	memberWS, err := srv.store.GetWorkspaceBySlug(memberSlug)
	if err != nil {
		t.Fatalf("GetWorkspaceBySlug(member): %v", err)
	}
	if err := srv.store.AddWorkspaceMember(memberWS.ID, admin.ID, "owner"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}

	// Premise, asserted: member of one workspace, not of the other. The
	// whole test is the difference between them.
	outsideWS, err := srv.store.GetWorkspaceBySlug(outsideSlug)
	if err != nil {
		t.Fatalf("GetWorkspaceBySlug(outside): %v", err)
	}
	if m, err := srv.store.GetWorkspaceMember(outsideWS.ID, admin.ID); err != nil || m != nil {
		t.Fatalf("fixture premise broken: admin must not be a member of %s (err=%v)", outsideSlug, err)
	}

	// The admin's BEARER-opened armed stream. Bearer is what denies it the
	// cookie-only admin bypass, which is the entire mechanism under test.
	adminTok, err := srv.store.CreateAPIToken(admin.ID, models.APITokenCreate{Name: "sym-stream"}, 0, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := connectArmedWatchStream(ctx, t, ts.URL, adminTok.Token)
	waitForWatchEvent(t, ch, 3*time.Second) // connected

	// Wait for the stream to actually be in the registry; otherwise the
	// counts below race the handler's Add and this test measures its own
	// timing rather than the predicate.
	deadline := time.Now().Add(3 * time.Second)
	for {
		sessions, err := srv.sessionPresence.ListForUser(admin.ID)
		if err != nil {
			t.Fatalf("ListForUser: %v", err)
		}
		if len(sessions) == 1 {
			if !sessions[0].BearerAuth {
				t.Fatal("fixture premise broken: the stream was opened with a bearer token but " +
					"the registry recorded BearerAuth=false")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stream never registered: %d sessions", len(sessions))
		}
		time.Sleep(10 * time.Millisecond)
	}

	countOf := func(t *testing.T, slug, itemSlug, message string) int {
		t.Helper()
		rr := cookiePush(t, srv, slug, itemSlug, adminSessionToken,
			map[string]interface{}{"message": message})
		if rr.Code != http.StatusOK {
			t.Fatalf("push %s: %d %s", itemSlug, rr.Code, rr.Body.String())
		}
		var resp pushResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp.DeliveredSessions == nil {
			t.Fatal("expected a delivered_sessions count")
		}
		return *resp.DeliveredSessions
	}

	// LEG 1 — invisible item: counted 0, and must not arrive.
	if got := countOf(t, outsideSlug, outsideItem.Slug, "invisible push"); got != 0 {
		t.Fatalf("delivered_sessions = %d for an item the stream cannot see, want 0", got)
	}

	// LEG 2 — visible item: counted 1, and must arrive. Issued BEFORE
	// draining, so leg 1's absence is checked against a stream that has
	// demonstrably kept working rather than against a silent one.
	if got := countOf(t, memberSlug, memberItem.Slug, "visible push"); got != 1 {
		t.Fatalf("delivered_sessions = %d for an item the stream CAN see, want 1", got)
	}

	// DISCRIMINATE ON WORKSPACE, NOT ON REF. Item refs are per-workspace
	// sequences, so the first item of each of these two workspaces is
	// TASK-1 — an assertion on ref alone cannot tell the two pushes
	// apart and passes whichever one arrives. That is not a hypothetical:
	// the first version of this test asserted on ref, failed, and the
	// failure was the fixture's, not the code's. The workspace slug and
	// the message are what actually distinguish them.
	ev := waitForWatchEvent(t, ch, 5*time.Second)
	var payload struct {
		Workspace string `json:"workspace"`
		ItemRef   string `json:"item_ref"`
		Summary   string `json:"summary"`
	}
	if err := json.Unmarshal([]byte(ev.Data), &payload); err != nil {
		t.Fatalf("parse stream payload %q: %v", ev.Data, err)
	}
	if payload.Workspace == outsideSlug || payload.Summary == "invisible push" {
		t.Fatalf("the invisible push REACHED the stream (%s/%s %q) — the count said 0 and delivery "+
			"said otherwise, which is the same count/delivery disagreement BUG-2725 exists to "+
			"remove, in the opposite direction",
			payload.Workspace, payload.ItemRef, payload.Summary)
	}
	if payload.Workspace != memberSlug || payload.Summary != "visible push" {
		t.Fatalf("expected the visible push from %s, got %s/%s %q",
			memberSlug, payload.Workspace, payload.ItemRef, payload.Summary)
	}
}

// TestPushSessionVisibility_MissingUserIsNotVisible covers the
// resolver's "definitively gone" branch: a user who does not exist
// resolves to not-visible, with NO error.
//
// It is tested here rather than through the handler because the branch
// is not reachable that way — RequireAuth rejects a deleted or disabled
// caller long before handlePushToItem runs. Defence in depth, pinned
// because the distinction it sits next to is the one codex round 1
// caught: absence is an answer, unreadability is not.
func TestPushSessionVisibility_MissingUserIsNotVisible(t *testing.T) {
	t.Parallel()
	srv := testServerWithWatchEvents(t)
	slug := createWSWithCollections(t, srv)
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil {
		t.Fatalf("GetWorkspaceBySlug: %v", err)
	}

	vis := srv.pushSessionVisibility("no-such-user-id", ws.ID, "some-collection", "some-item")
	for _, bearer := range []bool{true, false} {
		visible, err := vis.allows(bearer)
		if err != nil {
			t.Fatalf("bearerAuth=%v: a user who is definitively absent is an ANSWER, not an "+
				"unreadable store — it must not surface as an error: %v", bearer, err)
		}
		if visible {
			t.Fatalf("bearerAuth=%v: a nonexistent user resolved to VISIBLE; their streams "+
				"cannot receive anything, so they must not be counted", bearer)
		}
	}
}

// TestDeliveredSessionCount_PropagatesVisibilityError is the codex
// round-1 P1 regression, and it FAILS against the first version of this
// fix, which swallowed the error and returned false.
//
// Why it matters more than it looks: deliveredSessionCount's 0 is
// load-bearing. handlePushToItem skips the publish for a TARGETED push
// when the count is 0, so a store blip resolving to "not visible" would
// drop the instruction and answer 200 — precisely the defect BUG-2698
// filed, arriving a second time through BUG-2725's fix. The count must
// say "I could not find out", which the existing registry-unreadable
// branch already turns into a 503.
//
// The control leg is the same call with a resolver that succeeds: it
// must count normally, so the propagation cannot be satisfied by a
// version that simply errors always.
func TestDeliveredSessionCount_PropagatesVisibilityError(t *testing.T) {
	t.Parallel()

	presence := NewMemorySessionPresence()
	presence.Add("u1", SessionIdentity{Armed: true}, SessionOrigin{BearerAuth: true})
	presence.Add("u1", SessionIdentity{Armed: true}, SessionOrigin{BearerAuth: false})

	boom := errors.New("store unavailable")
	failing := &sessionVisibility{resolve: func(bool) (bool, error) { return false, boom }}

	count, err := deliveredSessionCount(presence, "u1", "", failing)
	if !errors.Is(err, boom) {
		t.Fatalf("expected the visibility error to propagate, got count=%d err=%v. Swallowing it "+
			"into a 0 makes a targeted push skip its publish and report success", count, err)
	}
	if count != 0 {
		t.Fatalf("an errored count must not also report a number, got %d", count)
	}

	working := &sessionVisibility{resolve: func(bool) (bool, error) { return true, nil }}
	count, err = deliveredSessionCount(presence, "u1", "", working)
	if err != nil {
		t.Fatalf("control leg: a succeeding resolver must not error: %v", err)
	}
	if count != 2 {
		t.Fatalf("control leg: expected both sessions counted, got %d — a fix that errors "+
			"unconditionally would satisfy the leg above and break every push", count)
	}
}
