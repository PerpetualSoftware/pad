package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/events"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/watchevents"
)

// TASK-3401 U6b: the app API's writes.

func appDo(srv *Server, method, path, token string, body any, headers ...string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		if s, ok := body.(string); ok {
			buf.WriteString(s)
		} else {
			_ = json.NewEncoder(&buf).Encode(body)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

func (f appAPIFix) etag(t *testing.T, itemID string) string {
	t.Helper()
	var it AppItem
	_ = json.Unmarshal(appGet(f.srv, f.path("/items/"+itemID), f.token).Body.Bytes(), &it)
	if it.ETag == "" {
		t.Fatalf("no etag for %s", itemID)
	}
	return it.ETag
}

func (f appAPIFix) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := f.srv.store.DB().QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Every write row: its happy path, and its response's key set is its DTO's.
func TestTask3401b_WriteCensus(t *testing.T) {
	f := appAPIFixture(t, "write")
	seen := map[string]bool{}

	rr := appDo(f.srv, "POST", f.path("/collections/requests/items"), f.token, map[string]any{"title": "From the app", "content": "Hello", "fields": map[string]any{"status": "open", "size": "S"}})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	var created map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &created)
	checkItem(t, "appCreateItem", created)
	if created["via_app"] != f.in.id {
		t.Errorf("created via_app = %v", created["via_app"])
	}
	seen["appCreateItem"] = true
	id := created["id"].(string)

	rr = appDo(f.srv, "PATCH", f.path("/items/"+id), f.token, map[string]any{"fields_patch": map[string]any{"size": "M"}, "expected_etag": created["etag"]})
	if rr.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rr.Code, rr.Body.String())
	}
	var updated map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &updated)
	checkItem(t, "appUpdateItem", updated)
	if updated["fields"].(map[string]any)["size"] != "M" {
		t.Errorf("update did not apply: %v", updated["fields"])
	}
	seen["appUpdateItem"] = true

	rr = appDo(f.srv, "POST", f.path("/items/"+id+"/comments"), f.token, map[string]any{"body": "on it"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("comment: %d %s", rr.Code, rr.Body.String())
	}
	var comment map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &comment)
	assertKeysWithin(t, "appCreateComment", comment, "comment", appDTOKeys["comment"])
	if comment["author_kind"] != "app" || comment["author_display"] != f.in.bot.Name {
		t.Errorf("comment author = %v / %v, want app / %q (lead ruling R3)", comment["author_kind"], comment["author_display"], f.in.bot.Name)
	}
	seen["appCreateComment"] = true
	cid := comment["id"].(string)

	rr = appDo(f.srv, "PATCH", f.path("/items/"+id+"/comments/"+cid), f.token, map[string]any{"body": "on it, done"})
	if rr.Code != http.StatusOK {
		t.Fatalf("comment update: %d %s", rr.Code, rr.Body.String())
	}
	seen["appUpdateComment"] = true

	rr = appDo(f.srv, "DELETE", f.path("/items/"+id+"/comments/"+cid), f.token, nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("comment delete: %d %s", rr.Code, rr.Body.String())
	}
	seen["appDeleteComment"] = true

	for _, rt := range appRoutes {
		if rt.Access == "write" && !seen[rt.Name] {
			t.Errorf("write row %s is not in the census", rt.Name)
		}
	}
}

// Request DTOs: their field sets are the spec table's.
func TestTask3401b_RequestDTOFieldSets(t *testing.T) {
	check := func(name string, v any, want ...string) {
		got := []string{}
		tp := reflect.TypeOf(v)
		for i := 0; i < tp.NumField(); i++ {
			got = append(got, strings.Split(tp.Field(i).Tag.Get("json"), ",")[0])
		}
		sort.Strings(got)
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s fields = %v, want %v", name, got, want)
		}
	}
	check("AppItemCreateRequest", AppItemCreateRequest{}, "title", "content", "fields")
	check("AppItemUpdateRequest", AppItemUpdateRequest{}, "fields_patch", "expected_etag")
	check("AppCommentCreateRequest", AppCommentCreateRequest{}, "body", "parent_comment_id")
	check("AppCommentUpdateRequest", AppCommentUpdateRequest{}, "body")
}

