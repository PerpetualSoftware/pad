package items

import (
	"errors"
	"fmt"
	"strings"
)

// ErrFieldEntryMalformed reports a `--field` / `field:[…]` entry with no
// usable `key=value` split: no `=` at all, or an `=` in the first position so
// the key half is empty.
//
// It is returned rather than handled here because the six call sites
// deliberately disagree about it and that disagreement predates this helper:
// `item create`, `item list`, `item update` and `item move` skip such an entry
// silently, `item copy` treats it as a hard error (its whole contract is "you
// were told what to fix"), and the remote door drops it. Unifying THAT is a
// separate decision from the one this helper exists to enforce, so callers
// keep their own disposition by testing for this error.
var ErrFieldEntryMalformed = errors.New("field entry is not key=value")

// SplitFieldEntry splits one `key=value` field entry into its halves.
//
// It exists because six sites parsed that entry independently — `item create`,
// `item list`, `item update`, `item move` and `item copy` in cmd/pad, plus
// ingestFieldKVP on the remote /mcp door — in four spellings, and they did not
// agree on what the entry MEANT (BUG-2870). The CLI sites used the halves
// verbatim, so `--field " effort=l"` stored an undeclared field literally named
// " effort" and left the declared `effort` untouched, while the remote door
// trimmed both halves and wrote `effort`. Same call, two different stored keys,
// decided by nothing but which transport the caller happened to be on.
//
// Two rules, and they are deliberately asymmetric:
//
//   - A KEY whose trimmed form differs from what the caller wrote is REFUSED,
//     on every door. Trimming it silently would retarget the write to a
//     different field than the one the caller typed, and a caller who has a
//     field whose key genuinely contains a space would have their data quietly
//     moved. Refusing tells them immediately instead of leaving a ghost field
//     to be discovered later.
//
//   - A VALUE is returned VERBATIM, on every door. A door that trims a value is
//     a door that reinterprets a caller's bytes, and on a text field leading or
//     trailing space is content, not noise. A padded value against a typed
//     field is refused one layer down by field validation, with a message
//     naming the field — the same answer on both doors, because the server
//     types declared fields rather than the client.
func SplitFieldEntry(entry string) (key, value string, err error) {
	idx := strings.Index(entry, "=")
	if idx <= 0 {
		return "", "", ErrFieldEntryMalformed
	}
	rawKey := entry[:idx]
	value = entry[idx+1:]

	key = strings.TrimSpace(rawKey)
	if key != rawKey {
		if key == "" {
			return "", "", fmt.Errorf(
				"field entry %q has an empty key: %q is only whitespace, so there is no field to write",
				entry, rawKey)
		}
		return "", "", fmt.Errorf(
			"field entry %q has whitespace around its key: write %q, not %q — "+
				"the padded form would store a separate, undeclared field",
			entry, key+"="+value, rawKey+"="+value)
	}
	return key, value, nil
}
