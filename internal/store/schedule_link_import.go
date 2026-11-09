package store

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/schedule"
)

// An import examines a bounded amount of links and override rows (the work is
// bounded because it runs in the import's one transaction: the SQLite write lock,
// and on Postgres the idle-in-transaction limit). Each link is checked against the
// links already accepted in its component, at most maxLinkComponent of them, so a
// bundle can make the import re-check one big component over and over; and each
// override row costs a parse, a handful of time zone reads and an insert. The
// ceilings leave a real workspace (many links, small components, an override per
// dragged occurrence) untouched:
//
//   - maxImportedLinks bounds the links examined, and with them the drop entries,
//     log lines and response body the rejects produce;
//   - maxImportedLinkWork bounds the sum, over the links examined, of the size of
//     the component each was checked against: about 2,000 checks of a full
//     component, a few seconds measured;
//   - maxImportedOverridesPerLink and maxImportedOverrides bound the override
//     rows examined on one link and on the whole import (50,000 rows of distinct
//     civil offsets is about 5 seconds, measured);
//   - maxOverrideDropEntries bounds the override rows reported one by one (each
//     with a log line); the rest of the refused rows are counted in one entry per
//     reason.
//
// Past a ceiling the links or rows not examined are left out and reported once,
// with their count ("links:bundle_limit", "link_overrides:bundle_limit"). They
// are variables only so a test can make a ceiling bite without a bundle that
// large.
var (
	maxImportedLinks            = 10_000
	maxImportedLinkWork         = 1_000_000
	maxImportedOverrides        = 50_000
	maxImportedOverridesPerLink = 10_000
	maxOverrideDropEntries      = 100
)

const (
	reasonBundleLimit         = "links:bundle_limit"
	reasonOverrideBundleLimit = "link_overrides:bundle_limit"
	reasonDates               = "links:dates"
)

// errScheduleDates marks a schedule whose stored role data gives no readable date
// (see isScheduleDataError).
var errScheduleDates = errors.New("schedule dates cannot be read")

// isScheduleDataError tells a fact about a schedule's stored data from a fault of
// the database. The dates of a schedule come from role offsets and walls: a role
// 100,000 days before an all-day schedule is a legal 32-bit offset that lands in
// the year 1756, which has no wall, and schedule.ParseWall refuses it with a
// *schedule.MalformedError (that package returns no other error for bad input; a
// stored value that does not parse as a time is a *time.ParseError). Those are the
// two types a data problem takes. Anything else (a driver or SQL error, a context
// that ended, a poisoned Postgres transaction) is not about the schedule and must
// stop the import.
func isScheduleDataError(err error) bool {
	var malformed *schedule.MalformedError
	var parse *time.ParseError
	return errors.As(err, &malformed) || errors.As(err, &parse)
}

// importedLinkEnd is what the checks read of one imported schedule, once.
type importedLinkEnd struct {
	sc       *models.ItemSchedule
	roles    scheduleRoles
	node     *linkNode
	datesErr error
	nodeRead bool
}

// outstanding is the end's dates for its outstanding occurrence. nil, with no
// error, means it has none (it ended, or has no valid next date): there is then
// no date to check an offset against, and the cascade skips such an edge too.
// A schedule whose stored role data gives no readable date is errScheduleDates,
// the same answer every time (the caller drops the link). Anything else that goes
// wrong reading them is a database fault and an error that fails the import: on
// Postgres a failed statement has already poisoned the transaction.
func (e *importedLinkEnd) outstanding(s *Store, tx *sql.Tx) (*linkNode, error) {
	if !e.nodeRead {
		n, err := s.linkNodeTx(tx, e.sc)
		switch {
		case err == nil:
			e.node = n
		case isScheduleDataError(err):
			e.datesErr = fmt.Errorf("%w: %v", errScheduleDates, err)
		default:
			return nil, err
		}
		e.nodeRead = true
	}
	return e.node, e.datesErr
}

// offsetCanApply reports whether any parent date the package supports could
// take the offset: one that leaves 1970..2200 from both ends of that span
// could never be written.
func offsetCanApply(off schedule.Offset) bool {
	for _, anchor := range []time.Time{time.Date(1970, 1, 2, 12, 0, 0, 0, time.UTC), time.Date(2199, 12, 30, 12, 0, 0, 0, time.UTC)} {
		if _, err := off.Apply(anchor); err == nil {
			return true
		}
	}
	return false
}

// importedOffset is the verdict on one stored offset.
type importedOffset struct {
	off schedule.Offset
	ok  bool
}

