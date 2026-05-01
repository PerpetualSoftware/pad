package cmdhelp

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// SchemaFileName is the conventional filename for the cmdhelp v0.1
// JSON Schema, located at `schema/cmdhelp.schema.json` relative to
// the pad repository root.
const SchemaFileName = "cmdhelp.schema.json"

// FindAndCompileSchema locates and compiles the cmdhelp v0.1 JSON
// Schema by walking up from `startDir` until it finds a directory
// containing `schema/cmdhelp.schema.json`. Returns the compiled
// schema ready for Validate(), or an error if the file isn't found
// or fails to compile.
//
// Used by tests in this package and in cmd/pad to validate emitted
// JSON against the published schema as a CI gate.
func FindAndCompileSchema(startDir string) (*jsonschema.Schema, error) {
	dir := startDir
	for {
		path := filepath.Join(dir, "schema", SchemaFileName)
		if _, err := os.Stat(path); err == nil {
			c := jsonschema.NewCompiler()
			s, err := c.Compile(path)
			if err != nil {
				return nil, fmt.Errorf("compile %s: %w", path, err)
			}
			return s, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, fmt.Errorf("schema/%s not found by walking up from %s", SchemaFileName, startDir)
		}
		dir = parent
	}
}
