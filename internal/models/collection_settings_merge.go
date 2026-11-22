package models

import (
	"encoding/json"
	"sort"
	"strings"
)

// MergeCollectionSettings returns the settings a collection update stores
// (TASK-4732): patchJSON merged shallowly onto storedJSON. Every top-level key
// in the patch replaces the stored key whole (an object or array value is not
// merged into the stored one); a key the patch does not mention is kept; a key
// the patch sets to JSON null is deleted. An empty patch ("" or "{}") changes
// nothing. Stored settings that do not parse as an object count as {}. A patch
// that is not an object is refused with ErrSettingsNotObject.
//
// Values are carried as raw JSON, so a key this package does not know keeps
// its exact bytes. Keys are written sorted, so the output is deterministic.
func MergeCollectionSettings(storedJSON, patchJSON string) (string, error) {
	patch := map[string]json.RawMessage{}
	if raw := strings.TrimSpace(patchJSON); raw != "" {
		if err := json.Unmarshal([]byte(raw), &patch); err != nil || patch == nil {
			return "", ErrSettingsNotObject
		}
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal([]byte(storedJSON), &merged); err != nil || merged == nil {
		merged = map[string]json.RawMessage{}
	}
	for k, v := range patch {
		if strings.TrimSpace(string(v)) == "null" {
			delete(merged, k)
			continue
		}
		merged[k] = v
	}
	// Written by hand rather than with json.Marshal, which compacts and
	// HTML-escapes every raw value and so would rewrite keys it was told to
	// keep.
	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		name, err := json.Marshal(k)
		if err != nil {
			return "", err
		}
		b.Write(name)
		b.WriteByte(':')
		b.Write(merged[k])
	}
	b.WriteByte('}')
	return b.String(), nil
}
