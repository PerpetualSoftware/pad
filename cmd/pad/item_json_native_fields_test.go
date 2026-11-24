package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/models"
)

const nativeFieldsTestItemJSON = `{
	"id": "11111111-1111-1111-1111-111111111111",
	"workspace_id": "ws-1",
	"collection_id": "col-1",
	"ref": "TASK-5",
	"title": "Native JSON item",
	"slug": "native-json-item",
	"seq": 7,
	"fields": "{\"status\":\"open\",\"big\":12345678901234567890}",
	"tags": "[\"alpha\",\"beta\"]"
}`

func assertNativeItemObject(t *testing.T, raw map[string]any, fullText string) {
	t.Helper()
	fields, ok := raw["fields"].(map[string]any)
	if !ok {
		t.Fatalf("fields = %T (%v), want JSON object (map[string]any)\nstdout:\n%s", raw["fields"], raw["fields"], fullText)
	}
	if fields["status"] != "open" {
		t.Errorf("fields.status = %v, want %q", fields["status"], "open")
	}
	if big, ok := fields["big"].(json.Number); !ok || big.String() != "12345678901234567890" {
		t.Errorf("fields.big = %T(%v), want json.Number(12345678901234567890)", fields["big"], fields["big"])
	}
	tags, ok := raw["tags"].([]any)
	if !ok {
		t.Fatalf("tags = %T (%v), want JSON array ([]any)\nstdout:\n%s", raw["tags"], raw["tags"], fullText)
	}
	if len(tags) != 2 || tags[0] != "alpha" || tags[1] != "beta" {
		t.Errorf("tags = %#v, want [alpha beta]", tags)
	}
	if raw["workspace_id"] != "ws-1" {
		t.Errorf("workspace_id = %v, want ws-1", raw["workspace_id"])
	}
	if raw["ref"] != "TASK-5" {
		t.Errorf("ref = %v, want TASK-5", raw["ref"])
	}
}

