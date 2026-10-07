package models

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// BuiltinOrigin is the built-in convention or playbook an item was made from
// (TASK-3462): the entry's key, the hash of the entry's text the item was
// given, and that text. Empty seed members mean the version is unknown.
type BuiltinOrigin struct {
	Key         string `json:"key"`
	SeedHash    string `json:"seed_hash,omitempty"`
	SeedContent string `json:"seed_content,omitempty"`
	SeedFields  string `json:"seed_fields,omitempty"`
}

var (
	builtinKeyShape  = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*(?:/[a-z0-9]+(?:-[a-z0-9]+)*){1,2}$`)
	builtinHashShape = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// MaxBuiltinKeyLen bounds a stored key. The longest compiled-in key is under
// 100 bytes; this leaves room without letting an import store an essay.
const MaxBuiltinKeyLen = 200

// MaxBuiltinSeedBytes bounds the stored seed text, body and fields each. The
// largest compiled-in body is under 40 KB.
const MaxBuiltinSeedBytes = 256 * 1024

// Validate refuses an origin whose key is not a lowercase path, whose hash is
// not a sha256 hex digest, whose seed text is oversized or whose seed fields
// are not a JSON object, or which carries seed text without the hash it is
// the text of. It does not require the key to name a built-in this binary
// knows: an imported origin may name one a newer Pad shipped. Whether the
// text hashes to SeedHash is the caller's check (collections owns the hash).
func (o BuiltinOrigin) Validate() error {
	if len(o.Key) > MaxBuiltinKeyLen || !builtinKeyShape.MatchString(o.Key) {
		return fmt.Errorf("built-in origin key %q is not a lowercase path like playbook/ship", o.Key)
	}
	if o.SeedHash != "" && !builtinHashShape.MatchString(o.SeedHash) {
		return fmt.Errorf("built-in origin seed_hash for %q is not a sha256 hex digest", o.Key)
	}
	if (o.SeedContent != "" || o.SeedFields != "") && o.SeedHash == "" {
		return fmt.Errorf("built-in origin for %q carries seed text without its hash", o.Key)
	}
	if len(o.SeedContent) > MaxBuiltinSeedBytes || len(o.SeedFields) > MaxBuiltinSeedBytes {
		return fmt.Errorf("built-in origin seed text for %q is over %d bytes", o.Key, MaxBuiltinSeedBytes)
	}
	if o.SeedFields != "" {
		var m map[string]any
		if err := json.Unmarshal([]byte(o.SeedFields), &m); err != nil || m == nil {
			return fmt.Errorf("built-in origin seed_fields for %q is not a JSON object", o.Key)
		}
		// An escape decoding to NUL is refused like a raw one (codex r1):
		// Postgres's jsonb refuses it, and SQLite's trigger 129 refuses both,
		// and a refused insert there poisons the whole import.
		if jsonHoldsNUL(m) {
			return fmt.Errorf("built-in origin seed_fields for %q holds a NUL", o.Key)
		}
	}
	if strings.ContainsRune(o.SeedContent, 0) || strings.ContainsRune(o.SeedFields, 0) {
		return fmt.Errorf("built-in origin seed text for %q holds a NUL", o.Key)
	}
	return nil
}

// jsonHoldsNUL reports whether any decoded string or key in v contains NUL.
func jsonHoldsNUL(v any) bool {
	switch t := v.(type) {
	case string:
		return strings.ContainsRune(t, 0)
	case map[string]any:
		for k, x := range t {
			if strings.ContainsRune(k, 0) || jsonHoldsNUL(x) {
				return true
			}
		}
	case []any:
		for _, x := range t {
			if jsonHoldsNUL(x) {
				return true
			}
		}
	}
	return false
}
