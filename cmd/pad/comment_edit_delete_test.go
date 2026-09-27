package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"
)

// TASK-2695: `pad item comment-edit` / `comment-delete` send only to a server
// that advertises item_scoped_comment_writes. An older build has no such
// route and would answer a bare 404, which reads as "comment not found".
// Every leg counts the writes, since what the gate prevents is a write.

type commentStub struct {
	caps string // "new": advertises; "old": 404; "flaky": 500

	mu     sync.Mutex
	writes []string // "METHOD path"
	bodies []map[string]any
}

func (s *commentStub) handler(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if strings.HasSuffix(r.URL.Path, "/server/capabilities") {
		switch s.caps {
		case "old":
			http.NotFound(w, r)
		case "flaky":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"item_scoped_comment_writes": true})
		}
		return
	}
	s.writes = append(s.writes, r.Method+" "+r.URL.Path)
	if r.Method == http.MethodDelete {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.bodies = append(s.bodies, body)
	_ = json.NewEncoder(w).Encode(map[string]any{"id": "c-1", "body": body["body"]})
}

func runCommentCmd(t *testing.T, stub *commentStub, cmd *cobra.Command, args ...string) error {
	t.Helper()
	setupPushTest(t, http.HandlerFunc(stub.handler))
	cmd.SetArgs(args)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	var err error
	captureStdout(t, func() { err = cmd.Execute() })
	return err
}

func TestCommentEditDelete_NewServer_ItemScopedRoute(t *testing.T) {
	stub := &commentStub{caps: "new"}
	if err := runCommentCmd(t, stub, commentEditCmd(), "TASK-5", "c-1", "fixed text"); err != nil {
		t.Fatal(err)
	}
	if err := runCommentCmd(t, stub, commentDeleteCmd(), "TASK-5", "c-1"); err != nil {
		t.Fatal(err)
	}
	if len(stub.writes) != 2 {
		t.Fatalf("writes = %v, want one PATCH and one DELETE", stub.writes)
	}
	for i, method := range []string{"PATCH", "DELETE"} {
		if !strings.HasPrefix(stub.writes[i], method+" ") || !strings.HasSuffix(stub.writes[i], "/items/TASK-5/comments/c-1") {
			t.Errorf("write %d = %q, want %s to the item-scoped route", i, stub.writes[i], method)
		}
	}
	if len(stub.bodies) != 1 || stub.bodies[0]["body"] != "fixed text" {
		t.Errorf("PATCH body = %v, want {body: fixed text}", stub.bodies)
	}
}

func TestCommentEditDelete_UnadvertisedServer_SendsNothing(t *testing.T) {
	for _, caps := range []string{"old", "flaky"} {
		stub := &commentStub{caps: caps}
		errEdit := runCommentCmd(t, stub, commentEditCmd(), "TASK-5", "c-1", "fixed text")
		errDelete := runCommentCmd(t, stub, commentDeleteCmd(), "TASK-5", "c-1")
		for name, err := range map[string]error{"edit": errEdit, "delete": errDelete} {
			if err == nil || !strings.Contains(err.Error(), "item_scoped_comment_writes") {
				t.Errorf("%s/%s: want the capability refusal, got %v", caps, name, err)
			}
		}
		if len(stub.writes) != 0 {
			t.Errorf("%s: sent %v to a server that did not advertise the routes", caps, stub.writes)
		}
	}
}
