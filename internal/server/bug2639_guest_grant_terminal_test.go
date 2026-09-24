package server

import (
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2639: listTerminalItemsSince passed a guest's item grants into EVERY
// per-(field, value) completed-work query. ListItems ORs ItemIDs past the
// CollectionIDs restriction, so a granted item was counted under ANOTHER
// collection's terminal value, and an item matching two groups' keys was
// listed twice. Members are unaffected (no grant list), which is why the
// fixture is a guest.
//
// The fixture's three granted items each discriminate one thing:
//   - overMatch: status=done in "ships", where done is NOT terminal (only
//     shipped is). tasks declares done terminal, so the unscoped grant list
//     counted it. Must be ABSENT.
//   - twoKeys: status=shipped (terminal in ships, its own collection) AND
//     stage=complete, the done value of the "stages" collection, whose done
//     field is `stage`. The unscoped list matched it in both groups. Must
//     appear ONCE.
//   - control: status=done in tasks, an item-grant-only collection. Must be
//     PRESENT; scoping by the group's collections must not drop a granted
//     item whose collection the caller holds no collection grant on.
func TestBUG2639_GuestItemGrantsCountOnlyUnderTheirOwnCollection(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		slug := createWSWithCollections(t, srv)
		ws, err := srv.store.GetWorkspaceBySlug(slug)
		if err != nil || ws == nil {
			t.Fatalf("GetWorkspaceBySlug: %v", err)
		}

		// Everything is created before any user exists: the fresh-install
		// path needs no CSRF token (see the note in
		// TestProjectChangelogEndpoint_GuestParentFilter_ItemGrantOnlyCollection).
		for _, c := range []map[string]any{
			{
				"name": "Ships", "slug": "ships", "prefix": "SHIP",
				"schema": `{"fields":[{"key":"status","type":"select","options":["open","done","shipped"],"terminal_options":["shipped"],"default":"open"}]}`,
			},
			{
				"name": "Stages", "slug": "stages", "prefix": "STG",
				"schema":   `{"fields":[{"key":"stage","type":"select","options":["todo","complete"],"terminal_options":["complete"],"default":"todo"}]}`,
				"settings": `{"board_group_by":"stage"}`,
			},
		} {
			if rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections", c); rr.Code != http.StatusCreated {
				t.Fatalf("create collection %v: %d %s", c["slug"], rr.Code, rr.Body.String())
			}
		}
		// A member-visible item in stages, so the (stage, complete) group is
		// populated and queried at all.
		createItem(t, srv, slug, "stages", map[string]any{"title": "Stage item", "fields": `{"stage":"complete"}`})

		overMatch := createItem(t, srv, slug, "ships", map[string]any{"title": "Done is not terminal here", "fields": `{"status":"done"}`})
		twoKeys := createItem(t, srv, slug, "ships", map[string]any{"title": "Matches two groups", "fields": `{"status":"shipped","stage":"complete"}`})
		control := createItem(t, srv, slug, "tasks", map[string]any{"title": "Granted task", "fields": `{"status":"done"}`})
		// Granted twice over: ideas is fully collection-granted AND this item
		// carries its own grant. It must appear, and once (codex round 1 on
		// BUG-2639 named it as the combination no leg covered).
		bothWays := createItem(t, srv, slug, "ideas", map[string]any{"title": "Granted both ways", "fields": `{"status":"implemented"}`})

		ideas, err := srv.store.GetCollectionBySlug(ws.ID, "ideas")
		if err != nil || ideas == nil {
			t.Fatalf("GetCollectionBySlug ideas: %v", err)
		}
		granter, err := srv.store.CreateUser(models.UserCreate{Email: "granter-2639@example.com", Name: "Granter", Username: "granter-2639", Password: "pw-test-12345"})
		if err != nil {
			t.Fatalf("CreateUser granter: %v", err)
		}
		guest, err := srv.store.CreateUser(models.UserCreate{Email: "guest-2639@example.com", Name: "Guest", Username: "guest-2639", Password: "pw-test-12345"})
		if err != nil {
			t.Fatalf("CreateUser guest: %v", err)
		}
		if _, err := srv.store.CreateCollectionGrant(ws.ID, ideas.ID, guest.ID, "view", granter.ID); err != nil {
			t.Fatalf("CreateCollectionGrant: %v", err)
		}
		for _, it := range []models.Item{overMatch, twoKeys, control, bothWays} {
			if _, err := srv.store.CreateItemGrant(ws.ID, it.ID, guest.ID, "view", granter.ID); err != nil {
				t.Fatalf("CreateItemGrant %s: %v", it.Ref, err)
			}
		}
		token, err := srv.store.CreateSession(guest.ID, "go-test", "192.0.2.1", "go-test", 24*time.Hour)
		if err != nil {
			t.Fatalf("CreateSession: %v", err)
		}

		want := []string{control.Ref, twoKeys.Ref, bothWays.Ref}
		sort.Strings(want)

		rr := doRequestWithCookie(srv, "GET", "/api/v1/workspaces/"+slug+"/changelog", nil, token)
		if rr.Code != http.StatusOK {
			t.Fatalf("changelog: %d %s", rr.Code, rr.Body.String())
		}
		var cl ChangelogResponse
		parseJSON(t, rr, &cl)
		var got []string
		for _, g := range cl.Groups {
			for _, it := range g.Items {
				got = append(got, it.Ref)
			}
		}
		sort.Strings(got)
		if !equalStrings(got, want) || cl.Total != len(want) {
			t.Errorf("changelog completed work = %v (total %d), want %v: %s over-matched under tasks' done, %s listed once, %s present",
				got, cl.Total, want, overMatch.Ref, twoKeys.Ref, control.Ref)
		}

		rr = doRequestWithCookie(srv, "GET", "/api/v1/workspaces/"+slug+"/standup?days=1", nil, token)
		if rr.Code != http.StatusOK {
			t.Fatalf("standup: %d %s", rr.Code, rr.Body.String())
		}
		var su StandupResponse
		parseJSON(t, rr, &su)
		got = got[:0]
		for _, it := range su.Completed {
			got = append(got, it.Ref)
		}
		sort.Strings(got)
		if !equalStrings(got, want) {
			t.Errorf("standup completed = %v, want %v", got, want)
		}
	})
}

