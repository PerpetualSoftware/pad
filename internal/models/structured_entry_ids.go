package models

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

// structuredEntryPrefixes are the namespaces derived ids are minted in, and
// the order the timeline and the repair walk the two kinds: notes, then
// decisions. Both kinds share one uniqueness map, so a raw id carrying EITHER prefix has
// to be diverted regardless of which kind is being numbered — otherwise one
// kind's kept id can collide with the other kind's derived id.
var structuredEntryPrefixes = []string{"note", "decision"}

// StructuredEntryIDKey is the ONE rule for what a structured entry's stored id
// means as a timeline id (BUG-2783, BUG-2788). It returns the key the entry
// claims and whether its raw id is usable at all:
//
//   - a raw id that is empty, not valid UTF-8, or carries a NUL is unusable;
//   - a raw id that could be confused with a row id (UUID-shaped) or with a
//     derived id (already "<prefix>:"-prefixed) is DIVERTED to prefix+":"+raw,
//     so kept and derived ids live in disjoint namespaces;
//   - any other raw id is its own key.
//
// The timeline (structuredTimelineEntries) and the persisted-id repair
// (EnsureStructuredEntryIDs) both call it, so the entries the repair re-mints
// are exactly the ones the timeline would otherwise have put on its
// index-dependent positional fallback, and no others.
func StructuredEntryIDKey(raw, prefix string) (key string, usable bool) {
	if raw == "" || !utf8.ValidString(raw) || strings.ContainsRune(raw, 0) {
		return "", false
	}
	if uuid.Validate(raw) == nil || hasStructuredEntryPrefix(raw) {
		return prefix + ":" + raw, true
	}
	return raw, true
}

// Why a UUID-SHAPED raw id is diverted. This reasoning is BUG-2783's,
// unchanged; it moved here from the timeline's looksLikeRowID with the rule
// itself (BUG-2788). A UUID-shaped raw id could be the id of a comment,
// activity or version row on the same item.
//
// Those are the only ids a structured entry can collide with in the merged
// timeline, and every one of them is minted by store.newID(), which is
// uuid.New().String(). Enumerated rather than sampled: the six INSERT sites
// into comments / activities / item_versions all pass newID(), and the import
// path re-mints instead of carrying an artifact's ids across.
//
// So the test is the UUID SHAPE, not membership in any particular set — which
// is what keeps the answer independent of which rows a given page fetched.
//
// SCOPE, because this is a property of the WRITERS and not of the schema: all
// three tables declare `id TEXT PRIMARY KEY` with no format constraint, and
// migrations carry existing ids across verbatim. A row whose id is not a UUID
// — written by some future path that does not go through newID(), or already
// present in a database this code has never seen — would be outside what this
// refuses, and a blob id equal to it would collide again. The same gap has a
// second face: the three SQL sources keep their own ids verbatim and are not
// deduped against EACH OTHER, so two rows in different tables sharing an id
// would collide in the merged stream without any structured entry involved.
// Nothing here can detect either without consulting the rows, which is the
// dependency the whole design exists to avoid. It is latent rather than reachable through any
// current write or import path (codex round 2, P2), and the enforcement that
// would close it belongs at the writers or the schema, not here.
// The cost is that a blob id that happens to be a well-formed UUID loses its
// raw id even when nothing collides with it; that is accepted, because such an
// id is indistinguishable from a colliding one without consulting the very
// rows this must not depend on.
func hasStructuredEntryPrefix(raw string) bool {
	for _, p := range structuredEntryPrefixes {
		if strings.HasPrefix(raw, p+":") {
			return true
		}
	}
	return false
}

