package server

import (
	"net/http"
	"testing"
)

// TestUpdateComment_AdminEditMentionIsFromTheEditor pins TASK-3946 ruling
// e6a86278 through the real edit route: when a platform admin edits another
// user's comment, a mention the edit adds is from the ADMIN, the admin's own
// @-mention is skipped, and the comment's author is told when mentioned.
// Kills: the handler passing the author (or nothing) as the editor; the store
// attributing edit mentions to the author.
func TestUpdateComment_AdminEditMentionIsFromTheEditor(t *testing.T) {
	env := setupRBACEnv(t)
	st := env.srv.store
	env.srv.SetPushGateway(pushTestGateway)
	owner, err := st.GetUserByEmail("owner@test.com")
	if err != nil || owner == nil {
		t.Fatal("owner not found")
	}
	editor, err := st.GetUserByEmail("editor@test.com")
	if err != nil || editor == nil {
		t.Fatal("editor not found")
	}
	viewer, err := st.GetUserByEmail("viewer@test.com")
	if err != nil || viewer == nil {
		t.Fatal("viewer not found")
	}
	for i, tok := range []string{env.ownerToken, env.editorToken, env.viewerToken} {
		if rr := putDevice(env.srv, tok, "conn-"+string(rune('a'+i)), pushBody("fcm-"+string(rune('a'+i)))); rr.Code != http.StatusOK {
			t.Fatalf("register device %d: %d %s", i, rr.Code, rr.Body.String())
		}
	}

	rr := doRequestWithCookie(env.srv, "POST",
		"/api/v1/workspaces/"+env.wsSlug+"/collections/docs/items",
		map[string]any{"title": "Mentionable"}, env.ownerToken)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create item: %d %s", rr.Code, rr.Body.String())
	}
	var item map[string]any
	parseJSON(t, rr, &item)
	slug, _ := item["slug"].(string)
	rr = doRequestWithCookie(env.srv, "POST",
		"/api/v1/workspaces/"+env.wsSlug+"/items/"+slug+"/comments",
		map[string]any{"body": "original"}, env.editorToken)
	if rr.Code != http.StatusCreated {
		t.Fatalf("post comment: %d %s", rr.Code, rr.Body.String())
	}
	var c map[string]any
	parseJSON(t, rr, &c)
	cid, _ := c["id"].(string)

	// The admin (owner) edits the editor's comment, mentioning the viewer, the
	// author and themselves.
	rr = doRequestWithCookie(env.srv, "PATCH",
		"/api/v1/workspaces/"+env.wsSlug+"/comments/"+cid,
		map[string]any{"body": "original @" + viewer.Username + " @" + editor.Username + " @" + owner.Username}, env.ownerToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("admin edit: %d %s", rr.Code, rr.Body.String())
	}

	rows, err := st.DB().Query(`SELECT user_id, COALESCE(actor_user_id, '') FROM user_notifications WHERE kind = 'mention'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	actorOf := map[string]string{}
	for rows.Next() {
		var u, a string
		if err := rows.Scan(&u, &a); err != nil {
			t.Fatal(err)
		}
		actorOf[u] = a
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(actorOf) != 2 {
		t.Fatalf("mention rows = %v, want the viewer and the author", actorOf)
	}
	if got := actorOf[viewer.ID]; got != owner.ID {
		t.Errorf("viewer's mention actor = %q, want the admin %q", got, owner.ID)
	}
	if got, ok := actorOf[editor.ID]; !ok || got != owner.ID {
		t.Errorf("author's mention = %q (present %v), want present and from the admin", got, ok)
	}
	if _, ok := actorOf[owner.ID]; ok {
		t.Error("the admin was told about their own mention")
	}
}
