package server

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/usertz"
)

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
	// A pinned token never names a group. The refusal is decided here, before
	// any group is looked up or the parameter is read, so the answer is the same
	// for its own group, an unknown id and the built-ins, and says nothing about
	// which ids exist. Without the parameter it keeps the one workspace it has.
	if pin := tokenPinFrom(r.Context()); pin != nil && r.URL.Query().Has("group") {
		writeTokenWorkspaceDeniedFor(w, pin)
		return
	}
	from, to, ok := parseAgendaWindow(w, r)
	if !ok {
		return
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
	run := s.newAgendaRead(from, to, zone, note)
	admitted, err := s.needsAdmittedWorkspaces(r, user)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	admitted, ok = s.agendaGroupWorkspaces(w, r, user, admitted)
	if !ok {
		return
	}
	sort.Slice(admitted, func(i, j int) bool {
		if admitted[i].Slug != admitted[j].Slug {
			return admitted[i].Slug < admitted[j].Slug
		}
		return admitted[i].ID < admitted[j].ID
	})
	days, _ := store.AgendaCountDays(from, to, zone.Loc)
	counts := make([]agendaCount, len(days))
	for i, d := range days {
		counts[i].Day = d
	}
	countRows, countsCut := 0, false
	for _, ws := range admitted {
		elig := &needsEligibility{s: s, wsID: ws.ID, cache: map[string]*needsVisibility{}}
		vis := elig.visibility(user.ID)
		if elig.err != nil {
			writeInternalError(w, elig.err)
			return
		}
		if vis == nil {
			continue
		}
		scope := store.PendingReminderScope{}
		if !vis.unrestricted {
			scope = store.PendingReminderScope{CollectionIDs: vis.collectionIDs, ItemIDs: vis.itemIDs}
			if scope.CollectionIDs == nil {
				scope.CollectionIDs = []string{}
			}
			if len(scope.CollectionIDs) == 0 && len(scope.ItemIDs) == 0 {
				continue
			}
		}
		if s.agendaExpired(run) {
			countsCut = true
			break
		}
		if !countsCut {
			budget := agendaMaxCountRows
			if s.agendaCountsLimit > 0 {
				budget = min(budget, s.agendaCountsLimit)
			}
			series, rows, cut, err := s.store.AgendaCounts(ws.ID, scope, from, to, zone.Loc, budget-countRows)
			if err != nil {
				writeInternalError(w, err)
				return
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
		}
		entryStart, unavailableStart, unexpandedStart := len(run.response.Entries), len(run.response.Unavailable), len(run.response.Unexpanded)
		if err := s.appendWorkspaceAgenda(run, ws.ID, scope); err != nil {
			writeInternalError(w, err)
			return
		}
		if err := s.appendAgendaItemEvents(run, ws.ID, scope); err != nil {
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
		for i := entryStart; i < len(run.response.Entries); i++ {
			e := &run.response.Entries[i]
			e.Workspace, e.WorkspaceName, e.OwnerUsername = ws.Slug, ws.Name, owner
		}
		for i := unavailableStart; i < len(run.response.Unavailable); i++ {
			run.response.Unavailable[i].Workspace, run.response.Unavailable[i].OwnerUsername = ws.Slug, owner
		}
		for i := unexpandedStart; i < len(run.response.Unexpanded); i++ {
			run.response.Unexpanded[i].Workspace, run.response.Unexpanded[i].OwnerUsername = ws.Slug, owner
		}
	}
	if countsCut {
		// A partial series would read as a total: send none, and say why.
		run.response.CountsTruncated, run.response.Truncated, run.setCut = true, true, true
	} else {
		run.response.Counts = counts
	}
	run.finish()
	writeJSON(w, http.StatusOK, run.response)
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
		return true
	}
	return false
}

func (s *Server) appendAgendaItemEvents(run *agendaRead, workspaceID string, scope store.PendingReminderScope) error {
	for _, source := range []struct {
		kind string
		read func(string, store.PendingReminderScope, time.Time, time.Time, int) ([]store.AgendaItemEvent, error)
	}{
		{"created", s.store.ListCreatedForAgenda},
		{"completed", s.store.ListCompletedForAgenda},
	} {
		if s.agendaExpired(run) {
			return nil
		}
		events, err := source.read(workspaceID, scope, run.from, run.to, agendaMaxEntries+1)
		if err != nil {
			return err
		}
		if len(events) > agendaMaxEntries {
			run.response.Truncated, run.setCut = true, true
		}
		for _, event := range events {
			run.response.Entries = append(run.response.Entries, agendaEntry{
				Type: source.kind, At: event.At.In(run.loc).Format(time.RFC3339), Day: event.At.In(run.loc).Format("2006-01-02"),
				ItemRef: event.ItemRef, ItemTitle: event.ItemTitle, Collection: event.Collection, instant: event.At, subKey: event.ItemID,
			})
		}
	}
	return nil
}
