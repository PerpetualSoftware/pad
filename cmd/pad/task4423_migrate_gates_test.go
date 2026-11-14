package main

import (
	"bytes"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// gateSignatures names endpoints by title and discards only the source gate id.
func gateSignatures(t *testing.T, b *models.WorkspaceExport) []string {
	t.Helper()
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Gates []map[string]any `json:"schedule_gates"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	titles := map[string]string{}
	for _, it := range b.Items {
		titles[it.ID] = it.Title
	}
	schedules := map[string]string{}
	for _, sc := range b.Schedules {
		schedules[sc.ID] = titles[sc.ItemID]
	}
	var out []string
	for _, g := range wire.Gates {
		delete(g, "id")
		for _, key := range []string{"blocked_item_id", "blocker_item_id"} {
			g[key] = titles[g[key].(string)]
		}
		if id, ok := g["blocker_schedule_id"].(string); ok {
			g["blocker_schedule_id"] = schedules[id]
		}
		raw, err := json.Marshal(g)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(raw))
	}
	sort.Strings(out)
	return out
}

func TestTASK4423_R10Migrate(t *testing.T) {
	for _, backend := range []struct {
		name string
		open func(*testing.T) *store.Store
	}{
		{"SQLite", storetest.NewSQLite}, {"Postgres", storetest.NewPostgres},
	} {
		t.Run(backend.name, func(t *testing.T) {
			dst := backend.open(t)
			src := storetest.NewSQLite(t)
			ws, err := src.CreateWorkspace(models.WorkspaceCreate{Name: "Migrate gates"})
			if err != nil {
				t.Fatal(err)
			}
			col, err := src.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks", Schema: `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","done"],"terminal_options":["done"]}]}`})
			if err != nil {
				t.Fatal(err)
			}
			access := models.CommitmentAccess{Readable: map[string]bool{}, Editable: map[string]bool{}}
			var items []*models.Item
			for _, title := range []string{"pending blocker", "child", "opened child", "deleted blocker", "held child"} {
				it, err := src.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: title})
				if err != nil {
					t.Fatal(err)
				}
				items = append(items, it)
				access.Readable[it.ID], access.Editable[it.ID] = true, true
			}
			sc, err := src.CreateCommitmentSchedule(ws.ID, items[0].ID, models.ScheduleCreate{Kind: "check", Dtstart: "2030-03-04T09:00", Tz: "UTC"}, models.CommitmentRolesInput{})
			if err != nil {
				t.Fatal(err)
			}
			for _, pair := range [][2]int{{1, 0}, {2, 0}, {4, 3}} {
				if _, err := src.CreateScheduleGate(ws.ID, items[pair[0]].ID, items[pair[1]].ID, access); err != nil {
					t.Fatal(err)
				}
			}
			occurrenceAt, err := time.Parse(time.RFC3339, *sc.NextAt)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := src.ResolveOccurrence(ws.ID, items[0].ID, store.ResolveInput{OccurrenceAt: occurrenceAt, State: "done", Actor: "user", EndSchedule: true}); err != nil {
				t.Fatal(err)
			}
			if _, err := src.ScheduleGates(ws.ID, items[2].ID, access); err != nil {
				t.Fatal(err)
			}
			if err := src.DeleteItem(items[3].ID); err != nil {
				t.Fatal(err)
			}
			data, err := src.ExportWorkspace(ws.Slug)
			if err != nil {
				t.Fatal(err)
			}
			want := gateSignatures(t, data)
			if len(want) != 3 {
				t.Fatalf("exported %d gates, want 3", len(want))
			}
			var report bytes.Buffer
			got, drops, err := migrateImportWorkspaceReporting(dst, data, &report)
			if err != nil || len(drops) != 0 {
				t.Fatalf("migrate: ws=%+v drops=%+v err=%v", got, drops, err)
			}
			again, err := dst.ExportWorkspace(got.Slug)
			if err != nil {
				t.Fatal(err)
			}
			if have := gateSignatures(t, again); !reflect.DeepEqual(want, have) {
				t.Fatalf("gates differ:\nsource %q\ndestination %q", want, have)
			}
			if !strings.Contains(report.String(), "3 schedule gates verified") {
				t.Fatalf("missing gate verification report: %q", report.String())
			}
		})
	}
}
