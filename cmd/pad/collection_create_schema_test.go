package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestCollectionSchemaJSONFromFlags_Inline verifies the inline JSON path:
// schemaInput is the literal JSON, gets unmarshaled then re-marshaled
// through the CollectionSchema shape (which normalizes / validates it).
func TestCollectionSchemaJSONFromFlags_Inline(t *testing.T) {
	in := `{"fields":[{"key":"status","label":"Status","type":"select","options":["new","done"],"terminal_options":["done"],"default":"new","required":true}]}`

	out, err := collectionSchemaJSONFromFlags(in, "", strings.NewReader(""))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var got models.CollectionSchema
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	if len(got.Fields) != 1 {
		t.Fatalf("expected 1 field, got %d", len(got.Fields))
	}
	f := got.Fields[0]
	if f.Key != "status" || f.Type != "select" {
		t.Fatalf("unexpected key/type: %+v", f)
	}
	if len(f.TerminalOptions) != 1 || f.TerminalOptions[0] != "done" {
		t.Fatalf("expected terminal_options=[done], got %v", f.TerminalOptions)
	}
	if !f.Required || f.Default != "new" {
		t.Fatalf("expected required=true default=new, got required=%v default=%v", f.Required, f.Default)
	}
}

// TestCollectionSchemaJSONFromFlags_File verifies the @<path> file path resolver.
func TestCollectionSchemaJSONFromFlags_File(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "schema.json")
	payload := `{"fields":[{"key":"status","type":"select","options":["a","b"],"terminal_options":["b"]}]}`
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatalf("write tmpfile: %v", err)
	}

	out, err := collectionSchemaJSONFromFlags("@"+path, "", strings.NewReader(""))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var got models.CollectionSchema
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	if len(got.Fields) != 1 || got.Fields[0].TerminalOptions[0] != "b" {
		t.Fatalf("expected terminal_options=[b], got %+v", got.Fields)
	}
}

// TestCollectionSchemaJSONFromFlags_Stdin verifies the "-" stdin resolver.
func TestCollectionSchemaJSONFromFlags_Stdin(t *testing.T) {
	stdin := strings.NewReader(`{"fields":[{"key":"priority","type":"select","options":["lo","hi"]}]}`)
	out, err := collectionSchemaJSONFromFlags("-", "", stdin)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var got models.CollectionSchema
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	if len(got.Fields) != 1 || got.Fields[0].Key != "priority" {
		t.Fatalf("unexpected schema: %+v", got)
	}
}

// TestCollectionSchemaJSONFromFlags_BothFlagsError checks the mutually-exclusive guard.
func TestCollectionSchemaJSONFromFlags_BothFlagsError(t *testing.T) {
	_, err := collectionSchemaJSONFromFlags(`{"fields":[]}`, "status:select:open,done", strings.NewReader(""))
	if err == nil {
		t.Fatal("expected error when both --fields and --schema set, got nil")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutually-exclusive error, got: %v", err)
	}
}

// TestCollectionSchemaJSONFromFlags_MalformedJSON ensures the unmarshal error
// is wrapped with the "invalid --schema JSON" prefix so users can see which
// flag caused the problem.
func TestCollectionSchemaJSONFromFlags_MalformedJSON(t *testing.T) {
	_, err := collectionSchemaJSONFromFlags(`{this is not json`, "", strings.NewReader(""))
	if err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}
	if !strings.Contains(err.Error(), "invalid --schema JSON") {
		t.Fatalf("expected 'invalid --schema JSON' in error, got: %v", err)
	}
}

// TestCollectionSchemaJSONFromFlags_MissingFile ensures a clear error when
// the @path target doesn't exist.
func TestCollectionSchemaJSONFromFlags_MissingFile(t *testing.T) {
	_, err := collectionSchemaJSONFromFlags("@/nonexistent/path/schema.json", "", strings.NewReader(""))
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
	if !strings.Contains(err.Error(), "read --schema file") {
		t.Fatalf("expected 'read --schema file' in error, got: %v", err)
	}
}

