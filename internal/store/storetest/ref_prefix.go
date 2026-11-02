package storetest

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// RefPrefixFixture shares BUG-4119's integrity assertions across real HTTP,
// CLI and MCP tests. Each caller gets an isolated database.
type RefPrefixFixture struct {
	Store     *store.Store
	Workspace *models.Workspace
	Item      *models.Item
	WrongRef  string
}

func NewRefPrefixFixture(t *testing.T, postgres, identExists bool) RefPrefixFixture {
	t.Helper()
	var s *store.Store
	if postgres {
		s = NewPostgres(t)
	} else {
		s = NewSQLite(t)
	}
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Prefix safety", Slug: "prefix-safety"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Conversations", Prefix: "CONVE", Schema: `{"fields":[]}`})
	if err != nil {
		t.Fatal(err)
	}
	it, err := s.CreateItem(ws.ID, c.ID, models.ItemCreate{Title: "Number holder", Content: "Original content"})
	if err != nil {
		t.Fatal(err)
	}
	if identExists {
		_, err = s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Identities", Prefix: "IDENT", Schema: `{"fields":[]}`})
		if err != nil {
			t.Fatal(err)
		}
	}
	return RefPrefixFixture{s, ws, it, fmt.Sprintf("IDENT-%d", *it.ItemNumber)}
}

// Snapshot includes the target's persisted state and workspace-wide mutation
// counts, including rows belonging to soft-deleted items.
func (f RefPrefixFixture) Snapshot(t *testing.T) string {
	t.Helper()
	it, err := f.Store.GetItemIncludeDeleted(f.Item.ID)
	if err != nil || it == nil {
		t.Fatalf("snapshot item: %v", err)
	}
	counts := map[string]int{}
	queries := map[string]string{
		"comments":    `SELECT COUNT(*) FROM comments WHERE workspace_id = ?`,
		"versions":    `SELECT COUNT(*) FROM item_versions WHERE item_id IN (SELECT id FROM items WHERE workspace_id = ?)`,
		"activities":  `SELECT COUNT(*) FROM activities WHERE workspace_id = ?`,
		"links":       `SELECT COUNT(*) FROM item_links WHERE workspace_id = ?`,
		"wiki":        `SELECT COUNT(*) FROM item_wiki_links WHERE source_item_id IN (SELECT id FROM items WHERE workspace_id = ?)`,
		"outbox":      `SELECT COUNT(*) FROM event_outbox WHERE workspace_id = ?`,
		"idempotency": `SELECT COUNT(*) FROM idempotency_keys WHERE workspace_id = ?`,
	}
	for name, query := range queries {
		var n int
		if err := f.Store.DB().QueryRow(f.Store.D().Rebind(query), f.Workspace.ID).Scan(&n); err != nil {
			t.Fatalf("snapshot %s: %v", name, err)
		}
		counts[name] = n
	}
	b, err := json.Marshal(struct {
		Item   *models.Item
		Counts map[string]int
	}{it, counts})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (f RefPrefixFixture) AssertUnchanged(t *testing.T, before string) {
	t.Helper()
	if after := f.Snapshot(t); after != before {
		t.Errorf("BUG-4119: wrong-prefix request wrote data\nbefore: %s\nafter:  %s", before, after)
	}
}

// NewRefSlugFixture pairs item #2 with a different item owning release-2.
// There is deliberately no RELEASE collection.
func NewRefSlugFixture(t *testing.T, postgres bool) (RefPrefixFixture, *models.Item) {
	t.Helper()
	f := NewRefPrefixFixture(t, postgres, false)
	holder, err := f.Store.CreateItem(f.Workspace.ID, f.Item.CollectionID, models.ItemCreate{Title: "Second number holder", Content: "Untouched number two"})
	if err != nil {
		t.Fatal(err)
	}
	if holder.ItemNumber == nil || *holder.ItemNumber != 2 {
		t.Fatalf("number holder: %v", holder.ItemNumber)
	}
	f.Item = holder
	slugged, err := f.Store.CreateItem(f.Workspace.ID, holder.CollectionID, models.ItemCreate{Title: "Release 2", Content: "Slug owner body"})
	if err != nil {
		t.Fatal(err)
	}
	if slugged.Slug != "release-2" {
		t.Fatalf("slug: %q", slugged.Slug)
	}
	return f, slugged
}
