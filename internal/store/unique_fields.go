package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// UniqueFieldConflictsQ returns the keys of fields declaring
// `unique_scope: workspace_collection` whose value in fields is already held by
// another live item in collectionID. excludeItemID is the item being written,
// so an item never conflicts with itself.
//
// It exists for the MIGRATE doors — single move, bulk move, the cross-workspace
// copy and its preflight (BUG-2367). Create and update have enforced the rule
// through the server's checkUniqueFields all along; a move or copy carried the
// value through MigrateFields and never asked, so a carried duplicate was
// stored outright on a field with no index behind it, and on invocation_slug,
// which has one (migration 054), single move answered 500 and bulk move
// reported the raw SQL error. The copy runs this inside its own transaction,
// which is why it is a Queryer function in `store` rather than a server helper:
// one implementation for four doors that sit in two packages.
//
// It returns KEYS only, never the conflicting item. The copy's existing 409
// stays generic for the reason recorded there — naming the holder would report
// on a row the caller may not be able to see — and a door that names nothing
// cannot become an existence oracle.
//
// Same value rule as checkUniqueFields: only a non-empty string is checked, and
// soft-deleted items are ignored, matching the partial index's
// `deleted_at IS NULL`. Returned keys are sorted, for stable messages.
func (s *Store) UniqueFieldConflictsQ(q Queryer, collectionID, excludeItemID string, schema []models.FieldDef, fields map[string]any) ([]string, error) {
	conflicts, err := s.uniqueFieldConflictsQ(q, collectionID, excludeItemID, schema, fields)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(conflicts))
	for _, c := range conflicts {
		out = append(out, c.key)
	}
	return out, nil
}

// uniqueConflict is one collision: the key, the value, and the live item that
// already holds it. The holder never leaves this package unfiltered — see
// DropCarriedUniqueCollisionsQ.
type uniqueConflict struct {
	key, value, holderID string
}

func (s *Store) uniqueFieldConflictsQ(q Queryer, collectionID, excludeItemID string, schema []models.FieldDef, fields map[string]any) ([]uniqueConflict, error) {
	var out []uniqueConflict
	for _, def := range schema {
		if def.UniqueScope != "workspace_collection" {
			continue
		}
		val, ok := fields[def.Key].(string)
		if !ok || val == "" {
			continue
		}
		// The key is interpolated into the JSON path, so it must be a key the
		// field-filter paths would also accept (search.go).
		if !validFieldKey.MatchString(def.Key) {
			return nil, fmt.Errorf("unique field check: unsupported field key %q", def.Key)
		}
		// BUG-3221: a holder is found by JSON type, identically on both
		// dialects.
		match, matchArgs := s.dialect.JSONFieldEquals("fields", def.Key, val)
		args := append(append([]any{collectionID}, matchArgs...), excludeItemID)
		var id string
		err := q.QueryRow(s.q(fmt.Sprintf(`
			SELECT id FROM items
			WHERE collection_id = ?
			  AND %s
			  AND id != ?
			  AND deleted_at IS NULL
			LIMIT 1`, match)), args...).Scan(&id)
		if err == nil {
			out = append(out, uniqueConflict{key: def.Key, value: val, holderID: id})
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("unique field check %q: %w", def.Key, err)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out, nil
}

// DropCarriedUniqueCollisionsQ removes from fields every CARRIED value that
// collides on a destination unique_scope field, and describes each removal
// (BUG-2367, lead ruling day 78).
//
// Provenance decides, as it does for relation referents (TASK-2878): a value
// the move or copy only CARRIED was asserted by nobody in this request, so it
// is dropped and reported rather than failing the whole write; a value the
// caller SUPPLIED (supplied(key) true) is left for the final check to refuse.
// Run BEFORE validation, so a required field left empty is refused as required
// — which is what routes it to the needs_value picker on the clients that
// have one.
//
// The report names the value, and names the item holding it ONLY when canSee
// says the caller may see that item. A drop already says the value is taken,
// so the value discloses nothing new; the holder's ref would, for an item
// behind a grant the caller lacks. A nil canSee means the caller sees
// everything (the same convention the relation passes use).
func (s *Store) DropCarriedUniqueCollisionsQ(q Queryer, canSee RelationVisibilityFunc, workspaceID string, coll *models.Collection, schema []models.FieldDef, fields map[string]any, supplied func(key string) bool) ([]models.NotUniqueDrop, error) {
	conflicts, err := s.uniqueFieldConflictsQ(q, coll.ID, "", schema, fields)
	if err != nil {
		return nil, err
	}
	var out []models.NotUniqueDrop
	for _, c := range conflicts {
		if supplied != nil && supplied(c.key) {
			continue
		}
		delete(fields, c.key)
		d := models.NotUniqueDrop{Key: c.key, Value: c.value}
		holder, herr := s.GetItemQ(q, c.holderID)
		if herr != nil && !errors.Is(herr, sql.ErrNoRows) {
			return nil, fmt.Errorf("unique field check %q: read holder: %w", c.key, herr)
		}
		if holder != nil {
			visible := true
			if canSee != nil {
				if visible, err = canSee(q, workspaceID, holder); err != nil {
					return nil, fmt.Errorf("unique field check %q: holder visibility: %w", c.key, err)
				}
			}
			if visible {
				d.Holder = holder.Ref
			}
		}
		if d.Holder != "" {
			d.Message = fmt.Sprintf("%s %q is taken by %s", c.key, c.value, d.Holder)
		} else {
			d.Message = NotUniqueAnonymousMessage(c.key, c.value, coll.Name)
		}
		out = append(out, d)
	}
	return out, nil
}

// NotUniqueAnonymousMessage is the not_unique sentence that names no holder.
// It is what a caller who may not see the holder is told, and it is the ONLY
// form written anywhere another reader will see it — an activity row is read
// by everyone who can see the moved item, not only by the actor whose
// visibility decided the response's sentence.
func NotUniqueAnonymousMessage(key, value, collectionName string) string {
	return fmt.Sprintf("%s %q is already used by another item in %s", key, value, collectionName)
}

// UniqueFieldConflictsMessage is the one sentence every migrate door refuses
// with, so a move, a bulk move, a copy and its preview say the same thing.
func UniqueFieldConflictsMessage(keys []string) string {
	if len(keys) == 1 {
		return fmt.Sprintf("field %q must be unique in the destination collection, and another item there already has this value; set a different value with a field override", keys[0])
	}
	return fmt.Sprintf("fields %q must be unique in the destination collection, and other items there already have these values; set different values with field overrides", keys)
}

// UniqueFieldConflictError is a copy refused because a destination
// unique_scope field already holds the value (BUG-2367). Typed so the HTTP
// layer answers the same 409 sentence the move doors do.
type UniqueFieldConflictError struct {
	Keys []string
}

func (e *UniqueFieldConflictError) Error() string {
	return UniqueFieldConflictsMessage(e.Keys)
}
