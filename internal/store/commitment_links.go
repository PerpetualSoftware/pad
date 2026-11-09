package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/schedule"
)

// Time links between schedules (TASK-4353, DOC-4348 unit 5, store level).
//
// A link ties one role date of a child schedule to one role date of a parent
// schedule in the same workspace. The structure is per schedule; the dates are
// the schedules outstanding occurrences. The pure layer (internal/schedule)
// decides the arithmetic, cycles and limits; this file stores the links and
// keeps what that layer says out of reach of a reader who cannot read an end.
//
// LOCK ORDER (Postgres). Every link writer and every cascade runs
// pinCommitmentTx: the ROOT ITEM first (FOR NO KEY UPDATE), then the workspace
// (its own statement), then the link component is read, then the component's
// schedule rows ascending by id, then their reminder rows ascending by schedule
// id. That extends the existing item, workspace, schedule, reminder order
// (schedule_fire.go) and nothing requests an item or workspace row after a
// schedule. The cascade locks NO child item row: child status and liveness are
// plain reads, and the cascade writes no item row. Resolve, the tick and the
// evaluator keep their single-item pin; it takes the workspace too, so a cascade
// and any of them serialise at the workspace lock, and a resolve of a child
// (which holds the child item and waits for the workspace) never needs anything
// the cascade holds.
//
// The seeds and the link component are read before the schedule locks, and a
// cross-workspace move takes no workspace row, so it can commit while a link
// writer waits for a schedule lock. After the locks the pin re-reads liveness
// and the stored links, and answers ErrScheduleLinkNotFound when an end moved
// away or its link is gone. It takes no lock for that.

// maxLinkComponent bounds the links one transaction will read. The per-move
// limits (50 nodes, 16 hops) bound a footprint, not a whole component.
const maxLinkComponent = 500

var (
	// ErrScheduleLinkNotFound is the answer for a link, or an end of one, the caller cannot read.
	ErrScheduleLinkNotFound = errors.New("schedule link not found")
	// ErrScheduleLinked refuses an edit of a schedule that is an end of a link
	// when the edit would shift a date the link supplies: its timing, or a role
	// an edge uses. Nothing is written. The route maps it to 409.
	ErrScheduleLinked = errors.New("this schedule is linked; unlink it before changing its timing or a linked role")
	// ErrLinkEditRequired refuses a link or gate write without edit on both ends.
	ErrLinkEditRequired = errors.New("linking commitments needs edit on both items")
	// ErrLinkViolationAck refuses an apply that would put a date outside one of
	// its own bounds unless the caller acknowledged it.
	ErrLinkViolationAck = errors.New("a linked date lands outside one of its own bounds; acknowledge to apply")
	// ErrLinkBudget refuses a change that outlasted its time budget. Nothing was written.
	ErrLinkBudget = errors.New("the linked change took too long and was not applied")
	// ErrDeadlineApprovalRequired is the one refusal for a cascade that changes a
	// hard deadline the caller has no authority over.
	ErrDeadlineApprovalRequired error = &DeadlineApprovalRequiredError{}
)

// DeadlineApprovalRequiredError carries nothing: it is byte-identical for one
// hidden dependent or many, and names no item and no count. 5c maps it to
// 409 deadline_approval_required.
type DeadlineApprovalRequiredError struct{}

func (*DeadlineApprovalRequiredError) Error() string {
	return "this change moves a hard deadline that needs its owner's approval"
}

// LinkRefusedError is a pure-layer refusal with the node ids, paths and counts
// removed. Code is one of link_cycle, link_self, link_duplicate_incoming,
// link_limit, link_invalid; Detail is set only for link_invalid and is a fixed
// text from the pure layer.
type LinkRefusedError struct{ Code, Detail string }

func (e *LinkRefusedError) Error() string {
	if e.Detail != "" {
		return e.Code + ": " + e.Detail
	}
	return e.Code
}

// redactLinkError replaces a pure-layer error by one that cannot name a node or
// show a count (LinkError.Path carries node ids; LimitError.Got is Limit+1).
func redactLinkError(err error) error {
	var le *schedule.LinkError
	var lim *schedule.LimitError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &lim):
		return &LinkRefusedError{Code: "link_limit"}
	case errors.As(err, &le):
		switch {
		case errors.Is(le, schedule.ErrLinkCycle):
			return &LinkRefusedError{Code: "link_cycle"}
		case errors.Is(le, schedule.ErrLinkSelf):
			return &LinkRefusedError{Code: "link_self"}
		case errors.Is(le, schedule.ErrLinkDuplicateIncoming):
			return &LinkRefusedError{Code: "link_duplicate_incoming"}
		}
		return &LinkRefusedError{Code: "link_invalid", Detail: le.Detail}
	}
	return err
}

var linkRoles = []string{"earliest", "target", "deadline", "occupied_until"}