// offsetMemo judges each distinct offset of an import once: a link and its
// overrides repeat the same few, and each check loads a time zone.
type offsetMemo map[models.ScheduleLinkOffset]importedOffset

// check validates an offset as a live write does (offsetOf) and bounds what the
// tables store: offset_days is a 32-bit INTEGER on Postgres, and a value past it
// fails the statement there and rolls the whole import back, where SQLite would
// store it silently. offset_elapsed is a BIGINT held to +-300 years of seconds
// and offset_clock to a day by offsetOf and the table's CHECK. An offset no
// supported date could take is refused too.
func (m offsetMemo) check(o models.ScheduleLinkOffset) (schedule.Offset, bool) {
	if v, hit := m[o]; hit {
		return v.off, v.ok
	}
	var v importedOffset
	if days := int64(o.Days); days >= math.MinInt32 && days <= math.MaxInt32 {
		if off, err := offsetOf(o); err == nil && offsetCanApply(off) {
			v = importedOffset{off, true}
		}
	}
	m[o] = v
	return v.off, v.ok
}

// derivedInRange reports whether off, applied to the parent's current date for
// role, gives a date the cascade could write on the child: Apply must succeed,
// and (as planLinkedTx's backstop requires) the child's own zone must read it as
// a supported year, whatever zone the offset used. planLinkedTx drops an edge
// whose parent or child has no outstanding occurrence (nil here), so for such a
// link there is nothing to check and the answer is true.
//
// Not mirrored: the cascade also takes a FINISHED (done or abandoned) dependent
// out of the year backstop, and a finished non-root parent's out-edges out of the
// graph. Whether an item is finished comes from its collection's terminal
// options (nonTerminalFilter), which reads through the pool and so cannot see the
// collections of the workspace this transaction is still creating; and a finished
// parent can still be the root of a later move. The import therefore errs on the
// strict side for a finished child: it can drop a link the cascade would have
// left alone, only within a day of 1970 or 2200 in a zone the offset did not use.
func derivedInRange(off schedule.Offset, parent, child *linkNode, role string) bool {
	if parent == nil || child == nil {
		return true
	}
	at, ok := parent.dates[linkRole(role)]
	if !ok {
		return true
	}
	to, err := off.Apply(at)
	if err != nil {
		return false
	}
	y := to.In(child.rec).Year()
	return y >= 1970 && y <= 2200
}

func validLinkStamps(stamps ...string) bool {
	for _, ts := range stamps {
		if _, err := time.Parse(time.RFC3339, ts); err != nil {
			return false
		}
	}
	return true
}

// linkReason is the drop code for a refusal the live writers give.
func linkReason(err error) string {
	var refused *LinkRefusedError
	if errors.As(err, &refused) {
		return "links:" + strings.TrimPrefix(refused.Code, "link_")
	}
	return "links:invalid"
}

// prepareLinkOverrides canonicalises a link's overrides for the child's
// floating policy and returns the rows to store, with one code per row it
// refuses: a key that is not an occurrence key, an offset a link could not
// carry (or that the parent's current date cannot take, applies), a row with
// neither an offset nor a "no longer needed" mark, or an unparseable stamp. Two rows on one key (a floating child keys by wall alone)
// keep the later edit.
func prepareLinkOverrides(in []models.ScheduleLinkOverrideExport, floating bool, offsets offsetMemo, applies func(schedule.Offset) bool) ([]models.ScheduleLinkOverrideExport, []string) {
	var out []models.ScheduleLinkOverrideExport
	var drops []string
	at := map[string]int{}
	fits := map[models.ScheduleLinkOffset]bool{}
	for _, o := range in {
		key, err := canonicalCarriedKey(o.OccurrenceKey, floating)
		stamps := []string{o.CreatedAt, o.UpdatedAt}
		if o.NoLongerNeededAt != "" {
			stamps = append(stamps, o.NoLongerNeededAt)
		}
		if err != nil || (o.Offset == nil && o.NoLongerNeededAt == "") || !validLinkStamps(stamps...) {
			drops = append(drops, "link_overrides:invalid")
			continue
		}
		if o.Offset != nil {
			off, ok := offsets.check(*o.Offset)
			if ok {
				fit, known := fits[*o.Offset]
				if !known {
					fit = applies(off)
					fits[*o.Offset] = fit
				}
				ok = fit
			}
			if !ok {
				drops = append(drops, "link_overrides:invalid")
				continue
			}
			stored := storedOffset(off)
			o.Offset = &stored
		}
		o.OccurrenceKey = key
		if prev, dup := at[key]; dup {
			drops = append(drops, "link_overrides:duplicate")
			if newer, err := laterInstant(o.UpdatedAt, out[prev].UpdatedAt); err == nil && newer {
				out[prev] = o
			}
			continue
		}
		at[key] = len(out)
		out = append(out, o)
	}
	return out, drops
}

