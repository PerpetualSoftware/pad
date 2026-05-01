package cmdhelp

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// loadSchemaForTest is a test-only wrapper around FindAndCompileSchema
// that fails the test on error and starts the walk from the test's CWD.
func loadSchemaForTest(t *testing.T) *jsonschema.Schema {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	schema, err := FindAndCompileSchema(cwd)
	if err != nil {
		t.Fatalf("load schema: %v", err)
	}
	return schema
}

// validateAgainstSchema runs the compiled schema against the given JSON
// document and returns the validation error (nil on success).
func validateAgainstSchema(t *testing.T, schema *jsonschema.Schema, jsonBytes []byte) error {
	t.Helper()
	var doc interface{}
	if err := json.Unmarshal(jsonBytes, &doc); err != nil {
		t.Fatalf("unmarshal emitted JSON: %v", err)
	}
	return schema.Validate(doc)
}

func TestEmittedJSON_ValidatesAgainstSchema_StaticTree(t *testing.T) {
	// Lock the static-tree path: emit the synthetic tree (no resolver,
	// no dynamic enums) and assert the result satisfies every contract
	// in cmdhelp.schema.json. This protects every TASK-934/935 emitter
	// change going forward — break the schema contract and CI fails.
	root := buildSyntheticTree()
	var buf bytes.Buffer
	if err := EmitJSON(root, root, Options{
		Binary:   "padtest",
		Version:  "test",
		Homepage: "https://example.test",
		MaxDepth: -1,
	}, &buf); err != nil {
		t.Fatalf("EmitJSON: %v", err)
	}

	schema := loadSchemaForTest(t)
	if err := validateAgainstSchema(t, schema, buf.Bytes()); err != nil {
		t.Errorf("synthetic tree's emitted JSON fails schema validation:\n%v\n--- output ---\n%s", err, buf.String())
	}
}

func TestEmittedJSON_ValidatesAgainstSchema_AfterDynamicResolution(t *testing.T) {
	// Same lock, post-resolver: dynamic enum injection must not break
	// schema validity. Common regressions this guards against:
	//   - dynamic enum_source not matching `^dynamic:.+$`
	//   - non-numeric exit_code keys after some future templating
	//   - flag-name keys violating the propertyNames pattern
	root := buildSyntheticTree()
	r := &Resolver{
		Workspace: "fixture",
		ArgEnumSources: map[string]string{
			"collection": EnumSourceCollections,
		},
		Sources: map[string]DynamicEnum{
			EnumSourceCollections: func() ([]interface{}, error) {
				return []interface{}{"tasks", "ideas"}, nil
			},
		},
	}
	var buf bytes.Buffer
	if err := EmitJSON(root, root, Options{
		Binary:   "padtest",
		MaxDepth: -1,
		Resolver: r,
	}, &buf); err != nil {
		t.Fatalf("EmitJSON: %v", err)
	}

	schema := loadSchemaForTest(t)
	if err := validateAgainstSchema(t, schema, buf.Bytes()); err != nil {
		t.Errorf("post-resolver JSON fails schema validation:\n%v\n--- output ---\n%s", err, buf.String())
	}
}

func TestEmittedJSON_ValidatesAgainstSchema_NoWorkspaceFallback(t *testing.T) {
	// When dynamic resolution would fail (no workspace, no live values),
	// the document must STILL validate against the schema. This is the
	// graceful-fallback guarantee from TASK-936.
	root := buildSyntheticTree()
	r := &Resolver{
		// No workspace, no sources — resolver is essentially a no-op
		// shape but with the path set up for testing the fallback.
	}
	var buf bytes.Buffer
	if err := EmitJSON(root, root, Options{
		Binary:   "padtest",
		MaxDepth: -1,
		Resolver: r,
	}, &buf); err != nil {
		t.Fatalf("EmitJSON: %v", err)
	}

	schema := loadSchemaForTest(t)
	if err := validateAgainstSchema(t, schema, buf.Bytes()); err != nil {
		t.Errorf("no-workspace fallback JSON fails schema validation:\n%v", err)
	}
}
