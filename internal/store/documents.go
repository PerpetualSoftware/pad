package store

import (
	"database/sql"
	"errors"
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
		// Validated HERE, not only at the handler, because here is where the
		// rename is decided — under the lock, against the title this
		// transaction re-read (codex round 11).
		//
		// The handler's check compares against a document it read BEFORE the
		// lock. That is fine for giving a caller a fast, friendly 400, but it
		// is a time-of-check that a concurrent rename can invalidate: echo a
		// legacy title back while another request renames the document, and
		// the handler sees "unchanged, skip validation" while this branch sees
		// a genuine rename and would write the legacy title through. Same
		// grandfathering rule, applied where the decision actually happens.
		if msg := models.ValidateDocumentTitle(*input.Title); msg != "" {
			return nil, &InvalidDocumentTitleError{Reason: msg}
		}
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

// escapeLikePattern escapes the three characters that carry meaning inside a
// LIKE pattern, for use with an explicit `ESCAPE '\'` clause.
//
// Without this the cascade's own search term is interpreted as a pattern, and
// a document TITLE decides how (BUG-2798, codex round 1 P2 — plus the rest of
// the class it was an instance of):
//
//   - `_` and `%` are wildcards in BOTH dialects, so a title containing them
//     selects documents that do not link it. Those extra rows rewrite to
//     themselves, so the damage is not corruption — it is that the guard below
//     is computed from this result set, so an over-broad pattern spends a
//     caller's budget on rows that were never going to change.
//   - `\` is where the two dialects DISAGREE, which is the dangerous half.
//     Postgres LIKE treats backslash as the default escape character; SQLite
//     LIKE has no default escape character at all. So `[[Alpha\Beta]]` is
//     searched for as the literal it is on SQLite and as `[[AlphaBeta]]` on
//     Postgres — the linking documents are simply not found, the cascade
//     rewrites nothing, and the rename succeeds leaving every link stale. A
//     silent, dialect-dependent data defect.
//
// The explicit ESCAPE clause makes both dialects agree, rather than leaving
// SQLite correct by accident and Postgres wrong by default.
func escapeLikePattern(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

func (s *Store) updateLinksInTx(tx *sql.Tx, workspaceID, oldTitle, newTitle string) error {
	// Find all documents in the workspace that contain [[oldTitle]]
	searchTerm := "[[" + oldTitle + "]]"
	rows, err := tx.Query(s.q(`
		SELECT id, content FROM documents
		WHERE workspace_id = ? AND deleted_at IS NULL AND content LIKE ? ESCAPE '\'
	`), workspaceID, "%"+escapeLikePattern(searchTerm)+"%")
	if err != nil {
		return err
	}
	defer rows.Close()

	type docUpdate struct {
		id string
		// read is the body this cascade rewrote FROM, kept verbatim: it is
		// the compare-and-set token below, so it must be the exact string
		// the column handed us and never a normalized form of it.
		read      string
		rewritten string
		// retained is what this row contributed to the running total, kept so
		// the compare-and-set below can be given ITS share of the budget when
		// a concurrent edit forces it to re-read and re-rewrite.
		retained int64
	}
	var updates []docUpdate
	var retained int64
	for rows.Next() {
		var du docUpdate
		if err := rows.Scan(&du.id, &du.read); err != nil {
			return err
		}

		// Project what this linker will make the cascade HOLD, before building
		// it, and refuse on the running TOTAL across the linking set
		// (BUG-2798).
		//
		// The total is the right thing to bound, and a per-document cap would
		// not be. Measured: with the title bound in place, one linker holding
		// the largest body a 2 MiB request can carry projects 108,632,370
		// bytes of output — 51.8x — and the loop below holds EVERY rewritten
		// body in `updates` before it writes any of them, so k linkers hold k
		// times that (measured linear at k = 1/2/4). A per-document cap of C
		// still admits k * C, which is the same unbounded shape one level up.
		//
		// RETAINED bytes, not output bytes. An earlier version of this guard
		// summed only the projected output, which bounds nothing when the new
		// title is SHORTER than the old one: renaming a 255-character title to
		// a one-character title makes each 2 MiB linker project about 40 KiB
		// while the cascade still retains its 2 MiB read for the
		// compare-and-set, so hundreds of linkers exhaust memory while the
		// counter reports well under the cap (codex round 1 P1). Both strings
		// are alive at once, so both are counted.
		//
		// Refusing here rather than after the loop is what makes the bound
		// real: at the moment of refusal the process holds the linkers already
		// counted (under the cap by construction) plus this one row's body,
		// and none of the amplified output.
		// The SELECT is a LIKE, and LIKE is not the rewriter. On SQLite it is
		// ASCII case-INSENSITIVE by default (Postgres's is not), so renaming
		// `Alpha` scans every body containing `[[alpha]]` — which
		// links.ReplaceTitle, being case-sensitive, will not touch. Charging
		// those bodies to the budget lets case-variant content that can never
		// be rewritten push a legitimate rename over the cap, on one dialect
		// only (codex round 7).
		//
		// Skipping them is the fix for both halves: no budget is spent, and no
		// no-op UPDATE is issued for a row whose content the cascade was never
		// going to change. Same class as the `%`/`_` over-matching the ESCAPE
		// clause closed — the pattern selects a superset of the linkers, and
		// the authority on what is actually a linker is the rewriter's own
		// case-sensitive count.
		occurrences := int64(strings.Count(du.read, searchTerm))
		if occurrences == 0 {
			continue
		}

		du.retained = cascadeRetainedBytes(du.read, occurrences, oldTitle, newTitle)
		retained += du.retained
		if retained > MaxRenameCascadeRetainedBytes {
			return newRenameCascadeTooLargeError(newTitle, retained)
		}

		du.rewritten = links.ReplaceTitle(du.read, oldTitle, newTitle)
		updates = append(updates, du)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// Test seam (BUG-2785): the last moment at which a concurrent content
	// edit to a linker can still commit ahead of the writes below, which is
	// the whole window this function's compare-and-set exists to survive.
	// Nil in production. No existing seam reaches here — afterDocumentPreLockRead
	// fires before the transaction and afterDocumentPreWrite fires before the
	// renamed document's OWN update, neither of which is this gap.
	if s.afterLinkCascadeRead != nil {
		s.afterLinkCascadeRead(workspaceID)
	}

	for _, du := range updates {
		// The compare-and-set is handed the scan's TOTAL, not a pre-computed
		// budget, so it can both bound and REPORT correctly: a retry must fit
		// in what remains under the cap, and a refusal must name the whole
		// operation's size rather than the re-read alone (codex rounds 3, 8).
		//
		// Everything the scan counted is still held — `updates` references the
		// original read and rewritten bodies for every linker, this one
		// included — so a re-read is allocated ON TOP of them. An earlier
		// version credited this document's share back, on the reasoning that
		// the retry replaces it; it does not, and the bound could be exceeded
		// by up to one document's share while the arithmetic reported it
		// satisfied.
		if err := s.rewriteLinkerCAS(tx, du.id, du.read, du.rewritten, oldTitle, newTitle, searchTerm, retained); err != nil {
			return err
		}
	}
	return nil
}

// cascadeRetainedBytes is what one linking document makes the cascade hold:
// the body it read (kept verbatim as the compare-and-set token) plus the body
// it will write.
//
// Exact rather than an estimate. strings.Replace substitutes every
// non-overlapping occurrence, so the rewritten length is
// len(read) + occurrences * (len(new) - len(old)) to the byte, and this
// function is the only place that arithmetic lives — the scan and the retry
// path must not be allowed to drift apart on it.
func cascadeRetainedBytes(read string, occurrences int64, oldTitle, newTitle string) int64 {
	if occurrences == 0 {
		// No second string exists to charge for: strings.Replace returns its
		// input unchanged when there is nothing to replace, so ReplaceTitle
		// allocates nothing and `rewritten` aliases `read`.
		//
		// This is not a micro-optimisation, it is a correctness case on the
		// retry path (codex round 3 P2): a concurrent edit that REMOVES the
		// link leaves a body with no occurrences, and charging it twice could
		// refuse an otherwise valid rename for memory the cascade never
		// allocates.
		return int64(len(read))
	}
	rewritten := int64(len(read)) + occurrences*int64(len(newTitle)-len(oldTitle))
	return int64(len(read)) + rewritten
}

// RenameCascadeTooLargeError carries the refusal's NUMBERS as typed fields, so
// a caller-facing layer can compose its own sentence instead of splicing this
// error's text into a response.
//
// The distinction matters (codex round 5): the HTTP handler used to append
// err.Error() verbatim, which meant every wrapper any caller added on the way
// up — "update links: " today, anything at all tomorrow — was published to the
// client as part of a public message. The two figures ARE meant to reach the
// caller (Dave's day-63 ruling: the refusal states what it would hold and what
// the cap is, so "split the rename" is actionable advice rather than a shrug);
// the internal call path is not.
//
// This is the round-3 lesson applied in the other direction: there, prose was
// being used to CLASSIFY an error and should have been identity; here, prose
// was being used to REPORT one and should have been data.
type RenameCascadeTooLargeError struct {
	// NewTitle is the caller's own requested title. Echoed back deliberately:
	// it is theirs, and it is what they need to see to understand which
	// rename was refused.
	NewTitle string
	// Retained is the lower bound on bytes the cascade would have held. A
	// lower bound, not a total: the scan stops at the first row that crosses
	// the cap, so the true figure is larger.
	Retained int64
	// Max is the cap in force.
	Max int64
}

func (e *RenameCascadeTooLargeError) Error() string {
	return fmt.Sprintf("%s: renaming to %q would hold at least %d bytes of linked-document content, maximum %d",
		ErrRenameCascadeTooLarge.Error(), e.NewTitle, e.Retained, e.Max)
}

// Unwrap makes errors.Is(err, ErrRenameCascadeTooLarge) hold, so every existing
// sentinel check keeps working.
func (e *RenameCascadeTooLargeError) Unwrap() error { return ErrRenameCascadeTooLarge }

func newRenameCascadeTooLargeError(newTitle string, retained int64) error {
	return &RenameCascadeTooLargeError{
		NewTitle: newTitle,
		Retained: retained,
		Max:      MaxRenameCascadeRetainedBytes,
	}
}

// ErrLinkCascadeContention reports that a rename's link cascade lost its
// compare-and-set on the same linking document too many times in a row.
//
// It is a CONTENTION signal, not a fault: the rename rolled back cleanly and
// retrying it can succeed. Exported so the HTTP layer can answer 503 with a
// Retry-After instead of the 500 that "an internal error occurred" implies —
// a caller told the request will never succeed will not retry, which is the
// opposite of the truth here (codex round 2 on BUG-2785).
var ErrLinkCascadeContention = errors.New("store: link cascade lost the compare-and-set")

// ErrInvalidDocumentTitle reports a rename to a title the wiki-link machinery
// cannot carry. See InvalidDocumentTitleError for why the store enforces this
// rather than trusting its callers to have done so.
var ErrInvalidDocumentTitle = errors.New("store: invalid document title")

// InvalidDocumentTitleError carries the human-readable reason a title was
// refused, so the HTTP layer can return it without re-deriving the rule or
// splicing an internal error's text into a response.
type InvalidDocumentTitleError struct{ Reason string }

func (e *InvalidDocumentTitleError) Error() string {
	return ErrInvalidDocumentTitle.Error() + ": " + e.Reason
}

func (e *InvalidDocumentTitleError) Unwrap() error { return ErrInvalidDocumentTitle }

// ErrRenameCascadeTooLarge reports that a rename was refused because the
// linked-document content it would hold exceeds MaxRenameCascadeRetainedBytes.
//
// Deliberately NOT in ErrLinkCascadeContention's family, and the distinction
// is the caller-visible one: contention means "someone else got there first,
// try again"; this means "this rename cannot be performed as asked, and
// retrying it unchanged will fail identically until the workspace's content
// changes." One wants 503 + Retry-After, the other a permanent 4xx carrying
// the projection so the caller can see what it asked for. Blurring the two
// vocabularies would tell a client to retry forever (BUG-2798, lead ruling
// day-63).
var ErrRenameCascadeTooLarge = errors.New("store: rename cascade exceeds the retained-content bound")

// MaxRenameCascadeRetainedBytes bounds the TOTAL linked-document content a
// single rename may hold in memory: for every linking document, the body read
// plus the body written.
//
// RETAINED rather than merely projected-output, because output alone is not
// the resource. A rename to a SHORTER title projects less output than its
// input while still holding every read body for the compare-and-set — so an
// output-only counter reports ~40 KiB per 2 MiB linker and bounds nothing in
// that direction (codex round 1). Counting both strings makes the cap a
// statement about resident memory, which is what actually runs out.
//
// 32 MiB, and both bounds of the gap are measured rather than picked:
//
//   - Legitimate ceiling. In this development instance's database — a mature
//     workspace set, 206 MB on disk — the ENTIRE corpus of wiki-linking
//     content is 2,949 items totalling 10,077,476 bytes (largest single body
//     86,147 bytes). A cascade over all of it would retain read + rewritten,
//     so ~20,154,952 bytes. That is the absolute ceiling on any conceivable
//     single cascade over that corpus: it assumes every wiki-linking document
//     links the one title being renamed, which no real workspace does. 32 MiB
//     is ~1.6x that impossible worst case.
//
//     The INFERENCE is worth naming rather than hiding, because the guard it
//     justifies is on documents and the measurement is not (codex round 6):
//     that instance's `documents` table is EMPTY, so there is no direct
//     figure to take. `items` is used as the proxy on the grounds that the
//     two hold the same kind of prose and cascade the same way — a reasonable
//     assumption, not a measurement of the guarded path. What can be said
//     without the proxy is narrower and still useful: a workspace whose
//     documents linking one title total more than ~16 MB of content will meet
//     this cap. If real document corpora ever get that large, this number is
//     the thing to re-measure.
//
//   - Hostile floor. A single linking document holding the largest body a
//     2 MiB request can carry retains 110,729,520 bytes once the title bound
//     is in place — 3.3x this cap — so the attack is refused at k = 1 and
//     every k above it, rather than at some threshold count of documents.
//
// The gap is deliberate and wide: a cap has to be far enough above real use
// that nobody meets it by accident, and far enough below the hazard that
// meeting it costs nothing.
//
// The consequence a cap necessarily has, stated because it is user-visible
// and was chosen rather than overlooked: once a workspace's documents linking
// one title exceed this, that title can no longer be renamed, and any EDITOR
// can put it in that state by creating enough linking content. That is a
// denial of one operation by a trusted role — an editor can already delete
// every document in the workspace — and it replaces the previous behaviour,
// where the same input took the server down for everybody. Trading an
// unbounded OOM for a bounded, legible refusal is the whole point of the
// guard, not a gap in it.
//
// Bounding the WORKSPACE's linking content, so the state cannot be reached at
// all, is a quota question rather than a cascade question and is filed
// separately.
//
// What it does NOT cover, stated so the next reader does not over-read it:
// this bounds ONE rename's linked-document content, not concurrent renames (N
// of them may each hold up to this), and not the base cost of a workspace
// whose linking documents are legitimately large — a cascade under the cap
// still allocates whatever it holds.
const MaxRenameCascadeRetainedBytes = 32 << 20

// cascadeRewriteAttempts bounds rewriteLinkerCAS's retry loop.
//
// Three, matching debounceMergeAttempts' reasoning rather than copying its
// number by coincidence: one attempt to lose, one to win against the writer
// that beat it, and one of headroom for a second writer arriving mid-retry.
// Exhausting it needs three consecutive commits to the SAME linker inside one
// cascade, which is pathological contention rather than ordinary editing.
//
// It is a LIVENESS bound, not a correctness one: correctness comes from the
// compare-and-set predicate, which cannot write a stale body however many
// attempts it is given.
// A var rather than a const — deliberately diverging from its sibling
// debounceMergeAttempts, which is a const — so a test can lower it to 1 and
// drive exhaustion deterministically. Forcing exhaustion at 3 would need a
// concurrent commit between each attempt, which needs a per-attempt hook in
// production code; lowering the bound tests the same disposition with less
// test-only surface.
//
// Never written outside tests, and a test that writes it must not run under
// t.Parallel() alongside anything that renames a DOCUMENT — same constraint the
// Store's test seams carry, for the same reason: it is process-global state
// with no synchronization. internal/store does contain parallel tests
// (items_empty_assignment, item_mutation_signal, watches), and none of them
// reaches this cascade today — but that is a fact about the current test
// corpus, not an invariant, which is why the constraint is written here rather
// than inferred from the absence of a collision.
var cascadeRewriteAttempts = 3

// rewriteLinkerCAS applies a title rewrite to one linking document, retrying
// on a lost compare-and-set.
//
// WHY A CAS AT ALL (BUG-2785). The cascade is a read-modify-write across two
// statements: the SELECT above reads a linker's content, Go rewrites the
// string, and this UPDATE writes the result. A content edit committing between
// those two statements is silently erased by an unconditional UPDATE — the
// same lost-update shape as BUG-2770's activity-metadata merge, one table over.
//
// DIALECT SCOPE, because it decides whether any test here means anything.
// The lost update is reachable on POSTGRES only. The SQLite DSN sets
// `_txlock=immediate` (see store.go), so UpdateDocument's db.Begin() takes the
// write lock at BEGIN and holds it across this whole window; a concurrent
// content edit cannot commit inside it and serializes on busy_timeout instead.
// On Postgres under READ COMMITTED each statement takes a fresh snapshot, so
// the edit commits between the two statements and the stale body wins. The CAS
// is therefore a no-op on SQLite by construction — the predicate always matches,
// because nobody else can have written — and load-bearing on Postgres.
//
// WHY THE ZERO-ROW RESULT NEEDS A PROBE. The UPDATE carries two predicates
// that can each refuse it, and RowsAffected cannot say which did:
//
//   - deleted_at IS NULL — the linker was soft-deleted after the SELECT. That
//     is the NORMAL outcome of a documented race (the guard predates this
//     change): the linker is gone, so there is no link left to keep consistent,
//     and the right response is to move on.
//
//     That guard used to carry a note saying it was UNTESTED and kept anyway,
//     because reaching its window needed a seam no test could schedule "and
//     which would cost a fifth one". This change adds that fifth seam for its
//     own reasons, so the note is no longer true and has been removed rather
//     than left to mislead: TestUpdateDocument_CascadeTreatsSoftDeletedLinkerAsDone
//     now drives exactly that window, and a mutation removing the probe arm
//     below dies to it. Recorded here because deleting a previous unit's
//     deliberate "this is untestable" finding is a claim in its own right,
//     and the next reader deserves to know it was closed rather than lost.
//
//   - content = ? — a concurrent edit landed. The right response is the
//     opposite: re-read, re-apply the rewrite to the NEW body, and try again.
//
// Treating the two alike would either retry forever against a deleted row or
// discard a live linker's rewrite. So a refusal is followed by a probe that
// distinguishes them, the same shape BUG-2770 needed for the same reason.
// The probe reads through tx, never the pool: a pool read from inside a
// transaction that holds row locks is BUG-2409's deadlock.
//
// WHAT THIS DOES NOT FIX, named because "the cascade is now safe" is the claim
// that would rot silently (codex round 2 enumerated both):
//
//   - THE MIRROR DIRECTION. This stops the cascade losing a content writer's
//     edit. It does not stop a content writer losing the CASCADE's rewrite: a
//     writer that read the body BEFORE this transaction, then blocked on the
//     row lock, commits its own unconditional UPDATE afterwards and reinstates
//     the old title. Fixing that means giving ordinary content writes a
//     compare-and-set too, which is a much wider change than a rename cascade
//     and belongs to whoever takes it on.
//   - DELETE-THEN-RESTORE. The ErrNoRows arm returns without taking a row
//     lock, so a linker soft-deleted during the cascade and RESTORED before
//     this transaction commits comes back holding the old title. RestoreDocument
//     takes no rename lock, so nothing serializes it against this.
//
// Both are pre-existing and neither is made worse here. They are recorded
// because the next reader's question is "is the cascade correct now", and the
// honest answer is "for the direction this bug named".
func (s *Store) rewriteLinkerCAS(tx *sql.Tx, id, read, rewritten, oldTitle, newTitle, searchTerm string, scanTotal int64) error {
	expected := read
	next := rewritten
	// Total charged by retries so far — see the accumulation below.
	var retriesSpent int64
	for attempt := 0; attempt < cascadeRewriteAttempts; attempt++ {
		res, err := tx.Exec(s.q(`
			UPDATE documents SET content = ?
			WHERE id = ? AND deleted_at IS NULL AND content = ?
		`), next, id, expected)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n > 0 {
			return nil
		}

		var current string
		err = tx.QueryRow(s.q(`
			SELECT content FROM documents WHERE id = ? AND deleted_at IS NULL
		`), id).Scan(&current)
		if err == sql.ErrNoRows {
			// Soft-deleted between the cascade's SELECT and now. Documented
			// normal outcome, not an error.
			return nil
		}
		if err != nil {
			return err
		}

		// The re-read body is a NEW input, supplied by whoever won the race,
		// and it is bounded by nothing this cascade has already checked. Its
		// budget is the cap less what the other linkers are holding, so the
		// aggregate bound holds across retries too — without this, an editor
		// could grow a linker between the scan and the retry and walk the
		// rename straight back into the amplification it was refused for
		// (BUG-2798, codex round 1 P1).
		grownOccurrences := int64(strings.Count(current, searchTerm))
		// ACCUMULATED across attempts, not just this one. Each retry's
		// buffers become unreachable when `expected`/`next` are reassigned
		// below, but unreachable is not the same as reclaimed — the runtime
		// may not have collected them yet, so a run of failures can hold
		// several copies at once (codex round 9). Counting every attempt is
		// the conservative reading, and it errs toward refusing, which is the
		// safe direction for a memory bound. The loop is capped at
		// cascadeRewriteAttempts, so this cannot accumulate without end.
		retriesSpent += cascadeRetainedBytes(current, grownOccurrences, oldTitle, newTitle)
		grown := retriesSpent
		if scanTotal+grown > MaxRenameCascadeRetainedBytes {
			// Report the AGGREGATE, not this body alone. The bodies the scan
			// counted are still held, so the operation's real size is their
			// total plus the re-read — and reporting only `grown` produced a
			// refusal that contradicted itself, telling the caller it would
			// hold 16 MiB against a 32 MiB limit (codex round 8). A refusal
			// whose own numbers do not justify it reads as a bug in the
			// server, which is the opposite of what an actionable error does.
			return newRenameCascadeTooLargeError(newTitle, scanTotal+grown)
		}

		// A concurrent edit won. Rewrite ITS body rather than ours: replaying
		// the original rewrite would reintroduce the very content this bug is
		// about losing. If that edit already removed the link, ReplaceTitle is
		// a no-op and the next attempt writes an identical body — which still
		// affects one row and terminates.
		expected = current
		next = links.ReplaceTitle(current, oldTitle, newTitle)
	}

	// Exhaustion fails the RENAME, rather than leaving this one linker holding
	// a title that no longer exists.
	//
	// The alternative — log and continue — was considered and rejected: it
	// trades a loud, retryable failure for a silent inconsistency, and a
	// rename is atomic in intent (either the title moves and its links follow,
	// or neither). The user can retry a failed rename; nobody goes looking for
	// a stale wiki-link.
	//
	// Wrapped around ErrLinkCascadeContention so the HTTP layer can tell this
	// from a genuine fault. It is CONTENTION, not a bug: retrying can succeed,
	// and a 500 would tell the caller the opposite (codex round 2).
	return fmt.Errorf("%w: document %s after %d attempts", ErrLinkCascadeContention, id, cascadeRewriteAttempts)
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
