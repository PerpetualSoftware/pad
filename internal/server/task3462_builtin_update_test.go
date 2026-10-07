package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/collab"
	"github.com/PerpetualSoftware/pad/internal/collections"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3462 U2: the state endpoint, the listing, and the opt-in update.
//
// The compiled-in library cannot change inside a test, so "the library
// changed since this item was seeded" is staged the other way round: the item
// is put back to an OLDER text and its origin records that older text as its
// seed. That is exactly the row a workspace seeded before a library fix holds.

func task3462GetState(t *testing.T, srv *Server, ws, ref string) builtinStateResponse {
	t.Helper()
	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+ws+"/items/"+ref+"/builtin", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET builtin %s: %d %s", ref, rr.Code, rr.Body.String())
	}
	var resp builtinStateResponse
	parseJSON(t, rr, &resp)
	return resp
}

// stageOldSeed makes the item hold oldContent with oldFields merged in, and
// records that as the text it was seeded with.
func stageOldSeed(t *testing.T, srv *Server, it models.Item, key, oldContent string, oldFields map[string]any) models.Item {
	t.Helper()
	e, _ := collections.LookupBuiltin(key)
	var fields map[string]any
	if err := json.Unmarshal([]byte(e.Fields), &fields); err != nil {
		t.Fatal(err)
	}
	for k, v := range oldFields {
		fields[k] = v
	}
	fb, _ := json.Marshal(fields)
	old := collections.BuiltinEntry{Key: key, Content: oldContent, Fields: string(fb)}
	if err := srv.store.SetItemBuiltinSeed(it.ID, models.BuiltinOrigin{Key: key, SeedHash: old.Hash(), SeedContent: oldContent, SeedFields: string(fb)}); err != nil {
		t.Fatal(err)
	}
	content := oldContent
	patch := map[string]any{}
	for k, v := range oldFields {
		patch[k] = v
	}
	if _, err := srv.store.UpdateItem(it.ID, models.ItemUpdate{Content: &content, FieldsPatch: patch}); err != nil {
		t.Fatal(err)
	}
	got, err := srv.store.GetItem(it.ID)
	if err != nil || got == nil {
		t.Fatalf("reload: %v", err)
	}
	return *got
}

func task3462Update(srv *Server, ws, ref string, body any) *task3462Result {
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/items/"+ref+"/builtin/update", body)
	return &task3462Result{code: rr.Code, body: rr.Body.String()}
}

type task3462Result struct {
	code int
	body string
}

func TestTASK3462_StateAndUpdate(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		slug, _ := task3462Workspace(t, srv, "Update 3462", "startup")
		ship := task3462ItemByTitle(t, srv, slug, "playbooks", "Ship tasks")
		lib, _ := collections.LookupBuiltin("playbook/ship")

		// Fresh from the template: current, nothing offered.
		st := task3462GetState(t, srv, slug, ship.Slug)
		if st.State != collections.BuiltinCurrent || st.Library != nil || st.SeedHash != lib.Hash() {
			t.Fatalf("fresh seed: %+v", st)
		}
		if r := task3462Update(srv, slug, ship.Slug, map[string]any{"expected_seq": st.Seq}); r.code != http.StatusConflict || !strings.Contains(r.body, "builtin_up_to_date") {
			t.Fatalf("update of a current item: %d %s", r.code, r.body)
		}

		// Seeded before a library fix, never edited: update_available. The
		// user deprecated it; the update must leave that alone.
		old := stageOldSeed(t, srv, ship, "playbook/ship", "the old ship body", map[string]any{"legacy_note": "x"})
		if _, err := srv.store.UpdateItem(old.ID, models.ItemUpdate{FieldsPatch: map[string]any{"status": "deprecated"}}); err != nil {
			t.Fatal(err)
		}
		st = task3462GetState(t, srv, slug, ship.Slug)
		if st.State != collections.BuiltinUpdateAvailable {
			t.Fatalf("staged old seed: state %s, want update_available", st.State)
		}
		if st.Library == nil || st.Library.Content != lib.Content || st.Seed == nil || st.Seed.Content != "the old ship body" {
			t.Fatalf("preview lacks the library or the seed text: %+v", st)
		}

		// The version token is required and checked.
		if r := task3462Update(srv, slug, ship.Slug, map[string]any{}); r.code != http.StatusBadRequest {
			t.Fatalf("no expected_seq: %d %s", r.code, r.body)
		}
		if r := task3462Update(srv, slug, ship.Slug, map[string]any{"expected_seq": st.Seq - 1}); r.code != http.StatusConflict || !strings.Contains(r.body, "update_conflict") {
			t.Fatalf("stale expected_seq: %d %s", r.code, r.body)
		}
		if after := task3462GetState(t, srv, slug, ship.Slug); after.State != collections.BuiltinUpdateAvailable {
			t.Fatalf("a refused update changed the item: %s", after.State)
		}

		r := task3462Update(srv, slug, ship.Slug, map[string]any{"expected_seq": st.Seq})
		if r.code != http.StatusOK {
			t.Fatalf("update: %d %s", r.code, r.body)
		}
		got, _ := srv.store.GetItem(ship.ID)
		if got.Content != lib.Content {
			t.Error("the body is not the library's")
		}
		var f map[string]any
		_ = json.Unmarshal([]byte(got.Fields), &f)
		if f["status"] != "deprecated" {
			t.Errorf("status = %v, want the user's deprecated kept", f["status"])
		}
		if _, ok := f["legacy_note"]; ok {
			t.Error("a field the seed had and the library dropped was kept")
		}
		after := task3462GetState(t, srv, slug, ship.Slug)
		if after.State != collections.BuiltinCurrent || after.SeedHash != lib.Hash() {
			t.Fatalf("after the update: %+v", after)
		}
		o, _ := srv.store.GetItemBuiltinOrigin(ship.ID)
		if o.SeedContent != lib.Content {
			t.Error("the seed text did not move to the library's")
		}
	})
}

