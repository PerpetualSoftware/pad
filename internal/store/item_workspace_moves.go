package store

import (
	"database/sql"
	"fmt"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// item_workspace_moves accessors — cross-workspace copy/move provenance
// (PLAN-2357 DR-2). See migrations/077_item_workspace_moves.sql for the
// schema rationale, and models.ItemWorkspaceMove for the row semantics.

// itemWorkspaceMoveColumns is the fixed select list scanItemWorkspaceMove
// expects, in order.
const itemWorkspaceMoveColumns = `id, source_workspace_id, source_item_id,
	target_workspace_id, target_item_id, archived_source, source_seq,
	created_by, created_at`

// itemWorkspaceMoveOrder is the newest-first ordering.
//
// created_at leads because source_seq is only as trustworthy as whatever the
// caller passed: imports, backfills and hand-repaired rows can carry a high
// seq with an old created_at, and under a seq-primary order such a row
// silently becomes the head. created_at is UTC RFC3339 TEXT, so lexicographic
// order is chronological order.
//
// source_seq breaks the ties created_at cannot: it is second-precision, and
// archive → restore → move again inside one second is legal (DR-2a). Within a
// single second the higher source_seq is unambiguously the later move.
// COALESCE rather than "NULLS LAST" because SQLite and Postgres disagree on
// default NULL placement in DESC order; -1 sorts below every real seq on both.
// The COALESCE is defensive rather than load-bearing today — the one
// production query using this ordering reads archived rows only, and
// RecordItemWorkspaceMoveTx requires a seq on those — but it costs nothing and
// keeps the ordering total if the predicate ever widens.
//
// id is a final tiebreak so the ordering is total and the result set is
// stable across calls.
const itemWorkspaceMoveOrder = ` ORDER BY created_at DESC, COALESCE(source_seq, -1) DESC, id DESC`

// RecordItemWorkspaceMoveTx inserts one provenance row inside an existing
// transaction. It is tx-taking by design: the row must be written in the same
// transaction as the destination create (and, on a move, the source archive),
// so a rollback can never leave a pointer at an item that does not exist —
// or lose the pointer for a copy that committed (DR-9). There is deliberately
// no self-committing variant; nothing would legitimately call one.
//
// ID and CreatedAt are filled in when blank. Callers doing a move should pass
// the CreatedAt they used for the archive so both rows agree, and MUST set
// SourceSeq to the seq that archive assigned — without it two moves in the
// same second are unorderable (DR-2a).
//
// Returns the stored row, including any generated ID/CreatedAt.
func (s *Store) RecordItemWorkspaceMoveTx(tx *sql.Tx, m models.ItemWorkspaceMove) (*models.ItemWorkspaceMove, error) {
	for _, required := range []struct{ name, value string }{
		{"source_workspace_id", m.SourceWorkspaceID},
		{"source_item_id", m.SourceItemID},
		{"target_workspace_id", m.TargetWorkspaceID},
		{"target_item_id", m.TargetItemID},
		{"created_by", m.CreatedBy},
	} {
		if required.value == "" {
			return nil, fmt.Errorf("record item workspace move: %s is required", required.name)
		}
	}

	// SourceSeq is required exactly when the source was archived, and
	// forbidden otherwise. Both halves are load-bearing (DR-2a):
	//
	//   archived without a seq — two moves of one source inside the same
	//   second become unorderable and the moved-to pointer picks arbitrarily,
	//   which is the precise failure this column exists to prevent. The
	//   ordering would silently fall through to the ID tiebreak, so the
	//   damage is invisible rather than loud.
	//
	//   a copy WITH a seq — a plain copy never archives, so it has no source
	//   workspace seq to record; a value there is a caller bug (most likely a
	//   move/copy mix-up) and would put a copy row into the move ordering if
	//   archived_source were ever recomputed from it.
	if m.ArchivedSource && m.SourceSeq == nil {
		return nil, fmt.Errorf("record item workspace move: source_seq is required when archived_source is set")
	}
	if !m.ArchivedSource && m.SourceSeq != nil {
		return nil, fmt.Errorf("record item workspace move: source_seq must be nil for a plain copy (archived_source is false)")
	}

	if m.ID == "" {
		m.ID = newID()
	}
	if m.CreatedAt == "" {
		m.CreatedAt = now()
	}

	var seq interface{}
	if m.SourceSeq != nil {
		seq = *m.SourceSeq
	}

	if _, err := tx.Exec(s.q(`
		INSERT INTO item_workspace_moves (
			id, source_workspace_id, source_item_id,
			target_workspace_id, target_item_id,
			archived_source, source_seq, created_by, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`), m.ID, m.SourceWorkspaceID, m.SourceItemID,
		m.TargetWorkspaceID, m.TargetItemID,
		s.dialect.BoolToInt(m.ArchivedSource), seq, m.CreatedBy, m.CreatedAt,
	); err != nil {
		return nil, fmt.Errorf("record item workspace move: %w", err)
	}

	return &m, nil
}

// ListArchivedItemWorkspaceMovesBySource returns at most `limit` MOVE rows for
// one source — rows with archived_source true — newest first.
//
// It serves the one consumer that wants moves and only moves: the
// archived-source "moved to" pointer (PLAN-2357 DR-2a / TASK-2359). The
// archived_source predicate and the LIMIT are both pushed into SQL rather than
// applied by the caller, because the caller bounds how many destinations it
// will AUTHORIZE, and that bound is only meaningful if the row set it
// authorizes over is bounded too. Filtering in Go over an unfiltered forward
// lookup instead would leave a source with a long tail of plain COPIES
// loading, scanning and allocating every one of them on every read of the
// source, while none of them can ever contribute to the result. A broad
// copies-and-moves forward lookup existed alongside this one until TASK-2374
// and was deleted unused; a "copied from" affordance that wanted it back would
// need its own dual-sided disclosure design (DR-20) before the query matters.
//
// What the LIMIT bounds is rows RETURNED — materialized, scanned into structs,
// and handed to the caller's per-row authorization. It is not a promise about
// the planner. idx_item_workspace_moves_moved_to is partial on
// `archived_source = 1` and ordered `(source_item_id, source_seq DESC)`, while
// the predicate here is a BOUND PARAMETER (dialect-dependent 1 vs TRUE) and
// the ordering leads with created_at, so neither engine is obliged to use it
// and the sort may well be materialized before the LIMIT applies. Rows per
// source are inherently tiny — one per copy or move of a single item — so the
// equality lookup on idx_item_workspace_moves_source is the part that matters.
//
// The ordering is itemWorkspaceMoveOrder, and leading with created_at rather
// than source_seq is a deliberate refusal to specialize. Ordering
// archived-only rows by `source_seq DESC` alone would match the partial
// index's columns and reads as strictly better — source_seq is NOT NULL for a
// move (RecordItemWorkspaceMoveTx enforces it) and is workspace-A-monotonic
// when the copy path supplies the seq the archive assigned — which the one
// production writer, CopyItemAcrossWorkspaces, does. But the STORE cannot
// enforce that it is that seq: the column takes whatever the caller passes, no
// constraint ties it to the source workspace's sequence, and imports,
// backfills and hand-repaired rows can carry a high seq with an old
// created_at. Under a seq-primary order such a row silently becomes the head —
// and with the cap, evicts the genuinely newest destination. Leading with
// created_at makes the pathological case merely mis-tiebroken instead of
// inverted. source_seq stays as the second term, which is the tie DR-2a
// actually needs it for.
//
// A non-positive limit returns no rows rather than every row: this is a bound,
// and a caller that forgot to set one should get the safe answer.
func (s *Store) ListArchivedItemWorkspaceMovesBySource(sourceItemID string, limit int) ([]models.ItemWorkspaceMove, error) {
	moves := []models.ItemWorkspaceMove{}
	if limit <= 0 {
		return moves, nil
	}

	rows, err := s.db.Query(s.q(`
		SELECT `+itemWorkspaceMoveColumns+`
		FROM item_workspace_moves
		WHERE source_item_id = ? AND archived_source = ?`+itemWorkspaceMoveOrder+`
		LIMIT ?`,
	), sourceItemID, s.dialect.BoolToInt(true), limit)
	if err != nil {
		return nil, fmt.Errorf("list archived item workspace moves by source: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		m, err := scanItemWorkspaceMove(rows)
		if err != nil {
			return nil, fmt.Errorf("list archived item workspace moves by source: %w", err)
		}
		moves = append(moves, *m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list archived item workspace moves by source: %w", err)
	}
	return moves, nil
}

// scanner (api_tokens.go) is satisfied by both *sql.Row and *sql.Rows.
func scanItemWorkspaceMove(sc scanner) (*models.ItemWorkspaceMove, error) {
	var (
		m         models.ItemWorkspaceMove
		archived  interface{}
		sourceSeq sql.NullInt64
	)
	if err := sc.Scan(
		&m.ID, &m.SourceWorkspaceID, &m.SourceItemID,
		&m.TargetWorkspaceID, &m.TargetItemID,
		&archived, &sourceSeq, &m.CreatedBy, &m.CreatedAt,
	); err != nil {
		return nil, err
	}
	// SQLite hands back INTEGER 0/1, Postgres a native bool.
	m.ArchivedSource = scanBool(archived)
	if sourceSeq.Valid {
		seq := sourceSeq.Int64
		m.SourceSeq = &seq
	}
	return &m, nil
}
