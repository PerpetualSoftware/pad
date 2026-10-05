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
// provisioning transaction), so the schema read HERE, under the item write's
// own seq lock, is the current one. The write's SET keys are re-validated
// against it with the partial validator: type and option validity of the keys
// this write sets, undeclared keys accepted as on every other path, a
// required key not deletable. Carried values are never re-judged (the
// TASK-2878 rule), and relation resolution and unique checks stay where they
// are: neither is the reported race.

// revalidateSetFieldsTx validates set against collectionID's schema as it
// stands in tx. It must be called with the workspace seq lock held. A failure
// is the ordinary validation error; nothing has been written.
func (s *Store) revalidateSetFieldsTx(tx *sql.Tx, collectionID string, set map[string]any) error {
	if len(set) == 0 {
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
	var schema models.CollectionSchema
	if err := models.UnmarshalItemFieldSchema([]byte(raw), &schema); err != nil {
		return fmt.Errorf("decode collection schema under lock: %w", err)
	}
	// ValidatePartialFields normalizes empty relation lists in place.
	cp := make(map[string]any, len(set))
	for k, v := range set {
		cp[k] = v
	}
	if err := items.ValidatePartialFields(cp, schema); err != nil {
		return invalidf("%s", err.Error())
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
