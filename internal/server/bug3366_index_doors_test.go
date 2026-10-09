package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3366, the two doors the local-first index is filled through. The
// collection page's parent filter (TASK-2215) lists the parents its rows
// carry, so a parent title on a granted row is a parent title in a menu.
// enrichItemsWithParent masks an ungranted parent per item for a restricted
// caller; the BUG-3366 test above pins the single-item and list doors, and
// this pins /items-index and /items-changes, which the web client actually
// reads.
func TestBUG3366_IndexDoorsDoNotNameAnUngrantedParent(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newAccessFixture(t, d)
		parent := f.itemIn("tasks", map[string]any{"title": "Secret parent", "fields": `{"status":"open"}`})
		child := f.itemIn("tasks", map[string]any{"title": "Granted child", "fields": `{"status":"open","parent":"` + parent.Ref + `"}`})

		guest := mkUser(t, f.srv, "guest@example.com")
		if _, err := f.srv.store.CreateItemGrant(f.wsID, child.ID, guest.ID, "view", f.owner.ID); err != nil {
			t.Fatalf("grant child: %v", err)
		}
		tok := f.token(guest)

		doors := []string{
			"/api/v1/workspaces/" + f.wsSlug + "/items-index",
			"/api/v1/workspaces/" + f.wsSlug + "/items-changes?since=0",
		}
		check := func(wantNamed bool) {
			for _, door := range doors {
				rr := f.do("GET", door, tok, nil)
				f.must(rr, http.StatusOK, door)
				body := rr.Body.String()
				if !strings.Contains(body, "Granted child") {
					t.Fatalf("precondition: %s should carry the granted child: %s", door, body)
				}
				for _, s := range []string{"Secret parent", parent.Ref, parent.ID} {
					if strings.Contains(body, s) != wantNamed {
						t.Errorf("%s: contains %q = %v, want %v: %s", door, s, !wantNamed, wantNamed, body)
					}
				}
			}
		}
		check(false)

		// Control: once the parent is granted, both doors name it.
		if _, err := f.srv.store.CreateItemGrant(f.wsID, parent.ID, guest.ID, "view", f.owner.ID); err != nil {
			t.Fatalf("grant parent: %v", err)
		}
		check(true)
	})
}
