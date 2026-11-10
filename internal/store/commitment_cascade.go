package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/schedule"
)

// The linked move (TASK-4353): move one date, cascade it down the links, in one
// transaction under the lock order in commitment_links.go.
//
// A hard deadline never moves without a renegotiation, however the move arrives.
// A cascade that would change one is preview-required, carries one reason, and
// needs the caller's authority over EACH deadline it changes; without it the
// answer is ErrDeadlineApprovalRequired, which carries nothing, and nothing is
// stored as a pending move. A cascade that changes only target, earliest or
// occupied_until dates stays cheap: no preview, no authority over the dependents.

// linkTxBudget bounds a linked change from Begin, lock waits included. The SET
// LOCAL timeouts bound each statement; checks between writes and before COMMIT
// bound the total (SQLite ignores the SET, so the checks are its only bound).
const linkTxBudget = 5 * time.Second

type linkBudget struct {
	s     *Store
	start time.Time
	ctx   context.Context
}

func (s *Store) linkNow() time.Time {
	if s.linkClock != nil {
		return s.linkClock()
	}
	return time.Now()
}

func (s *Store) newLinkBudget() *linkBudget {
	return &linkBudget{s: s, start: s.linkNow(), ctx: context.Background()}
}

// check refuses an over-budget change. The error is marked as proven rolled
// back: it is returned before any COMMIT is sent.
func (b *linkBudget) check() error {
	if b.s.linkNow().Sub(b.start) > linkTxBudget {
		return markRolledBack(ErrLinkBudget)
	}
	return nil
}

// commit checks the budget, then commits with no deadline context: a deadline
// that fired during COMMIT would leave the outcome unknown.
func (b *linkBudget) commit(tx *sql.Tx) error {
	if b.s.beforeLinkCommit != nil {
		b.s.beforeLinkCommit()
	}
	if err := b.check(); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit linked change: %w", err)
	}
	return nil
}

// linkNode is one live schedule in a component: its outstanding occurrence and
// the effective value of each role it has.
type linkNode struct {
	sc    *models.ItemSchedule
	rec   *time.Location
	x     time.Time
	key   string
	dates map[schedule.Role]time.Time
}

func (r roleResolution) instant(loc *time.Location) (time.Time, error) {
	if r.dateOnly {
		w, err := schedule.ParseWall(r.value)
		return schedule.Resolve(loc, w).UTC(), err
	}
	return time.Parse(time.RFC3339Nano, r.value)
}

