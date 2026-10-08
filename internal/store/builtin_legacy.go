package store

import (
	"encoding/json"
	"fmt"
	"strings"
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
// before the origin migration was applied, in a workspace that also predates
// it (an import keeps its items' old timestamps). After that, an item with no origin
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

	// The convention and playbook collections first, so the item read below
	// touches only their items (codex r1: reading every originless item's body
	// on the instance cost startup time and memory in proportion to it).
	kindOf := map[string]string{}
	crows, err := s.db.Query(s.q(`SELECT id, traits FROM collections WHERE deleted_at IS NULL`))
	if err != nil {
		return res, fmt.Errorf("list collections: %w", err)
	}
	for crows.Next() {
		var id string
		var traits *string
		if err := crows.Scan(&id, &traits); err != nil {
			crows.Close()
			return res, fmt.Errorf("scan collection: %w", err)
		}
		if traits == nil {
			continue
		}
		t, err := models.ParseCollectionTraits(*traits)
		if err != nil || t.ArtifactKind == nil {
			continue
		}
		if k := t.ArtifactKind.Kind; k == collections.BuiltinConvention || k == collections.BuiltinPlaybook {
			kindOf[id] = k
		}
	}
	crows.Close()
	if err := crows.Err(); err != nil {
		return res, err
	}

	type candidate struct {
		id, title, content, fields, collectionID string
	}
	var cands []candidate
	ids := make([]string, 0, len(kindOf))
	for id := range kindOf {
		ids = append(ids, id)
	}
	const chunk = 400
	for start := 0; start < len(ids); start += chunk {
		end := start + chunk
		if end > len(ids) {
			end = len(ids)
		}
		part := ids[start:end]
		args := make([]any, 0, len(part))
		ph := make([]string, len(part))
		for i, id := range part {
			ph[i] = "?"
			args = append(args, id)
		}
		rows, err := s.db.Query(s.q(`
			SELECT i.id, i.title, i.content, i.fields, i.collection_id, i.created_at, w.created_at
			FROM items i
			JOIN workspaces w ON w.id = i.workspace_id
			LEFT JOIN item_builtin_origin o ON o.item_id = i.id
			WHERE o.item_id IS NULL AND i.deleted_at IS NULL
			  AND i.collection_id IN (`+strings.Join(ph, ",")+`)
		`), args...)
		if err != nil {
			return res, fmt.Errorf("list legacy items: %w", err)
		}
		for rows.Next() {
			var c candidate
			var createdAt, wsCreatedAt string
			if err := rows.Scan(&c.id, &c.title, &c.content, &c.fields, &c.collectionID, &createdAt, &wsCreatedAt); err != nil {
				rows.Close()
				return res, fmt.Errorf("scan item: %w", err)
			}
			// AT OR BEFORE the migration's second (codex r1): timestamps are
			// second-precision, so an item made in that very second is still
			// a legacy item, and missing it now would miss it for good once the
			// marker is written. Parsed, not compared as text, because an
			// imported item keeps the timestamp format it was exported with;
			// one that does not parse is left alone.
			created, err := time.Parse(time.RFC3339, createdAt)
			if err != nil || created.After(cutoff) {
				continue
			}
			// The WORKSPACE must predate the cutoff too (codex r2): import
			// keeps each item's created_at from the bundle, so an old bundle
			// imported after the migration but before this pass first ran
			// would read as legacy. Every import door (workspace import,
			// bundle import, db migrate-to-pg) mints a new workspace, and none
			// writes into an existing one, so the workspace's created_at is
			// when its items arrived.
			wsCreated, err := time.Parse(time.RFC3339, wsCreatedAt)
			if err != nil || wsCreated.After(cutoff) {
				continue
			}
			cands = append(cands, c)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return res, err
		}
	}

	entries := collections.BuiltinEntries()
	tx, err := s.db.Begin()
	if err != nil {
		return res, err
	}
	defer tx.Rollback() //nolint:errcheck
	ts := now()
	for _, c := range cands {
		kind := kindOf[c.collectionID]
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
