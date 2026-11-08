package main

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

func migrationCommitment(t *testing.T) (*store.Store, *models.WorkspaceExport) {
	t.Helper()
	src := storetest.NewSQLite(t)
	ws, err := src.CreateWorkspace(models.WorkspaceCreate{Name: "Commitment migration"})
	if err != nil {
		t.Fatal(err)
	}
	col, err := src.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks", Schema: `{"fields":[]}`})
	if err != nil {
		t.Fatal(err)
	}
	item, err := src.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: "Renew"})
	if err != nil {
		t.Fatal(err)
	}
	sc, err := src.CreateSchedule(ws.ID, item.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2030-01-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"})
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := src.DB().Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	// Seed a consistent native label: the carry writer derives hard from deadline roles.
	exec(`UPDATE item_schedules SET kind='deadline_hard',target_days=1,earliest_days=-1,earliest_secs=-60,deadline_days=3,deadline_secs=0,deadline_behavior='lapses',occupied_days=0,occupied_secs=3600,roles_origin='native',outcome='Renewed' WHERE id=?`, sc.ID)
	key := *sc.NextAt + "|" + *sc.NextWall
	exec(`INSERT INTO schedule_occurrence_overrides (id,schedule_id,occurrence_key,role,value_at,value_wall,created_at,updated_at) VALUES ('override',?,?,'target','2030-01-01T12:00:00Z','2030-01-01T12:00:00','2026-11-01T12:00:00Z','2026-11-01T12:00:00Z')`, sc.ID, key)
	exec(`INSERT INTO schedule_occurrences (id,schedule_id,occurrence_at,occurrence_wall,state,actor,resolved_at,actual_done_at,actual_source) VALUES ('actual',?,'2029-12-31T09:00:00Z','2029-12-31T09:00:00','done','user','2029-12-31T11:00:00Z','2029-12-31T10:00:00Z','reported')`, sc.ID)
	data, err := src.ExportWorkspace(ws.Slug)
	if err != nil {
		t.Fatal(err)
	}
	return src, data
}

func TestMigrateCarriesCommitments(t *testing.T) {
	for _, backend := range []struct {
		name string
		open func(*testing.T) *store.Store
	}{{"SQLite", storetest.NewSQLite}, {"Postgres", storetest.NewPostgres}} {
		t.Run(backend.name, func(t *testing.T) {
			_, data := migrationCommitment(t)
			dst := backend.open(t)
			ws, err := migrateImportWorkspace(dst, data)
			if err != nil {
				t.Fatal(err)
			}
			out, err := dst.ExportWorkspace(ws.Slug)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(data.Schedules[0])
			var a map[string]any
			_ = json.Unmarshal(raw, &a)
			raw, _ = json.Marshal(out.Schedules[0])
			var b map[string]any
			_ = json.Unmarshal(raw, &b)
			if a["roles"] == nil || a["overrides"] == nil {
				t.Fatal("bundle omits commitments")
			}
			delete(a, "id")
			delete(b, "id")
			delete(a, "item_id")
			delete(b, "item_id")
			if !reflect.DeepEqual(a, b) {
				t.Fatalf("migration differs: %+v %+v", a, b)
			}
			raw, _ = json.Marshal(out)
			var m map[string]any
			_ = json.Unmarshal(raw, &m)
			if len(m["schedule_changes"].([]any)) != 2 {
				t.Fatal("migration lost journal")
			}
		})
	}
}

func TestMigrateImportNullSafeOnPG(t *testing.T) {
	dst := storetest.NewPostgres(t)
	_, data := migrationCommitment(t)
	ws, err := migrateImportWorkspace(dst, data)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := dst.DB().QueryRow(`SELECT COUNT(*) FROM schedule_changes WHERE workspace_id=$1 AND reason='' AND occurrence_key IS NULL`, ws.ID).Scan(&n); err != nil || n != 2 {
		t.Fatalf("NULL-safe journal: %d %v", n, err)
	}
}
