package store

import (
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestListForecastsForAgendaWindow(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	loc, err := time.LoadLocation("Pacific/Kiritimati")
	if err != nil {
		t.Fatal(err)
	}
	from, to := rt("2031-04-01T00:00:00Z"), rt("2031-04-02T00:00:00Z")
	values := []string{"2031-04-02", "2031-04-01T23:59:59Z", "2031-04-02T00:00:00Z", "2031-03-31"}
	ids := map[string]bool{}
	for i, value := range values {
		item, sc := f.mk(t, models.ScheduleCreate{Kind: "check", Dtstart: "2031-03-10T09:00", Tz: "UTC"}, "2031-03-01T00:00:00Z")
		if err := f.s.SetForecast(f.ws.ID, item.ID, "unknown", "agent", value, "estimate"); err != nil {
			t.Fatal(err)
		}
		if i < 2 {
			ids[sc.ID] = true
		}
	}
	rows, more, err := f.s.ListForecastsForAgenda(f.ws.ID, PendingReminderScope{}, from, to, loc, 10)
	if err != nil || more || len(rows) != 2 {
		t.Fatalf("window: rows=%+v more=%v err=%v", rows, more, err)
	}
	for _, row := range rows {
		if !ids[row.ScheduleID] || row.Day != "2031-04-02" || row.Forecast.SetBy != "unknown" || row.Forecast.SetByKind != "agent" || row.Forecast.Source != "estimate" {
			t.Errorf("unexpected forecast: %+v", row)
		}
		if row.AllDay != (len(row.Forecast.Value) == 10) {
			t.Errorf("all_day: %+v", row)
		}
	}
	// Rejected prefilter rows must not consume the exact-filter limit.
	rows, more, err = f.s.ListForecastsForAgenda(f.ws.ID, PendingReminderScope{}, from, to, loc, 1)
	if err != nil || !more || len(rows) != 1 || rows[0].Forecast.Value != values[1] {
		t.Fatalf("exact-filter cap: %+v more=%v err=%v", rows, more, err)
	}
	rows, more, err = f.s.ListForecastsForAgenda(f.ws.ID, PendingReminderScope{CollectionIDs: []string{}}, from, to, loc, 10)
	if err != nil || more || len(rows) != 0 {
		t.Fatalf("empty scope: %+v %v %v", rows, more, err)
	}
}

func TestListForecastsForAgendaExcludes(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	for _, kind := range []string{"ended", "errored", "terminal", "deleted", "other-workspace", "deleted-collection"} {
		world := f
		if kind == "other-workspace" {
			ws := createTestWorkspace(t, f.s, "Other forecasts")
			col := createTestCollection(t, f.s, ws.ID, "Tasks")
			world = &schedFixture{s: f.s, ws: ws, item: createTestItem(t, f.s, ws.ID, col.ID, "Other forecast", "")}
		}
		item, sc := world.mk(t, models.ScheduleCreate{Kind: "check", Dtstart: "2031-03-10T09:00", Tz: "UTC"}, "2031-03-01T00:00:00Z")
		if err := f.s.SetForecast(world.ws.ID, item.ID, "unknown", "user", "2031-04-02", ""); err != nil {
			t.Fatal(err)
		}
		switch kind {
		case "ended":
			f.exec(t, `UPDATE item_schedules SET ended_at='2031-03-11T00:00:00Z', next_at=NULL WHERE id=?`, sc.ID)
		case "errored":
			f.exec(t, `UPDATE item_schedules SET error_code='internal' WHERE id=?`, sc.ID)
		case "terminal":
			f.exec(t, `UPDATE items SET fields='{"status":"done"}' WHERE id=?`, item.ID)
		case "deleted":
			f.exec(t, `UPDATE items SET deleted_at='2031-03-11T00:00:00Z' WHERE id=?`, item.ID)
		case "deleted-collection":
			col := createTestCollection(t, f.s, f.ws.ID, "Deleted forecasts")
			f.exec(t, `UPDATE items SET collection_id=? WHERE id=?`, col.ID, item.ID)
			f.exec(t, `UPDATE collections SET deleted_at='2031-03-11T00:00:00Z' WHERE id=?`, col.ID)
		}
	}
	rows, more, err := f.s.ListForecastsForAgenda(f.ws.ID, PendingReminderScope{}, rt("2031-04-01T00:00:00Z"), rt("2031-04-03T00:00:00Z"), time.UTC, 10)
	if err != nil || more || len(rows) != 0 {
		t.Fatalf("excluded: rows=%+v more=%v err=%v", rows, more, err)
	}
}
