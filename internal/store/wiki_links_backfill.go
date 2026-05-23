package store

import (
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
// PLAN-1593 / TASK-1594.
func (s *Store) BackfillWikiLinks() (*BackfillWikiLinksResult, error) {
	result := &BackfillWikiLinksResult{}

	// Stream items rather than buffering — large workspaces could
	// have tens of thousands of items. SQLite handles streaming
	// fine; the modernc.org driver doesn't buffer everything by
	// default.
	rows, err := s.db.Query(s.q(`
		SELECT id, workspace_id, content
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
		id, workspaceID, content string
	}
	var items []itemRef
	for rows.Next() {
		var ir itemRef
		if err := rows.Scan(&ir.id, &ir.workspaceID, &ir.content); err != nil {
			return nil, fmt.Errorf("scan backfill row: %w", err)
		}
		items = append(items, ir)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate backfill rows: %w", err)
	}
	rows.Close()

	for _, ir := range items {
		result.ItemsScanned++

		// Short-circuit: an item that already has rows in the
		// wiki-links table was indexed at write time and doesn't
		// need re-parsing. Cheap EXISTS check.
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
		if err := s.db.QueryRow(s.q(`
			SELECT EXISTS(SELECT 1 FROM item_wiki_links WHERE source_item_id = ?)
		`), ir.id).Scan(&has); err != nil {
			slog.Warn("backfill wiki links: existence check failed",
				slog.String("item_id", ir.id), slog.String("err", err.Error()))
			result.Errors++
			continue
		}
		if has {
			continue
		}

		// Open a tiny tx for this one item. If `replaceWikiLinks`
		// fails, the tx rolls back and we log + move on.
		tx, err := s.db.Begin()
		if err != nil {
			slog.Warn("backfill wiki links: begin tx failed",
				slog.String("item_id", ir.id), slog.String("err", err.Error()))
			result.Errors++
			continue
		}
		if err := s.replaceWikiLinks(tx, ir.id, ir.workspaceID, ir.content); err != nil {
			tx.Rollback()
			slog.Warn("backfill wiki links: replace failed",
				slog.String("item_id", ir.id), slog.String("err", err.Error()))
			result.Errors++
			continue
		}

		// Count rows inserted via a SELECT before commit — we
		// could derive this from replaceWikiLinks's return but
		// keeping that function's signature minimal pays off
		// across other call sites; the extra COUNT(*) is cheap
		// on the just-inserted rows.
		var inserted int
		if err := tx.QueryRow(s.q(`
			SELECT COUNT(*) FROM item_wiki_links WHERE source_item_id = ?
		`), ir.id).Scan(&inserted); err != nil {
			tx.Rollback()
			slog.Warn("backfill wiki links: count failed",
				slog.String("item_id", ir.id), slog.String("err", err.Error()))
			result.Errors++
			continue
		}
		if err := tx.Commit(); err != nil {
			slog.Warn("backfill wiki links: commit failed",
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
