package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/schedule"
)

var ErrCommitmentChanged = errors.New("the commitment changed; re-preview before applying")

type renegotiationPlan struct {
	result                models.CommitmentRenegotiation
	roles                 scheduleRoles
	until                 time.Time
	before, after, change string
}

// RenegotiateCommitment previews or applies one plan. Authority belongs to the
// handler; expectedAssignee guards its decision (nil for an owner). The clock
// is taken after pinning so a wait cannot hide a deadline that passed meanwhile.
func (s *Store) RenegotiateCommitment(workspaceID, itemID, actor string, in models.CommitmentRenegotiateInput, expectedAssignee *string) (*models.CommitmentRenegotiation, error) {
	return s.renegotiateCommitmentAt(workspaceID, itemID, actor, in, time.Time{}, expectedAssignee)
}

func (s *Store) renegotiateCommitmentAt(workspaceID, itemID, actor string, in models.CommitmentRenegotiateInput, nowT time.Time, expectedAssignee *string) (*models.CommitmentRenegotiation, error) {
	in.Reason = strings.TrimSpace(in.Reason)
	if in.Reason == "" {
		return nil, fieldErr("reason", "required", "renegotiating a deadline needs a reason")
	}
	if in.Scope == "" {
		in.Scope = "occurrence"
	}
	if !oneOf(in.Scope, []string{"occurrence", "series"}) {
		return nil, fieldErr("scope", "enum", "scope must be occurrence or series")
	}
	if in.Scope == "occurrence" {
		if in.RemoveDeadline {
			return nil, fieldErr("scope", "combination", "removing a deadline needs scope series")
		}
		if in.DeadlineOffset != nil || in.DeadlineBehavior != nil {
			return nil, fieldErr("scope", "combination", "deadline offsets and behavior need scope series")
		}
	} else {
		if in.Deadline != nil {
			return nil, fieldErr("deadline", "combination", "an instant deadline needs scope occurrence")
		}
		if in.RemoveDeadline && (in.DeadlineOffset != nil || in.DeadlineBehavior != nil) {
			return nil, fieldErr("remove_deadline", "combination", "removal excludes deadline_offset and deadline_behavior")
		}
		if !in.RemoveDeadline && in.DeadlineOffset == nil && in.DeadlineBehavior == nil {
			return nil, fieldErr("deadline", "combination", "a series change needs deadline_offset, deadline_behavior or remove_deadline")
		}
	}
	if !in.Preview && in.Revision == "" {
		return nil, fieldErr("revision", "required", "an apply needs the preview revision")
	}
	// Load workspace terminal options before the transaction, as the evaluator
	// does: nonTerminalFilter uses the pool, which may have only one connection.
	filter, args := s.nonTerminalFilter(workspaceID, "i")
	if s.beforeCommitmentPin != nil {
		s.beforeCommitmentPin()
	}
	var budget *linkBudget
	if in.Scope == "occurrence" {
		budget = s.newLinkBudget()
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := s.pinScheduleTx(tx, workspaceID, itemID); err != nil {
		return nil, err
	}
	if matches, err := s.commitmentAssigneeMatchesTx(tx, itemID, expectedAssignee); err != nil {
		return nil, err
	} else if !matches {
		return nil, ErrCommitmentChanged
	}
	sc, err := s.getScheduleQ(tx, workspaceID, itemID, false)
	if err != nil {
		return nil, err
	}
	if sc == nil {
		return nil, ErrScheduleNotFound
	}
	// An ended schedule has no outstanding occurrence to move. Refuse it here,
	// under the pin, before the plan or the revision comparison, so every scope
	// and both modes answer the same code and nothing is stored.
	if sc.EndedAt != nil {
		return nil, fieldErr("schedule", "ended", "the commitment has ended; there is no outstanding occurrence to renegotiate")
	}
	if nowT.IsZero() {
		nowT = time.Now()
	}
	var open bool
	queryArgs := append(append([]any{}, args...), itemID)
	if err := tx.QueryRow(s.q(`SELECT `+filter+` FROM items i WHERE i.id=?`), queryArgs...).Scan(&open); err != nil {
		return nil, err
	}
	var component *commitmentComponent
	planInput := in
	if in.Scope == "occurrence" {
		component, err = s.cascadeLinkedTx(tx, sc, filter, args, "deadline")
		if err != nil {
			return nil, err
		}
		if component != nil {
			if err := s.setTxTimeoutsCtx(budget.ctx, tx, linkTxBudget, linkTxBudget); err != nil {
				return nil, err
			}
			planInput.Revision = ""
		}
	}
	plan, err := s.planRenegotiationTx(tx, sc, planInput, nowT, open)
	if err != nil {
		return nil, err
	}
	var linked *linkedPlan
	if component != nil {
		linked, err = s.planLinkedTx(tx, component, workspaceID, itemID, schedule.RoleDeadline, plan.until, true)
		if err != nil {
			return nil, err
		}
		// Both inputs are fixed-length opaque revisions; their combined token
		// invalidates the preview when either the root or its cascade changes.
		hash := sha256.Sum256([]byte(plan.result.Revision + linked.revision))
		plan.result.Revision = base64.RawURLEncoding.EncodeToString(hash[:])
		if !in.Preview && in.Revision != plan.result.Revision {
			return nil, ErrCommitmentChanged
		}
		cascade, _, err := s.settleLinkedPlan(linked, budget, linkedOptions{actor: actor, reason: in.Reason, revision: linked.revision, preview: in.Preview, acknowledge: in.AcknowledgeViolations, access: in.Access, rootAuthorized: true})
		if err != nil {
			return nil, err
		}
		// With nothing to show beyond the root, the answer is the unlinked one, so
		// a dependent the caller cannot read does not change its shape.
		if cascade != nil && cascadeNamesADependent(cascade, itemID) {
			plan.result.Cascade = cascade
		}
	}
	if in.Scope == "series" {
		// A series change replaces the deadline role. Refuse it for a role a link
		// uses in the preview as well, so a preview never promises an apply that
		// is then refused, and before anything is written.
		linked, err := s.linkedRolesTx(tx, sc.ID)
		if err != nil {
			return nil, err
		}
		if len(linked) > 0 {
			stored, err := s.scheduleRolesOf(tx, sc)
			if err != nil {
				return nil, err
			}
			if err := guardLinkedRoles(linked, stored, plan.roles); err != nil {
				return nil, err
			}
		}
	}
	if in.Preview {
		return &plan.result, nil
	}
	if actor == "" {
		actor = "user"
	}
	rootLapsed := false
	if open {
		d, state, err := s.deadlineStateTx(tx, sc.ID)
		if err != nil {
			return nil, err
		}
		if d != nil && d.dueAfterConversion(nowT, state) {
			if !state.recorded {
				if err := s.recordDeadlineMissTx(tx, *d); err != nil {
					return nil, err
				}
			}
			lapse, err := s.lapseOccurrenceTx(tx, *d, nowT)
			if err != nil {
				return nil, err
			}
			if lapse != nil {
				if linked != nil {
					rootLapsed = true
				} else {
					// The preview named an occurrence that just closed. Commit its
					// lapse even when its miss was recorded before this request.
					if err := tx.Commit(); err != nil {
						return nil, fmt.Errorf("lapse before renegotiation: %w", err)
					}
					return nil, ErrCommitmentChanged
				}
			}
		}
	}
	if linked != nil {
		lapsed, err := s.lapseDueInPlanTx(tx, linked, nowT)
		if err != nil {
			return nil, err
		}
		if !rootLapsed && !lapsed {
			if err := s.applyLinkedPlanTx(tx, linked, actor, in.Reason, "Moved with a linked date", nowT, budget); err != nil {
				return nil, err
			}
		}
		if err := budget.commit(tx); err != nil {
			return nil, err
		}
		if rootLapsed || lapsed {
			return nil, ErrCommitmentChanged
		}
		return &plan.result, nil
	}
	if in.Scope == "occurrence" {
		if err := s.upsertScheduleOverrideTx(tx, sc.ID, plan.result.Preview.OccurrenceKey, "deadline", plan.until, nil); err != nil {
			return nil, err
		}
	} else {
		if err := s.writeNativeRolesTx(tx, sc, plan.roles, false); err != nil {
			return nil, err
		}
		if in.RemoveDeadline {
			// Deadline overrides are written only for the outstanding occurrence.
			// Other keys can belong to resolved occurrences even when they sort later.
			if _, err := tx.Exec(s.q(`DELETE FROM schedule_occurrence_overrides WHERE schedule_id=? AND role='deadline' AND occurrence_key=?`), sc.ID, plan.result.Preview.OccurrenceKey); err != nil {
				return nil, err
			}
		}
		// Like SetCommitmentRoles, a role change leaves the trigger and its
		// managed reminder armed at the same instant, including any snooze.
	}
	if err := s.journalScheduleChangeReasonTx(tx, sc.ID, plan.change, plan.result.Preview.OccurrenceKey, "deadline", plan.before, plan.after, actor, in.Reason, "user"); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("renegotiate commitment: %w", err)
	}
	return &plan.result, nil
}

