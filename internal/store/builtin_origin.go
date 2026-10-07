package store

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/PerpetualSoftware/pad/internal/collections"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// validImportedBuiltinOrigin is Validate plus the check Validate leaves to
// collections: seed text a bundle carries must hash to the seed hash it
// carries, or a 3-way preview would describe a version that never existed.
func validImportedBuiltinOrigin(o models.BuiltinOrigin) error {
	if err := o.Validate(); err != nil {
		return err
	}
	if o.SeedContent == "" && o.SeedFields == "" {
		return nil
	}
	h, err := collections.BuiltinEntry{Key: o.Key, Content: o.SeedContent, Fields: o.SeedFields}.HashErr()
	if err != nil {
		return err
	}
	if h != o.SeedHash {
		return fmt.Errorf("built-in origin seed text for %q does not hash to its seed_hash", o.Key)
	}
	return nil
}

// insertBuiltinOriginTx records the built-in an item was made from
// (TASK-3462), inside the transaction that created the item.
func (s *Store) insertBuiltinOriginTx(tx *sql.Tx, itemID string, o models.BuiltinOrigin, ts string) error {
	if err := o.Validate(); err != nil {
		return err
	}
	_, err := tx.Exec(s.q(`
		INSERT INTO item_builtin_origin (item_id, builtin_key, seed_hash, seed_content, seed_fields, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`), itemID, o.Key, nullIfEmpty(o.SeedHash), nullIfEmpty(o.SeedContent), nullIfEmpty(o.SeedFields), ts)
	if err != nil {
		return fmt.Errorf("record built-in origin: %w", err)
	}
	return nil
}

// GetItemBuiltinOrigin returns the built-in an item was made from, or nil
// when it was not made from one.
func (s *Store) GetItemBuiltinOrigin(itemID string) (*models.BuiltinOrigin, error) {
	var o models.BuiltinOrigin
	var hash, content, fields sql.NullString
	err := s.db.QueryRow(s.q(`
		SELECT builtin_key, seed_hash, seed_content, seed_fields FROM item_builtin_origin WHERE item_id = ?
	`), itemID).Scan(&o.Key, &hash, &content, &fields)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get built-in origin: %w", err)
	}
	o.SeedHash, o.SeedContent, o.SeedFields = hash.String, content.String, fields.String
	return &o, nil
}

// WorkspaceBuiltinOrigins returns the origin of every live item in the
// workspace that has one, keyed by item id: one query, a join, however many
// items the workspace holds (the library page reads every row of it). The
// seed text is left out; GetItemBuiltinOrigin reads it for one item.
func (s *Store) WorkspaceBuiltinOrigins(workspaceID string) (map[string]models.BuiltinOrigin, error) {
	rows, err := s.db.Query(s.q(`
		SELECT o.item_id, o.builtin_key, o.seed_hash
		FROM item_builtin_origin o
		JOIN items i ON i.id = o.item_id
		WHERE i.workspace_id = ? AND i.deleted_at IS NULL
	`), workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list built-in origins: %w", err)
	}
	defer rows.Close()
	out := map[string]models.BuiltinOrigin{}
	for rows.Next() {
		var id string
		var o models.BuiltinOrigin
		var hash sql.NullString
		if err := rows.Scan(&id, &o.Key, &hash); err != nil {
			return nil, fmt.Errorf("scan built-in origin: %w", err)
		}
		o.SeedHash = hash.String
		out[id] = o
	}
	return out, rows.Err()
}

// BuiltinItem is one live item with a built-in origin, with what the
// built-in endpoints need to derive its state (TASK-3462).
type BuiltinItem struct {
	ItemID         string
	Ref            string
	Slug           string
	Title          string
	Content        string
	Fields         string
	CollectionID   string
	CollectionSlug string
	Origin         models.BuiltinOrigin
}

// WorkspaceBuiltinItems returns every live item in the workspace that has a
// built-in origin, in a live collection, with its body and fields and its
// seed text (the state needs it: see collections.BuiltinStateOf): ONE query,
// a join, however many such items there are.
func (s *Store) WorkspaceBuiltinItems(workspaceID string) ([]BuiltinItem, error) {
	rows, err := s.db.Query(s.q(`
		SELECT i.id, i.slug, i.title, i.content, i.fields, i.item_number, i.collection_id, c.slug, c.prefix,
		       o.builtin_key, o.seed_hash, o.seed_content, o.seed_fields
		FROM item_builtin_origin o
		JOIN items i ON i.id = o.item_id
		JOIN collections c ON c.id = i.collection_id AND c.deleted_at IS NULL
		WHERE i.workspace_id = ? AND i.deleted_at IS NULL
		ORDER BY i.item_number
	`), workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list built-in items: %w", err)
	}
	defer rows.Close()
	var out []BuiltinItem
	for rows.Next() {
		var b BuiltinItem
		var itemNumber *int
		var prefix string
		var hash, seedContent, seedFields sql.NullString
		if err := rows.Scan(&b.ItemID, &b.Slug, &b.Title, &b.Content, &b.Fields, &itemNumber, &b.CollectionID,
			&b.CollectionSlug, &prefix, &b.Origin.Key, &hash, &seedContent, &seedFields); err != nil {
			return nil, fmt.Errorf("scan built-in item: %w", err)
		}
		if prefix != "" && itemNumber != nil {
			b.Ref = fmt.Sprintf("%s-%d", prefix, *itemNumber)
		}
		b.Origin.SeedHash, b.Origin.SeedContent, b.Origin.SeedFields = hash.String, seedContent.String, seedFields.String
		out = append(out, b)
	}
	return out, rows.Err()
}

// SetItemBuiltinSeed records that an item has been given a built-in's text
// at this version (TASK-3462): after an update from the library. The key is
// unchanged; an item with no origin row is left alone.
func (s *Store) SetItemBuiltinSeed(itemID string, o models.BuiltinOrigin) error {
	if err := o.Validate(); err != nil {
		return err
	}
	_, err := s.db.Exec(s.q(`
		UPDATE item_builtin_origin SET seed_hash = ?, seed_content = ?, seed_fields = ?
		WHERE item_id = ? AND builtin_key = ?
	`), nullIfEmpty(o.SeedHash), nullIfEmpty(o.SeedContent), nullIfEmpty(o.SeedFields), itemID, o.Key)
	if err != nil {
		return fmt.Errorf("record built-in seed: %w", err)
	}
	return nil
}
