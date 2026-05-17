package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/collections"
	"github.com/PerpetualSoftware/pad/internal/models"
)

func (s *Store) CreateCollection(workspaceID string, input models.CollectionCreate) (*models.Collection, error) {
	id := newID()
	ts := now()

	schema := input.Schema
	if schema == "" {
		schema = `{"fields":[]}`
	}
	settings := input.Settings
	if settings == "" {
		settings = "{}"
	}
	icon := input.Icon
	description := input.Description

	prefix := input.Prefix
	if prefix == "" {
		prefix = collections.DerivePrefix(input.Name)
	}
	if prefix == "" {
		prefix = "ITEM"
	}

	baseSlug := input.Slug
	if baseSlug == "" {
		baseSlug = slugify(input.Name)
	}
	if baseSlug == "" {
		baseSlug = "collection"
	}
	// Avoid slugs that collide with workspace-level UI routes
	if isReservedCollectionSlug(baseSlug) {
		baseSlug = baseSlug + "-collection"
	}
	slug, err := s.uniqueSlug("collections", "workspace_id", workspaceID, baseSlug)
	if err != nil {
		return nil, fmt.Errorf("unique slug: %w", err)
	}

	_, err = s.db.Exec(s.q(`
		INSERT INTO collections (id, workspace_id, name, slug, prefix, icon, description, schema, settings, sort_order, is_default, is_system, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`), id, workspaceID, input.Name, slug, prefix, icon, description, schema, settings, 0, s.dialect.BoolToInt(input.IsDefault), s.dialect.BoolToInt(input.IsSystem), ts, ts)
	if err != nil {
		return nil, fmt.Errorf("insert collection: %w", err)
	}

	return s.GetCollection(id)
}

func (s *Store) GetCollection(id string) (*models.Collection, error) {
	var c models.Collection
	var createdAt, updatedAt string
	var deletedAt *string
	var isDefault bool

	err := s.db.QueryRow(s.q(`
		SELECT id, workspace_id, name, slug, prefix, icon, description, schema, settings, sort_order, is_default, is_system, created_at, updated_at, deleted_at
		FROM collections
		WHERE id = ? AND deleted_at IS NULL
	`), id).Scan(
		&c.ID, &c.WorkspaceID, &c.Name, &c.Slug, &c.Prefix, &c.Icon, &c.Description,
		&c.Schema, &c.Settings, &c.SortOrder, &isDefault, &c.IsSystem,
		&createdAt, &updatedAt, &deletedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get collection: %w", err)
	}

	c.IsDefault = isDefault
	c.CreatedAt = parseTime(createdAt)
	c.UpdatedAt = parseTime(updatedAt)
	c.DeletedAt = parseTimePtr(deletedAt)
	return &c, nil
}

// GetCollectionAnyState is GetCollection without the
// `deleted_at IS NULL` filter — used by the open-children guard
// (IDEA-1494 R3 P3) so a child still attached to a soft-deleted
// collection is evaluated against ITS collection's actual done-field
// schema instead of falling back to the default `status` terminal
// list (which would mis-classify children of custom-done-field
// collections as non-terminal and produce false blockers).
//
// Mirrors the inclusion rule baked into childrenDoneFiltersForParent /
// doneFiltersForWorkspace, both of which already include soft-deleted
// collections for exactly this reason.
func (s *Store) GetCollectionAnyState(id string) (*models.Collection, error) {
	var c models.Collection
	var createdAt, updatedAt string
	var deletedAt *string
	var isDefault bool

	err := s.db.QueryRow(s.q(`
		SELECT id, workspace_id, name, slug, prefix, icon, description, schema, settings, sort_order, is_default, is_system, created_at, updated_at, deleted_at
		FROM collections
		WHERE id = ?
	`), id).Scan(
		&c.ID, &c.WorkspaceID, &c.Name, &c.Slug, &c.Prefix, &c.Icon, &c.Description,
		&c.Schema, &c.Settings, &c.SortOrder, &isDefault, &c.IsSystem,
		&createdAt, &updatedAt, &deletedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get collection (any state): %w", err)
	}
	c.IsDefault = isDefault
	c.CreatedAt = parseTime(createdAt)
	c.UpdatedAt = parseTime(updatedAt)
	c.DeletedAt = parseTimePtr(deletedAt)
	return &c, nil
}