// A collection whose completed-work values include both "x" and "x,y" lists an
// item with status x once. When this was written, ListItems split a Fields
// value containing a comma into an IN list, so the item landed in two
// (field, value) groups and the dedup was what kept it single (BUG-2639).
// Since BUG-3167 the store matches exactly, the "x,y" group matches only a
// literal "x,y", and this pins the outcome whichever mechanism holds it.
func TestBUG2639_CompletedWorkListsAnItemOnce(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		slug := createWSWithCollections(t, srv)
		if rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections", map[string]any{
			"name": "Commas", "slug": "commas", "prefix": "CMA",
			"schema": `{"fields":[{"key":"status","type":"select","options":["open","x","x,y"],"terminal_options":["x","x,y"],"default":"open"}]}`,
		}); rr.Code != http.StatusCreated {
			t.Fatalf("create collection: %d %s", rr.Code, rr.Body.String())
		}
		it := createItem(t, srv, slug, "commas", map[string]any{"title": "In two groups", "fields": `{"status":"x"}`})

		rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/changelog", nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("changelog: %d %s", rr.Code, rr.Body.String())
		}
		var cl ChangelogResponse
		parseJSON(t, rr, &cl)
		n := 0
		for _, g := range cl.Groups {
			for _, got := range g.Items {
				if got.Ref == it.Ref {
					n++
				}
			}
		}
		if n != 1 {
			t.Errorf("%s listed %d times in changelog, want once", it.Ref, n)
		}
	})
}
