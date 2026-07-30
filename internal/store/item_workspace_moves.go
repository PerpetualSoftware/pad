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

// itemWorkspaceMoveOrder is the shared newest-first ordering.
//
// created_at leads because the forward lookup returns copies AND moves, and
// copies carry no source_seq at all — ordering by seq first would bury every
// copy behind every move regardless of recency. created_at is UTC RFC3339
// TEXT, so lexicographic order is chronological order.
//
// source_seq breaks the ties created_at cannot: it is second-precision, and
// archive → restore → move again inside one second is legal (DR-2a). Within a
// single second the higher source_seq is unambiguously the later move.
// COALESCE rather than "NULLS LAST" because SQLite and Postgres disagree on
// default NULL placement in DESC order; -1 sorts below every real seq on both.
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

// ListItemWorkspaceMovesBySource returns every destination one source item was
// copied or moved to, newest first.
//
// A SET, not a row: one source can be copied into several workspaces, and can
// additionally be moved after being restored. The caller decides how to render
// multiples. A caller resolving the archived-source "moved to" pointer wants
// the first entry with ArchivedSource true — plain-copy rows never feed that
// pointer (DR-2a).
func (s *Store) ListItemWorkspaceMovesBySource(sourceItemID string) ([]models.ItemWorkspaceMove, error) {
	rows, err := s.db.Query(s.q(`
		SELECT `+itemWorkspaceMoveColumns+`
		FROM item_workspace_moves
		WHERE source_item_id = ?`+itemWorkspaceMoveOrder,
	), sourceItemID)
	if err != nil {
		return nil, fmt.Errorf("list item workspace moves by source: %w", err)
	}
	defer rows.Close()

	moves := []models.ItemWorkspaceMove{}
	for rows.Next() {
		m, err := scanItemWorkspaceMove(rows)
		if err != nil {
			return nil, fmt.Errorf("list item workspace moves by source: %w", err)
		}
		moves = append(moves, *m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list item workspace moves by source: %w", err)
	}
	return moves, nil
}

// GetItemWorkspaceMoveByTarget returns where a destination item came from, or
// (nil, nil) when the item was not produced by a cross-workspace copy.
//
// One row, unlike the forward lookup, and that is enforced rather than
// assumed: a destination item is created by exactly one copy operation whose
// provenance row is written in the same transaction, and
// uq_item_workspace_moves_target makes a second row naming the same target
// impossible. No ordering or LIMIT is needed as a result.
func (s *Store) GetItemWorkspaceMoveByTarget(targetItemID string) (*models.ItemWorkspaceMove, error) {
	row := s.db.QueryRow(s.q(`
		SELECT `+itemWorkspaceMoveColumns+`
		FROM item_workspace_moves
		WHERE target_item_id = ?`,
	), targetItemID)

	m, err := scanItemWorkspaceMove(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get item workspace move by target: %w", err)
	}
	return m, nil
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
