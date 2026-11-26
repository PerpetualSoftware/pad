package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

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
	now := s.agendaNow().UTC().Truncate(time.Second)
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
		ws, err := s.findVisibleWorkspace(admitted, wsSlug)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		admitted = nil
		if ws != nil {
			admitted = []models.Workspace{*ws}
		}
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
	if err := s.needsAttachMarkers(user.ID, asOf, inView); err != nil {
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
	return s.needsAdmittedWorkspacesQ(s.store.QContext(r.Context()), r, user)
}

func (s *Server) needsAdmittedWorkspacesQ(q store.Queryer, r *http.Request, user *models.User) ([]models.Workspace, error) {
	member, err := s.store.GetUserWorkspacesQ(q, user.ID)
	if err != nil {
		return nil, err
	}
	guestIDs, err := s.store.GuestWorkspaceIDsQ(q, user.ID)
	if err != nil {
		return nil, err
	}
	guests, err := s.store.GetWorkspacesByIDsQ(q, guestIDs)
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

// needsWaitingSpellings returns the waiting values plus the done field's own
// spelling of each: awaits_human_options match their options
// case-insensitively, and an item stores the option's spelling, which the
// candidate SQL must see verbatim (SQLite's LOWER folds ASCII only).
func needsWaitingSpellings(schema models.CollectionSchema, key string, vals []string) []string {
	out := append([]string(nil), vals...)
	seen := map[string]bool{}
	for _, v := range vals {
		seen[strings.TrimSpace(v)] = true
	}
	for _, f := range schema.Fields {
		if f.Key != key {
			continue
		}
		for _, opt := range f.Options {
			o := strings.TrimSpace(opt)
			if seen[o] {
				continue
			}
			for _, v := range vals {
				if strings.EqualFold(o, strings.TrimSpace(v)) {
					seen[o] = true
					out = append(out, o)
					break
				}
			}
		}
		break
	}
	return out
}

// needsCollInfo is what the router reads from one collection: its parsed
// schema and settings, whether it is a collection of asks, and its waiting
// statuses.
type needsCollInfo struct {
	schema   models.CollectionSchema
	settings models.CollectionSettings
	awaits   bool
	// waitKey and waiting are the done field and its waiting statuses
	// (lowercased, trimmed); waiting is empty when it declares none.
	waitKey string
	waiting map[string]bool
}

// newNeedsCollInfo parses one collection for the router. vals are the done
// field's waiting statuses as declared; a system collection has none and is
// never a collection of asks.
func newNeedsCollInfo(c models.Collection) (ci *needsCollInfo, vals []string) {
	ci = &needsCollInfo{awaits: !c.IsSystem && models.CollectionAwaitsHumanJSON(c.Settings)}
	_ = models.UnmarshalItemFieldSchema([]byte(c.Schema), &ci.schema)
	if c.Settings != "" {
		_ = json.Unmarshal([]byte(c.Settings), &ci.settings)
	}
	if c.IsSystem {
		return ci, nil
	}
	key, vals := models.AwaitsHumanValuesForDoneField(ci.schema, ci.settings)
	if len(vals) > 0 {
		ci.waitKey, ci.waiting = key, map[string]bool{}
		for _, v := range vals {
			ci.waiting[strings.ToLower(strings.TrimSpace(v))] = true
		}
	}
	return ci, vals
}

// isWaiting reports whether an item's done field holds a waiting status.
func (ci *needsCollInfo) isWaiting(fields map[string]any) bool {
	v, ok := fields[ci.waitKey].(string)
	return ok && ci.waiting[strings.ToLower(strings.TrimSpace(v))]
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

	due, err := s.store.NeedsDueOccurrences(ws.ID, scope, user.ID, asOf)
	if err != nil {
		return nil, false, err
	}
	dueIDs := make([]string, 0, len(due))
	for id := range due {
		dueIDs = append(dueIDs, id)
	}
	sort.Strings(dueIDs)

	colls, err := s.store.ListCollectionsMinimal(ws.ID)
	if err != nil {
		return nil, false, err
	}
	infos := map[string]*needsCollInfo{}
	var awaitsIDs []string
	var awaitsStatus []store.NeedsAwaitsStatus
	for _, c := range colls {
		ci, vals := newNeedsCollInfo(c)
		infos[c.ID] = ci
		if ci.awaits {
			awaitsIDs = append(awaitsIDs, c.ID)
		}
		if len(vals) > 0 {
			awaitsStatus = append(awaitsStatus, store.NeedsAwaitsStatus{CollectionID: c.ID, Key: ci.waitKey, Values: needsWaitingSpellings(ci.schema, ci.waitKey, vals)})
		}
	}

	// An owner can be routed an ask nobody else can take, so reads all
	// candidates; anyone else only what names them.
	member, err := s.store.GetWorkspaceMember(ws.ID, user.ID)
	if err != nil {
		return nil, false, err
	}
	params := store.NeedsCandidateParams{AwaitsCollectionIDs: awaitsIDs, AwaitsStatus: awaitsStatus, DueItemIDs: dueIDs}
	if member == nil || member.Role != "owner" {
		params.ForUserID = user.ID
	}
	items, err := s.store.NeedsCandidateItems(ws.ID, scope, params)
	if err != nil {
		return nil, false, err
	}
	open := items[:0]
	waitingIDs := map[string]bool{}
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
			if ci.isWaiting(fields) {
				waitingIDs[it.ID] = true
			}
		}
	}
	items = open
	if len(items) == 0 {
		return nil, degraded, nil
	}

	ids := make([]string, len(items))
	mine := map[string]store.AssigneeRef{}
	for i, it := range items {
		ids[i] = it.ID
		if it.AssignedUserID != nil && *it.AssignedUserID == user.ID {
			mine[it.ID] = store.AssigneeRef{UserID: *it.AssignedUserID, Name: it.AssignedUserName}
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
		// BUG-4050: a store error is not a missing owner. GetUser maps
		// sql.ErrNoRows to (nil, nil), which leaves the label empty (an
		// unlinked row); any other error fails the request like the rest.
		u, err := s.store.GetUser(ws.OwnerID)
		if err != nil {
			return nil, false, err
		}
		if u != nil {
			label.OwnerUsername = u.Username
		}
	}
	routed := map[string]needs.Routing{}
	asked := map[string]bool{}
	cands := map[string]needs.Candidate{}
	var kept []models.Item
	for _, it := range items {
		c := needs.Candidate{
			ItemID: it.ID, CollectionID: it.CollectionID, AwaitsHuman: infos[it.CollectionID].awaits,
			WaitingStatus: waitingIDs[it.ID],
			AssignedAt:    it.CreatedAt, CreatedAt: it.CreatedAt,
		}
		if it.AgentRoleID != nil {
			c.AgentRoleID = *it.AgentRoleID
		}
		if d, ok := due[it.ID]; ok {
			at := d.Earliest
			c.Due, c.DueRecipientID = &at, user.ID
		}
		if it.AssignedUserID != nil {
			c.AssigneeUserID = *it.AssignedUserID
		}
		if t, ok := assignedAt[it.ID]; ok {
			c.AssignedAt = t
		}
		for _, m := range mentions[it.ID] {
			c.Mentions = append(c.Mentions, needs.Mention{UserID: m.UserID, CommentID: m.CommentID, By: m.By, At: m.At, CommentAt: m.CommentAt})
		}
		rt, ok := needs.Resolve(c, owners, elig)
		if elig.err != nil {
			return nil, false, elig.err
		}
		if ok && rt.Includes(user.ID) {
			routed[it.ID] = rt.For(user.ID)
			asked[it.ID] = c.Asked()
			cands[it.ID] = c
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
	notMine, err := s.store.NeedsNotMineForItems(user.ID, ws.ID, keptIDs)
	if err != nil {
		return nil, false, err
	}
	edit, err := s.needsEditRights(r, user, ws, member)
	if err != nil {
		return nil, false, err
	}
	for _, it := range kept {
		rt := routed[it.ID]
		// A workspace whose owner no longer resolves has no route to link to:
		// "/" + "" + "/" + slug would be "//<slug>/...", a network-path
		// reference. The row is still the caller's, so it stays, with an
		// empty path (BUG-4050).
		path := ""
		if label.OwnerUsername != "" {
			path = "/" + label.OwnerUsername + "/" + ws.Slug + "/" + it.CollectionSlug + "/" + it.Slug
		}
		row := &needs.Row{
			Workspace: label,
			Item: needs.ItemInfo{
				Ref: it.Ref, Title: it.Title, Collection: it.CollectionName, CollectionSlug: it.CollectionSlug,
				Seq: it.Seq, CreatedAt: it.CreatedAt, UpdatedAt: it.UpdatedAt,
				Path: path,
			},
			Why: rt.Why, Because: rt.Because, WaitingSince: rt.WaitingSince,
			// Kind and rule only: the target's ids (a role, other people)
			// never leave the server.
			WaitingOn: needs.WaitingOnView{Kind: rt.Target.Kind, Rule: rt.Target.Rule},
			ItemID:    it.ID, WorkspaceID: ws.ID, Ask: asked[it.ID],
			CollectionID:     it.CollectionID,
			AssignedToViewer: it.AssignedUserID != nil && *it.AssignedUserID == user.ID,
			CanEdit:          edit.allows(it.ID, it.CollectionID),
		}
		var fields map[string]any
		_ = json.Unmarshal([]byte(it.Fields), &fields)
		row.Item.Status, _ = fields["status"].(string)
		row.Item.Priority, _ = fields["priority"].(string)
		if sn := snoozes[it.ID]; sn != nil && sn.Holds {
			row.Held, row.Snooze = true, sn
		}
		if nm := notMine[it.ID]; nm != nil && !row.Held {
			c, ci := cands[it.ID], infos[it.CollectionID]
			facts := needs.NotMineFacts{
				ViewerID: user.ID, Mentions: c.Mentions,
				CollectionID: it.CollectionID, AwaitsHuman: c.AwaitsHuman,
				AssigneeUserID: c.AssigneeUserID, AssignedAt: c.AssignedAt,
			}
			for _, tr := range nm.Transitions {
				if ci.waitKey != "" && tr.FieldKey == ci.waitKey && ci.waiting[strings.ToLower(strings.TrimSpace(tr.ToStatus))] {
					facts.WaitingTransition = true
					break
				}
			}
			if d, ok := due[it.ID]; ok {
				latest := d.Latest
				facts.DueLatest = &latest
			}
			if needs.NotMineHolds(nm.NotMine.Marker, facts) {
				row.Held, row.NotMine = true, nm.NotMine
			}
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
		deg, serr := sc.Score(sctx, needs.Viewer{UserID: user.ID, AuthIsBearer: isBearerAuth(r), Now: asOf, Loc: viewerZone(r).Loc}, nscope, rows)
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
// viewer sees it now, one batch per workspace. The not-mine fields are the
// listing's as-of time, the item's collection, and the viewer's own id when
// the item is assigned to them (never anyone else's).
func (s *Server) needsAttachMarkers(userID string, asOf time.Time, rows []*needs.Row) error {
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
			m.SeenAt = asOf.UTC().Truncate(time.Second).Format(time.RFC3339)
			m.SeenCollectionID = row.CollectionID
			if row.AssignedToViewer {
				m.SeenAssigneeID = userID
			}
			row.Marker = &m
		}
	}
	return nil
}

// needsEditRight is the viewer's edit right across one workspace's rows,
// read once per workspace (never per item). It is canEditInCollection's
// decision: an editor or owner role inside the collections it reaches, else
// the item grant, else the collection grant.
type needsEditRight struct {
	all      bool
	reach    map[string]bool
	itemPerm map[string]string
	collPerm map[string]string
}

func (e *needsEditRight) allows(itemID, collectionID string) bool {
	if e.all || e.reach[collectionID] {
		return true
	}
	if p, ok := e.itemPerm[itemID]; ok {
		return permissionLevel(p) >= permissionLevel("edit")
	}
	return permissionLevel(e.collPerm[collectionID]) >= permissionLevel("edit")
}

// needsEditRights loads the viewer's edit right in one workspace from the
// membership the router already read, plus the viewer's own grants and
// collection access when the role does not cover the whole workspace.
func (s *Server) needsEditRights(r *http.Request, user *models.User, ws *models.Workspace, member *models.WorkspaceMember) (*needsEditRight, error) {
	e := &needsEditRight{reach: map[string]bool{}, itemPerm: map[string]string{}, collPerm: map[string]string{}}
	switch {
	case user.Role == "admin" && !isBearerAuth(r), ws.OwnerID != "" && ws.OwnerID == user.ID:
		e.all = true
		return e, nil
	case member != nil && member.Role == "owner":
		e.all = true
		return e, nil
	case member != nil && member.Role == "editor":
		if member.CollectionAccess == "all" || member.CollectionAccess == "" {
			e.all = true
			return e, nil
		}
		listed, err := s.store.GetMemberCollectionAccess(ws.ID, user.ID)
		if err != nil {
			return nil, err
		}
		for _, id := range listed {
			e.reach[id] = true
		}
	}
	colls, items, err := s.store.ListUserGrants(ws.ID, user.ID)
	if err != nil {
		return nil, err
	}
	for _, g := range colls {
		e.collPerm[g.CollectionID] = g.Permission
	}
	for _, g := range items {
		e.itemPerm[g.ItemID] = g.Permission
	}
	return e, nil
}
