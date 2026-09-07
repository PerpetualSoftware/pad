package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2870, the CLI half of the door-parity claim. The remote half is pinned in
// internal/mcp (TestParseFieldKVP_PaddedKeyRefused /
// TestParseFieldKVP_PaddedValueVerbatim); the shared rule is pinned in
// internal/items (TestSplitFieldEntry).
//
// Before this, `--field " effort=l"` here stored an UNDECLARED field literally
// named " effort" and left the declared `effort` untouched, while the same call
// through /mcp wrote `effort`. The write reported success either way.

func TestItemUpdateFieldEntry_PaddedKeyRefused(t *testing.T) {
	for _, entry := range []string{" effort=l", "effort =l"} {
		t.Run(entry, func(t *testing.T) {
			var sawPatch bool
			setupFormatRoutingTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPatch {
					sawPatch = true
				}
				_ = json.NewEncoder(w).Encode(models.Item{Slug: "task-5", CollectionSlug: "tasks"})
			}))
			formatFlag = "table"

			cmd := updateCmd()
			cmd.SetArgs([]string{"TASK-5", "--field", entry})

			var execErr error
			out := captureStdout(t, func() { execErr = cmd.Execute() })

			if execErr == nil {
				t.Fatalf("--field %q must be refused, got success:\n%s", entry, out)
			}
			if !strings.Contains(execErr.Error(), "whitespace around its key") {
				t.Errorf("refusal should name the problem; got %q", execErr)
			}
			// The refusal has to happen BEFORE the write. A command that
			// refuses after dispatching has already stored the ghost field
			// it is complaining about.
			if sawPatch {
				t.Errorf("a refused --field must not reach the server")
			}
		})
	}
}

// The value half: padded values are content and must still be written. This is
// the case a future "just trim everything" edit would break, and it is why the
// key rule and the value rule are separate.
func TestItemUpdateFieldEntry_PaddedValueIsSent(t *testing.T) {
	var sentFields map[string]any

	setupFormatRoutingTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			var payload struct {
				FieldsPatch map[string]any `json:"fields_patch"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Errorf("decode PATCH body: %v", err)
			}
			sentFields = payload.FieldsPatch
		}
		_ = json.NewEncoder(w).Encode(models.Item{Slug: "task-5", CollectionSlug: "tasks"})
	}))
	formatFlag = "table"

	cmd := updateCmd()
	cmd.SetArgs([]string{"TASK-5", "--field", "note= x "})

	var execErr error
	out := captureStdout(t, func() { execErr = cmd.Execute() })
	if execErr != nil {
		t.Fatalf("padded VALUE must be accepted: %v\n%s", execErr, out)
	}
	if got := sentFields["note"]; got != " x " {
		t.Errorf("fields_patch[note] = %#v, want %q — trimming here would rewrite the caller's text", got, " x ")
	}
}
