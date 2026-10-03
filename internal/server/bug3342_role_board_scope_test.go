package server

import (
	"errors"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3342: the role-board reorder loaded each item with the global
// GetItem and never compared its workspace with the route's, so an item
// from another workspace the caller could edit passed every check. The
// workspace-scoped UPDATE then matched nothing and the answer was 200,
// which also told a caller the id existed somewhere.

func TestBUG3342_RoleBoardReorderRefusesAnotherWorkspacesItem(t *testing.T) {
	srv := testServer(t)
	user, tok := loginTestUserAs(t, srv, "rb3342@example.test", "RB 3342", "pw-3342-abcdefgh")
	wsA := mustCreateOwnedWorkspace(t, srv, "RB Alpha 3342", user)
	wsB := mustCreateOwnedWorkspace(t, srv, "RB Beta 3342", user)
	itemA := mustItem(t, srv, wsA.ID, mustCollection(t, srv, wsA.ID, "Tasks").ID, "a")
	itemB := mustItem(t, srv, wsB.ID, mustCollection(t, srv, wsB.ID, "Tasks").ID, "b")

	path := "/api/v1/workspaces/" + wsA.Slug + "/roles/board/reorder"
	reorder := func(ids ...string) (int, string) {
		body := make([]store.RoleSortUpdate, 0, len(ids))
		for i, id := range ids {
			body = append(body, store.RoleSortUpdate{ItemID: id, RoleSortOrder: 10 + i})
		}
		rr := doRequestWithCookie(srv, http.MethodPut, path, body, tok)
		return rr.Code, rr.Body.String()
	}

	if code, body := reorder(itemA.ID); code != http.StatusOK {
		t.Fatalf("control: reordering the workspace's own item: %d %s", code, body)
	}

	foreignCode, foreignBody := reorder(itemB.ID)
	if foreignCode == http.StatusOK {
		t.Fatalf("another workspace's item was accepted: %d %s", foreignCode, foreignBody)
	}
	missingCode, missingBody := reorder("00000000-0000-0000-0000-000000000000")
	if foreignCode != missingCode || foreignBody != missingBody {
		t.Errorf("another workspace's item answered %d %s, a missing one %d %s; they must match",
			foreignCode, foreignBody, missingCode, missingBody)
	}

	// A batch with one foreign entry writes nothing, its own item included.
	before, err := srv.store.GetItem(itemA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := reorder(itemA.ID, itemB.ID); code == http.StatusOK {
		t.Fatalf("mixed batch accepted: %d", code)
	}
	after, err := srv.store.GetItem(itemA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.RoleSortOrder != before.RoleSortOrder {
		t.Errorf("mixed batch moved the workspace's own item: %d -> %d", before.RoleSortOrder, after.RoleSortOrder)
	}
}

// The store refuses an entry that updates no live row in the workspace,
// and rolls the whole batch back, so the handler cannot answer 200 for a
// write that did nothing (an item deleted after its check, or one from
// another workspace).
func TestBUG3342_UpdateRoleSortOrderCountsRows(t *testing.T) {
	srv := testServer(t)
	user, _ := loginTestUserAs(t, srv, "rbs3342@example.test", "RBS 3342", "pw-3342-abcdefgh")
	wsA := mustCreateOwnedWorkspace(t, srv, "RBS Alpha 3342", user)
	wsB := mustCreateOwnedWorkspace(t, srv, "RBS Beta 3342", user)
	collA := mustCollection(t, srv, wsA.ID, "Tasks")
	itemA := mustItem(t, srv, wsA.ID, collA.ID, "a")
	gone := mustItem(t, srv, wsA.ID, collA.ID, "gone")
	itemB := mustItem(t, srv, wsB.ID, mustCollection(t, srv, wsB.ID, "Tasks").ID, "b")
	if err := srv.store.DeleteItem(gone.ID); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}

	cases := []struct {
		name string
		id   string
	}{
		{"another workspace's item", itemB.ID},
		{"a soft-deleted item", gone.ID},
		{"an id that names nothing", "00000000-0000-0000-0000-000000000000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := srv.store.UpdateRoleSortOrder(wsA.ID, []store.RoleSortUpdate{
				{ItemID: itemA.ID, RoleSortOrder: 41},
				{ItemID: tc.id, RoleSortOrder: 42},
			})
			if !errors.Is(err, store.ErrRoleSortItemNotFound) {
				t.Fatalf("err = %v, want ErrRoleSortItemNotFound", err)
			}
			got, gerr := srv.store.GetItem(itemA.ID)
			if gerr != nil {
				t.Fatal(gerr)
			}
			if got.RoleSortOrder == 41 {
				t.Error("the refused batch still wrote its first entry")
			}
		})
	}
	if err := srv.store.UpdateRoleSortOrder(wsA.ID, []store.RoleSortUpdate{{ItemID: itemA.ID, RoleSortOrder: 7}}); err != nil {
		t.Fatalf("control: %v", err)
	}
}

// Defence in depth: the visibility check itself refuses an item from
// another workspace, even for a caller who may see everything in both.
func TestBUG3342_CheckItemVisibleRequiresTheItemsWorkspace(t *testing.T) {
	srv := testServer(t)
	user, _ := loginTestUserAs(t, srv, "rbv3342@example.test", "RBV 3342", "pw-3342-abcdefgh")
	wsA := mustCreateOwnedWorkspace(t, srv, "RBV Alpha 3342", user)
	wsB := mustCreateOwnedWorkspace(t, srv, "RBV Beta 3342", user)
	itemB := mustItem(t, srv, wsB.ID, mustCollection(t, srv, wsB.ID, "Tasks").ID, "b")

	for _, tc := range []struct {
		name string
		user *models.User
		role string
	}{
		{"owner of both", user, "owner"},
		{"tokenized nil-user owner bypass", nil, "owner"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if ok, err := srv.checkItemVisible(wsB.ID, itemB, tc.user, tc.role, false); err != nil || !ok {
				t.Fatalf("control: own workspace: %v %v", ok, err)
			}
			if ok, err := srv.checkItemVisible(wsA.ID, itemB, tc.user, tc.role, false); err != nil || ok {
				t.Errorf("another workspace's item visible: %v %v", ok, err)
			}
		})
	}
}
