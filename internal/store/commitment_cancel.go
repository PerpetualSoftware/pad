package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// The cancel policy and "No longer needed" (TASK-4353, DOC-4348 case 7).
//
// A link's cancel_policy says what a cancelled parent occurrence does to the
// child occurrence it is paired with. The default, waiting_on, does nothing: the
// child keeps waiting on its parent. cancel_child_occurrence marks that ONE child
// occurrence No longer needed. The mark is a no_longer_needed_at on the link's
// per-occurrence override row, keyed by the child's occurrence key, so:
//
//   - it records no miss from then on: the deadline evaluator treats the
//     occurrence as never due (deadlineJournalState.noLongerNeeded). A miss that
//     had already happened is not erased: a deadline that passed before the
//     parent was cancelled, and was not recorded yet, is recorded first, once,
//     exactly as a skip records it, and only for an open item;
//   - it disarms the child managed reminder (only while it is armed: one that
//     already fired has gone out and stays);
//   - it never cancels the series: the child's schedule, reminder and later
//     occurrences are untouched, and the next occurrence has another key;
//   - it never touches finished work: a child that is done or abandoned, whose
//     series has ended, or that was removed, is left alone;
//   - the cascade and an offset update leave a marked occurrence where it is.
//
// The only cancel door today is skip: a skip of the parent's outstanding
// occurrence. The child occurrence a link pairs with it is the child's
// outstanding one, which is also the pairing the cascade uses.

// markChildrenNoLongerNeededTx answers the skip of parent's outstanding
// occurrence. It runs inside the skip's transaction, under the parent's pin
// (item, workspace, schedule), after the skip is written, so the skip and every
// mark commit together or not at all.
//
// LOCKS: it takes none of its own on an item or a schedule. The links and the
// child rows are plain reads: every link writer and every writer of a child
// schedule holds the workspace row this transaction holds. The writes are the
// miss (a journal row and an outbox row), one override row, one journal row and
// the delete of the child armed reminder row. A resolve of the child (which holds
// the child item and waits for the workspace) and a cascade (which holds the
// workspace) never wait on anything this holds; the one row another writer can
// hold is the reminder, and it takes that only after the workspace this
// transaction already has.
//
// openFilter and openArgs are nonTerminalFilter's, loaded before Begin: the pool
// can be one connection.
func (s *Store) markChildrenNoLongerNeededTx(tx *sql.Tx, parent *models.ItemSchedule, actor string, nowT time.Time, openFilter string, openArgs []any) error {
	rows, err := tx.Query(s.q(`SELECT id, child_schedule_id FROM schedule_links WHERE parent_schedule_id=? AND workspace_id=? AND cancel_policy=? ORDER BY id`),
		parent.ID, parent.WorkspaceID, models.LinkCancelChildOccurrence)
	if err != nil {
		return fmt.Errorf("read cancel links: %w", err)
	}
	type pair struct{ link, child string }
	var pairs []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.link, &p.child); err != nil {
			rows.Close()
			return err
		}
		pairs = append(pairs, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, p := range pairs {
		child, err := s.getScheduleByIDQ(tx, p.child)
		if err != nil {
			return err
		}
		if child == nil || child.WorkspaceID != parent.WorkspaceID || child.EndedAt != nil || child.NextAt == nil || child.NextWall == nil {
			continue // no outstanding occurrence to mark
		}
		var open bool
		err = tx.QueryRow(s.q(`SELECT `+openFilter+` FROM items i WHERE i.id=? AND i.deleted_at IS NULL`), append(append([]any{}, openArgs...), child.ItemID)...).Scan(&open)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && !open) {
			continue // removed, done or abandoned: finished work is left alone
		} else if err != nil {
			return fmt.Errorf("read cancelled child: %w", err)
		}
		loc, err := s.scheduleViewZone(tx, child)
		if err != nil {
			return err
		}
		key, err := outstandingKey(child, loc)
		if err != nil {
			return err
		}
		if key == "" {
			continue
		}
		if gone, err := s.occurrenceNoLongerNeeded(tx, child.ID, key); err != nil {
			return err
		} else if gone {
			continue
		}
		// A miss that already happened is not erased by the mark: record it first,
		// through the evaluator own candidate, so it is recorded once.
		if miss, err := s.deadlineMissCandidateTx(tx, child.ID, nowT); err != nil {
			return err
		} else if miss != nil {
			if err := s.recordDeadlineMissTx(tx, *miss); err != nil {
				return err
			}
		}
		if err := s.upsertLinkOverrideTx(tx, p.link, key, nil, nowNano()); err != nil {
			return err
		}
		// The row says nothing of the parent, not even that there is one: the
		// child's journal is read by people who may not be able to read it.
		if err := s.journalScheduleChangeRowTx(tx, newID(), child.ID, "update", key, "", "", `{"no_longer_needed":true}`, actor,
			"No longer needed", "system", "", ""); err != nil {
			return err
		}
		// Nothing is due for work that is no longer needed. A reminder that
		// already fired has gone out and stays.
		if err := s.cancelArmedReminderTx(tx, child.ID); err != nil {
			return err
		}
	}
	return nil
}

// occurrenceNoLongerNeeded reports whether some link marked this occurrence of
// the child schedule No longer needed.
func (s *Store) occurrenceNoLongerNeeded(q Queryer, scheduleID, key string) (bool, error) {
	var n int
	err := q.QueryRow(s.q(`SELECT COUNT(*) FROM schedule_link_overrides o JOIN schedule_links l ON l.id=o.link_id
 WHERE l.child_schedule_id=? AND o.occurrence_key=? AND o.no_longer_needed_at IS NOT NULL`), scheduleID, key).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("read no longer needed: %w", err)
	}
	return n > 0, nil
}

// OccurrenceNoLongerNeeded reports whether the item's outstanding occurrence is
// marked No longer needed: a parent occurrence it was linked to, under the
// cancel_child_occurrence policy, was cancelled. It records no miss. It says
// nothing about which parent: the caller shows the reason only to a reader who
// can read it. False for an item with no outstanding occurrence.
func (s *Store) OccurrenceNoLongerNeeded(workspaceID, itemID string) (bool, error) {
	sc, err := s.getScheduleQ(s.db, workspaceID, itemID, false)
	if err != nil || sc == nil || sc.EndedAt != nil || sc.NextWall == nil {
		return false, err
	}
	loc, err := s.scheduleViewZone(s.db, sc)
	if err != nil {
		return false, err
	}
	key, err := outstandingKey(sc, loc)
	if err != nil || key == "" {
		return false, err
	}
	return s.occurrenceNoLongerNeeded(s.db, sc.ID, key)
}
