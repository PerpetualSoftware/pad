package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// agendaPriority maps only declared priority values with shared meaning.
func agendaPriority(c store.AgendaWorkCandidate) string {
	var schema models.CollectionSchema
	var fields map[string]any
	if models.UnmarshalItemFieldSchema([]byte(c.Schema), &schema) != nil || json.Unmarshal([]byte(c.Fields), &fields) != nil {
		return "unranked"
	}
	value, _ := fields["priority"].(string)
	for _, f := range schema.Fields {
		if f.Key != "priority" || f.Type != "select" {
			continue
		}
		declared := false
		for _, v := range f.Options {
			if v == value {
				declared = true
				break
			}
		}
		if !declared {
			return "unranked"
		}
		switch strings.ToLower(value) {
		case "critical", "high":
			return "high"
		case "normal", "medium":
			return "normal"
		case "low":
			return "low"
		}
	}
	return "unranked"
}

func (s *Server) appendAgendaWork(run *agendaRead, workspaceID string, scope store.PendingReminderScope) {
	resp := &run.response
	priorityStart, waitingStart := len(resp.PriorityList), len(resp.WaitingOn)
	cut := func() { resp.PriorityListComplete, resp.WaitingOnComplete, resp.NoDateComplete = false, false, false }
	if s.agendaNow().After(run.deadline) {
		cut()
		return
	}
	candidates, more, err := s.store.ListAgendaWork(workspaceID, scope, run.recipient, run.audience, run.workRemaining, run.listNoDate)
	if err != nil {
		slog.Warn("agenda: work candidates unavailable", "workspace_id", workspaceID, "error_class", fmt.Sprintf("%T", err))
		cut()
		return
	}
	run.workRemaining -= len(candidates)
	if more {
		cut()
	}
	ids := make([]string, len(candidates))
	for i, c := range candidates {
		ids[i] = c.ItemID
	}
	schedules, gated, err := s.store.AgendaWorkDetails(workspaceID, ids)
	if err != nil {
		slog.Warn("agenda: work details unavailable", "workspace_id", workspaceID, "error_class", fmt.Sprintf("%T", err))
		cut()
		return
	}
	for _, c := range candidates {
		if s.agendaNow().After(run.deadline) {
			cut()
			break
		}
		sc := schedules[c.ItemID]
		gateState := models.GateStateReleased
		if gated[c.ItemID] {
			gates, err := s.store.ScheduleGates(workspaceID, c.ItemID, models.CommitmentAccess{Readable: map[string]bool{c.ItemID: true}})
			if err != nil {
				slog.Warn("agenda: work gates unavailable", "workspace_id", workspaceID, "item_id", c.ItemID, "error_class", fmt.Sprintf("%T", err))
				resp.WaitingOnComplete = false
				continue
			}
			gateState = gates.State
		}
		e := agendaEntry{Type: "item", ItemRef: c.ItemRef, ItemTitle: c.ItemTitle, Collection: c.Collection, subKey: c.ItemID}
		if gateState != models.GateStateReleased {
			if sc != nil {
				current, err := s.store.AgendaOutstanding(sc, run.loc, run.now)
				if err != nil {
					resp.WaitingOnComplete = false
				} else if current != nil {
					e.Roles, e.OccurrenceKey, e.ScheduleID = &current.Roles, current.OccurrenceKey, sc.ID
					e.State, e.Fired = current.State, current.Fired
					e.AcknowledgedAt, e.AcknowledgedBy = current.AcknowledgedAt, current.AcknowledgedBy
				}
			}
			e.Reason = gateState
			resp.WaitingOn = append(resp.WaitingOn, e)
			continue
		}
		if sc == nil {
			if !run.listNoDate {
				continue
			}
			e.Reason = "unplaced"
			resp.NoDate = append(resp.NoDate, e)
			continue
		}
		if sc.EndedAt != nil {
			// An open item whose schedule ended (TASK-4537): Outstanding until it
			// is closed or an editor dismisses the ending. A dismissed one, or
			// an item with no writable terminal, is ordinary undated work.
			if sc.EndedDismissedAt == nil && c.CanCloseItem {
				e.Reason, e.ScheduleID, e.EndedAt, e.Rank = "schedule_ended", sc.ID, *sc.EndedAt, agendaPriority(c)
				resp.PriorityList = append(resp.PriorityList, e)
			} else if run.listNoDate {
				e.Reason, e.ScheduleID = "unplaced", sc.ID
				resp.NoDate = append(resp.NoDate, e)
			}
			continue
		}
		if sc.ErrorCode != nil {
			resp.PriorityListComplete = false
			continue
		}
		current, err := s.store.AgendaOutstanding(sc, run.loc, run.now)
		if err != nil {
			resp.PriorityListComplete = false
			continue
		}
		if current == nil {
			if run.listNoDate {
				e.Reason, e.ScheduleID = "unplaced", sc.ID
				resp.NoDate = append(resp.NoDate, e)
			} else {
				resp.PriorityListComplete = false
			}
			continue
		}
		e.Type, e.At, e.Day, e.State, e.ScheduleID = "schedule", current.At.In(run.loc).Format(time.RFC3339), current.At.In(run.loc).Format("2006-01-02"), current.State, sc.ID
		e.Roles, e.OccurrenceKey, e.RolesOrigin = &current.Roles, current.OccurrenceKey, current.Roles.RolesOrigin
		e.Fired = current.Fired
		e.AcknowledgedAt, e.AcknowledgedBy = current.AcknowledgedAt, current.AcknowledgedBy
		e.Wall, e.Overdue, e.Rank = current.Wall.String(), current.Overdue, agendaPriority(c)
		e.Reason = "outstanding"
		if e.Overdue {
			e.Reason = "target_passed"
		}
		if current.DeadlinePassed && current.Roles.DeadlineBehavior != nil && *current.Roles.DeadlineBehavior == "stays_owed" {
			e.Reason = "deadline_still_owed"
		}
		resp.PriorityList = append(resp.PriorityList, e)
	}
	ids = ids[:0]
	for _, rows := range [][]agendaEntry{resp.PriorityList[priorityStart:], resp.WaitingOn[waitingStart:]} {
		for _, e := range rows {
			if e.ScheduleID != "" {
				ids = append(ids, e.ScheduleID)
			}
		}
	}
	forecasts, err := s.store.ForecastSummaries(ids)
	if err != nil {
		slog.Warn("agenda: forecasts unavailable", "workspace_id", workspaceID, "error_class", fmt.Sprintf("%T", err))
		cut()
		return
	}
	for _, rows := range [][]agendaEntry{resp.PriorityList[priorityStart:], resp.WaitingOn[waitingStart:]} {
		for i := range rows {
			rows[i].Forecast = forecasts[rows[i].ScheduleID]
		}
	}
}

