package store

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// AgendaDayCount is one local day of the agenda's counts series (TASK-4244).
// Completed is the items that reached a positive terminal value that day.
// Open is the items still open at the day's end. Burndown is how many of the
// items open at the window's start are still open at the day's end; items
// created later never add to it.
type AgendaDayCount struct {
	Day       string
	Completed int
	Open      int
	Burndown  int
}

// AgendaCountDays lists the local days a window touches, each with the instant
// its count is taken at: the next local midnight, or the window's end. It
// advances by calendar date, so a zone that skips a local date still ends.
func AgendaCountDays(from, to time.Time, loc *time.Location) (days []string, ends []time.Time) {
	y, m, d := from.In(loc).Date()
	for {
		start := time.Date(y, m, d, 0, 0, 0, 0, loc)
		if !start.Before(to) && len(days) > 0 {
			return
		}
		next := time.Date(y, m, d+1, 0, 0, 0, 0, loc)
		end := next
		if end.After(to) {
			end = to
		}
		ny, nm, nd := next.Date()
		if (ny == y && nm == m && nd == d) || len(days) >= 100 {
			return // a calendar that does not advance cannot make progress
		}
		days = append(days, fmt.Sprintf("%04d-%02d-%02d", y, int(m), d))
		ends = append(ends, end)
		y, m, d = ny, nm, nd
		if !next.Before(to) {
			return
		}
	}
}

// terminalStateExpr is the SQL test that a status_transitions row (alias st)
// moved its item to a terminal value of the collection it was written under.
func terminalStateExpr(colls []reportCollection) (string, []any) {
	var clauses []string
	var args []any
	for _, c := range colls {
		if len(c.allTerminals) == 0 {
			continue
		}
		args = append(args, c.id, c.doneKey)
		for _, v := range c.allTerminals {
			args = append(args, strings.ToLower(v))
		}
		clauses = append(clauses, fmt.Sprintf("(st.collection_id = ? AND st.field_key = ? AND LOWER(st.to_status) IN (%s))", placeholders(len(c.allTerminals))))
	}
	if len(clauses) == 0 {
		return "", nil
	}
	return "(" + strings.Join(clauses, " OR ") + ")", args
}

const (
	countOpen = iota
	countDone
	countAbandoned
)

type countEvent struct {
	at          time.Time
	create      bool
	seq         int64
	id          string
	itemID      string
	collID      string
	from, to    string
	itemCreated time.Time
}