func linkRole(name string) schedule.Role {
	switch name {
	case "earliest":
		return schedule.RoleEarliest
	case "target":
		return schedule.RoleTarget
	case "deadline":
		return schedule.RoleDeadline
	case "occupied_until":
		return schedule.RoleOccupiedEnd
	}
	return ""
}

func linkRoleName(r schedule.Role) string {
	if r == schedule.RoleOccupiedEnd {
		return "occupied_until"
	}
	return string(r)
}

// linkRow is a stored link by schedule.
type linkRow struct {
	ID, ParentSchedule, ParentRole, ChildSchedule, ChildRole string
	Offset                                                   models.ScheduleLinkOffset
	CancelPolicy, CreatedAt, UpdatedAt                       string
}

const linkColumns = `id,parent_schedule_id,parent_role,child_schedule_id,child_role,offset_mode,offset_elapsed,offset_days,offset_clock,offset_all_day,offset_zone,cancel_policy,created_at,updated_at`

func scanLink(r interface{ Scan(...any) error }) (linkRow, error) {
	var l linkRow
	var allDay int
	err := r.Scan(&l.ID, &l.ParentSchedule, &l.ParentRole, &l.ChildSchedule, &l.ChildRole, &l.Offset.Mode, &l.Offset.ElapsedSecs, &l.Offset.Days, &l.Offset.ClockSecs, &allDay, &l.Offset.Zone, &l.CancelPolicy, &l.CreatedAt, &l.UpdatedAt)
	l.Offset.AllDay = allDay != 0
	return l, err
}

func (o linkRow) edgeOffset() (schedule.Offset, error) { return offsetOf(o.Offset) }

// offsetOf converts a stored offset to the pure layer's.
func offsetOf(o models.ScheduleLinkOffset) (schedule.Offset, error) {
	out := schedule.Offset{Mode: schedule.OffsetMode(o.Mode), Days: o.Days, AllDay: o.AllDay, Zone: o.Zone}
	const maxSecs = int64(300 * 366 * 86400)
	if o.ElapsedSecs > maxSecs || o.ElapsedSecs < -maxSecs || o.ClockSecs < 0 || o.ClockSecs >= 86400 {
		return out, &LinkRefusedError{Code: "link_invalid", Detail: "offset is out of range"}
	}
	out.Elapsed = time.Duration(o.ElapsedSecs) * time.Second
	out.Hour, out.Minute, out.Second = o.ClockSecs/3600, o.ClockSecs%3600/60, o.ClockSecs%60
	if err := out.Validate(); err != nil {
		return out, redactLinkError(err)
	}
	return out, nil
}

func storedOffset(o schedule.Offset) models.ScheduleLinkOffset {
	return models.ScheduleLinkOffset{Mode: string(o.Mode), ElapsedSecs: int64(o.Elapsed / time.Second), Days: o.Days,
		ClockSecs: o.Hour*3600 + o.Minute*60 + o.Second, AllDay: o.AllDay, Zone: o.Zone}
}

func (l linkRow) edge() (schedule.Edge, error) {
	off, err := l.edgeOffset()
	return schedule.Edge{Parent: schedule.Node{Schedule: l.ParentSchedule}, ParentRole: linkRole(l.ParentRole),
		Child: schedule.Node{Schedule: l.ChildSchedule}, ChildRole: linkRole(l.ChildRole), Offset: off}, err
}

// commitmentComponent is what pinCommitmentTx read under the locks.
type commitmentComponent struct {
	links  []linkRow
	scheds map[string]*models.ItemSchedule
	live   map[string]bool // schedule id -> its item is live
	// terminal marks the schedules whose live item is done or abandoned. It comes
	// from the same statement as live, so the plan, the preview and the writes
	// all use one answer.
	terminal map[string]bool
	// explicitLink is the link whose offset the caller is writing: its
	// per-occurrence offset override must not stand in for the new offset.
	explicitLink string
}

func (c *commitmentComponent) itemOf(scheduleID string) string {
	if sc := c.scheds[scheduleID]; sc != nil {
		return sc.ItemID
	}
	return ""
}

// pinCommitmentTx takes the locks in the order the file comment names and
// returns the component of the root schedule plus the schedules of any extra
// items. A root item with no schedule is ErrScheduleNotFound.
//
// openFilter and openArgs are nonTerminalFilter's, loaded before Begin; empty
// means terminal state is not read. overCap lets a delete or an update read a
// component that already holds more than maxLinkComponent links, so it can be
// repaired.
func (s *Store) pinCommitmentTx(tx *sql.Tx, workspaceID, openFilter string, openArgs []any, overCap bool, rootItemID string, extraItemIDs ...string) (*commitmentComponent, error) {
	return s.pinCommitmentOpt(tx, workspaceID, openFilter, openArgs, overCap, false, rootItemID, extraItemIDs...)
}

