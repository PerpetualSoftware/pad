package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/PerpetualSoftware/pad/internal/decision"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/needs"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// The cross-workspace needs-you inbox (TASK-3920, IDEA-3906 half A unit A4).
// Documented in docs/api.md under GET /me/needs.

const (
	needsDefaultLimit = 50
	needsMaxLimit     = 200
	needsViewActive   = "active"
	needsViewHidden   = "hidden"
)

type needsWorkspaceCount struct {
	Slug          string `json:"slug"`
	Name          string `json:"name"`
	OwnerUsername string `json:"owner_username"`
	Count         int    `json:"count"`
}

type needsResponse struct {
	Rows       []*needs.Row          `json:"rows"`
	NextCursor string                `json:"next_cursor,omitempty"`
	Total      int                   `json:"total"`
	Hidden     int                   `json:"hidden"`
	Workspaces []needsWorkspaceCount `json:"workspaces"`
	// Degraded lists the admitted workspaces whose scorer failed, by slug.
	// It is set ONLY on a provider error, never from the decision provider's
	// failing bit: that bit is computed over the whole workspace, items the
	// caller cannot see included, so surfacing it would leak one bit about them.
	Degraded []string `json:"degraded,omitempty"`
}

// An unknown, not admitted or token-excluded ?workspace= needs no special
// case: it leaves no admitted workspace, so the pipeline below answers the
// same empty 200 as a known workspace with nothing waiting, byte for byte
// (rows [], total 0, hidden 0, workspaces [], no degraded).

