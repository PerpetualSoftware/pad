package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// fieldsJSONFlagUsage describes --fields-json on item create and update.
const fieldsJSONFlagUsage = `set fields from one JSON object, values keeping their JSON type (e.g. '{"labels":["a","b"],"meta":{"k":1}}'); ` +
	`for structured values --field key=value cannot carry. A key also set by --field or a dedicated flag is refused, and so is null`

// applyFieldsJSON merges a --fields-json object into dst (TASK-3611). It is
// how the local stdio MCP server writes a structured field value: --field
// key=value carries text, and the remote transport already sends these
// values as JSON. Values keep their JSON form (numbers keep their digits,
// as on the remote door). A key dst already holds, from --field or a
// dedicated flag, is refused rather than resolved, and so is null, which
// the MCP `fields` param refuses too.
func applyFieldsJSON(dst map[string]any, raw string) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var obj any
	if err := models.DecodeJSONKeepingNumbers([]byte(raw), &obj); err != nil {
		return fmt.Errorf("--fields-json: not valid JSON: %w", err)
	}
	m, ok := obj.(map[string]any)
	if !ok {
		return fmt.Errorf("--fields-json: must be a JSON object of field values")
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := m[k]
		if strings.TrimSpace(k) == "" {
			return fmt.Errorf("--fields-json: a field key is empty")
		}
		if v == nil {
			return fmt.Errorf("--fields-json: %s is null; omit the key to leave the field unchanged", k)
		}
		if _, dup := dst[k]; dup {
			return fmt.Errorf("--fields-json: %s is also set by another flag; set it once", k)
		}
		dst[k] = v
	}
	return nil
}