func TestItemJSONOutputsEmitNativeFieldsAndTags(t *testing.T) {
	t.Run("item create", func(t *testing.T) {
		reply := func(c recordedCall) (int, string) {
			if c.method == "POST" {
				return 201, nativeFieldsTestItemJSON
			}
			return 200, `{"id":"col-1","slug":"tasks","schema":"{\"fields\":[]}"}`
		}
		out, _, err, _ := runSched(t, reply, "", "item", "create", "tasks", "Native JSON item", "--format", "json")
		if err != nil {
			t.Fatalf("item create --format json: %v", err)
		}
		var got map[string]any
		if err := models.DecodeJSONKeepingNumbers([]byte(out), &got); err != nil {
			t.Fatalf("decode output: %v\n%s", err, out)
		}
		assertNativeItemObject(t, got, out)
	})

	t.Run("item update", func(t *testing.T) {
		reply := func(c recordedCall) (int, string) {
			return 200, nativeFieldsTestItemJSON
		}
		out, _, err, _ := runSched(t, reply, "", "item", "update", "TASK-5", "--title", "Native JSON item", "--format", "json")
		if err != nil {
			t.Fatalf("item update --format json: %v", err)
		}
		var got map[string]any
		if err := models.DecodeJSONKeepingNumbers([]byte(out), &got); err != nil {
			t.Fatalf("decode output: %v\n%s", err, out)
		}
		assertNativeItemObject(t, got, out)
	})

	t.Run("item list --full", func(t *testing.T) {
		reply := func(c recordedCall) (int, string) {
			return 200, `[` + nativeFieldsTestItemJSON + `]`
		}
		out, _, err, _ := runSched(t, reply, "", "item", "list", "--full", "--format", "json")
		if err != nil {
			t.Fatalf("item list --full --format json: %v", err)
		}
		var got []map[string]any
		if err := models.DecodeJSONKeepingNumbers([]byte(out), &got); err != nil {
			t.Fatalf("decode output: %v\n%s", err, out)
		}
		if len(got) != 1 {
			t.Fatalf("len = %d, want 1", len(got))
		}
		assertNativeItemObject(t, got[0], out)
	})

	t.Run("item list --full empty stays []", func(t *testing.T) {
		reply := func(c recordedCall) (int, string) {
			return 200, `[]`
		}
		out, _, err, _ := runSched(t, reply, "", "item", "list", "--full", "--format", "json")
		if err != nil {
			t.Fatalf("item list --full --format json (empty): %v", err)
		}
		if strings.TrimSpace(out) != "[]" {
			t.Errorf("empty item list --full = %q, want []", strings.TrimSpace(out))
		}
	})

	t.Run("item starred", func(t *testing.T) {
		reply := func(c recordedCall) (int, string) {
			return 200, `[` + nativeFieldsTestItemJSON + `]`
		}
		out, _, err, _ := runSched(t, reply, "", "item", "starred", "--format", "json")
		if err != nil {
			t.Fatalf("item starred --format json: %v", err)
		}
		var got []map[string]any
		if err := models.DecodeJSONKeepingNumbers([]byte(out), &got); err != nil {
			t.Fatalf("decode output: %v\n%s", err, out)
		}
		if len(got) != 1 {
			t.Fatalf("len = %d, want 1", len(got))
		}
		assertNativeItemObject(t, got[0], out)
	})

	t.Run("item starred empty stays []", func(t *testing.T) {
		reply := func(c recordedCall) (int, string) {
			return 200, `[]`
		}
		out, _, err, _ := runSched(t, reply, "", "item", "starred", "--format", "json")
		if err != nil {
			t.Fatalf("item starred --format json (empty): %v", err)
		}
		if strings.TrimSpace(out) != "[]" {
			t.Errorf("empty item starred = %q, want []", strings.TrimSpace(out))
		}
	})

	t.Run("library activate server-supported", func(t *testing.T) {
		reply := func(c recordedCall) (int, string) {
			if strings.HasSuffix(c.path, "/server/capabilities") {
				return 200, `{"library_activate":true}`
			}
			return 201, nativeFieldsTestItemJSON
		}
		out, _, err, _ := runSched(t, reply, "", "library", "activate", "Ship tasks", "--format", "json")
		if err != nil {
			t.Fatalf("library activate --format json: %v", err)
		}
		var got map[string]any
		if err := models.DecodeJSONKeepingNumbers([]byte(out), &got); err != nil {
			t.Fatalf("decode output: %v\n%s", err, out)
		}
		assertNativeItemObject(t, got, out)
	})

	t.Run("library activate legacy convention", func(t *testing.T) {
		convItemJSON := `{
			"id": "11111111-1111-1111-1111-111111111111",
			"workspace_id": "ws-1",
			"ref": "TASK-5",
			"title": "Commit often",
			"slug": "commit-often",
			"seq": 7,
			"fields": "{\"status\":\"open\",\"big\":12345678901234567890,\"convention\":{\"category\":\"git\"}}",
			"tags": "[\"alpha\",\"beta\"]",
			"convention": {"category":"git"}
		}`
		reply := func(c recordedCall) (int, string) {
			switch {
			case strings.HasSuffix(c.path, "/server/capabilities"):
				return 200, `{"library_activate":false}`
			case strings.HasSuffix(c.path, "/convention-library"):
				return 200, `{"categories":[{"name":"git","conventions":[{"title":"Commit often","content":"Body","category":"git","trigger":"always","enforcement":"must"}]}]}`
			case strings.HasSuffix(c.path, "/collections"):
				return 200, `[{"slug":"conventions","artifact_kind":"convention"}]`
			default:
				return 201, convItemJSON
			}
		}
		out, _, err, _ := runSched(t, reply, "", "library", "activate", "Commit often", "--format", "json")
		if err != nil {
			t.Fatalf("library activate (legacy convention) --format json: %v", err)
		}
		var got map[string]any
		if err := models.DecodeJSONKeepingNumbers([]byte(out), &got); err != nil {
			t.Fatalf("decode output: %v\n%s", err, out)
		}
		assertNativeItemObject(t, got, out)
	})

	t.Run("library activate legacy playbook", func(t *testing.T) {
		reply := func(c recordedCall) (int, string) {
			switch {
			case strings.HasSuffix(c.path, "/server/capabilities"):
				return 200, `{"library_activate":false}`
			case strings.HasSuffix(c.path, "/convention-library"):
				return 200, `{"categories":[]}`
			case strings.HasSuffix(c.path, "/playbook-library"):
				return 200, `{"categories":[{"name":"delivery","playbooks":[{"title":"Ship tasks","content":"Steps","category":"delivery","trigger":"manual","scope":"workspace"}]}]}`
			case strings.HasSuffix(c.path, "/collections"):
				return 200, `[{"slug":"playbooks","artifact_kind":"playbook"}]`
			default:
				return 201, nativeFieldsTestItemJSON
			}
		}
		out, _, err, _ := runSched(t, reply, "", "library", "activate", "Ship tasks", "--format", "json")
		if err != nil {
			t.Fatalf("library activate (legacy playbook) --format json: %v", err)
		}
		var got map[string]any
		if err := models.DecodeJSONKeepingNumbers([]byte(out), &got); err != nil {
			t.Fatalf("decode output: %v\n%s", err, out)
		}
		assertNativeItemObject(t, got, out)
	})

	t.Run("library update", func(t *testing.T) {
		reply := func(c recordedCall) (int, string) {
			switch {
			case strings.HasSuffix(c.path, "/server/capabilities"):
				return 200, `{"builtin_update":true}`
			case strings.HasSuffix(c.path, "/builtin"):
				return 200, `{"key":"playbook/plan","state":"update_available","seq":7}`
			case strings.HasSuffix(c.path, "/builtin/update"):
				return 200, nativeFieldsTestItemJSON
			default:
				return 200, nativeFieldsTestItemJSON
			}
		}
		out, _, err, _ := runSched(t, reply, "", "library", "update", "TASK-5", "--format", "json")
		if err != nil {
			t.Fatalf("library update --format json: %v", err)
		}
		var got map[string]any
		if err := models.DecodeJSONKeepingNumbers([]byte(out), &got); err != nil {
			t.Fatalf("decode output: %v\n%s", err, out)
		}
		assertNativeItemObject(t, got, out)
	})

	t.Run("github link", func(t *testing.T) {
		linkedItemJSON := `{
			"id": "11111111-1111-1111-1111-111111111111",
			"workspace_id": "ws-1",
			"ref": "TASK-5",
			"title": "Native JSON item",
			"slug": "native-json-item",
			"seq": 8,
			"fields": "{\"status\":\"open\",\"big\":12345678901234567890,\"github_pr\":{\"number\":42}}",
			"tags": "[\"alpha\",\"beta\"]",
			"code_context": {"provider":"github","pull_request":{"number":42,"url":"https://github.com/acme/app/pull/42","title":"PR","state":"open"}}
		}`
		reply := func(c recordedCall) (int, string) {
			if c.method == "PATCH" {
				return 200, linkedItemJSON
			}
			return 200, nativeFieldsTestItemJSON
		}
		out, _, err, _ := runSched(t, reply, "", "github", "link", "TASK-5",
			"--pr-number", "42", "--pr-url", "https://github.com/acme/app/pull/42", "--pr-state", "open", "--format", "json")
		if err != nil {
			t.Fatalf("github link --format json: %v", err)
		}
		var got map[string]any
		if err := models.DecodeJSONKeepingNumbers([]byte(out), &got); err != nil {
			t.Fatalf("decode output: %v\n%s", err, out)
		}
		assertNativeItemObject(t, got, out)
	})

	t.Run("github unlink", func(t *testing.T) {
		linkedItemJSON := `{
			"id": "11111111-1111-1111-1111-111111111111",
			"workspace_id": "ws-1",
			"ref": "TASK-5",
			"title": "Native JSON item",
			"slug": "native-json-item",
			"seq": 7,
			"fields": "{\"status\":\"open\",\"github_pr\":{\"number\":42}}",
			"tags": "[\"alpha\",\"beta\"]"
		}`
		reply := func(c recordedCall) (int, string) {
			if c.method == "PATCH" {
				return 200, nativeFieldsTestItemJSON
			}
			return 200, linkedItemJSON
		}
		out, _, err, _ := runSched(t, reply, "", "github", "unlink", "TASK-5", "--format", "json")
		if err != nil {
			t.Fatalf("github unlink --format json: %v", err)
		}
		var got map[string]any
		if err := models.DecodeJSONKeepingNumbers([]byte(out), &got); err != nil {
			t.Fatalf("decode output: %v\n%s", err, out)
		}
		assertNativeItemObject(t, got, out)
	})

	t.Run("item search", func(t *testing.T) {
		searchRaw := `{"z_extra":12345678901234567890,"results":[{"snippet":"match","item":{"z_first":"yes","id":"11111111-1111-1111-1111-111111111111","workspace_id":"ws-1","ref":"TASK-5","title":"Native JSON item","slug":"native-json-item","seq":7,"fields":"{\"status\":\"open\",\"big\":12345678901234567890}","tags":"[\"alpha\",\"beta\"]","a_last":"ok"}}],"total":1,"a_tail":"keep"}`
		reply := func(c recordedCall) (int, string) {
			return 200, searchRaw
		}
		out, _, err, _ := runSched(t, reply, "", "item", "search", "Native", "--format", "json")
		if err != nil {
			t.Fatalf("item search --format json: %v", err)
		}
		if !strings.Contains(out, "12345678901234567890") {
			t.Errorf("item search lost big integer literal:\n%s", out)
		}
		if strings.Index(out, `"z_extra"`) > strings.Index(out, `"a_tail"`) {
			t.Errorf("item search did not preserve top-level key order:\n%s", out)
		}
		if strings.Index(out, `"z_first"`) > strings.Index(out, `"a_last"`) {
			t.Errorf("item search did not preserve item key order:\n%s", out)
		}
		var got map[string]any
		if err := models.DecodeJSONKeepingNumbers([]byte(out), &got); err != nil {
			t.Fatalf("decode output: %v\n%s", err, out)
		}
		results, ok := got["results"].([]any)
		if !ok || len(results) != 1 {
			t.Fatalf("results = %#v, want 1 element", got["results"])
		}
		entry, _ := results[0].(map[string]any)
		itemObj, ok := entry["item"].(map[string]any)
		if !ok {
			t.Fatalf("results[0].item = %T, want object", entry["item"])
		}
		assertNativeItemObject(t, itemObj, out)
	})

	t.Run("item copy", func(t *testing.T) {
		preflightJSON := `{
			"source":{"workspace_slug":"test","collection_slug":"tasks","ref":"TASK-5","slug":"native-json-item","title":"Native JSON item"},
			"destination":{"workspace_slug":"dest","workspace_name":"Dest","collection_slug":"tasks","collection_name":"Tasks"},
			"archive_source":false,
			"valid":true,
			"fields":{"carried":[],"dropped":[],"needs_value":[]},
			"warnings":{"outgoing_links":{},"incoming_links":{}}
		}`
		copyRaw := `{"z_extra":12345678901234567890,"source":{"workspace_slug":"test","collection_slug":"tasks","ref":"TASK-5","slug":"native-json-item","title":"Native JSON item","archived":false},"destination":{"workspace_slug":"dest","workspace_name":"Dest","collection_slug":"tasks","collection_name":"Tasks","ref":"TASK-9","slug":"native-json-item"},"archive_source":false,"item":{"z_first":"yes","id":"11111111-1111-1111-1111-111111111111","workspace_id":"ws-1","ref":"TASK-5","title":"Native JSON item","slug":"native-json-item","seq":7,"fields":"{\"status\":\"open\",\"big\":12345678901234567890}","tags":"[\"alpha\",\"beta\"]","a_last":"ok"},"warnings":{"dropped_fields":[]},"a_tail":"keep"}`
		reply := func(c recordedCall) (int, string) {
			if strings.HasSuffix(c.path, "/copy/preflight") {
				return 200, preflightJSON
			}
			return 201, copyRaw
		}
		out, _, err, _ := runSched(t, reply, "", "item", "copy", "TASK-5", "--to-workspace", "dest", "--collection", "tasks", "--format", "json")
		if err != nil {
			t.Fatalf("item copy --format json: %v", err)
		}
		if !strings.Contains(out, "12345678901234567890") {
			t.Errorf("item copy lost big integer literal:\n%s", out)
		}
		if strings.Index(out, `"z_extra"`) > strings.Index(out, `"a_tail"`) {
			t.Errorf("item copy did not preserve top-level key order:\n%s", out)
		}
		if strings.Index(out, `"z_first"`) > strings.Index(out, `"a_last"`) {
			t.Errorf("item copy did not preserve item key order:\n%s", out)
		}
		var got map[string]any
		if err := models.DecodeJSONKeepingNumbers([]byte(out), &got); err != nil {
			t.Fatalf("decode output: %v\n%s", err, out)
		}
		itemObj, ok := got["item"].(map[string]any)
		if !ok {
			t.Fatalf("item = %T, want object", got["item"])
		}
		assertNativeItemObject(t, itemObj, out)
	})

	t.Run("item copy committed-but-undecodable path rewrites valid item JSON", func(t *testing.T) {
		rawRes := json.RawMessage(`{"z_extra":12345678901234567890,"item":{"id":"11111111-1111-1111-1111-111111111111","workspace_id":"ws-1","ref":"TASK-5","title":"Native JSON item","slug":"native-json-item","seq":7,"fields":"{\"status\":\"open\",\"big\":12345678901234567890}","tags":"[\"alpha\",\"beta\"]"},"warnings":"unexpected-shape"}`)
		d := &recordingDeps{
			t:         t,
			preflight: emptyPreflight(),
			resultRaw: rawRes,
			copyErr:   fmt.Errorf("%w: decode failed", cli.ErrCopyCommitted),
		}
		opts := baseOpts()
		opts.Format = "json"
		var out, errOut bytes.Buffer
		if err := runItemCopy(opts, d.deps(), &out, &errOut); err != nil {
			t.Fatalf("runItemCopy committed-but-undecodable must exit 0: %v", err)
		}
		var got map[string]any
		if err := models.DecodeJSONKeepingNumbers(out.Bytes(), &got); err != nil {
			t.Fatalf("decode output: %v\n%s", err, out.String())
		}
		itemObj, ok := got["item"].(map[string]any)
		if !ok {
			t.Fatalf("item = %T, want object", got["item"])
		}
		assertNativeItemObject(t, itemObj, out.String())
		if got["warnings"] != "unexpected-shape" {
			t.Errorf("unknown/undecodable field lost: %v", got["warnings"])
		}
	})
}