// handleMeNeeds lists, across every workspace the caller belongs to or has a
// grant in, the open items that need THIS person: oldest-waiting first by default, or by importance
// or simplicity (highest first, the other score breaking ties).
//
//	GET /me/needs?sort=age|importance|simplicity&workspace=&view=active|hidden&limit=50&cursor=
func (s *Server) handleMeNeeds(w http.ResponseWriter, r *http.Request) {
	// An app principal has its own door; refused before anything else, since
	// app contexts also set a current user.
	if appContextFrom(r) != nil {
		writeError(w, http.StatusNotFound, "not_found", "Not found")
		return
	}
	user := currentUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Sign in to see what needs you")
		return
	}
	q := r.URL.Query()
	sortBy, view, wsSlug := q.Get("sort"), q.Get("view"), q.Get("workspace")
	if sortBy == "" {
		sortBy = needs.SortAge
	}
	if !needs.ValidSort(sortBy) {
		writeError(w, http.StatusBadRequest, "validation_error", "sort must be age, importance or simplicity")
		return
	}
	if view == "" {
		view = needsViewActive
	}
	if view != needsViewActive && view != needsViewHidden {
		writeError(w, http.StatusBadRequest, "validation_error", "view must be active or hidden")
		return
	}
	limit := needsDefaultLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "validation_error", "limit must be a positive integer")
			return
		}
		limit = min(n, needsMaxLimit)
	}
	now := time.Now().UTC().Truncate(time.Second)
	asOf := now
	var after *needs.Key
	if tok := q.Get("cursor"); tok != "" {
		c, err := needs.DecodeCursor(tok, sortBy, view, wsSlug)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_cursor", "The cursor is not valid for this request")
			return
		}
		k := c.Key()
		after, asOf = &k, c.At()
		if asOf.After(now) {
			asOf = now // a forged future marker is clamped, never trusted
		}
	}

	admitted, err := s.needsAdmittedWorkspaces(r, user)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if wsSlug != "" {
		kept := admitted[:0]
		for _, ws := range admitted {
			if ws.Slug == wsSlug {
				kept = append(kept, ws)
			}
		}
		admitted = kept
	}

	var all []*needs.Row
	var degraded []string
	for i := range admitted {
		rows, deg, err := s.needsForWorkspace(r, user, &admitted[i], asOf)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		all = append(all, rows...)
		if deg {
			degraded = append(degraded, admitted[i].Slug)
		}
	}
	sort.Strings(degraded)

	resp := needsResponse{Rows: []*needs.Row{}, Workspaces: []needsWorkspaceCount{}, Degraded: degraded}
	counts := map[string]*needsWorkspaceCount{}
	var inView []*needs.Row
	for _, row := range all {
		if row.Held {
			resp.Hidden++
		}
		if row.Held != (view == needsViewHidden) {
			continue
		}
		inView = append(inView, row)
		c := counts[row.WorkspaceID]
		if c == nil {
			c = &needsWorkspaceCount{Slug: row.Workspace.Slug, Name: row.Workspace.Name, OwnerUsername: row.Workspace.OwnerUsername}
			counts[row.WorkspaceID] = c
		}
		c.Count++
	}
	for _, c := range counts {
		resp.Workspaces = append(resp.Workspaces, *c)
	}
	sort.Slice(resp.Workspaces, func(a, b int) bool { return resp.Workspaces[a].Slug < resp.Workspaces[b].Slug })
	resp.Total = len(inView)

	sort.Slice(inView, func(a, b int) bool { return needs.KeyFor(sortBy, inView[a]).Less(needs.KeyFor(sortBy, inView[b])) })
	if after != nil {
		i := sort.Search(len(inView), func(i int) bool { return after.Less(needs.KeyFor(sortBy, inView[i])) })
		inView = inView[i:]
	}
	more := len(inView) > limit
	if more {
		inView = inView[:limit]
	}
	if err := s.needsAttachMarkers(user.ID, inView); err != nil {
		writeInternalError(w, err)
		return
	}
	if len(inView) > 0 {
		resp.Rows = inView
		if more {
			resp.NextCursor = needs.NewCursor(sortBy, view, wsSlug, asOf, inView[len(inView)-1]).Encode()
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// needsAdmittedWorkspaces is the workspaces the caller may be shown needs from:
// memberships and guest workspaces, narrowed to a token's consent allow-list
// and, for a token bound to one workspace, to that workspace alone. Since
// BUG-3982 every door confines a pinned user token that way; this ID filter
// predates it and is kept.
func (s *Server) needsAdmittedWorkspaces(r *http.Request, user *models.User) ([]models.Workspace, error) {
	member, err := s.store.GetUserWorkspaces(user.ID)
	if err != nil {
		return nil, err
	}
	guestIDs, err := s.store.GuestWorkspaceIDs(user.ID)
	if err != nil {
		return nil, err
	}
	guests, err := s.store.GetWorkspacesByIDs(guestIDs)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var all []models.Workspace
	for _, group := range [][]models.Workspace{member, guests} {
		for _, ws := range group {
			if !seen[ws.ID] {
				seen[ws.ID] = true
				all = append(all, ws)
			}
		}
	}
	all = filterWorkspacesByTokenAllowlist(r.Context(), all)
	if id := tokenWorkspaceID(r); id != "" {
		kept := all[:0]
		for _, ws := range all {
			if ws.ID == id {
				kept = append(kept, ws)
			}
		}
		all = kept
	}
	return all, nil
}

// needsVisibility is what one user may see in one workspace.
type needsVisibility struct {
	unrestricted  bool
	collections   map[string]bool
	items         map[string]bool
	collectionIDs []string
	itemIDs       []string
}

// needsEligibility answers router eligibility for one workspace and caches it
// per user: a live human who may see the item, by the same bearer-style rule
// as cross-workspace backlinks (a platform admin counts by membership only).
type needsEligibility struct {
	s     *Server
	wsID  string
	cache map[string]*needsVisibility
	err   error
}

func (e *needsEligibility) visibility(userID string) *needsVisibility {
	if v, ok := e.cache[userID]; ok {
		return v
	}
	var v *needsVisibility
	// Load the user FIRST. ResolveBacklinksVisibility answers (nil, nil, nil)
	// for a user that does not exist, which is also its "unrestricted"
	// sentinel: asked about a missing user it would hand back everything.
	u, err := e.s.store.GetUser(userID)
	switch {
	case err != nil:
		e.err = err
	case u != nil && u.Kind == models.UserKindHuman && u.DisabledAt == "":
		full, granted, err := e.s.store.ResolveBacklinksVisibility(userID, e.wsID, false, true)
		if err != nil {
			e.err = err
			break
		}
		v = &needsVisibility{unrestricted: full == nil && granted == nil, collectionIDs: full, itemIDs: granted,
			collections: map[string]bool{}, items: map[string]bool{}}
		for _, id := range full {
			v.collections[id] = true
		}
		for _, id := range granted {
			v.items[id] = true
		}
	}
	e.cache[userID] = v
	return v
}

func (e *needsEligibility) Eligible(userID, collectionID, itemID string) bool {
	v := e.visibility(userID)
	return v != nil && (v.unrestricted || v.collections[collectionID] || v.items[itemID])
}

// callerVisibleDecidedItems returns the ids of the items holding an attention
// decision row that fall inside the caller's scope.
func (s *Server) callerVisibleDecidedItems(workspaceID string, scope store.CatchUpScope) ([]string, error) {
	latest, err := s.store.LatestWorkspaceDecisions(workspaceID, decision.AttentionSetName)
	if err != nil {
		return nil, err
	}
	var decided []string
	seen := map[string]bool{}
	for _, d := range latest {
		if !seen[d.ItemID] {
			seen[d.ItemID] = true
			decided = append(decided, d.ItemID)
		}
	}
	if len(decided) == 0 {
		return nil, nil
	}
	// The candidate read applies the scope in SQL; what it returns is visible.
	items, err := s.store.NeedsCandidateItems(workspaceID, scope, store.NeedsCandidateParams{FlaggedItemIDs: decided})
	if err != nil {
		return nil, err
	}
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.ID
	}
	return out, nil
}

// needsForWorkspace runs the router for one admitted workspace and returns the
// rows routed to the caller, held ones included (the caller splits the views).
// Order matters for what is read: visibility first (in the candidate SQL), then
// everything else only for the surviving ids.
func (s *Server) needsForWorkspace(r *http.Request, user *models.User, ws *models.Workspace, asOf time.Time) (rows []*needs.Row, degraded bool, err error) {
	elig := &needsEligibility{s: s, wsID: ws.ID, cache: map[string]*needsVisibility{}}
	vis := elig.visibility(user.ID)
	if elig.err != nil {
		return nil, false, elig.err
	}
	if vis == nil {
		return nil, false, nil
	}
	scope := store.CatchUpScope{}
	if !vis.unrestricted {
		scope = store.CatchUpScope{CollectionIDs: vis.collectionIDs, ItemIDs: vis.itemIDs}
		if scope.CollectionIDs == nil {
			scope.CollectionIDs = []string{}
		}
	}

	colls, err := s.store.ListCollectionsMinimal(ws.ID)
	if err != nil {
		return nil, false, err
	}
	type collInfo struct {
		schema   models.CollectionSchema
		settings models.CollectionSettings
		awaits   bool
	}
	infos := map[string]*collInfo{}
	var awaitsIDs []string
	for _, c := range colls {
		ci := &collInfo{awaits: !c.IsSystem && models.CollectionAwaitsHumanJSON(c.Settings)}
		_ = models.UnmarshalItemFieldSchema([]byte(c.Schema), &ci.schema)
		if c.Settings != "" {
			_ = json.Unmarshal([]byte(c.Settings), &ci.settings)
		}
		infos[c.ID] = ci
		if ci.awaits {
			awaitsIDs = append(awaitsIDs, c.ID)
		}
	}

	// needs_human answers. A provider ERROR degrades this workspace; the
	// provider's failing bit is deliberately not read (see needsResponse).
	var flaggedIDs []string
	flaggedAt := map[string]time.Time{}
	flaggedSet := map[string]bool{}
	// Only items the caller can see are evaluated: an error caused by a hidden
	// item must not set degraded, which would leak one bit about it.
	visibleDecided, derr := s.callerVisibleDecidedItems(ws.ID, scope)
	if derr != nil {
		return nil, false, derr
	}
	flags, _, ferr := s.decisionRunner().WorkspaceFlagsForItems(ws.ID, decision.AttentionSetName, decision.AttentionThreshold, visibleDecided)
	if ferr != nil {
		slog.Error("needs: attention decisions failed; workspace degraded", "workspace_id", ws.ID, "error", ferr)
		degraded = true
	} else {
		for id, answers := range flags {
			if _, ok := answers[decision.AttentionNeedsHuman]; ok {
				flaggedIDs = append(flaggedIDs, id)
			}
		}
		sort.Strings(flaggedIDs)
		for _, id := range flaggedIDs {
			flaggedSet[id] = true
		}
		if len(flaggedIDs) > 0 {
			latest, err := s.store.LatestWorkspaceDecisions(ws.ID, decision.AttentionSetName)
			if err != nil {
				return nil, false, err
			}
			for _, d := range latest {
				if d.QuestionKey != decision.AttentionNeedsHuman || !flaggedSet[d.ItemID] {
					continue
				}
				if t, perr := time.Parse("2006-01-02T15:04:05.000000000Z", d.EvaluatedAt); perr == nil {
					flaggedAt[d.ItemID] = t
				}
			}
		}
	}

	// An owner can be routed anything the other rules leave over, so reads all
	// candidates; anyone else only what names them.
	member, err := s.store.GetWorkspaceMember(ws.ID, user.ID)
	if err != nil {
		return nil, false, err
	}
	params := store.NeedsCandidateParams{AwaitsCollectionIDs: awaitsIDs, FlaggedItemIDs: flaggedIDs}
	if member == nil || member.Role != "owner" {
		params.ForUserID = user.ID
	}
	items, err := s.store.NeedsCandidateItems(ws.ID, scope, params)
	if err != nil {
		return nil, false, err
	}
	open := items[:0]
	for _, it := range items {
		ci := infos[it.CollectionID]
		if ci == nil {
			continue
		}
		var fields map[string]any
		if it.Fields != "" {
			_ = json.Unmarshal([]byte(it.Fields), &fields)
		}
		if !models.IsTerminalItem(fields, ci.schema, ci.settings) {
			open = append(open, it)
		}
	}
	items = open
	if len(items) == 0 {
		return nil, degraded, nil
	}

	ids := make([]string, len(items))
	mine := map[string]string{}
	for i, it := range items {
		ids[i] = it.ID
		if it.AssignedUserID != nil && *it.AssignedUserID == user.ID && it.AssignedUserName != "" {
			mine[it.ID] = it.AssignedUserName
		}
	}
	mentions, err := s.store.OpenMentionsForItems(ids)
	if err != nil {
		return nil, false, err
	}
	assignedAt, err := s.store.LatestAssignmentTimes(ws.ID, mine)
	if err != nil {
		return nil, false, err
	}
	owners, err := s.store.WorkspaceOwnerIDs(ws.ID)
	if err != nil {
		return nil, false, err
	}

	label := needs.WorkspaceLabel{Slug: ws.Slug, Name: ws.Name, OwnerUsername: ws.OwnerUsername}
	if label.OwnerUsername == "" && ws.OwnerID != "" {
		if u, err := s.store.GetUser(ws.OwnerID); err == nil && u != nil {
			label.OwnerUsername = u.Username
		}
	}
	routed := map[string]needs.Routing{}
	var kept []models.Item
	for _, it := range items {
		c := needs.Candidate{
			ItemID: it.ID, CollectionID: it.CollectionID, AwaitsHuman: infos[it.CollectionID].awaits,
			AssignedAt: it.CreatedAt, CreatedAt: it.CreatedAt,
		}
		if it.AssignedUserID != nil {
			c.AssigneeUserID = *it.AssignedUserID
		}
		if t, ok := assignedAt[it.ID]; ok {
			c.AssignedAt = t
		}
		for _, m := range mentions[it.ID] {
			c.Mentions = append(c.Mentions, needs.Mention{UserID: m.UserID, CommentID: m.CommentID, By: m.By, At: m.At})
		}
		if flaggedSet[it.ID] {
			if t, ok := flaggedAt[it.ID]; ok {
				c.NeedsHuman = &t
			} else {
				t := it.UpdatedAt
				c.NeedsHuman = &t
			}
		}
		rt, ok := needs.Resolve(c, owners, elig)
		if elig.err != nil {
			return nil, false, elig.err
		}
		if ok && rt.Includes(user.ID) {
			routed[it.ID] = rt.For(user.ID)
			kept = append(kept, it)
		}
	}
	if len(kept) == 0 {
		return nil, degraded, nil
	}

	keptIDs := make([]string, len(kept))
	for i, it := range kept {
		keptIDs[i] = it.ID
	}
	snoozes, err := s.store.NeedsSnoozesForItems(user.ID, ws.ID, keptIDs, asOf)
	if err != nil {
		return nil, false, err
	}
	for _, it := range kept {
		rt := routed[it.ID]
		row := &needs.Row{
			Workspace: label,
			Item: needs.ItemInfo{
				Ref: it.Ref, Title: it.Title, Collection: it.CollectionName, CollectionSlug: it.CollectionSlug,
				CreatedAt: it.CreatedAt, UpdatedAt: it.UpdatedAt,
				Path: "/" + label.OwnerUsername + "/" + ws.Slug + "/" + it.CollectionSlug + "/" + it.Slug,
			},
			Why: rt.Why, Because: rt.Because, WaitingSince: rt.WaitingSince,
			ItemID: it.ID, WorkspaceID: ws.ID,
		}
		var fields map[string]any
		_ = json.Unmarshal([]byte(it.Fields), &fields)
		row.Item.Status, _ = fields["status"].(string)
		row.Item.Priority, _ = fields["priority"].(string)
		for _, b := range rt.Because {
			if b.Kind == needs.BecauseNeedsHuman {
				at := b.At
				row.NeedsHuman = &at
			}
		}
		if sn := snoozes[it.ID]; sn != nil && sn.Holds {
			row.Held, row.Snooze = true, sn
		}
		rows = append(rows, row)
	}

	// Scorers run last: after visibility, routing and snooze, before sort and
	// pagination. A failing one degrades this workspace only.
	nscope := needs.Scope{WorkspaceID: ws.ID, Unrestricted: vis.unrestricted, CollectionIDs: scope.CollectionIDs, ItemIDs: scope.ItemIDs}
	if nscope.CollectionIDs == nil {
		nscope.CollectionIDs = []string{}
	}
	if nscope.ItemIDs == nil {
		nscope.ItemIDs = []string{}
	}
	// The scorers share this workspace's batch reads through a memo that lives
	// for this call alone (see needsMemo).
	sctx := withNeedsMemo(r.Context(), s.newNeedsMemo(ws.ID))
	for _, sc := range s.needsScorers {
		deg, serr := sc.Score(sctx, needs.Viewer{UserID: user.ID, AuthIsBearer: isBearerAuth(r), Now: asOf}, nscope, rows)
		if serr != nil {
			slog.Error("needs: scorer failed; workspace degraded", "scorer", sc.Name(), "workspace_id", ws.ID, "error", serr)
		}
		if deg || serr != nil {
			degraded = true
		}
	}
	return rows, degraded, nil
}

// needsAttachMarkers fills each shown row's marker: the item's state as the
// viewer sees it now, one batch per workspace.
func (s *Server) needsAttachMarkers(userID string, rows []*needs.Row) error {
	byWS := map[string][]*needs.Row{}
	for _, row := range rows {
		byWS[row.WorkspaceID] = append(byWS[row.WorkspaceID], row)
	}
	for _, group := range byWS {
		ids := make([]string, len(group))
		for i, row := range group {
			ids[i] = row.ItemID
		}
		markers, err := s.store.NeedsMarkersForItems(userID, ids)
		if err != nil {
			return err
		}
		for _, row := range group {
			m, ok := markers[row.ItemID]
			if !ok {
				return errors.New("needs: no marker for a shown row")
			}
			row.Marker = &m
		}
	}
	return nil
}