func (s *Store) GetCollectionBySlug(workspaceID, slug string) (*models.Collection, error) {
	var id string
	err := s.db.QueryRow(s.q(`
		SELECT id FROM collections
		WHERE workspace_id = ? AND slug = ? AND deleted_at IS NULL
	`), workspaceID, slug).Scan(&id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get collection by slug: %w", err)
	}
	return s.GetCollection(id)
}

// ListCollectionsMinimal returns collection rows populated with just the
// fields needed for done-detection context: ID, Schema, Settings.
// Skips the per-collection COUNT queries that ListCollections runs, which
// matters on hot paths that only need the schema + settings pair (e.g.
// handlers that build a ctxMap for isItemDone). Includes soft-deleted
// collections so items still attached to them can be evaluated.
func (s *Store) ListCollectionsMinimal(workspaceID string) ([]models.Collection, error) {
	rows, err := s.db.Query(
		s.q(`SELECT id, schema, settings FROM collections WHERE workspace_id = ?`),
		workspaceID,
	)
	if err != nil {
		return nil, fmt.Errorf("list collections minimal: %w", err)
	}
	defer rows.Close()
	var result []models.Collection
	for rows.Next() {
		var c models.Collection
		if err := rows.Scan(&c.ID, &c.Schema, &c.Settings); err != nil {
			return nil, fmt.Errorf("scan collection minimal: %w", err)
		}
		result = append(result, c)
	}
	return result, rows.Err()
}

