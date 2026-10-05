package server

import (
	"net/http"
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
	exec(`INSERT INTO workspace_members (workspace_id, user_id, role, collection_access, created_at) VALUES (?, ?, 'viewer', 'all', ?)`,
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
	}
	check("Portal")

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
