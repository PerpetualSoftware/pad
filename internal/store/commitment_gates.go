package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/schedule"
)

// Commitment gates (TASK-4353, DOC-4348 C7, store level).
//
// A gate holds one gated ITEM behind one blocker item. It is one row per (gated
// item, blocker item). The gated item need not hold a commitment: an unplaced
// child waits on its gate with no date. The blocker is either a commitment, in
// which case the gate is held against the ONE occurrence that was outstanding
// when the gate was written (its occurrence key, fixed then), or an item with no
// commitment, which is judged by its status. A gate opens once and never closes,
// so a recurring blocker moving on to its next occurrence cannot re-block work
// that already went ahead. Per-occurrence gating, each weekly child waiting on
// its matching blocker occurrence, is not built.
//
// This is for commitment gates only. A plain blocks link between items means
// what it always meant, and nothing here changes it.
//
// LOCK ORDER. A gate write first locks both of its end items, ascending by id, so
// it serializes with a write to the blocker on the blocker's row. It is then a
// link write: it runs the link pin with the gated item as the root (whether or not
// it has a schedule) and the blocker as a second seed when the blocker has a
// commitment. That is the root item (FOR NO KEY UPDATE), the workspace, the
// component read after the workspace lock, the component's schedules ascending by
// id, then their reminders, and then the same re-validation under those locks.
// Every item lock comes before the workspace, so a resolve of the blocker (which
// holds the blocker item and waits for the workspace) queues with a gate write on
// that one row and cannot wait in a cycle with it. The gate rows themselves are only touched
// under the workspace lock, except the one-way write that records a gate opening
// (opened_at), which is a single statement on one gate row and waits for nothing.
//
// DISPLAY AND ACT. ScheduleGates is what ONE reader is shown. Anything that acts
// on a gate, opening it, writing opened_at, deciding that a child is released,
// goes through evaluateAndOpenGates, which reads every gate and no reader. The
// display's state covers every gate, so a child whose hidden blocker is moved,
// removed or still pending is not shown as released; its detail names only the
// blockers the reader can read.
//
// Opening is recorded by blocker writes and the tick. Display reads never write.
//
// WHEN A GATE STOPS HOLDING WITHOUT ITS BLOCKER SUCCEEDING. The default is that
// the child stays held; nothing here releases it silently.
//
//	blocker commitment deleted         the gate stays (blocker_schedule_id goes NULL, the
//	                                   foreign key's ON DELETE SET NULL), judged by the
//	                                   blocker item status; an open gate stays open.
//	                                   TestDeletingABlockersCommitmentLeavesItsGateJudgedByTheItemStatus,
//	                                   TestAnOpenGateStaysOpenWhenItsBlockersCommitmentIsDeleted
//	blocker item soft-deleted          cancelled: blocked for a decision.
//	                                   TestASoftDeletedBlockerLeavesTheChildBlockedForADecision
//	blocker item hard-deleted/purged   only a workspace purge deletes items, and it takes the
//	                                   gated item and its gates with it.
//	                                   TestAWorkspacePurgeRemovesItsGates
//	blocker item abandoned             cancelled, even with its occurrence still pending; a
//	                                   recorded done occurrence and an open gate win.
//	                                   TestAnAbandonedBlockerItemLeavesTheChildBlockedWhileItsOccurrenceIsPending,
//	                                   TestADoneOccurrenceStillWinsOverAnAbandonedBlockerItem
//	blocker moved to another workspace the source is archived: cancelled, blocked for a decision,
//	                                   for a reader of the blocker and for one who is not. A
//	                                   copy leaves the blocker where it is.
//	                                   TestAMovedBlockerLeavesTheChildBlockedForADecision
//	blocker timing edited              a pending gate fixed on the edited occurrence follows the
//	                                   new outstanding one (none left: status-judged); an open
//	                                   gate and a gate fixed on another occurrence are untouched.
//	                                   TestATimingEditOfTheBlockerRekeysAPendingGateFixedOnTheEditedOccurrence and its siblings
//	blocker schedule ended             done: succeeded; skipped: blocked; ended by an edit or
//	                                   before the gate was written: judged by the item status.
//	                                   TestABlockerWhoseCommitmentHadEndedIsJudgedByItsStatus,
//	                                   TestATimingEditThatEndsTheBlockersScheduleFallsBackToTheItemStatus
//	blocker occurrence skipped, passed over or moved
//	                                   skipped, lapsed and passed-over occurrences are blocked
//	                                   for a decision; a move keeps the key and the gate pending.
//	                                   TestASkippedBlockerOccurrenceLeavesTheChildBlockedForADecision,
//	                                   TestAPassedOverBlockerOccurrenceLeavesTheChildBlockedForADecision,
//	                                   TestMovingTheBlockerTargetKeepsTheGateOnTheSameOccurrence
//	gated item moved/copied to another workspace
//	                                   a move deletes its gates and counts the readable ones
//	                                   (CrossWorkspaceCopyResult.GatesDropped); a copy starts with
//	                                   none and the source keeps hers.
//	                                   TestACrossWorkspaceMoveOfAGatedItemDropsItsGatesAndCountsTheReadableOnes,
//	                                   TestACrossWorkspaceCopyOfAGatedItemGetsNoGatesAndTheSourceKeepsHers
//	gated item deleted                 soft-deleted: the gate stays and a restore brings it back;
//	                                   purged: gone with it.
//	                                   TestASoftDeletedGatedItemKeepsItsGateAcrossARestore
//	gated item collection/status change nothing: a gate is keyed by the item.
//	                                   TestAGatedItemsCollectionAndStatusLeaveItsGate,
//	                                   TestAMoveToAnotherCollectionKeepsTheGates
//	blocker item reopened after done   an open gate stays open.
//	                                   TestABlockerWithNoCommitmentSucceedsOnATerminalStatusThatIsNotAbandoned
//	blocker hidden from the reader     the display still holds the child (waiting or blocked), and
//	                                   the gate opens on the blocker's outcome whoever reads.
//	                                   TestADisplayReaderWhoCannotSeeTheBlockerStillSeesTheChildHeld