func (s *Store) ListCollections(workspaceID string) ([]models.Collection, error) {
	rows, err := s.db.Query(s.q(`
		SELECT c.id, c.workspace_id, c.name, c.slug, c.prefix, c.icon, c.description,
		       c.schema, c.settings, c.sort_order, c.is_default, c.is_system, c.created_at, c.updated_at,
		       COUNT(i.id) as item_count
		FROM collections c
		LEFT JOIN items i ON i.collection_id = c.id AND i.deleted_at IS NULL
		WHERE c.workspace_id = ? AND c.deleted_at IS NULL
		GROUP BY c.id
		ORDER BY c.sort_order ASC, c.created_at ASC
	`), workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list collections: %w", err)
	}
	defer rows.Close()

	var result []models.Collection
	for rows.Next() {
		var c models.Collection
		var createdAt, updatedAt string
		var isDefault bool
		if err := rows.Scan(
			&c.ID, &c.WorkspaceID, &c.Name, &c.Slug, &c.Prefix, &c.Icon, &c.Description,
			&c.Schema, &c.Settings, &c.SortOrder, &isDefault, &c.IsSystem,
			&createdAt, &updatedAt, &c.ItemCount,
		); err != nil {
			return nil, err
		}
		c.IsDefault = isDefault
		c.CreatedAt = parseTime(createdAt)
		c.UpdatedAt = parseTime(updatedAt)
		result = append(result, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Compute active_item_count per collection using that collection's own
	// done-field + terminal options from its schema + settings (not the
	// global default list, and not hardcoded to `status`). See TASK-604:
	// collections whose board is grouped by e.g. `resolution` have
	// done-detection follow that field naturally.
	for idx := range result {
		c := &result[idx]
		var schema models.CollectionSchema
		if err := json.Unmarshal([]byte(c.Schema), &schema); err != nil {
			schema = models.CollectionSchema{}
		}
		var settings models.CollectionSettings
		if c.Settings != "" {
			_ = json.Unmarshal([]byte(c.Settings), &settings)
		}
		doneKey, termPlaceholders, termArgs := models.TerminalPlaceholdersForDoneField(schema, settings)
		jsonExtractDone := s.dialect.JSONExtractText("i.fields", doneKey)
		args := append([]any{c.ID}, termArgs...)
		err := s.db.QueryRow(s.q(fmt.Sprintf(`
			SELECT COUNT(*) FROM items i
			WHERE i.collection_id = ? AND i.deleted_at IS NULL
			AND LOWER(COALESCE(%s, '')) NOT IN (%s)
		`, jsonExtractDone, termPlaceholders)), args...).Scan(&c.ActiveItemCount)
		if err != nil {
			return nil, fmt.Errorf("count active items for collection %s: %w", c.Slug, err)
		}
	}

	return result, nil
}

func (s *Store) UpdateCollection(id string, input models.CollectionUpdate) (*models.Collection, error) {
	existing, err := s.GetCollection(id)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, nil
	}

	ts := now()
	sets := []string{"updated_at = ?"}
	args := []interface{}{ts}

	if input.Name != nil {
		sets = append(sets, "name = ?")
		args = append(args, *input.Name)
		// Update slug too
		baseSlug := slugify(*input.Name)
		if baseSlug == "" {
			baseSlug = "collection"
		}
		if isReservedCollectionSlug(baseSlug) {
			baseSlug = baseSlug + "-collection"
		}
		newSlug, err := s.uniqueSlugExcluding("collections", "workspace_id", existing.WorkspaceID, baseSlug, id)
		if err != nil {
			return nil, fmt.Errorf("unique slug: %w", err)
		}
		sets = append(sets, "slug = ?")
		args = append(args, newSlug)
	}
	if input.Prefix != nil {
		sets = append(sets, "prefix = ?")
		args = append(args, *input.Prefix)
	}
	if input.Icon != nil {
		sets = append(sets, "icon = ?")
		args = append(args, *input.Icon)
	}
	if input.Description != nil {
		sets = append(sets, "description = ?")
		args = append(args, *input.Description)
	}
	if input.Schema != nil {
		sets = append(sets, "schema = ?")
		args = append(args, *input.Schema)
	}
	if input.Settings != nil {
		// Normalize the empty-string sentinel to a valid JSON object before
		// writing. The NOT NULL DEFAULT '{}' constraint (IDEA-1484) only
		// fires when the UPDATE omits the column; explicit values are
		// written verbatim. Postgres rejects `""` at JSONB type-validation;
		// SQLite would silently store invalid JSON. Same boundary
		// normalization as ImportWorkspace.
		settings := *input.Settings
		if settings == "" {
			settings = "{}"
		}
		sets = append(sets, "settings = ?")
		args = append(args, settings)
	}
	if input.SortOrder != nil {
		sets = append(sets, "sort_order = ?")
		args = append(args, *input.SortOrder)
	}

	args = append(args, id)
	query := fmt.Sprintf("UPDATE collections SET %s WHERE id = ?", strings.Join(sets, ", "))
	_, err = s.db.Exec(s.q(query), args...)
	if err != nil {
		return nil, fmt.Errorf("update collection: %w", err)
	}

	return s.GetCollection(id)
}

func (s *Store) DeleteCollection(id string) error {
	// Check if it's a default collection
	var isDefault bool
	err := s.db.QueryRow(s.q("SELECT is_default FROM collections WHERE id = ? AND deleted_at IS NULL"), id).Scan(&isDefault)
	if err == sql.ErrNoRows {
		return sql.ErrNoRows
	}
	if err != nil {
		return fmt.Errorf("check collection: %w", err)
	}
	if isDefault {
		return fmt.Errorf("cannot delete default collection")
	}

	ts := now()
	result, err := s.db.Exec(s.q(`
		UPDATE collections SET deleted_at = ?, updated_at = ?
		WHERE id = ? AND deleted_at IS NULL
	`), ts, ts, id)
	if err != nil {
		return fmt.Errorf("delete collection: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// MigrateItemFieldValues bulk-updates items in a collection when select
// options are renamed. Each entry in renames maps old_value → new_value
// for the given field key.
//
// Each affected row gets its OWN fresh seq (no two share the same
// value) so /items-changes pagination is correct even when a
// rename touches more rows than the page limit. We do this with a
// per-row UPDATE loop: an earlier single-statement bulk UPDATE gave
// every row the same MAX(seq)+1, which broke the cursor contract —
// `limit=N` could cut through an equal-seq group, the cursor would
// advance to that shared seq, and the next `seq > cursor` poll
// would permanently miss the remaining rows in the group (Codex
// review of TASK-1354 round 2 [P1]).
//
// Trade-off: O(N) statements instead of O(1) for the bulk path.
// A schema-option rename is an admin one-off, so even a few
// thousand rows is acceptable (~1s/1000 rows on a warm SQLite
// connection). If a future use case demands a higher row budget,
// switch to an UPDATE..FROM with a ROW_NUMBER() CTE that assigns
// sequential per-row seqs in a single statement.
func (s *Store) MigrateItemFieldValues(collectionID string, migrations []models.FieldMigration) (int64, error) {
	if len(migrations) == 0 {
		return 0, nil
	}

	// Look up the workspace for advisory locking + scoping the seq
	// subquery. If the collection has vanished out from under the
	// caller we can short-circuit.
	var workspaceID string
	if err := s.db.QueryRow(s.q(`SELECT workspace_id FROM collections WHERE id = ?`), collectionID).Scan(&workspaceID); err != nil {
		if err == sql.ErrNoRows {
			return 0, nil
		}
		return 0, fmt.Errorf("lookup workspace for migrate: %w", err)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	if err := s.acquireWorkspaceSeqLock(tx, workspaceID); err != nil {
		return 0, err
	}

	ts := now()
	var totalAffected int64

	for _, m := range migrations {
		for oldVal, newVal := range m.RenameOptions {
			if oldVal == newVal {
				continue
			}
			jsonSet := s.dialect.JSONSet("fields", m.Field)
			jsonExtract := s.dialect.JSONExtractText("fields", m.Field)

			// Step 1: find all matching item IDs. SELECT inside the
			// same transaction as the subsequent UPDATEs, so we
			// observe a consistent snapshot of who needs to migrate.
			idRows, err := tx.Query(s.q(fmt.Sprintf(`
				SELECT id FROM items
				WHERE collection_id = ?
				  AND %s = ?
				  AND deleted_at IS NULL
			`, jsonExtract)), collectionID, oldVal)
			if err != nil {
				return totalAffected, fmt.Errorf("migrate field %s (%s → %s) list: %w", m.Field, oldVal, newVal, err)
			}
			var ids []string
			for idRows.Next() {
				var id string
				if err := idRows.Scan(&id); err != nil {
					idRows.Close()
					return totalAffected, fmt.Errorf("migrate field %s scan: %w", m.Field, err)
				}
				ids = append(ids, id)
			}
			if err := idRows.Err(); err != nil {
				idRows.Close()
				return totalAffected, fmt.Errorf("migrate field %s rows: %w", m.Field, err)
			}
			idRows.Close()

			// Step 2: update each row individually. Each UPDATE is
			// a separate statement, so MAX(seq) advances between
			// them inside the transaction — every row ends up with
			// a unique sequential seq.
			updateSQL := s.q(fmt.Sprintf(`
				UPDATE items
				SET fields = %s,
				    updated_at = ?,
				    seq = `+nextWorkspaceSeqSubquery+`
				WHERE id = ?
			`, jsonSet))
			for _, id := range ids {
				result, err := tx.Exec(updateSQL, newVal, ts, workspaceID, id)
				if err != nil {
					return totalAffected, fmt.Errorf("migrate field %s (%s → %s) row %s: %w", m.Field, oldVal, newVal, id, err)
				}
				n, _ := result.RowsAffected()
				totalAffected += n
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return totalAffected, err
	}
	return totalAffected, nil
}

// SeedDefaultCollections rescues a workspace that ended up with zero
// collections by seeding it with the standard Software-shape Defaults().
// It is intentionally a no-op for workspaces that already have any
// collections — those have an established shape (from a template or
// user edits) that the rescue must not clobber.
//
// No longer called automatically at server startup (removed in
// IDEA-1479); preserved as a building block for any future explicit
// rescue command or migration that wants this behavior.
func (s *Store) SeedDefaultCollections(workspaceID string) error {
	// Use a direct COUNT query rather than ListCollectionsMinimal: the
	// rescue gate only needs to know whether ANY collection exists, and
	// the minimal lister's SELECT touches JSON/JSONB columns whose
	// COALESCE expression doesn't round-trip cleanly on Postgres
	// (BUG triggered in the IDEA-1479 PR's first Postgres CI run).
	// COUNT(*) sidesteps that path entirely and is also cheaper.
	var existing int
	if err := s.db.QueryRow(
		s.q(`SELECT COUNT(*) FROM collections WHERE workspace_id = ?`),
		workspaceID,
	).Scan(&existing); err != nil {
		return fmt.Errorf("check existing collections for rescue seed: %w", err)
	}
	if existing > 0 {
		// Workspace already has collections — its shape was set by a
		// template (default, blank, hiring, etc.) or by manual user
		// edits. Either way, the rescue path doesn't apply.
		return nil
	}
	return s.SeedCollectionsFromTemplate(workspaceID, "")
}

// SeedCollectionsFromTemplate seeds the workspace with collections from the
// named template. An empty template name materializes the default collections
// without any seed items or starter pack — this preserves backward
// compatibility for callers that don't opt into a template. An explicit
// template name (including "startup") additionally seeds the template's
// SeedItems, Conventions, and Playbooks as items in the new workspace.
//
// Seeding is idempotent per-item by title: existing collections are skipped,
// and existing items (matched by title within their target collection) are
// skipped. That design lets the server's startup auto-upgrade re-run safely
// AND lets a partial init (e.g. DB error after some items were seeded) be
// recovered by simply retrying — the retry fills in missing items instead
// of stopping at the first "collection already exists" signal.
func (s *Store) SeedCollectionsFromTemplate(workspaceID string, templateName string) error {
	var defs []collections.DefaultCollection
	var seedItems []collections.SeedItem
	var seedConventions []collections.SeedConvention
	var seedPlaybooks []collections.SeedPlaybook

	if templateName == "" {
		defs = collections.Defaults()
		// Empty template = backward-compatible, no starter pack.
	} else {
		tmpl := collections.GetTemplate(templateName)
		if tmpl == nil {
			return fmt.Errorf("unknown workspace template: %s", templateName)
		}
		defs = tmpl.Collections
		seedItems = tmpl.SeedItems
		seedConventions = tmpl.Conventions
		seedPlaybooks = tmpl.Playbooks
	}

	for _, def := range defs {
		existing, err := s.GetCollectionBySlug(workspaceID, def.Slug)
		if err != nil {
			return fmt.Errorf("check existing collection %s: %w", def.Slug, err)
		}
		if existing != nil {
			continue
		}

		schemaJSON, err := json.Marshal(def.Schema)
		if err != nil {
			return fmt.Errorf("marshal schema for %s: %w", def.Slug, err)
		}
		settingsJSON, err := json.Marshal(def.Settings)
		if err != nil {
			return fmt.Errorf("marshal settings for %s: %w", def.Slug, err)
		}

		_, err = s.CreateCollection(workspaceID, models.CollectionCreate{
			Name:        def.Name,
			Slug:        def.Slug,
			Prefix:      def.Prefix,
			Icon:        def.Icon,
			Description: def.Description,
			Schema:      string(schemaJSON),
			Settings:    string(settingsJSON),
			IsDefault:   true,
			IsSystem:    def.IsSystem,
		})
		if err != nil {
			return fmt.Errorf("create default collection %s: %w", def.Slug, err)
		}
	}

	// existingTitles caches the set of item titles already present in a
	// collection so repeated seed calls against the same collection don't
	// re-query. Lazily populated on first use per slug.
	existingTitles := make(map[string]map[string]bool)

	// seedItem inserts a seed item if no item with the same title already
	// exists in the target collection. Missing target collections (a
	// template-authoring mistake) are silently skipped; real DB errors are
	// propagated so callers can detect partial init failures and retry.
	seedItem := func(collSlug, title, content, fields string) error {
		coll, err := s.GetCollectionBySlug(workspaceID, collSlug)
		if err != nil {
			return fmt.Errorf("lookup %s collection for seeding %q: %w", collSlug, title, err)
		}
		if coll == nil {
			return nil
		}

		titles, ok := existingTitles[collSlug]
		if !ok {
			items, err := s.ListItems(workspaceID, models.ItemListParams{CollectionSlug: collSlug})
			if err != nil {
				return fmt.Errorf("list existing items in %s: %w", collSlug, err)
			}
			titles = make(map[string]bool, len(items))
			for _, it := range items {
				titles[it.Title] = true
			}
			existingTitles[collSlug] = titles
		}
		if titles[title] {
			return nil // already seeded (idempotent + retry-safe)
		}

		_, err = s.CreateItem(workspaceID, coll.ID, models.ItemCreate{
			Title:     title,
			Content:   content,
			Fields:    fields,
			CreatedBy: "system",
			Source:    "template",
		})
		if err != nil {
			return fmt.Errorf("seed item %q in %s: %w", title, collSlug, err)
		}
		titles[title] = true
		return nil
	}

	// Sample items
	for _, item := range seedItems {
		if err := seedItem(item.CollectionSlug, item.Title, item.Content, item.Fields); err != nil {
			return err
		}
	}
	// Starter conventions
	for _, conv := range seedConventions {
		if err := seedItem("conventions", conv.Title, conv.Content, conv.Fields); err != nil {
			return err
		}
	}
	// Starter playbooks
	for _, pb := range seedPlaybooks {
		if err := seedItem("playbooks", pb.Title, pb.Content, pb.Fields); err != nil {
			return err
		}
	}

	// Universal onboard playbook (PLAN-1496 / TASK-1500). Auto-seeded
	// into every workspace created via a real template — including
	// `blank` (where it's the only payload) and the opinionated
	// software/people templates (where it complements the seeded
	// starter pack with the "/pad onboard" entry point for further
	// adaptation).
	//
	// Skipped when templateName == "" — that's the explicit
	// backward-compat escape hatch for tests and direct API callers
	// who want a bare workspace with default system collections and
	// NO seeded content. cmd/pad/init.go ALWAYS supplies a non-empty
	// template (interactive picker or defaultTemplateName), so real
	// user-facing workspace creation always lands here.
	//
	// Seeded last so any future template that ships its own
	// "Onboard a workspace"-titled playbook can take precedence —
	// the seedItem helper is idempotent by title inside a collection.
	if templateName != "" {
		onboardSeed := collections.OnboardSeedPlaybook()
		if err := seedItem("playbooks", onboardSeed.Title, onboardSeed.Content, onboardSeed.Fields); err != nil {
			return err
		}
	}

	return nil
}

// reservedCollectionSlugs are workspace-level UI route paths that must not
// be used as collection slugs, to avoid routing collisions.
var reservedCollectionSlugs = map[string]bool{
	"settings": true,
	"activity": true,
	"roles":    true,
	"starred":  true,
	"library":  true,
	"new":      true,
	// "ref" is reserved for the cross-workspace wiki-link resolver route
	// (IDEA-1492): GET /{username}/{workspace}/ref/{REF} → 302 to the
	// canonical item URL. A collection slug of "ref" would intercept every
	// item URL under that collection (the resolver's `{ref}` segment can't
	// match an arbitrary slug shape), so we forbid it at create time
	// rather than tolerate silent 404s after the fact.
	"ref": true,
}

// isReservedCollectionSlug checks whether a slug would collide with a
// workspace-level UI route.
func isReservedCollectionSlug(slug string) bool {
	return reservedCollectionSlugs[strings.ToLower(slug)]
}