// Every write row refuses a READ token with 403, and nothing is written.
func TestTask3401b_EveryWriteRowRefusesAReadToken(t *testing.T) {
	f := appAPIFixture(t, "read")
	items := f.count(t, `SELECT COUNT(*) FROM items`)
	comments := f.count(t, `SELECT COUNT(*) FROM comments`)
	calls := map[string][2]string{
		"appCreateItem":    {"POST", "/collections/requests/items"},
		"appUpdateItem":    {"PATCH", "/items/" + f.item.ID},
		"appCreateComment": {"POST", "/items/" + f.item.ID + "/comments"},
		"appUpdateComment": {"PATCH", "/items/" + f.item.ID + "/comments/" + f.comment.ID},
		"appDeleteComment": {"DELETE", "/items/" + f.item.ID + "/comments/" + f.comment.ID},
	}
	for _, rt := range appRoutes {
		if rt.Access != "write" {
			continue
		}
		c, ok := calls[rt.Name]
		if !ok {
			t.Errorf("write row %s is not exercised", rt.Name)
			continue
		}
		rr := appDo(f.srv, c[0], f.path(c[1]), f.token, map[string]any{"title": "x", "body": "x", "fields_patch": map[string]any{"size": "x"}, "expected_etag": "x"})
		if rr.Code != http.StatusForbidden {
			t.Errorf("%s with a read token: %d %s, want 403", rt.Name, rr.Code, rr.Body.String())
		}
	}
	if items != f.count(t, `SELECT COUNT(*) FROM items`) || comments != f.count(t, `SELECT COUNT(*) FROM comments`) {
		t.Error("a read token wrote something")
	}
}

// The write rule, both times: a collection inside the read ceiling but not a
// companion (the system collection) is refused, by requireEditPermission
// ahead of the editor fast path, for a bot whose membership reaches it.
func TestTask3401b_TheWriteRule(t *testing.T) {
	f := appAPIFixture(t, "write")
	rr := appDo(f.srv, "POST", f.path("/collections/"+f.system.Slug+"/items"), f.token, map[string]any{"title": "Into the system"})
	if rr.Code != http.StatusForbidden {
		t.Errorf("create in a non-companion collection: %d %s, want 403", rr.Code, rr.Body.String())
	}
	if n := f.count(t, `SELECT COUNT(*) FROM items WHERE collection_id = ?`, f.system.ID); n != 0 {
		t.Errorf("%d items written outside the companions", n)
	}
	// The rule inside requireEditPermission itself, for a bot that is an
	// editor with "all" access, so only the rule can refuse it. The companion
	// call is the control: the same request is allowed there.
	req := httptest.NewRequest("POST", "/x", nil)
	ctx := withAppContextForTest(req, &appContext{Companions: []string{f.companion.ID}})
	ctx = WithCurrentUser(ctx, f.in.bot)
	ctx = context.WithValue(ctx, ctxWorkspaceRole, "editor")
	req = req.WithContext(ctx)
	if !f.srv.requireEditPermission(httptest.NewRecorder(), req, f.ws.ID, "", f.companion.ID) {
		t.Fatal("control: requireEditPermission refused the app in its own companion")
	}
	if f.srv.requireEditPermission(httptest.NewRecorder(), req, f.ws.ID, "", f.system.ID) {
		t.Error("requireEditPermission let an app write outside its companions")
	}
}

