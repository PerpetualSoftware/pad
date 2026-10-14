package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3665, codex r1: the move handler builds the moved fields from the item
// as read before the move's lock. A write landing in between was overwritten
// by fields migrated from the older body. Every move, forced or not, now
// checks under the lock that the item is unchanged and answers 409 if not.
func TestBUG3665_MoveRefusesAnItemChangedSinceItsPayloadWasBuilt(t *testing.T) {
	for _, force := range []bool{false, true} {
		name := "unforced"
		if force {
			name = "forced"
		}
		t.Run(name, func(t *testing.T) {
			srv := testServer(t)
			ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Mover " + name})
			if err != nil {
				t.Fatal(err)
			}
			schema := `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"},{"key":"note","type":"text"}]}`
			a, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Alpha", Slug: "alpha", Prefix: "ALP", Schema: schema})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Beta", Slug: "beta", Prefix: "BET", Schema: schema}); err != nil {
				t.Fatal(err)
			}
			item, err := srv.store.CreateItem(ws.ID, a.ID, models.ItemCreate{Title: "Mover", Fields: `{"status":"open","note":"old"}`})
			if err != nil {
				t.Fatal(err)
			}
			path := "/api/v1/workspaces/" + ws.Slug + "/items/" + item.Slug + "/move"
			if force {
				path += "?force=true"
			}

			// Another writer edits the note between the handler's read and the
			// move's lock.
			fresh := `{"status":"open","note":"new"}`
			restore := srv.store.SetBeforeMoveLockForTesting(func(id string) {
				if _, err := srv.store.UpdateItem(id, models.ItemUpdate{Fields: &fresh}); err != nil {
					t.Errorf("the concurrent edit: %v", err)
				}
			})
			rr := doRequest(srv, "POST", path, map[string]any{"target_collection": "beta"})
			restore()
			if rr.Code != http.StatusConflict {
				t.Fatalf("a move over a concurrent edit: %d %s, want 409", rr.Code, rr.Body.String())
			}
			got, err := srv.store.GetItem(item.ID)
			if err != nil || got == nil {
				t.Fatal(err)
			}
			if got.CollectionID != a.ID || !strings.Contains(got.Fields, `"new"`) {
				t.Errorf("the concurrent edit was lost or the item moved: collection %s fields %s", got.CollectionID, got.Fields)
			}

			// Control: with nothing in between, the same move succeeds.
			if rr := doRequest(srv, "POST", path, map[string]any{"target_collection": "beta"}); rr.Code != http.StatusOK {
				t.Fatalf("control move: %d %s", rr.Code, rr.Body.String())
			}
		})
	}
}
