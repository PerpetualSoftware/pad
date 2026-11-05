package server

import (
	"net/http"
	"sort"
	"strings"
	"time"

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
	sort.Slice(admitted, func(i, j int) bool {
		if admitted[i].Slug != admitted[j].Slug {
			return admitted[i].Slug < admitted[j].Slug
		}
		return admitted[i].ID < admitted[j].ID
	})
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
			break
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
	run.finish()
	writeJSON(w, http.StatusOK, run.response)
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
