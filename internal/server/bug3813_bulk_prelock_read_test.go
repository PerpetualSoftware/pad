package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3813: the bulk move and the bulk tag and untag built their writes from
// the item ResolveItem read before any lock, so a write landing in between was
// undone. The single-item move closed this in BUG-3665; these are its bulk
// twins, driven through the same seams.

func bulkRequest(t *testing.T, srv *Server, wsSlug string, body map[string]any) bulkItemsResponse {
	t.Helper()
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+wsSlug+"/items/bulk", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("bulk %v: %d %s", body["op"], rr.Code, rr.Body.String())
	}
	var resp bulkItemsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rr.Body.String())
	}
	return resp
}

// The bulk move re-checks every input of its payload under the lock, as the
// single move does, and refuses the row with code "conflict" when one changed.
// Forced or not: force skips only the open-children guard.
func TestBUG3813_BulkMoveRefusesWhenAnInputChangedSinceItsPayloadWasBuilt(t *testing.T) {
	const schema = `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"},{"key":"note","type":"text"}]}`
	const uniqueSchema = `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"},{"key":"note","type":"text","unique_scope":"workspace_collection"}]}`
	type fixture struct {
		srv        *Server
		ws         *models.Workspace
		alpha, bet *models.Collection
		item       *models.Item
	}
	for _, tc := range []struct {
		name       string
		betaSchema string
		change     func(t *testing.T, f fixture)
		check      func(t *testing.T, f fixture)
	}{
		{"item fields", schema, func(t *testing.T, f fixture) {
			fresh := `{"status":"open","note":"new"}`
			if _, err := f.srv.store.UpdateItem(f.item.ID, models.ItemUpdate{Fields: &fresh}); err != nil {
				t.Error(err)
			}
		}, func(t *testing.T, f fixture) {
			if got, _ := f.srv.store.GetItem(f.item.ID); got == nil || !strings.Contains(got.Fields, `"new"`) {
				t.Errorf("the concurrent edit was lost: %+v", got)
			}
		}},
		{"source schema", schema, func(t *testing.T, f fixture) {
			next := `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"},{"key":"note","type":"number"}]}`
			if _, err := f.srv.store.UpdateCollection(f.alpha.ID, models.CollectionUpdate{Schema: &next}); err != nil {
				t.Error(err)
			}
		}, nil},
		{"target settings", schema, func(t *testing.T, f fixture) {
			next := `{"default_view":"board"}`
			if _, err := f.srv.store.UpdateCollection(f.bet.ID, models.CollectionUpdate{Settings: &next}); err != nil {
				t.Error(err)
			}
		}, nil},
		{"target unique holder", uniqueSchema, func(t *testing.T, f fixture) {
			if _, err := f.srv.store.CreateItem(f.ws.ID, f.bet.ID, models.ItemCreate{Title: "Holder", Fields: `{"status":"open","note":"old"}`}); err != nil {
				t.Error(err)
			}
		}, nil},
	} {
		for _, force := range []bool{false, true} {
			name := tc.name
			if force {
				name += "/forced"
			}
			t.Run(name, func(t *testing.T) {
				srv := testServer(t)
				ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Mover"})
				if err != nil {
					t.Fatal(err)
				}
				alpha, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Alpha", Slug: "alpha", Prefix: "ALP", Schema: schema})
				if err != nil {
					t.Fatal(err)
				}
				bet, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Beta", Slug: "beta", Prefix: "BET", Schema: tc.betaSchema})
				if err != nil {
					t.Fatal(err)
				}
				item, err := srv.store.CreateItem(ws.ID, alpha.ID, models.ItemCreate{Title: "Mover", Fields: `{"status":"open","note":"old"}`})
				if err != nil {
					t.Fatal(err)
				}
				f := fixture{srv, ws, alpha, bet, item}
				fired := false
				restore := srv.store.SetBeforeMoveLockForTesting(func(string) {
					fired = true
					tc.change(t, f)
				})
				resp := bulkRequest(t, srv, ws.Slug, map[string]any{"op": "move", "ids": []string{item.ID}, "collection": "beta", "force": force})
				restore()
				if !fired {
					t.Fatal("instrument: the change never ran")
				}
				if len(resp.Failed) != 1 || resp.Failed[0].Code != "conflict" {
					t.Fatalf("a bulk move over a changed %s: updated=%+v failed=%+v, want one failure with code conflict", tc.name, resp.Updated, resp.Failed)
				}
				if got, _ := srv.store.GetItem(item.ID); got == nil || got.CollectionID != alpha.ID {
					t.Errorf("the refused move moved the item: %+v", got)
				}
				if tc.check != nil {
					tc.check(t, f)
				}
			})
		}
	}
}