// An item edited since it was seeded is diverged; the update replaces the
// edit, which stays in the item's history.
func TestTASK3462_DivergedUpdateKeepsHistory(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		slug, _ := task3462Workspace(t, srv, "Diverged 3462", "startup")
		plan := task3462ItemByTitle(t, srv, slug, "playbooks", "Plan a new initiative")
		old := stageOldSeed(t, srv, plan, "playbook/plan", "the old plan body", nil)
		edited := "the old plan body, with my team's own step"
		if _, err := srv.store.UpdateItem(old.ID, models.ItemUpdate{Content: &edited, ForceVersion: true}); err != nil {
			t.Fatal(err)
		}
		st := task3462GetState(t, srv, slug, plan.Slug)
		if st.State != collections.BuiltinDiverged || st.Seed == nil || st.Seed.Content != "the old plan body" {
			t.Fatalf("edited after an old seed: %+v", st)
		}
		if r := task3462Update(srv, slug, plan.Slug, map[string]any{"expected_seq": st.Seq}); r.code != http.StatusOK {
			t.Fatalf("update: %d %s", r.code, r.body)
		}
		current, _ := srv.store.GetItem(plan.ID)
		versions, err := srv.store.ListItemVersionsResolved(plan.ID, current.Content)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, v := range versions {
			if v.Content == edited {
				found = true
			}
		}
		if !found {
			t.Error("the user's edit is not in the item's history after the update")
		}
	})
}

// A convention's typed metadata is part of what an update writes; after it
// the item holds the library's text exactly.
func TestTASK3462_ConventionUpdateWritesTypedMetadata(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		slug, _ := task3462Workspace(t, srv, "Convention 3462", "startup")
		key := "convention/conventional-commit-format"
		conv := task3462ItemByTitle(t, srv, slug, "conventions", "Conventional commit format")
		lib, _ := collections.LookupBuiltin(key)
		oldMeta := map[string]any{"category": "git", "trigger": "on-commit", "commands": []string{"git commit --old"}}
		// The reserved key cannot be patched by a caller; the store can.
		old := stageOldSeed(t, srv, conv, key, "old convention body", map[string]any{
			"commands": []string{"git commit --old"}, "convention": oldMeta,
		})
		st := task3462GetState(t, srv, slug, old.Slug)
		if st.State != collections.BuiltinUpdateAvailable {
			t.Fatalf("staged: %s", st.State)
		}
		if r := task3462Update(srv, slug, old.Slug, map[string]any{"expected_seq": st.Seq}); r.code != http.StatusOK {
			t.Fatalf("update: %d %s", r.code, r.body)
		}
		got, _ := srv.store.GetItem(conv.ID)
		if h, _ := lib.ItemStateHash(got.Content, got.Fields); h != lib.Hash() {
			t.Fatalf("after the update the item does not hold the library text: %s", got.Fields)
		}
		// And a caller still cannot patch the reserved key directly.
		rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+conv.Slug, map[string]any{
			"fields_patch": map[string]any{"convention": map[string]any{"category": "x"}},
		})
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("a caller patched the convention key: %d %s", rr.Code, rr.Body.String())
		}
	})
}

