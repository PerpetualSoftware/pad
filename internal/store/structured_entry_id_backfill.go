package store

import (
	"fmt"
	"log/slog"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// structuredEntryIDBackfillBatch bounds one keyset page of the backfill.
const structuredEntryIDBackfillBatch = 200

// BackfillStructuredEntryIDsResult reports one run.
type BackfillStructuredEntryIDsResult struct {
	RowsRepaired int
}

// BackfillStructuredEntryIDs gives every implementation note and decision-log
// entry already stored without a usable, unique id a persisted one
// (BUG-2788), via models.EnsureStructuredEntryIDs. New writes cannot create
// such an entry (the append helpers and workspace import repair the blob), so
// this drains the rows written before that.
//
// NO seq bump, NO updated_at, NO activity row, NO outbox event, lead-ruled on
// BUG-2788's trail. It adds an id nobody authored and changes no content, and
// the premise that makes a silent write safe is measured, not assumed:
//
//   - The one thing a silent change to a reserved key could break is BUG-3163's
//     full-`fields` carry check, which refuses an update whose
//     implementation_notes / decision_log differ from the stored value. So the
//     question is who sends a FULL `fields` update carrying those keys from a
//     cache. Census at main 22520505: web — 0 of 20 `items.update` call sites
//     (payloads are fields_patch, tags, title, content, sort_order or
//     assignment columns; webmcp's full `fields` is create-only); CLI — only
//     legacyAppendNote / legacyAppendDecision, which run solely against a
//     server WITHOUT item_field_append, i.e. one that never runs this
//     backfill; remote MCP — `payload["fields"]` only in mapItemCreate;
//     pad-mobile — its native code makes no item writes.
//   - BOUNDARY: third-party API scripts cannot be enumerated. For them this
//     adds no new class of refusal: the carry check already refuses a stale
//     cached blob the moment any note or decision is appended between that
//     client's read and its write, so a full-fields read-modify-write client
//     is exposed to exactly this by ordinary appends.
//
// If a client that sends reserved keys in a full `fields` update is ever
// added, this premise has to be revisited before this runs silently again.
//
// Each UPDATE is conditional on BOTH the row's seq and its fields text as
// read, so a concurrent write is never overwritten, including one that
// changed fields without moving seq; that row is picked up by the next boot.
// A repaired blob changes in its ids and in formatting only (key order and
// spacing may differ from a SQLite row's stored text); every value is equal. Idempotent: a
// repaired blob needs nothing, so a second run rewrites no row. It selects by
// content on every boot, like the bidi filename backfill.
func (s *Store) BackfillStructuredEntryIDs() (*BackfillStructuredEntryIDsResult, error) {
	res := &BackfillStructuredEntryIDsResult{}

	fieldsExpr := "fields"
	if s.dialect.Driver() == DriverPostgres {
		fieldsExpr = "fields::text"
	}
	// A cheap prefilter; EnsureStructuredEntryIDs is the actual decision.
	query := s.q(`SELECT id, workspace_id, ` + fieldsExpr + `, seq FROM items
		WHERE id > ? AND (` + fieldsExpr + ` LIKE ? OR ` + fieldsExpr + ` LIKE ?)
		ORDER BY id LIMIT ?`)

	lastID := ""
	for {
		type row struct {
			id, workspaceID, fields string
			seq                     int64
		}
		rows, err := s.db.Query(query, lastID, "%implementation_notes%", "%decision_log%", structuredEntryIDBackfillBatch)
		if err != nil {
			return res, fmt.Errorf("backfill structured entry ids: select: %w", err)
		}
		var batch []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.workspaceID, &r.fields, &r.seq); err != nil {
				_ = rows.Close()
				return res, fmt.Errorf("backfill structured entry ids: scan: %w", err)
			}
			batch = append(batch, r)
		}
		if err := rows.Close(); err != nil {
			return res, err
		}
		if err := rows.Err(); err != nil {
			return res, err
		}
		if len(batch) == 0 {
			return res, nil
		}
		for _, r := range batch {
			lastID = r.id
			fixed, changed, err := models.EnsureStructuredEntryIDs(r.fields)
			if err != nil {
				return res, fmt.Errorf("backfill structured entry ids: item %s: %w", r.id, err)
			}
			if !changed {
				continue
			}
			result, err := s.db.Exec(s.q(`UPDATE items SET fields = ? WHERE id = ? AND seq = ? AND `+fieldsExpr+` = ?`), fixed, r.id, r.seq, r.fields)
			if err != nil {
				return res, fmt.Errorf("backfill structured entry ids: update %s: %w", r.id, err)
			}
			n, err := result.RowsAffected()
			if err != nil {
				return res, fmt.Errorf("backfill structured entry ids: rows affected %s: %w", r.id, err)
			}
			if n == 0 {
				continue // written concurrently; the next boot sees the new blob
			}
			res.RowsRepaired++
			slog.Info("structured entry ids persisted", "item_id", r.id, "workspace_id", r.workspaceID)
		}
		if len(batch) < structuredEntryIDBackfillBatch {
			return res, nil
		}
	}
}
