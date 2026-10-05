package server

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// TASK-3413 (SPEC-6 U9c): human-facing item, comment and version JSON name
// the installed app that wrote them, and the name outlives the install.

func TestTask3413_ViaAppOnHumanJSON(t *testing.T) {
	f := appAPIFixture(t, "read")
	db := f.srv.store.DB()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	viewer, tok := loginTestUserAs(t, f.srv, "viewer-3413@example.com", "Vera Viewer", "pw-3413-viewer")
	exec(`INSERT INTO workspace_members (workspace_id, user_id, role, collection_access, created_at) VALUES (?, ?, 'editor', 'all', ?)`,
		f.ws.ID, viewer.ID, time.Now().UTC().Format(time.RFC3339))
	exec(`UPDATE comments SET via_app = ? WHERE id = ?`, f.in.id, f.comment.ID)
	exec(`UPDATE item_versions SET via_app = ? WHERE item_id = ?`, f.in.id, f.item.ID)
	var nVersions int
	if err := db.QueryRow(`SELECT COUNT(*) FROM item_versions WHERE item_id = ?`, f.item.ID).Scan(&nVersions); err != nil || nVersions == 0 {
		t.Fatalf("the fixture item has no version row to attribute (n=%d, err=%v)", nVersions, err)
	}
	// A human comment beside the app's.
	if _, err := f.srv.store.CreateComment(f.ws.ID, f.item.ID, viewer.ID, models.CommentCreate{Body: "mine", Author: "Vera", CreatedBy: "user"}); err != nil {
		t.Fatal(err)
	}
	base := "/api/v1/workspaces/" + f.ws.Slug
	get := func(path string, out any) {
		t.Helper()
		rr := doRequestWithCookie(f.srv, "GET", base+path, nil, tok)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, rr.Code, rr.Body.String())
		}
		parseJSON(t, rr, out)
	}
	want := func(where string, m map[string]any, name string) {
		t.Helper()
		if name == "" {
			if _, ok := m["via_app"]; ok {
				t.Errorf("%s: via_app present on a human write: %v", where, m["via_app"])
			}
			if _, ok := m["via_app_name"]; ok {
				t.Errorf("%s: via_app_name present on a human write", where)
			}
			return
		}
		if m["via_app"] != f.in.id || m["via_app_name"] != name {
			t.Errorf("%s: via_app=%v via_app_name=%v, want %s / %s", where, m["via_app"], m["via_app_name"], f.in.id, name)
		}
	}
	check := func(name string) {
		t.Helper()
		var item map[string]any
		get("/items/"+f.item.ID, &item)
		want("item GET", item, name)
		var other map[string]any
		get("/items/"+f.privateItem.ID, &other)
		want("human item GET", other, "")

		var list []map[string]any
		get("/collections/requests/items", &list)
		if len(list) != 1 {
			t.Fatalf("list: %d rows", len(list))
		}
		want("item list row", list[0], name)

		var comments []map[string]any
		get("/items/"+f.item.ID+"/comments", &comments)
		seen := 0
		for _, c := range comments {
			if c["id"] == f.comment.ID {
				want("app comment", c, name)
				seen++
			} else {
				want("human comment", c, "")
			}
		}
		if seen != 1 || len(comments) != 2 {
			t.Fatalf("comments: %d rows, app comment seen %d", len(comments), seen)
		}

		var versions []map[string]any
		get("/items/"+f.item.ID+"/versions", &versions)
		if len(versions) == 0 {
			t.Fatal("versions: none")
		}
		for _, v := range versions {
			want("version row", v, name)
		}
		vid, _ := versions[0]["id"].(string)
		var one map[string]any
		get("/items/"+f.item.ID+"/versions/"+vid, &one)
		want("version GET", one, name)
		var diff map[string]any
		get("/items/"+f.item.ID+"/versions/"+vid+"/diff", &diff)
		dv, _ := diff["version"].(map[string]any)
		want("version diff", dv, name)

		var tl struct {
			Entries []struct {
				Kind    string         `json:"kind"`
				Comment map[string]any `json:"comment"`
				Version map[string]any `json:"version"`
			} `json:"entries"`
		}
		get("/items/"+f.item.ID+"/timeline", &tl)
		var sawComment, sawVersion bool
		for _, e := range tl.Entries {
			switch {
			case e.Kind == "comment" && e.Comment["id"] == f.comment.ID:
				want("timeline comment", e.Comment, name)
				sawComment = true
			case e.Kind == "version" && e.Version != nil:
				want("timeline version", e.Version, name)
				sawVersion = true
			}
		}
		if !sawComment || !sawVersion {
			t.Fatalf("timeline: app comment %v, version %v", sawComment, sawVersion)
		}

		// Search and the outage /changes sync serve the item too (codex r1).
		var sr struct {
			Results []struct {
				Item map[string]any `json:"item"`
			} `json:"results"`
		}
		rr := doRequestWithCookie(f.srv, "GET", "/api/v1/search?q=Login&workspace="+f.ws.Slug, nil, tok)
		if rr.Code != http.StatusOK {
			t.Fatalf("search: %d %s", rr.Code, rr.Body.String())
		}
		parseJSON(t, rr, &sr)
		if len(sr.Results) != 1 {
			t.Fatalf("search: %d results", len(sr.Results))
		}
		want("search result", sr.Results[0].Item, name)
		var ch struct {
			Updated []map[string]any `json:"updated"`
		}
		get("/changes?since=0", &ch)
		sawChanged := false
		for _, it := range ch.Updated {
			if it["id"] == f.item.ID {
				want("changes row", it, name)
				sawChanged = true
			}
		}
		if !sawChanged {
			t.Fatal("changes: the item is missing")
		}
	}
	check("Portal")

	// A person's restore answers with the item; its creator is still the app.
	var versions []map[string]any
	get("/items/"+f.item.ID+"/versions", &versions)
	vid, _ := versions[len(versions)-1]["id"].(string)
	rr := doRequestWithCookie(f.srv, "POST", base+"/items/"+f.item.ID+"/versions/"+vid+"/restore", nil, tok)
	if rr.Code != http.StatusOK {
		t.Fatalf("restore: %d %s", rr.Code, rr.Body.String())
	}
	var restored map[string]any
	parseJSON(t, rr, &restored)
	want("restore response", restored, "Portal")
	exec(`UPDATE item_versions SET via_app = ? WHERE item_id = ?`, f.in.id, f.item.ID)

	// The bot's name is preferred; with none, the install's origin.
	exec(`UPDATE users SET name = '' WHERE id = ?`, f.in.bot.ID)
	check("https://portal.example")
	exec(`UPDATE users SET name = 'Portal' WHERE id = ?`, f.in.bot.ID)

	// Uninstall leaves a tombstone; the attribution and its name survive.
	if err := f.srv.store.BeginInstallTeardown(f.ws.ID, f.in.id, store.TeardownUninstall); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.store.UninstallAppTx(f.ws.ID, f.in.id); err != nil {
		t.Fatal(err)
	}
	if st, _ := f.srv.store.InstallState(f.ws.ID, f.in.id); st != store.InstallUninstalled {
		t.Fatalf("state %s, want uninstalled", st)
	}
	check("Portal")
}