// TestCollectionSchemaJSONFromFlags_EmptyFallsThroughToFields verifies that an
// empty --schema falls through to the --fields DSL parser (backward compat).
func TestCollectionSchemaJSONFromFlags_EmptyFallsThroughToFields(t *testing.T) {
	out, err := collectionSchemaJSONFromFlags("", "status:select:open,done", strings.NewReader(""))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var got models.CollectionSchema
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	if len(got.Fields) != 1 {
		t.Fatalf("expected 1 field, got %d", len(got.Fields))
	}
	f := got.Fields[0]
	if f.Key != "status" || f.Type != "select" {
		t.Fatalf("unexpected key/type: %+v", f)
	}
	// DSL preserves the legacy "first status select gets required+default" heuristic.
	if !f.Required || f.Default != "open" {
		t.Fatalf("expected legacy DSL heuristic (required=true, default=open), got required=%v default=%v", f.Required, f.Default)
	}
}

// TestCollectionSchemaJSONFromFlags_BothEmptyReturnsEmptySchema verifies the
// no-flags case yields an empty schema (and no error).
func TestCollectionSchemaJSONFromFlags_BothEmptyReturnsEmptySchema(t *testing.T) {
	out, err := collectionSchemaJSONFromFlags("", "", strings.NewReader(""))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != `{"fields":null}` && out != `{"fields":[]}` && out != `{}` {
		// We accept any JSON shape that round-trips to a Fields-less schema —
		// the wire format here is opaque to callers.
		var got models.CollectionSchema
		if jsonErr := json.Unmarshal([]byte(out), &got); jsonErr != nil {
			t.Fatalf("unparseable empty-schema output %q: %v", out, jsonErr)
		}
		if len(got.Fields) != 0 {
			t.Fatalf("expected empty Fields, got %d", len(got.Fields))
		}
	}
}

// TestCollectionSchemaJSONFromFlags_PreservesAllFieldDefProperties is the
// regression test for BUG-1284: confirms that every FieldDef property — not
// just the DSL-expressible subset — round-trips through --schema unchanged.
func TestCollectionSchemaJSONFromFlags_PreservesAllFieldDefProperties(t *testing.T) {
	in := `{
		"fields": [
			{
				"key": "status",
				"label": "Status",
				"type": "select",
				"options": ["idea","drafting","review","approved","scheduled","published","archived"],
				"terminal_options": ["published","archived"],
				"default": "idea",
				"required": true
			},
			{
				"key": "progress",
				"label": "Progress",
				"type": "number",
				"computed": true,
				"suffix": "%"
			},
			{
				"key": "parent_plan",
				"label": "Parent Plan",
				"type": "relation",
				"collection": "plans"
			}
		]
	}`
	out, err := collectionSchemaJSONFromFlags(in, "", strings.NewReader(""))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var got models.CollectionSchema
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	if len(got.Fields) != 3 {
		t.Fatalf("expected 3 fields, got %d", len(got.Fields))
	}

	status := got.Fields[0]
	if len(status.TerminalOptions) != 2 || status.TerminalOptions[0] != "published" || status.TerminalOptions[1] != "archived" {
		t.Errorf("status.terminal_options not preserved: %v", status.TerminalOptions)
	}
	if status.Default != "idea" {
		t.Errorf("status.default not preserved: %v", status.Default)
	}
	if !status.Required {
		t.Errorf("status.required not preserved")
	}

	progress := got.Fields[1]
	if !progress.Computed {
		t.Errorf("progress.computed not preserved")
	}
	if progress.Suffix != "%" {
		t.Errorf("progress.suffix not preserved: %q", progress.Suffix)
	}

	parent := got.Fields[2]
	if parent.Collection != "plans" {
		t.Errorf("relation.collection not preserved: %q", parent.Collection)
	}
}
