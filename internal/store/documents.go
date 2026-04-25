package store

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/xarmian/pad/internal/diff"
	"github.com/xarmian/pad/internal/links"
	"github.com/xarmian/pad/internal/models"
)

func (s *Store) ListDocuments(workspaceID string, params models.DocumentListParams) ([]models.Document, error) {
	query := `
		SELECT id, workspace_id, title, slug, content, doc_type, status, tags,
		       pinned, sort_order, created_by, last_modified_by, source,
		       created_at, updated_at
		FROM documents
		WHERE workspace_id = ? AND deleted_at IS NULL
	`
	args := []interface{}{workspaceID}

	if params.Type != "" {
		query += " AND doc_type = ?"
		args = append(args, params.Type)
	}
	if params.Status != "" {
		query += " AND status = ?"
		args = append(args, params.Status)
	}
	if params.Tag != "" {
		tagExpr, tagArg := s.dialect.JSONArrayContains("tags", params.Tag)
		query += " AND " + tagExpr
		args = append(args, tagArg)
	}
	if params.Pinned != nil {
		if *params.Pinned {
			query += " AND pinned = TRUE"
		} else {
			query += " AND pinned = FALSE"
		}
	}
	if params.Query != "" {
		// Use FTS for search
		if s.dialect.Driver() == DriverSQLite {
			ftsMatch := s.dialect.FTSMatch("documents_fts", "search_vector")
			query = fmt.Sprintf(`
				SELECT d.id, d.workspace_id, d.title, d.slug, d.content, d.doc_type, d.status, d.tags,
				       d.pinned, d.sort_order, d.created_by, d.last_modified_by, d.source,
				       d.created_at, d.updated_at
				FROM documents d
				JOIN documents_fts fts ON d.rowid = fts.rowid
				WHERE d.workspace_id = ? AND d.deleted_at IS NULL
				AND %s
			`, ftsMatch)
		} else {
			// PostgreSQL: search_vector lives on the documents table (aliased as "d").
			ftsMatch := s.dialect.FTSMatch("d", "search_vector")
			query = fmt.Sprintf(`
				SELECT d.id, d.workspace_id, d.title, d.slug, d.content, d.doc_type, d.status, d.tags,
				       d.pinned, d.sort_order, d.created_by, d.last_modified_by, d.source,
				       d.created_at, d.updated_at
				FROM documents d
				WHERE d.workspace_id = ? AND d.deleted_at IS NULL
				AND %s
			`, ftsMatch)
		}
		args = []interface{}{workspaceID, params.Query}

		if params.Type != "" {
			query += " AND d.doc_type = ?"
			args = append(args, params.Type)
		}
		if params.Status != "" {
			query += " AND d.status = ?"
			args = append(args, params.Status)
		}
	}

	// Sort
	sortCol := "updated_at"
	if params.Sort != "" {
		switch params.Sort {
		case "title":
			sortCol = "title"
		case "created_at":
			sortCol = "created_at"
		case "updated_at":
			sortCol = "updated_at"
		case "sort_order":
			sortCol = "sort_order"
		}
	}
	order := "DESC"
	if params.Order == "asc" {
		order = "ASC"
	}

	if params.Query != "" {
		if s.dialect.Driver() == DriverPostgres {
			// PostgreSQL ts_rank(): higher = more relevant → DESC
			ftsRank := s.dialect.FTSRank("d", "search_vector")
			query += fmt.Sprintf(" ORDER BY %s DESC, d.%s %s", ftsRank, sortCol, order)
			args = append(args, params.Query) // extra placeholder for ts_rank
		} else {
			// SQLite FTS5: rank is a hidden column on the FTS JOIN (ascending = better)
			query += fmt.Sprintf(" ORDER BY rank, d.%s %s", sortCol, order)
		}
	} else {
		query += fmt.Sprintf(" ORDER BY pinned DESC, %s %s", sortCol, order)
	}

	rows, err := s.db.Query(s.q(query), args...)
	if err != nil {
		return nil, fmt.Errorf("list documents: %w", err)
	}
	defer rows.Close()

	return scanDocuments(rows)
}

