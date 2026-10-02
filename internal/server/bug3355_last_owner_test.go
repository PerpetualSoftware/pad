package server

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3355: a workspace must keep at least one owner, and its CANONICAL owner
// (workspaces.owner_id, which sets its URL, restore rights and plan limits)
// cannot be demoted or removed through the member doors: that would split
// authority from ownership. Moving owner_id is a transfer, a separate unit.

func (f *accessFixture) membersPath(userID string) string {
	return "/api/v1/workspaces/" + f.wsSlug + "/members/" + userID
}

func (f *accessFixture) roleOf(userID string) string {
	f.t.Helper()
	m, err := f.srv.store.GetWorkspaceMember(f.wsID, userID)
	if err != nil {
		f.t.Fatalf("GetWorkspaceMember: %v", err)
	}
	if m == nil {
		return ""
	}
	return m.Role
}

func TestBUG3355_RoleMustBeKnown(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newAccessFixture(t, d)
		ed := f.member("ed@example.com", "editor")
		rr := f.do("PATCH", f.membersPath(ed.ID), f.ownerTok, map[string]any{"role": "superuser"})
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("unknown role: got %d %s, want 400", rr.Code, rr.Body.String())
		}
		if got := f.roleOf(ed.ID); got != "editor" {
			t.Fatalf("role changed to %q", got)
		}
	})
}

func TestBUG3355_CanonicalOwnerCannotBeDemotedOrRemoved(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newAccessFixture(t, d)
		co := f.member("co@example.com", "owner") // a second owner
		coTok := f.token(co)

		// The canonical owner demoting themselves, even with another owner present.
		rr := f.do("PATCH", f.membersPath(f.owner.ID), f.ownerTok, map[string]any{"role": "editor"})
		if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "canonical_owner") {
			t.Fatalf("self-demotion of the canonical owner: got %d %s, want 409 canonical_owner", rr.Code, rr.Body.String())
		}
		// Another owner demoting or removing the canonical owner.
		rr = f.do("PATCH", f.membersPath(f.owner.ID), coTok, map[string]any{"role": "viewer"})
		if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "canonical_owner") {
			t.Fatalf("demotion of the canonical owner: got %d %s, want 409 canonical_owner", rr.Code, rr.Body.String())
		}
		rr = f.do("DELETE", f.membersPath(f.owner.ID), coTok, nil)
		if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "canonical_owner") {
			t.Fatalf("removal of the canonical owner: got %d %s, want 409 canonical_owner", rr.Code, rr.Body.String())
		}
		if got := f.roleOf(f.owner.ID); got != "owner" {
			t.Fatalf("canonical owner's role is now %q", got)
		}

		// Controls: the other owner can be demoted, re-promoted and removed.
		f.must(f.do("PATCH", f.membersPath(co.ID), f.ownerTok, map[string]any{"role": "editor"}), http.StatusOK, "demote co-owner")
		f.must(f.do("PATCH", f.membersPath(co.ID), f.ownerTok, map[string]any{"role": "owner"}), http.StatusOK, "re-promote co-owner")
		f.must(f.do("DELETE", f.membersPath(co.ID), f.ownerTok, nil), http.StatusNoContent, "remove co-owner")
	})
}

// A legacy workspace whose canonical owner holds no owner membership: the
// member doors must still never leave it with zero owners.
func TestBUG3355_LastOwnerCannotLeave(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newAccessFixture(t, d)
		last := f.member("last@example.com", "owner")
		lastTok := f.token(last)
		other := f.member("other@example.com", "owner")
		// Legacy state: the canonical owner is no longer an owner member.
		// Written directly: the store's own door now refuses exactly this.
		if _, err := f.srv.store.DB().Exec(f.srv.store.D().Rebind(`UPDATE workspace_members SET role = 'editor' WHERE workspace_id = ? AND user_id = ?`), f.wsID, f.owner.ID); err != nil {
			t.Fatalf("set up legacy state: %v", err)
		}
		// Two owners: one may go.
		f.must(f.do("DELETE", f.membersPath(other.ID), lastTok, nil), http.StatusNoContent, "remove one of two owners")

		// Now "last" is the only owner: demoting or removing them is refused.
		promoted := f.member("p@example.com", "viewer")
		rr := f.do("PATCH", f.membersPath(last.ID), lastTok, map[string]any{"role": "editor"})
		if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "last_owner") {
			t.Fatalf("demoting the last owner: got %d %s, want 409 last_owner", rr.Code, rr.Body.String())
		}
		if got := f.roleOf(last.ID); got != "owner" {
			t.Fatalf("last owner's role is now %q", got)
		}

		// Control: once a second owner exists, the first may step down.
		f.must(f.do("PATCH", f.membersPath(promoted.ID), lastTok, map[string]any{"role": "owner"}), http.StatusOK, "promote a second owner")
		f.must(f.do("PATCH", f.membersPath(last.ID), lastTok, map[string]any{"role": "editor"}), http.StatusOK, "step down with another owner present")
	})
}

// Two owners demoting each other at once must not both succeed: the owner
// rows are read FOR UPDATE on Postgres (SQLite serializes every write
// transaction), so the second re-reads the first's result.
func TestBUG3355_ConcurrentDemotionsKeepAnOwner(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		for round := 0; round < 10; round++ {
			f := newAccessFixture(t, d)
			a := f.member(fmt.Sprintf("a%d@example.com", round), "owner")
			b := f.member(fmt.Sprintf("b%d@example.com", round), "owner")
			if _, err := f.srv.store.DB().Exec(f.srv.store.D().Rebind(`UPDATE workspace_members SET role = 'editor' WHERE workspace_id = ? AND user_id = ?`), f.wsID, f.owner.ID); err != nil {
				t.Fatalf("set up legacy state: %v", err)
			}
			var wg sync.WaitGroup
			errs := make([]error, 2)
			for i, id := range []string{a.ID, b.ID} {
				wg.Add(1)
				go func(i int, id string) {
					defer wg.Done()
					errs[i] = f.srv.store.UpdateWorkspaceMemberRole(f.wsID, id, "editor")
				}(i, id)
			}
			wg.Wait()
			owners := 0
			for _, id := range []string{a.ID, b.ID} {
				if f.roleOf(id) == "owner" {
					owners++
				}
			}
			if owners != 1 {
				t.Fatalf("round %d: %d owners left (errors %v, %v), want exactly 1", round, owners, errs[0], errs[1])
			}
		}
	})
}
