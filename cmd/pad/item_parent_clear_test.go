package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2941: `pad item update <ref> --parent ""` exited 0 and printed the
// updated item while doing nothing. `hasFieldChanges` tests `parentRef != ""`,
// so the empty value built no patch and the key the server's clear-path needs
// (`parent` present, empty) never reached the wire. The two representations of
// the link then disagreed — `parent_id` read null while `parent_ref` and the
// child listing still named the parent, and the terminal-status guard still
// refused to close the parent for `open_children`.
//
// BUG-2078 shipped `--clear-parent` as the working route and left this one
// looking like it worked. The public CLI docs still taught it, which is where
// the expectation came from.

func TestItemUpdateParent_EmptyValueRefused(t *testing.T) {
	// ZERO requests, not "no writes" (codex round 1 [P1]). The first version
	// of this test ignored GETs, and the first version of the fix refused
	// AFTER the item fetch — so a refused call still hit the server and this
	// test passed anyway. Counting every method is what makes it an
	// instrument for "refused before anything happened".
	var requests []string

	setupFormatRoutingTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		_ = json.NewEncoder(w).Encode(models.Item{Slug: "task-5", CollectionSlug: "tasks"})
	}))
	formatFlag = "table"

	cmd := updateCmd()
	cmd.SetArgs([]string{"TASK-5", "--parent", ""})

	var execErr error
	out := captureStdout(t, func() { execErr = cmd.Execute() })

	if execErr == nil {
		t.Fatalf(`--parent "" must be refused, got success:\n%s`, out)
	}
	// The refusal has to name the flag that works, or it just moves the dead
	// end one step later.
	if !strings.Contains(execErr.Error(), "--clear-parent") {
		t.Errorf("refusal must point at --clear-parent; got %q", execErr)
	}
	if len(requests) != 0 {
		t.Errorf("a refused --parent must not reach the server at all; it made %d request(s): %v",
			len(requests), requests)
	}
}

// The control legs. Refusing the empty form must not disturb either
// neighbouring behaviour, or the fix trades one silent surprise for another.
func TestItemUpdateParent_NonEmptyAndAbsentStillWork(t *testing.T) {
	t.Run("a real parent still sets the link", func(t *testing.T) {
		var patched bool
		setupFormatRoutingTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPatch {
				patched = true
			}
			_ = json.NewEncoder(w).Encode(models.Item{ID: "parent-id", Slug: "plan-9", CollectionSlug: "plans"})
		}))
		formatFlag = "table"

		cmd := updateCmd()
		cmd.SetArgs([]string{"TASK-5", "--parent", "PLAN-9"})

		var execErr error
		out := captureStdout(t, func() { execErr = cmd.Execute() })
		if execErr != nil {
			t.Fatalf("a non-empty --parent must still work: %v\n%s", execErr, out)
		}
		if !patched {
			t.Error("expected the update to dispatch")
		}
	})

	t.Run("no --parent at all is untouched", func(t *testing.T) {
		setupFormatRoutingTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(models.Item{Slug: "task-5", CollectionSlug: "tasks"})
		}))
		formatFlag = "table"

		cmd := updateCmd()
		cmd.SetArgs([]string{"TASK-5", "--status", "done"})

		var execErr error
		out := captureStdout(t, func() { execErr = cmd.Execute() })
		if execErr != nil {
			t.Fatalf("an update that never mentions --parent must be unaffected: %v\n%s", execErr, out)
		}
	})
}

// CREATE is deliberately NOT refused: an empty --parent there expresses nothing
// to ignore, since there is no parent to detach, and `--parent "$MAYBE_EMPTY"`
// is a normal shell idiom. This test exists so that asymmetry is a decision
// somebody made rather than a gap — if a later change refuses it there too,
// this test says what was traded away.
func TestItemCreateParent_EmptyValueStillAccepted(t *testing.T) {
	setupFormatRoutingTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(models.Item{Slug: "new-task", CollectionSlug: "tasks"})
	}))
	formatFlag = "table"

	cmd := createCmd()
	cmd.SetArgs([]string{"tasks", "a title", "--parent", ""})

	var execErr error
	out := captureStdout(t, func() { execErr = cmd.Execute() })
	if execErr != nil {
		t.Fatalf(`create with an empty --parent must still be accepted: %v\n%s`, execErr, out)
	}
}
