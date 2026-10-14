package store

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
)

// BackfillWikiLinksResult reports what the backfill did so cmd/pad/main.go
// can log a one-line summary at startup. Repeat runs should report
// ItemsScanned > 0 but LinksInserted near 0 (already-indexed items
// re-parse to the same row set, and we cap the per-source work with a
// short-circuit on "already has rows" — see below).
type BackfillWikiLinksResult struct {
	// ItemsScanned is the number of items the backfill iterated.
	// Counts every non-soft-deleted item with non-empty content,
	// regardless of whether the item produced any link rows.
	ItemsScanned int

	// ItemsIndexed is the count of items that actually got new
	// rows on this run. On a healthy steady-state install this
	// should be 0 (every item already indexed at write time).
	ItemsIndexed int

	// LinksInserted is the total number of `item_wiki_links` rows
	// the backfill inserted.
	LinksInserted int

	// Errors tracks items the backfill skipped due to a parse or
	// write failure. Errors are logged at WARN but don't abort
	// the run — a broken row beats a missing whole index.
	Errors int
}

// wikiBackfillAfterScanHook, when set by a test, runs once after the scan has
// been read and before any item is processed: the window in which a write the
// scan did not see can land. wikiBackfillAfterReadHook runs inside an item's
// transaction, after its locked re-read and before the replacement. Both get
// the store, so a test can ignore calls from other stores running in parallel.
var (
	wikiBackfillAfterScanHook func(s *Store)
	wikiBackfillAfterReadHook func(s *Store, tx *sql.Tx, workspaceID, itemID string)
)

// BackfillWikiLinks scans every item with non-empty content and
// re-parses its body to populate `item_wiki_links`. Designed to be
// called from server startup after migrations have run. Idempotent:
//
//   - On first boot after migration 061, the table is empty and
//     every item with content produces its full link row set.
//   - On every subsequent boot, items that already have rows in
//     `item_wiki_links` are skipped via a cheap EXISTS check — the
//     work-per-startup decays to "scan the items list and short-circuit".
//   - If an item is rewritten between boots (write-time hook ran),
//     its rows are already current and the backfill skips it.
//
// The short-circuit pattern is intentionally conservative: a future
// "force re-parse" flag could clear `item_wiki_links` and re-run this
// function to pick up parser improvements. We don't expose that flag
// yet — the schema is fresh — but the structure supports it.
//
// Per-item write uses its own transaction so a single bad row doesn't
// poison the whole pass. The per-item work is small enough that
// per-tx overhead is dominated by parser cost.
//
// The scan is identity only. Everything the write depends on (the content,
// whether the item is still live, whether it already has rows) is read again
// inside the item's transaction under the workspace seq lock, which every
// content write holds (BUG-3666). Reading the content at scan time and the
// rows separately let the backfill rebuild links from a body a concurrent
// write had already changed; once rows existed, later boots skipped the item
// and the false backlink stayed until the next content write.
//
// PLAN-1593 / TASK-1594.
func (s *Store) BackfillWikiLinks() (*BackfillWikiLinksResult, error) {
	result := &BackfillWikiLinksResult{}

	// The predicate only narrows the candidates; the locked re-read below
	// decides. An item emptied or deleted after the scan is skipped there.
	rows, err := s.db.Query(s.q(`
		SELECT id, workspace_id
		FROM items
		WHERE content != '' AND deleted_at IS NULL
	`))
	if err != nil {
		return nil, fmt.Errorf("scan items for backfill: %w", err)
	}
	defer rows.Close()

	// Collect ids first to release the rows iterator before we
	// begin per-item transactions — some drivers (including
	// modernc.org/sqlite at high concurrency) get unhappy about
	// nested writes while a SELECT cursor is open.
	type itemRef struct {
		id, workspaceID string
	}
	var items []itemRef
	for rows.Next() {
		var ir itemRef
		if err := rows.Scan(&ir.id, &ir.workspaceID); err != nil {
			return nil, fmt.Errorf("scan backfill row: %w", err)
		}
		items = append(items, ir)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate backfill rows: %w", err)
	}
	rows.Close()

	if wikiBackfillAfterScanHook != nil {
		wikiBackfillAfterScanHook(s)
	}

	for _, ir := range items {
		result.ItemsScanned++
		inserted, err := s.backfillWikiLinksItem(ir.id, ir.workspaceID)
		if err != nil {
			slog.Warn("backfill wiki links: item failed",
				slog.String("item_id", ir.id), slog.String("err", err.Error()))
			result.Errors++
			continue
		}
		if inserted > 0 {
			result.ItemsIndexed++
			result.LinksInserted += inserted
		}
	}
	return result, nil
}

// backfillWikiLinksItem indexes one item in its own transaction and returns
// the number of rows it wrote (0 when the item was skipped).
func (s *Store) backfillWikiLinksItem(itemID, workspaceID string) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	// WORKSPACE LOCK FIRST, then the item row: the order every item write
	// takes, so this cannot invert against them. The row lock alone would
	// hold off writes to THIS item, but resolution reads other items' titles,
	// and a create that repairs broken links to its title runs under the
	// workspace lock, not this row's.
	if err := s.acquireWorkspaceSeqLock(tx, workspaceID); err != nil {
		return 0, err
	}

	// SQLite's write lock already serialises and it does not take the clause,
	// hence the dialect gate (the shape backfillRelationChunk uses).
	lockedRead := `SELECT content FROM items WHERE id = ? AND deleted_at IS NULL`
	if s.dialect.Driver() == DriverPostgres {
		lockedRead += " FOR UPDATE"
	}
	var content string
	if err := tx.QueryRow(s.q(lockedRead), itemID).Scan(&content); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Deleted since the scan.
			return 0, nil
		}
		return 0, fmt.Errorf("re-read item: %w", err)
	}
	if wikiBackfillAfterReadHook != nil {
		wikiBackfillAfterReadHook(s, tx, workspaceID, itemID)
	}
	if content == "" {
		// Emptied since the scan; the write that emptied it cleared the rows.
		return 0, nil
	}

	// Short-circuit: an item that already has rows was indexed at write time.
	//
	// Scanned into a bool — not an int — because `SELECT EXISTS`
	// returns a boolean on Postgres (`true`/`false`) and an
	// integer 0/1 on SQLite. Both database/sql drivers in use
	// (modernc.org/sqlite and lib/pq) coerce their native
	// representation into Go's bool, so this single shape works
	// on both engines. Scanning into `int` happened to work on
	// SQLite but blew up on Postgres, silently disabling the
	// backfill there (Codex round-3 P1).
	var has bool
	if err := tx.QueryRow(s.q(`
		SELECT EXISTS(SELECT 1 FROM item_wiki_links WHERE source_item_id = ?)
	`), itemID).Scan(&has); err != nil {
		return 0, fmt.Errorf("existence check: %w", err)
	}
	if has {
		return 0, nil
	}

	if err := s.replaceWikiLinks(tx, itemID, workspaceID, content); err != nil {
		return 0, fmt.Errorf("replace: %w", err)
	}

	// Count rows inserted via a SELECT before commit — we
	// could derive this from replaceWikiLinks's return but
	// keeping that function's signature minimal pays off
	// across other call sites; the extra COUNT(*) is cheap
	// on the just-inserted rows.
	var inserted int
	if err := tx.QueryRow(s.q(`
		SELECT COUNT(*) FROM item_wiki_links WHERE source_item_id = ?
	`), itemID).Scan(&inserted); err != nil {
		return 0, fmt.Errorf("count: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return inserted, nil
}