func (s *Store) CreateDocument(workspaceID string, input models.DocumentCreate) (*models.Document, error) {
	id := newID()
	ts := now()

	docType := input.DocType
	if docType == "" {
		docType = "notes"
	}
	status := input.Status
	if status == "" {
		status = "draft"
	}
	tags := input.Tags
	if tags == "" {
		tags = "[]"
	}
	createdBy := input.CreatedBy
	if createdBy == "" {
		createdBy = "user"
	}
	source := input.Source
	if source == "" {
		source = "web"
	}

	baseSlug := slugify(input.Title)
	if baseSlug == "" {
		baseSlug = "untitled"
	}
	slug, err := s.uniqueSlug("documents", "workspace_id", workspaceID, baseSlug)
	if err != nil {
		return nil, err
	}

	_, err = s.db.Exec(s.q(`
		INSERT INTO documents (id, workspace_id, title, slug, content, doc_type, status, tags,
		                       pinned, sort_order, created_by, last_modified_by, source, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?, ?)
	`), id, workspaceID, input.Title, slug, input.Content, docType, status, tags,
		s.dialect.BoolToInt(input.Pinned), createdBy, createdBy, source, ts, ts)
	if err != nil {
		return nil, fmt.Errorf("insert document: %w", err)
	}

	return s.GetDocument(id)
}

