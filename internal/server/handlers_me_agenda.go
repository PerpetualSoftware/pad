package server

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/usertz"
	"github.com/google/uuid"
)

// agendaCountsResponse deliberately has no empty list fields: this is only a series.
type agendaCountsResponse struct {
	From            string        `json:"from"`
	To              string        `json:"to"`
	Tz              string        `json:"tz"`
	TimezoneNote    string        `json:"timezone_note,omitempty"`
	Counts          []agendaCount `json:"counts,omitempty"`
	CountsTruncated bool          `json:"counts_truncated,omitempty"`
}

// handleMeAgenda serves the admitted, visible workspaces as one bounded agenda.
func (s *Server) handleMeAgenda(w http.ResponseWriter, r *http.Request) {
	if appContextFrom(r) != nil {
		writeError(w, http.StatusNotFound, "not_found", "Not found")
		return
	}
	user := currentUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Sign in to see your agenda")
		return
	}
	// Selector presence is refused before parsing or lookup, independently of
	// the requested ID. Without selectors, the token retains its pinned scope.
	if pin := tokenPinFrom(r.Context()); pin != nil && (r.URL.Query().Has("group") || r.URL.Query().Has("workspace")) {
		writeTokenWorkspaceDeniedFor(w, pin)
		return
	}
	from, to, ok := parseAgendaWindow(w, r)
	if !ok {
		return
	}
	include := map[string]bool{"completed": true}
	if r.URL.Query().Has("include") {
		include = map[string]bool{}
		if raw := r.URL.Query().Get("include"); raw != "" {
			for _, value := range strings.Split(raw, ",") {
				kind := strings.TrimSpace(value)
				if kind != "created" && kind != "completed" {
					writeAgendaParam(w, "include", "format", "`include` must be a comma-separated list of created and completed")
					return
				}
				include[kind] = true
			}
		}
	}
	countsMode := "include"
	if query := r.URL.Query(); query.Has("counts") {
		countsMode = strings.TrimSpace(query.Get("counts"))
		if len(query["counts"]) != 1 || (countsMode != "include" && countsMode != "omit" && countsMode != "only") {
			writeAgendaParam(w, "counts", "format", "`counts` must be include, omit or only")
			return
		}
	}
	zone := usertz.Resolve(user.Timezone)
	note := usertz.Note(zone)
	if r.URL.Query().Has("tz") {
		tz := strings.TrimSpace(r.URL.Query().Get("tz"))
		loc, err := usertz.Validate(tz)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_tz", "`tz` must be an IANA time zone name, such as America/Los_Angeles")
			return
		}
		zone, note = usertz.Zone{Name: tz, Loc: loc, Set: true}, ""
	}
	workspaceID := ""
	if query := r.URL.Query(); query.Has("workspace") {
		if query.Has("group") {
			writeAgendaParam(w, "workspace", "conflict", "`workspace` cannot be combined with `group`")
			return
		}
		if len(query["workspace"]) != 1 {
			writeAgendaParam(w, "workspace", "format", "`workspace` must be a single workspace UUID")
			return
		}
		raw := strings.TrimSpace(query.Get("workspace"))
		parsed, err := uuid.Parse(raw)
		if len(raw) != 36 || err != nil {
			writeAgendaParam(w, "workspace", "format", "`workspace` must be a workspace UUID")
			return
		}
		workspaceID = parsed.String()
	}
	unavailable := func() {
		writeError(w, http.StatusNotFound, "not_found", "Workspace not available")
	}
	run := s.newAgendaRead(from, to, zone, note)
	run.response.Forecasts, run.response.ForecastsComplete = []agendaForecast{}, true
	run.recipient, run.audience = user.ID, "human"
	if actor, _ := actorFromRequest(r); actor == "agent" {
		run.audience = "agent"
	}
	admitted, err := s.needsAdmittedWorkspaces(r, user)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if workspaceID != "" {
		var selected []models.Workspace
		for _, ws := range admitted {
			if ws.ID == workspaceID {
				selected = []models.Workspace{ws}
				break
			}
		}
		if len(selected) == 0 {
			unavailable()
			return
		}
		admitted = selected
	} else {
		admitted, ok = s.agendaGroupWorkspaces(w, r, user, admitted)
		if !ok {
			return
		}
	}
	sort.Slice(admitted, func(i, j int) bool {
		if admitted[i].Slug != admitted[j].Slug {
			return admitted[i].Slug < admitted[j].Slug
		}
		return admitted[i].ID < admitted[j].ID
	})
	var counts []agendaCount
	if countsMode != "omit" {
		days, _ := store.AgendaCountDays(from, to, zone.Loc)
		counts = make([]agendaCount, len(days))
		for i, d := range days {
			counts[i].Day = d
		}
	}
	countRows, countsCut := 0, false
	appendCounts := func(wsID string, scope store.PendingReminderScope) error {
		if countsMode == "omit" || countsCut {
			return nil
		}
		budget := agendaMaxCountRows
		if s.agendaCountsLimit > 0 {
			budget = min(budget, s.agendaCountsLimit)
		}
		series, rows, cut, err := s.store.AgendaCounts(wsID, scope, from, to, zone.Loc, budget-countRows)
		if err != nil {
			return err
		}
		countRows += rows
		if cut {
			countsCut = true
		} else {
			for i := range counts {
				counts[i].Completed += series[i].Completed
				counts[i].Open += series[i].Open
				counts[i].Burndown += series[i].Burndown
			}
		}
		return nil
	}
	if countsMode == "only" {
		for _, ws := range admitted {
			scope, visible, err := s.meAgendaScope(ws.ID, user.ID)
			if err != nil {
				writeInternalError(w, err)
				return
			}
			if !visible {
				if workspaceID != "" {
					unavailable()
					return
				}
				continue
			}
			if s.agendaExpired(run) {
				countsCut = true
				break
			}
			if err := appendCounts(ws.ID, scope); err != nil {
				writeInternalError(w, err)
				return
			}
		}
		if countsCut {
			counts = nil
		}
		writeJSON(w, http.StatusOK, agendaCountsResponse{
			From: run.response.From, To: run.response.To, Tz: run.response.Tz, TimezoneNote: run.response.TimezoneNote,
			Counts: counts, CountsTruncated: countsCut,
		})
		return
	}
	// Save the admitted scope and owner once for the later group pass.
	type groupWorkspace struct {
		workspace models.Workspace
		scope     store.PendingReminderScope
		owner     string
	}
	var groupWorkspaces []groupWorkspace
	for _, ws := range admitted {
		scope, visible, err := s.meAgendaScope(ws.ID, user.ID)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		if !visible {
			if workspaceID != "" {
				unavailable()
				return
			}
			continue
		}
		if s.agendaExpired(run) {
			run.response.ForecastsComplete = false
			countsCut = true
			break
		}
		if err := appendCounts(ws.ID, scope); err != nil {
			writeInternalError(w, err)
			return
		}
		entryStart, unavailableStart, unexpandedStart := len(run.response.Entries), len(run.response.Unavailable), len(run.response.Unexpanded)
		if err := s.appendWorkspaceAgenda(run, ws.ID, scope); err != nil {
			writeInternalError(w, err)
			return
		}
		if err := s.appendAgendaItemEvents(run, ws.ID, scope, include); err != nil {
			writeInternalError(w, err)
			return
		}
		owner := ws.OwnerUsername
		if owner == "" && ws.OwnerID != "" {
			// A store error is not a missing owner; a vanished owner leaves it empty.
			u, err := s.store.GetUser(ws.OwnerID)
			if err != nil {
				writeInternalError(w, err)
				return
			}
			if u != nil {
				owner = u.Username
			}
		}
		ws.OwnerUsername = owner
		if err := s.appendWorkspaceForecasts(run, ws.ID, scope, &ws); err != nil {
			writeInternalError(w, err)
			return
		}
		for i := entryStart; i < len(run.response.Entries); i++ {
			e := &run.response.Entries[i]
			e.Workspace, e.WorkspaceName, e.OwnerUsername = ws.Slug, ws.Name, owner
		}
		groupWorkspaces = append(groupWorkspaces, groupWorkspace{ws, scope, owner})
		for i := unavailableStart; i < len(run.response.Unavailable); i++ {
			run.response.Unavailable[i].Workspace, run.response.Unavailable[i].OwnerUsername = ws.Slug, owner
		}
		for i := unexpandedStart; i < len(run.response.Unexpanded); i++ {
			run.response.Unexpanded[i].Workspace, run.response.Unexpanded[i].OwnerUsername = ws.Slug, owner
		}
	}
	// Every workspace's window, counts and events precede actionable work.
	// Group budget cuts affect only the group completeness flags.
	for _, group := range groupWorkspaces {
		ws := group.workspace
		priorityStart, waitingStart, noDateStart := len(run.response.PriorityList), len(run.response.WaitingOn), len(run.response.NoDate)
		s.appendAgendaWork(run, ws.ID, group.scope)
		for _, rows := range [][]agendaEntry{run.response.PriorityList[priorityStart:], run.response.WaitingOn[waitingStart:], run.response.NoDate[noDateStart:]} {
			for i := range rows {
				rows[i].Workspace, rows[i].WorkspaceName, rows[i].OwnerUsername = ws.Slug, ws.Name, group.owner
			}
		}
	}
	if countsMode != "omit" {
		if countsCut {
			// A partial series would read as a total: send none, and say why.
			run.response.CountsTruncated = true
		} else {
			run.response.Counts = counts
		}
	}
	run.finish()
	writeJSON(w, http.StatusOK, run.response)
}

