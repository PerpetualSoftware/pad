package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCommitmentResolveForwardsActualDoneAt(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/v1/workspaces/a%2Fb/items/TASK-5/commitment/resolve" && r.URL.EscapedPath() != "/api/v1/workspaces/a%2Fb/items/TASK-5/commitment/resolve" {
			t.Errorf("request: %s %s", r.Method, r.URL.EscapedPath())
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["actual_done_at"] != "2026-11-08T09:00:00Z" {
			t.Errorf("actual_done_at lost: %v", body)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ts.Close()
	var in CommitmentResolve
	if err := json.Unmarshal([]byte(`{"state":"done","occurrence_at":"2026-11-08T10:00:00Z","actual_done_at":"2026-11-08T09:00:00Z"}`), &in); err != nil {
		t.Fatal(err)
	}
	if _, err := newCopyTestClient(t, ts).ResolveItemCommitment("a/b", "TASK-5", in); err != nil {
		t.Fatal(err)
	}
}
