package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// BackfillRelationLinksResult reports what the backfill did so cmd/pad can log
// a one-line summary at startup.
type BackfillRelationLinksResult struct {
	// Skipped is true when the backfill decided it had nothing to do without
	// scanning items — see BackfillRelationLinks for the two ways that
	// happens.
	Skipped bool

	// ItemsScanned counts the live items the backfill walked.
	ItemsScanned int

	// LinksInserted is the number of index rows written.
	LinksInserted int
}

// BackfillRelationLinks derives `item_relation_links` for every live item.
// Called from server startup after migrations.
//
// ITS GUARD IS A COMPLETION MARKER, and the two shapes it is not are both
// instructive.
//
// It is not the wiki backfill's per-item EXISTS. That test does not work here:
// most items carry no relation value, so "zero rows" is their correct steady
// state and is indistinguishable from "never indexed" — a per-item EXISTS
// would re-derive nearly every item on every boot, forever.
//
// It is also no longer "does the table have any row", which is what I wrote
// first and which codex round 1 correctly called a P1. Rows commit per item,
// so a crash partway through leaves the table NON-EMPTY AND INCOMPLETE — and
// that guard then skips forever, with the missing edges never derived. The
// "rebuild by deleting the table" story only ever covered deliberate deletion,
// not an interrupted run.
//
// So completion is recorded EXPLICITLY, in platform_settings, and only AFTER a
// full pass finishes. A crash before that leaves no marker and the next boot
// re-derives — which is safe because replaceRelationLinks deletes before it
// inserts, so a repeat pass is idempotent rather than additive. The marker is
// written after the work for the same reason a dedupe token must be: writing
// it first turns a mid-run failure into a permanent silent loss.
//
// A REBUILD is still one line, just a different one: delete the marker (or the
// table and the marker) and restart.
//
// The schema probe stays as a cheap second skip — a database where no
// collection declares a relation field has nothing to derive — and it records
// completion too, because a pass over zero work is a completed pass. A
// relation field arriving later is handled by UpdateCollection's reindex and
// the write hooks, not by this.
//
// TWO SERVERS BOOTING AT ONCE is redundant, not corrupting, and the argument
// is worth writing down because the obvious fix — a startup lock — has worse
// failure modes than the thing it prevents (a crashed holder blocking boot).
//
// Both servers see no marker and both walk items. Each per-item write re-reads
// the blob inside its own transaction and replaces that item's rows wholesale,
// so two passes over one item converge on the same rows rather than doubling
// them. Whichever server finishes first writes the marker, and the marker is
// honest at that moment: a full pass DID complete. The other server's
// remaining writes re-derive rows that are already correct.
//
// The interleaving that WOULD corrupt — an older pass writing a stale blob
// over a newer one — is closed by the in-transaction re-read, not by the
// marker. That is the load-bearing part; the marker only decides whether a
// pass runs at all.
//
// PLAN-2857 U5 / TASK-2997.
func (s *Store) BackfillRelationLinks() (*BackfillRelationLinksResult, error) {
	result := &BackfillRelationLinksResult{}

	var marker string
	err := s.db.QueryRow(s.q(`SELECT value FROM platform_settings WHERE key = ?`), relationLinksBackfilledFlag).Scan(&marker)
	if err == nil && marker == "1" {
		result.Skipped = true
		return result, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("probe relation backfill marker: %w", err)
	}

	// Which collections declare a relation field, and what their relation keys
	// are. One pass, so the per-item work below is a map lookup rather than a
	// schema parse.
	relationKeysByCollection, err := s.collectionsWithRelationFields()
	if err != nil {
		return nil, err
	}
	if len(relationKeysByCollection) == 0 {
		// A pass over zero work is a completed pass.
		if err := s.markRelationLinksBackfilled(); err != nil {
			return nil, err
		}
		result.Skipped = true
		return result, nil
	}

	// IDS ONLY. The scan used to carry every item's `fields` blob, which put
	// the whole database's field JSON in memory before a single row was
	// written (codex round 6). It was never needed: each write re-reads the
	// blob under a lock, precisely because a snapshot written later is a
	// snapshot that can be stale. Three ids per row is ~100 bytes, so 100k
	// items is ~10MB rather than the sum of every blob.
	rows, err := s.db.Query(s.q(`
		SELECT id, workspace_id, collection_id
		FROM items
		WHERE deleted_at IS NULL
	`))
	if err != nil {
		return nil, fmt.Errorf("scan items for relation backfill: %w", err)
	}
	var items []itemRow
	for rows.Next() {
		var r itemRow
		if scanErr := rows.Scan(&r.id, &r.workspaceID, &r.collectionID); scanErr != nil {
			rows.Close()
			return nil, fmt.Errorf("scan relation backfill row: %w", scanErr)
		}
		items = append(items, r)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate relation backfill rows: %w", rowsErr)
	}
	rows.Close()

	// BATCHED BY WORKSPACE, in bounded chunks. One transaction per ITEM was
	// the first shape and codex round 5 was right to call it out: a 100k-item
	// database meant 100k transactions and 100k advisory-lock acquisitions in
	// a synchronous startup pause, which is connection churn nobody asked for.
	//
	// The other extreme — one transaction per workspace — is worse in a
	// different way: at the measured ~1.8s per 10k items, a 100k-item
	// workspace would hold its write lock for ~18 seconds at boot. Bounded
	// chunks keep both the transaction count and the lock hold time small, and
	// the per-chunk failure granularity is what the per-item version was
	// really buying.
	byWorkspace := map[string][]itemRow{}
	for _, it := range items {
		if _, ok := relationKeysByCollection[it.collectionID]; !ok {
			continue
		}
		byWorkspace[it.workspaceID] = append(byWorkspace[it.workspaceID], it)
	}
	for workspaceID, wsItems := range byWorkspace {
		for start := 0; start < len(wsItems); start += relationBackfillChunk {
			end := start + relationBackfillChunk
			if end > len(wsItems) {
				end = len(wsItems)
			}
			inserted, err := s.backfillRelationChunk(workspaceID, wsItems[start:end])
			if err != nil {
				return nil, err
			}
			result.ItemsScanned += end - start
			result.LinksInserted += inserted
		}
	}

	// ONLY NOW. Every earlier return path is a failure, and none of them
	// records completion — so an interrupted run is re-derived on the next
	// boot rather than skipped forever.
	if err := s.markRelationLinksBackfilled(); err != nil {
		return nil, err
	}
	return result, nil
}

