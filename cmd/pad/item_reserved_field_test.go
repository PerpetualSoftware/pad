package main

// BUG-2627 part 2 / BUG-2675 — the CLI half of two claims the server-side work
// rests on.
//
//  1. `pad item update --field <reserved>=…` lowers into `fields_patch`, which
//     is why ONE server-side gate closes the CLI, remote MCP, and stdio MCP at
//     once. If the CLI ever sent these another way, the gate would still
//     report success on this transport while writing the key.
//  2. `pad item note` / `pad item decide` emit the retry-hostile structured
//     marker for their LOCAL refusal, and — the part that matters — do not
//     send the destructive PATCH.

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestItemUpdate_ReservedFieldLowersIntoFieldsPatch pins claim (1).
//
// It asserts the WIRE SHAPE rather than a rejection, deliberately: this CLI
// never sees the gate (the server owns it), so the only thing it can prove is
// that the value arrives at the door the gate is on.
func TestItemUpdate_ReservedFieldLowersIntoFieldsPatch(t *testing.T) {
	body := captureUpdateBody(t, "TASK-9", "--field",
		models.ItemFieldImplementationNotes+`=[{"summary":"x"}]`)

	fp := fieldsPatchOf(t, body)
	if fp == nil {
		t.Fatal("no fields_patch on the wire — a reserved --field reaching the server another way " +
			"would bypass the fields_patch gate entirely")
	}
	if _, ok := fp[models.ItemFieldImplementationNotes]; !ok {
		t.Fatalf("fields_patch does not carry %q: %v", models.ItemFieldImplementationNotes, fp)
	}
	// And not through a second door at the same time.
	if _, ok := body["fields"]; ok {
		t.Errorf("update also sent a full `fields` blob; the gate only covers fields_patch: %v", body)
	}
}

// captureStderr runs fn with os.Stderr redirected and returns what was written.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	os.Stderr = orig
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}

// serveItemWithUnreadableNotes stands up the fake server for the refusal
// tests: the GET returns the exact defect shape (the entries array stored as a
// JSON-encoded STRING), and any PATCH is recorded as a failure signal.
func serveItemWithUnreadableNotes(t *testing.T, key, storedValue string, patched *bool) {
	t.Helper()
	setupPushTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			*patched = true
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "item-1", "slug": "legacy-row"})
			return
		}
		fields, _ := json.Marshal(map[string]string{"status": "done", key: storedValue})
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "item-1", "slug": "legacy-row", "title": "Legacy row",
			"collection_slug": "tasks", "collection_prefix": "TASK",
			"item_number": 860, "fields": string(fields),
			"schema": `{"fields":[{"key":"status","type":"select"}]}`,
		})
	}))
}

func TestItemNote_RefusesUnreadableFieldWithStructuredMarker(t *testing.T) {
	var patched bool
	serveItemWithUnreadableNotes(t, models.ItemFieldImplementationNotes,
		`[{"summary":"the legacy note","details":"body"}]`, &patched)

	var execErr error
	stderr := captureStderr(t, func() {
		cmd := noteCmd()
		cmd.SetArgs([]string{"TASK-860", "a new note"})
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		execErr = cmd.Execute()
	})

	if execErr == nil {
		t.Fatal("pad item note succeeded against an unreadable stored value — that append destroys it")
	}

	// THE ASSERTION THAT DISCRIMINATES. "It errored" is also true of a build
	// that errored after sending the PATCH; the destructive act is the write.
	if patched {
		t.Fatal("refused note still issued the PATCH — the stored value would have been overwritten")
	}

	if !strings.Contains(stderr, cli.StructuredErrorMarker) {
		t.Fatalf("stderr carries no %q line, so a stdio MCP agent gets server_error and retries forever; got: %q",
			cli.StructuredErrorMarker, stderr)
	}
	if !strings.Contains(stderr, cli.StoredStateUnreadableCode) {
		t.Errorf("marker line does not carry the retry-hostile code; got: %q", stderr)
	}
}

func TestItemDecide_RefusesUnreadableFieldWithStructuredMarker(t *testing.T) {
	var patched bool
	serveItemWithUnreadableNotes(t, models.ItemFieldDecisionLog,
		`[{"decision":"the legacy decision"}]`, &patched)

	var execErr error
	stderr := captureStderr(t, func() {
		cmd := decideCmd()
		cmd.SetArgs([]string{"TASK-860", "a new decision"})
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		execErr = cmd.Execute()
	})

	if execErr == nil {
		t.Fatal("pad item decide succeeded against an unreadable stored value — that append destroys it")
	}
	if patched {
		t.Fatal("refused decision still issued the PATCH — the stored value would have been overwritten")
	}
	if !strings.Contains(stderr, cli.StoredStateUnreadableCode) {
		t.Fatalf("stderr does not carry the retry-hostile code; got: %q", stderr)
	}
}

// TestItemNote_HealthyItemStillAppends is the over-breadth control leg: the
// refusal must fire on the defect shape and NOT on an ordinary item, or the
// two tests above would pass against a build that refused every note.
func TestItemNote_HealthyItemStillAppends(t *testing.T) {
	var patched bool
	serveItemWithUnreadableNotes(t, "unrelated_key", "ordinary value", &patched)

	cmd := noteCmd()
	cmd.SetArgs([]string{"TASK-860", "a new note"})
	cmd.SilenceUsage = true
	if err := cmd.Execute(); err != nil {
		t.Fatalf("note on a healthy item must succeed: %v", err)
	}
	if !patched {
		t.Fatal("note on a healthy item issued no PATCH")
	}
}