func (s *Store) insertLinkOverrideTx(tx *sql.Tx, linkID string, o models.ScheduleLinkOverrideExport) error {
	var off models.ScheduleLinkOffset
	var mode any
	if o.Offset != nil {
		off, mode = *o.Offset, o.Offset.Mode
	}
	allDay := 0
	if off.AllDay {
		allDay = 1
	}
	_, err := tx.Exec(s.q(`INSERT INTO schedule_link_overrides (id,link_id,occurrence_key,offset_mode,offset_elapsed,offset_days,offset_clock,offset_all_day,offset_zone,no_longer_needed_at,created_at,updated_at)
 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`), newID(), linkID, o.OccurrenceKey, mode, off.ElapsedSecs, off.Days, off.ClockSecs, allDay, off.Zone, nilIfEmpty(o.NoLongerNeededAt), o.CreatedAt, o.UpdatedAt)
	if err != nil {
		return fmt.Errorf("insert link override: %w", err)
	}
	return nil
}

// linkComponent is the accepted links connected to any of seeds.
func linkComponent(accepted []linkRow, touching map[string][]int, seeds ...string) []linkRow {
	var out []linkRow
	seenLink, seenSchedule := map[int]bool{}, map[string]bool{}
	for queue := seeds; len(queue) > 0; queue = queue[1:] {
		id := queue[0]
		if seenSchedule[id] {
			continue
		}
		seenSchedule[id] = true
		for _, i := range touching[id] {
			if !seenLink[i] {
				seenLink[i] = true
				out = append(out, accepted[i])
				queue = append(queue, accepted[i].ParentSchedule, accepted[i].ChildSchedule)
			}
		}
	}
	return out
}

// importScheduleLinksTx stores a bundle's links on the schedules the import
// stored, inside tx, after the schedules and before anything else reads them.
// schedules maps a source schedule id to its new id and sources holds the
// schedules of the bundle, so an end that was not in the bundle, or whose
// schedule the import refused, is an orphan.
//
// Every link is checked BEFORE it is written (a failed statement poisons a
// Postgres transaction) by the checks a live write gets: roles and policy from
// the enums, no self link, offsets valid and bounded to their columns and not
// past 1970..2200 (against the parent's date when it has one, and in the
// child's zone), both roles configured on their schedules (and a deadline only
// on native roles), and CheckLink against the links accepted so far in the same
// component, which is the one-incoming-edge rule, the cycle rule and the 50/16
// limits, with the live component cap. A link that fails is dropped whole with
// its overrides and reported. A schedule whose stored role data gives no
// readable date drops its links as links:dates; a database fault fails the
// import. Nothing here moves a date: the bundle's dates are the truth, so there
// is no plan, no cascade, no journal row and no reminder.
//
// The work is bounded (see maxImportedLinks): what is past a ceiling is reported
// once, with its count.
func (s *Store) importScheduleLinksTx(tx *sql.Tx, workspaceID string, links []models.ScheduleLinkExport, schedules map[string]string, sources map[string]models.ScheduleExport, itemRef func(sourceItemID string) string) ([]models.ImportScheduleDrop, error) {
	st := &linkImport{s: s, tx: tx, workspaceID: workspaceID, ends: map[string]*importedLinkEnd{}, offsets: offsetMemo{}, touching: map[string][]int{}, folded: map[string]int{}}
	st.end = func(id string) (*importedLinkEnd, error) {
		if e, ok := st.ends[id]; ok {
			return e, nil
		}
		sc, err := s.getScheduleByIDQ(tx, id)
		if err != nil || sc == nil || sc.WorkspaceID != workspaceID {
			return nil, err
		}
		roles, err := s.scheduleRolesOf(tx, sc)
		if err != nil {
			return nil, err
		}
		st.ends[id] = &importedLinkEnd{sc: sc, roles: roles}
		return st.ends[id], nil
	}
	for i, x := range links {
		var v linkVerdict
		if i >= maxImportedLinks {
			v.reason = reasonBundleLimit
		} else {
			var err error
			if v, err = st.check(x, schedules, sources); err != nil {
				return nil, fmt.Errorf("import schedule links: %w", err)
			}
		}
		if v.reason == reasonBundleLimit {
			slog.Warn("workspace import: schedule links past the bundle limit were not examined",
				"workspace_id", workspaceID, "examined", i, "left_out", len(links)-i)
			st.drops = append(st.drops, models.ImportScheduleDrop{Reason: reasonBundleLimit, Count: len(links) - i})
			break
		}
		child, childKnown := sources[x.ChildScheduleID]
		item := ""
		if childKnown {
			item = itemRef(child.ItemID)
		}
		if v.reason != "" {
			slog.Warn("workspace import: dropped a schedule link", "workspace_id", workspaceID, "item", item, "reason", v.reason)
			st.drops = append(st.drops, models.ImportScheduleDrop{Item: item, Reason: v.reason})
			continue
		}
		l := v.link
		l.ID, l.CreatedAt, l.UpdatedAt = newID(), x.CreatedAt, x.UpdatedAt
		if err := s.insertLinkTx(tx, workspaceID, l); err != nil {
			return nil, fmt.Errorf("import schedule links: %w", err)
		}
		for _, o := range v.overrides {
			if err := s.insertLinkOverrideTx(tx, l.ID, o); err != nil {
				return nil, fmt.Errorf("import schedule links: %w", err)
			}
		}
		for _, reason := range v.overrideDrops {
			st.dropOverrideRow(item, reason)
		}
		st.overrideLeftOut += v.overridesLeftOut
		st.accept(l)
	}
	st.flushOverrideAggregates()
	return st.drops, nil
}

