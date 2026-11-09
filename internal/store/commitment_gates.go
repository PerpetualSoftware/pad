package store

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/schedule"
)

// Commitment gates (TASK-4353, DOC-4348 C7, store level).
//
// A gate holds one gated commitment behind one blocker item. It is one row per
// (gated schedule, blocker item). The blocker is either a commitment, in which
// case the gate is held against the ONE occurrence that was outstanding when the
// gate was written (its occurrence key, fixed then), or an item with no
// commitment, which is judged by its status. A gate opens once and never closes,
// so a recurring blocker moving on to its next occurrence cannot re-block work
// that already went ahead. Per-occurrence gating, each weekly child waiting on
// its matching blocker occurrence, is not built.
//
// This is for commitment gates only. A plain blocks link between items means
// what it always meant, and nothing here changes it.
//
// LOCK ORDER. A gate write is a link write: it runs pinCommitmentOpt with the
// gated item as the root and the blocker as a second seed when the blocker has a
// commitment. That is the root item (FOR NO KEY UPDATE), the workspace, the
// component read after the workspace lock, the component's schedules ascending
// by id, then their reminders, and then the same re-validation under those locks.
// No child item row is locked, so a resolve of the blocker (which holds the
// blocker item and waits for the workspace) never needs anything a gate write
// holds. The gate rows themselves are only touched under the workspace lock,
// except the one-way write that records a gate opening (opened_at), which is a
// single statement on one gate row and waits for nothing.

const (
	// maxGatesPerSchedule bounds how many blockers one gated schedule can have.
	maxGatesPerSchedule = 50
	// maxGateClosure bounds the gate edges read below a new gate's gated item to
	// find a cycle, so the check costs a bounded read however large the workspace.
	maxGateClosure = 500
)

// CreateScheduleGate gates blockedItemID's commitment behind blockerItemID. Both
// ends need edit; access holds the caller's decision, and an end it does not
// make readable, or that is missing, soft-deleted or in another workspace, is
// ErrScheduleLinkNotFound. The blocked item needs a commitment; the blocker need
// not have one. The blocker's outstanding occurrence is fixed here.
func (s *Store) CreateScheduleGate(workspaceID, blockedItemID, blockerItemID string, access models.CommitmentAccess) (*models.ScheduleGate, error) {
	if !access.Readable[blockedItemID] || !access.Readable[blockerItemID] {
		return nil, ErrScheduleLinkNotFound
	}
	if !access.Editable[blockedItemID] || !access.Editable[blockerItemID] {
		return nil, ErrLinkEditRequired
	}
	if blockedItemID == blockerItemID {
		return nil, &LinkRefusedError{Code: "link_self"}
	}
	// The blocker joins the pin as a seed when it has a commitment, so its
	// schedule row is locked in id order with the rest. This read goes through the
	// pool, before the transaction holds a connection.
	var extra []string
	if has, err := s.itemHasSchedule(workspaceID, blockerItemID); err != nil {
		return nil, err
	} else if has {
		extra = append(extra, blockerItemID)
	}
	budget := s.newLinkBudget()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("create schedule gate: %w", err)
	}
	defer tx.Rollback()
	if err := s.setTxTimeoutsCtx(budget.ctx, tx, linkTxBudget, linkTxBudget); err != nil {
		return nil, err
	}
	c, err := s.pinCommitmentTx(tx, workspaceID, "", nil, false, blockedItemID, extra...)
	if errors.Is(err, ErrScheduleItemGone) {
		return nil, ErrScheduleLinkNotFound
	}
	if err != nil {
		return nil, err
	}
	var blocked, blocker *models.ItemSchedule
	for _, sc := range c.scheds {
		switch sc.ItemID {
		case blockedItemID:
			blocked = sc
		case blockerItemID:
			blocker = sc
		}
	}
	if blocked == nil {
		return nil, ErrScheduleLinkNotFound
	}
	var live int
	if err := tx.QueryRow(s.q(`SELECT COUNT(*) FROM items WHERE id=? AND workspace_id=? AND deleted_at IS NULL`), blockerItemID, workspaceID).Scan(&live); err != nil {
		return nil, fmt.Errorf("read gate blocker: %w", err)
	} else if live == 0 {
		return nil, ErrScheduleLinkNotFound
	}
	// The blocker's occurrence outstanding now. The schedule row is locked when
	// the blocker was a seed and, in any case, every writer of it holds the
	// workspace lock this transaction holds.
	var blockerSchedule any
	key := ""
	if blocker != nil && c.live[blocker.ID] {
		blockerSchedule = blocker.ID
		loc, err := s.scheduleViewZone(tx, blocker)
		if err != nil {
			return nil, err
		}
		if key, err = outstandingKey(blocker, loc); err != nil {
			return nil, err
		}
	}
	var held int
	if err := tx.QueryRow(s.q(`SELECT COUNT(*) FROM schedule_gates WHERE blocked_schedule_id=?`), blocked.ID).Scan(&held); err != nil {
		return nil, fmt.Errorf("count schedule gates: %w", err)
	} else if held >= maxGatesPerSchedule {
		return nil, &LinkRefusedError{Code: "link_limit"}
	}
	existing, err := s.gateClosureTx(tx, workspaceID, blockedItemID)
	if err != nil {
		return nil, err
	}
	add := schedule.GateEdge{Blocker: schedule.Node{Schedule: blockerItemID}, Blocked: schedule.Node{Schedule: blockedItemID}}
	if err := schedule.CheckGateAdd(existing, add); err != nil {
		return nil, redactLinkError(err)
	}
	g := &models.ScheduleGate{ID: newID(), BlockedItemID: blockedItemID, BlockerItemID: blockerItemID, Outcome: models.GateOutcomePending, CreatedAt: nowNano()}
	if _, err := tx.Exec(s.q(`INSERT INTO schedule_gates (id,workspace_id,blocked_schedule_id,blocker_item_id,blocker_schedule_id,blocker_occurrence_key,created_at) VALUES (?,?,?,?,?,?,?)`),
		g.ID, workspaceID, blocked.ID, blockerItemID, blockerSchedule, key, g.CreatedAt); err != nil {
		if isUniqueViolation(err) {
			return nil, &LinkRefusedError{Code: "gate_duplicate"}
		}
		return nil, fmt.Errorf("insert schedule gate: %w", err)
	}
	if err := budget.commit(tx); err != nil {
		return nil, err
	}
	return g, nil
}

