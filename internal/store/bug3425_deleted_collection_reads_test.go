package store

import (
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3425: soft-deleting a collection sets deleted_at on the collection row
// only, so its items stay live. ListItems, its FTS path and the graph's edges
// joined collections without checking c.deleted_at, so an unrestricted reader
// (who has no collection filter to catch it) kept seeing them. Every test
// keeps a live item beside the doomed one, so a query that returns nothing
// cannot pass.

type bug3425World struct {
	s                  *Store
	ws                 *models.Workspace
	liveColl, doomColl *models.Collection
	live, doomed       *models.Item
}

func newBug3425World(t *testing.T) bug3425World {
	t.Helper()
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Deleted collection reads")
	w := bug3425World{s: s, ws: ws}
	w.liveColl = createTestCollection(t, s, ws.ID, "Notes")
	w.doomColl = createTestCollection(t, s, ws.ID, "Drafts")
	w.live = createTestItem(t, s, ws.ID, w.liveColl.ID, "Zephyr live row", "")
	w.doomed = createTestItem(t, s, ws.ID, w.doomColl.ID, "Zephyr doomed row", "")
	return w
}

func (w bug3425World) deleteDoomed(t *testing.T) {
	t.Helper()
	if err := w.s.DeleteCollection(w.doomColl.ID, ""); err != nil {
		t.Fatalf("soft-delete collection: %v", err)
	}
}

func hasItem(items []models.Item, id string) bool {
	for _, it := range items {
		if it.ID == id {
			return true
		}
	}
	return false
}

func TestListItems_ExcludesSoftDeletedCollections(t *testing.T) {
	t.Parallel()
	w := newBug3425World(t)

	before, err := w.s.ListItems(w.ws.ID, models.ItemListParams{})
	if err != nil {
		t.Fatalf("ListItems before: %v", err)
	}
	if !hasItem(before, w.doomed.ID) || !hasItem(before, w.live.ID) {
		t.Fatalf("control: both items should be listed before the delete")
	}

	w.deleteDoomed(t)
	after, err := w.s.ListItems(w.ws.ID, models.ItemListParams{})
	if err != nil {
		t.Fatalf("ListItems after: %v", err)
	}
	if hasItem(after, w.doomed.ID) {
		t.Errorf("ListItems still lists an item from a soft-deleted collection")
	}
	if !hasItem(after, w.live.ID) {
		t.Errorf("ListItems lost the live item")
	}

	// The account data export keeps exporting everything the user owns.
	optOut, err := w.s.ListItems(w.ws.ID, models.ItemListParams{IncludeArchived: true, IncludeDeletedCollections: true})
	if err != nil {
		t.Fatalf("ListItems opt-out: %v", err)
	}
	if !hasItem(optOut, w.doomed.ID) || !hasItem(optOut, w.live.ID) {
		t.Errorf("IncludeDeletedCollections must keep the deleted collection's items")
	}
}

func TestListItemsSearch_ExcludesSoftDeletedCollections(t *testing.T) {
	t.Parallel()
	w := newBug3425World(t)
	search := func() []models.Item {
		t.Helper()
		got, err := w.s.ListItems(w.ws.ID, models.ItemListParams{Search: "Zephyr"})
		if err != nil {
			t.Fatalf("ListItems search: %v", err)
		}
		return got
	}

	if before := search(); !hasItem(before, w.doomed.ID) || !hasItem(before, w.live.ID) {
		t.Fatalf("control: search should find both items before the delete")
	}
	w.deleteDoomed(t)
	after := search()
	if hasItem(after, w.doomed.ID) {
		t.Errorf("search still finds an item from a soft-deleted collection")
	}
	if !hasItem(after, w.live.ID) {
		t.Errorf("search lost the live item")
	}
	optOut, err := w.s.ListItems(w.ws.ID, models.ItemListParams{Search: "Zephyr", IncludeDeletedCollections: true})
	if err != nil {
		t.Fatalf("ListItems search opt-out: %v", err)
	}
	if !hasItem(optOut, w.doomed.ID) {
		t.Errorf("search ignores IncludeDeletedCollections")
	}
}

func TestListWorkspaceGraphLinks_ExcludesEdgesTouchingSoftDeletedCollections(t *testing.T) {
	t.Parallel()
	w := newBug3425World(t)
	s, ws := w.s, w.ws
	partner := createTestItem(t, s, ws.ID, w.liveColl.ID, "Partner", "")

	link := func(src, tgt *models.Item, typ string) {
		t.Helper()
		if _, err := s.CreateItemLink(ws.ID, models.ItemLinkCreate{TargetID: tgt.ID, LinkType: typ}, src.ID); err != nil {
			t.Fatalf("link %s -> %s: %v", src.Title, tgt.Title, err)
		}
	}
	wiki := func(src, tgt *models.Item) {
		t.Helper()
		body := "See [[" + refOf(tgt) + "]]."
		if _, err := s.UpdateItem(src.ID, models.ItemUpdate{Content: &body}); err != nil {
			t.Fatalf("wiki %s -> %s: %v", src.Title, tgt.Title, err)
		}
	}
	// Live control, then each edge shape touching the doomed collection from
	// either end.
	link(w.live, partner, "blocks")
	link(w.doomed, partner, "related")
	link(partner, w.doomed, "blocks")
	wiki(w.live, partner)
	wiki(w.doomed, w.live)
	wiki(partner, w.doomed)

	touches := func(links []GraphLink, id string) []string {
		var out []string
		for _, l := range links {
			if l.SourceID == id || l.TargetID == id {
				out = append(out, l.Type)
			}
		}
		return out
	}
	liveEdges := func(links []GraphLink) (blocks, wikiLink bool) {
		for _, l := range links {
			if l.SourceID == w.live.ID && l.TargetID == partner.ID {
				blocks = blocks || l.Type == "blocks"
				wikiLink = wikiLink || l.Type == "wiki-link"
			}
		}
		return
	}

	before, err := s.ListWorkspaceGraphLinks(ws.ID)
	if err != nil {
		t.Fatalf("graph links before: %v", err)
	}
	// Control: every doomed-collection edge shape is present before the
	// delete (item_links both ways, wiki both ways).
	if got := touches(before, w.doomed.ID); len(got) != 4 {
		t.Fatalf("control: want 4 edges touching the doomed item before the delete, got %v", got)
	}

	w.deleteDoomed(t)
	after, err := s.ListWorkspaceGraphLinks(ws.ID)
	if err != nil {
		t.Fatalf("graph links after: %v", err)
	}
	if got := touches(after, w.doomed.ID); len(got) != 0 {
		t.Errorf("graph still has edges touching a soft-deleted collection's item: %s", strings.Join(got, ", "))
	}
	if b, wl := liveEdges(after); !b || !wl {
		t.Errorf("graph lost a live edge: blocks=%v wiki-link=%v", b, wl)
	}
}

// The title-rename cascade deliberately still rewrites link text inside a
// soft-deleted collection's items, so a restored collection's links keep
// working. BUG-3425 must not narrow it.
func TestTitleRenameCascade_StillRewritesSoftDeletedCollectionSources(t *testing.T) {
	t.Parallel()
	w := newBug3425World(t)
	body := "See [[Zephyr live row]]."
	if _, err := w.s.UpdateItem(w.doomed.ID, models.ItemUpdate{Content: &body}); err != nil {
		t.Fatalf("seed link: %v", err)
	}
	w.deleteDoomed(t)

	title := "Zephyr renamed row"
	if _, err := w.s.UpdateItem(w.live.ID, models.ItemUpdate{Title: &title}); err != nil {
		t.Fatalf("rename target: %v", err)
	}
	got, err := w.s.GetItem(w.doomed.ID)
	if err != nil || got == nil {
		t.Fatalf("read source: %v", err)
	}
	if !strings.Contains(got.Content, "[[Zephyr renamed row]]") {
		t.Errorf("cascade no longer rewrites a soft-deleted collection's source: %q", got.Content)
	}
}

// Codex r1: the role lanes list their items through ListItems, so the role
// COUNTS must leave out a soft-deleted collection's items too, or a lane
// reports more items than it shows.
func TestRoleCounts_ExcludeSoftDeletedCollections(t *testing.T) {
	t.Parallel()
	w := newBug3425World(t)
	role, err := w.s.CreateAgentRole(w.ws.ID, models.AgentRoleCreate{Name: "Implementer"})
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	for _, it := range []*models.Item{w.live, w.doomed} {
		if _, err := w.s.UpdateItem(it.ID, models.ItemUpdate{AgentRoleID: &role.ID}); err != nil {
			t.Fatalf("assign role to %s: %v", it.Title, err)
		}
	}
	counts := func() (listed, breakdown int) {
		t.Helper()
		roles, err := w.s.ListAgentRoles(w.ws.ID)
		if err != nil {
			t.Fatalf("ListAgentRoles: %v", err)
		}
		for _, r := range roles {
			if r.ID == role.ID {
				listed = r.ItemCount
			}
		}
		rb, err := w.s.GetRoleBreakdown(w.ws.ID)
		if err != nil {
			t.Fatalf("GetRoleBreakdown: %v", err)
		}
		for _, b := range rb {
			if b.RoleID != nil && *b.RoleID == role.ID {
				breakdown = b.ItemCount
			}
		}
		return
	}

	if l, b := counts(); l != 2 || b != 2 {
		t.Fatalf("control: want 2 and 2 before the delete, got ListAgentRoles=%d GetRoleBreakdown=%d", l, b)
	}
	w.deleteDoomed(t)
	if l, b := counts(); l != 1 || b != 1 {
		t.Errorf("after the delete: want 1 and 1 (the live item only), got ListAgentRoles=%d GetRoleBreakdown=%d", l, b)
	}
}
