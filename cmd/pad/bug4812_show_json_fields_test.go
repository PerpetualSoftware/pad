package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestBUG4812ShowJSONNativeFieldsAndTags(t *testing.T) {
	itemJSON := `{
		"id": "11111111-1111-1111-1111-111111111111",
		"ref": "TASK-5",
		"title": "Native JSON fields",
		"slug": "native-json-fields",
		"seq": 7,
		"fields": "{\"status\":\"open\",\"big\":12345678901234567890,\"implementation_notes\":[{\"summary\":\"x\"}]}",
		"tags": "[\"a\",\"b\"]"
	}`
	backlinksJSON := `[{
		"source_item_id": "22222222-2222-2222-2222-222222222222",
		"source_ref": "TASK-1",
		"source_title": "Mentioner",
		"source_collection_slug": "tasks",
		"source_collection_icon": "",
		"snippet": "see [[TASK-5]]",
		"updated_at": "2026-11-20T00:00:00Z"
	}]`

	reply := func(c recordedCall) (int, string) {
		if strings.Contains(c.path, "backlinks") {
			return 200, backlinksJSON
		}
		return 200, itemJSON
	}

	out, _, err, _ := runSched(t, reply, "", "item", "show", "TASK-5", "--format", "json")
	if err != nil {
		t.Fatalf("item show --format json: %v", err)
	}

	if !strings.Contains(out, "12345678901234567890") {
		t.Errorf("raw output did not preserve big number digits verbatim:\n%s", out)
	}

	var got map[string]any
	if err := models.DecodeJSONKeepingNumbers([]byte(out), &got); err != nil {
		t.Fatalf("decode output JSON: %v\n%s", err, out)
	}

	fields, ok := got["fields"].(map[string]any)
	if !ok {
		t.Fatalf("fields = %T (%v), want JSON object (map[string]any)", got["fields"], got["fields"])
	}
	if fields["status"] != "open" {
		t.Errorf("fields.status = %v, want %q", fields["status"], "open")
	}
	if big, ok := fields["big"].(json.Number); !ok || big.String() != "12345678901234567890" {
		t.Errorf("fields.big = %T(%v), want json.Number(12345678901234567890)", fields["big"], fields["big"])
	}
	notes, ok := fields["implementation_notes"].([]any)
	if !ok || len(notes) != 1 {
		t.Errorf("fields.implementation_notes = %#v, want 1-element slice preserved", fields["implementation_notes"])
	}

	tags, ok := got["tags"].([]any)
	if !ok {
		t.Fatalf("tags = %T (%v), want JSON array ([]any)", got["tags"], got["tags"])
	}
	if len(tags) != 2 || tags[0] != "a" || tags[1] != "b" {
		t.Errorf("tags = %#v, want [a b]", tags)
	}

	if got["ref"] != "TASK-5" {
		t.Errorf("ref = %v, want TASK-5", got["ref"])
	}
	if got["title"] != "Native JSON fields" {
		t.Errorf("title = %v, want Native JSON fields", got["title"])
	}
	if seq, ok := got["seq"].(json.Number); !ok || seq.String() != "7" {
		t.Errorf("seq = %T(%v), want 7", got["seq"], got["seq"])
	}

	backlinks, ok := got["backlinks_top"].([]any)
	if !ok || len(backlinks) != 1 {
		t.Fatalf("backlinks_top = %#v, want 1-element slice", got["backlinks_top"])
	}
	bl, _ := backlinks[0].(map[string]any)
	if bl["source_ref"] != "TASK-1" {
		t.Errorf("backlinks_top[0].source_ref = %v, want TASK-1", bl["source_ref"])
	}
}

func TestBUG4812ShowJSONEmptyFieldsAndTags(t *testing.T) {
	reply := func(c recordedCall) (int, string) {
		if strings.Contains(c.path, "backlinks") {
			return 200, `[]`
		}
		return 200, `{"id":"1","ref":"TASK-6","title":"Empty","slug":"empty","seq":1,"fields":"","tags":""}`
	}

	out, _, err, _ := runSched(t, reply, "", "item", "show", "TASK-6", "--format", "json")
	if err != nil {
		t.Fatalf("item show --format json: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode output JSON: %v\n%s", err, out)
	}

	fields, ok := got["fields"].(map[string]any)
	if !ok || len(fields) != 0 {
		t.Errorf("fields = %T(%v), want empty JSON object {}", got["fields"], got["fields"])
	}
	tags, ok := got["tags"].([]any)
	if !ok || len(tags) != 0 {
		t.Errorf("tags = %T(%v), want empty JSON array []", got["tags"], got["tags"])
	}
}

func TestBUG4812ShowJSONInvalidFieldsString(t *testing.T) {
	reply := func(c recordedCall) (int, string) {
		if strings.Contains(c.path, "backlinks") {
			return 200, `[]`
		}
		return 200, `{"id":"1","ref":"TASK-7","title":"Malformed","slug":"malformed","seq":2,"fields":"{not json","tags":"[]"}`
	}

	out, _, err, _ := runSched(t, reply, "", "item", "show", "TASK-7", "--format", "json")
	if err != nil {
		t.Fatalf("item show --format json must not error on malformed fields: %v", err)
	}
	if !json.Valid([]byte(out)) {
		t.Fatalf("stdout must be valid JSON:\n%s", out)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode output JSON: %v\n%s", err, out)
	}
	fields, ok := got["fields"].(string)
	if !ok || fields != "{not json" {
		t.Errorf("fields = %T(%q), want JSON string %q", got["fields"], got["fields"], "{not json")
	}
	tags, ok := got["tags"].([]any)
	if !ok || len(tags) != 0 {
		t.Errorf("tags = %T(%v), want empty JSON array []", got["tags"], got["tags"])
	}
}