func TestTask3401b_UpdateRefusals(t *testing.T) {
	f := appAPIFixture(t, "write")
	human := createTestUserDirect(t, f.srv, "maker-3401b@example.com")
	humanItem, err := f.srv.store.CreateItem(f.ws.ID, f.companion.ID, models.ItemCreate{Title: "Human made", ActorUserID: human.ID})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		item   string
		body   any
		status int
	}{
		{"an item this app did not create", humanItem.ID, map[string]any{"fields_patch": map[string]any{"size": "M"}, "expected_etag": f.etag(t, humanItem.ID)}, http.StatusForbidden},
		{"a stale etag", f.item.ID, map[string]any{"fields_patch": map[string]any{"size": "M"}, "expected_etag": "00000000000000000000000000000000"}, http.StatusPreconditionFailed},
		{"no etag", f.item.ID, map[string]any{"fields_patch": map[string]any{"size": "M"}}, http.StatusBadRequest},
		{"an unknown request field", f.item.ID, map[string]any{"fields_patch": map[string]any{"size": "M"}, "expected_etag": f.etag(t, f.item.ID), "title": "renamed"}, http.StatusBadRequest},
		{"a relation key", f.item.ID, map[string]any{"fields_patch": map[string]any{"owner": f.privateItem.ID}, "expected_etag": f.etag(t, f.item.ID)}, http.StatusBadRequest},
		{"an attachment reference", f.item.ID, map[string]any{"fields_patch": map[string]any{"size": "pad-attachment:abc"}, "expected_etag": f.etag(t, f.item.ID)}, http.StatusBadRequest},
	}
	for _, tc := range cases {
		rr := appDo(f.srv, "PATCH", f.path("/items/"+tc.item), f.token, tc.body)
		if rr.Code != tc.status {
			t.Errorf("%s: %d %s, want %d", tc.name, rr.Code, rr.Body.String(), tc.status)
		}
	}
	for name, body := range map[string]any{
		"an attachment reference in content": map[string]any{"title": "x", "content": "see pad-attachment:abc"},
		"a cross-workspace link":             map[string]any{"title": "x", "content": "see [[other::TASK-1]]"},
		"an unknown request field":           map[string]any{"title": "x", "parent": "y"},
	} {
		if rr := appDo(f.srv, "POST", f.path("/collections/requests/items"), f.token, body); rr.Code != http.StatusBadRequest {
			t.Errorf("create with %s: %d %s, want 400", name, rr.Code, rr.Body.String())
		}
	}
}

func TestTask3401b_CommentRules(t *testing.T) {
	f := appAPIFixture(t, "write")
	// A person's comment on the app's item: the app may not change it.
	if rr := appDo(f.srv, "PATCH", f.path("/items/"+f.item.ID+"/comments/"+f.comment.ID), f.token, map[string]any{"body": "edited by the app"}); rr.Code != http.StatusForbidden {
		t.Errorf("edit a person's comment: %d %s, want 403", rr.Code, rr.Body.String())
	}
	if rr := appDo(f.srv, "DELETE", f.path("/items/"+f.item.ID+"/comments/"+f.comment.ID), f.token, nil); rr.Code != http.StatusForbidden {
		t.Errorf("delete a person's comment: %d %s, want 403", rr.Code, rr.Body.String())
	}
	// A comment that is not on the item in the path: the comment 404.
	human := createTestUserDirect(t, f.srv, "elsewhere-3401b@example.com")
	other, err := f.srv.store.CreateItem(f.ws.ID, f.companion.ID, models.ItemCreate{Title: "Other", ActorUserID: human.ID})
	if err != nil {
		t.Fatal(err)
	}
	if rr := appDo(f.srv, "PATCH", f.path("/items/"+other.ID+"/comments/"+f.comment.ID), f.token, map[string]any{"body": "x"}); rr.Code != http.StatusNotFound {
		t.Errorf("a comment under another item's path: %d %s, want 404", rr.Code, rr.Body.String())
	}
}

// The mid-request disable race: a write admitted by the middleware, with a
// disable committed before its handler runs, is refused inside its FencedTx
// and writes nothing.
func TestTask3401b_ADisableBeforeTheHandlerRefusesTheWrite(t *testing.T) {
	for name, change := range map[string]string{
		"disable": `UPDATE app_installs SET state = 'disabling', auth_epoch = auth_epoch + 1 WHERE id = ?`,
		"rotate":  `UPDATE app_installs SET auth_epoch = auth_epoch + 1 WHERE id = ?`,
	} {
		t.Run(name, func(t *testing.T) {
			f := appAPIFixture(t, "write")
			before := f.count(t, `SELECT COUNT(*) FROM items`)
			f.srv.appBeforeHandler = func() {
				if _, err := f.srv.store.DB().Exec(change, f.in.id); err != nil {
					t.Error(err)
				}
			}
			rr := appDo(f.srv, "POST", f.path("/collections/requests/items"), f.token, map[string]any{"title": "Racing"})
			if rr.Code != http.StatusUnauthorized {
				t.Errorf("%s before the handler: %d %s, want 401", name, rr.Code, rr.Body.String())
			}
			if after := f.count(t, `SELECT COUNT(*) FROM items`); after != before {
				t.Errorf("%s before the handler: %d items written", name, after-before)
			}
		})
	}
}