// linkNodeTx reads the outstanding occurrence's dates, nil when the schedule has none.
func (s *Store) linkNodeTx(tx *sql.Tx, sc *models.ItemSchedule) (*linkNode, error) {
	if sc.EndedAt != nil || sc.NextAt == nil || sc.NextWall == nil || sc.ErrorCode != nil {
		return nil, nil
	}
	roles, err := s.scheduleRolesOf(tx, sc)
	if err != nil {
		return nil, err
	}
	loc, err := s.scheduleViewZone(tx, sc)
	if err != nil {
		return nil, err
	}
	x, ok, err := OutstandingInstant(sc, loc)
	if err != nil || !ok {
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
	n := &linkNode{sc: sc, rec: loc, x: x.Truncate(time.Second), key: key, dates: map[schedule.Role]time.Time{}}
	for _, r := range []struct {
		name       string
		days, secs *int
	}{{"target", &roles.TargetDays, &roles.TargetSecs}, {"earliest", roles.EarliestDays, roles.EarliestSecs},
		{"deadline", roles.DeadlineDays, roles.DeadlineSecs}, {"occupied_until", roles.OccupiedDays, roles.OccupiedSecs}} {
		if r.days == nil || r.secs == nil {
			continue
		}
		res, err := resolveRoleValue(loc, *sc.NextWall, *r.days, *r.secs, overrideFor(overrides, r.name))
		if err != nil {
			return nil, err
		}
		if n.dates[linkRole(r.name)], err = res.instant(loc); err != nil {
			return nil, err
		}
	}
	return n, nil
}

// effectiveLinkOffset is the link's offset for the child's outstanding
// occurrence: its override when one exists. skip is true when the child
// occurrence was marked no longer needed, so the cascade leaves it alone.
// ignoreOffset drops a stored offset override (an explicit offset write replaces
// it); a no-longer-needed mark still skips, so an offset update leaves such an
// occurrence alone on purpose and says so nowhere else.
func (s *Store) effectiveLinkOffset(tx *sql.Tx, l linkRow, childKey string, ignoreOffset bool) (off models.ScheduleLinkOffset, skip bool, err error) {
	var mode, gone sql.NullString
	var allDay int
	o := models.ScheduleLinkOffset{}
	err = tx.QueryRow(s.q(`SELECT offset_mode,offset_elapsed,offset_days,offset_clock,offset_all_day,offset_zone,no_longer_needed_at FROM schedule_link_overrides WHERE link_id=? AND occurrence_key=?`), l.ID, childKey).
		Scan(&mode, &o.ElapsedSecs, &o.Days, &o.ClockSecs, &allDay, &o.Zone, &gone)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return l.Offset, false, nil
	case err != nil:
		return l.Offset, false, fmt.Errorf("read link override: %w", err)
	}
	if gone.Valid {
		return l.Offset, true, nil
	}
	if !mode.Valid || ignoreOffset {
		return l.Offset, false, nil
	}
	o.Mode, o.AllDay = mode.String, allDay != 0
	return o, false, nil
}

func (s *Store) upsertLinkOverrideTx(tx *sql.Tx, linkID, key string, off *models.ScheduleLinkOffset, goneAt string) error {
	o := models.ScheduleLinkOffset{}
	var mode any
	if off != nil {
		o, mode = *off, off.Mode
	}
	allDay := 0
	if o.AllDay {
		allDay = 1
	}
	ts := nowNano()
	_, err := tx.Exec(s.q(`INSERT INTO schedule_link_overrides (id,link_id,occurrence_key,offset_mode,offset_elapsed,offset_days,offset_clock,offset_all_day,offset_zone,no_longer_needed_at,created_at,updated_at)
 VALUES (?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT (link_id,occurrence_key) DO UPDATE SET
 offset_mode=COALESCE(excluded.offset_mode,schedule_link_overrides.offset_mode),
 offset_elapsed=CASE WHEN excluded.offset_mode IS NULL THEN schedule_link_overrides.offset_elapsed ELSE excluded.offset_elapsed END,
 offset_days=CASE WHEN excluded.offset_mode IS NULL THEN schedule_link_overrides.offset_days ELSE excluded.offset_days END,
 offset_clock=CASE WHEN excluded.offset_mode IS NULL THEN schedule_link_overrides.offset_clock ELSE excluded.offset_clock END,
 offset_all_day=CASE WHEN excluded.offset_mode IS NULL THEN schedule_link_overrides.offset_all_day ELSE excluded.offset_all_day END,
 offset_zone=CASE WHEN excluded.offset_mode IS NULL THEN schedule_link_overrides.offset_zone ELSE excluded.offset_zone END,
 no_longer_needed_at=COALESCE(excluded.no_longer_needed_at,schedule_link_overrides.no_longer_needed_at),updated_at=excluded.updated_at`),
		newID(), linkID, key, mode, o.ElapsedSecs, o.Days, o.ClockSecs, allDay, o.Zone, nilIfEmpty(goneAt), ts, ts)
	if err != nil {
		return fmt.Errorf("write link override: %w", err)
	}
	return nil
}

// linkedPlan is the cascade as computed under the locks.
type linkedPlan struct {
	c        *commitmentComponent
	nodes    map[string]*linkNode
	impact   schedule.Impact
	root     string // root schedule id
	to       time.Time
	incoming *linkRow // set only for a direct move of a date a link supplies
	// incomingOffset is that link's effective offset for the occurrence (its
	// override when one exists), the mode a drag is measured in.
	incomingOffset models.ScheduleLinkOffset
	// leftInPlace are the changes a finished dependent would have had; they are
	// not in impact.
	leftInPlace []schedule.Change
	revision    string
}

