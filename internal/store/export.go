package store

import (
	"fmt"
	"strings"
	"time"

	"github.com/xarmian/pad/internal/models"
)

// ExportWorkspace exports all data for a workspace into a portable format.
func (s *Store) ExportWorkspace(slug string) (*models.WorkspaceExport, error) {
	ws, err := s.GetWorkspaceBySlug(slug)
	if err != nil {
		return nil, fmt.Errorf("workspace lookup: %w", err)
	}
	if ws == nil {
		return nil, fmt.Errorf("workspace not found: %s", slug)
	}

	export := &models.WorkspaceExport{
		Version:    1,
		ExportedAt: time.Now().UTC().Format(time.RFC3339),
		Workspace: models.WorkspaceExportMeta{
			Name:        ws.Name,
			Slug:        ws.Slug,
			Description: ws.Description,
			Settings:    ws.Settings,
		},
	}

	// Collections
	rows, err := s.db.Query(s.q(`
		SELECT id, name, slug, icon, description, schema, settings, prefix, sort_order, is_default, created_at, updated_at
		FROM collections WHERE workspace_id = ? AND deleted_at IS NULL
		ORDER BY sort_order, name`), ws.ID)
	if err != nil {
		return nil, fmt.Errorf("export collections: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var c models.CollectionExport
		var isDefault bool
		if err := rows.Scan(&c.ID, &c.Name, &c.Slug, &c.Icon, &c.Description, &c.Schema, &c.Settings, &c.Prefix, &c.SortOrder, &isDefault, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan collection: %w", err)
		}
		c.IsDefault = isDefault
		export.Collections = append(export.Collections, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Items
	itemRows, err := s.db.Query(s.q(`
		SELECT id, collection_id, title, slug, content, fields, tags, pinned, sort_order,
		       COALESCE(parent_id, ''), created_by, last_modified_by, source, COALESCE(item_number, 0), created_at, updated_at
		FROM items WHERE workspace_id = ? AND deleted_at IS NULL
		ORDER BY created_at, id`), ws.ID)
	if err != nil {
		return nil, fmt.Errorf("export items: %w", err)
	}
	defer itemRows.Close()
	for itemRows.Next() {
		var it models.ItemExport
		var pinned bool
		if err := itemRows.Scan(&it.ID, &it.CollectionID, &it.Title, &it.Slug, &it.Content, &it.Fields, &it.Tags, &pinned, &it.SortOrder, &it.ParentID, &it.CreatedBy, &it.LastModifiedBy, &it.Source, &it.ItemNumber, &it.CreatedAt, &it.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan item: %w", err)
		}
		it.Pinned = pinned
		export.Items = append(export.Items, it)
	}
	if err := itemRows.Err(); err != nil {
		return nil, err
	}

	// Comments
	commentRows, err := s.db.Query(s.q(`
		SELECT c.id, c.item_id, c.author, c.body, c.created_by, c.source, c.created_at, c.updated_at
		FROM comments c
		JOIN items i ON c.item_id = i.id
		WHERE c.workspace_id = ? AND i.deleted_at IS NULL
		ORDER BY c.created_at`), ws.ID)
	if err != nil {
		return nil, fmt.Errorf("export comments: %w", err)
	}
	defer commentRows.Close()
	for commentRows.Next() {
		var cm models.CommentExport
		if err := commentRows.Scan(&cm.ID, &cm.ItemID, &cm.Author, &cm.Body, &cm.CreatedBy, &cm.Source, &cm.CreatedAt, &cm.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan comment: %w", err)
		}
		export.Comments = append(export.Comments, cm)
	}
	if err := commentRows.Err(); err != nil {
		return nil, err
	}

	// Item links
	linkRows, err := s.db.Query(s.q(`
		SELECT id, source_id, target_id, link_type, created_by, created_at
		FROM item_links WHERE workspace_id = ?
		ORDER BY created_at`), ws.ID)
	if err != nil {
		return nil, fmt.Errorf("export item links: %w", err)
	}
	defer linkRows.Close()
	for linkRows.Next() {
		var lk models.ItemLinkExport
		if err := linkRows.Scan(&lk.ID, &lk.SourceID, &lk.TargetID, &lk.LinkType, &lk.CreatedBy, &lk.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan item link: %w", err)
		}
		export.ItemLinks = append(export.ItemLinks, lk)
	}
	if err := linkRows.Err(); err != nil {
		return nil, err
	}

	// Item versions
	versionRows, err := s.db.Query(s.q(`
		SELECT v.id, v.item_id, v.content, v.change_summary, v.created_by, v.source, v.is_diff, v.created_at
		FROM item_versions v
		JOIN items i ON v.item_id = i.id
		WHERE i.workspace_id = ? AND i.deleted_at IS NULL
		ORDER BY v.created_at`), ws.ID)
	if err != nil {
		return nil, fmt.Errorf("export item versions: %w", err)
	}
	defer versionRows.Close()
	for versionRows.Next() {
		var ver models.ItemVersionExport
		var isDiff bool
		if err := versionRows.Scan(&ver.ID, &ver.ItemID, &ver.Content, &ver.ChangeSummary, &ver.CreatedBy, &ver.Source, &isDiff, &ver.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan item version: %w", err)
		}
		ver.IsDiff = isDiff
		export.ItemVersions = append(export.ItemVersions, ver)
	}
	if err := versionRows.Err(); err != nil {
		return nil, err
	}

	return export, nil
}

// ImportWorkspace imports a workspace from an exported data structure.
// It creates a new workspace with regenerated IDs, remapping all references.
// If newName is non-empty, it overrides the workspace name and slug.
func (s *Store) ImportWorkspace(data *models.WorkspaceExport, newName string) (*models.Workspace, error) {
	if data.Version != 1 {
		return nil, fmt.Errorf("unsupported export version: %d", data.Version)
	}

	// Determine workspace name/slug
	wsName := data.Workspace.Name
	wsSlug := data.Workspace.Slug
	if newName != "" {
		wsName = newName
		wsSlug = newName
	}

	ws, err := s.CreateWorkspace(models.WorkspaceCreate{
		Name:        wsName,
		Slug:        wsSlug,
		Description: data.Workspace.Description,
		Settings:    data.Workspace.Settings,
	})
	if err != nil {
		return nil, fmt.Errorf("create workspace: %w", err)
	}

	// Run all data inserts in a single transaction for atomicity
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	// ID mapping: old ID -> new ID
	collMap := make(map[string]string)
	itemMap := make(map[string]string)

	// Import collections
	for _, c := range data.Collections {
		newCollID := newID()
		collMap[c.ID] = newCollID

		_, err := tx.Exec(s.q(`
			INSERT INTO collections (id, workspace_id, name, slug, icon, description, schema, settings, prefix, sort_order, is_default, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			newCollID, ws.ID, c.Name, c.Slug, c.Icon, c.Description, c.Schema, c.Settings, c.Prefix, c.SortOrder, s.dialect.BoolToInt(c.IsDefault),
			c.CreatedAt, c.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("import collection %s: %w", c.Name, err)
		}
	}

	// Import items (first pass: create items, remap collection_id)
	// Item numbers are assigned sequentially in created_at order to produce
	// workspace-global numbering. Exported item_number values are ignored
	// because old exports used per-collection numbering which can have
	// duplicates within a workspace.
	var nextItemNumber int
	for _, it := range data.Items {
		newItemID := newID()
		itemMap[it.ID] = newItemID
		newCollID := collMap[it.CollectionID]
		if newCollID == "" {
			continue // skip orphaned items
		}

		// On first pass, parent_id may refer to an item not yet created, so use empty
		parentID := ""
		if it.ParentID != "" {
			if mapped, ok := itemMap[it.ParentID]; ok {
				parentID = mapped
			}
		}

		nextItemNumber++
		_, err := tx.Exec(s.q(`
			INSERT INTO items (id, workspace_id, collection_id, title, slug, content, fields, tags, pinned, sort_order, parent_id, created_by, last_modified_by, source, item_number, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?, ?)`),
			newItemID, ws.ID, newCollID, it.Title, it.Slug, it.Content, it.Fields, it.Tags, s.dialect.BoolToInt(it.Pinned), it.SortOrder,
			parentID, it.CreatedBy, it.LastModifiedBy, it.Source, nextItemNumber,
			it.CreatedAt, it.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("import item %s: %w", it.Title, err)
		}
	}

	// Second pass: remap parent_id and relation fields (now all items exist)
	for _, it := range data.Items {
		newItemID := itemMap[it.ID]
		if newItemID == "" {
			continue
		}
		// Remap relation fields now that ALL items are mapped
		fields := remapFieldIDs(it.Fields, itemMap, collMap)
		parentID := ""
		if it.ParentID != "" {
			if mapped, ok := itemMap[it.ParentID]; ok {
				parentID = mapped
			}
		}
		_, err := tx.Exec(s.q(`UPDATE items SET fields = ?, parent_id = NULLIF(?, '') WHERE id = ?`),
			fields, parentID, newItemID)
		if err != nil {
			return nil, fmt.Errorf("remap item %s: %w", it.Title, err)
		}
	}

	// Import comments
	for _, cm := range data.Comments {
		newItemID := itemMap[cm.ItemID]
		if newItemID == "" {
			continue
		}
		_, err := tx.Exec(s.q(`
			INSERT INTO comments (id, item_id, workspace_id, author, body, created_by, source, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			newID(), newItemID, ws.ID, cm.Author, cm.Body, cm.CreatedBy, cm.Source,
			cm.CreatedAt, cm.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("import comment: %w", err)
		}
	}

	// Import item links
	for _, lk := range data.ItemLinks {
		newSourceID := itemMap[lk.SourceID]
		newTargetID := itemMap[lk.TargetID]
		if newSourceID == "" || newTargetID == "" {
			continue
		}
		_, err := tx.Exec(s.q(`
			INSERT INTO item_links (id, workspace_id, source_id, target_id, link_type, created_by, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`),
			newID(), ws.ID, newSourceID, newTargetID, lk.LinkType, lk.CreatedBy,
			lk.CreatedAt)
		if err != nil {
			// Ignore duplicate links
			continue
		}
	}

	// Import item versions
	for _, ver := range data.ItemVersions {
		newItemID := itemMap[ver.ItemID]
		if newItemID == "" {
			continue
		}
		_, err := tx.Exec(s.q(`
			INSERT INTO item_versions (id, item_id, content, change_summary, created_by, source, is_diff, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
			newID(), newItemID, ver.Content, ver.ChangeSummary, ver.CreatedBy, ver.Source, s.dialect.BoolToInt(ver.IsDiff),
			ver.CreatedAt)
		if err != nil {
			// Log detail but skip — version history is non-critical
			fmt.Printf("warning: skipped version for item %s: %v\n", ver.ItemID, err)
			continue
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit import: %w", err)
	}

	// Rebuild FTS indexes for the new workspace (outside transaction)
	s.rebuildFTSForWorkspace(ws.ID)

	return ws, nil
}

// rebuildFTSForWorkspace rebuilds the FTS index for all items in a workspace.
// This is needed after import because direct INSERTs bypass the FTS triggers.
// Only applicable to SQLite (PostgreSQL uses trigger-maintained tsvector columns).
func (s *Store) rebuildFTSForWorkspace(wsID string) {
	if s.dialect.Driver() != DriverSQLite {
		return
	}
	rows, err := s.db.Query(s.q(`SELECT rowid, title, content, tags FROM items WHERE workspace_id = ? AND deleted_at IS NULL`), wsID)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var rowid int64
		var title, content, tags string
		if err := rows.Scan(&rowid, &title, &content, &tags); err != nil {
			continue
		}
		s.db.Exec(s.q(`INSERT INTO items_fts(rowid, title, content, tags) VALUES (?, ?, ?, ?)`), rowid, title, content, tags)
	}
}

// remapFieldIDs replaces old UUIDs in a JSON fields string with their new IDs.
// This handles relation fields (e.g. parent: "uuid") without needing to parse the schema.
func remapFieldIDs(fieldsJSON string, itemMap, collMap map[string]string) string {
	result := fieldsJSON
	for oldID, newID := range itemMap {
		if oldID != "" && newID != "" {
			result = strings.ReplaceAll(result, oldID, newID)
		}
	}
	return result
}