// meAgendaScope applies the same visibility and empty-scope rules to list and counts reads.
func (s *Server) meAgendaScope(workspaceID, userID string) (store.PendingReminderScope, bool, error) {
	elig := &needsEligibility{s: s, wsID: workspaceID, cache: map[string]*needsVisibility{}}
	vis := elig.visibility(userID)
	scope := store.PendingReminderScope{}
	if elig.err != nil {
		return scope, false, elig.err
	}
	if vis == nil {
		return scope, false, nil
	}
	if !vis.unrestricted {
		scope = store.PendingReminderScope{CollectionIDs: vis.collectionIDs, ItemIDs: vis.itemIDs}
		if scope.CollectionIDs == nil {
			scope.CollectionIDs = []string{}
		}
		if len(scope.CollectionIDs) == 0 && len(scope.ItemIDs) == 0 {
			return scope, false, nil
		}
	}
	return scope, true, nil
}

// agendaGroupWorkspaces narrows the admitted workspaces to the requested
// group: all (the default), mine, or a saved group of the caller's own. Another
// user's group answers 404 as it does on the group routes. Members the caller
// cannot see are dropped by the same filter the group routes use, so a group
// reaches no workspace the caller could not already read.
func (s *Server) agendaGroupWorkspaces(w http.ResponseWriter, r *http.Request, user *models.User, admitted []models.Workspace) ([]models.Workspace, bool) {
	if !r.URL.Query().Has("group") {
		return admitted, true
	}
	id := strings.TrimSpace(r.URL.Query().Get("group"))
	switch {
	case id == "":
		writeAgendaParam(w, "group", "format", "`group` must be all, mine or a saved group id")
		return nil, false
	case id == "all":
		return admitted, true
	case id == "mine":
		kept := make([]models.Workspace, 0, len(admitted))
		for _, ws := range admitted {
			if ws.OwnerID == user.ID {
				kept = append(kept, ws)
			}
		}
		return kept, true
	}
	g, err := s.store.GetWorkspaceGroup(r.Context(), user.ID, id)
	if err != nil {
		writeWorkspaceGroupError(w, err)
		return nil, false
	}
	members := map[string]bool{}
	for _, wsID := range visibleWorkspaceGroup(*g, admitted).WorkspaceIDs {
		members[wsID] = true
	}
	kept := make([]models.Workspace, 0, len(admitted))
	for _, ws := range admitted {
		if members[ws.ID] {
			kept = append(kept, ws)
		}
	}
	return kept, true
}

