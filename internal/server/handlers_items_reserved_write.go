package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3163: the two doors a caller's hand-written reserved metadata key could
// still reach after BUG-2696 closed the field-patch door — item CREATE's
// `fields`, and a FULL `fields` blob on update.
//
// Create refuses the key outright. Its only legitimate writer, convention
// activation, now sends the typed ItemCreate.Convention member instead (four
// writers: the CLI and remote MCP library activate, the web library activate,
// and the web Conventions page's create form).
//
// A full `fields` update refuses a reserved key it would CHANGE, and that
// includes deleting one by omitting it, because the store replaces the whole
// blob (lead ruling on BUG-3163 checkpoint 3). CARRYING the stored value
// unchanged stays legal, since a full-blob round-trip must keep working.
// Deletes have typed routes (clear_github_pr) or none. A server-side
// "preserve what the caller left out" was weighed and rejected: it silently
// answers a request the caller did not make.

// quoteKeys renders keys as a comma-separated list of quoted names.
func quoteKeys(keys []string) string {
	quoted := make([]string, 0, len(keys))
	for _, k := range keys {
		quoted = append(quoted, fmt.Sprintf("%q", k))
	}
	return strings.Join(quoted, ", ")
}

// reservedFieldCreateMessage renders the refusal for a reserved key in create's
// `fields`. There is no stored item yet, so unlike reservedFieldPatchMessage it
// has no "already unreadable" case to qualify.
func reservedFieldCreateMessage(keys []string) string {
	var b strings.Builder
	if len(keys) == 1 {
		fmt.Fprintf(&b, "%q is system metadata and cannot be set through an item create's fields.", keys[0])
	} else {
		fmt.Fprintf(&b, "%s are system metadata and cannot be set through an item create's fields.", quoteKeys(keys))
	}
	anyAppendBacked := false
	for _, k := range keys {
		if k == models.ItemFieldConvention {
			b.WriteString(` Send convention metadata as the typed "convention" member of the create request, which is what library activation sends.`)
			continue
		}
		if remedy := reservedFieldRemedy(k); remedy != "" {
			fmt.Fprintf(&b, " Maintain %s with %s once the item exists.", k, remedy)
		}
		if appendBackedReservedKey(k) {
			anyAppendBacked = true
		}
	}
	if anyAppendBacked {
		b.WriteString(" A raw field write stores a value Pad cannot read back, and the append path then" +
			" refuses on this item until the stored value is repaired (BUG-2627).")
	}
	return b.String()
}

// reservedFieldsCarryError is the in-transaction refusal of a full `fields`
// update that would change stored reserved metadata. Changed names keys the
// write carries with a value different from the stored one (or that are not
// stored at all); Omitted names keys that are stored but absent from the write.
type reservedFieldsCarryError struct {
	Changed []string
	Omitted []string
}

func (e *reservedFieldsCarryError) Error() string {
	return reservedFullFieldsMessage(e.Changed, e.Omitted)
}

// callerReservedFields returns the reserved keys of a caller's full `fields`
// blob, with their values. It is taken from the caller's map as parsed, before
// any validation pass touches it, because the carry check is a question about
// what the CALLER sent.
func callerReservedFields(fieldMap map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range fieldMap {
		if models.IsReservedItemField(k) {
			out[k] = v
		}
	}
	return out
}

// reservedCarryViolations compares a caller's reserved keys against the stored
// fields blob. The comparison is SEMANTIC: both sides are decoded JSON compared
// with reflect.DeepEqual, never bytes, because Postgres JSONB re-spaces and
// reorders a stored object (the first CI run on #1464 failed on exactly that).
// A stored blob that does not decode as an object is treated as EMPTY, on
// purpose (codex round 1 on BUG-3163 raised it as a bypass). Every reader of
// reserved metadata parses the blob as an object first (models.parseItemFields),
// so such a row holds no readable reserved metadata for this check to protect,
// and a full `fields` write is the only repair path the unreadable-state
// message points callers at. Refusing here would make the row unrepairable.
// A caller key then counts as a SET, and is still refused.
func reservedCarryViolations(caller map[string]any, storedFieldsJSON string) (changed, omitted []string) {
	stored := map[string]any{}
	if storedFieldsJSON != "" {
		_ = json.Unmarshal([]byte(storedFieldsJSON), &stored)
		if stored == nil {
			stored = map[string]any{}
		}
	}
	for k, v := range caller {
		sv, ok := stored[k]
		if !ok || !reflect.DeepEqual(normalizeJSONValue(v), sv) {
			changed = append(changed, k)
		}
	}
	for k := range stored {
		if !models.IsReservedItemField(k) {
			continue
		}
		if _, ok := caller[k]; !ok {
			omitted = append(omitted, k)
		}
	}
	sort.Strings(changed)
	sort.Strings(omitted)
	return changed, omitted
}

