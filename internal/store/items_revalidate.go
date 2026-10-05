package store

import (
	"database/sql"
	"errors"
	"fmt"
	"reflect"

	"github.com/PerpetualSoftware/pad/internal/items"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3407: an item write is validated against its collection's schema in the
// handler, BEFORE the store takes the workspace seq lock. A schema change that
// commits in that window (an owner's UpdateCollection, an app upgrade's
// additive field) used to let the write store a value validated against the
// OLD schema: `color: "blue"` accepted as an undeclared key, then stored under
// a select that now allows only "red".
//
// Every schema writer takes the same workspace seq lock (UpdateCollection
// whenever the schema moves, for its relation reindex; the app upgrade in its
// provisioning transaction), so the schema read under the item write's own
// seq lock is the current one. The handler passes the exact schema bytes it
// validated against; the store compares them under the lock and re-validates
// only when they differ. An always-on re-check was tried first and refused:
// the store is not a validation layer, and internal flows write below the
// schema on purpose (lead ruling, day 86).

// revalidateIfSchemaMovedTx is the optimistic schema compare (lead ruling,
// day 86). validated is the schema, as the exact bytes read from the
// collection row, that the caller validated against; nil means the caller
// made no such claim and nothing is checked, which keeps the store's contract
// for internal flows that write below the schema on purpose. Otherwise the
// schema is read again here, under the workspace seq lock the caller holds,
// and compared byte for byte: unchanged, nothing to do; moved, the keys this
// write sets are re-validated against the new schema and a failure refuses
// the write before anything is written.
func (s *Store) revalidateIfSchemaMovedTx(tx *sql.Tx, collectionID string, validated *string, set map[string]any) error {
	if validated == nil || len(set) == 0 {
		return nil
	}
	var raw string
	err := tx.QueryRow(s.q(`SELECT schema FROM collections WHERE id = ?`), collectionID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // a vanished collection is refused elsewhere, not here
	}
	if err != nil {
		return fmt.Errorf("re-read collection schema under lock: %w", err)
	}
	if raw == *validated {
		return nil
	}
	var schema models.CollectionSchema
	// UnmarshalItemFieldSchema, not json.Unmarshal: it drops reserved
	// declarations, so system metadata is judged by no schema, as on every
	// handler path.
	if err := models.UnmarshalItemFieldSchema([]byte(raw), &schema); err != nil {
		return fmt.Errorf("decode collection schema under lock: %w", err)
	}
	// ValidatePartialFields normalizes empty relation lists in place.
	cp := make(map[string]any, len(set))
	for k, v := range set {
		cp[k] = v
	}
	if err := items.ValidatePartialFields(cp, schema); err != nil {
		return invalidf("the collection's schema changed while this write was in flight: %s", err.Error())
	}
	return nil
}

// decodeFieldsBlob decodes a stored or proposed fields blob for comparison.
func decodeFieldsBlob(blob string) (map[string]any, error) {
	m := map[string]any{}
	if blob == "" || blob == "{}" {
		return m, nil
	}
	if err := models.DecodeJSONKeepingNumbers([]byte(blob), &m); err != nil {
		return nil, err
	}
	return m, nil
}

// changedFieldKeys is the subset of next whose decoded value differs from
// prev: the keys a full `fields` write SETS, as opposed to carries. A key
// next removes is a deletion, marked with a nil value.
func changedFieldKeys(prevBlob, nextBlob string) (map[string]any, error) {
	prev, err := decodeFieldsBlob(prevBlob)
	if err != nil {
		return nil, err
	}
	next, err := decodeFieldsBlob(nextBlob)
	if err != nil {
		return nil, err
	}
	set := map[string]any{}
	for k, v := range next {
		if old, ok := prev[k]; !ok || !reflect.DeepEqual(old, v) {
			set[k] = v
		}
	}
	for k := range prev {
		if _, ok := next[k]; !ok {
			set[k] = nil
		}
	}
	return set, nil
}
