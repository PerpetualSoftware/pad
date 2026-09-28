package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// TASK-2247: `pad attachment attach` sends only to a server that advertises
// attachment_attach. An older build has no such route and would answer a
// bare 404, which reads as "attachment not found". Every leg counts the
// writes, since what the gate prevents is a write.

type attachStub struct {
	caps string // "new": advertises; "old": 404; "flaky": 500

	mu     sync.Mutex
	writes []string
	bodies []map[string]any
}

func (s *attachStub) handler(w http.ResponseWriter, r *http.Request) {
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
			_ = json.NewEncoder(w).Encode(map[string]any{"attachment_attach": true})
		}
		return
	}
	s.writes = append(s.writes, r.Method+" "+r.URL.Path)
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.bodies = append(s.bodies, body)
	_ = json.NewEncoder(w).Encode(map[string]any{"id": "a-1", "item_id": "i-1"})
}

func TestAttachmentAttach_NewServer_PostsItemRef(t *testing.T) {
	stub := &attachStub{caps: "new"}
	setupPushTest(t, http.HandlerFunc(stub.handler))
	cmd := attachmentAttachCmd()
	cmd.SetArgs([]string{"a-1", "TASK-5"})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	var err error
	captureStdout(t, func() { err = cmd.Execute() })
	if err != nil {
		t.Fatal(err)
	}
	if len(stub.writes) != 1 || !strings.HasPrefix(stub.writes[0], "POST ") || !strings.HasSuffix(stub.writes[0], "/attachments/a-1/attach") {
		t.Fatalf("writes = %v, want one POST to /attachments/a-1/attach", stub.writes)
	}
	if stub.bodies[0]["item"] != "TASK-5" {
		t.Errorf("body = %v, want {item: TASK-5}", stub.bodies[0])
	}
}

func TestAttachmentAttach_UnadvertisedServer_SendsNothing(t *testing.T) {
	for _, caps := range []string{"old", "flaky"} {
		stub := &attachStub{caps: caps}
		setupPushTest(t, http.HandlerFunc(stub.handler))
		cmd := attachmentAttachCmd()
		cmd.SetArgs([]string{"a-1", "TASK-5"})
		cmd.SilenceUsage, cmd.SilenceErrors = true, true
		var err error
		captureStdout(t, func() { err = cmd.Execute() })
		if err == nil || !strings.Contains(err.Error(), "attachment_attach") {
			t.Errorf("%s: want the capability refusal, got %v", caps, err)
		}
		if len(stub.writes) != 0 {
			t.Errorf("%s: sent %v to a server that did not advertise the route", caps, stub.writes)
		}
	}
}
