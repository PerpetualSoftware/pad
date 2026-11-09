package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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

// ---- evaluation ----

// The outcome of a gate is the outcome of the blocker occurrence it was fixed to.
//
//   - A blocker with a commitment and a fixed occurrence key is judged by that
//     occurrence's recorded state. `done` wins and is succeeded, even over a
//     recorded miss: the work happened and the miss stays on record. `skipped`
//     is skipped. `lapsed`, and a passed-over `missed`, are lapsed. A recorded
//     deadline miss under deadline_behavior `lapses` is lapsed whether or not the
//     occurrence was closed; under `stays_owed` the occurrence is still owed, so
//     it stays pending and a later Done opens the gate. A `moved` occurrence, or
//     one with no row, is pending.
//   - A blocker with no commitment, or whose commitment had ended when the gate
//     was written (an empty key: there is no occurrence to fix), is judged by the
//     item's status in its collection: a terminal status that is not abandoned
//     succeeded, an abandoned one is cancelled, anything else is pending.
//   - A blocker removed from the workspace (soft-deleted) that has not succeeded
//     is cancelled: it can no longer deliver, and the child stays blocked for a
//     decision. A restore puts it back to what it was.
//
// Only succeeded opens a gate, once and for all. Every other outcome leaves the
// child blocked for a decision (or waiting, while pending): a cancelled or
// lapsed blocker never starts the child's work.

// gateRow is a stored gate with what its outcome is read from.
type gateRow struct {
	models.ScheduleGate
	blockerSchedule sql.NullString
	key             string
}

// gateEvaluator judges gates of one workspace. The workspace's done filters are
// read through the pool the first time a status decides a gate.
type gateEvaluator struct {
	s           *Store
	workspaceID string
	filters     []collectionDoneFilter
	loaded      bool
}

func (e *gateEvaluator) outcome(g gateRow) (schedule.Outcome, error) {
	if g.blockerSchedule.Valid && g.key != "" {
		out, decided, err := e.s.occurrenceOutcome(g.blockerSchedule.String, g.key)
		if err != nil || decided {
			return out, err
		}
		gone, err := e.itemRemoved(g.BlockerItemID)
		if err != nil || gone {
			return schedule.OutcomeCancelled, err
		}
		return schedule.OutcomePending, nil
	}
	return e.statusOutcome(g.BlockerItemID)
}

// occurrenceOutcome reads the state of the occurrence a key names. decided is
// false when the occurrence is still open: pending, unless a miss under
// `lapses` was recorded.
func (s *Store) occurrenceOutcome(scheduleID, key string) (out schedule.Outcome, decided bool, err error) {
	col, val := "occurrence_wall", key
	if i := strings.IndexByte(key, '|'); i >= 0 {
		col, val = "occurrence_at", key[:i]
	}
	var state string
	switch err := s.db.QueryRow(s.q(`SELECT state FROM schedule_occurrences WHERE schedule_id=? AND `+col+`=? ORDER BY resolved_at DESC LIMIT 1`), scheduleID, val).Scan(&state); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return "", false, fmt.Errorf("read blocker occurrence: %w", err)
	}
	switch state {
	case "done":
		return schedule.OutcomeSucceeded, true, nil
	case "skipped":
		return schedule.OutcomeSkipped, true, nil
	case "lapsed", "missed":
		return schedule.OutcomeLapsed, true, nil
	}
	var misses int
	if err := s.db.QueryRow(s.q(`SELECT COUNT(*) FROM schedule_changes c JOIN item_schedules sc ON sc.id=c.schedule_id
 WHERE c.schedule_id=? AND c.change_type='missed_recorded' AND c.role='deadline' AND c.occurrence_key=? AND sc.deadline_behavior='lapses'`), scheduleID, key).Scan(&misses); err != nil {
		return "", false, fmt.Errorf("read blocker miss: %w", err)
	}
	if misses > 0 {
		return schedule.OutcomeLapsed, true, nil
	}
	return schedule.OutcomePending, false, nil
}

// itemRemoved reports whether the blocker item is soft-deleted (or gone).
func (e *gateEvaluator) itemRemoved(itemID string) (bool, error) {
	var removed int
	err := e.s.db.QueryRow(e.s.q(`SELECT CASE WHEN deleted_at IS NULL THEN 0 ELSE 1 END FROM items WHERE id=? AND workspace_id=?`), itemID, e.workspaceID).Scan(&removed)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	return removed == 1, err
}