// linkedOptions is the caller's side of a linked change, a move or a link write.
type linkedOptions struct {
	actor, reason, revision string
	preview, acknowledge    bool
	access                  models.CommitmentAccess
	rootAuthorized          bool // occurrence renegotiation already checked root authority
}

// MoveLinkedDate moves one date of an item's outstanding occurrence and the
// dates linked below it. See the file comment for the deadline rule.
func (s *Store) MoveLinkedDate(workspaceID string, in models.LinkedMoveInput) (*models.LinkedMoveResult, error) {
	role := linkRole(in.Role)
	if role == "" {
		return nil, fieldErr("role", "enum", "role must be earliest, target, deadline or occupied_until")
	}
	to, err := time.Parse(time.RFC3339, in.To)
	if err != nil {
		return nil, fieldErr("to", "format", "to must be an RFC3339 instant")
	}
	if !in.Access.Readable[in.ItemID] {
		return nil, ErrScheduleNotFound
	}
	if !in.Access.Editable[in.ItemID] {
		return nil, ErrLinkEditRequired
	}
	reason := strings.TrimSpace(in.Reason)
	actor := in.Actor
	if actor == "" {
		actor = "user"
	}
	// The terminal filter reads through the pool: load it before the transaction holds a connection.
	openFilter, openArgs := s.nonTerminalFilter(workspaceID, "i")
	if s.beforeCommitmentPin != nil {
		s.beforeCommitmentPin()
	}
	budget := s.newLinkBudget()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("move linked date: %w", err)
	}
	defer tx.Rollback()
	if err := s.setTxTimeoutsCtx(budget.ctx, tx, linkTxBudget, linkTxBudget); err != nil {
		return nil, err
	}
	c, err := s.pinCommitmentTx(tx, workspaceID, openFilter, openArgs, false, in.ItemID)
	if err != nil {
		return nil, err
	}
	p, err := s.planLinkedTx(tx, c, workspaceID, in.ItemID, role, to.UTC().Truncate(time.Second), true)
	if err != nil {
		return nil, err
	}
	res, apply, err := s.settleLinkedPlan(p, budget, linkedOptions{actor: actor, reason: reason, revision: in.Revision, preview: in.Preview, acknowledge: in.AcknowledgeViolations, access: in.Access})
	if err != nil || !apply {
		return res, err
	}
	nowT := s.linkNow().UTC().Truncate(time.Second)
	if lapsed, err := s.lapseDueInPlanTx(tx, p, nowT); err != nil {
		return nil, err
	} else if lapsed {
		if err := budget.commit(tx); err != nil {
			return nil, err
		}
		return nil, ErrCommitmentChanged
	}
	if err := s.applyLinkedPlanTx(tx, p, actor, linkNoticeActor(actor, in.ActorUserID), reason, "Moved with a linked date", nowT, budget); err != nil {
		return nil, err
	}
	if err := budget.commit(tx); err != nil {
		return nil, err
	}
	return res, nil
}

