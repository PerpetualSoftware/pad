package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3585: idempotency keys on comment create.
func commentKeyFixture(t *testing.T) (*Server, string) {
	t.Helper()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]any{"title": "Commented on", "fields": `{"status":"open"}`})
	if rr.Code != http.StatusCreated {
		t.Fatalf("item: %d %s", rr.Code, rr.Body.String())
	}
	var it models.Item
	_ = json.Unmarshal(rr.Body.Bytes(), &it)
	return srv, "/api/v1/workspaces/" + slug + "/items/" + it.Slug + "/comments"
}

func countComments(t *testing.T, srv *Server, path, body string) int {
	t.Helper()
	rr := doRequest(srv, "GET", path, nil)
	var cs []models.Comment
	_ = json.Unmarshal(rr.Body.Bytes(), &cs)
	n := 0
	for _, c := range cs {
		if c.Body == body {
			n++
		}
	}
	return n
}

func TestTASK3585_CommentReplaysTheCommentItMade(t *testing.T) {
	srv, path := commentKeyFixture(t)
	body := map[string]any{"body": "Keyed comment", "op_key": "k-1"}
	first := doRequest(srv, "POST", path, body)
	again := doRequest(srv, "POST", path, body)
	var a, b models.Comment
	_ = json.Unmarshal(first.Body.Bytes(), &a)
	_ = json.Unmarshal(again.Body.Bytes(), &b)
	if first.Code != http.StatusCreated || again.Code != http.StatusCreated || a.ID == "" || a.ID != b.ID || again.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: %d/%d %q vs %q", first.Code, again.Code, a.ID, b.ID)
	}
	if n := countComments(t, srv, path, "Keyed comment"); n != 1 {
		t.Fatalf("%d comments; the retry made a twin", n)
	}
	if rr := doRequest(srv, "POST", path, map[string]any{"body": "Different", "op_key": "k-1"}); rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a reused key: %d", rr.Code)
	}
}

func TestTASK3585_RefusedCommentReleasesItsKey(t *testing.T) {
	srv, path := commentKeyFixture(t)
	bad := doRequestWithHeaders(srv, "POST", path, map[string]any{"body": "Reply", "parent_id": "no-such-comment"}, map[string]string{"Idempotency-Key": "k-2"})
	if bad.Code < 400 {
		t.Fatalf("premise: a reply to a missing parent must be refused; got %d", bad.Code)
	}
	ok := doRequestWithHeaders(srv, "POST", path, map[string]any{"body": "Reply"}, map[string]string{"Idempotency-Key": "k-2"})
	if ok.Code != http.StatusCreated || ok.Header().Get("Idempotent-Replayed") != "" {
		t.Fatalf("corrected retry with the same key: %d %s", ok.Code, ok.Body.String())
	}
}

func TestTASK3585_CommentReplayOfADeletedComment(t *testing.T) {
	srv, path := commentKeyFixture(t)
	body := map[string]any{"body": "Then deleted", "op_key": "k-3"}
	first := doRequest(srv, "POST", path, body)
	var c models.Comment
	_ = json.Unmarshal(first.Body.Bytes(), &c)
	if first.Code != http.StatusCreated || c.ID == "" {
		t.Fatalf("create: %d", first.Code)
	}
	if rr := doRequest(srv, "DELETE", path+"/"+c.ID, nil); rr.Code >= 300 {
		t.Fatalf("delete: %d %s", rr.Code, rr.Body.String())
	}
	again := doRequest(srv, "POST", path, body)
	if again.Code != http.StatusGone || !json.Valid(again.Body.Bytes()) {
		t.Fatalf("replay of a deleted comment: %d %s", again.Code, again.Body.String())
	}
	if n := countComments(t, srv, path, "Then deleted"); n != 0 {
		t.Fatalf("the replay re-created the comment: %d", n)
	}
}