func cascadeNamesADependent(c *models.LinkedMoveResult, rootItemID string) bool {
	if len(c.Violations) > 0 || len(c.LeftInPlace) > 0 {
		return true
	}
	for _, ch := range c.Changes {
		if ch.ItemID != rootItemID {
			return true
		}
	}
	return false
}

func deadlineRolesSnapshot(r scheduleRoles) string {
	b, _ := json.Marshal(struct {
		Days     *int    `json:"deadline_days"`
		Secs     *int    `json:"deadline_secs"`
		Behavior *string `json:"deadline_behavior"`
	}{r.DeadlineDays, r.DeadlineSecs, r.DeadlineBehavior})
	return string(b)
}

func (s *Store) planRenegotiationTx(tx *sql.Tx, sc *models.ItemSchedule, in models.CommitmentRenegotiateInput, nowT time.Time, open bool) (*renegotiationPlan, error) {
	stored, err := s.scheduleRolesQ(tx, sc.ID)
	if err != nil {
		return nil, err
	}
	loc, err := s.scheduleViewZone(tx, sc)
	if err != nil {
		return nil, err
	}
	key, err := outstandingKey(sc, loc)
	if err != nil {
		return nil, err
	}
	overrides, err := s.occurrenceOverrides(tx, sc.ID, key)
	if err != nil {
		return nil, err
	}
	var miss *deadlineCandidate
	if open {
		miss, err = s.deadlineMissCandidateTx(tx, sc.ID, nowT)
		if err != nil {
			return nil, err
		}
	}
	// Revision inputs: the schedule (including ID, updated_at, timing and
	// outstanding state), every role column, the outstanding key and deadline
	// override value, the effective zone, and the miss-first decision. Including
	// that decision also invalidates a preview when the old deadline passes or
	// the evaluator records its miss before apply. The request is not hashed.
	state, err := json.Marshal(struct {
		Schedule  *models.ItemSchedule
		Roles     *models.ScheduleRolesExport
		Key       string
		Override  *string
		Zone      string
		MissFirst bool
	}{sc, stored, key, overrideFor(overrides, "deadline"), loc.String(), miss != nil})
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(state)
	revision := base64.RawURLEncoding.EncodeToString(hash[:])
	if in.Revision != "" && in.Revision != revision {
		return nil, ErrCommitmentChanged
	}
	if stored.RolesOrigin != "native" || stored.DeadlineDays == nil {
		return nil, fieldErr("deadline", "combination", "the schedule has no native deadline to renegotiate")
	}
	if key == "" || sc.NextWall == nil {
		return nil, fieldErr("deadline", "combination", "the schedule has no outstanding occurrence")
	}
	cur := scheduleRolesFromExport(stored)
	if _, err := occupiedEnd(sc.Dtstart, cur); err != nil {
		return nil, err
	}
	prior, err := resolveRoleValue(loc, *sc.NextWall, *cur.DeadlineDays, *cur.DeadlineSecs, overrideFor(overrides, "deadline"))
	if err != nil {
		return nil, err
	}
	p := &renegotiationPlan{roles: cur}
	p.result.Revision = revision
	view := &p.result.Preview
	*view = models.CommitmentRenegotiationPreview{Scope: in.Scope, OccurrenceKey: key, AppliesFrom: key, CurrentDeadline: ptr(prior.value), CurrentDeadlineBehavior: cur.DeadlineBehavior, NewDeadlineBehavior: cur.DeadlineBehavior,
		CurrentOffset: &models.CommitmentDeadlineOffset{DeadlineDays: cur.DeadlineDays, DeadlineSecs: cur.DeadlineSecs}, MissRecordedFirst: miss != nil}
	view.NewOffset = view.CurrentOffset
	if miss != nil {
		view.MissOccurrenceKey = ptr(miss.key)
	}
	if in.Scope == "occurrence" {
		if in.Deadline == nil || *in.Deadline == "" {
			return nil, fieldErr("deadline", "required", "renegotiating a deadline needs the new deadline")
		}
		until, err := time.Parse(time.RFC3339, *in.Deadline)
		if err != nil {
			return nil, fieldErr("deadline", "format", "deadline must be an RFC3339 instant")
		}
		// Validate exactly the value the override writer stores.
		p.until, err = time.Parse(time.RFC3339, NormalizeInstant(until))
		if err != nil {
			return nil, err
		}
		if !p.until.After(nowT) || p.until.After(nowT.Add(maxResolveUntil)) {
			return nil, fieldErr("deadline", "range", "deadline must be after now and within 366 days")
		}
		view.NewDeadline = ptr(p.until.UTC().Format(time.RFC3339Nano))
		p.change = "move"
		b, _ := json.Marshal(map[string]string{"deadline": prior.value})
		p.before = string(b)
		b, _ = json.Marshal(map[string]string{"deadline": *view.NewDeadline})
		p.after = string(b)
		return p, nil
	}
	input := models.CommitmentRolesInput{TargetDays: cur.TargetDays, TargetSecs: cur.TargetSecs, EarliestDays: cur.EarliestDays, EarliestSecs: cur.EarliestSecs, DeadlineDays: cur.DeadlineDays, DeadlineSecs: cur.DeadlineSecs, DeadlineBehavior: cur.DeadlineBehavior, OccupiedDays: cur.OccupiedDays, OccupiedSecs: cur.OccupiedSecs, Outcome: cur.Outcome}
	if in.RemoveDeadline {
		input.DeadlineDays = nil
		input.DeadlineSecs = nil
		input.DeadlineBehavior = nil
	} else {
		if in.DeadlineOffset != nil {
			input.DeadlineDays = in.DeadlineOffset.DeadlineDays
			input.DeadlineSecs = in.DeadlineOffset.DeadlineSecs
		}
		if in.DeadlineBehavior != nil {
			input.DeadlineBehavior = in.DeadlineBehavior
		}
	}
	p.roles, err = rolesFromInput(input)
	if err != nil {
		return nil, err
	}
	view.NewDeadlineBehavior = p.roles.DeadlineBehavior
	if in.RemoveDeadline {
		view.NewOffset = nil
		view.ClearsDeadlineOverrides = overrideFor(overrides, "deadline") != nil
	} else {
		view.NewOffset = &models.CommitmentDeadlineOffset{DeadlineDays: p.roles.DeadlineDays, DeadlineSecs: p.roles.DeadlineSecs}
		// An outstanding override is an explicit earlier renegotiation: keep it
		// for offset or behavior changes, and clear it only on deadline removal.
		view.KeepsDeadlineOverride = overrideFor(overrides, "deadline") != nil
		next, err := resolveRoleValue(loc, *sc.NextWall, *p.roles.DeadlineDays, *p.roles.DeadlineSecs, overrideFor(overrides, "deadline"))
		if err != nil {
			return nil, err
		}
		// A behavior-only change or a kept override leaves the effective deadline
		// alone. Refuse only a changed effective deadline that is already past.
		if !next.expires.Equal(prior.expires) && !next.expires.After(nowT) {
			return nil, fieldErr("deadline_offset", "range", "the new deadline is already past")
		}
		view.NewDeadline = ptr(next.value)
	}
	p.change = "update"
	p.before = deadlineRolesSnapshot(cur)
	p.after = deadlineRolesSnapshot(p.roles)
	return p, nil
}

// commitmentAssigneeMatchesTx checks the assignee used by the handler authority
// decision. A nil guard means authority does not depend on the assignee;
// a pointer to an empty string means the item was unassigned.
func (s *Store) commitmentAssigneeMatchesTx(tx *sql.Tx, itemID string, expected *string) (bool, error) {
	if expected == nil {
		return true, nil
	}
	// pinScheduleTx holds FOR NO KEY UPDATE on the item. UpdateItem locks that
	// row with its UPDATE before journaling assignment, so a reassignment cannot
	// commit after our pin until this tx ends. This separate plain SELECT sees
	// reassignment committed before the pin without upgrading the item lock.
	var assigned sql.NullString
	if err := tx.QueryRow(s.q(`SELECT assigned_user_id FROM items WHERE id=?`), itemID).Scan(&assigned); err != nil {
		return false, err
	}
	return assigned.String == *expected, nil
}