// statusOutcome judges an item by its done-field value, with the collection's
// own terminal and abandoned options (the workspace defaults where it declares
// none), as the rest of the product reads them.
func (e *gateEvaluator) statusOutcome(itemID string) (schedule.Outcome, error) {
	var collectionID, fields string
	var removed int
	err := e.s.db.QueryRow(e.s.q(`SELECT collection_id, fields, CASE WHEN deleted_at IS NULL THEN 0 ELSE 1 END FROM items WHERE id=? AND workspace_id=?`), itemID, e.workspaceID).Scan(&collectionID, &fields, &removed)
	if errors.Is(err, sql.ErrNoRows) {
		return schedule.OutcomeCancelled, nil
	} else if err != nil {
		return "", fmt.Errorf("read gate blocker: %w", err)
	}
	if !e.loaded {
		e.filters, e.loaded = e.s.doneFiltersForWorkspace(e.workspaceID), true
	}
	doneKey, terminals, abandoned := "status", models.DefaultTerminalStatuses, models.DefaultAbandonedStatuses()
	if len(e.filters) > 0 {
		known := false
		for _, f := range e.filters {
			if f.collectionID == collectionID {
				doneKey, terminals, abandoned, known = f.doneKey, f.values, f.abandoned, true
				break
			}
		}
		if !known {
			terminals, abandoned = nil, nil
		}
	}
	var values map[string]any
	_ = json.Unmarshal([]byte(fields), &values)
	status, _ := values[doneKey].(string)
	switch {
	case containsFold(abandoned, status):
		return schedule.OutcomeCancelled, nil
	case containsFold(terminals, status):
		return schedule.OutcomeSucceeded, nil
	case removed == 1:
		return schedule.OutcomeCancelled, nil
	}
	return schedule.OutcomePending, nil
}

// ScheduleGates evaluates the gates of an item's commitment on read and opens,
// once and for good, each whose blocker succeeded. The caller sees only the
// gates whose blocker it can read, and only if it can read the gated item: a
// gate with a hidden blocker is left out of the state, the detail and the
// unresolved list alike, so the item reads as it would with no such gate, and
// nothing is evaluated on that reader's behalf.
//
// Opening is one statement on one gate row, outside any transaction and without
// the workspace lock: it is monotone (NULL to a time, once), so a concurrent
// reader or a gate delete can only make it a no-op.
func (s *Store) ScheduleGates(workspaceID, itemID string, access models.CommitmentAccess) (*models.ScheduleGateView, error) {
	if !access.Readable[itemID] {
		return nil, ErrScheduleNotFound
	}
	rows, err := s.db.Query(s.q(`SELECT g.id,g.blocker_item_id,g.blocker_schedule_id,g.blocker_occurrence_key,g.opened_at,g.created_at FROM schedule_gates g
 JOIN item_schedules sc ON sc.id=g.blocked_schedule_id WHERE sc.item_id=? AND g.workspace_id=? ORDER BY g.id`), itemID, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("read schedule gates: %w", err)
	}
	var gates []gateRow
	for rows.Next() {
		g := gateRow{}
		var opened sql.NullString
		g.BlockedItemID = itemID
		if err := rows.Scan(&g.ID, &g.BlockerItemID, &g.blockerSchedule, &g.key, &opened, &g.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		if opened.Valid {
			g.OpenedAt = &opened.String
		}
		gates = append(gates, g)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	view := &models.ScheduleGateView{State: models.GateStateReleased, Gates: []models.ScheduleGate{}}
	eval := &gateEvaluator{s: s, workspaceID: workspaceID}
	outcomes := map[schedule.Node]schedule.Outcome{}
	for _, g := range gates {
		if !access.Readable[g.BlockerItemID] {
			continue
		}
		outcome := schedule.OutcomeSucceeded
		if g.OpenedAt == nil {
			if outcome, err = eval.outcome(g); err != nil {
				return nil, err
			}
			if outcome == schedule.OutcomeSucceeded {
				if g.OpenedAt, err = s.openGate(g.ID); err != nil {
					return nil, err
				}
				if g.OpenedAt == nil {
					continue // deleted while it was read
				}
			}
		}
		g.Outcome = string(outcome)
		outcomes[schedule.Node{Schedule: g.BlockerItemID}] = outcome
		view.Gates = append(view.Gates, g.ScheduleGate)
	}
	res := schedule.EvaluateGates(outcomes)
	view.State = string(res.State)
	for _, n := range res.Unresolved {
		view.Unresolved = append(view.Unresolved, n.Schedule)
	}
	return view, nil
}

// openGate records that a gate opened, if nobody did first, and returns the
// recorded time; nil when the gate no longer exists.
func (s *Store) openGate(gateID string) (*string, error) {
	if _, err := s.db.Exec(s.q(`UPDATE schedule_gates SET opened_at=? WHERE id=? AND opened_at IS NULL`), nowNano(), gateID); err != nil {
		return nil, fmt.Errorf("open schedule gate: %w", err)
	}
	var at sql.NullString
	if err := s.db.QueryRow(s.q(`SELECT opened_at FROM schedule_gates WHERE id=?`), gateID).Scan(&at); errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("read opened gate: %w", err)
	}
	if !at.Valid {
		return nil, nil
	}
	return &at.String, nil
}