// settleLinkedPlan applies the rules that stand between a plan and its writes:
// the time budget, authority over each hard deadline, the one reason of a
// renegotiation, the preview revision and the acknowledgement of a date outside
// its own bounds. It returns the result the caller may read, and whether the
// plan is to be applied (not a preview, and not empty).
func (s *Store) settleLinkedPlan(p *linkedPlan, budget *linkBudget, o linkedOptions) (*models.LinkedMoveResult, bool, error) {
	if err := budget.check(); err != nil {
		return nil, false, err
	}
	// A preview the world has moved past is the first refusal, before authority
	// or a reason: the token is opaque and fixed-length and the answer is the same
	// for any hidden change, so it reveals nothing. With no revision supplied the
	// order below stands.
	if !o.preview && o.revision != "" && o.revision != p.revision {
		return nil, false, ErrCommitmentChanged
	}
	// Authority over each deadline the change makes. The same refusal for a
	// deadline the caller cannot read, cannot edit, or one among many.
	for _, ch := range p.impact.HardDeadlines {
		if o.rootAuthorized && ch.Node.Schedule == p.root {
			continue
		}
		if !o.access.Editable[p.c.itemOf(ch.Node.Schedule)] {
			return nil, false, ErrDeadlineApprovalRequired
		}
	}
	renegotiates := len(p.impact.HardDeadlines) > 0
	// Only a bound the caller can read gates the apply. A dependent they cannot
	// read moves as planned and its crossed bound changes no answer: it neither
	// asks for a revision or an acknowledgement nor refuses, so the response is
	// byte-identical with and without it. (Its own deadline is never touched by
	// the cascade; a deadline the cascade does change is the refusal above.)
	var violations []schedule.Violation
	for _, v := range p.impact.Violations {
		if v.Node.Schedule != p.root && o.access.Readable[p.c.itemOf(v.Node.Schedule)] {
			violations = append(violations, v)
		}
	}
	if renegotiates && o.reason == "" {
		return nil, false, fieldErr("reason", "required", "moving a hard deadline needs a reason")
	}
	res := s.linkedResult(p, o.access, renegotiates)
	if o.preview {
		return res, false, nil
	}
	if (renegotiates || len(violations) > 0) && o.revision == "" {
		return nil, false, fieldErr("revision", "required", "this change needs the preview revision")
	}
	if len(violations) > 0 && !o.acknowledge {
		return nil, false, ErrLinkViolationAck
	}
	return res, len(p.impact.Changes) > 0, nil
}

// planLinkedTx plans the cascade of a change to one date of itemID's
// outstanding occurrence over the component c that pinCommitmentTx read (the
// caller may have put a link of its own into c.links). It writes nothing.
//
// A direct move sets the date to `to`. With direct false nothing is moved by
// hand: the date stays where it is and the plan brings every date linked below
// it to where its link puts it. That plan is nil when itemID has no outstanding
// occurrence, so there is nothing to bring.
func (s *Store) planLinkedTx(tx *sql.Tx, c *commitmentComponent, workspaceID, itemID string, role schedule.Role, to time.Time, direct bool) (*linkedPlan, error) {
	p := &linkedPlan{c: c, nodes: map[string]*linkNode{}}
	for id, sc := range c.scheds {
		if sc.ItemID == itemID {
			p.root = id
		}
		if !c.live[id] {
			continue
		}
		n, err := s.linkNodeTx(tx, sc)
		if err != nil {
			return nil, err
		}
		if n != nil {
			p.nodes[id] = n
		}
	}
	root := p.nodes[p.root]
	if root == nil {
		if !direct {
			return nil, nil
		}
		return nil, fieldErr("schedule", "ended", "the commitment has no outstanding occurrence to move")
	}
	var edges []schedule.Edge
	for i := range c.links {
		l := c.links[i]
		parent, child := p.nodes[l.ParentSchedule], p.nodes[l.ChildSchedule]
		if parent == nil || child == nil {
			continue
		}
		off, skip, err := s.effectiveLinkOffset(tx, l, child.key, l.ID != "" && l.ID == c.explicitLink)
		if err != nil {
			return nil, err
		}
		// A no-longer-needed mark skips the occurrence even when the link's offset
		// is written explicitly: the mark is the user's, and only the offset
		// override is replaced by an explicit write.
		if skip {
			continue
		}
		l.Offset = off
		// A direct move of the child is the child's own drag, so it is recorded
		// even when the parent is finished.
		if direct && l.ChildSchedule == p.root && l.ChildRole == linkRoleName(role) {
			p.incoming, p.incomingOffset = &c.links[i], off
		}
		// A finished dependent is history: nothing moves it, and nothing below it
		// follows, so a link out of a finished non-root node is not an edge.
		if c.terminal[l.ParentSchedule] && l.ParentSchedule != p.root {
			continue
		}
		e, err := l.edge()
		if err != nil {
			return nil, err
		}
		edges = append(edges, e)
	}
	g, err := schedule.NewGraph(edges)
	if err != nil {
		return nil, redactLinkError(err)
	}
	st := schedule.State{}
	for id, n := range p.nodes {
		st[schedule.Node{Schedule: id}] = schedule.NodeState{Dates: n.dates}
	}
	cur, ok := root.dates[role]
	if !ok {
		return nil, fieldErr("role", "combination", "the commitment has no "+linkRoleName(role)+" date to move")
	}
	if !direct {
		to = cur
	}
	p.to = to
	if direct && role == schedule.RoleTarget && !to.After(s.linkNow()) {
		if _, hasDeadline := root.dates[schedule.RoleDeadline]; hasDeadline {
			return nil, fieldErr("to", "range", "to must be after now on a schedule with a hard deadline")
		}
	}
	p.impact, err = g.Plan(st, schedule.Move{Node: schedule.Node{Schedule: p.root}, Role: role, To: to}, schedule.LinkLimits{})
	if err != nil {
		return nil, redactLinkError(err)
	}
	p.dropFinished()
	// Store backstop for the range rule: a derived wall read in the child's own
	// zone must be a supported year (1970..2200), whatever zone the offset used.
	for _, ch := range p.impact.Changes {
		if y := ch.To.In(p.nodes[ch.Node.Schedule].rec).Year(); y < 1970 || y > 2200 {
			return nil, &LinkRefusedError{Code: "link_invalid", Detail: "derived date is out of range"}
		}
	}
	p.revision, err = s.linkedRevision(workspaceID, itemID, role, to, p)
	return p, err
}

