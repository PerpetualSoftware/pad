package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// BUG-3448: `pad item show` printed a json field (a playbook's `arguments`)
// in Go's map syntax, `[map[description:… name:target …]]`, and labelled a
// playbook's trigger/surfaces block "Convention Metadata". A structured value
// now prints as JSON, and the block is labelled for the item's own kind.

func showFixture(collSlug, collName string, fields map[string]any) map[string]any {
	raw, _ := json.Marshal(fields)
	return map[string]any{
		"id":              "11111111-1111-1111-1111-111111111111",
		"workspace_id":    "ws-id",
		"collection_id":   "c-id",
		"collection_slug": collSlug,
		"collection_name": collName,
		"ref":             "PLAYB-5",
		"slug":            "ship",
		"title":           "Ship",
		"content":         "body",
		"fields":          string(raw),
		"seq":             3,
		"convention":      map[string]any{"trigger": "manual", "surfaces": []string{"all"}},
		"created_at":      "2026-10-06T00:00:00Z",
		"updated_at":      "2026-10-06T00:00:00Z",
	}
}

func runShow3448(t *testing.T, item map[string]any) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/items/PLAYB-5") {
			_ = json.NewEncoder(w).Encode(item)
			return
		}
		_, _ = w.Write([]byte("[]"))
	}))
	t.Cleanup(srv.Close)
	setTempHomeMain(t)
	t.Setenv("PAD_URL", srv.URL)
	t.Setenv("PAD_TOKEN", "pad_testtoken")
	origWS, origFormat := workspaceFlag, formatFlag
	t.Cleanup(func() { workspaceFlag, formatFlag = origWS, origFormat })
	workspaceFlag, formatFlag = "ws", ""

	cmd := showCmd()
	cmd.SetArgs([]string{"PLAYB-5"})
	var execErr error
	var stdout string
	_ = captureStderr(t, func() {
		stdout = captureStdout(t, func() { execErr = cmd.Execute() })
	})
	if execErr != nil {
		t.Fatalf("item show: %v\n%s", execErr, stdout)
	}
	return stdout
}

func TestItemShowPrintsJSONFieldsAsJSON(t *testing.T) {
	out := runShow3448(t, showFixture("playbooks", "Playbooks", map[string]any{
		"status":    "active",
		"arguments": []any{map[string]any{"name": "target", "type": "string", "required": true}},
	}))
	if strings.Contains(out, "map[") {
		t.Errorf("a json field printed in Go map syntax:\n%s", out)
	}
	if !strings.Contains(out, `"name":"target"`) {
		t.Errorf("the arguments field is not shown as JSON:\n%s", out)
	}
	// Control: a scalar field prints as itself.
	if !strings.Contains(out, "active") {
		t.Errorf("the scalar status field is missing:\n%s", out)
	}
}

func TestItemShowLabelsTheMetadataBlockForTheItemsKind(t *testing.T) {
	out := runShow3448(t, showFixture("playbooks", "Playbooks", map[string]any{"status": "active", "trigger": "manual"}))
	if strings.Contains(out, "Convention Metadata") {
		t.Errorf("a playbook's metadata block is labelled as a convention's:\n%s", out)
	}
	if !strings.Contains(out, "--- Playbook Metadata ---") {
		t.Errorf("the playbook's metadata block is not labelled for a playbook:\n%s", out)
	}
	// Control: a convention keeps its label.
	conv := runShow3448(t, showFixture("conventions", "Conventions", map[string]any{"status": "active", "trigger": "on-commit"}))
	if !strings.Contains(conv, "--- Convention Metadata ---") {
		t.Errorf("a convention lost its metadata label:\n%s", conv)
	}
}

// Codex r1: a number above 2^53 keeps its digits; a JSON null is shown as
// null, not as an empty value.
func TestItemShowKeepsNumbersAndNull(t *testing.T) {
	out := runShow3448(t, showFixture("playbooks", "Playbooks", map[string]any{
		"arguments":     []any{map[string]any{"default": json.Number("9007199254740993")}},
		"configuration": nil,
	}))
	if !strings.Contains(out, "9007199254740993") {
		t.Errorf("a large number lost its digits:\n%s", out)
	}
	if !strings.Contains(out, "configuration: null") {
		t.Errorf("a JSON null did not print as null:\n%s", out)
	}
}

// Codex r1: no guessing at plurals. The two system kinds have exact labels;
// any other collection is named as it is.
func TestItemShowMetadataLabelUsesTheNameAsIs(t *testing.T) {
	out := runShow3448(t, showFixture("analysis", "Analysis", map[string]any{"trigger": "manual"}))
	if !strings.Contains(out, "--- Analysis Metadata ---") {
		t.Errorf("a collection name ending in s was mangled:\n%s", out)
	}
}
