package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// importInvocationSlugKey is the one item field with a unique index behind it
// (idx_items_invocation_slug_per_collection, migrations 054 / PG 033). The
// index covers every collection, whatever its schema declares, so the import
// treats the key as unique everywhere: a duplicate in the archive used to fail
// the whole import on the raw constraint (BUG-3825).
const importInvocationSlugKey = "invocation_slug"

// importUniqueKeys returns the field keys whose values must be unique among a
// collection's live items: every field declaring unique_scope:
// workspace_collection, plus invocation_slug. Each key once, since a schema
// the import stores as carried may declare one twice, and a key checked twice
// would collide with its own claim (codex r1). Sorted, so drops are reported
// in a stable order.
func importUniqueKeys(schema models.CollectionSchema) []string {
	seen := map[string]bool{importInvocationSlugKey: true}
	keys := []string{importInvocationSlugKey}
	for _, def := range schema.Fields {
		if def.UniqueScope == "workspace_collection" && !seen[def.Key] {
			seen[def.Key] = true
			keys = append(keys, def.Key)
		}
	}
	sort.Strings(keys)
	return keys
}

// importUniqueValue is the value reported for a field, and the slots it
// claims. A declared unique_scope field is checked as checkUniqueFields checks
// it: a non-empty string only, one slot.
//
// invocation_slug is checked as its INDEX compares it, and the two dialects'
// indexes compare differently (hunter r1). Postgres reads fields->>'key', the
// text form, so the string "true" and the boolean true collide. SQLite reads
// json_extract, a typed value, so true and 1 collide (a JSON true is integer
// 1), and so do 1 and 1.0, while the string "1" and the number 1 do not. A
// value claims one slot under each rule, and collides when either is taken: an
// archive that imports on one dialect imports on the other.
func importUniqueValue(key string, v any) (string, []string, bool) {
	if s, ok := v.(string); ok {
		if s == "" {
			return "", nil, false
		}
		if key != importInvocationSlugKey {
			return s, []string{"s:" + s}, true
		}
		return s, []string{"pg:" + s, "sqlite:s:" + s}, true
	}
	if v == nil || key != importInvocationSlugKey {
		return "", nil, false
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", nil, false
	}
	text := string(b)
	switch t := v.(type) {
	case bool:
		n := "0"
		if t {
			n = "1"
		}
		return text, []string{"pg:" + text, "sqlite:n:" + n}, true
	case json.Number:
		// Numbers compare by value under both rules here. Postgres prints a
		// jsonb number as written (1.0 stays "1.0"), so this merges more than
		// its index would; dropping there is harmless.
		f, err := t.Float64()
		if err != nil {
			return text, []string{"pg:" + text, "sqlite:s:" + text}, true
		}
		n := strconv.FormatFloat(f, 'g', -1, 64)
		return text, []string{"pg:" + n, "sqlite:n:" + n}, true
	default:
		// An object or array: both dialects read it as its JSON text.
		return text, []string{"pg:" + text, "sqlite:s:" + text}, true
	}
}

// dropImportedUniqueDuplicates removes from fieldsJSON every unique value an
// earlier item of the same collection already claimed, and claims the rest
// for itemRef. A blob with nothing to drop is returned byte for byte; one with
// a drop is re-encoded with the other values unchanged.
//
// It does not check required fields. The import never has: it stores each
// item's fields as the archive carries them.
func dropImportedUniqueDuplicates(fieldsJSON, collID, itemRef string, keys []string, claimed map[[3]string]string) (string, []models.ImportUniqueDrop, error) {
	dec := json.NewDecoder(bytes.NewReader([]byte(fieldsJSON)))
	dec.UseNumber()
	var fields map[string]any
	if err := dec.Decode(&fields); err != nil || fields == nil {
		// coerceJSONForImport has already made this an object; anything else
		// carries no field values to compare.
		return fieldsJSON, nil, nil
	}
	var drops []models.ImportUniqueDrop
	for _, key := range keys {
		val, slots, ok := importUniqueValue(key, fields[key])
		if !ok {
			continue
		}
		holder := ""
		for _, sl := range slots {
			if h, taken := claimed[[3]string{collID, key, sl}]; taken {
				holder = h
				break
			}
		}
		if holder != "" {
			delete(fields, key)
			drops = append(drops, models.ImportUniqueDrop{Item: itemRef, Key: key, Value: val, Holder: holder})
			continue
		}
		for _, sl := range slots {
			claimed[[3]string{collID, key, sl}] = itemRef
		}
	}
	if len(drops) == 0 {
		return fieldsJSON, nil, nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(fields); err != nil {
		return "", nil, fmt.Errorf("re-encode fields after dropping unique values: %w", err)
	}
	return string(bytes.TrimRight(buf.Bytes(), "\n")), drops, nil
}