// The lookup is chunked: one IN list per response would pass SQLite's
// 32,766-variable limit on a large unbounded response (codex r1 on U9c).
func TestTask3413_AttributionLookupIsChunked(t *testing.T) {
	f := appAPIFixture(t, "read")
	ids := make([]string, 0, 40001)
	for i := 0; i < 40000; i++ {
		ids = append(ids, fmt.Sprintf("no-such-item-%d", i))
	}
	ids = append(ids, f.item.ID)
	got, err := f.srv.store.ItemsCreatedViaApp(ids)
	if err != nil {
		t.Fatalf("lookup over 40,001 ids: %v", err)
	}
	if len(got) != 1 || got[f.item.ID].InstallID != f.in.id {
		t.Fatalf("got %v", got)
	}
}

// pad playbook show (and pad_playbook get) serves the playbook item outside
// the common enrichment path; it carries the creating app too (codex r1).
func TestTask3413_PlaybookShowCarriesViaApp(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	pb := createItem(t, srv, slug, "playbooks", map[string]interface{}{
		"title":  "Ship something",
		"fields": `{"status":"active","invocation_slug":"ship"}`,
	})
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil || ws == nil {
		t.Fatal(err)
	}
	ts := time.Now().UTC().Format(time.RFC3339)
	db := srv.store.DB()
	if _, err := db.Exec(`INSERT INTO app_installs (id, workspace_id, origin, created_at, updated_at) VALUES ('inst-pb', ?, 'https://pb.example', ?, ?)`, ws.ID, ts, ts); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE items SET created_via_app = 'inst-pb' WHERE id = ?`, pb.ID); err != nil {
		t.Fatal(err)
	}
	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/playbooks/ship", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("show: %d %s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	parseJSON(t, rr, &got)
	if got["via_app"] != "inst-pb" || got["via_app_name"] != "https://pb.example" {
		t.Fatalf("playbook show: via_app=%v via_app_name=%v", got["via_app"], got["via_app_name"])
	}
}

// The role board, a comment edit's response and the account export carry
// the attribution too (codex r2 on U9c).
func TestTask3413_RoleBoardCommentEditAndExport(t *testing.T) {
	f := appAPIFixture(t, "read")
	db := f.srv.store.DB()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	me, tok := loginTestUserAs(t, f.srv, "owner-3413@example.com", "Olive Owner", "pw-3413-owner")
	exec(`INSERT INTO workspace_members (workspace_id, user_id, role, collection_access, created_at) VALUES (?, ?, 'owner', 'all', ?)`,
		f.ws.ID, me.ID, time.Now().UTC().Format(time.RFC3339))
	exec(`UPDATE workspaces SET owner_id = ? WHERE id = ?`, me.ID, f.ws.ID)
	base := "/api/v1/workspaces/" + f.ws.Slug

	// Role board: the app-created item sits in the unassigned lane.
	rr := doRequestWithCookie(f.srv, "GET", base+"/roles/board", nil, tok)
	if rr.Code != http.StatusOK {
		t.Fatalf("board: %d %s", rr.Code, rr.Body.String())
	}
	var board struct {
		Lanes []struct {
			Items []map[string]any `json:"items"`
		} `json:"lanes"`
	}
	parseJSON(t, rr, &board)
	seen := false
	for _, l := range board.Lanes {
		for _, it := range l.Items {
			if it["id"] == f.item.ID {
				seen = true
				if it["via_app"] != f.in.id || it["via_app_name"] != "Portal" {
					t.Errorf("board row: via_app=%v via_app_name=%v", it["via_app"], it["via_app_name"])
				}
			}
		}
	}
	if !seen {
		t.Fatalf("board: the app item is missing: %s", rr.Body.String())
	}

	// A comment edit answers with the comment as the list serves it.
	c, err := f.srv.store.CreateComment(f.ws.ID, f.item.ID, me.ID, models.CommentCreate{Body: "first", Author: "Olive", CreatedBy: "user"})
	if err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE comments SET via_app = ? WHERE id = ?`, f.in.id, c.ID)
	rr = doRequestWithCookie(f.srv, "PATCH", base+"/items/"+f.item.ID+"/comments/"+c.ID, map[string]string{"body": "edited"}, tok)
	if rr.Code != http.StatusOK {
		t.Fatalf("comment edit: %d %s", rr.Code, rr.Body.String())
	}
	var edited map[string]any
	parseJSON(t, rr, &edited)
	if edited["via_app"] != f.in.id || edited["via_app_name"] != "Portal" {
		t.Errorf("comment edit: via_app=%v via_app_name=%v", edited["via_app"], edited["via_app_name"])
	}

	// The account export's item rows.
	rr = doRequestWithCookie(f.srv, "GET", "/api/v1/auth/export", nil, tok)
	if rr.Code != http.StatusOK {
		t.Fatalf("export: %d %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"via_app":"`+f.in.id+`","via_app_name":"Portal"`) {
		t.Errorf("export lacks the item's app attribution")
	}
}