func (s *Store) GetDocument(id string) (*models.Document, error) {
	var d models.Document
	var createdAt, updatedAt string
	var deletedAt *string
	var pinned bool

	err := s.db.QueryRow(s.q(`
		SELECT id, workspace_id, title, slug, content, doc_type, status, tags,
		       pinned, sort_order, created_by, last_modified_by, source,
		       created_at, updated_at, deleted_at
		FROM documents
		WHERE id = ? AND deleted_at IS NULL
	`), id).Scan(
		&d.ID, &d.WorkspaceID, &d.Title, &d.Slug, &d.Content, &d.DocType, &d.Status, &d.Tags,
		&pinned, &d.SortOrder, &d.CreatedBy, &d.LastModifiedBy, &d.Source,
		&createdAt, &updatedAt, &deletedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	d.Pinned = pinned
	d.CreatedAt = parseTime(createdAt)
	d.UpdatedAt = parseTime(updatedAt)
	d.DeletedAt = parseTimePtr(deletedAt)
	return &d, nil
}

func (s *Store) GetDocumentByTitle(workspaceID, title string) (*models.Document, error) {
	var id string
	err := s.db.QueryRow(s.q(`
		SELECT id FROM documents
		WHERE workspace_id = ? AND title = ? AND deleted_at IS NULL
	`), workspaceID, title).Scan(&id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.GetDocument(id)
}

func (s *Store) UpdateDocument(id string, input models.DocumentUpdate) (*models.Document, error) {
	existing, err := s.GetDocument(id)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	ts := now()

	// Create version if content is changing (throttled to avoid bloat from auto-save)
	if input.Content != nil && *input.Content != existing.Content {
		createdBy := input.LastModifiedBy
		if createdBy == "" {
			createdBy = "user"
		}
		source := input.Source
		if source == "" {
			source = "web"
		}

		// Title changes always get a version; content-only changes are throttled
		forceVersion := input.Title != nil && *input.Title != existing.Title
		shouldVersion := forceVersion
		if !shouldVersion {
			shouldVersion, err = s.ShouldCreateVersion(id, createdBy, source)
			if err != nil {
				return nil, fmt.Errorf("check version throttle: %w", err)
			}
		}

		if shouldVersion {
			vid := newID()

			// Store a reverse diff (patch from new → old) instead of full content.
			// Falls back to full content if the diff isn't meaningfully smaller.
			versionContent := existing.Content
			isDiff := false
			patch := diff.CreateReversePatch(existing.Content, *input.Content)
			if diff.IsDiffSmaller(patch, existing.Content) {
				versionContent = patch
				isDiff = true
			}

			_, err = tx.Exec(s.q(`
				INSERT INTO versions (id, document_id, content, change_summary, created_by, source, is_diff, created_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			`), vid, id, versionContent, input.ChangeSummary, createdBy, source, s.dialect.BoolToInt(isDiff), ts)
			if err != nil {
				return nil, fmt.Errorf("create version: %w", err)
			}
		}
	}

	// Update [[link]] references if title is changing
	if input.Title != nil && *input.Title != existing.Title {
		err = s.updateLinksInTx(tx, existing.WorkspaceID, existing.Title, *input.Title)
		if err != nil {
			return nil, fmt.Errorf("update links: %w", err)
		}
	}

	// Build update query
	sets := []string{"updated_at = ?"}
	args := []interface{}{ts}

	if input.Title != nil {
		sets = append(sets, "title = ?")
		args = append(args, *input.Title)
		// Update slug too, ensuring uniqueness within workspace
		baseSlug := slugify(*input.Title)
		if baseSlug == "" {
			baseSlug = "untitled"
		}
		newSlug, err := s.uniqueSlugExcluding("documents", "workspace_id", existing.WorkspaceID, baseSlug, id)
		if err != nil {
			return nil, fmt.Errorf("unique slug: %w", err)
		}
		sets = append(sets, "slug = ?")
		args = append(args, newSlug)
	}
	if input.Content != nil {
		sets = append(sets, "content = ?")
		args = append(args, *input.Content)
	}
	if input.DocType != nil {
		sets = append(sets, "doc_type = ?")
		args = append(args, *input.DocType)
	}
	if input.Status != nil {
		sets = append(sets, "status = ?")
		args = append(args, *input.Status)
	}
	if input.Tags != nil {
		sets = append(sets, "tags = ?")
		args = append(args, *input.Tags)
	}
	if input.Pinned != nil {
		sets = append(sets, "pinned = ?")
		args = append(args, s.dialect.BoolToInt(*input.Pinned))
	}
	if input.SortOrder != nil {
		sets = append(sets, "sort_order = ?")
		args = append(args, *input.SortOrder)
	}
	if input.LastModifiedBy != "" {
		sets = append(sets, "last_modified_by = ?")
		args = append(args, input.LastModifiedBy)
	}
	if input.Source != "" {
		sets = append(sets, "source = ?")
		args = append(args, input.Source)
	}

	args = append(args, id)
	query := fmt.Sprintf("UPDATE documents SET %s WHERE id = ?", strings.Join(sets, ", "))
	_, err = tx.Exec(s.q(query), args...)
	if err != nil {
		return nil, fmt.Errorf("update document: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return s.GetDocument(id)
}

func (s *Store) updateLinksInTx(tx *sql.Tx, workspaceID, oldTitle, newTitle string) error {
	// Find all documents in the workspace that contain [[oldTitle]]
	searchTerm := "[[" + oldTitle + "]]"
	rows, err := tx.Query(s.q(`
		SELECT id, content FROM documents
		WHERE workspace_id = ? AND deleted_at IS NULL AND content LIKE ?
	`), workspaceID, "%"+searchTerm+"%")
	if err != nil {
		return err
	}
	defer rows.Close()

	type docUpdate struct {
		id      string
		content string
	}
	var updates []docUpdate
	for rows.Next() {
		var du docUpdate
		if err := rows.Scan(&du.id, &du.content); err != nil {
			return err
		}
		du.content = links.ReplaceTitle(du.content, oldTitle, newTitle)
		updates = append(updates, du)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, du := range updates {
		_, err = tx.Exec(s.q("UPDATE documents SET content = ? WHERE id = ?"), du.content, du.id)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) DeleteDocument(id string) error {
	ts := now()
	result, err := s.db.Exec(s.q(`
		UPDATE documents SET deleted_at = ?, updated_at = ?, status = 'archived'
		WHERE id = ? AND deleted_at IS NULL
	`), ts, ts, id)
	if err != nil {
		return err
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) RestoreDocument(id string) (*models.Document, error) {
	ts := now()
	result, err := s.db.Exec(s.q(`
		UPDATE documents SET deleted_at = NULL, updated_at = ?, status = 'draft'
		WHERE id = ? AND deleted_at IS NOT NULL
	`), ts, id)
	if err != nil {
		return nil, err
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return nil, sql.ErrNoRows
	}
	return s.GetDocument(id)
}

func (s *Store) QuickSave(workspaceID string, input models.QuickSave) (*models.Document, error) {
	// Try to find existing document by title
	existing, err := s.GetDocumentByTitle(workspaceID, input.Title)
	if err != nil {
		return nil, err
	}

	if existing != nil {
		// Update existing
		update := models.DocumentUpdate{
			Content:        &input.Content,
			LastModifiedBy: input.CreatedBy,
			Source:         input.Source,
			ChangeSummary:  input.ChangeSummary,
		}
		if input.DocType != "" {
			update.DocType = &input.DocType
		}
		if input.Status != "" {
			update.Status = &input.Status
		}
		if input.Tags != "" {
			update.Tags = &input.Tags
		}
		return s.UpdateDocument(existing.ID, update)
	}

	// Create new
	create := models.DocumentCreate{
		Title:     input.Title,
		Content:   input.Content,
		DocType:   input.DocType,
		Status:    input.Status,
		Tags:      input.Tags,
		CreatedBy: input.CreatedBy,
		Source:    input.Source,
	}
	return s.CreateDocument(workspaceID, create)
}

func (s *Store) BulkRead(ids []string) ([]models.Document, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	placeholders := make([]string, len(ids))
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}

	query := fmt.Sprintf(`
		SELECT id, workspace_id, title, slug, content, doc_type, status, tags,
		       pinned, sort_order, created_by, last_modified_by, source,
		       created_at, updated_at
		FROM documents
		WHERE id IN (%s) AND deleted_at IS NULL
	`, strings.Join(placeholders, ","))

	rows, err := s.db.Query(s.q(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanDocuments(rows)
}

func (s *Store) GetBacklinks(workspaceID, documentTitle string) ([]models.Document, error) {
	searchTerm := "[[" + documentTitle + "]]"
	rows, err := s.db.Query(s.q(`
		SELECT id, workspace_id, title, slug, content, doc_type, status, tags,
		       pinned, sort_order, created_by, last_modified_by, source,
		       created_at, updated_at
		FROM documents
		WHERE workspace_id = ? AND deleted_at IS NULL AND content LIKE ?
	`), workspaceID, "%"+searchTerm+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDocuments(rows)
}

func (s *Store) GetLinks(workspaceID, content string) ([]models.Document, error) {
	titles := links.Extract(content)
	if len(titles) == 0 {
		return nil, nil
	}

	placeholders := make([]string, len(titles))
	args := []interface{}{workspaceID}
	for i, t := range titles {
		placeholders[i] = "?"
		args = append(args, t)
	}

	query := fmt.Sprintf(`
		SELECT id, workspace_id, title, slug, content, doc_type, status, tags,
		       pinned, sort_order, created_by, last_modified_by, source,
		       created_at, updated_at
		FROM documents
		WHERE workspace_id = ? AND deleted_at IS NULL AND title IN (%s)
	`, strings.Join(placeholders, ","))

	rows, err := s.db.Query(s.q(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDocuments(rows)
}

func (s *Store) GetContext(workspaceID string, types []string, includeContent bool) ([]models.Document, error) {
	query := `
		SELECT id, workspace_id, title, slug, content, doc_type, status, tags,
		       pinned, sort_order, created_by, last_modified_by, source,
		       created_at, updated_at
		FROM documents
		WHERE workspace_id = ? AND deleted_at IS NULL
		AND (status = 'active' OR pinned = TRUE)
	`
	args := []interface{}{workspaceID}

	if len(types) > 0 {
		placeholders := make([]string, len(types))
		for i, t := range types {
			placeholders[i] = "?"
			args = append(args, t)
		}
		query += fmt.Sprintf(" AND doc_type IN (%s)", strings.Join(placeholders, ","))
	}

	query += " ORDER BY pinned DESC, updated_at DESC"

	rows, err := s.db.Query(s.q(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	docs, err := scanDocuments(rows)
	if err != nil {
		return nil, err
	}

	if !includeContent {
		for i := range docs {
			docs[i].Content = ""
		}
	}

	return docs, nil
}

func scanDocuments(rows *sql.Rows) ([]models.Document, error) {
	var docs []models.Document
	for rows.Next() {
		var d models.Document
		var createdAt, updatedAt string
		var pinned bool
		if err := rows.Scan(
			&d.ID, &d.WorkspaceID, &d.Title, &d.Slug, &d.Content, &d.DocType, &d.Status, &d.Tags,
			&pinned, &d.SortOrder, &d.CreatedBy, &d.LastModifiedBy, &d.Source,
			&createdAt, &updatedAt,
		); err != nil {
			return nil, err
		}
		d.Pinned = pinned
		d.CreatedAt = parseTime(createdAt)
		d.UpdatedAt = parseTime(updatedAt)
		docs = append(docs, d)
	}
	return docs, rows.Err()
}
