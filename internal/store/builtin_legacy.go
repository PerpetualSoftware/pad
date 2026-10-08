package store

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/PerpetualSoftware/pad/internal/collections"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// legacyBuiltinAdoptionSetting records that the one-time adoption ran.
const legacyBuiltinAdoptionSetting = "builtin_legacy_adoption"

// LegacyBuiltinAdoption is what AdoptLegacyBuiltins did.
type LegacyBuiltinAdoption struct {
	// Skipped: it had already run on this instance.
	Skipped bool
	// Considered: live items in convention/playbook collections with no
	// origin and created before origins were recorded.
	Considered int
	// Adopted: of those, the ones given an origin.
	Adopted int
}

// AdoptLegacyBuiltins gives items made before built-in origins were recorded
// (TASK-3462 U4) the origin their exact title names, with no seed version, so
// they read current or unknown_origin and the 2-way preview applies (Dave's
// ruling: no historical hashes).
//
// ONCE per instance, recorded in platform_settings, and only for items created
// before the origin migration was applied. After that, an item with no origin
// is one that deliberately has none (a copy, an import of an older bundle, a
// hand-made item with a library title), and adopting it by title would offer
// it someone else's text.
func (s *Store) AdoptLegacyBuiltins() (LegacyBuiltinAdoption, error) {
	var res LegacyBuiltinAdoption
	if v, err := s.GetPlatformSetting(legacyBuiltinAdoptionSetting); err != nil {
		return res, fmt.Errorf("read adoption marker: %w", err)
	} else if v == "done" {
		res.Skipped = true
		return res, nil
	}

	var appliedAt string
	if err := s.db.QueryRow(s.q(`SELECT applied_at FROM schema_migrations WHERE version LIKE ? ORDER BY applied_at LIMIT 1`),
		"%item_builtin_origin.sql").Scan(&appliedAt); err != nil {
		return res, fmt.Errorf("read the origin migration's time: %w", err)
	}
	cutoff, err := time.Parse(time.RFC3339, appliedAt)
	if err != nil {
		return res, fmt.Errorf("parse the origin migration's time %q: %w", appliedAt, err)
	}

	type candidate struct {
		id, title, content, fields, createdAt, traits string
	}
	rows, err := s.db.Query(s.q(`
		SELECT i.id, i.title, i.content, i.fields, i.created_at, c.traits
		FROM items i
		JOIN collections c ON c.id = i.collection_id AND c.deleted_at IS NULL
		LEFT JOIN item_builtin_origin o ON o.item_id = i.id
		WHERE o.item_id IS NULL AND i.deleted_at IS NULL
	`))
	if err != nil {
		return res, fmt.Errorf("list items without an origin: %w", err)
	}
	var cands []candidate
	for rows.Next() {
		var c candidate
		var traits *string
		if err := rows.Scan(&c.id, &c.title, &c.content, &c.fields, &c.createdAt, &traits); err != nil {
			rows.Close()
			return res, fmt.Errorf("scan item: %w", err)
		}
		if traits != nil {
			c.traits = *traits
		}
		cands = append(cands, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}

	entries := collections.BuiltinEntries()
	tx, err := s.db.Begin()
	if err != nil {
		return res, err
	}
	defer tx.Rollback() //nolint:errcheck
	ts := now()
	for _, c := range cands {
		t, err := models.ParseCollectionTraits(c.traits)
		if err != nil || t.ArtifactKind == nil {
			continue
		}
		kind := t.ArtifactKind.Kind
		if kind != collections.BuiltinConvention && kind != collections.BuiltinPlaybook {
			continue
		}
		created, err := time.Parse(time.RFC3339, c.createdAt)
		if err != nil || !created.Before(cutoff) {
			continue
		}
		res.Considered++
		var f struct {
			InvocationSlug string `json:"invocation_slug"`
		}
		_ = json.Unmarshal([]byte(c.fields), &f)
		key := collections.MatchLegacyBuiltin(entries, kind, c.title, f.InvocationSlug, c.content)
		if key == "" {
			continue
		}
		if err := s.insertBuiltinOriginTx(tx, c.id, models.BuiltinOrigin{Key: key}, ts); err != nil {
			return res, err
		}
		res.Adopted++
	}
	if err := tx.Commit(); err != nil {
		return res, err
	}
	if err := s.SetPlatformSetting(legacyBuiltinAdoptionSetting, "done"); err != nil {
		return res, fmt.Errorf("record adoption marker: %w", err)
	}
	return res, nil
}
