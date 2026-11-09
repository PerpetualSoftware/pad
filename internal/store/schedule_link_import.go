package store

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/schedule"
)

// importedLinkEnd is what the checks read of one imported schedule, once.
type importedLinkEnd struct {
	sc       *models.ItemSchedule
	roles    scheduleRoles
	node     *linkNode
	nodeRead bool
}

// date is the end's current date for a role, if it has one. A schedule with no
// outstanding occurrence, or whose dates cannot be read, has none to check.
func (e *importedLinkEnd) date(s *Store, tx *sql.Tx, role string) (time.Time, bool) {
	if !e.nodeRead {
		e.nodeRead = true
		e.node, _ = s.linkNodeTx(tx, e.sc)
	}
	if e.node == nil {
		return time.Time{}, false
	}
	at, ok := e.node.dates[linkRole(role)]
	return at, ok
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
// carry, a row with neither an offset nor a "no longer needed" mark, or an
// unparseable stamp. Two rows on one key (a floating child keys by wall alone)
// keep the later edit.
func prepareLinkOverrides(in []models.ScheduleLinkOverrideExport, floating bool) ([]models.ScheduleLinkOverrideExport, []string) {
	var out []models.ScheduleLinkOverrideExport
	var drops []string
	at := map[string]int{}
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
			off, err := offsetOf(*o.Offset)
			if err != nil || !offsetCanApply(off) {
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
// the enums, no self link, offsets valid and not past 1970..2200 (against the
// parent's date when it has one), both roles configured on their schedules
// (and a deadline only on native roles), and CheckLink against the links
// accepted so far in the same component, which is the one-incoming-edge rule,
// the cycle rule and the 50/16 limits, with the live component cap. A link
// that fails is dropped whole with its overrides and reported. Nothing here
// moves a date: the bundle's dates are the truth, so there is no plan, no
// cascade, no journal row and no reminder.
func (s *Store) importScheduleLinksTx(tx *sql.Tx, workspaceID string, links []models.ScheduleLinkExport, schedules map[string]string, sources map[string]models.ScheduleExport, itemRef func(sourceItemID string) string) ([]models.ImportScheduleDrop, error) {
	var drops []models.ImportScheduleDrop
	ends := map[string]*importedLinkEnd{}
	end := func(id string) (*importedLinkEnd, error) {
		if e, ok := ends[id]; ok {
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
		ends[id] = &importedLinkEnd{sc: sc, roles: roles}
		return ends[id], nil
	}
	var accepted []linkRow
	touching := map[string][]int{}
	for _, x := range links {
		child, childKnown := sources[x.ChildScheduleID]
		item := ""
		if childKnown {
			item = itemRef(child.ItemID)
		}
		v, err := s.checkImportedLink(tx, x, schedules, sources, end, accepted, touching)
		if err != nil {
			return nil, err
		}
		if v.reason != "" {
			slog.Warn("workspace import: dropped a schedule link", "workspace_id", workspaceID, "item", item, "reason", v.reason)
			drops = append(drops, models.ImportScheduleDrop{Item: item, Reason: v.reason})
			continue
		}
		l := v.link
		l.ID, l.CreatedAt, l.UpdatedAt = newID(), x.CreatedAt, x.UpdatedAt
		if err := s.insertLinkTx(tx, workspaceID, l); err != nil {
			return nil, err
		}
		for _, o := range v.overrides {
			if err := s.insertLinkOverrideTx(tx, l.ID, o); err != nil {
				return nil, err
			}
		}
		for _, reason := range v.overrideDrops {
			slog.Warn("workspace import: dropped a schedule link override", "workspace_id", workspaceID, "item", item, "reason", reason)
			drops = append(drops, models.ImportScheduleDrop{Item: item, Reason: reason})
		}
		accepted = append(accepted, l)
		touching[l.ParentSchedule] = append(touching[l.ParentSchedule], len(accepted)-1)
		touching[l.ChildSchedule] = append(touching[l.ChildSchedule], len(accepted)-1)
	}
	return drops, nil
}

// linkVerdict is the answer to one bundled link: a reason it is not imported,
// or the row to store with its overrides and the codes of the override rows
// refused.
type linkVerdict struct {
	reason        string
	link          linkRow
	overrides     []models.ScheduleLinkOverrideExport
	overrideDrops []string
}

// checkImportedLink judges one bundled link against the links accepted so far.
// An error is a database failure; a refusal is a reason.
func (s *Store) checkImportedLink(tx *sql.Tx, x models.ScheduleLinkExport, schedules map[string]string, sources map[string]models.ScheduleExport, end func(string) (*importedLinkEnd, error), accepted []linkRow, touching map[string][]int) (linkVerdict, error) {
	refuse := func(reason string) (linkVerdict, error) { return linkVerdict{reason: reason}, nil }
	_, parentKnown := sources[x.ParentScheduleID]
	childSource, childKnown := sources[x.ChildScheduleID]
	parentID, childID := schedules[x.ParentScheduleID], schedules[x.ChildScheduleID]
	if !parentKnown || !childKnown || parentID == "" || childID == "" {
		return refuse("links:orphan")
	}
	parent, err := end(parentID)
	if err != nil {
		return linkVerdict{}, err
	}
	child, err := end(childID)
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
	off, err := offsetOf(x.Offset)
	if err != nil || !offsetCanApply(off) {
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
	if at, ok := parent.date(s, tx, x.ParentRole); ok {
		if _, err := off.Apply(at); err != nil {
			return refuse("links:offset")
		}
	}
	if !validLinkStamps(x.CreatedAt, x.UpdatedAt) {
		return refuse("links:invalid")
	}
	l := linkRow{ParentSchedule: parentID, ParentRole: x.ParentRole, ChildSchedule: childID, ChildRole: x.ChildRole,
		Offset: storedOffset(off), CancelPolicy: policy}
	component := linkComponent(accepted, touching, parentID, childID)
	if len(component)+1 > maxLinkComponent {
		return refuse("links:limit")
	}
	g, err := (&commitmentComponent{links: component}).structuralGraph("")
	if err != nil {
		return linkVerdict{}, err
	}
	edge, err := l.edge()
	if err != nil {
		return refuse(linkReason(err))
	}
	if err := schedule.CheckLink(g, edge, schedule.LinkLimits{}); err != nil {
		return refuse(linkReason(redactLinkError(err)))
	}
	overrides, overrideDrops := prepareLinkOverrides(x.Overrides, childSource.Floating)
	return linkVerdict{link: l, overrides: overrides, overrideDrops: overrideDrops}, nil
}