func finishAgendaWork(resp *agendaResponse) {
	rank := func(s string) int {
		switch s {
		case "high":
			return 0
		case "normal":
			return 1
		case "low":
			return 2
		default:
			return 3
		}
	}
	role := func(e agendaEntry, deadline bool) string {
		if e.Roles == nil {
			return ""
		}
		v := e.Roles.Target
		if deadline {
			v = e.Roles.Deadline
		}
		if v == nil {
			return ""
		}
		return *v
	}
	dateLess := func(a, b string) (bool, bool) {
		if a == b {
			return false, false
		}
		if a == "" {
			return false, true
		}
		if b == "" {
			return true, true
		}
		at, ae := time.Parse(time.RFC3339, a)
		bt, be := time.Parse(time.RFC3339, b)
		if ae == nil && be == nil {
			return at.Before(bt), !at.Equal(bt)
		}
		return a < b, true
	}
	sort.Slice(resp.PriorityList, func(i, j int) bool {
		a, b := resp.PriorityList[i], resp.PriorityList[j]
		if rank(a.Rank) != rank(b.Rank) {
			return rank(a.Rank) < rank(b.Rank)
		}
		for _, deadline := range []bool{true, false} {
			if less, diff := dateLess(role(a, deadline), role(b, deadline)); diff {
				return less
			}
		}
		if a.subKey != b.subKey {
			return a.subKey < b.subKey
		}
		if a.Workspace != b.Workspace {
			return a.Workspace < b.Workspace
		}
		return a.ItemRef < b.ItemRef
	})
	for _, g := range []struct {
		rows     *[]agendaEntry
		complete *bool
	}{{&resp.PriorityList, &resp.PriorityListComplete}, {&resp.WaitingOn, &resp.WaitingOnComplete}, {&resp.NoDate, &resp.NoDateComplete}} {
		if len(*g.rows) > agendaMaxEntries {
			*g.rows = (*g.rows)[:agendaMaxEntries]
			*g.complete = false
		}
		if *g.rows == nil {
			*g.rows = []agendaEntry{}
		}
	}
}
