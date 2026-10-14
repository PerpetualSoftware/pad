package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3665, codex r1 and r2: the move handler builds the moved fields from
// reads taken before the move's lock: the item, the source and target
// collections, and the target's unique-field holders. A change to any of them
// in between was written over, or let a stale payload through. Every move,
// forced or not, now re-reads those inputs under the lock and answers 409 if
// one changed. Each case changes one input between the handler's reads and
// the lock (the beforeMoveLock seam).
func TestBUG3665_MoveRefusesWhenAnInputChangedSinceItsPayloadWasBuilt(t *testing.T) {
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
				path := "/api/v1/workspaces/" + ws.Slug + "/items/" + item.Slug + "/move"
				if force {
					path += "?force=true"
				}
				fired := false
				restore := srv.store.SetBeforeMoveLockForTesting(func(string) {
					fired = true
					tc.change(t, f)
				})
				rr := doRequest(srv, "POST", path, map[string]any{"target_collection": "beta"})
				restore()
				if !fired {
					t.Fatal("instrument: the change never ran")
				}
				if rr.Code != http.StatusConflict {
					t.Fatalf("a move over a changed %s: %d %s, want 409", tc.name, rr.Code, rr.Body.String())
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

// Control: with nothing in between, both move routes succeed.
func TestBUG3665_UnchangedMoveSucceeds(t *testing.T) {
	for _, q := range []string{"", "?force=true"} {
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
		if rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws.Slug+"/items/"+item.Slug+"/move"+q, map[string]any{"target_collection": "beta"}); rr.Code != http.StatusOK {
			t.Fatalf("move%s: %d %s", q, rr.Code, rr.Body.String())
		}
	}
}
