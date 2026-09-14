package store_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// BUG-3032: the workspace BUNDLE is the one stale-body door where the content can
// be destroyed rather than merely served late.
//
// BUG-3000 put the marker on models.Item, so every door serialising that struct
// inherited it. The bundle does not serialise models.Item — it has its own
// models.ItemExport — and its items query selects `content` with no table alias,
// so contentStateSQL could not have been spliced in mechanically.
//
// What makes the bundle different from a read: it carries NO op-log.
// models.WorkspaceExport enumerates its sections explicitly and none of them is
// item_yjs_updates, and ImportWorkspace writes ItemExport.Content back as the
// destination's canonical content. On `pad db migrate-to-pg` — ExportWorkspace
// piped into ImportWorkspace across two different databases, after which the
// source is abandoned — an unflushed body therefore becomes the only copy.
//
// THE CONTROL LEG RUNS FIRST, for the reason the BUG-3000 suite states: a marker
// that is always on is indistinguishable from a marker that is always right, and
// this workspace's export carries every item, so a blanket stamp would look like
// success.

func TestExportWorkspaceMarksOnlyItemsWhoseDocumentIsAhead(t *testing.T) {
	for _, backend := range []struct {
		name string
		open func(*testing.T) *store.Store
	}{
		{"SQLite", storetest.NewSQLite},
		{"Postgres", storetest.NewPostgres}, // skips unless PAD_TEST_POSTGRES_URL is set
	} {
		t.Run(backend.name, func(t *testing.T) {
			s := backend.open(t)
			_, _, item := seedStaleItem(t, s)

			find := func(t *testing.T, exp *models.WorkspaceExport) models.ItemExport {
				t.Helper()
				for _, it := range exp.Items {
					if it.ID == item.ID {
						return it
					}
				}
				t.Fatalf("item %s not in the bundle's %d items", item.ID, len(exp.Items))
				return models.ItemExport{}
			}

			// CONTROL. A freshly created item has no op-log at all, so an
			// unconditional stamp fails here before the assertion below can pass
			// for the wrong reason.
			exp, err := s.ExportWorkspace("cs")
			if err != nil {
				t.Fatalf("ExportWorkspace: %v", err)
			}
			if got := find(t, exp).ContentState; got != "" {
				t.Fatalf("a fresh item with no op-log is exported marked %q; the marker is "+
					"firing unconditionally and proves nothing below", got)
			}

			// The absent-key half of the control, which is the compatibility claim
			// rather than the correctness one: `content_state` is additive and
			// omitempty, so a bundle from a workspace with nothing pending must be
			// byte-identical to one produced before this change. There is no golden
			// fixture pinning the bundle's bytes (unlike the single-item artifact's
			// testdata/*.golden.md), so this assertion IS that receipt.
			clean, err := json.Marshal(find(t, exp))
			if err != nil {
				t.Fatalf("marshal clean item: %v", err)
			}
			if strings.Contains(string(clean), "content_state") {
				t.Errorf("a current item's exported JSON carries a content_state key: %s\n"+
					"An older binary tolerates an unknown key, but emitting one for every item "+
					"in every bundle is a format change nobody asked for.", clean)
			}

			// Now put an op-log row ABOVE the (NULL) flush watermark: the document
			// is ahead of the row, which is the state an applier-path write leaves
			// behind before any tab flushes.
			if _, err := s.AppendYjsUpdate(item.ID, []byte{1, 2, 3}, "1"); err != nil {
				t.Fatalf("AppendYjsUpdate: %v", err)
			}

			exp, err = s.ExportWorkspace("cs")
			if err != nil {
				t.Fatalf("ExportWorkspace: %v", err)
			}
			marked := find(t, exp)
			if marked.ContentState != models.ContentOutcomeAppliedPendingFlush {
				t.Errorf("exported content_state = %q, want %q: this bundle's copy of the body "+
					"is behind the live document, and the bundle carries no op-log, so nothing "+
					"downstream of it can discover that",
					marked.ContentState, models.ContentOutcomeAppliedPendingFlush)
			}
			// The marker describes the body; it does not repair it. Stated so the
			// mark is never read as a fix.
			if marked.Content != "stored body" {
				t.Errorf("exported content = %q, want the stored body unchanged", marked.Content)
			}
		})
	}
}