func TestTASK3462_NotBuiltinAndListing(t *testing.T) {
	srv := testServer(t)
	slug, wsID := task3462Workspace(t, srv, "List 3462", "startup")
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]any{"title": "Plain task"})
	if rr.Code != http.StatusCreated {
		t.Fatal(rr.Body.String())
	}
	var plain models.Item
	parseJSON(t, rr, &plain)
	if rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items/"+plain.Slug+"/builtin", nil); rr.Code != http.StatusNotFound || !strings.Contains(rr.Body.String(), "not_builtin") {
		t.Fatalf("an ordinary item: %d %s", rr.Code, rr.Body.String())
	}

	// An item naming a built-in this Pad does not ship.
	ship := task3462ItemByTitle(t, srv, slug, "playbooks", "Ship tasks")
	if _, err := srv.store.DB().Exec(`UPDATE item_builtin_origin SET builtin_key = 'acme/playbook/deploy' WHERE item_id = ?`, ship.ID); err != nil {
		t.Fatal(err)
	}
	st := task3462GetState(t, srv, slug, ship.Slug)
	if st.State != collections.BuiltinUnknownEntry {
		t.Fatalf("unknown key: %s", st.State)
	}
	if r := task3462Update(srv, slug, ship.Slug, map[string]any{"expected_seq": st.Seq}); r.code != http.StatusConflict || !strings.Contains(r.body, "builtin_unknown") {
		t.Fatalf("update of an unknown built-in: %d %s", r.code, r.body)
	}

	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/builtins", nil)
	if rr.Code != http.StatusOK {
		t.Fatal(rr.Body.String())
	}
	var list []builtinListEntry
	parseJSON(t, rr, &list)
	origins, _ := srv.store.WorkspaceBuiltinOrigins(wsID)
	if len(list) != len(origins) || len(list) == 0 {
		t.Fatalf("listing has %d rows, the workspace has %d origins", len(list), len(origins))
	}
	for _, e := range list {
		if e.Ref == "" || e.Key == "" || e.State == "" {
			t.Errorf("incomplete row %+v", e)
		}
	}
}