// EnsureStructuredEntryIDs gives every implementation note and decision-log
// entry a PERSISTED id that nothing else in the blob claims (BUG-2788).
//
// An entry whose id is absent or unusable, or whose key an EARLIER entry
// already claims (notes first, then decisions, the order the timeline walks),
// gets a freshly minted id. The FIRST holder of a duplicated id keeps it,
// which is who holds it in the timeline today. Every other entry is left
// byte-for-byte alone, including ids the timeline diverts to a derived form.
//
// Without it those entries fell to the timeline's positional fallback,
// "<prefix>-idx-<i>", which changes whenever an earlier entry is inserted or
// removed, or when the first of two duplicates is deleted and the survivor's
// id changes KIND. The id is the cursor's tie-breaker and the client's list
// key, so an entry could be skipped or shown twice across a page boundary.
// A persisted id makes an entry's identity independent of its siblings.
//
// It edits the decoded JSON directly, so unknown keys inside an entry survive
// untouched, and it only touches arrays the timeline can read (see
// timelineCanRead). It is idempotent: a second pass finds nothing to change. changed is false, and fixed equals the input,
// when nothing needed an id.
func EnsureStructuredEntryIDs(fieldsJSON string) (fixed string, changed bool, err error) {
	// Decoded with UseNumber: the backfill rewrites blobs nothing else is
	// touching, and a float64 round trip would silently alter any number in
	// them (an integer past 2^53, "1.0" re-emitted as "1"). json.Number
	// re-marshals as the literal it was read from.
	if fieldsJSON == "" || fieldsJSON == "{}" {
		return fieldsJSON, false, nil
	}
	dec := json.NewDecoder(strings.NewReader(fieldsJSON))
	dec.UseNumber()
	var fieldsMap map[string]any
	if dec.Decode(&fieldsMap) != nil || fieldsMap == nil {
		return fieldsJSON, false, nil
	}
	// Exactly ONE JSON value, or leave the blob alone: a single Decode stops
	// after the first value, so trailing bytes would otherwise be dropped,
	// turning a blob every other reader rejects into a valid one silently.
	if _, err := dec.Token(); err != io.EOF {
		return fieldsJSON, false, nil
	}
	// Only kinds the TIMELINE can read. It extracts each array into a typed
	// slice, and an array with a non-object element or a non-string id fails
	// that whole decode and shows nothing for the kind. Repairing inside such
	// an array would stabilise ids nothing displays, and replacing a
	// non-string id would change what the timeline shows at all. So the
	// repaired population is exactly the one the timeline numbers by position.
	arrays := map[string][]any{}
	for _, key := range []string{ItemFieldImplementationNotes, ItemFieldDecisionLog} {
		list, isList := fieldsMap[key].([]any)
		if !isList || !timelineCanRead(key, list) {
			continue
		}
		arrays[key] = list
	}
	if len(arrays) == 0 {
		return fieldsJSON, false, nil
	}
	used := map[string]bool{}
	for i, key := range []string{ItemFieldImplementationNotes, ItemFieldDecisionLog} {
		prefix := structuredEntryPrefixes[i]
		for _, raw := range arrays[key] {
			entry, isObject := raw.(map[string]any)
			if !isObject {
				continue
			}
			id, _ := entry["id"].(string)
			if k, usable := StructuredEntryIDKey(id, prefix); usable && !used[k] {
				used[k] = true
				continue
			}
			minted := NewStructuredEntryID(prefix)
			for used[minted] {
				// Two mints inside one clock tick are possible on a coarse
				// clock; the key must be unique within the blob.
				minted += "x"
			}
			used[minted] = true
			entry["id"] = minted
			changed = true
		}
	}
	if !changed {
		return fieldsJSON, false, nil
	}
	// Without HTML escaping, so a repaired blob differs from the stored one
	// in the ids and in formatting only; values, including every string, are
	// unchanged (json.Marshal would rewrite <, > and & as \u escapes).
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(fieldsMap); err != nil {
		return fieldsJSON, false, fmt.Errorf("marshal item fields: %w", err)
	}
	return strings.TrimSuffix(buf.String(), "\n"), true, nil
}

// timelineCanRead reports whether the timeline's typed extraction accepts
// this array, i.e. whether its entries are displayed at all.
func timelineCanRead(key string, list []any) bool {
	raw, err := json.Marshal(list)
	if err != nil {
		return false
	}
	if key == ItemFieldImplementationNotes {
		var notes []ItemImplementationNote
		return json.Unmarshal(raw, &notes) == nil
	}
	var entries []ItemDecisionLogEntry
	return json.Unmarshal(raw, &entries) == nil
}