// Control: with nothing in between, the bulk move succeeds, forced or not.
func TestBUG3813_UnchangedBulkMoveSucceeds(t *testing.T) {
	for _, force := range []bool{false, true} {
		srv := testServer(t)
		ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Mover"})
		if err != nil {
			t.Fatal(err)
		}
		schema := `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"}]}`
		alpha, _ := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Alpha", Slug: "alpha", Prefix: "ALP", Schema: schema})
		if _, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Beta", Slug: "beta", Prefix: "BET", Schema: schema}); err != nil {
			t.Fatal(err)
		}
		item, _ := srv.store.CreateItem(ws.ID, alpha.ID, models.ItemCreate{Title: "Mover", Fields: `{"status":"open"}`})
		resp := bulkRequest(t, srv, ws.Slug, map[string]any{"op": "move", "ids": []string{item.ID}, "collection": "beta", "force": force})
		if len(resp.Updated) != 1 || len(resp.Failed) != 0 {
			t.Fatalf("bulk move (force=%v): updated=%+v failed=%+v", force, resp.Updated, resp.Failed)
		}
	}
}

// A tag change committed between the handler's read and the write survives a
// bulk tag or untag, which now applies its change to the item as it is under
// the lock.
func TestBUG3813_BulkTagKeepsAConcurrentTagChange(t *testing.T) {
	for _, tc := range []struct {
		op        string
		tags      []string
		want, not []string
	}{
		// Start ["a"]; a concurrent writer sets ["a","concurrent"].
		{"tag", []string{"added"}, []string{"a", "concurrent", "added"}, nil},
		{"untag", []string{"a"}, []string{"concurrent"}, []string{"a"}},
	} {
		t.Run(tc.op, func(t *testing.T) {
			srv := testServer(t)
			slug := createWSWithCollections(t, srv)
			ws, err := srv.store.GetWorkspaceBySlug(slug)
			if err != nil {
				t.Fatal(err)
			}
			rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items",
				map[string]any{"title": "Tagged", "tags": []string{"a"}})
			if rr.Code != http.StatusCreated {
				t.Fatalf("seed: %d %s", rr.Code, rr.Body.String())
			}
			var item models.Item
			parseJSON(t, rr, &item)

			fired := 0
			var restore func()
			restore = srv.store.SetAfterItemPreLockReadHookForTesting(func(id string) {
				if id != item.ID || fired > 0 {
					return
				}
				fired++
				// The seam fires inside UpdateItem; clear it around the
				// nested write, as its doc requires.
				restore()
				concurrent := `["a","concurrent"]`
				if _, err := srv.store.UpdateItem(item.ID, models.ItemUpdate{Tags: &concurrent}); err != nil {
					t.Error(err)
				}
			})
			resp := bulkRequest(t, srv, ws.Slug, map[string]any{"op": tc.op, "ids": []string{item.ID}, "tags": tc.tags})
			restore()
			if fired != 1 {
				t.Fatalf("instrument: the concurrent write ran %d times, want 1", fired)
			}
			if len(resp.Updated) != 1 || len(resp.Failed) != 0 {
				t.Fatalf("bulk %s: updated=%+v failed=%+v", tc.op, resp.Updated, resp.Failed)
			}
			got, _ := srv.store.GetItem(item.ID)
			var tags []string
			if got != nil {
				_ = json.Unmarshal([]byte(got.Tags), &tags)
			}
			has := map[string]bool{}
			for _, tg := range tags {
				has[tg] = true
			}
			for _, w := range tc.want {
				if !has[w] {
					t.Errorf("tags after bulk %s = %v, want %q present", tc.op, tags, w)
				}
			}
			for _, n := range tc.not {
				if has[n] {
					t.Errorf("tags after bulk %s = %v, want %q absent", tc.op, tags, n)
				}
			}
		})
	}
}

// #2188 codex r1 Low: the retry is bounded. A concurrent writer that wins
// every attempt exhausts bulkTagAttempts, and the row fails with conflict
// rather than retrying without end or writing over the last concurrent set.
func TestBUG3813_BulkTagGivesUpAfterBulkTagAttempts(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items",
		map[string]any{"title": "Contended", "tags": []string{"a"}})
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed: %d %s", rr.Code, rr.Body.String())
	}
	var item models.Item
	parseJSON(t, rr, &item)

	fired, nested := 0, false
	restore := srv.store.SetAfterItemPreLockReadHookForTesting(func(id string) {
		if id != item.ID || nested {
			return
		}
		fired++
		nested = true // the seam fires for the nested write too
		concurrent := fmt.Sprintf(`["a","c%d"]`, fired)
		if _, err := srv.store.UpdateItem(item.ID, models.ItemUpdate{Tags: &concurrent}); err != nil {
			t.Error(err)
		}
		nested = false
	})
	resp := bulkRequest(t, srv, slug, map[string]any{"op": "tag", "ids": []string{item.ID}, "tags": []string{"added"}})
	restore()
	if fired != bulkTagAttempts {
		t.Fatalf("the bulk tag made %d attempts, want bulkTagAttempts (%d)", fired, bulkTagAttempts)
	}
	if len(resp.Updated) != 0 || len(resp.Failed) != 1 || resp.Failed[0].Code != "conflict" {
		t.Fatalf("updated=%+v failed=%+v, want one failure with code conflict", resp.Updated, resp.Failed)
	}
	got, _ := srv.store.GetItem(item.ID)
	if got == nil {
		t.Fatal("the item is gone")
	}
	if want := fmt.Sprintf(`["a","c%d"]`, bulkTagAttempts); got.Tags != want {
		t.Fatalf("tags = %s, want the last concurrent set %s", got.Tags, want)
	}
}