// itemHasSchedule reports whether the item has a schedule row in the workspace,
// whether or not the item is soft-deleted (the pin judges liveness).
func (s *Store) itemHasSchedule(workspaceID, itemID string) (bool, error) {
	var n int
	if err := s.db.QueryRow(s.q(`SELECT COUNT(*) FROM item_schedules WHERE item_id=? AND workspace_id=?`), itemID, workspaceID).Scan(&n); err != nil {
		return false, fmt.Errorf("read blocker schedule: %w", err)
	}
	return n > 0, nil
}

// gateClosureTx reads the gate edges below `from`: every gate whose blocker is
// `from`, then every gate whose blocker is one of the items they gate, and so on.
// A cycle through a new edge into `from` can only run along these, so they are
// all the cycle check needs. Past maxGateClosure edges it refuses. The caller
// holds the workspace lock, which every gate writer takes.
func (s *Store) gateClosureTx(tx *sql.Tx, workspaceID, from string) ([]schedule.GateEdge, error) {
	var edges []schedule.GateEdge
	seen := map[string]bool{from: true}
	for frontier := []string{from}; len(frontier) > 0; {
		args := []any{workspaceID}
		for _, id := range frontier {
			args = append(args, id)
		}
		rows, err := tx.Query(s.q(`SELECT g.blocker_item_id, sc.item_id FROM schedule_gates g
 JOIN item_schedules sc ON sc.id=g.blocked_schedule_id
 WHERE g.workspace_id=? AND g.blocker_item_id IN (`+placeholders(len(frontier))+`) ORDER BY g.id LIMIT ?`), append(args, maxGateClosure+1)...)
		if err != nil {
			return nil, fmt.Errorf("read gate closure: %w", err)
		}
		frontier = nil
		for rows.Next() {
			var blocker, blocked string
			if err := rows.Scan(&blocker, &blocked); err != nil {
				rows.Close()
				return nil, err
			}
			edges = append(edges, schedule.GateEdge{Blocker: schedule.Node{Schedule: blocker}, Blocked: schedule.Node{Schedule: blocked}})
			if !seen[blocked] {
				seen[blocked] = true
				frontier = append(frontier, blocked)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		if len(edges) > maxGateClosure {
			return nil, &LinkRefusedError{Code: "link_limit"}
		}
	}
	return edges, nil
}

// DeleteScheduleGate removes a gate. It needs edit on both ends, as a link does,
// and works whether or not either end is soft-deleted: an end absent from
// access.Readable, or a gate that does not exist, is ErrScheduleLinkNotFound.
func (s *Store) DeleteScheduleGate(workspaceID, gateID string, access models.CommitmentAccess) error {
	var blocked, blocker string
	var blockerSchedule sql.NullString
	err := s.db.QueryRow(s.q(`SELECT sc.item_id, g.blocker_item_id, g.blocker_schedule_id FROM schedule_gates g
 JOIN item_schedules sc ON sc.id=g.blocked_schedule_id WHERE g.id=? AND g.workspace_id=?`), gateID, workspaceID).Scan(&blocked, &blocker, &blockerSchedule)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrScheduleLinkNotFound
	} else if err != nil {
		return fmt.Errorf("read schedule gate: %w", err)
	}
	if !readableLinkEnds(access, blocked, blocker) {
		return ErrScheduleLinkNotFound
	}
	if !access.Editable[blocked] || !access.Editable[blocker] {
		return ErrLinkEditRequired
	}
	var extra []string
	if blockerSchedule.Valid {
		extra = append(extra, blocker)
	}
	budget := s.newLinkBudget()
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("delete schedule gate: %w", err)
	}
	defer tx.Rollback()
	if err := s.setTxTimeoutsCtx(budget.ctx, tx, linkTxBudget, linkTxBudget); err != nil {
		return err
	}
	// Same pin as a link delete: deleted ends are found, and never live.
	if _, err := s.pinCommitmentOpt(tx, workspaceID, "", nil, true, true, blocked, extra...); errors.Is(err, ErrScheduleItemGone) || errors.Is(err, ErrScheduleNotFound) {
		return ErrScheduleLinkNotFound
	} else if err != nil {
		return err
	}
	res, err := tx.Exec(s.q(`DELETE FROM schedule_gates WHERE id=? AND workspace_id=?`), gateID, workspaceID)
	if err != nil {
		return fmt.Errorf("delete schedule gate: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrScheduleLinkNotFound
	}
	return budget.commit(tx)
}
