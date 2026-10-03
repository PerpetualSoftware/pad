package server

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3372: attribution (who wrote a comment, an item, a link or a version,
// and from which client) comes from the request, never the body. A body
// used to win on author, created_by, source, last_modified_by and
// version_source, so any editor could post as another member, or as an
// agent or a human at will. Each leg sends the forged fields and a control
// without them: both must stamp the same values, which proves the
// assertions read the stamping rather than a fixture default.

type attributionFixture struct {
	srv  *Server
	tok  string
	user *models.User
	ws   *models.Workspace
	coll *models.Collection
	item *models.Item
}

func newAttributionFixture(t *testing.T) attributionFixture {
	t.Helper()
	srv := testServer(t)
	user, tok := loginTestUserAs(t, srv, "alice3372@example.test", "Alice", "pw-3372-abcdefgh")
	ws := mustCreateOwnedWorkspace(t, srv, "Attribution 3372", user)
	coll := mustCollection(t, srv, ws.ID, "Tasks")
	item := mustItem(t, srv, ws.ID, coll.ID, "target")
	return attributionFixture{srv, tok, user, ws, coll, item}
}

func (f attributionFixture) do(t *testing.T, method, path string, body any) map[string]any {
	t.Helper()
	rr := doRequestWithCookie(f.srv, method, "/api/v1/workspaces/"+f.ws.Slug+path, body, f.tok)
	if rr.Code/100 != 2 {
		t.Fatalf("%s %s: %d %s", method, path, rr.Code, rr.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return out
}

var forgedAttribution = map[string]any{
	"author": "Mallory", "created_by": "agent", "source": "skill",
}

func withForged(body map[string]any, forged bool) map[string]any {
	out := map[string]any{}
	for k, v := range body {
		out[k] = v
	}
	if forged {
		for k, v := range forgedAttribution {
			out[k] = v
		}
	}
	return out
}

func TestBUG3372_CommentAttributionIsTheRequests(t *testing.T) {
	f := newAttributionFixture(t)
	parent := f.do(t, "POST", "/items/"+f.item.Slug+"/comments", map[string]any{"body": "parent"})

	for _, forged := range []bool{false, true} {
		name := map[bool]string{false: "control", true: "forged"}[forged]
		t.Run(name, func(t *testing.T) {
			for _, c := range []struct {
				route string
				path  string
			}{
				{"comment", "/items/" + f.item.Slug + "/comments"},
				{"reply", "/comments/" + parent["id"].(string) + "/replies"},
			} {
				got := f.do(t, "POST", c.path, withForged(map[string]any{"body": "x"}, forged))
				if got["author"] != "Alice" || got["created_by"] != "user" || got["source"] != "web" || got["user_id"] != f.user.ID {
					t.Errorf("%s: author=%v created_by=%v source=%v user_id=%v, want Alice/user/web/%s",
						c.route, got["author"], got["created_by"], got["source"], got["user_id"], f.user.ID)
				}
			}
		})
	}
}

func readUserColumns(t *testing.T, srv *Server, query, id string) (string, string) {
	t.Helper()
	var a, b sql.NullString
	if err := srv.store.DB().QueryRow(query, id).Scan(&a, &b); err != nil {
		t.Fatal(err)
	}
	return a.String, b.String
}

func TestBUG3372_ItemAttributionIsTheRequests(t *testing.T) {
	f := newAttributionFixture(t)
	for _, forged := range []bool{false, true} {
		name := map[bool]string{false: "control", true: "forged"}[forged]
		t.Run(name, func(t *testing.T) {
			created := f.do(t, "POST", "/collections/"+f.coll.Slug+"/items",
				withForged(map[string]any{"title": "made " + name}, forged))
			if created["created_by"] != "user" || created["source"] != "web" {
				t.Errorf("create: created_by=%v source=%v, want user/web", created["created_by"], created["source"])
			}
			id := created["id"].(string)
			by, mod := readUserColumns(t, f.srv, `SELECT created_by_user_id, last_modified_by_user_id FROM items WHERE id = ?`, id)
			if by != f.user.ID || mod != f.user.ID {
				t.Errorf("create: created_by_user_id=%q last_modified_by_user_id=%q, want %s", by, mod, f.user.ID)
			}

			update := map[string]any{"content": "edited " + name}
			if forged {
				update["last_modified_by"] = "agent"
				update["source"] = "cli"
				update["version_source"] = "cli"
			}
			updated := f.do(t, "PATCH", "/items/"+created["slug"].(string), update)
			if updated["last_modified_by"] != "user" || updated["source"] != "web" {
				t.Errorf("update: last_modified_by=%v source=%v, want user/web (items.source does not move on update)",
					updated["last_modified_by"], updated["source"])
			}
			var vsource string
			if err := f.srv.store.DB().QueryRow(
				`SELECT source FROM item_versions WHERE item_id = ? ORDER BY version_seq DESC LIMIT 1`, id).Scan(&vsource); err != nil {
				t.Fatal(err)
			}
			if vsource != "web" {
				t.Errorf("update: newest version source = %q, want web", vsource)
			}
		})
	}
}

func TestBUG3372_LinkAttributionIsTheRequests(t *testing.T) {
	f := newAttributionFixture(t)
	for _, forged := range []bool{false, true} {
		name := map[bool]string{false: "control", true: "forged"}[forged]
		t.Run(name, func(t *testing.T) {
			for _, linkType := range []string{"blocks", "parent"} {
				target := mustItem(t, f.srv, f.ws.ID, f.coll.ID, linkType+" "+name)
				body := map[string]any{"target_id": target.ID, "link_type": linkType}
				if forged {
					body["created_by"] = "agent"
				}
				link := f.do(t, "POST", "/items/"+f.item.Slug+"/links", body)
				if link["created_by"] != "user" {
					t.Errorf("%s link: created_by=%v, want user", linkType, link["created_by"])
				}
				var uid sql.NullString
				if err := f.srv.store.DB().QueryRow(`SELECT user_id FROM item_links WHERE id = ?`, link["id"]).Scan(&uid); err != nil {
					t.Fatal(err)
				}
				if uid.String != f.user.ID {
					t.Errorf("%s link: user_id=%q, want %s", linkType, uid.String, f.user.ID)
				}
			}
		})
	}
}

// The self-declared agent name is kept (it only ever labels the caller's
// own writes) but bounded: no control characters, at most 64 runes, and a
// header that is nothing but those does not make the request an agent's.
func TestBUG3372_AgentNameIsBounded(t *testing.T) {
	req := func(v string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("X-Pad-Agent", v)
		return r
	}
	if got := agentNameFromRequest(req("  Claude\tCode \x01")); got != "ClaudeCode" {
		t.Errorf("control characters: %q, want ClaudeCode", got)
	}
	long := agentNameFromRequest(req(strings.Repeat("é", 200)))
	if utf8.RuneCountInString(long) != maxAgentNameRunes {
		t.Errorf("long name kept %d runes, want %d", utf8.RuneCountInString(long), maxAgentNameRunes)
	}
	if actor, _ := actorFromRequest(req("\x01\x02")); actor != "user" {
		t.Errorf("a control-only header made the request %q, want user", actor)
	}
	if actor, _ := actorFromRequest(req("Claude")); actor != "agent" {
		t.Errorf("a real agent name made the request %q, want agent", actor)
	}
}

// A cross-workspace copy creates a row in the destination; the copier is
// the account that created it.
func TestBUG3372_CopyRecordsTheCopiersAccount(t *testing.T) {
	f := newAttributionFixture(t)
	dst := mustCreateOwnedWorkspace(t, f.srv, "Attribution 3372 Dest", f.user)
	dstColl := mustCollection(t, f.srv, dst.ID, "Tasks")
	rr := doRequestWithCookie(f.srv, "POST", "/api/v1/workspaces/"+f.ws.Slug+"/items/"+f.item.Slug+"/copy",
		map[string]any{"target_workspace": dst.Slug, "target_collection": dstColl.Slug}, f.tok)
	if rr.Code != http.StatusCreated {
		t.Fatalf("copy: %d %s", rr.Code, rr.Body.String())
	}
	var out struct {
		Item struct {
			ID string `json:"id"`
		} `json:"item"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil || out.Item.ID == "" {
		t.Fatalf("decode copy: %v %s", err, rr.Body.String())
	}
	by, mod := readUserColumns(t, f.srv, `SELECT created_by_user_id, last_modified_by_user_id FROM items WHERE id = ?`, out.Item.ID)
	if by != f.user.ID || mod != f.user.ID {
		t.Errorf("copy: created_by_user_id=%q last_modified_by_user_id=%q, want %s", by, mod, f.user.ID)
	}
}
