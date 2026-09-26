package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// BUG-2659: `pad item search --collection X` sends X exactly as typed only to
// a server that advertises search_collection_resolution, which resolves the
// filter itself with an exact match first. Any other server matches a literal
// slug, so the CLI still expands the shorthand for it, as before.

type searchCollectionStub struct {
	caps string // "new": advertises; "old": 404; "flaky": 500

	mu   sync.Mutex
	sent []string
}

func (s *searchCollectionStub) handler(w http.ResponseWriter, r *http.Request) {
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
			_ = json.NewEncoder(w).Encode(map[string]any{"collection_resolution": true, "search_collection_resolution": true})
		}
		return
	}
	if strings.HasSuffix(r.URL.Path, "/search") {
		s.sent = append(s.sent, r.URL.Query().Get("collection"))
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{}, "total": 0})
		return
	}
	http.NotFound(w, r)
}

func runSearchWithCollection(t *testing.T, stub *searchCollectionStub, collection string) {
	t.Helper()
	setupPushTest(t, http.HandlerFunc(stub.handler))
	cmd := searchCmd()
	cmd.SetArgs([]string{"needle", "--collection", collection})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	var err error
	_ = captureStdout(t, func() { err = cmd.Execute() })
	if err != nil {
		t.Fatalf("search: %v", err)
	}
}

func TestSearchSendsCollectionAsTypedToAResolvingServer(t *testing.T) {
	stub := &searchCollectionStub{caps: "new"}
	runSearchWithCollection(t, stub, "task")
	if len(stub.sent) != 1 || stub.sent[0] != "task" {
		t.Fatalf("collection sent: %q, want [\"task\"]", stub.sent)
	}
}

func TestSearchStillExpandsShorthandForOtherServers(t *testing.T) {
	for _, caps := range []string{"old", "flaky"} {
		stub := &searchCollectionStub{caps: caps}
		runSearchWithCollection(t, stub, "task")
		if len(stub.sent) != 1 || stub.sent[0] != "tasks" {
			t.Fatalf("%s: collection sent: %q, want [\"tasks\"]", caps, stub.sent)
		}
	}
}
