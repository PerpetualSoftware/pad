package models

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// FirstDuplicateJSONKey reports the first object member name that appears twice
// in the same object, at any depth.
//
// It walks EVERY depth: each object on the token stack keeps its own set of
// member names, so {"a":1,"a":2} and {"x":{"k":1,"k":2}} are both reported,
// and a name repeated in two DIFFERENT objects ({"x":{"k":1},"y":{"k":2}}) is
// not a duplicate. Moved here from internal/server (BUG-2896) so the store's
// import door can ask the same question the request-body gate asks.
//
// A token walk rather than a decode, because a decode is exactly what loses the
// information: by the time there is a map, the duplicate is gone.
//
// Malformed input answers false — the caller has already checked json.Valid,
// and a decode error there is the caller's to report, not this function's to
// duplicate.
func FirstDuplicateJSONKey(raw []byte) (string, bool) {
	type frame struct {
		isObject  bool
		seen      map[string]bool
		expectKey bool
	}
	var stack []*frame
	top := func() *frame {
		if len(stack) == 0 {
			return nil
		}
		return stack[len(stack)-1]
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", false // EOF, or malformed — nothing to report either way.
		}
		if d, isDelim := tok.(json.Delim); isDelim {
			switch d {
			case '{':
				stack = append(stack, &frame{isObject: true, seen: map[string]bool{}, expectKey: true})
			case '[':
				stack = append(stack, &frame{})
			case '}', ']':
				if len(stack) > 0 {
					stack = stack[:len(stack)-1]
				}
				// The container that just closed WAS a value of its parent, so
				// the parent's next token is a key again.
				if f := top(); f != nil && f.isObject {
					f.expectKey = true
				}
			}
			continue
		}
		f := top()
		if f == nil || !f.isObject {
			continue
		}
		if f.expectKey {
			if name, isString := tok.(string); isString {
				if f.seen[name] {
					return name, true
				}
				f.seen[name] = true
			}
			f.expectKey = false
			continue
		}
		// A scalar value; the next token in this object is a key.
		f.expectKey = true
	}
}

// CollapseDuplicateJSONKeys returns an item fields blob unchanged when no
// object in it repeats a member name, and otherwise re-encoded from Go's decode
// with the repeats collapsed (BUG-2896).
//
// Why: SQLite's json_extract reads the FIRST occurrence of a repeated key,
// while Go (last wins) and Postgres jsonb (last wins) read the LAST, so on
// SQLite a status report, a filter or an index and every Go reader of the same
// row can disagree about what the row says. Storing the collapsed form makes
// them agree by construction. A map decode replaces a repeated key's value
// WHOLESALE, as jsonb does, which is how every item-field reader decodes the
// blob. Numbers keep their literals (DecodeJSONKeepingNumbers).
//
// Bytes are rewritten ONLY when a repeat exists: every other blob is returned
// verbatim, so an import stays byte-faithful except in the one case where the
// two databases already disagree about what the bytes mean. dupKey names the
// first repeated member, for the caller's warning.
func CollapseDuplicateJSONKeys(raw string) (out, dupKey string, collapsed bool, err error) {
	key, dup := FirstDuplicateJSONKey([]byte(raw))
	if !dup {
		return raw, "", false, nil
	}
	var v any
	if err := DecodeJSONKeepingNumbers([]byte(raw), &v); err != nil {
		return raw, "", false, fmt.Errorf("collapse duplicate keys: %w", err)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return raw, "", false, fmt.Errorf("collapse duplicate keys: %w", err)
	}
	return string(bytes.TrimRight(buf.Bytes(), "\n")), key, true, nil
}
