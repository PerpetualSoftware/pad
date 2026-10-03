package server

import (
	"encoding/json"
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

	// Idempotent: limiting again keeps the list as it is.
	if rr := doAuthedJSON(srv, "POST", "/api/v1/connected-apps/bug3338-limit/limit-to-current", nil, tok); rr.Code != http.StatusOK {
		t.Fatalf("second limit: %d %s", rr.Code, rr.Body.String())
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
