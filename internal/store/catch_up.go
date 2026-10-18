package store

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// CatchUpLineCap is how many lines each catch-up section carries. The rest
// is in the section's total ("and N more").
const CatchUpLineCap = 5

// CatchUpScope is the viewer's item visibility, in the shape every list door
// already computes (TASK-3615): CollectionIDs nil means no collection
// restriction (a cookie-session admin); CollectionIDs non-nil and empty with
// no ItemIDs means the viewer sees nothing. ItemIDs are item-level grants,
// additive to CollectionIDs.
type CatchUpScope struct {
	CollectionIDs []string
	ItemIDs       []string
}

func (sc CatchUpScope) seesNothing() bool {
	return sc.CollectionIDs != nil && len(sc.CollectionIDs) == 0 && len(sc.ItemIDs) == 0
}

// clause renders the scope as a predicate on the items alias.
func (sc CatchUpScope) clause(alias string) (string, []any) {
	ph := func(ids []string, args *[]any) string {
		p := make([]string, len(ids))
		for i, id := range ids {
			p[i] = "?"
			*args = append(*args, id)
		}
		return strings.Join(p, ",")
	}
	var args []any
	switch {
	case len(sc.CollectionIDs) > 0 && len(sc.ItemIDs) > 0:
		c := ph(sc.CollectionIDs, &args)
		it := ph(sc.ItemIDs, &args)
		return fmt.Sprintf(" AND (%s.collection_id IN (%s) OR %s.id IN (%s))", alias, c, alias, it), args
	case len(sc.CollectionIDs) > 0:
		return fmt.Sprintf(" AND %s.collection_id IN (%s)", alias, ph(sc.CollectionIDs, &args)), args
	case len(sc.ItemIDs) > 0:
		return fmt.Sprintf(" AND %s.id IN (%s)", alias, ph(sc.ItemIDs, &args)), args
	}
	return "", nil
}

// CatchUpLine is one item in a catch-up section.
type CatchUpLine struct {
	Ref        string `json:"ref"`
	Title      string `json:"title"`
	Collection string `json:"collection"`
	// What happened, in a word or two: "finished", "created", "reopened",
	// "commented", "needs a decision", "blocked", or an activity action.
	What  string `json:"what"`
	Actor string `json:"actor,omitempty"`
	Agent string `json:"agent,omitempty"`
	At    string `json:"at,omitempty"`

	itemID string
}

// CatchUpSection is a capped list with the full count beside it.
type CatchUpSection struct {
	Total int           `json:"total"`
	Lines []CatchUpLine `json:"lines"`
}

// CatchUpAgentGroup is one agent's work in the window.
type CatchUpAgentGroup struct {
	Agent   string        `json:"agent"`
	Changes int           `json:"changes"`
	Items   int           `json:"items"`
	Lines   []CatchUpLine `json:"lines"`
}

// CatchUpAgents is the agents section: one group per agent name.
type CatchUpAgents struct {
	Total  int                 `json:"total"`
	Groups []CatchUpAgentGroup `json:"groups"`
}

// CatchUp is the windowed half of the digest. needs_you is current state,
// not windowed, and is assembled by the server from the attention set.
type CatchUp struct {
	Finished       CatchUpSection `json:"finished"`
	NewAndReopened CatchUpSection `json:"new_and_reopened"`
	Comments       CatchUpSection `json:"comments"`
	Agents         CatchUpAgents  `json:"agents"`
}

func capSection(lines []CatchUpLine) CatchUpSection {
	sec := CatchUpSection{Total: len(lines), Lines: lines}
	if len(sec.Lines) > CatchUpLineCap {
		sec.Lines = sec.Lines[:CatchUpLineCap]
	}
	if sec.Lines == nil {
		sec.Lines = []CatchUpLine{}
	}
	return sec
}