// ListItemsPendingContentFlush is the migration gate's half of the same
// predicate the bundle marker uses, and it exists as a separate query, so it
// gets its own control leg: a gate that listed every item would refuse every
// migration, and a gate that listed none would refuse nothing.
func TestListItemsPendingContentFlushNamesOnlyStaleItems(t *testing.T) {
	for _, backend := range []struct {
		name string
		open func(*testing.T) *store.Store
	}{
		{"SQLite", storetest.NewSQLite},
		{"Postgres", storetest.NewPostgres}, // skips unless PAD_TEST_POSTGRES_URL is set
	} {
		t.Run(backend.name, func(t *testing.T) {
			s := backend.open(t)
			wsID, collID, item := seedStaleItem(t, s)

			// A second item that stays current throughout, so the stale-item
			// assertion below cannot be satisfied by a query that returns the
			// whole collection.
			current, err := s.CreateItem(wsID, collID, models.ItemCreate{Title: "Current", Content: "fine"})
			if err != nil {
				t.Fatalf("CreateItem: %v", err)
			}

			// CONTROL: nothing pending yet.
			pending, err := s.ListItemsPendingContentFlush(wsID)
			if err != nil {
				t.Fatalf("ListItemsPendingContentFlush: %v", err)
			}
			if len(pending) != 0 {
				t.Fatalf("a workspace with no op-log rows reports %d pending item(s): %+v — the gate "+
					"would refuse every migration and the assertion below would prove nothing",
					len(pending), pending)
			}

			if _, err := s.AppendYjsUpdate(item.ID, []byte{1, 2, 3}, "1"); err != nil {
				t.Fatalf("AppendYjsUpdate: %v", err)
			}

			pending, err = s.ListItemsPendingContentFlush(wsID)
			if err != nil {
				t.Fatalf("ListItemsPendingContentFlush: %v", err)
			}
			if len(pending) != 1 {
				t.Fatalf("want exactly the one stale item, got %d: %+v", len(pending), pending)
			}
			// The REF is what the refusal message asks the operator to open, so a
			// wrong or empty one makes the refusal unactionable.
			if pending[0].Ref != item.Ref {
				t.Errorf("pending ref = %q, want %q — this is the string the migration's refusal "+
					"tells an operator to open", pending[0].Ref, item.Ref)
			}
			if pending[0].Title != item.Title {
				t.Errorf("pending title = %q, want %q", pending[0].Title, item.Title)
			}
			if pending[0].Ref == current.Ref {
				t.Errorf("the gate named the CURRENT item %q", current.Ref)
			}
		})
	}
}

// The gate and the bundle must agree about WHICH rows are stale, or the
// migration passes while the bundle it is about to write carries a stale body.
// They are two different queries over the same predicate, so the agreement is a
// property to assert rather than a thing to read off the source — and the
// divergence is not hypothetical: the export's items query does not join
// collections at all, so any collection-liveness or collection-existence clause
// on the gate's side silently shrinks its population.
func TestGatePopulationEqualsTheBundlesMarkedPopulation(t *testing.T) {
	for _, backend := range []struct {
		name string
		open func(*testing.T) *store.Store
	}{
		{"SQLite", storetest.NewSQLite},
		{"Postgres", storetest.NewPostgres}, // skips unless PAD_TEST_POSTGRES_URL is set
	} {
		t.Run(backend.name, func(t *testing.T) {
			s := backend.open(t)
			wsID, collID, stale := seedStaleItem(t, s)

			// A SOFT-DELETED collection holding a stale item. DeleteCollection
			// soft-deletes the collection row alone; its items stay live and stay
			// exported (BUG-2884), so the gate has to name this one too. This is
			// the reachable half of the divergence — an item orphaned from a
			// missing collection row is the other, and is why the gate LEFT joins.
			doomed, err := s.CreateCollection(wsID, models.CollectionCreate{Name: "Doomed"})
			if err != nil {
				t.Fatalf("CreateCollection: %v", err)
			}
			inDoomed, err := s.CreateItem(wsID, doomed.ID, models.ItemCreate{Title: "In a deleted collection", Content: "x"})
			if err != nil {
				t.Fatalf("CreateItem(doomed): %v", err)
			}
			// A live item that never goes stale, so neither side can pass by
			// returning everything.
			if _, err := s.CreateItem(wsID, collID, models.ItemCreate{Title: "Current", Content: "fine"}); err != nil {
				t.Fatalf("CreateItem(current): %v", err)
			}
			for _, id := range []string{stale.ID, inDoomed.ID} {
				if _, err := s.AppendYjsUpdate(id, []byte{1, 2, 3}, "1"); err != nil {
					t.Fatalf("AppendYjsUpdate(%s): %v", id, err)
				}
			}
			// Empty expectedUpdatedAt skips the optimistic-concurrency check, as
			// every other store test that soft-deletes a collection does.
			if err := s.DeleteCollection(doomed.ID, ""); err != nil {
				t.Fatalf("DeleteCollection: %v", err)
			}

			exp, err := s.ExportWorkspace("cs")
			if err != nil {
				t.Fatalf("ExportWorkspace: %v", err)
			}
			marked := map[string]bool{}
			for _, it := range exp.Items {
				if it.ContentState != "" {
					marked[it.ID] = true
				}
			}
			// The precondition: without it, both sides being EMPTY would pass.
			if len(marked) != 2 {
				t.Fatalf("the bundle marked %d items, want 2 (%v) — the fixture is not exercising "+
					"the property", len(marked), marked)
			}

			pending, err := s.ListItemsPendingContentFlush(wsID)
			if err != nil {
				t.Fatalf("ListItemsPendingContentFlush: %v", err)
			}
			if len(pending) != len(marked) {
				t.Errorf("the gate names %d item(s) %+v but the bundle marks %d — the migration "+
					"would pass while writing a stale body",
					len(pending), pending, len(marked))
			}
			// Every named ref must be non-empty and actionable: the refusal's
			// whole job is to tell an operator what to open.
			for _, p := range pending {
				if p.Ref == "" {
					t.Errorf("the gate named an item with an empty ref: %+v", p)
				}
			}
		})
	}
}