// Parity: an app write publishes the SSE events and watch notifications its
// human twin would, and X-Pad-Agent does not change the actor.
func TestTask3401b_SSEAndWatchParity(t *testing.T) {
	f := appAPIFixture(t, "write")
	bus := events.New()
	f.srv.SetEventBus(bus)
	watch := watchevents.New()
	f.srv.SetWatchEventsBus(watch)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sse, _, _ := bus.Subscribe(ctx, f.ws.ID)
	notes, _, err := watch.Subscribe()
	if err != nil {
		t.Fatal(err)
	}

	etag := f.etag(t, f.item.ID)
	rr := appDo(f.srv, "PATCH", f.path("/items/"+f.item.ID), f.token, map[string]any{"fields_patch": map[string]any{"status": "done"}, "expected_etag": etag}, "X-Pad-Agent", "spoofed-agent")
	if rr.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rr.Code, rr.Body.String())
	}
	rr = appDo(f.srv, "POST", f.path("/items/"+f.item.ID+"/comments"), f.token, map[string]any{"body": "closing"}, "X-Pad-Agent", "spoofed-agent")
	if rr.Code != http.StatusCreated {
		t.Fatalf("comment: %d %s", rr.Code, rr.Body.String())
	}

	gotSSE := map[string]bool{}
	gotWatch := map[string]bool{}
	deadline := time.After(3 * time.Second)
	for len(gotSSE) < 2 || len(gotWatch) < 2 {
		select {
		case e := <-sse:
			gotSSE[e.Type] = true
			if strings.Contains(e.Actor, "spoofed") || strings.Contains(e.ActorName, "spoofed") {
				t.Errorf("SSE actor taken from X-Pad-Agent: %+v", e)
			}
		case n := <-notes:
			gotWatch[string(n.Kind)] = true
			if strings.Contains(n.ActorName, "spoofed") {
				t.Errorf("watch actor taken from X-Pad-Agent: %+v", n)
			}
		case <-deadline:
			t.Fatalf("parity events missing: SSE %v, watch %v", gotSSE, gotWatch)
		}
	}
	if !gotSSE[sseItemUpdated] || !gotSSE[sseCommentCreated] {
		t.Errorf("SSE = %v, want item updated and comment created", gotSSE)
	}
	if !gotWatch[string(watchevents.KindStatusChange)] || !gotWatch[string(watchevents.KindComment)] {
		t.Errorf("watch = %v, want a status change and a comment (lead ruling R1)", gotWatch)
	}
	var createdBy string
	if err := f.srv.store.DB().QueryRow(`SELECT created_by FROM comments WHERE item_id = ? AND body = 'closing'`, f.item.ID).Scan(&createdBy); err != nil {
		t.Fatal(err)
	}
	if createdBy != "agent" {
		t.Errorf("comment created_by = %q, want agent", createdBy)
	}
}

// lazyBody produces a request body only when the handler reads it, which is
// after the handler's own read of the item: build runs in that gap.
type lazyBody struct {
	build func() []byte
	r     *bytes.Reader
}

func (b *lazyBody) Read(p []byte) (int, error) {
	if b.r == nil {
		b.r = bytes.NewReader(b.build())
	}
	return b.r.Read(p)
}

// Codex U6b round 1 P2: a status another writer changed between the
// handler's read and the app's commit is not the app's change. The app
// patches only size; no status notification may name it.
func TestTask3401b_AnotherWritersStatusChangeIsNotTheApps(t *testing.T) {
	f := appAPIFixture(t, "write")
	watch := watchevents.New()
	f.srv.SetWatchEventsBus(watch)
	notes, _, err := watch.Subscribe()
	if err != nil {
		t.Fatal(err)
	}
	body := &lazyBody{build: func() []byte {
		status := "done"
		if _, err := f.srv.store.UpdateItem(f.item.ID, models.ItemUpdate{FieldsPatch: map[string]interface{}{"status": status}}); err != nil {
			t.Error(err)
		}
		// Drain the human write's own notifications, if any were published.
		for len(notes) > 0 {
			<-notes
		}
		b, _ := json.Marshal(map[string]any{"fields_patch": map[string]any{"size": "M"}, "expected_etag": f.etag(t, f.item.ID)})
		return b
	}}
	req := httptest.NewRequest("PATCH", f.path("/items/"+f.item.ID), body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	f.srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rr.Code, rr.Body.String())
	}
	if body.r == nil {
		t.Fatal("control: the handler never read the body, so the race did not run")
	}
	select {
	case n := <-notes:
		t.Errorf("a size-only app write published %+v", n)
	case <-time.After(500 * time.Millisecond):
	}
}