// CatchUp reads what happened in workspaceID in (since, until] that viewerID
// did not do, limited to scope. Bounds are compared at second precision, the
// precision the source tables store, and the lower bound is INCLUSIVE: an
// event stamped in the same second as since may have happened after the
// press, so it is shown again rather than risk hiding it. Showing a seen line
// twice is the safe failure; hiding an unseen one is not.
func (s *Store) CatchUp(workspaceID, viewerID string, since, until time.Time, scope CatchUpScope) (*CatchUp, error) {
	out := &CatchUp{
		Finished:       capSection(nil),
		NewAndReopened: capSection(nil),
		Comments:       capSection(nil),
		Agents:         CatchUpAgents{Groups: []CatchUpAgentGroup{}},
	}
	if scope.seesNothing() {
		return out, nil
	}
	sinceStr := since.UTC().Format(time.RFC3339)
	untilStr := until.UTC().Format(time.RFC3339)

	colls, err := s.resolveReportCollections(workspaceID, ReportOptions{})
	if err != nil {
		return nil, err
	}
	finished, reopened, err := s.catchUpTransitions(workspaceID, viewerID, sinceStr, untilStr, scope, colls)
	if err != nil {
		return nil, err
	}
	out.Finished = capSection(finished)

	created, err := s.catchUpCreated(workspaceID, viewerID, sinceStr, untilStr, scope)
	if err != nil {
		return nil, err
	}
	out.NewAndReopened = capSection(mergeNewAndReopened(created, reopened))

	comments, err := s.catchUpComments(workspaceID, viewerID, sinceStr, untilStr, scope)
	if err != nil {
		return nil, err
	}
	out.Comments = capSection(comments)

	agents, err := s.catchUpAgents(workspaceID, sinceStr, untilStr, scope)
	if err != nil {
		return nil, err
	}
	out.Agents = agents
	return out, nil
}