// pinCommitmentOpt is pinCommitmentTx. With withDeleted the root item and the
// seed schedules are found whether or not their item is soft-deleted: the one
// caller is DeleteScheduleLink, which must be able to remove a link whose end
// was deleted. The lock order is the same (root item by id, workspace, then the
// component), a deleted root is locked like a live one, and liveness is still
// read from the items, so a deleted end is never live in the component.
func (s *Store) pinCommitmentOpt(tx *sql.Tx, workspaceID, openFilter string, openArgs []any, overCap, withDeleted bool, rootItemID string, extraItemIDs ...string) (*commitmentComponent, error) {
	liveItem := ` AND deleted_at IS NULL`
	liveJoin := ` AND i.deleted_at IS NULL`
	if withDeleted {
		liveItem, liveJoin = "", ""
	}
	pg := s.dialect.Driver() == DriverPostgres
	lock := ""
	if pg {
		lock = ` FOR NO KEY UPDATE`
	}
	var one string
	err := tx.QueryRow(s.q(`SELECT id FROM items WHERE id=? AND workspace_id=?`+liveItem+lock), rootItemID, workspaceID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrScheduleItemGone
	} else if err != nil {
		return nil, fmt.Errorf("pin commitment item: %w", err)
	}
	if err := tx.QueryRow(s.q(`SELECT id FROM workspaces WHERE id=? AND deleted_at IS NULL`+lock), workspaceID).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrScheduleItemGone
		}
		return nil, fmt.Errorf("pin commitment workspace: %w", err)
	}
	seeds := map[string]bool{}
	var seedOrder []string // the root's schedule first, then each extra's
	for i, item := range append([]string{rootItemID}, extraItemIDs...) {
		var id string
		err := tx.QueryRow(s.q(`SELECT sc.id FROM item_schedules sc JOIN items i ON i.id=sc.item_id WHERE sc.item_id=? AND sc.workspace_id=?`+liveJoin), item, workspaceID).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			if i == 0 {
				return nil, ErrScheduleNotFound
			}
			return nil, ErrScheduleLinkNotFound
		} else if err != nil {
			return nil, fmt.Errorf("pin commitment schedule: %w", err)
		}
		seeds[id] = true
		seedOrder = append(seedOrder, id)
	}
	c := &commitmentComponent{scheds: map[string]*models.ItemSchedule{}, live: map[string]bool{}, terminal: map[string]bool{}}
	known := map[string]bool{}
	for frontier := keysOf(seeds); len(frontier) > 0; {
		for _, id := range frontier {
			known[id] = true
		}
		args := make([]any, 0, 2*len(frontier)+1)
		args = append(args, workspaceID)
		for i := 0; i < 2; i++ {
			for _, id := range frontier {
				args = append(args, id)
			}
		}
		ph := placeholders(len(frontier))
		rows, err := tx.Query(s.q(`SELECT `+linkColumns+` FROM schedule_links WHERE workspace_id=? AND (parent_schedule_id IN (`+ph+`) OR child_schedule_id IN (`+ph+`)) ORDER BY id`), args...)
		if err != nil {
			return nil, fmt.Errorf("read link component: %w", err)
		}
		frontier = nil
		for rows.Next() {
			l, err := scanLink(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			if !containsLink(c.links, l.ID) {
				c.links = append(c.links, l)
			}
			for _, id := range []string{l.ParentSchedule, l.ChildSchedule} {
				if !known[id] {
					known[id] = true
					frontier = append(frontier, id)
				}
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
		if len(c.links) > maxLinkComponent && !overCap {
			return nil, &LinkRefusedError{Code: "link_limit"}
		}
	}
	// Read order must not reach the revision: links are held in id order.
	sort.Slice(c.links, func(i, j int) bool { return c.links[i].ID < c.links[j].ID })
	ids := keysOf(known)
	sort.Strings(ids)
	ph := placeholders(len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	// Schedule rows ascending by id, then the managed reminder rows ascending by schedule id.
	if pg {
		for _, q := range []string{
			`SELECT id FROM item_schedules WHERE id IN (` + ph + `) ORDER BY id FOR NO KEY UPDATE`,
			`SELECT id FROM item_reminders WHERE schedule_id IN (` + ph + `) ORDER BY schedule_id FOR NO KEY UPDATE`,
		} {
			if err := s.drainIDs(tx, q, args); err != nil {
				return nil, err
			}
		}
	}
	if s.afterCommitmentLocks != nil {
		s.afterCommitmentLocks(tx)
	}
	for _, id := range ids {
		sc, err := s.getScheduleByIDQ(tx, id)
		if err != nil {
			return nil, err
		}
		if sc != nil && sc.WorkspaceID == workspaceID {
			c.scheds[id] = sc
		}
	}
	// Liveness and terminal state are one plain read, after the schedule and
	// reminder locks: the cascade never locks a child item.
	openExpr, liveArgs := "1", args
	if openFilter != "" {
		openExpr, liveArgs = openFilter, append(append([]any{}, openArgs...), args...)
	}
	rows, err := tx.Query(s.q(`SELECT sc.id, `+openExpr+` FROM item_schedules sc JOIN items i ON i.id=sc.item_id WHERE sc.id IN (`+ph+`) AND i.deleted_at IS NULL`), liveArgs...)
	if err != nil {
		return nil, fmt.Errorf("read link liveness: %w", err)
	}
	for rows.Next() {
		var id string
		var open bool
		if err := rows.Scan(&id, &open); err != nil {
			rows.Close()
			return nil, err
		}
		c.live[id] = true
		c.terminal[id] = !open
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	// Everything above up to the schedule locks was read without them. On
	// Postgres a transaction that held a schedule lock (a cross-workspace move
	// takes no workspace row) may have committed while this one waited: it can
	// have archived an end, ended its schedule and deleted its links. The reads
	// after the locks saw that, so check the seeds and the links against them. No
	// new lock is taken. Nothing can have ADDED a link to the component: every
	// link writer holds the workspace row this transaction holds.
	for i, id := range seedOrder {
		switch {
		case c.scheds[id] == nil && i == 0:
			return nil, ErrScheduleNotFound
		case !withDeleted && !c.live[id] && i == 0:
			return nil, ErrScheduleItemGone
		case c.scheds[id] == nil || (!withDeleted && !c.live[id]):
			return nil, ErrScheduleLinkNotFound
		}
	}
	if err := s.keepStoredLinks(tx, workspaceID, c); err != nil {
		return nil, err
	}
	return c, nil
}

// keepStoredLinks drops from the component the links that no longer exist.
func (s *Store) keepStoredLinks(tx *sql.Tx, workspaceID string, c *commitmentComponent) error {
	if len(c.links) == 0 {
		return nil
	}
	args := []any{workspaceID}
	for _, l := range c.links {
		args = append(args, l.ID)
	}
	rows, err := tx.Query(s.q(`SELECT id FROM schedule_links WHERE workspace_id=? AND id IN (`+placeholders(len(c.links))+`)`), args...)
	if err != nil {
		return fmt.Errorf("re-read link component: %w", err)
	}
	stored := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		stored[id] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if len(stored) == len(c.links) {
		return nil
	}
	kept := c.links[:0:0]
	for _, l := range c.links {
		if stored[l.ID] {
			kept = append(kept, l)
		}
	}
	c.links = kept
	return nil
}

func (s *Store) drainIDs(tx *sql.Tx, query string, args []any) error {
	rows, err := tx.Query(s.q(query), args...)
	if err != nil {
		return fmt.Errorf("lock link component: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
	}
	return rows.Err()
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func containsLink(ls []linkRow, id string) bool {
	for _, l := range ls {
		if l.ID == id {
			return true
		}
	}
	return false
}

// structuralGraph is every stored link, live or not: the footprint admission
// (CheckLink) must hold whatever is live when a move later runs.
func (c *commitmentComponent) structuralGraph(skip string) (*schedule.Graph, error) {
	var edges []schedule.Edge
	for _, l := range c.links {
		if l.ID == skip {
			continue
		}
		e, err := l.edge()
		if err != nil {
			return nil, err
		}
		edges = append(edges, e)
	}
	g, err := schedule.NewGraph(edges)
	return g, redactLinkError(err)
}

// roleConfigured reports whether a schedule's roles include role.
func roleConfigured(r scheduleRoles, name string) bool {
	switch name {
	case "target":
		return true
	case "earliest":
		return r.EarliestDays != nil
	case "deadline":
		return r.DeadlineDays != nil
	case "occupied_until":
		return r.OccupiedDays != nil
	}
	return false
}

func readableLinkEnds(acc models.CommitmentAccess, a, b string) bool {
	return acc.Readable[a] && acc.Readable[b]
}

// CreateScheduleLink links two commitments. Both ends need edit (the caller's
// decision in access); an end the caller cannot read is reported as not found.
// The 50/16 limits and the cycle rule are checked here, at write time, against
// the whole stored component.
//
// A link supplies the child's date, so writing one moves the child's
// outstanding occurrence, and what hangs below it, to the parent plus the
// offset, through the plan a cascade uses (see linkWritePlanTx). A date that
// already matches moves nothing. With Move.Preview the move is computed and
// nothing is written, the link included.
func (s *Store) CreateScheduleLink(workspaceID string, in models.ScheduleLinkCreate, access models.CommitmentAccess) (*models.ScheduleLinkResult, error) {
	if !access.Readable[in.ParentItemID] || !access.Readable[in.ChildItemID] {
		return nil, ErrScheduleLinkNotFound
	}
	if !access.Editable[in.ParentItemID] || !access.Editable[in.ChildItemID] {
		return nil, ErrLinkEditRequired
	}
	if !oneOf(in.ParentRole, linkRoles) {
		return nil, fieldErr("parent_role", "enum", "parent_role must be earliest, target, deadline or occupied_until")
	}
	if !oneOf(in.ChildRole, linkRoles) {
		return nil, fieldErr("child_role", "enum", "child_role must be earliest, target, deadline or occupied_until")
	}
	policy := in.CancelPolicy
	if policy == "" {
		policy = models.LinkWaitingOn
	}
	if !oneOf(policy, []string{models.LinkWaitingOn, models.LinkCancelChildOccurrence}) {
		return nil, fieldErr("cancel_policy", "enum", "cancel_policy must be waiting_on or cancel_child_occurrence")
	}
	off, err := offsetOf(in.Offset)
	if err != nil {
		return nil, err
	}
	if in.ParentItemID == in.ChildItemID {
		return nil, &LinkRefusedError{Code: "link_self"}
	}
	openFilter, openArgs := s.nonTerminalFilter(workspaceID, "i")
	budget := s.newLinkBudget()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("create schedule link: %w", err)
	}
	defer tx.Rollback()
	if err := s.setTxTimeoutsCtx(budget.ctx, tx, linkTxBudget, linkTxBudget); err != nil {
		return nil, err
	}
	c, err := s.pinCommitmentTx(tx, workspaceID, openFilter, openArgs, false, in.ParentItemID, in.ChildItemID)
	if errors.Is(err, ErrScheduleItemGone) {
		// A soft-deleted parent is the root the pin cannot find: answer as for an
		// unreadable end, which is what a soft-deleted child already gets.
		return nil, ErrScheduleLinkNotFound
	}
	if err != nil {
		return nil, err
	}
	var parent, child *models.ItemSchedule
	for _, sc := range c.scheds {
		switch sc.ItemID {
		case in.ParentItemID:
			parent = sc
		case in.ChildItemID:
			child = sc
		}
	}
	if parent == nil || child == nil {
		return nil, ErrScheduleLinkNotFound
	}
	if len(c.links)+1 > maxLinkComponent {
		return nil, &LinkRefusedError{Code: "link_limit"}
	}
	for _, end := range []struct {
		sc   *models.ItemSchedule
		role string
	}{{parent, in.ParentRole}, {child, in.ChildRole}} {
		roles, err := s.scheduleRolesOf(tx, end.sc)
		if err != nil {
			return nil, err
		}
		if !roleConfigured(roles, end.role) {
			return nil, fieldErr("role", "combination", "the commitment has no "+end.role+" date to link")
		}
		if end.role == "deadline" && roles.Origin != "native" {
			return nil, fieldErr("role", "combination", "a legacy hard deadline cannot be linked; PATCH .../commitment naming a role converts the row to native roles")
		}
	}
	edge := schedule.Edge{Parent: schedule.Node{Schedule: parent.ID}, ParentRole: linkRole(in.ParentRole), Child: schedule.Node{Schedule: child.ID}, ChildRole: linkRole(in.ChildRole), Offset: off}
	g, err := c.structuralGraph("")
	if err != nil {
		return nil, err
	}
	if err := schedule.CheckLink(g, edge, schedule.LinkLimits{}); err != nil {
		return nil, redactLinkError(err)
	}
	// The link as planned carries no id or stamp, so a preview and its apply hash alike.
	l := linkRow{ParentSchedule: parent.ID, ParentRole: in.ParentRole, ChildSchedule: child.ID, ChildRole: in.ChildRole,
		Offset: storedOffset(off), CancelPolicy: policy}
	moved, p, apply, err := s.linkWritePlanTx(tx, workspaceID, c, l, parent, budget, in.Move, access)
	if err != nil {
		return nil, err
	}
	if in.Move.Preview {
		return &models.ScheduleLinkResult{Moved: *moved}, nil
	}
	l.ID, l.CreatedAt, l.UpdatedAt = newID(), nowNano(), nowNano()
	if err := s.insertLinkTx(tx, workspaceID, l); err != nil {
		if isUniqueViolation(err) {
			return nil, &LinkRefusedError{Code: "link_duplicate_incoming"}
		}
		return nil, err
	}
	if apply {
		if err := s.applyLinkedPlanTx(tx, p, linkActor(in.Move), strings.TrimSpace(in.Move.Reason), linkWriteWhy, s.linkNow().UTC().Truncate(time.Second), budget); err != nil {
			return nil, err
		}
	}
	if err := budget.commit(tx); err != nil {
		return nil, err
	}
	return &models.ScheduleLinkResult{Link: linkDTO(l, in.ParentItemID, in.ChildItemID), Moved: *moved}, nil
}

const linkWriteWhy = "Moved to follow its link"

func linkActor(o models.LinkWriteOptions) string {
	if o.Actor == "" {
		return "user"
	}
	return o.Actor
}

// linkWritePlanTx plans the move a link write makes: with l in the component in
// place of any stored version of itself, the child's date, and every date below
// it, go to where their links put them, the parent's date staying where it is.
// It settles the plan under the same rules as a linked move and returns the
// result the caller may read, the plan, and whether it is to be applied. The
// caller holds the pin on the parent item, and so the workspace.
func (s *Store) linkWritePlanTx(tx *sql.Tx, workspaceID string, c *commitmentComponent, l linkRow, parent *models.ItemSchedule, budget *linkBudget, o models.LinkWriteOptions, access models.CommitmentAccess) (*models.LinkedMoveResult, *linkedPlan, bool, error) {
	replaced := false
	for i := range c.links {
		if l.ID != "" && c.links[i].ID == l.ID {
			c.links[i], replaced = l, true
		}
	}
	if !replaced {
		c.links = append(c.links, l)
	}
	p, err := s.planLinkedTx(tx, c, workspaceID, parent.ItemID, linkRole(l.ParentRole), time.Time{}, false)
	if err != nil {
		return &models.LinkedMoveResult{}, nil, false, err
	}
	if p == nil {
		return &models.LinkedMoveResult{Changes: []models.LinkedDateChange{}}, nil, false, nil
	}
	res, apply, err := s.settleLinkedPlan(p, budget, linkedOptions{actor: linkActor(o), reason: strings.TrimSpace(o.Reason), revision: o.Revision, preview: o.Preview, acknowledge: o.AcknowledgeViolations, access: access})
	if res == nil {
		res = &models.LinkedMoveResult{}
	}
	return res, p, apply, err
}

func linkDTO(l linkRow, parentItem, childItem string) *models.ScheduleLink {
	return &models.ScheduleLink{ID: l.ID, ParentItemID: parentItem, ParentRole: l.ParentRole, ChildItemID: childItem, ChildRole: l.ChildRole,
		Offset: l.Offset, CancelPolicy: l.CancelPolicy, CreatedAt: l.CreatedAt, UpdatedAt: l.UpdatedAt}
}

func (s *Store) insertLinkTx(tx *sql.Tx, workspaceID string, l linkRow) error {
	allDay := 0
	if l.Offset.AllDay {
		allDay = 1
	}
	_, err := tx.Exec(s.q(`INSERT INTO schedule_links (id,workspace_id,`+strings.TrimPrefix(linkColumns, "id,")+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`),
		l.ID, workspaceID, l.ParentSchedule, l.ParentRole, l.ChildSchedule, l.ChildRole, l.Offset.Mode, l.Offset.ElapsedSecs, l.Offset.Days, l.Offset.ClockSecs, allDay, l.Offset.Zone, l.CancelPolicy, l.CreatedAt, l.UpdatedAt)
	if err != nil {
		return fmt.Errorf("insert schedule link: %w", err)
	}
	return nil
}

// linkEndsOf reads a link by id (unlocked) so a writer can name its root item.
func (s *Store) linkEndsOf(workspaceID, linkID string) (parentItem, childItem string, err error) {
	err = s.db.QueryRow(s.q(`SELECT p.item_id, c.item_id FROM schedule_links l
 JOIN item_schedules p ON p.id=l.parent_schedule_id JOIN item_schedules c ON c.id=l.child_schedule_id WHERE l.id=? AND l.workspace_id=?`), linkID, workspaceID).Scan(&parentItem, &childItem)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrScheduleLinkNotFound
	}
	return
}

// UpdateScheduleLink changes a link's offset or cancel policy. It needs edit on
// both ends, rechecks the limits and cycle rule, and, like a new link, moves the
// child's outstanding occurrence to the parent plus the new offset when it is
// not already there.
func (s *Store) UpdateScheduleLink(workspaceID, linkID string, upd models.ScheduleLinkUpdate, access models.CommitmentAccess) (*models.ScheduleLinkResult, error) {
	parentItem, childItem, err := s.linkEndsOf(workspaceID, linkID)
	if err != nil {
		return nil, err
	}
	if !readableLinkEnds(access, parentItem, childItem) {
		return nil, ErrScheduleLinkNotFound
	}
	if !access.Editable[parentItem] || !access.Editable[childItem] {
		return nil, ErrLinkEditRequired
	}
	openFilter, openArgs := s.nonTerminalFilter(workspaceID, "i")
	budget := s.newLinkBudget()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("update schedule link: %w", err)
	}
	defer tx.Rollback()
	if err := s.setTxTimeoutsCtx(budget.ctx, tx, linkTxBudget, linkTxBudget); err != nil {
		return nil, err
	}
	c, err := s.pinCommitmentTx(tx, workspaceID, openFilter, openArgs, true, parentItem, childItem)
	if errors.Is(err, ErrScheduleItemGone) {
		// A soft-deleted parent is the root the pin cannot find; answer as for an
		// unreadable end, not with a code that tells the two apart.
		return nil, ErrScheduleLinkNotFound
	}
	if err != nil {
		return nil, err
	}
	var cur *linkRow
	for i := range c.links {
		if c.links[i].ID == linkID {
			cur = &c.links[i]
		}
	}
	if cur == nil {
		return nil, ErrScheduleLinkNotFound
	}
	next := *cur
	if upd.Offset != nil {
		off, err := offsetOf(*upd.Offset)
		if err != nil {
			return nil, err
		}
		next.Offset = storedOffset(off)
	}
	if upd.CancelPolicy != nil {
		if !oneOf(*upd.CancelPolicy, []string{models.LinkWaitingOn, models.LinkCancelChildOccurrence}) {
			return nil, fieldErr("cancel_policy", "enum", "cancel_policy must be waiting_on or cancel_child_occurrence")
		}
		next.CancelPolicy = *upd.CancelPolicy
	}
	g, err := c.structuralGraph(linkID)
	if err != nil {
		return nil, err
	}
	edge, err := next.edge()
	if err != nil {
		return nil, err
	}
	if err := schedule.CheckLink(g, edge, schedule.LinkLimits{}); err != nil {
		return nil, redactLinkError(err)
	}
	parentSc := c.scheds[next.ParentSchedule]
	if parentSc == nil {
		return nil, ErrScheduleLinkNotFound
	}
	// Only an offset change moves a date; a cancel policy alone plans, asks and
	// moves nothing.
	moved, p, apply := &models.LinkedMoveResult{Changes: []models.LinkedDateChange{}}, (*linkedPlan)(nil), false
	if upd.Offset != nil {
		c.explicitLink = linkID
		if moved, p, apply, err = s.linkWritePlanTx(tx, workspaceID, c, next, parentSc, budget, upd.Move, access); err != nil {
			return nil, err
		}
	}
	if upd.Move.Preview {
		return &models.ScheduleLinkResult{Moved: *moved}, nil
	}
	if upd.Offset != nil {
		// The plan is nil when the parent has no outstanding occurrence; the child's
		// own occurrence still takes the new offset, with nothing to move.
		var childNode *linkNode
		if p != nil {
			childNode = p.nodes[next.ChildSchedule]
		} else if c.live[next.ChildSchedule] {
			if childNode, err = s.linkNodeTx(tx, c.scheds[next.ChildSchedule]); err != nil {
				return nil, err
			}
		}
		if err := s.replaceLinkOverrideTx(tx, c, childNode, linkID, next.Offset, linkActor(upd.Move)); err != nil {
			return nil, err
		}
	}
	allDay := 0
	if next.Offset.AllDay {
		allDay = 1
	}
	next.UpdatedAt = nowNano()
	res, err := tx.Exec(s.q(`UPDATE schedule_links SET offset_mode=?,offset_elapsed=?,offset_days=?,offset_clock=?,offset_all_day=?,offset_zone=?,cancel_policy=?,updated_at=? WHERE id=?`),
		next.Offset.Mode, next.Offset.ElapsedSecs, next.Offset.Days, next.Offset.ClockSecs, allDay, next.Offset.Zone, next.CancelPolicy, next.UpdatedAt, linkID)
	if err != nil {
		return nil, fmt.Errorf("update schedule link: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, err
	} else if n == 0 {
		return nil, ErrScheduleLinkNotFound
	}
	if apply {
		if err := s.applyLinkedPlanTx(tx, p, linkActor(upd.Move), strings.TrimSpace(upd.Move.Reason), linkWriteWhy, s.linkNow().UTC().Truncate(time.Second), budget); err != nil {
			return nil, err
		}
	}
	if err := budget.commit(tx); err != nil {
		return nil, err
	}
	return &models.ScheduleLinkResult{Link: linkDTO(next, parentItem, childItem), Moved: *moved}, nil
}

// DeleteScheduleLink removes a link. It needs edit on both ends.
func (s *Store) DeleteScheduleLink(workspaceID, linkID string, access models.CommitmentAccess) error {
	parentItem, childItem, err := s.linkEndsOf(workspaceID, linkID)
	if err != nil {
		return err
	}
	if !readableLinkEnds(access, parentItem, childItem) {
		return ErrScheduleLinkNotFound
	}
	if !access.Editable[parentItem] || !access.Editable[childItem] {
		return ErrLinkEditRequired
	}
	budget := s.newLinkBudget()
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("delete schedule link: %w", err)
	}
	defer tx.Rollback()
	if err := s.setTxTimeoutsCtx(budget.ctx, tx, linkTxBudget, linkTxBudget); err != nil {
		return err
	}
	// The root is the parent's item whether or not it is soft-deleted: it is
	// pinned by id, first, then the workspace, as for a live link. A deleted end
	// is read as part of the component but is never live in it.
	if _, err := s.pinCommitmentOpt(tx, workspaceID, "", nil, true, true, parentItem, childItem); err != nil {
		return err
	}
	res, err := tx.Exec(s.q(`DELETE FROM schedule_links WHERE id=? AND workspace_id=?`), linkID, workspaceID)
	if err != nil {
		return fmt.Errorf("delete schedule link: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrScheduleLinkNotFound
	}
	return budget.commit(tx)
}

// ListScheduleLinks returns the links touching itemID whose BOTH ends the
// caller can read. A link with an unreadable other end is absent: the item
// shows its own date and nothing of the parent.
func (s *Store) ListScheduleLinks(workspaceID, itemID string, access models.CommitmentAccess) ([]models.ScheduleLink, error) {
	if !access.Readable[itemID] {
		return nil, ErrScheduleNotFound
	}
	rows, err := s.db.Query(s.q(`SELECT l.`+strings.ReplaceAll(linkColumns, ",", ",l.")+`, p.item_id, c.item_id FROM schedule_links l
 JOIN item_schedules p ON p.id=l.parent_schedule_id JOIN item_schedules c ON c.id=l.child_schedule_id
 WHERE l.workspace_id=? AND (p.item_id=? OR c.item_id=?) ORDER BY l.id`), workspaceID, itemID, itemID)
	if err != nil {
		return nil, fmt.Errorf("list schedule links: %w", err)
	}
	defer rows.Close()
	out := []models.ScheduleLink{}
	for rows.Next() {
		var l linkRow
		var allDay int
		var pItem, cItem string
		if err := rows.Scan(&l.ID, &l.ParentSchedule, &l.ParentRole, &l.ChildSchedule, &l.ChildRole, &l.Offset.Mode, &l.Offset.ElapsedSecs, &l.Offset.Days, &l.Offset.ClockSecs, &allDay, &l.Offset.Zone, &l.CancelPolicy, &l.CreatedAt, &l.UpdatedAt, &pItem, &cItem); err != nil {
			return nil, err
		}
		l.Offset.AllDay = allDay != 0
		if readableLinkEnds(access, pItem, cItem) {
			out = append(out, *linkDTO(l, pItem, cItem))
		}
	}
	return out, rows.Err()
}

// linkedRolesTx names the roles of scheduleID that some link uses, on whichever
// end the schedule is. It is empty for a schedule with no link. It is one plain
// read, taken under the schedule's own pin, so a link writer (which needs the
// workspace lock this transaction holds) cannot add an edge between the read
// and the commit.
func (s *Store) linkedRolesTx(tx *sql.Tx, scheduleID string) (map[string]bool, error) {
	rows, err := tx.Query(s.q(`SELECT parent_schedule_id, parent_role, child_role FROM schedule_links WHERE parent_schedule_id=? OR child_schedule_id=?`), scheduleID, scheduleID)
	if err != nil {
		return nil, fmt.Errorf("read linked roles: %w", err)
	}
	defer rows.Close()
	used := map[string]bool{}
	for rows.Next() {
		var parent, pr, cr string
		if err := rows.Scan(&parent, &pr, &cr); err != nil {
			return nil, err
		}
		if parent == scheduleID {
			used[pr] = true
		} else {
			used[cr] = true
		}
	}
	return used, rows.Err()
}

// roleOffsetsDiffer reports whether the offsets of the named role differ.
func roleOffsetsDiffer(role string, a, b scheduleRoles) bool {
	same := func(x, y *int) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	switch role {
	case "target":
		return a.TargetDays != b.TargetDays || a.TargetSecs != b.TargetSecs
	case "earliest":
		return !same(a.EarliestDays, b.EarliestDays) || !same(a.EarliestSecs, b.EarliestSecs)
	case "deadline":
		return !same(a.DeadlineDays, b.DeadlineDays) || !same(a.DeadlineSecs, b.DeadlineSecs)
	case "occupied_until":
		return !same(a.OccupiedDays, b.OccupiedDays) || !same(a.OccupiedSecs, b.OccupiedSecs)
	}
	return false
}

// guardLinkedRoles is ErrScheduleLinked when a role an edge uses differs
// between the two role sets. Roles no edge uses are free.
func guardLinkedRoles(used map[string]bool, before, after scheduleRoles) error {
	for role := range used {
		if roleOffsetsDiffer(role, before, after) {
			return ErrScheduleLinked
		}
	}
	return nil
}
