package store

import (
	"errors"
	"fmt"
)

// ItemSortUpdate is one entry of a bulk reorder (TASK-3517): the item and the
// sort_order it should hold.
type ItemSortUpdate struct {
	ItemID    string
	SortOrder int
}

// ItemSortResult is what one entry did: the value it held before, the value
// it holds now and its seq afterwards. Changed is false when the item already
// held SortOrder, in which case nothing was written for it.
type ItemSortResult struct {
	ItemID  string
	From    int
	To      int
	Seq     int64
	Changed bool
}

// ErrItemSortNotFound reports that an UpdateItemSortOrders entry named no live
// item in the workspace, so the whole batch was rolled back.
var ErrItemSortNotFound = errors.New("item sort: item not found in workspace")

// UpdateItemSortOrders sets sort_order for a set of items in ONE transaction:
// every entry lands or none does (TASK-3517). It replaces the web reorder's
// one-PATCH-per-row loop, which left a half-applied order on the server when a
// write in the middle was refused.
//
// The caller has already checked that each item is visible to, and editable
// by, the requester. The store checks only that each entry names a live item
// in workspaceID, under the workspace seq lock, and rolls the batch back with
// ErrItemSortNotFound otherwise (an item deleted between the check and the
// write).
//
// A changed row gets what the single-item PATCH gives it: updated_at, a fresh
// workspace seq (so delta-sync clients see the reorder) and an item.updated
// outbox event, stamped with batchID so delivery can fold the batch under one
// header. A row that already holds its value is left alone.
func (s *Store) UpdateItemSortOrders(workspaceID string, updates []ItemSortUpdate, batchID string) ([]ItemSortResult, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Serialize seq assignment per workspace (Postgres); held to COMMIT.
	if err := s.acquireWorkspaceSeqLock(tx, workspaceID); err != nil {
		return nil, err
	}

	stmt, err := tx.Prepare(s.q("UPDATE items SET sort_order = ?, updated_at = ?, seq = " + nextWorkspaceSeqSubquery + " WHERE id = ? AND workspace_id = ? AND deleted_at IS NULL"))
	if err != nil {
		return nil, fmt.Errorf("prepare item sort update: %w", err)
	}
	defer stmt.Close()

	ts := now()
	results := make([]ItemSortResult, 0, len(updates))
	for _, u := range updates {
		before, err := s.getItemTx(tx, u.ItemID)
		if err != nil {
			return nil, err
		}
		if before == nil || before.WorkspaceID != workspaceID {
			return nil, fmt.Errorf("%w: %s", ErrItemSortNotFound, u.ItemID)
		}
		if before.SortOrder == u.SortOrder {
			results = append(results, ItemSortResult{ItemID: u.ItemID, From: before.SortOrder, To: u.SortOrder, Seq: before.Seq})
			continue
		}
		res, err := stmt.Exec(u.SortOrder, ts, workspaceID, u.ItemID, workspaceID)
		if err != nil {
			return nil, fmt.Errorf("update sort order for %s: %w", u.ItemID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("update sort order for %s: rows affected: %w", u.ItemID, err)
		}
		if n != 1 {
			return nil, fmt.Errorf("%w: %s", ErrItemSortNotFound, u.ItemID)
		}
		after, err := s.getItemTx(tx, u.ItemID)
		if err != nil {
			return nil, err
		}
		if after == nil {
			return nil, fmt.Errorf("%w: %s", ErrItemSortNotFound, u.ItemID)
		}
		if err := s.emitItemUpdateEventsTx(tx, before, after, false, "", "", batchID, false); err != nil {
			return nil, err
		}
		results = append(results, ItemSortResult{ItemID: u.ItemID, From: before.SortOrder, To: after.SortOrder, Seq: after.Seq, Changed: true})
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return results, nil
}