// AgendaCounts reads the counts series for one workspace over [from, to) in
// loc's days. scope is the caller's visibility, applied to the items before
// any count, so a hidden item moves nothing. maxRows bounds the rows read; a
// call that would read more reports truncated and returns no series, because a
// partial series would be read as a total. rows is what the call read, for a
// request-wide budget.
func (s *Store) AgendaCounts(workspaceID string, scope PendingReminderScope, from, to time.Time, loc *time.Location, maxRows int) (days []AgendaDayCount, rows int, truncated bool, err error) {
	keys, ends := AgendaCountDays(from, to, loc)
	out := make([]AgendaDayCount, len(keys))
	for i, k := range keys {
		out[i].Day = k
	}
	if scope.CollectionIDs != nil && len(scope.CollectionIDs) == 0 && len(scope.ItemIDs) == 0 {
		return out, 0, false, nil
	}
	all, err := s.ListCollectionsMinimal(workspaceID)
	if err != nil {
		return nil, 0, false, err
	}
	byColl := map[string]reportCollection{}
	var colls []reportCollection
	for _, c := range all {
		if !c.IsSystem {
			rc := reportCollectionFor(c)
			colls = append(colls, rc)
			byColl[rc.id] = rc
		}
	}
	if len(colls) == 0 {
		return out, 0, false, nil
	}
	kind := func(collID, status string) int {
		rc, ok := byColl[collID]
		if !ok {
			return countOpen
		}
		v := strings.ToLower(status)
		for _, p := range rc.positiveTerminals {
			if strings.ToLower(p) == v {
				return countDone
			}
		}
		for _, p := range rc.allTerminals {
			if strings.ToLower(p) == v {
				return countAbandoned
			}
		}
		return countOpen
	}

	scopeSQL, scopeArgs := scopeClause("i", scope)
	live := ` AND i.deleted_at IS NULL AND c.deleted_at IS NULL AND NOT c.is_system`
	fromB, toB := agendaEventBound(from), agendaEventBound(to)

	// Items open when the window starts: created before it, and not at a
	// terminal value by their last transition before it.
	terminal, termArgs := terminalStateExpr(colls)
	startQ := `SELECT COUNT(*) FROM items i JOIN collections c ON c.id = i.collection_id
  WHERE i.workspace_id = ? AND i.created_at < ?` + live + scopeSQL
	startArgs := append([]any{workspaceID, fromB}, scopeArgs...)
	if terminal != "" {
		startQ += ` AND NOT EXISTS (SELECT 1 FROM status_transitions st WHERE st.item_id = i.id AND st.created_at < ? AND ` + terminal + `
   AND NOT EXISTS (SELECT 1 FROM status_transitions s2 WHERE s2.item_id = st.item_id AND s2.created_at < ?
    AND (s2.created_at > st.created_at OR (s2.created_at = st.created_at AND s2.seq > st.seq))))`
		startArgs = append(append(append(startArgs, fromB), termArgs...), fromB)
	}
	var startOpen int
	if err := s.db.QueryRow(s.q(startQ), startArgs...).Scan(&startOpen); err != nil {
		return nil, 0, false, fmt.Errorf("agenda counts start: %w", err)
	}

	var events []countEvent
	// Items created in the window open on their creation instant.
	created, err := s.db.Query(s.q(`SELECT i.id, i.collection_id, i.created_at FROM items i JOIN collections c ON c.id = i.collection_id
  WHERE i.workspace_id = ? AND i.created_at >= ? AND i.created_at < ?`+live+scopeSQL+` ORDER BY i.created_at, i.id LIMIT ?`),
		append(append([]any{workspaceID, fromB, toB}, scopeArgs...), maxRows+1)...)
	if err != nil {
		return nil, 0, false, fmt.Errorf("agenda counts created: %w", err)
	}
	for created.Next() {
		var e countEvent
		var at string
		if err := created.Scan(&e.itemID, &e.collID, &at); err != nil {
			created.Close()
			return nil, 0, false, err
		}
		e.create, e.at, e.id = true, parseTime(at), e.itemID
		events = append(events, e)
	}
	if err := created.Err(); err != nil {
		created.Close()
		return nil, 0, false, err
	}
	created.Close()

	var pairs []string
	var pairArgs []any
	for _, c := range colls {
		pairs = append(pairs, "(st.collection_id = ? AND st.field_key = ?)")
		pairArgs = append(pairArgs, c.id, c.doneKey)
	}
	trans, err := s.db.Query(s.q(`SELECT st.id, st.item_id, st.collection_id, st.from_status, st.to_status, st.created_at, st.seq, i.created_at
  FROM status_transitions st JOIN items i ON i.id = st.item_id JOIN collections c ON c.id = i.collection_id
  WHERE st.workspace_id = ? AND i.workspace_id = ? AND st.created_at >= ? AND st.created_at < ?`+live+scopeSQL+`
   AND (`+strings.Join(pairs, " OR ")+`) ORDER BY st.created_at, st.seq, st.id LIMIT ?`),
		append(append(append([]any{workspaceID, workspaceID, fromB, toB}, scopeArgs...), pairArgs...), maxRows+1)...)
	if err != nil {
		return nil, 0, false, fmt.Errorf("agenda counts transitions: %w", err)
	}
	for trans.Next() {
		var e countEvent
		var at, itemAt string
		if err := trans.Scan(&e.id, &e.itemID, &e.collID, &e.from, &e.to, &at, &e.seq, &itemAt); err != nil {
			trans.Close()
			return nil, 0, false, err
		}
		e.at, e.itemCreated = parseTime(at), parseTime(itemAt)
		events = append(events, e)
	}
	if err := trans.Err(); err != nil {
		trans.Close()
		return nil, 0, false, err
	}
	trans.Close()
	if len(events) > maxRows {
		return nil, len(events), true, nil
	}

	// Creations first at an instant, then transitions in write order.
	sort.SliceStable(events, func(i, j int) bool {
		a, b := events[i], events[j]
		switch {
		case !a.at.Equal(b.at):
			return a.at.Before(b.at)
		case a.create != b.create:
			return a.create
		case a.seq != b.seq:
			return a.seq < b.seq
		}
		return a.id < b.id
	})
	type itemState struct {
		known, inStart bool
		kind           int
	}
	state := map[string]*itemState{}
	completedOn := map[string]bool{}
	open, closedFromStart, day := startOpen, 0, 0
	snapshot := func(upTo int) {
		for ; day < upTo; day++ {
			out[day].Open, out[day].Burndown = open, startOpen-closedFromStart
		}
	}
	for _, e := range events {
		for day < len(ends) && !e.at.Before(ends[day]) {
			snapshot(day + 1)
		}
		if day >= len(out) {
			break
		}
		if e.create {
			state[e.itemID] = &itemState{known: true, kind: countOpen}
			open++
			continue
		}
		st := state[e.itemID]
		if st == nil {
			// Created before the window: its state going in is the row's from value.
			k := kind(e.collID, e.from)
			st = &itemState{known: true, inStart: k == countOpen, kind: k}
			state[e.itemID] = st
		}
		next := kind(e.collID, e.to)
		if (st.kind == countOpen) != (next == countOpen) {
			delta := 1
			if next != countOpen {
				delta = -1
			}
			open += delta
			if st.inStart {
				closedFromStart -= delta
			}
		}
		if next == countDone && st.kind != countDone && !completedOn[e.itemID+"|"+out[day].Day] {
			completedOn[e.itemID+"|"+out[day].Day] = true
			out[day].Completed++
		}
		st.kind = next
	}
	snapshot(len(out))
	return out, len(events), false, nil
}
