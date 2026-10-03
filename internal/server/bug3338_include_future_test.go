package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"sort"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3338: include_future_workspaces never narrowed anything. The
// wildcard (all_current_workspaces) is live and covers workspaces joined
// later, and nothing auto-added on the flag, so the Connected Apps
// checkbox promised something false either way. It is no longer a
// control, and "only my current workspaces" is an explicit action that
// copies the user's memberships into the list.

func storedIncludeFuture(t *testing.T, srv *Server, requestID string) bool {
	t.Helper()
	c, err := srv.store.GetOAuthConnection(requestID)
	if err != nil || c == nil {
		t.Fatalf("GetOAuthConnection(%s): %v", requestID, err)
	}
	return c.IncludeFutureWorkspaces
}

func TestBUG3338_FlagsPatchIgnoresIncludeFuture(t *testing.T) {
	srv, _ := connectedAppsTestServer(t)
	_, tok, _ := seedConnectedAppForMutations(t, srv, "bug3338-flags")

	cases := []struct {
		name                   string
		allCurrent, sentFuture bool
	}{
		// The combination the checkbox used to store: wildcard on,
		// "future" off. It now follows the wildcard.
		{"wildcard on, future sent off", true, false},
		{"wildcard off, future sent on", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := doAuthedJSON(srv, "PATCH", "/api/v1/connected-apps/bug3338-flags/flags",
				map[string]bool{
					"may_create_workspaces":     true,
					"all_current_workspaces":    tc.allCurrent,
					"include_future_workspaces": tc.sentFuture,
				}, tok)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d (body=%s)", rr.Code, rr.Body.String())
			}
			if got := storedIncludeFuture(t, srv, "bug3338-flags"); got != tc.allCurrent {
				t.Errorf("stored include_future = %v, want it to follow all_current (%v)", got, tc.allCurrent)
			}
		})
	}
}

func TestBUG3338_LimitToCurrentSnapshotsMemberships(t *testing.T) {
	srv, _ := connectedAppsTestServer(t)
	user, tok := loginTestUser(t, srv)
	if err := srv.store.CreateOAuthConnection(store.OAuthConnection{
		RequestID:               "bug3338-limit",
		UserID:                  user.ID,
		MayCreateWorkspaces:     true,
		AllCurrentWorkspaces:    true,
		IncludeFutureWorkspaces: false,
	}); err != nil {
		t.Fatalf("CreateOAuthConnection: %v", err)
	}
	_, slugA := mustSeedWorkspaceForMutation(t, srv, user.ID, "bug3338-a", "owner")
	_, slugB := mustSeedWorkspaceForMutation(t, srv, user.ID, "bug3338-b", "editor")
	_, gone := mustSeedWorkspaceForMutation(t, srv, user.ID, "bug3338-gone", "owner")
	if err := srv.store.DeleteWorkspace(gone); err != nil {
		t.Fatalf("DeleteWorkspace: %v", err)
	}

	// Before: the wildcard grants a workspace joined later, whatever the
	// stored "future" flag says. That is the defect the checkbox hid.
	if access, err := srv.store.GetOAuthConnectionAccess("bug3338-limit"); err != nil || !access.AllCurrentWorkspaces {
		t.Fatalf("control: wildcard connection: %+v %v", access, err)
	}

	rr := doAuthedJSON(srv, "POST", "/api/v1/connected-apps/bug3338-limit/limit-to-current", nil, tok)
	if rr.Code != http.StatusOK {
		t.Fatalf("limit-to-current: %d %s", rr.Code, rr.Body.String())
	}
	var dto connectedAppDTO
	_ = json.Unmarshal(rr.Body.Bytes(), &dto)
	if dto.AllCurrentWorkspaces {
		t.Error("response still reads all_current_workspaces=true")
	}

	want := []string{slugA, slugB}
	sort.Strings(want)
	access, err := srv.store.GetOAuthConnectionAccess("bug3338-limit")
	if err != nil {
		t.Fatal(err)
	}
	if access.AllCurrentWorkspaces || !reflect.DeepEqual(access.WorkspaceSlugs, want) {
		t.Fatalf("after limit: %+v, want specific %v (the soft-deleted workspace left out)", access, want)
	}
	if storedIncludeFuture(t, srv, "bug3338-limit") {
		t.Error("include_future left on after limiting")
	}

	// A workspace joined afterwards is not covered.
	mustSeedWorkspaceForMutation(t, srv, user.ID, "bug3338-later", "owner")
	access, err = srv.store.GetOAuthConnectionAccess("bug3338-limit")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(access.WorkspaceSlugs, want) {
		t.Errorf("a workspace joined after the limit is covered: %v", access.WorkspaceSlugs)
	}

	// A second press (a stale tab) on the now-specific list must not add
	// the workspace joined since the first one.
	if rr := doAuthedJSON(srv, "POST", "/api/v1/connected-apps/bug3338-limit/limit-to-current", nil, tok); rr.Code != http.StatusOK {
		t.Fatalf("second limit: %d %s", rr.Code, rr.Body.String())
	}
	access, err = srv.store.GetOAuthConnectionAccess("bug3338-limit")
	if err != nil {
		t.Fatal(err)
	}
	if access.AllCurrentWorkspaces || !reflect.DeepEqual(access.WorkspaceSlugs, want) {
		t.Errorf("a repeated limit widened the list: %+v, want %v", access, want)
	}
}

