package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3366 (Dave's ruling on TASK-3345 #4: HIDE). An item-grant guest sees
// the items they were granted, and nothing about the items those link to.
// Lineage (parent title/ref) and closure (superseded/implemented/split
// related items) used to authorize the OTHER item by its collection, and
// collection visibility is navigation-lenient for item grants, so an
// ungranted parent or superseder in the same collection was named to the
// guest. Both now use the per-item policy relation_targets uses.

func (f *accessFixture) itemIn(coll string, body map[string]any) models.Item {
	f.t.Helper()
	rr := f.do("POST", "/api/v1/workspaces/"+f.wsSlug+"/collections/"+coll+"/items", f.ownerTok, body)
	f.must(rr, http.StatusCreated, "create item")
	var it models.Item
	parseJSON(f.t, rr, &it)
	return it
}

func TestBUG3366_GuestDoesNotSeeUngrantedLineage(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newAccessFixture(t, d)
		parent := f.itemIn("tasks", map[string]any{"title": "Secret parent", "fields": `{"status":"open"}`})
		child := f.itemIn("tasks", map[string]any{"title": "Granted child", "fields": `{"status":"open","parent":"` + parent.Ref + `"}`})
		superseder := f.itemIn("tasks", map[string]any{"title": "Secret superseder", "fields": `{"status":"done"}`})
		if _, err := f.srv.store.CreateItemLink(f.wsID, models.ItemLinkCreate{TargetID: child.ID, LinkType: models.ItemLinkTypeSupersedes}, superseder.ID); err != nil {
			t.Fatalf("link: %v", err)
		}

		guest := mkUser(t, f.srv, "guest@example.com")
		if _, err := f.srv.store.CreateItemGrant(f.wsID, child.ID, guest.ID, "view", f.owner.ID); err != nil {
			t.Fatalf("grant child: %v", err)
		}
		tok := f.token(guest)

		leaks := func(body string) []string {
			var out []string
			for _, s := range []string{"Secret parent", parent.Ref, "Secret superseder", superseder.Ref} {
				if strings.Contains(body, s) {
					out = append(out, s)
				}
			}
			return out
		}

		single := f.do("GET", "/api/v1/workspaces/"+f.wsSlug+"/items/"+child.Slug, tok, nil)
		f.must(single, http.StatusOK, "guest reads the granted child")
		if l := leaks(single.Body.String()); len(l) > 0 {
			t.Errorf("single-item read names ungranted items %v: %s", l, single.Body.String())
		}
		list := f.do("GET", "/api/v1/workspaces/"+f.wsSlug+"/collections/tasks/items", tok, nil)
		f.must(list, http.StatusOK, "guest lists")
		if !strings.Contains(list.Body.String(), "Granted child") {
			t.Fatalf("precondition: the guest's list should hold the granted child: %s", list.Body.String())
		}
		if l := leaks(list.Body.String()); len(l) > 0 {
			t.Errorf("list names ungranted items %v: %s", l, list.Body.String())
		}

		// Control: once granted, parent and superseder are named.
		for _, it := range []models.Item{parent, superseder} {
			if _, err := f.srv.store.CreateItemGrant(f.wsID, it.ID, guest.ID, "view", f.owner.ID); err != nil {
				t.Fatalf("grant: %v", err)
			}
		}
		single = f.do("GET", "/api/v1/workspaces/"+f.wsSlug+"/items/"+child.Slug, tok, nil)
		f.must(single, http.StatusOK, "guest reads the child")
		if l := leaks(single.Body.String()); len(l) != 4 {
			t.Errorf("once granted, the read should name parent and superseder; named %v: %s", l, single.Body.String())
		}
	})
}