// The listing never names an item its caller cannot see: an item-grant guest
// granted one playbook sees no rows, and a viewer may read but not update.
func TestTASK3462_ListingAndUpdateRespectAccess(t *testing.T) {
	srv := testServer(t)
	ownerCookie := bootstrapFirstUser(t, srv, "owner@test.com", "Owner")
	rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces", map[string]string{"name": "Access 3462", "template": "startup"}, ownerCookie)
	if rr.Code != http.StatusCreated {
		t.Fatal(rr.Body.String())
	}
	var ws models.Workspace
	parseJSON(t, rr, &ws)
	owner, _ := srv.store.GetUserByEmail("owner@test.com")
	ship, err := srv.store.ResolveItem(ws.ID, "PLAYB-1")
	if err != nil || ship == nil {
		items, _ := srv.store.ListItems(ws.ID, models.ItemListParams{CollectionSlug: "playbooks"})
		for i := range items {
			if items[i].Title == "Ship tasks" {
				ship = &items[i]
			}
		}
	}
	if ship == nil {
		t.Fatal("no ship playbook")
	}
	bearer := func(email, role string, grantItem bool) string {
		u, err := srv.store.CreateUser(models.UserCreate{Email: email, Name: email, Password: "correct-horse-battery-staple"})
		if err != nil {
			t.Fatal(err)
		}
		if grantItem {
			if _, err := srv.store.CreateItemGrant(ws.ID, ship.ID, u.ID, "view", owner.ID); err != nil {
				t.Fatal(err)
			}
		} else if err := srv.store.AddWorkspaceMember(ws.ID, u.ID, role); err != nil {
			t.Fatal(err)
		}
		tok, err := srv.store.CreateAPIToken(u.ID, models.APITokenCreate{Name: "t"}, 30, 0)
		if err != nil {
			t.Fatal(err)
		}
		return tok.Token
	}
	guest := bearer("guest@test.com", "", true)
	viewer := bearer("viewer@test.com", "viewer", false)
	as := func(tok, method, path string, body any) (int, string) {
		rr := doRequestWithHeaders(srv, method, path, body, map[string]string{"Authorization": "Bearer " + tok})
		return rr.Code, rr.Body.String()
	}
	base := "/api/v1/workspaces/" + ws.Slug

	code, body := as(guest, "GET", base+"/builtins", nil)
	if code != http.StatusOK {
		t.Fatalf("guest listing: %d %s", code, body)
	}
	var list []builtinListEntry
	_ = json.Unmarshal([]byte(body), &list)
	if len(list) != 0 {
		t.Errorf("an item-grant guest was shown %d rows: %s", len(list), body)
	}
	// The granted item's own state is readable to it.
	if code, body := as(guest, "GET", base+"/items/"+ship.Slug+"/builtin", nil); code != http.StatusOK {
		t.Errorf("guest reading its granted item's state: %d %s", code, body)
	}

	code, body = as(viewer, "GET", base+"/items/"+ship.Slug+"/builtin", nil)
	if code != http.StatusOK {
		t.Fatalf("viewer read: %d %s", code, body)
	}
	var st builtinStateResponse
	_ = json.Unmarshal([]byte(body), &st)
	if code, body := as(viewer, "POST", base+"/items/"+ship.Slug+"/builtin/update", map[string]any{"expected_seq": st.Seq}); code != http.StatusForbidden {
		t.Errorf("viewer update: %d %s, want 403", code, body)
	}
}

// With the item open in an editor the body travels through the live document
// (the applier path), and the row write lands first. If the apply then fails,
// the item keeps its old body, so its seed must stay where it was and the
// update must still be on offer (codex r2: the seed had committed with the
// row and every later offer answered builtin_up_to_date).
func TestTASK3462_FailedApplyLeavesTheSeedAndTheOffer(t *testing.T) {
	restore := collab.SetApplierTimeoutsForTesting(150*time.Millisecond, 150*time.Millisecond)
	t.Cleanup(restore)
	srv := testServerWithCollab(t)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	slug, _ := task3462Workspace(t, srv, "Applier 3462", "startup")
	ship := task3462ItemByTitle(t, srv, slug, "playbooks", "Ship tasks")
	stageOldSeed(t, srv, ship, "playbook/ship", "the old ship body", nil)
	before, _ := srv.store.GetItemBuiltinOrigin(ship.ID)

	conn, resp, err := dialCollab(t, ts.URL, ship.ID, nil, "")
	if err != nil {
		status := ""
		if resp != nil {
			status = resp.Status
		}
		t.Fatalf("dialCollab: %v (%s)", err, status)
	}
	stop := silentApplier(t, conn)
	t.Cleanup(stop)
	deadline := time.Now().Add(3 * time.Second)
	for !srv.collab.HasElectableApplier(ship.ID) {
		if time.Now().After(deadline) {
			t.Fatal("the conn never became electable; this would measure the direct path")
		}
		time.Sleep(2 * time.Millisecond)
	}

	st := task3462GetState(t, srv, slug, ship.Slug)
	if st.State != collections.BuiltinUpdateAvailable {
		t.Fatalf("staged: %s", st.State)
	}
	r := task3462Update(srv, slug, ship.Slug, map[string]any{"expected_seq": st.Seq})
	if r.code != http.StatusConflict || !strings.Contains(r.body, "content_not_applied") {
		t.Fatalf("an update whose apply fails: %d %s, want 409 content_not_applied", r.code, r.body)
	}
	after, _ := srv.store.GetItemBuiltinOrigin(ship.ID)
	if after == nil || after.SeedHash != before.SeedHash {
		t.Fatalf("the seed moved although the body did not land: %+v, want %+v", after, before)
	}
	if again := task3462GetState(t, srv, slug, ship.Slug); again.State != collections.BuiltinUpdateAvailable {
		t.Fatalf("after a failed apply the offer is gone: %s", again.State)
	}
}
