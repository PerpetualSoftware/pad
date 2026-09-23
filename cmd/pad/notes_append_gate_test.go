package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3056: `pad item note` / `decide` choose the server-side append only when
// the server ADVERTISES it, before writing. An older server ignores the
// append keys and answers 200 having written nothing, and discovering that
// afterwards could only be repaired by a second write — a duplicate whenever
// the discovery was wrong. Every leg below counts the writes, because the
// defect this gate exists to prevent is a write count, not an error.

type appendStub struct {
	caps string // "new": advertises; "old": 404; "flaky": 500; "noecho": advertises, response lacks the echo

	mu      sync.Mutex
	gets    int
	patches []map[string]any
}

func (s *appendStub) handler(w http.ResponseWriter, r *http.Request) {
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
			_ = json.NewEncoder(w).Encode(map[string]any{"collection_resolution": true, "item_field_append": true})
		}
		return
	}
	item := map[string]any{
		"id": "item-1", "slug": "the-item", "title": "The item",
		"collection_slug": "tasks", "collection_prefix": "TASK", "item_number": 5,
		"fields": `{"status":"open"}`,
	}
	if r.Method != http.MethodPatch {
		s.gets++
		_ = json.NewEncoder(w).Encode(item)
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.patches = append(s.patches, body)

	switch {
	case s.caps == "old":
		// An old server: append_* is an unknown key and is ignored; only a
		// full `fields` write changes anything.
		if f, ok := body["fields"].(string); ok {
			item["fields"] = f
		}
	case s.caps == "noecho":
		// Accepts, but its response shows nothing appended.
	default:
		if a, ok := body["append_implementation_note"].(map[string]any); ok {
			n := models.ItemImplementationNote{ID: "note-1", Summary: a["summary"].(string), CreatedBy: "agent"}
			f, _ := models.AppendImplementationNote(`{"status":"open"}`, n)
			item["fields"] = f
			item["appended"] = map[string]any{"implementation_note": n}
		}
		if a, ok := body["append_decision"].(map[string]any); ok {
			d := models.ItemDecisionLogEntry{ID: "decision-1", Decision: a["decision"].(string), CreatedBy: "agent"}
			f, _ := models.AppendDecisionLogEntry(`{"status":"open"}`, d)
			item["fields"] = f
			item["appended"] = map[string]any{"decision": d}
		}
	}
	_ = json.NewEncoder(w).Encode(item)
}

func runNoteAgainst(t *testing.T, stub *appendStub, decide bool) (string, error) {
	t.Helper()
	setupPushTest(t, http.HandlerFunc(stub.handler))
	cmd := noteCmd()
	if decide {
		cmd = decideCmd()
	}
	cmd.SetArgs([]string{"TASK-5", "the text"})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	var err error
	stdout := captureStdout(t, func() { err = cmd.Execute() })
	return stdout, err
}

func TestNote_NewServer_OnePatchCarryingOnlyTheAppend(t *testing.T) {
	for _, decide := range []bool{false, true} {
		stub := &appendStub{caps: "new"}
		if _, err := runNoteAgainst(t, stub, decide); err != nil {
			t.Fatalf("decide=%v: %v", decide, err)
		}
		if len(stub.patches) != 1 {
			t.Fatalf("decide=%v: %d PATCHes, want 1", decide, len(stub.patches))
		}
		p := stub.patches[0]
		if _, has := p["fields"]; has {
			t.Errorf("decide=%v: the PATCH carries a full fields blob — the BUG-3056 race: %v", decide, p)
		}
		key := "append_implementation_note"
		if decide {
			key = "append_decision"
		}
		if _, has := p[key]; !has {
			t.Errorf("decide=%v: the PATCH has no %s: %v", decide, key, p)
		}
		if stub.gets != 0 {
			t.Errorf("decide=%v: %d item GETs — the append needs no read", decide, stub.gets)
		}
	}
}

// TestNote_OldServer_LegacyPathExactlyOnce is the lead's required leg: a
// server that does not advertise the flag and ignores append_* gets today's
// full-fields write, once, and the note is really in it.
func TestNote_OldServer_LegacyPathExactlyOnce(t *testing.T) {
	for _, caps := range []string{"old", "flaky"} {
		stub := &appendStub{caps: caps}
		if _, err := runNoteAgainst(t, stub, false); err != nil {
			t.Fatalf("%s: %v", caps, err)
		}
		if len(stub.patches) != 1 {
			t.Fatalf("%s: %d PATCHes, want exactly 1", caps, len(stub.patches))
		}
		p := stub.patches[0]
		if _, has := p["append_implementation_note"]; has {
			t.Errorf("%s: sent an append to a server that did not advertise it: %v", caps, p)
		}
		f, _ := p["fields"].(string)
		if notes := models.ExtractItemImplementationNotes(f); len(notes) != 1 || notes[0].Summary != "the text" {
			t.Errorf("%s: the legacy write does not carry the note: %q", caps, f)
		}
	}
}

// TestNote_UnconfirmedAppendIsReportedNotRetried: the post-write check is an
// assertion. When it fails the command errors and sends nothing more.
func TestNote_UnconfirmedAppendIsReportedNotRetried(t *testing.T) {
	stub := &appendStub{caps: "noecho"}
	_, err := runNoteAgainst(t, stub, false)
	if err == nil {
		t.Fatal("an append the server did not confirm was reported as success")
	}
	if !strings.Contains(err.Error(), "NOT re-sent") {
		t.Errorf("error should say nothing was re-sent: %v", err)
	}
	if len(stub.patches) != 1 {
		t.Errorf("%d PATCHes, want exactly 1 — a second write duplicates the entry whenever the first landed", len(stub.patches))
	}
}

// TestNote_ServerRefusalCarriesTheStructuredMarker: on the append path the
// unreadable-state refusal comes from the server as a 409; the CLI must still
// emit the marker a stdio MCP agent reads as stored_state_unreadable.
func TestNote_ServerRefusalCarriesTheStructuredMarker(t *testing.T) {
	setupPushTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/server/capabilities") {
			_ = json.NewEncoder(w).Encode(map[string]any{"item_field_append": true})
			return
		}
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
			"code": cli.StoredStateUnreadableCode, "message": "implementation_notes holds a value that is not a list of entries"}})
	}))
	var execErr error
	stderr := captureStderr(t, func() {
		cmd := noteCmd()
		cmd.SetArgs([]string{"TASK-5", "x"})
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		execErr = cmd.Execute()
	})
	if execErr == nil {
		t.Fatal("a refused append reported success")
	}
	if !strings.Contains(stderr, cli.StructuredErrorMarker) || !strings.Contains(stderr, cli.StoredStateUnreadableCode) {
		t.Errorf("stderr lacks the structured stored_state_unreadable marker: %q", stderr)
	}
}
