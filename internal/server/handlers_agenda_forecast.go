package server

import (
	"sort"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

const agendaMaxForecasts = 500

type agendaForecast struct {
	ScheduleID    string                 `json:"schedule_id"`
	ItemRef       string                 `json:"item_ref"`
	ItemTitle     string                 `json:"item_title"`
	Collection    string                 `json:"collection"`
	Day           string                 `json:"day"`
	AllDay        bool                   `json:"all_day,omitempty"`
	Forecast      models.ForecastSummary `json:"forecast"`
	Workspace     string                 `json:"workspace,omitempty"`
	WorkspaceName string                 `json:"workspace_name,omitempty"`
	OwnerUsername string                 `json:"owner_username,omitempty"`
}

func agendaForecastRow(row store.AgendaForecast) agendaForecast {
	return agendaForecast{ScheduleID: row.ScheduleID, ItemRef: row.ItemRef, ItemTitle: row.ItemTitle, Collection: row.Collection, Day: row.Day, AllDay: row.AllDay, Forecast: row.Forecast}
}

// appendWorkspaceForecasts keeps the earliest forecasts across all admitted
// workspaces, without consuming or changing any occurrence bounds or cuts.
// workspace supplies the personal agenda identity; it is nil on workspace reads.
func (s *Server) appendWorkspaceForecasts(run *agendaRead, workspaceID string, scope store.PendingReminderScope, workspace *models.Workspace) error {
	rows, more, err := s.store.ListForecastsForAgenda(workspaceID, scope, run.from, run.to, run.loc, agendaMaxForecasts)
	if err != nil {
		return err
	}
	resp := &run.response
	for _, row := range rows {
		f := agendaForecastRow(row)
		if workspace != nil {
			f.Workspace, f.WorkspaceName, f.OwnerUsername = workspace.Slug, workspace.Name, workspace.OwnerUsername
		}
		resp.Forecasts = append(resp.Forecasts, f)
	}
	sort.SliceStable(resp.Forecasts, func(i, j int) bool {
		a, b := resp.Forecasts[i], resp.Forecasts[j]
		if a.Forecast.Value != b.Forecast.Value {
			return a.Forecast.Value < b.Forecast.Value
		}
		return a.ScheduleID < b.ScheduleID
	})
	if more || len(resp.Forecasts) > agendaMaxForecasts {
		resp.ForecastsComplete = false
	}
	if len(resp.Forecasts) > agendaMaxForecasts {
		resp.Forecasts = resp.Forecasts[:agendaMaxForecasts]
	}
	return nil
}
