package store

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// FieldUsage counts, per field key, the live items in one collection that
// hold a value there (TASK-2188), and per string value how many of those
// items hold it. A schema edit that removes a field or a select option reads
// it to say how many items keep a value the schema no longer shows.
//
// The scan is in Go, over the stored fields blobs, rather than in SQL: the
// question is "does the validator's notion of a value live here", and the
// validator is Go. Doing it per dialect would be two more readings of
// "empty" to keep in step (see field_value_dialect_test.go for what that
// costs).
type FieldUsage struct {
	Fields map[string]FieldKeyUsage `json:"fields"`
}

// FieldKeyUsage is one key's count. Values counts each string value (a
// select's value, or each distinct element of a multi_select array) by the
// number of items holding it; a non-string value counts toward Items only.
type FieldKeyUsage struct {
	Items  int            `json:"items"`
	Values map[string]int `json:"values,omitempty"`
}

// CollectionFieldUsage scans the live (not soft-deleted) items of one
// collection. Every key present in a blob is counted, declared or not, so a
// field removed earlier still shows what it retains.
func (s *Store) CollectionFieldUsage(collectionID string) (*FieldUsage, error) {
	rows, err := s.db.Query(s.q(`
		SELECT fields FROM items
		WHERE collection_id = ? AND deleted_at IS NULL
	`), collectionID)
	if err != nil {
		return nil, fmt.Errorf("field usage: %w", err)
	}
	defer rows.Close()

	usage := &FieldUsage{Fields: map[string]FieldKeyUsage{}}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("field usage scan: %w", err)
		}
		var blob map[string]any
		if strings.TrimSpace(raw) == "" || json.Unmarshal([]byte(raw), &blob) != nil {
			// An unreadable blob holds no value anyone can see; it counts
			// toward nothing rather than failing the whole report.
			continue
		}
		for key, val := range blob {
			if fieldValueEmpty(val) {
				continue
			}
			u := usage.Fields[key]
			u.Items++
			for _, v := range fieldStringValues(val) {
				if u.Values == nil {
					u.Values = map[string]int{}
				}
				u.Values[v]++
			}
			usage.Fields[key] = u
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("field usage rows: %w", err)
	}
	return usage, nil
}

// fieldValueEmpty is the absent-value rule: null, "", [] and {} hold nothing.
// false and 0 are values.
func fieldValueEmpty(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	}
	return false
}

// fieldStringValues is the distinct string values one stored value holds: the
// string itself, or each distinct string element of an array.
func fieldStringValues(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		seen := map[string]bool{}
		var out []string
		for _, e := range t {
			if s, ok := e.(string); ok && s != "" && !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// OrphanedBySchemaEdit names what going from prev to next removed that items
// still hold, read from usage taken AFTER the edit (so a value an option
// rename migrated away is already gone and is not reported). A field removed
// reports its item count; an option removed from a field that is still a
// select or multi_select reports the items holding it. Removals that orphan
// nothing are not reported. Ordered by field, then option.
func OrphanedBySchemaEdit(prev, next models.CollectionSchema, usage *FieldUsage) []models.OrphanedValue {
	if usage == nil {
		return nil
	}
	nextByKey := make(map[string]models.FieldDef, len(next.Fields))
	for _, f := range next.Fields {
		nextByKey[f.Key] = f
	}
	var out []models.OrphanedValue
	for _, p := range prev.Fields {
		u := usage.Fields[p.Key]
		n, kept := nextByKey[p.Key]
		if !kept {
			if u.Items > 0 {
				out = append(out, models.OrphanedValue{Field: p.Key, Items: u.Items})
			}
			continue
		}
		if !isOptionType(p.Type) || !isOptionType(n.Type) {
			continue
		}
		still := make(map[string]bool, len(n.Options))
		for _, o := range n.Options {
			still[o] = true
		}
		for _, o := range p.Options {
			if !still[o] && u.Values[o] > 0 {
				out = append(out, models.OrphanedValue{Field: p.Key, Option: o, Items: u.Values[o]})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Field != out[j].Field {
			return out[i].Field < out[j].Field
		}
		return out[i].Option < out[j].Option
	})
	return out
}

// SchemaEditRemoves reports whether going from prev to next drops a field or
// drops an option from a field that stays a select or multi_select: the only
// edits OrphanedBySchemaEdit can report, so a caller can skip the item scan
// for every other edit.
func SchemaEditRemoves(prev, next models.CollectionSchema) bool {
	nextByKey := make(map[string]models.FieldDef, len(next.Fields))
	for _, f := range next.Fields {
		nextByKey[f.Key] = f
	}
	for _, p := range prev.Fields {
		n, kept := nextByKey[p.Key]
		if !kept {
			return true
		}
		if !isOptionType(p.Type) || !isOptionType(n.Type) {
			continue
		}
		still := make(map[string]bool, len(n.Options))
		for _, o := range n.Options {
			still[o] = true
		}
		for _, o := range p.Options {
			if !still[o] {
				return true
			}
		}
	}
	return false
}

func isOptionType(t string) bool { return t == "select" || t == "multi_select" }
