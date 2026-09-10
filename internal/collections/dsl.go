// Package collections holds collection-shape utilities shared by the
// CLI, the MCP HTTP dispatcher, and (eventually) any other surface
// that needs to translate user-friendly DSL forms into the canonical
// JSON shapes the API consumes.
//
// `dsl.go` is the home for the legacy "compact field DSL" parser that
// originally lived in cmd/pad/main.go. Moved up to this shared package
// (PR #572 follow-up to Codex finding on TASK-1510) so the MCP HTTP
// route mapper can accept `fields=...` for `collection update` without
// reimplementing the parser. The CLI calls the same helper via
// CollectionSchemaJSONFromDSL; both surfaces now have parity.
package collections

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/models"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

// ParseFieldsDSL parses the compact field DSL (key:type[:options];...)
// into a CollectionSchema. Empty input returns an empty schema with no
// error.
//
// First select field named `status` automatically gets `required=true`
// and `default=<first option>` — backward-compatible behavior preserved
// from the original cmd/pad implementation.
func ParseFieldsDSL(fieldsDSL string) (models.CollectionSchema, error) {
	schema := models.CollectionSchema{}
	if fieldsDSL == "" {
		return schema, nil
	}
	for _, f := range strings.Split(fieldsDSL, ";") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		parts := strings.SplitN(f, ":", 3)
		if len(parts) < 2 {
			return schema, fmt.Errorf("invalid field definition: %q (expected key:type[:options])", f)
		}
		fd := models.FieldDef{
			Key:   parts[0],
			Label: cases.Title(language.English).String(strings.ReplaceAll(parts[0], "_", " ")),
			Type:  parts[1],
		}
		if fd.Type == "relation" {
			// PLAN-2857 U6. For a relation the third part is the TARGET
			// COLLECTION slug, not an options list.
			//
			// It used to fall into Options like every other type, which did not
			// merely fail to declare the target: it produced a relation field
			// with Options=["colors"] and NO target, and every write to that
			// field was then refused with `target_missing`. A DSL that builds a
			// field nothing can ever write to is worse than one that refuses,
			// so a bare `owner:relation` is now an error at parse time.
			target := ""
			if len(parts) == 3 {
				target = strings.TrimSpace(parts[2])
			}
			if target == "" {
				return schema, fmt.Errorf("invalid field definition: %q — a relation needs its target collection (expected %s:relation:<collection-slug>)", f, fd.Key)
			}
			if strings.Contains(target, ",") {
				// A comma here means the caller wrote an options list, which is
				// the pre-U6 shape. Refusing names the mistake; accepting the
				// first item would silently pick a target they did not choose.
				return schema, fmt.Errorf("invalid field definition: %q — a relation takes ONE target collection, not a list", f)
			}
			fd.Collection = target
		} else if len(parts) == 3 && parts[2] != "" {
			fd.Options = strings.Split(parts[2], ",")
		}
		if fd.Type == "select" && fd.Key == "status" {
			fd.Required = true
			if len(fd.Options) > 0 {
				fd.Default = fd.Options[0]
			}
		}
		schema.Fields = append(schema.Fields, fd)
	}
	return schema, nil
}

// FieldsDSLToSchemaJSON parses the DSL and returns the marshaled
// CollectionSchema JSON string. Empty input returns "{}" — an empty
// schema. Convenience wrapper for callers that want to feed the
// result straight into models.CollectionUpdate.Schema (which is a
// *string).
func FieldsDSLToSchemaJSON(fieldsDSL string) (string, error) {
	schema, err := ParseFieldsDSL(fieldsDSL)
	if err != nil {
		return "", err
	}
	out, err := json.Marshal(schema)
	if err != nil {
		return "", fmt.Errorf("marshal schema from DSL: %w", err)
	}
	return string(out), nil
}