// dropFinished takes the finished non-root nodes out of the impact. The edges
// out of such a node were never in the graph, so what it would have changed is
// only itself: those changes are kept aside for the preview, and its violations
// and hard deadlines go with it (a closed promise is not renegotiated).
func (p *linkedPlan) dropFinished() {
	finished := func(sched string) bool { return p.c.terminal[sched] && sched != p.root }
	var changes, hard []schedule.Change
	for _, ch := range p.impact.Changes {
		if finished(ch.Node.Schedule) {
			p.leftInPlace = append(p.leftInPlace, ch)
		} else {
			changes = append(changes, ch)
		}
	}
	for _, ch := range p.impact.HardDeadlines {
		if !finished(ch.Node.Schedule) {
			hard = append(hard, ch)
		}
	}
	var viol []schedule.Violation
	for _, v := range p.impact.Violations {
		if !finished(v.Node.Schedule) {
			viol = append(viol, v)
		}
	}
	p.impact.Changes, p.impact.HardDeadlines, p.impact.Violations = changes, hard, viol
	p.impact.RenegotiationRequired = len(hard) > 0
}

// linkedRevision is an opaque fixed-length token over everything the plan
// depends on: the move, the component's links, each schedule's outstanding
// state and dates, and the computed changes. Hidden parts go in the hash, so a
// change to them invalidates a preview; the token shows nothing of them.
func (s *Store) linkedRevision(workspaceID, itemID string, role schedule.Role, to time.Time, p *linkedPlan) (string, error) {
	type nodeState struct {
		ID, Updated, NextAt, NextWall, Override, Key string
		Terminal                                     bool
		Dates                                        map[string]string
	}
	var nodes []nodeState
	for _, id := range keysOfNodes(p.nodes) {
		n := p.nodes[id]
		dates := map[string]string{}
		for r, at := range n.dates {
			dates[string(r)] = at.UTC().Format(time.RFC3339Nano)
		}
		nodes = append(nodes, nodeState{id, n.sc.UpdatedAt, derefStr(n.sc.NextAt), derefStr(n.sc.NextWall), derefStr(n.sc.NextOverrideAt), n.key, p.c.terminal[id], dates})
	}
	b, err := json.Marshal(struct {
		Workspace, Item, Role, To string
		Links                     []linkRow
		Nodes                     []nodeState
		Changes                   []schedule.Change
	}{workspaceID, itemID, string(role), to.Format(time.RFC3339Nano), sortedLinks(p.c.links), nodes, p.impact.Changes})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// sortedLinks is a copy in id order (a link being written has no id yet and
// sorts first, in preview and in apply alike).
func sortedLinks(ls []linkRow) []linkRow {
	out := append([]linkRow(nil), ls...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func keysOfNodes(m map[string]*linkNode) []string {
	set := make(map[string]bool, len(m))
	for k := range m {
		set[k] = true
	}
	return keysOf(set)
}

// linkedResult keeps only the changes and violations on items the caller can read.
func (s *Store) linkedResult(p *linkedPlan, acc models.CommitmentAccess, renegotiates bool) *models.LinkedMoveResult {
	res := &models.LinkedMoveResult{Revision: p.revision, Changes: []models.LinkedDateChange{}, RenegotiatesDeadline: renegotiates}
	format := func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.UTC().Format(time.RFC3339Nano)
	}
	for _, ch := range p.impact.Changes {
		if item := p.c.itemOf(ch.Node.Schedule); acc.Readable[item] {
			res.Changes = append(res.Changes, models.LinkedDateChange{ItemID: item, Role: linkRoleName(ch.Role), From: format(ch.From), To: format(ch.To), Root: ch.Root})
		}
	}
	for _, v := range p.impact.Violations {
		if v.Node.Schedule == p.root {
			continue
		}
		if item := p.c.itemOf(v.Node.Schedule); acc.Readable[item] {
			res.Violations = append(res.Violations, models.LinkedDateChange{ItemID: item, Role: linkRoleName(v.Role), To: format(v.Value)})
		}
	}
	// A finished dependent the caller can read is named; the rest are not counted.
	for _, ch := range p.leftInPlace {
		if item := p.c.itemOf(ch.Node.Schedule); acc.Readable[item] {
			res.LeftInPlace = append(res.LeftInPlace, models.LinkedDateChange{ItemID: item, Role: linkRoleName(ch.Role), From: format(ch.From), To: format(ch.To)})
		}
	}
	return res
}

// applyLinkedPlanTx writes every change in plan order (the moved date first),
// each with its journal row, and checks the budget between writes. The first
// row is the root of the lineage the rest name. why is the journal reason of a
// dependent date that moved with another.
func (s *Store) applyLinkedPlanTx(tx *sql.Tx, p *linkedPlan, actor, noticeActor, reason, why string, nowT time.Time, budget *linkBudget) error {
	ts := nowT.Format(time.RFC3339)
	rootChange := newID()
	if p.incoming != nil && len(p.impact.Changes) > 0 {
		// A child dragged away from its parent keeps the link at the new offset.
		if err := s.keepLinkAtNewOffsetTx(tx, p); err != nil {
			return err
		}
	}
	for i, ch := range p.impact.Changes {
		if err := budget.check(); err != nil {
			return err
		}
		n := p.nodes[ch.Node.Schedule]
		name := linkRoleName(ch.Role)
		before, after := dateJSON(name, ch.From), dateJSON(name, ch.To)
		why, source := why, "system"
		switch {
		case ch.Role == schedule.RoleDeadline:
			why, source = reason, "user"
		case ch.Root:
			why = "Rescheduled"
			if reason != "" {
				why, source = reason, "user"
			}
		}
		if ch.Role == schedule.RoleDeadline {
			// The pin's read decides, not a second one.
			if !p.c.terminal[n.sc.ID] {
				// Nothing erases a miss that already happened.
				if miss, err := s.deadlineMissCandidateTx(tx, n.sc.ID, nowT); err != nil {
					return err
				} else if miss != nil {
					if err := s.recordDeadlineMissTx(tx, *miss); err != nil {
						return err
					}
				}
			}
		}
		if err := s.writeLinkedDateTx(tx, n, ch, nowT, ts, actor); err != nil {
			return err
		}
		id, parent := newID(), rootChange
		if i == 0 {
			id, parent = rootChange, ""
		}
		root := rootChange
		if err := s.journalScheduleChangeRowTx(tx, id, n.sc.ID, "move", n.key, name, before, after, actor, why, source, parent, root); err != nil {
			return err
		}
	}
	return s.enqueueLinkedMoveNoticesTx(tx, p, rootChange, noticeActor)
}

// lapseDueInPlanTx closes due lapsing occurrences before any planned date or
// link write. The component pin holds their schedules, never their item rows.
// A stays-owed miss belongs to the normal apply path, not a refused plan.
func (s *Store) lapseDueInPlanTx(tx *sql.Tx, p *linkedPlan, nowT time.Time) (bool, error) {
	if p == nil {
		return false, nil
	}
	ids := map[string]bool{}
	for _, ch := range p.impact.Changes {
		if !p.c.terminal[ch.Node.Schedule] {
			ids[ch.Node.Schedule] = true
		}
	}
	lapsed := false
	for _, id := range keysOf(ids) {
		d, state, err := s.deadlineStateTx(tx, id)
		if err != nil {
			return false, err
		}
		if d == nil || d.behavior != "lapses" || !d.dueAfterConversion(nowT, state) {
			continue
		}
		if !state.recorded {
			if err := s.recordDeadlineMissTx(tx, *d); err != nil {
				return false, err
			}
		}
		closed, err := s.lapseOccurrenceTx(tx, *d, nowT)
		if err != nil {
			return false, err
		}
		// A row that cannot be expanded keeps its miss and stays outstanding, so
		// a refusal here would repeat on every retry.
		if closed != nil {
			lapsed = true
		}
	}
	return lapsed, nil
}

func dateJSON(role string, at time.Time) string {
	if at.IsZero() {
		return ""
	}
	if role == "deadline" {
		b, _ := json.Marshal(map[string]string{"deadline": at.UTC().Format(time.RFC3339Nano)})
		return string(b)
	}
	b, _ := json.Marshal(map[string]string{role: at.UTC().Format(time.RFC3339Nano)})
	return string(b)
}

// writeLinkedDateTx stores one changed date as the outstanding occurrence's
// role override. A target also follows the move rules of unit 3: the occurrence
// reads at its new target (next_override_at, with next_at pinned to the instant
// it was read at, so a floating row keeps its identity) and the managed
// reminder is re-armed at the target (the target itself when the lead would put
// the fire at or before now).
//
// A target that lands in the past arms nothing and fires nothing: the
// occurrence stays outstanding and reads at its new target as passed, its
// deadline is untouched, and an armed reminder for the old target is cancelled.
// A reminder that already fired is left alone.
func (s *Store) writeLinkedDateTx(tx *sql.Tx, n *linkNode, ch schedule.Change, nowT time.Time, ts, actor string) error {
	name := linkRoleName(ch.Role)
	sc, to := n.sc, ch.To.UTC().Truncate(time.Second)
	if ch.Role != schedule.RoleTarget {
		return s.upsertScheduleOverrideTx(tx, sc.ID, n.key, name, to, nil)
	}
	if _, err := tx.Exec(s.q(`UPDATE item_schedules SET next_at = ?, next_override_at = ?, updated_at = ? WHERE id = ?`),
		n.x.Format(time.RFC3339), to.Format(time.RFC3339), ts, sc.ID); err != nil {
		return fmt.Errorf("move linked target: %w", err)
	}
	if err := s.upsertOccurrenceTx(tx, sc.ID, n.x.Format(time.RFC3339), *sc.NextWall, "moved", "", actor, ts); err != nil {
		return err
	}
	if err := s.upsertScheduleOverrideTx(tx, sc.ID, n.key, name, to, nil); err != nil {
		return err
	}
	if !to.After(nowT) {
		return s.cancelArmedReminderTx(tx, sc.ID)
	}
	fire, err := reminderFireAt(sc, n.rec, n.x, *sc.NextWall, to, true, to, nowT)
	if err != nil {
		return err
	}
	return s.armManagedReminderTx(tx, sc, NormalizeInstant(fire))
}

// cancelArmedReminderTx deletes the schedule's managed reminder only while it is
// armed. A row that already fired has gone out and stays, so ReminderFired and
// the instant a floating occurrence reads do not change under it.
func (s *Store) cancelArmedReminderTx(tx *sql.Tx, scheduleID string) error {
	if _, err := tx.Exec(s.q(`DELETE FROM item_reminders WHERE schedule_id = ? AND fired_at IS NULL`), scheduleID); err != nil {
		return fmt.Errorf("cancel armed reminder: %w", err)
	}
	return nil
}

// keepLinkAtNewOffsetTx records the dragged child's new offset for its
// outstanding occurrence, against the parent date as it is now.
func (s *Store) keepLinkAtNewOffsetTx(tx *sql.Tx, p *linkedPlan) error {
	l := p.incoming
	parent := p.nodes[l.ParentSchedule]
	to := p.to
	cur, err := offsetOf(p.incomingOffset)
	if err != nil {
		return err
	}
	off, err := schedule.OffsetFromDrop(cur.Mode, parent.dates[linkRole(l.ParentRole)], to, cur.Zone, cur.AllDay)
	if err != nil {
		return redactLinkError(err)
	}
	stored := storedOffset(off)
	return s.upsertLinkOverrideTx(tx, l.ID, p.nodes[l.ChildSchedule].key, &stored, "")
}

// replaceLinkOverrideTx drops the per-occurrence offset a drag left on the
// child's outstanding occurrence (n; nil when it has none) when the link's
// offset is written explicitly: the new offset is the offset for that occurrence
// now. The reset is journalled on the child under actor (an update row naming
// the offset it replaced). A finished child is history and is left alone, and a
// no-longer-needed mark stays; its occurrence was skipped by the plan.
func (s *Store) replaceLinkOverrideTx(tx *sql.Tx, c *commitmentComponent, n *linkNode, linkID string, now models.ScheduleLinkOffset, actor string) error {
	if n == nil || c.terminal[n.sc.ID] {
		return nil
	}
	var mode sql.NullString
	var o models.ScheduleLinkOffset
	var allDay int
	err := tx.QueryRow(s.q(`SELECT offset_mode,offset_elapsed,offset_days,offset_clock,offset_all_day,offset_zone FROM schedule_link_overrides WHERE link_id=? AND occurrence_key=?`), linkID, n.key).
		Scan(&mode, &o.ElapsedSecs, &o.Days, &o.ClockSecs, &allDay, &o.Zone)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !mode.Valid) {
		return nil
	} else if err != nil {
		return fmt.Errorf("read link override: %w", err)
	}
	o.Mode, o.AllDay = mode.String, allDay != 0
	if _, err := tx.Exec(s.q(`UPDATE schedule_link_overrides SET offset_mode=NULL,offset_elapsed=0,offset_days=0,offset_clock=0,offset_all_day=0,offset_zone='',updated_at=? WHERE link_id=? AND occurrence_key=?`), nowNano(), linkID, n.key); err != nil {
		return fmt.Errorf("clear link override: %w", err)
	}
	if _, err := tx.Exec(s.q(`DELETE FROM schedule_link_overrides WHERE link_id=? AND occurrence_key=? AND offset_mode IS NULL AND no_longer_needed_at IS NULL`), linkID, n.key); err != nil {
		return fmt.Errorf("clear link override: %w", err)
	}
	before, _ := json.Marshal(o)
	after, _ := json.Marshal(now)
	return s.journalScheduleChangeRowTx(tx, newID(), n.sc.ID, "update", n.key, "", string(before), string(after), actor, "Link offset replaced a dragged offset", "system", "", "")
}