// The snapshot is what the wildcard reaches today, which includes a
// workspace the user reaches only as a guest through a grant.
func TestBUG3338_LimitToCurrentKeepsGuestWorkspaces(t *testing.T) {
	srv, _ := connectedAppsTestServer(t)
	user, tok := loginTestUser(t, srv)
	owner, _ := loginTestUserAs(t, srv, "owner3338@example.test", "Owner", "pw-3338-abcdefgh")
	if err := srv.store.CreateOAuthConnection(store.OAuthConnection{
		RequestID:            "bug3338-guest",
		UserID:               user.ID,
		AllCurrentWorkspaces: true,
	}); err != nil {
		t.Fatalf("CreateOAuthConnection: %v", err)
	}
	_, mine := mustSeedWorkspaceForMutation(t, srv, user.ID, "bug3338-mine", "owner")
	itemWSID, itemWS := mustSeedWorkspaceForMutation(t, srv, owner.ID, "bug3338-item-guest", "owner")
	collWSID, collWS := mustSeedWorkspaceForMutation(t, srv, owner.ID, "bug3338-coll-guest", "owner")
	it := mustItem(t, srv, itemWSID, mustCollection(t, srv, itemWSID, "Tasks").ID, "shared")
	if _, err := srv.store.CreateItemGrant(itemWSID, it.ID, user.ID, "view", owner.ID); err != nil {
		t.Fatalf("CreateItemGrant: %v", err)
	}
	if _, err := srv.store.CreateCollectionGrant(collWSID, mustCollection(t, srv, collWSID, "Docs").ID, user.ID, "view", owner.ID); err != nil {
		t.Fatalf("CreateCollectionGrant: %v", err)
	}
	mustSeedWorkspaceForMutation(t, srv, owner.ID, "bug3338-unrelated", "owner")

	if rr := doAuthedJSON(srv, "POST", "/api/v1/connected-apps/bug3338-guest/limit-to-current", nil, tok); rr.Code != http.StatusOK {
		t.Fatalf("limit-to-current: %d %s", rr.Code, rr.Body.String())
	}
	access, err := srv.store.GetOAuthConnectionAccess("bug3338-guest")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{mine, itemWS, collWS}
	sort.Strings(want)
	if !reflect.DeepEqual(access.WorkspaceSlugs, want) {
		t.Errorf("limited to %v, want %v (member + both guest workspaces, not the unrelated one)", access.WorkspaceSlugs, want)
	}
}

// The empty-list rule on removal and on turning the wildcard off is
// enforced by the store under the connection's lock.
func TestBUG3338_GuardedWritesRefuseAnEmptyList(t *testing.T) {
	srv, _ := connectedAppsTestServer(t)
	user, _ := loginTestUser(t, srv)
	wsID, _ := mustSeedWorkspaceForMutation(t, srv, user.ID, "bug3338-only", "owner")
	if err := srv.store.CreateOAuthConnection(store.OAuthConnection{RequestID: "bug3338-guard", UserID: user.ID}); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.AddConnectionWorkspace("bug3338-guard", wsID, store.AddedByUser); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.RemoveConnectionWorkspaceUnlessLast("bug3338-guard", wsID); !errors.Is(err, store.ErrLastConnectionWorkspace) {
		t.Fatalf("removing the last workspace: err = %v", err)
	}
	if n, _ := srv.store.ConnectionWorkspaceCount("bug3338-guard"); n != 1 {
		t.Errorf("refused removal deleted the row: count %d", n)
	}
	// Under the wildcard the last row may go, and then the wildcard may
	// not be turned off.
	if err := srv.store.SetScopeFlagsGuarded("bug3338-guard", true, true, true); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.RemoveConnectionWorkspaceUnlessLast("bug3338-guard", wsID); err != nil {
		t.Fatalf("removal under the wildcard: %v", err)
	}
	if err := srv.store.SetScopeFlagsGuarded("bug3338-guard", true, false, false); !errors.Is(err, store.ErrLastConnectionWorkspace) {
		t.Fatalf("turning the wildcard off with an empty list: err = %v", err)
	}
	if a, _ := srv.store.GetOAuthConnectionAccess("bug3338-guard"); !a.AllCurrentWorkspaces {
		t.Error("refused flag change still turned the wildcard off")
	}
}

func TestBUG3338_LimitToCurrentRefusals(t *testing.T) {
	srv, _ := connectedAppsTestServer(t)
	user, tok := loginTestUser(t, srv)
	if err := srv.store.CreateOAuthConnection(store.OAuthConnection{
		RequestID:            "bug3338-empty",
		UserID:               user.ID,
		AllCurrentWorkspaces: true,
	}); err != nil {
		t.Fatalf("CreateOAuthConnection: %v", err)
	}

	t.Run("no memberships: refused, still a wildcard", func(t *testing.T) {
		rr := doAuthedJSON(srv, "POST", "/api/v1/connected-apps/bug3338-empty/limit-to-current", nil, tok)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body=%s)", rr.Code, rr.Body.String())
		}
		access, err := srv.store.GetOAuthConnectionAccess("bug3338-empty")
		if err != nil || !access.AllCurrentWorkspaces {
			t.Errorf("a refused limit changed the connection: %+v %v", access, err)
		}
	})

	t.Run("another user's connection: 404, untouched", func(t *testing.T) {
		_, otherTok := loginTestUserAs(t, srv, "other3338@example.test", "Other", "pw-3338-abcdefgh")
		rr := doAuthedJSON(srv, "POST", "/api/v1/connected-apps/bug3338-empty/limit-to-current", nil, otherTok)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (body=%s)", rr.Code, rr.Body.String())
		}
		access, err := srv.store.GetOAuthConnectionAccess("bug3338-empty")
		if err != nil || !access.AllCurrentWorkspaces {
			t.Errorf("another user's call changed the connection: %+v %v", access, err)
		}
	})
}