// catchUpTransitions returns the in-window finishes (positive terminal) and
// reopens (terminal to non-terminal), one line per item at its latest such
// transition, newest first, with the viewer's own transitions left out.
//
// status_transitions has no actor, so the actor is read from the activity
// row the same write logged: same item, action updated or moved, same
// second (lead ruling, day 13). A transition is the viewer's only when such
// a row exists and every such row is the viewer's. No row, or another
// person's row in that second, keeps the line: showing a user their own
// change is the safe failure, hiding someone else's is not. Debounced update
// rows move their created_at forward, so an update followed by another edit
// by the same writer misses here and is kept.
func (s *Store) catchUpTransitions(workspaceID, viewerID, sinceStr, untilStr string, scope CatchUpScope, colls []reportCollection) (finished, reopened []CatchUpLine, err error) {
	finished, reopened = []CatchUpLine{}, []CatchUpLine{}
	byColl := make(map[string]reportCollection, len(colls))
	collIDs := make([]string, 0, len(colls))
	for _, c := range colls {
		if c.doneKey == "" || len(c.allTerminals) == 0 {
			continue
		}
		byColl[c.id] = c
		collIDs = append(collIDs, c.id)
	}
	if len(collIDs) == 0 {
		return finished, reopened, nil
	}
	scopeSQL, scopeArgs := scope.clause("i")
	ph := make([]string, len(collIDs))
	args := []any{workspaceID, sinceStr, untilStr}
	for i, id := range collIDs {
		ph[i] = "?"
		args = append(args, id)
	}
	args = append(args, scopeArgs...)
	// The actor columns come from the activity rows of the same second. A
	// LEFT JOIN, so a transition with no row survives with nulls; several
	// rows give several result rows, folded below.
	rows, err := s.db.Query(s.q(`
		SELECT st.item_id, st.collection_id, st.field_key, st.from_status, st.to_status, st.created_at,
		       c.prefix, i.item_number, i.title, c.slug,
		       a.user_id, COALESCE(u.name, ''), COALESCE(a.actor, ''), `+s.activityMetaText("a")+`
		FROM status_transitions st
		JOIN items i ON i.id = st.item_id AND i.deleted_at IS NULL
		JOIN collections c ON c.id = i.collection_id AND c.deleted_at IS NULL
		LEFT JOIN activities a ON a.document_id = st.item_id AND a.action IN ('updated', 'moved')
		     AND a.created_at = st.created_at
		LEFT JOIN users u ON u.id = a.user_id
		WHERE st.workspace_id = ? AND st.created_at >= ? AND st.created_at <= ?
		  AND st.collection_id IN (`+strings.Join(ph, ",")+`)
		  AND i.collection_id = st.collection_id`+scopeSQL+`
		ORDER BY st.created_at DESC, st.id`), args...)
	if err != nil {
		return nil, nil, fmt.Errorf("catch-up transitions: %w", err)
	}
	defer rows.Close()

	type key struct{ item, field, from, to, at string }
	type agg struct {
		line      CatchUpLine
		kind      string // "finished" | "reopened"
		anyRow    bool
		otherRow  bool
		otherName string
		agent     string
	}
	order := []key{}
	seen := map[key]*agg{}
	for rows.Next() {
		var itemID, collID, field, from, to, at, prefix, title, slug, actorName, actorKind, meta string
		var num int
		var userID *string
		if err := rows.Scan(&itemID, &collID, &field, &from, &to, &at, &prefix, &num, &title, &slug,
			&userID, &actorName, &actorKind, &meta); err != nil {
			return nil, nil, fmt.Errorf("scan catch-up transition: %w", err)
		}
		c := byColl[collID]
		if field != c.doneKey {
			continue
		}
		kind := ""
		switch {
		case containsFold(c.positiveTerminals, to):
			kind = "finished"
		case containsFold(c.allTerminals, from) && !containsFold(c.allTerminals, to):
			kind = "reopened"
		default:
			continue
		}
		k := key{itemID, field, from, to, at}
		g, ok := seen[k]
		if !ok {
			g = &agg{kind: kind, line: CatchUpLine{
				Ref: fmt.Sprintf("%s-%d", prefix, num), Title: title, Collection: slug,
				What: kind, At: at, itemID: itemID,
			}}
			seen[k] = g
			order = append(order, k)
		}
		if userID == nil && actorKind == "" {
			continue // the LEFT JOIN found no activity row
		}
		g.anyRow = true
		if userID == nil || *userID != viewerID {
			g.otherRow = true
			if g.otherName == "" {
				g.otherName = actorName
			}
		}
		if actorKind == "agent" && g.agent == "" {
			g.agent = models.AgentNameFromMetadata(meta)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("catch-up transitions: %w", err)
	}

	// One line per item per kind, at its newest kept transition (rows are
	// newest first, so the first kept one wins).
	done := map[string]bool{}
	for _, k := range order {
		g := seen[k]
		if g.anyRow && !g.otherRow {
			continue // the viewer's own
		}
		dk := g.kind + "\x00" + k.item
		if done[dk] {
			continue
		}
		done[dk] = true
		g.line.Actor = g.otherName
		g.line.Agent = g.agent
		if g.kind == "finished" {
			finished = append(finished, g.line)
		} else {
			reopened = append(reopened, g.line)
		}
	}
	return finished, reopened, nil
}

// catchUpCreated returns items created in the window by someone other than
// the viewer, newest first. System collections (conventions, playbooks) are
// how the workspace is run, not its work, and are left out. An item with no
// recorded creator is kept.
func (s *Store) catchUpCreated(workspaceID, viewerID, sinceStr, untilStr string, scope CatchUpScope) ([]CatchUpLine, error) {
	scopeSQL, scopeArgs := scope.clause("i")
	args := append([]any{s.dialect.BoolToInt(false), workspaceID, sinceStr, untilStr, viewerID}, scopeArgs...)
	rows, err := s.db.Query(s.q(`
		SELECT i.id, c.prefix, i.item_number, i.title, c.slug, i.created_at, COALESCE(u.name, ''), COALESCE(i.created_by, '')
		FROM items i
		JOIN collections c ON c.id = i.collection_id AND c.deleted_at IS NULL AND c.is_system = ?
		LEFT JOIN users u ON u.id = i.created_by_user_id
		WHERE i.workspace_id = ? AND i.deleted_at IS NULL
		  AND i.created_at >= ? AND i.created_at <= ?
		  AND (i.created_by_user_id IS NULL OR i.created_by_user_id <> ?)`+scopeSQL+`
		ORDER BY i.created_at DESC, i.id`), args...)
	if err != nil {
		return nil, fmt.Errorf("catch-up created: %w", err)
	}
	defer rows.Close()
	out := []CatchUpLine{}
	for rows.Next() {
		var l CatchUpLine
		var num int
		var prefix, createdBy string
		if err := rows.Scan(&l.itemID, &prefix, &num, &l.Title, &l.Collection, &l.At, &l.Actor, &createdBy); err != nil {
			return nil, fmt.Errorf("scan catch-up created: %w", err)
		}
		l.Ref = fmt.Sprintf("%s-%d", prefix, num)
		l.What = "created"
		out = append(out, l)
	}
	return out, rows.Err()
}

// mergeNewAndReopened interleaves the two lists newest first, one line per
// item: an item created and then reopened in the same window shows once, as
// its newest event.
func mergeNewAndReopened(created, reopened []CatchUpLine) []CatchUpLine {
	all := append(append([]CatchUpLine{}, created...), reopened...)
	sort.SliceStable(all, func(a, b int) bool { return all[a].At > all[b].At })
	seen := map[string]bool{}
	out := []CatchUpLine{}
	for _, l := range all {
		if seen[l.itemID] {
			continue
		}
		seen[l.itemID] = true
		out = append(out, l)
	}
	return out
}

// catchUpComments returns comments by others in the window on items the
// viewer created or is assigned to (Dave, v1), one line per item at its
// newest such comment. Deleted comments are left out.
func (s *Store) catchUpComments(workspaceID, viewerID, sinceStr, untilStr string, scope CatchUpScope) ([]CatchUpLine, error) {
	scopeSQL, scopeArgs := scope.clause("i")
	args := append([]any{workspaceID, sinceStr, untilStr, viewerID, viewerID, viewerID}, scopeArgs...)
	rows, err := s.db.Query(s.q(`
		SELECT i.id, c.prefix, i.item_number, i.title, c.slug, cm.created_at, COALESCE(u.name, cm.author, '')
		FROM comments cm
		JOIN items i ON i.id = cm.item_id AND i.deleted_at IS NULL
		JOIN collections c ON c.id = i.collection_id AND c.deleted_at IS NULL
		LEFT JOIN users u ON u.id = cm.user_id
		WHERE cm.workspace_id = ? AND cm.deleted_at IS NULL
		  AND cm.created_at >= ? AND cm.created_at <= ?
		  AND (cm.user_id IS NULL OR cm.user_id <> ?)
		  AND (i.created_by_user_id = ? OR i.assigned_user_id = ?)`+scopeSQL+`
		ORDER BY cm.created_at DESC, cm.id`), args...)
	if err != nil {
		return nil, fmt.Errorf("catch-up comments: %w", err)
	}
	defer rows.Close()
	out := []CatchUpLine{}
	seen := map[string]bool{}
	for rows.Next() {
		var l CatchUpLine
		var num int
		var prefix string
		if err := rows.Scan(&l.itemID, &prefix, &num, &l.Title, &l.Collection, &l.At, &l.Actor); err != nil {
			return nil, fmt.Errorf("scan catch-up comment: %w", err)
		}
		if seen[l.itemID] {
			continue
		}
		seen[l.itemID] = true
		l.Ref = fmt.Sprintf("%s-%d", prefix, num)
		l.What = "commented"
		out = append(out, l)
	}
	return out, rows.Err()
}

// catchUpAgents groups the window's agent activity by agent name. Agent rows
// carry the HUMAN's user_id, so the viewer's own agents are deliberately
// kept: their work is exactly what a returning person wants to see. The name
// is read with the renderer's accessor, in Go, so a malformed metadata row
// is one unnamed row rather than a failed query.
func (s *Store) catchUpAgents(workspaceID, sinceStr, untilStr string, scope CatchUpScope) (CatchUpAgents, error) {
	out := CatchUpAgents{Groups: []CatchUpAgentGroup{}}
	scopeSQL, scopeArgs := scope.clause("i")
	args := append([]any{workspaceID, sinceStr, untilStr}, scopeArgs...)
	rows, err := s.db.Query(s.q(`
		SELECT a.action, a.created_at, `+s.activityMetaText("a")+`, i.id, c.prefix, i.item_number, i.title, c.slug
		FROM activities a
		JOIN items i ON i.id = a.document_id AND i.deleted_at IS NULL
		JOIN collections c ON c.id = i.collection_id AND c.deleted_at IS NULL
		WHERE a.workspace_id = ? AND a.actor = 'agent'
		  AND a.created_at >= ? AND a.created_at <= ?`+scopeSQL+`
		ORDER BY a.created_at DESC, a.id`), args...)
	if err != nil {
		return out, fmt.Errorf("catch-up agents: %w", err)
	}
	defer rows.Close()
	type grp struct {
		g     CatchUpAgentGroup
		items map[string]bool
		lines []CatchUpLine
	}
	byName := map[string]*grp{}
	order := []string{}
	for rows.Next() {
		var action, at, meta, itemID, prefix, title, slug string
		var num int
		if err := rows.Scan(&action, &at, &meta, &itemID, &prefix, &num, &title, &slug); err != nil {
			return out, fmt.Errorf("scan catch-up agent row: %w", err)
		}
		name := models.AgentNameFromMetadata(meta)
		if name == "" {
			name = "agent"
		}
		g, ok := byName[name]
		if !ok {
			g = &grp{g: CatchUpAgentGroup{Agent: name}, items: map[string]bool{}}
			byName[name] = g
			order = append(order, name)
		}
		g.g.Changes++
		if g.items[itemID] {
			continue
		}
		g.items[itemID] = true
		g.lines = append(g.lines, CatchUpLine{
			Ref: fmt.Sprintf("%s-%d", prefix, num), Title: title, Collection: slug,
			What: action, Agent: name, At: at, itemID: itemID,
		})
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("catch-up agents: %w", err)
	}
	// Groups in order of their newest activity (rows are newest first).
	for _, name := range order {
		g := byName[name]
		g.g.Items = len(g.lines)
		g.g.Lines = capSection(g.lines).Lines
		out.Groups = append(out.Groups, g.g)
	}
	out.Total = len(out.Groups)
	return out, nil
}

// CatchUpItemLines renders the given items as section lines, limited to
// scope, in the order given. Ids that are deleted, invisible or unknown are
// dropped, so the section's total counts only what the viewer may see.
func (s *Store) CatchUpItemLines(workspaceID string, itemIDs []string, what map[string]string, scope CatchUpScope) (CatchUpSection, error) {
	if len(itemIDs) == 0 || scope.seesNothing() {
		return capSection(nil), nil
	}
	scopeSQL, scopeArgs := scope.clause("i")
	ph := make([]string, len(itemIDs))
	args := []any{workspaceID}
	for i, id := range itemIDs {
		ph[i] = "?"
		args = append(args, id)
	}
	args = append(args, scopeArgs...)
	rows, err := s.db.Query(s.q(`
		SELECT i.id, c.prefix, i.item_number, i.title, c.slug
		FROM items i
		JOIN collections c ON c.id = i.collection_id AND c.deleted_at IS NULL
		WHERE i.workspace_id = ? AND i.deleted_at IS NULL
		  AND i.id IN (`+strings.Join(ph, ",")+`)`+scopeSQL), args...)
	if err != nil {
		return CatchUpSection{}, fmt.Errorf("catch-up item lines: %w", err)
	}
	defer rows.Close()
	byID := map[string]CatchUpLine{}
	for rows.Next() {
		var l CatchUpLine
		var num int
		var prefix string
		if err := rows.Scan(&l.itemID, &prefix, &num, &l.Title, &l.Collection); err != nil {
			return CatchUpSection{}, fmt.Errorf("scan catch-up item line: %w", err)
		}
		l.Ref = fmt.Sprintf("%s-%d", prefix, num)
		l.What = what[l.itemID]
		byID[l.itemID] = l
	}
	if err := rows.Err(); err != nil {
		return CatchUpSection{}, fmt.Errorf("catch-up item lines: %w", err)
	}
	lines := []CatchUpLine{}
	for _, id := range itemIDs {
		if l, ok := byID[id]; ok {
			lines = append(lines, l)
		}
	}
	return capSection(lines), nil
}

// activityMetaText reads activities.metadata as text on both backends: it is
// TEXT on SQLite and JSONB on Postgres, where a COALESCE to the empty string
// would cast that string to JSON and fail the query.
func (s *Store) activityMetaText(alias string) string {
	if s.dialect.Driver() == DriverPostgres {
		return "COALESCE(" + alias + ".metadata::text, '')"
	}
	return "COALESCE(" + alias + ".metadata, '')"
}

func containsFold(list []string, v string) bool {
	for _, x := range list {
		if strings.EqualFold(x, v) {
			return true
		}
	}
	return false
}