// linkImport is the state of one import's link checks.
type linkImport struct {
	s           *Store
	tx          *sql.Tx
	workspaceID string
	end         func(string) (*importedLinkEnd, error)
	ends        map[string]*importedLinkEnd
	offsets     offsetMemo
	accepted    []linkRow
	touching    map[string][]int // schedule id -> indexes into accepted
	work        int              // sum of the component sizes checked so far

	drops           []models.ImportScheduleDrop
	overrideRows    int            // override rows examined so far
	overrideEntries int            // refused override rows reported one by one so far
	folded          map[string]int // refused override rows past that, by reason
	overrideLeftOut int            // override rows past a ceiling, not examined
}

func (st *linkImport) accept(l linkRow) {
	st.accepted = append(st.accepted, l)
	st.touching[l.ParentSchedule] = append(st.touching[l.ParentSchedule], len(st.accepted)-1)
	st.touching[l.ChildSchedule] = append(st.touching[l.ChildSchedule], len(st.accepted)-1)
}

// dropOverrideRow reports a refused override row: one entry and one log line
// each for the first maxOverrideDropEntries of the import, then a count by reason.
func (st *linkImport) dropOverrideRow(item, reason string) {
	if st.overrideEntries >= maxOverrideDropEntries {
		st.folded[reason]++
		return
	}
	st.overrideEntries++
	slog.Warn("workspace import: dropped a schedule link override", "workspace_id", st.workspaceID, "item", item, "reason", reason)
	st.drops = append(st.drops, models.ImportScheduleDrop{Item: item, Reason: reason})
}

