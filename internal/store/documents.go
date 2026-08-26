package store

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/diff"
	"github.com/PerpetualSoftware/pad/internal/links"
	"github.com/PerpetualSoftware/pad/internal/models"
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
	// Whitespace-only queries collapse to empty after FTS sanitization, and
	// SQLite FTS5 errors on `MATCH ''`. Treat them as "no search filter" to
	// match the !="" semantics callers expect. See BUG-818.
	hasSearch := strings.TrimSpace(params.Query) != ""
	if hasSearch {
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
			// Sanitize so FTS5 specials (hyphens, AND/OR/NOT, parens) are
			// treated as literals rather than boolean operators — see BUG-818.
			args = []interface{}{workspaceID, sanitizeFTSQuery(params.Query)}
		} else {
			// PostgreSQL: search_vector lives on the documents table (aliased as "d").
			// PG FTSMatch consumes TWO args (raw + hyphen-sanitized) for the
			// OR-combined plainto_tsquery — see dialect.go and BUG-842.
			ftsMatch := s.dialect.FTSMatch("d", "search_vector")
			query = fmt.Sprintf(`
				SELECT d.id, d.workspace_id, d.title, d.slug, d.content, d.doc_type, d.status, d.tags,
				       d.pinned, d.sort_order, d.created_by, d.last_modified_by, d.source,
				       d.created_at, d.updated_at
				FROM documents d
				WHERE d.workspace_id = ? AND d.deleted_at IS NULL
				AND %s
			`, ftsMatch)
			args = []interface{}{workspaceID, params.Query, sanitizePGFTSQuery(params.Query)}
		}

		if params.Type != "" {
			query += " AND d.doc_type = ?"
			args = append(args, params.Type)
		}
		if params.Status != "" {
			query += " AND d.status = ?"
			args = append(args, params.Status)
		}
		// Tag and Pinned filters were silently dropped by the FTS branch
		// before this fix — see BUG-820 (documents analog of BUG-812).
		if params.Tag != "" {
			tagExpr, tagArg := s.dialect.JSONArrayContains("d.tags", params.Tag)
			query += " AND " + tagExpr
			args = append(args, tagArg)
		}
		if params.Pinned != nil {
			if *params.Pinned {
				query += " AND d.pinned = TRUE"
			} else {
				query += " AND d.pinned = FALSE"
			}
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

	if hasSearch {
		if s.dialect.Driver() == DriverPostgres {
			// PostgreSQL ts_rank(): higher = more relevant → DESC.
			// PG FTSRank consumes TWO args (raw + sanitized) — BUG-842.
			ftsRank := s.dialect.FTSRank("d", "search_vector")
			query += fmt.Sprintf(" ORDER BY %s DESC, d.%s %s", ftsRank, sortCol, order)
			args = append(args, params.Query, sanitizePGFTSQuery(params.Query))
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

	// Transactional so the attachment-reference stamp commits atomically with
	// the content that carries the reference (BUG-2415's protocol, extended to
	// documents by BUG-2614). Without the transaction the stamp and the insert
	// could not serialize against a concurrent orphan-GC claim, which is the
	// entire point of stamping.
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin create document: %w", err)
	}
	defer tx.Rollback()

	// BEFORE the insert, per stampAttachmentRefsTx's ORDERING note: on
	// Postgres the stamp row-locks the attachment rows for the rest of the
	// transaction, so a concurrent claim blocks and then re-evaluates against
	// the fresh stamp. Stamping after the write would leave a gap.
	if err := stampAttachmentRefsTx(tx, s, workspaceID, input.Content); err != nil {
		return nil, err
	}

	_, err = tx.Exec(s.q(`
		INSERT INTO documents (id, workspace_id, title, slug, content, doc_type, status, tags,
		                       pinned, sort_order, created_by, last_modified_by, source, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?, ?)
	`), id, workspaceID, input.Title, slug, input.Content, docType, status, tags,
		s.dialect.BoolToInt(input.Pinned), createdBy, createdBy, source, ts, ts)
	if err != nil {
		return nil, fmt.Errorf("insert document: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit create document: %w", err)
	}

	return s.GetDocument(id)
}

func (s *Store) GetDocument(id string) (*models.Document, error) {
	return s.getDocumentQ(s.db, id)
}

// getDocumentTx reads a document through an OPEN TRANSACTION, so the caller
// sees the row as its own transaction sees it and needs no second pool
// connection while the first is held (BUG-2778). Same SELECT and hydration
// as GetDocument.
//
// Deliberately NOT `FOR UPDATE`. An earlier version locked the row here to
// stop a concurrent soft-delete committing before the write at the end of the
// rename — but the rows-affected guard on that write already makes the
// outcome atomic (the whole transaction, cascade included, rolls back), so
// the lock changed only WHICH writer wins, not whether the result is
// consistent. A mutation removing it survived the suite, which is the honest
// signal that it was a second mechanism for a window one mechanism already
// covers; the remaining guard has its own justification and its own test.
func (s *Store) getDocumentTx(tx *sql.Tx, id string) (*models.Document, error) {
	return s.getDocumentQ(tx, id)
}

// getDocumentQ is the one document-row read behind GetDocument and
// getDocumentTx, differing only in executor.
func (s *Store) getDocumentQ(q rowQueryer, id string) (*models.Document, error) {
	var d models.Document
	var createdAt, updatedAt string
	var deletedAt *string
	var pinned bool

	err := q.QueryRow(s.q(`
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

func (s *Store) UpdateDocument(id string, input models.DocumentUpdate) (*models.Document, error) {
	existing, err := s.GetDocument(id)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, nil
	}

	// Test seam (BUG-2778): the read above is stale by exactly this window,
	// which is why the rename decision and the cascade's old title come from
	// the re-read below. Nil in production.
	if s.afterDocumentPreLockRead != nil {
		s.afterDocumentPreLockRead(id)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// BUG-2778: serialize TITLE RENAMES per workspace on Postgres, before
	// this transaction takes any row lock.
	//
	// A rename locks rows in two stages: updateLinksInTx writes every OTHER
	// document whose content links the old title, and the UPDATE at the end
	// of this function writes THIS document. Two concurrent renames of
	// documents that link to each other therefore take the same two row locks
	// in opposite orders — tx1 locks B then wants A, tx2 locks A then wants B
	// — and Postgres aborts one with SQLSTATE 40P01. Measured, not argued:
	// before this lock, a probe that renamed two mutually-linking documents
	// concurrently deadlocked on 12 of 12 rounds.
	//
	// WHY NOT `ORDER BY id` ON THE CASCADE, which is what BUG-2778 proposed
	// when it was filed from reading rather than from a repro: the cycle does
	// not come from the ORDER of the cascade's own rows. Each transaction's
	// cascade set here is a single row, and the cycle is cascade-then-self.
	// Ordering the cascade leaves it exactly as reachable; only a rule that
	// covers BOTH stages removes it.
	//
	// A workspace-scoped advisory lock rather than a sorted lock batch,
	// because the two stages touch different tables and different row sets
	// (versions, the slug-uniqueness scan) and a sorted batch would have to
	// predict all of them. It is the same instrument BUG-2074 uses for
	// parent-edge writes, on its own namespaced key so renames contend only
	// with renames. No-op on SQLite, whose single writer cannot produce the
	// cycle at all (the probe found zero deadlocks there).
	//
	// It fires whenever a title is SUPPLIED, not only when the supplied title
	// differs from what we read before the lock — deciding that from the
	// pre-lock value is precisely the staleness this block exists to remove,
	// and a same-title PATCH is a cheap uncontended lock acquisition.
	if input.Title != nil {
		if err := s.acquireWorkspaceDocumentRenameLock(tx, existing.WorkspaceID); err != nil {
			return nil, err
		}
		// Re-read UNDER the lock and decide from that row, not from the
		// pre-transaction read above (codex round 1). `existing` was loaded
		// before this transaction and before this lock, so a rename that
		// committed in between is invisible to it — and every decision below
		// is made from it: whether to cascade at all, and which OLD TITLE the
		// cascade rewrites. Two concurrent renames of the SAME document could
		// therefore leave backlinks pointing at a title nothing carries any
		// more, or skip the cascade entirely because the stale row's title
		// happens to equal the requested one. This is the same defect family
		// as BUG-2776 one layer down: a decision made from a snapshot taken
		// before the lock that protects it.
		//
		// The lock is taken whenever a title is SUPPLIED rather than only
		// when it differs from the stale read — deciding that from the stale
		// value is exactly what this block exists to stop.
		fresh, ferr := s.getDocumentTx(tx, id)
		if ferr != nil {
			return nil, fmt.Errorf("re-read document under rename lock: %w", ferr)
		}
		if fresh == nil {
			// Deleted between the pre-tx read and the lock; the caller's 404
			// path handles a nil document.
			return nil, nil
		}
		existing = fresh
	}

	// Stamp the incoming content's attachment references first (BUG-2614,
	// same protocol and ordering as items and comments). Only when content is
	// actually being written — a metadata-only PATCH neither adds nor keeps a
	// reference, and stamping on one would refresh rows this write has no
	// opinion about.
	if input.Content != nil {
		if err := stampAttachmentRefsTx(tx, s, existing.WorkspaceID, *input.Content); err != nil {
			return nil, err
		}
	}

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
			// Through the transaction, not the pool (BUG-2778): this runs
			// with the transaction open and, for a rename, with the rename
			// lock held.
			shouldVersion, err = s.shouldCreateVersionQ(tx, id, createdBy, source)
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

	// Update [[link]] references if title is changing.
	//
	// KNOWN GAP, filed as BUG-2629, deliberately not fixed here: this cascade
	// rewrites OTHER documents' bodies without stamping the attachment
	// references in the text it writes, and wiki_links.go::cascadeTitleRename
	// does the same on the items side. Uniform across both surfaces, so fixing
	// only this one would leave the larger hole open. It is also the weakest
	// member of that family — the cascade rewrites link text in content whose
	// references were already stamped when written and are still visible to
	// the scan, so a NEW reference requires a title that literally contains a
	// `pad-attachment:` token.
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
		newSlug, err := s.uniqueSlugExcluding(tx, "documents", "workspace_id", existing.WorkspaceID, baseSlug, id)
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

	if s.afterDocumentPreWrite != nil {
		s.afterDocumentPreWrite(id)
	}

	args = append(args, id)
	// deleted_at IS NULL, and the row count is CHECKED (BUG-2778): without
	// it, a document soft-deleted since this transaction began is written
	// anyway — and on the rename path the backlink cascade above has already
	// rewritten every linker, so the caller is told not-found (GetDocument
	// filters archived rows) while those rewrites stay behind. Returning
	// early here leaves the deferred Rollback to undo the cascade with it, so
	// the rename and its cascade land together or not at all.
	//
	// This is the ONLY thing standing between a concurrent soft-delete and a
	// write to the archived row — on every path, rename or not. A
	// content-only PATCH takes no rename lock and holds no row lock until
	// this statement, so the delete can land at any point before it.
	query := fmt.Sprintf("UPDATE documents SET %s WHERE id = ? AND deleted_at IS NULL", strings.Join(sets, ", "))
	res, err := tx.Exec(s.q(query), args...)
	if err != nil {
		return nil, fmt.Errorf("update document: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("update document: rows affected: %w", err)
	}
	if affected == 0 {
		return nil, nil
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return s.GetDocument(id)
}

// acquireWorkspaceDocumentRenameLock serializes document TITLE RENAMES within
// a workspace on Postgres (BUG-2778). Its key is namespaced so it contends
// only with other renames — reusing acquireWorkspaceSeqLock's bare workspace
// key would have made every rename wait behind every item-number and seq
// write in the workspace, and vice versa, for no benefit: the cycle this
// closes is rename-against-rename (codex round 2). Same shape and no-op
// dialect gate as acquireWorkspaceParentLinkLock.
func (s *Store) acquireWorkspaceDocumentRenameLock(tx *sql.Tx, workspaceID string) error {
	if s.dialect.Driver() != DriverPostgres {
		return nil
	}
	// BOUNDED WAIT (codex round 5). The transaction has already taken a pool
	// connection by the time it waits here, so an unbounded wait converts
	// lock contention into pool exhaustion: a burst of renames in ONE
	// workspace can pin connections and stall unrelated work, and a client
	// disconnecting does not cancel the wait (the HTTP context is not
	// threaded into this call).
	//
	// WHAT THE 5s DOES AND DOES NOT COVER, because a bound with no receipt
	// invites more trust than it earns: it bounds each LOCK WAIT inside this
	// transaction once the connection is already held. It does NOT bound the
	// transaction's total duration, and it does NOT bound the wait for a pool
	// connection BEFORE Begin() — a saturated pool still queues callers ahead
	// of this code. The number is a safety cap chosen to be far above any
	// plausible uncontended acquisition, NOT a tuned value: no rename-duration
	// or cascade-size percentile has been measured, and this comment should
	// not be read as if one had. Measure before treating 5s as meaningful.
	//
	// SET LOCAL, so it dies with the transaction rather than leaking back to
	// the pool. It also bounds the row-lock waits later in this transaction,
	// which is intended by the same argument.
	if _, err := tx.Exec("SET LOCAL lock_timeout = '5s'"); err != nil {
		return fmt.Errorf("set rename lock timeout: %w", err)
	}
	// An operator seeing contention here sees it as wait_event_type='Lock',
	// wait_event='advisory' in pg_stat_activity, with this query text — the
	// 'pad:document-rename:' literal is what identifies the class. The
	// workspace id is a bound parameter and will not appear in the text;
	// pg_blocking_pids() on the waiter finds the holder.
	if _, err := tx.Exec("SELECT pg_advisory_xact_lock(hashtext('pad:document-rename:' || $1))", workspaceID); err != nil {
		return fmt.Errorf("acquire workspace document-rename lock: %w", err)
	}
	return nil
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
		// deleted_at IS NULL here too (codex round 4): the SELECT above filters
		// archived linkers, but a soft-delete can land between that read and
		// this write, and an unqualified UPDATE would then rewrite the content
		// of a document the user has already archived. A zero-row result is
		// the NORMAL outcome of that race and not an error — the linker is
		// gone, so there is no link left to keep consistent.
		//
		// UNTESTED, and deliberately kept anyway. A mutation removing this
		// clause survives the suite: reaching it needs a delete landing
		// between the SELECT a few lines above and this loop, which no
		// existing seam can schedule and which would cost a fifth one. It
		// stays because it closes a window NOTHING else covers — the opposite
		// of the FOR UPDATE this unit deleted, which survived its mutation
		// because another mechanism already covered the same window. Same
		// signal, opposite disposition, and the difference is whether the
		// guard is redundant or merely unreachable by the current
		// instruments.
		_, err = tx.Exec(s.q("UPDATE documents SET content = ? WHERE id = ? AND deleted_at IS NULL"), du.content, du.id)
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
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Read the soft-deleted document's content + workspace inside the tx so
	// we can re-stamp its attachment references before it becomes live again.
	// GetDocument filters deleted_at IS NULL, so it can't see this row yet.
	var content, workspaceID string
	if err := tx.QueryRow(s.q(`
		SELECT content, workspace_id FROM documents WHERE id = ? AND deleted_at IS NOT NULL
	`), id).Scan(&content, &workspaceID); err != nil {
		if err == sql.ErrNoRows {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}

	// BUG-2629: re-assert the document's attachment references at the moment
	// it becomes live again. While archived, the live AttachmentReferenced
	// scan can't see this document's refs (BUG-2614 only scans live docs), so
	// the orphan GC may have let their last_referenced_at go stale; a claim
	// racing this restore keys on that stamp, not the live scan. Stamp BEFORE
	// the deleted_at clear, in this tx, per stampAttachmentRefsTx's ORDERING
	// note: the stamp's row-lock makes a concurrent claim block until commit
	// and re-evaluate against the fresh stamp — refusing. Document refs are
	// necessarily never-attached (no document_id column on attachments), so
	// this is the leg with nothing else standing between it and the claim.
	// Prevention only: an already-reclaimed blob is gone (stamp matches zero
	// rows).
	if err := stampAttachmentRefsTx(tx, s, workspaceID, content); err != nil {
		return nil, err
	}

	ts := now()
	result, err := tx.Exec(s.q(`
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
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetDocument(id)
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