// normalizeJSONValue round-trips a value through JSON so a caller value that is
// not already in decoded-JSON form (a typed struct, an int) compares equal to
// its stored decoding. Values straight out of json.Unmarshal pass through with
// the same shape.
func normalizeJSONValue(v any) any {
	raw, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return v
	}
	return out
}

// composeReservedCarryGuard wraps an update precheck with the full-fields
// carry check. It runs against the row as re-read UNDER THE WRITE LOCK, so a
// note appended between the handler's read and this write counts as stored:
// a blob built before that append omits it and is refused, rather than
// silently deleting it. Same composition as composePendingContentGuard
// (BUG-3133), so every ordering the update handler has inherits it.
func composeReservedCarryGuard(inner func(*sql.Tx, *models.Item) error, caller map[string]any) func(*sql.Tx, *models.Item) error {
	return func(tx *sql.Tx, existing *models.Item) error {
		if inner != nil {
			if err := inner(tx, existing); err != nil {
				return err
			}
		}
		changed, omitted := reservedCarryViolations(caller, existing.Fields)
		if len(changed) > 0 || len(omitted) > 0 {
			return &reservedFieldsCarryError{Changed: changed, Omitted: omitted}
		}
		return nil
	}
}

// reservedFullFieldsMessage renders the full-`fields` refusal. It names each
// key and gives the one remedy that works for every caller: carry the stored
// value, or send fields_patch, which leaves unnamed keys alone.
func reservedFullFieldsMessage(changed, omitted []string) string {
	var b strings.Builder
	// "cannot" is load-bearing beyond the prose: the stdio MCP classifier maps
	// CLI stderr to validation_failed by matching it (internal/mcp/errors.go).
	b.WriteString(`A full "fields" write cannot change stored system metadata:`)
	var parts []string
	if len(changed) == 1 {
		parts = append(parts, fmt.Sprintf(" %q differs from the stored value", changed[0]))
	} else if len(changed) > 1 {
		parts = append(parts, fmt.Sprintf(" %s differ from the stored values", quoteKeys(changed)))
	}
	if len(omitted) == 1 {
		parts = append(parts, fmt.Sprintf(" %q is stored on this item and missing from the write, which would delete it", omitted[0]))
	} else if len(omitted) > 1 {
		parts = append(parts, fmt.Sprintf(" %s are stored on this item and missing from the write, which would delete them", quoteKeys(omitted)))
	}
	b.WriteString(strings.Join(parts, ";"))
	b.WriteString(". Carry the stored value unchanged (read it with `pad item show <ref> --format json`), or send" +
		" fields_patch, which leaves the keys it does not name untouched.")
	all := append(append([]string(nil), changed...), omitted...)
	sort.Strings(all)
	for _, k := range all {
		if remedy := reservedFieldRemedy(k); remedy != "" {
			fmt.Fprintf(&b, " Maintain %s with %s.", k, remedy)
		}
	}
	return b.String()
}

// writeReservedFieldsCarryError answers a reservedFieldsCarryError as 400
// validation_error and reports whether err was one. It is a refusal of the
// request's shape against the stored row, so re-sending it unchanged fails the
// same way; a 409 would read as contention that clears on retry.
func writeReservedFieldsCarryError(w http.ResponseWriter, err error) bool {
	var carry *reservedFieldsCarryError
	if !errors.As(err, &carry) {
		return false
	}
	writeError(w, http.StatusBadRequest, "validation_error", carry.Error())
	return true
}
