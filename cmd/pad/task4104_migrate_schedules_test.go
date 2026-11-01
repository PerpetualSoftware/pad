package main

import (
	"reflect"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// TASK-4104 D2: `pad db migrate-to-pg` rides the workspace bundle, so an item's
// schedule, its managed reminder and its history must reach the destination.
// Before, the schedule was lost and only the migrated-tables guard refused a
// later write to the abandoned file. The source file is SQLite either way; the
// destination runs on both drivers (Postgres skips without PAD_TEST_POSTGRES_URL).
func TestTASK4104_MigrateCarriesSchedules(t *testing.T) {
	for _, b := range []struct {
		name string
		open func(*testing.T) *store.Store
	}{{"SQLite", storetest.NewSQLite}, {"Postgres", storetest.NewPostgres}} {
		t.Run(b.name, func(t *testing.T) {
			src := storetest.NewSQLite(t)
			ws, err := src.CreateWorkspace(models.WorkspaceCreate{Name: "Migrate 4104"})
			if err != nil {
				t.Fatal(err)
			}
			col, err := src.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks",
				Schema: `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","done"],"default":"open","required":true}]}`})
			if err != nil {
				t.Fatal(err)
			}
			item, err := src.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: "Renew certificate"})
			if err != nil {
				t.Fatal(err)
			}
			want, err := src.CreateSchedule(ws.ID, item.ID, models.ScheduleCreate{
				Kind: "deadline_hard", Dtstart: "2030-01-01T09:00", Tz: "America/New_York", Rrule: "FREQ=WEEKLY",
				RDates: []string{"2030-01-03T09:00"}, LeadMinutes: 15})
			if err != nil {
				t.Fatal(err)
			}
			data, err := src.ExportWorkspace(ws.Slug)
			if err != nil {
				t.Fatal(err)
			}
			if len(data.Schedules) != 1 {
				t.Fatalf("the bundle carries %d schedules, want 1", len(data.Schedules))
			}

			dst := b.open(t)
			got, err := migrateImportWorkspace(dst, data)
			if err != nil {
				t.Fatalf("migrate import: %v", err)
			}
			items, err := dst.ListItems(got.ID, models.ItemListParams{})
			if err != nil || len(items) != 1 {
				t.Fatalf("destination items %v, %v", items, err)
			}
			sc, err := dst.GetSchedule(got.ID, items[0].ID)
			if err != nil || sc == nil {
				t.Fatalf("the schedule was lost by the migration: %v %v", sc, err)
			}
			a, c := *want, *sc
			for _, x := range []*models.ItemSchedule{&a, &c} {
				x.ID, x.WorkspaceID, x.ItemID, x.ReminderID = "", "", "", nil
			}
			if !reflect.DeepEqual(a, c) {
				t.Errorf("schedule differs after migration:\n src %+v\n dst %+v", a, c)
			}
			if sc.ReminderID == nil {
				t.Fatal("no managed reminder row after migration")
			}
			sr, err1 := src.GetReminder(ws.ID, *want.ReminderID)
			dr, err2 := dst.GetReminder(got.ID, *sc.ReminderID)
			if err1 != nil || err2 != nil || sr == nil || dr == nil || sr.RemindAt != dr.RemindAt {
				t.Errorf("managed reminder: src %+v (%v), dst %+v (%v)", sr, err1, dr, err2)
			}
		})
	}
}