// relationLinksBackfilledFlag is the platform_settings key recording that a
// FULL relation-index derivation has completed. Same mechanism the
// webhook-secret encryption migration uses for the same question.
const relationLinksBackfilledFlag = "relation_links_backfilled"

// markRelationLinksBackfilled records a completed pass.
func (s *Store) markRelationLinksBackfilled() error {
	if _, err := s.db.Exec(s.q(`
		INSERT INTO platform_settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
	`), relationLinksBackfilledFlag, "1", now()); err != nil {
		return fmt.Errorf("persist relation backfill marker: %w", err)
	}
	return nil
}

// collectionsWithRelationFields maps collection id -> its relation field keys,
// for every collection that declares at least one.
func (s *Store) collectionsWithRelationFields() (map[string]map[string]struct{}, error) {
	rows, err := s.db.Query(s.q(`SELECT id, schema FROM collections`))
	if err != nil {
		return nil, fmt.Errorf("scan collections for relation keys: %w", err)
	}
	defer rows.Close()
	out := map[string]map[string]struct{}{}
	for rows.Next() {
		var id, schemaJSON string
		if err := rows.Scan(&id, &schemaJSON); err != nil {
			return nil, fmt.Errorf("scan collection schema: %w", err)
		}
		keys := relationKeysFromSchemaJSON(schemaJSON)
		if len(keys) > 0 {
			out[id] = keys
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate collection schemas: %w", err)
	}
	return out, nil
}

// itemRow is one row of the backfill's initial scan — identity only. The blob
// is deliberately absent: every write re-reads it under a lock, so carrying it
// here would cost memory proportional to the database's total field JSON and
// buy a value that must be discarded anyway.
type itemRow struct{ id, workspaceID, collectionID string }

// relationBackfillChunk bounds one backfill transaction. Small enough that the
// workspace lock is never held long at boot, large enough that a big database
// does not pay a transaction per item.
const relationBackfillChunk = 500

// backfillRelationChunk indexes one bounded batch of items from ONE workspace,
// in a single transaction holding that workspace's lock.
func (s *Store) backfillRelationChunk(workspaceID string, batch []itemRow) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin relation backfill tx: %w", err)
	}
	// Belt and braces on top of the explicit rollbacks: a driver can return a
	// RECOVERABLE error from Commit, which no explicit path covers. A rollback
	// after a successful commit is a no-op.
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	// WORKSPACE LOCK FIRST, then each item row — the order every item write and
	// UpdateCollection use, so this cannot invert against them.
	//
	// The row lock alone serialises against concurrent ITEM writes but not
	// against a concurrent SCHEMA change, which is guarded a level up by this
	// lock: without it the backfill can read the old schema, watch
	// UpdateCollection reindex the collection, then write rows derived from the
	// shape it just replaced and commit last (codex rounds 3 and 4).
	if err := s.acquireWorkspaceSeqLock(tx, workspaceID); err != nil {
		return 0, err
	}

	inserted := 0
	for _, it := range batch {
		// LOCKED, not just re-read. The re-read closes the gap between the
		// SCAN and this transaction; the lock closes the gap between this read
		// and the replacement below. SQLite's write lock already serialises
		// and it does not take the clause here, hence the dialect gate — the
		// same shape UpdateCollection uses for its own re-read.
		lockedRead := `SELECT fields, collection_id FROM items WHERE id = ? AND deleted_at IS NULL`
		if s.dialect.Driver() == DriverPostgres {
			lockedRead += " FOR UPDATE"
		}
		var fields, collectionID string
		if err := tx.QueryRow(s.q(lockedRead), it.id).Scan(&fields, &collectionID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				// Deleted between the scan and now. Not a match any more.
				continue
			}
			return 0, fmt.Errorf("re-read item %s for backfill: %w", it.id, err)
		}

		// CALLED UNCONDITIONALLY, even when the blob carries no edges. Skipping
		// the zero-edge case was an optimisation that preserved exactly the
		// stale rows this pass exists to clear — replaceRelationLinks deletes
		// before it inserts, so that case is the one that must run.
		// The COUNT comes from the write itself. Deriving it from the
		// schema map captured before the pass would disagree with reality
		// whenever a schema changed underneath — reporting a number of rows
		// that were never written (codex round 6).
		n, err := s.replaceRelationLinks(tx, it.id, workspaceID, collectionID, fields)
		if err != nil {
			return 0, err
		}
		inserted += n
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit relation backfill tx: %w", err)
	}
	return inserted, nil
}