func (s *Server) agendaExpired(run *agendaRead) bool {
	if s.agendaNow().After(run.deadline) {
		run.response.Truncated, run.setCut = true, true
		run.response.PriorityListComplete, run.response.WaitingOnComplete, run.response.NoDateComplete = false, false, false
		return true
	}
	return false
}

func (s *Server) appendAgendaItemEvents(run *agendaRead, workspaceID string, scope store.PendingReminderScope, include map[string]bool) error {
	for _, source := range []struct {
		kind string
		read func(string, store.PendingReminderScope, time.Time, time.Time, int) ([]store.AgendaItemEvent, error)
	}{
		{"created", s.store.ListCreatedForAgenda},
		{"completed", s.store.ListCompletedForAgenda},
	} {
		if !include[source.kind] {
			continue
		}
		if s.agendaExpired(run) {
			return nil
		}
		events, err := source.read(workspaceID, scope, run.from, run.to, agendaMaxEntries+1)
		if err != nil {
			return err
		}
		if len(events) > agendaMaxEntries {
			run.response.EventsTruncated = true
		}
		for _, event := range events {
			run.response.Entries = append(run.response.Entries, agendaEntry{
				Type: source.kind, At: event.At.In(run.loc).Format(time.RFC3339), Day: event.At.In(run.loc).Format("2006-01-02"),
				ItemRef: event.ItemRef, ItemID: event.ItemID, ItemTitle: event.ItemTitle, Collection: event.Collection, instant: event.At, subKey: event.ItemID,
			})
		}
	}
	return nil
}