// flushOverrideAggregates adds the counted entries: the refused rows past the
// individual entries, one per reason, and the rows past a ceiling.
func (st *linkImport) flushOverrideAggregates() {
	reasons := make([]string, 0, len(st.folded))
	for reason := range st.folded {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	for _, reason := range reasons {
		slog.Warn("workspace import: dropped schedule link overrides", "workspace_id", st.workspaceID, "reason", reason, "count", st.folded[reason])
		st.drops = append(st.drops, models.ImportScheduleDrop{Reason: reason, Count: st.folded[reason]})
	}
	if st.overrideLeftOut > 0 {
		slog.Warn("workspace import: schedule link overrides past the bundle limit were not examined",
			"workspace_id", st.workspaceID, "examined", st.overrideRows, "left_out", st.overrideLeftOut)
		st.drops = append(st.drops, models.ImportScheduleDrop{Reason: reasonOverrideBundleLimit, Count: st.overrideLeftOut})
	}
}

// linkVerdict is the answer to one bundled link: a reason it is not imported,
// or the row to store with its overrides, the codes of the override rows refused
// and the number of override rows left out past a ceiling.
type linkVerdict struct {
	reason           string
	link             linkRow
	overrides        []models.ScheduleLinkOverrideExport
	overrideDrops    []string
	overridesLeftOut int
}

// structuralEdge is the link as the structure checks see it. Cycles, the
// one-incoming-edge rule and the 50/16 footprints depend on the vertices only,
// never on the offset, so the offset is a placeholder that validates without
// loading a time zone: with real offsets every check rebuilt the whole component
// and loaded one zone per edge each time. The offset of a link is checked once,
// as it is examined.
func structuralEdge(l linkRow) schedule.Edge {
	return schedule.Edge{Parent: schedule.Node{Schedule: l.ParentSchedule}, ParentRole: linkRole(l.ParentRole),
		Child: schedule.Node{Schedule: l.ChildSchedule}, ChildRole: linkRole(l.ChildRole), Offset: schedule.Offset{Mode: schedule.OffsetElapsed}}
}

// check judges one bundled link against the links accepted so far. An error is
// a database failure; a refusal is a reason.
func (st *linkImport) check(x models.ScheduleLinkExport, schedules map[string]string, sources map[string]models.ScheduleExport) (linkVerdict, error) {
	refuse := func(reason string) (linkVerdict, error) { return linkVerdict{reason: reason}, nil }
	_, parentKnown := sources[x.ParentScheduleID]
	childSource, childKnown := sources[x.ChildScheduleID]
	parentID, childID := schedules[x.ParentScheduleID], schedules[x.ChildScheduleID]
	if !parentKnown || !childKnown || parentID == "" || childID == "" {
		return refuse("links:orphan")
	}
	parent, err := st.end(parentID)
	if err != nil {
		return linkVerdict{}, err
	}
	child, err := st.end(childID)
	if err != nil {
		return linkVerdict{}, err
	}
	if parent == nil || child == nil {
		return refuse("links:orphan")
	}
	policy := x.CancelPolicy
	if policy == "" {
		policy = models.LinkWaitingOn
	}
	if !oneOf(x.ParentRole, linkRoles) || !oneOf(x.ChildRole, linkRoles) || !oneOf(policy, []string{models.LinkWaitingOn, models.LinkCancelChildOccurrence}) {
		return refuse("links:enum")
	}
	if parentID == childID {
		return refuse("links:self")
	}
	off, ok := st.offsets.check(x.Offset)
	if !ok {
		return refuse("links:offset")
	}
	for _, e := range []struct {
		end  *importedLinkEnd
		role string
	}{{parent, x.ParentRole}, {child, x.ChildRole}} {
		if !roleConfigured(e.end.roles, e.role) || (e.role == "deadline" && e.end.roles.Origin != "native") {
			return refuse("links:role")
		}
	}
	parentNode, err := parent.outstanding(st.s, st.tx)
	if errors.Is(err, errScheduleDates) {
		return refuse(reasonDates)
	} else if err != nil {
		return linkVerdict{}, err
	}
	// The child's dates are read whatever state the parent is in. The range check
	// below needs them only when both ends have dates, but a later linked move
	// reads the dates of every live schedule of the component (planLinkedTx), so a
	// child whose dates cannot be read would make a move on ANY schedule of its
	// component fail, even under an ended parent. A live create of the link is
	// refused, and so is its import. A child that is merely ended is not an error.
	childNode, err := child.outstanding(st.s, st.tx)
	if errors.Is(err, errScheduleDates) {
		return refuse(reasonDates)
	} else if err != nil {
		return linkVerdict{}, err
	}
	if !derivedInRange(off, parentNode, childNode, x.ParentRole) {
		return refuse("links:offset")
	}
	if !validLinkStamps(x.CreatedAt, x.UpdatedAt) {
		return refuse("links:invalid")
	}
	l := linkRow{ParentSchedule: parentID, ParentRole: x.ParentRole, ChildSchedule: childID, ChildRole: x.ChildRole,
		Offset: storedOffset(off), CancelPolicy: policy}
	component := linkComponent(st.accepted, st.touching, parentID, childID)
	if st.work += len(component) + 1; st.work > maxImportedLinkWork {
		return refuse(reasonBundleLimit)
	}
	if len(component)+1 > maxLinkComponent {
		return refuse("links:limit")
	}
	edges := make([]schedule.Edge, len(component))
	for i, c := range component {
		edges[i] = structuralEdge(c)
	}
	g, err := schedule.NewGraph(edges)
	if err != nil {
		return linkVerdict{}, fmt.Errorf("accepted links no longer form a graph: %w", err)
	}
	if err := schedule.CheckLink(g, structuralEdge(l), schedule.LinkLimits{}); err != nil {
		return refuse(linkReason(redactLinkError(err)))
	}
	examine := min(len(x.Overrides), maxImportedOverridesPerLink, max(maxImportedOverrides-st.overrideRows, 0))
	st.overrideRows += examine
	overrides, overrideDrops := prepareLinkOverrides(x.Overrides[:examine], childSource.Floating, st.offsets, func(o schedule.Offset) bool {
		return derivedInRange(o, parentNode, childNode, x.ParentRole)
	})
	return linkVerdict{link: l, overrides: overrides, overrideDrops: overrideDrops, overridesLeftOut: len(x.Overrides) - examine}, nil
}
