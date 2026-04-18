package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xarmian/pad/internal/collections"
	"github.com/xarmian/pad/internal/models"
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
		s.q(`SELECT id, schema, COALESCE(settings, '') FROM collections WHERE workspace_id = ?`),
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
		sets = append(sets, "settings = ?")
		args = append(args, *input.Settings)
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
func (s *Store) MigrateItemFieldValues(collectionID string, migrations []models.FieldMigration) (int64, error) {
	if len(migrations) == 0 {
		return 0, nil
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
			result, err := s.db.Exec(s.q(fmt.Sprintf(`
				UPDATE items
				SET fields = %s,
				    updated_at = ?
				WHERE collection_id = ?
				  AND %s = ?
				  AND deleted_at IS NULL
			`, jsonSet, jsonExtract)), newVal, ts, collectionID, oldVal)
			if err != nil {
				return totalAffected, fmt.Errorf("migrate field %s (%s → %s): %w", m.Field, oldVal, newVal, err)
			}
			n, _ := result.RowsAffected()
			totalAffected += n
		}
	}

	return totalAffected, nil
}

func (s *Store) SeedDefaultCollections(workspaceID string) error {
	return s.SeedCollectionsFromTemplate(workspaceID, "")
}

// SeedCollectionsFromTemplate seeds the workspace with collections from the
// named template. An empty template name materializes the default collections
// without any seed items or starter pack — this preserves backward
// compatibility for callers that don't opt into a template (including the
// server-side auto-upgrade path that re-runs on every boot). An explicit
// template name (including "startup") additionally seeds the template's
// SeedItems, Conventions, and Playbooks as items in the new workspace.
//
// Seeding is idempotent with respect to collections (existing collections are
// skipped) and with respect to seed items / conventions / playbooks (those
// are only created in collections that were freshly created during this
// call). That invariant is what lets the server safely re-run this function
// across every workspace on startup without duplicating items.
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

	// freshlyCreated tracks which collection slugs were created by THIS call.
	// Seed items only land in those so we don't duplicate items when the
	// function is re-run across pre-existing workspaces.
	freshlyCreated := make(map[string]bool)

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
		freshlyCreated[def.Slug] = true
	}

	// seedItem creates a seed item in the given collection, but only if that
	// collection was freshly created in this call. Silently no-ops otherwise.
	seedItem := func(collSlug, title, content, fields string) error {
		if !freshlyCreated[collSlug] {
			return nil
		}
		coll, err := s.GetCollectionBySlug(workspaceID, collSlug)
		if err != nil || coll == nil {
			return nil
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
}

// isReservedCollectionSlug checks whether a slug would collide with a
// workspace-level UI route.
func isReservedCollectionSlug(slug string) bool {
	return reservedCollectionSlugs[strings.ToLower(slug)]
}