const (
	// maxGatesPerItem bounds how many blockers one gated item can have.
	maxGatesPerItem = 50
	// maxGateClosure bounds the gate edges read below a new gate's gated item to
	// find a cycle, so the check costs a bounded read however large the workspace.
	maxGateClosure = 500
)

// CreateScheduleGate gates blockedItemID behind blockerItemID. Both ends need
// edit; access holds the caller's decision, and an end it does not make
// readable, or that is missing, soft-deleted or in another workspace, is
// ErrScheduleLinkNotFound. Neither end needs a commitment. The blocker's
// outstanding occurrence, if it has one, is fixed here.
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
	if err := s.lockGateEndsTx(tx, workspaceID, blockedItemID, blockerItemID); err != nil {
		return nil, err
	}
	c, err := s.pinCommitmentRoot(tx, workspaceID, "", nil, false, false, true, blockedItemID, extra...)
	if errors.Is(err, ErrScheduleItemGone) {
		return nil, ErrScheduleLinkNotFound
	}
	if err != nil {
		return nil, err
	}
	var blocker *models.ItemSchedule
	for _, sc := range c.scheds {
		if sc.ItemID == blockerItemID {
			blocker = sc
		}
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
	if err := tx.QueryRow(s.q(`SELECT COUNT(*) FROM schedule_gates WHERE blocked_item_id=?`), blockedItemID).Scan(&held); err != nil {
		return nil, fmt.Errorf("count schedule gates: %w", err)
	} else if held >= maxGatesPerItem {
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
	if _, err := tx.Exec(s.q(`INSERT INTO schedule_gates (id,workspace_id,blocked_item_id,blocker_item_id,blocker_schedule_id,blocker_occurrence_key,created_at) VALUES (?,?,?,?,?,?,?)`),
		g.ID, workspaceID, blockedItemID, blockerItemID, blockerSchedule, key, g.CreatedAt); err != nil {
		if isUniqueViolation(err) {
			return nil, &LinkRefusedError{Code: "gate_duplicate"}
		}
		return nil, fmt.Errorf("insert schedule gate: %w", err)
	}
	if err := s.openGatesForBlockerTx(tx, workspaceID, blockerItemID); err != nil {
		return nil, err
	}
	var opened sql.NullString
	if err := tx.QueryRow(s.q(`SELECT opened_at FROM schedule_gates WHERE id=?`), g.ID).Scan(&opened); err != nil {
		return nil, err
	}
	if opened.Valid {
		g.OpenedAt = &opened.String
		g.Outcome = models.GateOutcomeSucceeded
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
		rows, err := tx.Query(s.q(`SELECT g.blocker_item_id, g.blocked_item_id FROM schedule_gates g
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
	err := s.db.QueryRow(s.q(`SELECT g.blocked_item_id, g.blocker_item_id, g.blocker_schedule_id FROM schedule_gates g
 WHERE g.id=? AND g.workspace_id=?`), gateID, workspaceID).Scan(&blocked, &blocker, &blockerSchedule)
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
	if _, err := s.pinCommitmentRoot(tx, workspaceID, "", nil, true, true, true, blocked, extra...); errors.Is(err, ErrScheduleItemGone) || errors.Is(err, ErrScheduleNotFound) {
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
//     recorded miss and even over an abandoned item: the work was recorded, with
//     its actual time, and a gate opens once. `skipped` is skipped. `lapsed`, and
//     a passed-over `missed`, are lapsed. A recorded deadline miss under
//     deadline_behavior `lapses` is lapsed whether or not the occurrence was
//     closed; under `stays_owed` the occurrence is still owed, so it stays
//     pending and a later Done opens the gate. A `moved` occurrence, or one with
//     no row, is pending, unless the blocker ITEM can no longer deliver: an
//     abandoned status, or an item removed, is cancelled.
//   - A blocker with no commitment, or whose commitment had ended when the gate
//     was written (an empty key: there is no occurrence to fix), or whose
//     commitment was deleted since (the schedule id is NULL), is judged by the
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
// read on its Queryer the first time a status decides a gate.
type gateEvaluator struct {
	s           *Store
	q           Queryer
	workspaceID string
	filters     []collectionDoneFilter
	loaded      bool
}

func (e *gateEvaluator) outcome(g gateRow) (schedule.Outcome, error) {
	if g.blockerSchedule.Valid && g.key != "" {
		out, decided, err := e.s.occurrenceOutcomeQ(e.q, g.blockerSchedule.String, g.key)
		if err != nil || decided {
			return out, err
		}
		// The occurrence is still open. Only an item that can no longer deliver ends it.
		status, removed, err := e.itemState(g.BlockerItemID)
		if err != nil {
			return "", err
		}
		if removed || status == schedule.OutcomeCancelled {
			return schedule.OutcomeCancelled, nil
		}
		return schedule.OutcomePending, nil
	}
	status, removed, err := e.itemState(g.BlockerItemID)
	if err != nil {
		return "", err
	}
	if status == schedule.OutcomePending && removed {
		return schedule.OutcomeCancelled, nil
	}
	return status, nil
}

// occurrenceOutcome reads the state of the occurrence a key names. decided is
// false when the occurrence is still open: pending, unless a miss under
// `lapses` was recorded.
func (s *Store) occurrenceOutcomeQ(q Queryer, scheduleID, key string) (out schedule.Outcome, decided bool, err error) {
	col, val := "occurrence_wall", key
	if i := strings.IndexByte(key, '|'); i >= 0 {
		col, val = "occurrence_at", key[:i]
	}
	var state string
	switch err := q.QueryRow(s.q(`SELECT state FROM schedule_occurrences WHERE schedule_id=? AND `+col+`=? ORDER BY resolved_at DESC LIMIT 1`), scheduleID, val).Scan(&state); {
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
	if err := q.QueryRow(s.q(`SELECT COUNT(*) FROM schedule_changes c JOIN item_schedules sc ON sc.id=c.schedule_id
 WHERE c.schedule_id=? AND c.change_type='missed_recorded' AND c.role='deadline' AND c.occurrence_key=? AND sc.deadline_behavior='lapses'`), scheduleID, key).Scan(&misses); err != nil {
		return "", false, fmt.Errorf("read blocker miss: %w", err)
	}
	if misses > 0 {
		return schedule.OutcomeLapsed, true, nil
	}
	return schedule.OutcomePending, false, nil
}

// itemState reads a blocker item by its done-field value, with the collection's
// own terminal and abandoned options (the workspace defaults where it declares
// none), as the rest of the product reads them: succeeded for a terminal status
// that is not abandoned, cancelled for an abandoned one, pending otherwise. removed
// is true for an item that is soft-deleted or gone.
func (e *gateEvaluator) itemState(itemID string) (status schedule.Outcome, removed bool, err error) {
	var collectionID, fields string
	var gone int
	err = e.q.QueryRow(e.s.q(`SELECT collection_id, fields, CASE WHEN deleted_at IS NULL THEN 0 ELSE 1 END FROM items WHERE id=? AND workspace_id=?`), itemID, e.workspaceID).Scan(&collectionID, &fields, &gone)
	if errors.Is(err, sql.ErrNoRows) {
		return schedule.OutcomePending, true, nil
	} else if err != nil {
		return "", false, fmt.Errorf("read gate blocker: %w", err)
	}
	if !e.loaded {
		rows, readErr := e.q.Query(e.s.q(`SELECT id,schema,settings FROM collections WHERE workspace_id=?`), e.workspaceID)
		if readErr != nil {
			return "", false, readErr
		}
		e.filters = scanCollectionDoneFilters(rows)
		readErr = rows.Err()
		rows.Close()
		if readErr != nil {
			return "", false, readErr
		}
		e.loaded = true
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
	value, _ := values[doneKey].(string)
	switch {
	case containsFold(abandoned, value):
		return schedule.OutcomeCancelled, gone == 1, nil
	case containsFold(terminals, value):
		return schedule.OutcomeSucceeded, gone == 1, nil
	}
	return schedule.OutcomePending, gone == 1, nil
}

// gateEvaluation is the UNFILTERED result for one gated item: every gate, whoever
// is looking, with its outcome and opening. state covers every gate.
type gateEvaluation struct {
	state      string
	gates      []models.ScheduleGate
	unresolved []string // blocker item ids of every gate that has not succeeded
}

// evaluateGatesQ evaluates every gate without a reader filter or any writes.
func (s *Store) evaluateGatesQ(q Queryer, workspaceID, itemID string) (*gateEvaluation, error) {
	rows, err := q.Query(s.q(`SELECT g.id,g.blocker_item_id,g.blocker_schedule_id,g.blocker_occurrence_key,g.opened_at,g.created_at FROM schedule_gates g
 WHERE g.blocked_item_id=? AND g.workspace_id=? ORDER BY g.id`), itemID, workspaceID)
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
	ev := &gateEvaluation{gates: []models.ScheduleGate{}}
	eval := &gateEvaluator{s: s, q: q, workspaceID: workspaceID}
	outcomes := map[schedule.Node]schedule.Outcome{}
	for _, g := range gates {
		outcome := schedule.OutcomeSucceeded
		if g.OpenedAt == nil {
			if outcome, err = eval.outcome(g); err != nil {
				return nil, err
			}
		}
		g.Outcome = string(outcome)
		outcomes[schedule.Node{Schedule: g.BlockerItemID}] = outcome
		ev.gates = append(ev.gates, g.ScheduleGate)
	}
	res := schedule.EvaluateGates(outcomes)
	ev.state = string(res.State)
	for _, n := range res.Unresolved {
		ev.unresolved = append(ev.unresolved, n.Schedule)
	}
	return ev, nil
}

// ScheduleGates is what one reader is shown of an item's gates. It is DISPLAY
// only: it evaluates every gate without writing, and
// then leaves out of the detail the gates whose blocker the reader cannot read.
// The state covers every gate, so a child held by a blocker the reader cannot see
// is shown held, not released; the detail (Gates, Unresolved) names only blockers
// the reader can read, so a hidden blocker's item and gate are not named.
func (s *Store) ScheduleGates(workspaceID, itemID string, access models.CommitmentAccess) (*models.ScheduleGateView, error) {
	if !access.Readable[itemID] {
		return nil, ErrScheduleNotFound
	}
	ev, err := s.evaluateGatesQ(s.db, workspaceID, itemID)
	if err != nil {
		return nil, err
	}
	view := &models.ScheduleGateView{State: ev.state, Gates: []models.ScheduleGate{}}
	for _, g := range ev.gates {
		if access.Readable[g.BlockerItemID] {
			view.Gates = append(view.Gates, g)
		}
	}
	for _, id := range ev.unresolved {
		if access.Readable[id] {
			view.Unresolved = append(view.Unresolved, id)
		}
	}
	return view, nil
}

// evaluateAndOpenGates records successful gates and their notices atomically.
func (s *Store) evaluateAndOpenGates(workspaceID, itemID string) (*gateEvaluation, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	ev, err := s.evaluateAndOpenGatesTx(tx, workspaceID, itemID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ev, nil
}

func (s *Store) evaluateAndOpenGatesTx(tx *sql.Tx, workspaceID, itemID string) (*gateEvaluation, error) {
	ev, err := s.evaluateGatesQ(tx, workspaceID, itemID)
	if err != nil {
		return nil, err
	}
	for i := range ev.gates {
		g := &ev.gates[i]
		if g.OpenedAt != nil || g.Outcome != models.GateOutcomeSucceeded {
			continue
		}
		at := nowNano()
		res, err := tx.Exec(s.q(`UPDATE schedule_gates SET opened_at=? WHERE id=? AND opened_at IS NULL`), at, g.ID)
		if err != nil {
			return nil, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return nil, err
		}
		if n == 1 {
			g.OpenedAt = &at
			if err := s.enqueueCommitmentNoticeTx(tx, workspaceID, itemID, "commitment_gate_opened", "commitment-gate:"+g.ID); err != nil {
				return nil, err
			}
		}
	}
	return ev, nil
}

// openGatesForBlockerTx observes success on the writer connection before it can disappear.
// The caller holds the blocker's row, by its UPDATE or its pin, and this locks
// no other item. The fenced app door does not call this: an app write stays inside its reviewed door, and
// the tick opens the gates of a blocker an app finished.
func (s *Store) openGatesForBlockerTx(tx *sql.Tx, workspaceID, blockerID string) error {
	rows, err := tx.Query(s.q(`SELECT DISTINCT blocked_item_id FROM schedule_gates WHERE workspace_id=? AND blocker_item_id=? AND opened_at IS NULL ORDER BY blocked_item_id`), workspaceID, blockerID)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := s.evaluateAndOpenGatesTx(tx, workspaceID, id); err != nil {
			return err
		}
	}
	return nil
}

// lockGateEndsTx locks both ends of a new gate, ascending by id so two gate
// writes cannot wait on each other, and before the workspace row, as every item
// lock in the commitment order is taken. Holding the blocker's row is what
// serializes this gate with a write to that blocker: the writer's UPDATE holds
// the same row, so either this waits and then reads the success, or the writer
// waits and then finds the gate. An end that is gone is left for the pin to
// answer.
func (s *Store) lockGateEndsTx(tx *sql.Tx, workspaceID, a, b string) error {
	if s.dialect.Driver() != DriverPostgres {
		return nil
	}
	if b < a {
		a, b = b, a
	}
	for _, itemID := range []string{a, b} {
		var id string
		err := tx.QueryRow(s.q(`SELECT id FROM items WHERE id=? AND workspace_id=? FOR NO KEY UPDATE`), itemID, workspaceID).Scan(&id)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("lock gate end: %w", err)
		}
	}
	return nil
}

// EvaluateCommitmentGates advances a deterministic cursor across pending gates.
// Pending gates keep their place behind the cursor until the next complete pass.
func (s *Store) EvaluateCommitmentGates(now string, limit int) error {
	if _, err := time.Parse(time.RFC3339, now); err != nil {
		return err
	}
	if limit <= 0 {
		limit = defaultReminderFireLimit
	}
	s.gateSweepMu.Lock()
	defer s.gateSweepMu.Unlock()
	rows, err := s.db.Query(s.q(`SELECT id,workspace_id,blocked_item_id FROM schedule_gates WHERE opened_at IS NULL AND id>? ORDER BY id LIMIT ?`), s.gateSweepCursor, limit)
	if err != nil {
		return err
	}
	type candidate struct{ id, ws, item string }
	var page []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.ws, &c.item); err != nil {
			rows.Close()
			return err
		}
		page = append(page, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	// A broken row does not stop the pass or hold the cursor, so it cannot
	// starve the gates behind it.
	var errs []error
	for _, c := range page {
		if _, err := s.evaluateAndOpenGates(c.ws, c.item); err != nil {
			errs = append(errs, fmt.Errorf("evaluate gate %s: %w", c.id, err))
		}
		s.gateSweepCursor = c.id
	}
	if len(page) < limit {
		s.gateSweepCursor = ""
	}
	return errors.Join(errs...)
}

// ---- carrying the gated item to another workspace ----

// gatesDropped counts the gates that hold itemID whose blocker canRead accepts
// (nil accepts every blocker). It names no item.
func (s *Store) gatesDropped(q Queryer, workspaceID, itemID string, canRead func(string) bool) (int, error) {
	rows, err := q.Query(s.q(`SELECT blocker_item_id FROM schedule_gates WHERE blocked_item_id=? AND workspace_id=?`), itemID, workspaceID)
	if err != nil {
		return 0, fmt.Errorf("count schedule gates: %w", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var blocker string
		if err := rows.Scan(&blocker); err != nil {
			return 0, err
		}
		if canRead != nil && canRead(blocker) {
			n++
		}
	}
	return n, rows.Err()
}

// ItemGatesDropped is what a cross-workspace copy or move of itemID would leave
// out: how many gates hold it whose blocker the mover can read (nil: none). Gates
// stay within one workspace, so none is carried, with or without a commitment.
func (s *Store) ItemGatesDropped(workspaceID, itemID string, canRead func(string) bool) (int, error) {
	return s.gatesDropped(s.db, workspaceID, itemID, canRead)
}

// carryGatesTx runs in the cross-workspace copy transaction. It reports the gates
// that hold the source item, readable blockers only. A MOVE also deletes all of
// them, the ones behind hidden blockers too (which are not counted): the item
// leaves the workspace, and must neither arrive unblocked by accident nor leave a
// gate behind on an archived item. A copy leaves the source its own. The item row
// is already held by the archive on a move, which is the first lock every gate
// writer takes, so no gate write can interleave. A moved BLOCKER keeps its gates:
// it is archived, which reads as cancelled and holds the child.
func (s *Store) carryGatesTx(tx *sql.Tx, workspaceID, itemID string, move bool, canRead func(string) bool) (int, error) {
	n, err := s.gatesDropped(tx, workspaceID, itemID, canRead)
	if err != nil || !move {
		return n, err
	}
	if _, err := tx.Exec(s.q(`DELETE FROM schedule_gates WHERE blocked_item_id=? AND workspace_id=?`), itemID, workspaceID); err != nil {
		return 0, fmt.Errorf("copy item across workspaces: delete source gates: %w", err)
	}
	return n, nil
}
