package store_test

import (
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// BUG-3000: a read that serves items.content must say so when that content is
// behind the item's live collaborative document.
//
// The window is UNBOUNDED, which is what makes the marker worth having rather than
// a documentation note: nothing server-side moves applier content into the row.
// PruneSweep's candidate query requires MAX(op-log.id) <= content_flushed_op_log_id,
// so an item in exactly this state is EXCLUDED from the sweep — its op-log rows are
// retained (the content is durable) and the row stays behind until a tab next opens
// the item and flushes.
//
// THE CONTROL THAT MATTERS is the absence leg. A marker that is always on is
// indistinguishable from a marker that is always right, and the population is large
// enough that a blanket stamp would look like success at every door. Every case
// below therefore asserts both directions on the same item.

func seedStaleItem(t *testing.T, s *store.Store) (wsID, collID string, item *models.Item) {
	t.Helper()
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "CS", Slug: "cs"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if err := s.SeedCollectionsFromTemplate(ws.ID, "startup"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	colls, err := s.ListCollections(ws.ID)
	if err != nil || len(colls) == 0 {
		t.Fatalf("ListCollections: %v", err)
	}
	var tasks string
	for _, c := range colls {
		if c.Slug == "tasks" {
			tasks = c.ID
		}
	}
	if tasks == "" {
		t.Fatal("no tasks collection")
	}
	it, err := s.CreateItem(ws.ID, tasks, models.ItemCreate{Title: "Stale", Content: "stored body"})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	return ws.ID, tasks, it
}

func TestContentStateMarksOnlyItemsWhoseDocumentIsAhead(t *testing.T) {
	for _, backend := range []struct {
		name string
		open func(*testing.T) *store.Store
	}{
		{"SQLite", storetest.NewSQLite},
		{"Postgres", storetest.NewPostgres}, // skips unless PAD_TEST_POSTGRES_URL is set
	} {
		t.Run(backend.name, func(t *testing.T) {
			s := backend.open(t)
			wsID, _, item := seedStaleItem(t, s)

			// CONTROL, and it runs FIRST so the later assertion cannot be read as
			// "the marker is simply always set". A freshly created item has no
			// op-log at all.
			got, err := s.GetItem(item.ID)
			if err != nil {
				t.Fatalf("GetItem: %v", err)
			}
			if got.ContentState != "" {
				t.Fatalf("a fresh item with no op-log is marked %q; the marker is firing "+
					"unconditionally and proves nothing below", got.ContentState)
			}

			// Now put a row in the op-log ABOVE the (NULL) flush watermark: the
			// document is ahead of the row, which is exactly the state an
			// applier-path write leaves behind before any tab flushes.
			if _, err := s.AppendYjsUpdate(item.ID, []byte{1, 2, 3}, "1"); err != nil {
				t.Fatalf("AppendYjsUpdate: %v", err)
			}

			got, err = s.GetItem(item.ID)
			if err != nil {
				t.Fatalf("GetItem: %v", err)
			}
			if got.ContentState != models.ContentOutcomeAppliedPendingFlush {
				t.Errorf("GetItem content_state = %q, want %q: the op-log is ahead of the row, so "+
					"this body is stale and a caller has no other way to learn that",
					got.ContentState, models.ContentOutcomeAppliedPendingFlush)
			}
			// The body itself is unchanged — the marker describes the row, it does
			// not repair it. Stated so nobody reads the marker as a fix.
			if got.Content != "stored body" {
				t.Errorf("content = %q, want the stored body unchanged", got.Content)
			}

			// EVERY read door that serves a body must agree with GetItem. A marker
			// present on one door and missing on another is the failure this
			// enumeration exists to prevent.
			items, err := s.ListItems(wsID, models.ItemListParams{})
			if err != nil {
				t.Fatalf("ListItems: %v", err)
			}
			var found bool
			for _, li := range items {
				if li.ID != item.ID {
					continue
				}
				found = true
				if li.ContentState != models.ContentOutcomeAppliedPendingFlush {
					t.Errorf("ListItems content_state = %q, want %q", li.ContentState,
						models.ContentOutcomeAppliedPendingFlush)
				}
			}
			if !found {
				t.Fatal("the item did not come back from ListItems; this leg measured nothing")
			}

			// NoContent asks for the body to be omitted. The marker must go with it:
			// claiming an omitted body is stale is a false signal about content this
			// query never returned.
			noBody, err := s.ListItems(wsID, models.ItemListParams{NoContent: true})
			if err != nil {
				t.Fatalf("ListItems(NoContent): %v", err)
			}
			for _, li := range noBody {
				if li.ID != item.ID {
					continue
				}
				if li.Content != "" {
					t.Fatalf("NoContent returned a body (%q); this leg cannot measure what it claims", li.Content)
				}
				if li.ContentState != "" {
					t.Errorf("NoContent omitted the body but still marked it %q", li.ContentState)
				}
			}
		})
	}
}

// TestContentStateClearsOnceTheFlushWatermarkCatchesUp is the other half of the
// control: the marker must go AWAY, not merely appear. A predicate that latches on
// once an op-log row exists would pass every assertion above and still be wrong for
// every item that has ever been edited collaboratively — which, on this deployment,
// is most of them.
func TestContentStateClearsOnceTheFlushWatermarkCatchesUp(t *testing.T) {
	s := storetest.NewSQLite(t)
	_, _, item := seedStaleItem(t, s)

	opID, err := s.AppendYjsUpdate(item.ID, []byte{9}, "1")
	if err != nil {
		t.Fatalf("AppendYjsUpdate: %v", err)
	}
	got, err := s.GetItem(item.ID)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if got.ContentState == "" {
		t.Fatal("precondition failed: the item is not marked stale, so clearing it proves nothing")
	}

	// Advance the watermark to cover the op-log, which is what a flush does.
	if err := s.SetItemContentFlushedOpLogIDForTesting(item.ID, opID); err != nil {
		t.Fatalf("advance watermark: %v", err)
	}

	got, err = s.GetItem(item.ID)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if got.ContentState != "" {
		t.Errorf("content_state = %q after the watermark caught up, want empty: the row is current "+
			"and a permanent marker would be worse than none", got.ContentState)
	}
}

// TestEveryContentBearingReadDoorCarriesTheMarker drives EVERY store function whose
// query was spliced, against one item in the stale state.
//
// It exists because of how the first implementation pass failed. A column added to a
// SELECT without its matching Scan destination is a RUNTIME error, not a compile
// error: `go build` stayed green across a half-applied edit, and the mismatch
// surfaced only when a test happened to exercise the query. Two of the fifteen
// spliced queries were reachable only through paths no content-state test touched,
// so "the suite is green" was not evidence that all fifteen were aligned.
//
// So this is a column/Scan alignment guard first and a behaviour test second. A door
// that returns the right marker has, necessarily, a matching destination.
func TestEveryContentBearingReadDoorCarriesTheMarker(t *testing.T) {
	s := storetest.NewSQLite(t)
	wsID, _, item := seedStaleItem(t, s)
	if _, err := s.AppendYjsUpdate(item.ID, []byte{7}, "1"); err != nil {
		t.Fatalf("AppendYjsUpdate: %v", err)
	}
	want := models.ContentOutcomeAppliedPendingFlush

	// A child so the child-item doors have something to return.
	child, err := s.CreateItem(wsID, item.CollectionID, models.ItemCreate{
		Title: "Child", Content: "child body",
	})
	if err != nil {
		t.Fatalf("create child: %v", err)
	}
	// The hierarchy lives in item_links, not in a parent_id column, so the link has
	// to be created explicitly — a ParentID on ItemCreate leaves GetChildItems
	// empty and the leg below would report "measured nothing" rather than a marker
	// failure. (It did, first time round.)
	if _, err := s.CreateItemLink(wsID, models.ItemLinkCreate{
		TargetID: item.ID, LinkType: "parent",
	}, child.ID); err != nil {
		t.Fatalf("link child to parent: %v", err)
	}
	if _, err := s.AppendYjsUpdate(child.ID, []byte{8}, "1"); err != nil {
		t.Fatalf("AppendYjsUpdate(child): %v", err)
	}

	one := func(name string, get func() (*models.Item, error)) {
		t.Run(name, func(t *testing.T) {
			got, err := get()
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if got == nil {
				t.Fatalf("%s returned no item; this leg measured nothing", name)
			}
			if got.ContentState != want {
				t.Errorf("%s content_state = %q, want %q", name, got.ContentState, want)
			}
		})
	}
	first := func(name string, get func() ([]models.Item, error), id string) {
		t.Run(name, func(t *testing.T) {
			items, err := get()
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			for _, it := range items {
				if it.ID != id {
					continue
				}
				if it.ContentState != want {
					t.Errorf("%s content_state = %q, want %q", name, it.ContentState, want)
				}
				return
			}
			t.Fatalf("%s did not return the item; this leg measured nothing", name)
		})
	}

	one("GetItem", func() (*models.Item, error) { return s.GetItem(item.ID) })
	one("GetItemIncludeDeleted", func() (*models.Item, error) { return s.GetItemIncludeDeleted(item.ID) })
	one("GetItemBySlug", func() (*models.Item, error) { return s.GetItemBySlug(wsID, item.Slug) })
	one("GetItemBySlugIncludeDeleted", func() (*models.Item, error) {
		return s.GetItemBySlugIncludeDeleted(wsID, item.Slug)
	})
	one("ResolveItem", func() (*models.Item, error) { return s.ResolveItem(wsID, item.Slug) })
	one("ResolveItemIncludeDeleted", func() (*models.Item, error) {
		return s.ResolveItemIncludeDeleted(wsID, item.Slug)
	})

	first("ListItems", func() ([]models.Item, error) {
		return s.ListItems(wsID, models.ItemListParams{})
	}, item.ID)
	first("ListItems(search→FTS)", func() ([]models.Item, error) {
		return s.ListItems(wsID, models.ItemListParams{Search: "Stale"})
	}, item.ID)
	first("GetChildItems", func() ([]models.Item, error) { return s.GetChildItems(item.ID) }, child.ID)
	first("ItemsModifiedSince", func() ([]models.Item, error) {
		updated, _, err := s.ItemsModifiedSince(wsID, time.Time{})
		return updated, err
	}, item.ID)

	// The starred door specifically: it lives in item_stars.go and reaches the
	// SHARED scanItems helper from outside items.go, which is exactly how the first
	// implementation pass broke — and it broke at runtime, with the build green.
	t.Run("ListStarredItems", func(t *testing.T) {
		// A real user row: item_stars carries an FK on user_id, so a synthetic id
		// fails the insert rather than the assertion.
		u, err := s.CreateUser(models.UserCreate{
			Email: "star@example.com", Name: "Star", Password: "correct-horse-battery-staple",
		})
		if err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		if err := s.StarItem(u.ID, item.ID); err != nil {
			t.Fatalf("StarItem: %v", err)
		}
		items, err := s.ListStarredItems(u.ID, wsID, true)
		if err != nil {
			t.Fatalf("ListStarredItems: %v", err)
		}
		for _, it := range items {
			if it.ID != item.ID {
				continue
			}
			if it.ContentState != want {
				t.Errorf("ListStarredItems content_state = %q, want %q", it.ContentState, want)
			}
			return
		}
		t.Fatal("ListStarredItems did not return the item; this leg measured nothing")
	})

	t.Run("SearchItems", func(t *testing.T) {
		results, err := s.SearchItems(wsID, "Stale")
		if err != nil {
			t.Fatalf("SearchItems: %v", err)
		}
		for _, r := range results {
			if r.Item.ID != item.ID {
				continue
			}
			if r.Item.ContentState != want {
				t.Errorf("SearchItems content_state = %q, want %q", r.Item.ContentState, want)
			}
			return
		}
		t.Fatal("SearchItems did not return the item; this leg measured nothing")
	})

	t.Run("Search", func(t *testing.T) {
		resp, err := s.Search(store.SearchParams{WorkspaceIDs: []string{wsID}, Query: "Stale"})
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		for _, r := range resp.Results {
			if r.Item.ID != item.ID {
				continue
			}
			if r.Item.ContentState != want {
				t.Errorf("Search content_state = %q, want %q", r.Item.ContentState, want)
			}
			return
		}
		t.Fatal("Search did not return the item; this leg measured nothing")
	})
}
