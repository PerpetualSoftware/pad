package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3434: relationship reads that already hide a soft-deleted ENDPOINT
// (BUG-734) kept showing an endpoint whose COLLECTION was soft-deleted, though
// every list leaves that item out (BUG-3425 / BUG-3428). Each test links a live
// item to both a live partner (the control) and an item in the doomed
// collection. Reuses bug3425World.

type bug3434World struct {
	bug3425World
	partner *models.Item // a second live item in the live collection
}

func newBug3434World(t *testing.T) bug3434World {
	t.Helper()
	w := newBug3425World(t)
	return bug3434World{bug3425World: w, partner: createTestItem(t, w.s, w.ws.ID, w.liveColl.ID, "Zephyr partner row", "")}
}

func (w bug3434World) link(t *testing.T, src, tgt *models.Item, typ string) {
	t.Helper()
	if _, err := w.s.CreateItemLink(w.ws.ID, models.ItemLinkCreate{TargetID: tgt.ID, LinkType: typ}, src.ID); err != nil {
		t.Fatalf("link %s -[%s]-> %s: %v", src.Title, typ, tgt.Title, err)
	}
}

func linksTouch(links []models.ItemLink, id string) bool {
	for _, l := range links {
		if l.SourceID == id || l.TargetID == id {
			return true
		}
	}
	return false
}

func TestGetItemLinks_ExcludesSoftDeletedCollections(t *testing.T) {
	t.Parallel()
	w := newBug3434World(t)
	w.link(t, w.live, w.partner, "related")
	w.link(t, w.live, w.doomed, "related")
	w.link(t, w.doomed, w.live, "blocks")

	before, err := w.s.GetItemLinks(w.live.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !linksTouch(before, w.doomed.ID) || !linksTouch(before, w.partner.ID) {
		t.Fatalf("control: both links should be listed before the delete")
	}
	w.deleteDoomed(t)
	after, err := w.s.GetItemLinks(w.live.ID)
	if err != nil {
		t.Fatal(err)
	}
	if linksTouch(after, w.doomed.ID) {
		t.Errorf("GetItemLinks still lists a link to an item of a soft-deleted collection")
	}
	if !linksTouch(after, w.partner.ID) {
		t.Errorf("GetItemLinks lost the live link")
	}
}

func TestParentReads_ExcludeSoftDeletedCollections(t *testing.T) {
	t.Parallel()
	w := newBug3434World(t)
	// live -> doomed parent; partner -> live parent (control); and the
	// doomed item as a CHILD, so both ends of a parent link are exercised.
	w.link(t, w.live, w.doomed, "parent")
	w.link(t, w.partner, w.live, "parent")
	extra := createTestItem(t, w.s, w.ws.ID, w.doomColl.ID, "Zephyr doomed child", "")
	w.link(t, extra, w.partner, "parent")

	check := func(when string, wantDoomedParent bool) {
		t.Helper()
		p, err := w.s.GetParentForItem(w.live.ID)
		if err != nil {
			t.Fatalf("%s GetParentForItem: %v", when, err)
		}
		if got := p != nil && p.TargetID == w.doomed.ID; got != wantDoomedParent {
			t.Errorf("%s GetParentForItem names the doomed parent = %v, want %v", when, got, wantDoomedParent)
		}
		if cp, _ := w.s.GetParentForItem(w.partner.ID); cp == nil || cp.TargetID != w.live.ID {
			t.Errorf("%s control: the live parent link is lost", when)
		}
		m, err := w.s.GetParentMap(w.ws.ID)
		if err != nil {
			t.Fatalf("%s GetParentMap: %v", when, err)
		}
		if got := m[w.live.ID] == w.doomed.ID; got != wantDoomedParent {
			t.Errorf("%s GetParentMap maps to the doomed parent = %v, want %v", when, got, wantDoomedParent)
		}
		if m[w.partner.ID] != w.live.ID {
			t.Errorf("%s control: GetParentMap lost the live parent", when)
		}
		if _, got := m[extra.ID]; got != wantDoomedParent {
			t.Errorf("%s GetParentMap has the doomed child = %v, want %v", when, got, wantDoomedParent)
		}
		if p, _ := w.s.GetParentForItem(extra.ID); (p != nil) != wantDoomedParent {
			t.Errorf("%s GetParentForItem(doomed child) found a parent = %v, want %v", when, p != nil, wantDoomedParent)
		}
		lin, err := w.s.GetItemLineageByIDs([]string{w.doomed.ID, w.live.ID})
		if err != nil {
			t.Fatalf("%s GetItemLineageByIDs: %v", when, err)
		}
		if _, got := lin[w.doomed.ID]; got != wantDoomedParent {
			t.Errorf("%s GetItemLineageByIDs returns the doomed item = %v, want %v", when, got, wantDoomedParent)
		}
		if _, ok := lin[w.live.ID]; !ok {
			t.Errorf("%s control: GetItemLineageByIDs lost the live item", when)
		}
	}
	check("before the delete (control)", true)
	w.deleteDoomed(t)
	check("after the delete", false)
}

func TestGetBlocksEdges_ExcludesSoftDeletedCollections(t *testing.T) {
	t.Parallel()
	w := newBug3434World(t)
	w.link(t, w.doomed, w.live, "blocks")
	w.link(t, w.partner, w.live, "blocks")
	// And a live blocker of a doomed target, so both ends are exercised.
	extra := createTestItem(t, w.s, w.ws.ID, w.doomColl.ID, "Zephyr doomed blocked", "")
	w.link(t, w.partner, extra, "blocks")
	edges := func() (doomed, partner bool) {
		t.Helper()
		es, err := w.s.GetBlocksEdges(w.ws.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range es {
			doomed = doomed || e.SourceID == w.doomed.ID || e.TargetID == w.doomed.ID || e.TargetID == extra.ID
			partner = partner || (e.SourceID == w.partner.ID && e.TargetID == w.live.ID)
		}
		return
	}
	if d, p := edges(); !d || !p {
		t.Fatalf("control: both blocks edges before the delete, got doomed=%v partner=%v", d, p)
	}
	w.deleteDoomed(t)
	d, p := edges()
	if d {
		t.Errorf("a blocker in a soft-deleted collection still blocks a live item")
	}
	if !p {
		t.Errorf("the live blocker is lost")
	}
}

// The structural unparented predicate deliberately counts a parent link
// whatever state its target is in ("the relationship itself is what parents
// the source item"): it already counts a soft-deleted parent. BUG-3434 keeps
// that rule for a parent in a soft-deleted collection too, and pins it.
func TestUnparented_StillCountsAParentInASoftDeletedCollection(t *testing.T) {
	t.Parallel()
	w := newBug3434World(t)
	w.link(t, w.live, w.doomed, "parent")
	w.deleteDoomed(t)
	got, err := w.s.ListItems(w.ws.ID, models.ItemListParams{Unparented: true})
	if err != nil {
		t.Fatal(err)
	}
	if hasItem(got, w.live.ID) {
		t.Errorf("the structural rule changed: a child of a parent in a soft-deleted collection now reads as unparented")
	}
	if !hasItem(got, w.partner.ID) {
		t.Errorf("control: an item with no parent link should be unparented")
	}
}