// postCommitBus runs after once, on the first event published: the publish
// follows the write's commit, so after injects a failure that only a
// post-commit step can meet.
type postCommitBus struct {
	events.EventBus
	after func()
	done  bool
}

func (b *postCommitBus) Publish(e events.Event) error {
	if !b.done {
		b.done = true
		b.after()
	}
	return b.EventBus.Publish(e)
}

// Codex U6b round 2 P2: a write's response must not depend on a read after
// its commit. With the database gone after the commit, the create still
// answers 201 and describes the item it wrote; a 500 would tell the app its
// write failed and invite a duplicate.
func TestTask3401b_AFailureAfterCommitDoesNotHideTheWrite(t *testing.T) {
	f := appAPIFixture(t, "write")
	var id string
	f.srv.SetEventBus(&postCommitBus{EventBus: events.New(), after: func() {
		if err := f.srv.store.DB().QueryRow(`SELECT id FROM items WHERE title = 'Committed'`).Scan(&id); err != nil {
			t.Errorf("control: the item is not committed when the publish runs: %v", err)
		}
		_ = f.srv.store.DB().Close()
	}})
	rr := appDo(f.srv, "POST", f.path("/collections/requests/items"), f.token, map[string]any{"title": "Committed", "fields": map[string]any{"size": "S"}})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create after a post-commit failure: %d %s, want 201", rr.Code, rr.Body.String())
	}
	var got AppItem
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got.ID != id || got.ViaApp != f.in.id || got.CreatedByDisplay != f.in.bot.Name || got.ETag == "" || got.Fields["size"] != "S" {
		t.Errorf("response = %+v, want the committed item %s by %q via %s", got, id, f.in.bot.Name, f.in.id)
	}
}

// Codex U6b round 2 P2: the response projects fields by the schema the
// write validated against, read in its transaction, not the handler's
// earlier copy. A field added with a default between the two is stored,
// and must be in the response.
func TestTask3401b_TheResponseUsesTheWritesSchema(t *testing.T) {
	f := appAPIFixture(t, "write")
	body := &lazyBody{build: func() []byte {
		var schema models.CollectionSchema
		if err := json.Unmarshal([]byte(f.companion.Schema), &schema); err != nil {
			t.Fatal(err)
		}
		schema.Fields = append(schema.Fields, models.FieldDef{Key: "fresh", Label: "Fresh", Type: "text", Default: "stored default"})
		b, _ := json.Marshal(schema)
		if _, err := f.srv.store.DB().Exec(`UPDATE collections SET schema = ? WHERE id = ?`, string(b), f.companion.ID); err != nil {
			t.Fatal(err)
		}
		out, _ := json.Marshal(map[string]any{"title": "After the schema change"})
		return out
	}}
	req := httptest.NewRequest("POST", f.path("/collections/requests/items"), body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	f.srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	var got AppItem
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	var stored string
	if err := f.srv.store.DB().QueryRow(`SELECT fields FROM items WHERE id = ?`, got.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stored, "stored default") {
		t.Fatalf("control: the default was not stored (%s), so the race did not run", stored)
	}
	if got.Fields["fresh"] != "stored default" {
		t.Errorf("response fields = %v, want the stored default under the write's schema", got.Fields)
	}
}

// Lead ruling R3, U5b half: a DELEGATED write's author is the person, with
// via_app set ("Dave via Support Portal"), never the bot. Enabled by
// TASK-3399, which brings delegated tokens.
func TestTask3401_DelegatedWriteAuthorIsThePerson(t *testing.T) {
	t.Skip("TASK-3399 (U5b): delegated tokens are refused until then; enable this with them")
}
