package server

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/PerpetualSoftware/pad/internal/ics"
	"github.com/PerpetualSoftware/pad/internal/logging"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/usertz"
	"github.com/go-chi/chi/v5"
)

const calendarFeedMaxEvents = 2000
const calendarFeedBudget = 5 * time.Second

func calendarFeedCaller(w http.ResponseWriter, r *http.Request) *models.User {
	if appContextFrom(r) != nil {
		writeCalendarFeedNotFound(w)
		return nil
	}
	if pin := tokenPinFrom(r.Context()); pin != nil {
		writeTokenWorkspaceDeniedFor(w, pin)
		return nil
	}
	u := currentUser(r)
	if u == nil {
		writeError(w, 401, "unauthorized", "Authentication required")
		return nil
	}
	if u.Kind != models.UserKindHuman {
		writeCalendarFeedNotFound(w)
		return nil
	}
	return u
}
func writeCalendarFeedNotFound(w http.ResponseWriter) { writeError(w, 404, "not_found", "Not found") }
func (s *Server) handleCreateCalendarFeed(w http.ResponseWriter, r *http.Request) {
	u := calendarFeedCaller(w, r)
	if u == nil {
		return
	}
	input, err := strictScheduleBody[struct {
		Layer        string   `json:"layer"`
		WorkspaceIDs []string `json:"workspace_ids"`
		Name         string   `json:"name"`
	}](r)
	if err != nil {
		writeScheduleBodyError(w)
		return
	}
	if input.Layer != "mine" && input.Layer != "agent" {
		writeWorkspaceGroupValidation(w, "layer", "enum", "layer must be mine or agent")
		return
	}
	if utf8.RuneCountInString(input.Name) > 100 {
		writeWorkspaceGroupValidation(w, "name", "range", "name must contain at most 100 characters")
		return
	}
	if _, ok := s.validateWorkspaceGroupMembers(w, r, u, &input.WorkspaceIDs); !ok {
		return
	}
	// Canonical UUID spellings can coincide after validation.
	ids := make([]string, 0, len(input.WorkspaceIDs))
	seen := map[string]bool{}
	for _, id := range input.WorkspaceIDs {
		if !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	f, token, err := s.store.CreateCalendarFeed(r.Context(), u.ID, input.Layer, ids, input.Name)
	if errors.Is(err, store.ErrUserDisabled) {
		writeCalendarFeedNotFound(w)
		return
	}
	if errors.Is(err, store.ErrCalendarFeedLimit) {
		writeWorkspaceGroupValidation(w, "feeds", "range", "At most 10 calendar feeds are allowed")
		return
	}
	if err != nil {
		writeInternalError(w, err)
		return
	}
	out := struct {
		Feed  *store.CalendarFeed `json:"feed"`
		Token string              `json:"token"`
		URL   string              `json:"url,omitempty"`
	}{Feed: f, Token: token}
	if s.hasUsableBaseURL() {
		out.URL = s.baseURL + "/api/v1/feeds/ics/" + token
	}
	writeJSON(w, 201, out)
}
func (s *Server) handleListCalendarFeeds(w http.ResponseWriter, r *http.Request) {
	u := calendarFeedCaller(w, r)
	if u == nil {
		return
	}
	feeds, err := s.store.ListCalendarFeeds(r.Context(), u.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, 200, struct {
		Feeds []store.CalendarFeed `json:"feeds"`
	}{feeds})
}
func (s *Server) handleDeleteCalendarFeed(w http.ResponseWriter, r *http.Request) {
	u := calendarFeedCaller(w, r)
	if u == nil {
		return
	}
	err := s.store.RevokeCalendarFeed(r.Context(), u.ID, chi.URLParam(r, "id"))
	if errors.Is(err, sql.ErrNoRows) {
		writeCalendarFeedNotFound(w)
		return
	}
	if err != nil {
		writeInternalError(w, err)
		return
	}
	w.WriteHeader(204)
}

// calendarFeedRecoverer never logs a panic value or stack: either can contain
// the bearer URL. It also encloses the feed's public rate limiter.
func calendarFeedRecoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recover() != nil {
				slog.Error("feed route panic", "path", logging.RedactPath(r.URL.Path))
				writeError(w, 500, "internal_error", "Internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
func (s *Server) handleICSFeed(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), calendarFeedBudget)
	defer cancel()
	r = r.WithContext(ctx)
	if s.calendarFeedHook != nil {
		s.calendarFeedHook()
	}
	f, u, err := s.store.LookupCalendarFeed(ctx, chi.URLParam(r, "token"))
	if err != nil {
		s.calendarFeedError(w, "")
		return
	}
	if f == nil {
		writeCalendarFeedNotFound(w)
		return
	}
	r = r.WithContext(WithCurrentUser(ctx, u))
	calendar, complete, err := s.readCalendarFeed(r, f, u)
	if err != nil {
		s.calendarFeedError(w, f.ID)
		return
	}
	var body bytes.Buffer
	if err := ics.Write(&body, calendar); err != nil {
		s.calendarFeedError(w, f.ID)
		return
	}
	// Use wall time for polling metadata, independently of the agenda test clock.
	now := time.Now().UTC()
	recent := false
	if f.LastUsedAt != nil {
		last, err := time.Parse(time.RFC3339Nano, *f.LastUsedAt)
		recent = err == nil && last.After(now.Add(-time.Hour))
	}
	if !recent {
		if err := s.store.TouchCalendarFeed(ctx, f.ID, now); err != nil {
			slog.Warn("calendar feed last use update failed", "feed_id", f.ID)
		}
	}
	w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	w.Header().Set("Cache-Control", "private")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	flag := "true"
	if !complete || ctx.Err() != nil {
		flag = "false"
	}
	w.Header().Set("X-Pad-Feed-Complete", flag)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body.Bytes())
}
func (s *Server) calendarFeedError(w http.ResponseWriter, id string) {
	// SQL/driver errors can repeat bound values. Feed errors log no error text.
	slog.Error("calendar feed read failed", "feed_id", id)
	writeError(w, 500, "internal_error", "Internal server error")
}
func (s *Server) readCalendarFeed(r *http.Request, f *store.CalendarFeed, u *models.User) (ics.Calendar, bool, error) {
	now := s.agendaNow().UTC()
	deadline := now.Add(calendarFeedBudget)
	expired := func() bool { return r.Context().Err() != nil || s.agendaNow().After(deadline) }
	c := ics.Calendar{Name: "Pad (" + u.Username + ")", Agent: f.Layer == "agent", Now: now}
	if f.Name != "" {
		c.Name += ": " + f.Name
	}
	if c.Agent {
		c.Name += " Agent calendar"
	}
	admitted, err := s.needsAdmittedWorkspaces(r, u)
	if err != nil {
		return c, false, err
	}
	wanted := map[string]bool{}
	for _, id := range f.WorkspaceIDs {
		wanted[id] = true
	}
	sort.Slice(admitted, func(i, j int) bool {
		if admitted[i].Slug != admitted[j].Slug {
			return admitted[i].Slug < admitted[j].Slug
		}
		return admitted[i].ID < admitted[j].ID
	})
	// Build the same live-human visibility scopes as /me/agenda; an empty
	// restricted scope must never become the unrestricted zero-value scope.
	type workspaceScope struct {
		ws    models.Workspace
		scope store.PendingReminderScope
		owner string
	}
	workspaces := []workspaceScope{}
	for _, ws := range admitted {
		if expired() {
			return c, false, nil
		}
		if len(wanted) > 0 && !wanted[ws.ID] {
			continue
		}
		elig := &needsEligibility{s: s, wsID: ws.ID, cache: map[string]*needsVisibility{}}
		vis := elig.visibility(u.ID)
		if elig.err != nil {
			return c, false, elig.err
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
		owner := ws.OwnerUsername
		if owner == "" && ws.OwnerID != "" {
			user, err := s.store.GetUser(ws.OwnerID)
			if err != nil {
				return c, false, err
			}
			if user != nil {
				owner = user.Username
			}
		}
		workspaces = append(workspaces, workspaceScope{ws, scope, owner})
	}
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	from, to := day.AddDate(0, 0, -90), day.AddDate(0, 0, 365)
	complete := true
	count := 0
	seen := map[string]bool{}

	for start := from; start.Before(to); {
		if expired() {
			return c, false, nil
		}
		end := start.Add(agendaMaxWindow)
		if end.After(to) {
			end = to
		}
		run := s.newAgendaRead(start, end, usertz.Zone{Name: "UTC", Loc: time.UTC}, "")
		run.recipient = u.ID
		run.audience = "human"
		if c.Agent {
			run.audience = "agent"
			run.skipDated = true
		}
		if deadline.Before(run.deadline) {
			run.deadline = deadline
		}
		for _, part := range workspaces {
			if expired() || s.agendaExpired(run) {
				complete = false
				break
			}
			entryStart := len(run.response.Entries)
			if err := s.appendWorkspaceAgenda(run, part.ws.ID, part.scope); err != nil {
				return c, false, err
			}
			for i := entryStart; i < len(run.response.Entries); i++ {
				e := &run.response.Entries[i]
				e.Workspace, e.OwnerUsername = part.ws.Slug, part.owner
			}
		}
		run.finish()
		complete = complete && len(run.response.Unavailable) == 0 && len(run.response.Unexpanded) == 0
		// A truncated chunk resumes at next_from, which promises every instant
		// before it was examined. Without one, the feed ends here rather than
		// skip to the next chunk and leave a hole mid-window.
		next := end
		if run.response.Truncated {
			nf, err := time.Parse(time.RFC3339, run.response.NextFrom)
			if err == nil && nf.After(start) && !nf.After(end) {
				next = nf
			} else {
				complete = false
				next = to
			}
		}
		for _, e := range run.response.Entries {
			if expired() {
				return c, false, nil
			}
			if e.Type == "date" && c.Agent {
				continue
			}
			if e.Type == "schedule" {
				if c.Agent && e.Audience != "agent" || !c.Agent && e.Audience != "human" && e.Audience != "both" {
					continue
				}
			} else if e.Type != "date" {
				continue
			}
			event := ics.Event{Summary: e.ItemRef + " " + e.ItemTitle}
			if e.Type == "date" {
				// The agenda admits any value whose first ten characters are a
				// date; only a full RFC 3339 timestamp is written as timed.
				event.UID = e.itemID + "." + e.Field + "." + e.Day
				event.Target = e.Day
				if _, err := time.Parse(time.RFC3339, e.Date); err == nil {
					event.Target = e.Date
				}
			} else {
				event.UID = e.ScheduleID + "." + e.OccurrenceKey
				if e.Roles == nil || e.Roles.Target == nil {
					complete = false
					continue
				}
				event.Target = *e.Roles.Target
				if e.Roles.OccupiedUntil != nil {
					event.End = *e.Roles.OccupiedUntil
				}
				if e.Roles.Deadline != nil {
					event.Deadline = *e.Roles.Deadline
				}
			}
			if seen[event.UID] {
				continue
			}
			seen[event.UID] = true
			if s.hasUsableBaseURL() && e.OwnerUsername != "" {
				event.URL = s.baseURL + "/" + url.PathEscape(e.OwnerUsername) + "/" + url.PathEscape(e.Workspace) + "/" + url.PathEscape(e.Collection) + "/" + url.PathEscape(e.ItemRef)
			}
			if count+event.Count() > calendarFeedMaxEvents {
				return c, false, nil
			}
			c.Events = append(c.Events, event)
			count += event.Count()
			if count >= calendarFeedMaxEvents {
				return c, false, nil
			}
		}
		start = next
	}
	return c, complete && !expired(), nil
}
