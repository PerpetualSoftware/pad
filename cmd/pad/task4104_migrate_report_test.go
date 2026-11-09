package main

import (
	"bytes"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// TASK-4104: `pad db migrate-to-pg` drops a schedule the import
// refuses, so it must say so: a line per drop as it happens, then a closing
// summary naming the SQLite file the schedule is still in. Print-only.
func TestTASK4104_MigrateReportsDroppedSchedules(t *testing.T) {
	src := storetest.NewSQLite(t)
	ws, err := src.CreateWorkspace(models.WorkspaceCreate{Name: "Migrate Drop"})
	if err != nil {
		t.Fatal(err)
	}
	col, err := src.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks",
		Schema: `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","done"],"default":"open","required":true}]}`})
	if err != nil {
		t.Fatal(err)
	}
	for _, title := range []string{"good", "bad"} {
		it, err := src.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: title})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := src.CreateSchedule(ws.ID, it.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2030-01-01T09:00", Tz: "UTC", Rrule: "FREQ=WEEKLY"}); err != nil {
			t.Fatal(err)
		}
	}
	data, err := src.ExportWorkspace(ws.Slug)
	if err != nil {
		t.Fatal(err)
	}
	var badID string
	for _, it := range data.Items {
		if it.Title == "bad" {
			badID = it.ID
		}
	}
	for i := range data.Schedules {
		if data.Schedules[i].ItemID == badID {
			data.Schedules[i].Rrule = "FREQ=HOURLY;BYSETPOS=1"
		}
	}

	var out bytes.Buffer
	dst := storetest.NewSQLite(t)
	got, drops, err := migrateImportWorkspaceReporting(dst, data, &out)
	if err != nil || got == nil {
		t.Fatalf("migrate import: %v", err)
	}
	want := []models.ImportScheduleDrop{{Item: "TASK-2", Reason: "timing:combination"}, {Item: "TASK-2", Reason: "changes:orphan"}}
	if !reflect.DeepEqual(drops, want) {
		t.Fatalf("drops %+v, want %+v", drops, want)
	}
	if !strings.Contains(out.String(), "dropped schedule: ") {
		t.Errorf("no per-drop line in %q", out.String())
	}
	var sum bytes.Buffer
	printMigrateDropSummary(&sum, []migrateDropped{{Workspace: "Migrate Drop", Drops: drops}}, "/data/pad.db")
	// The source is marked migrated (pad refuses to open it), so the summary
	// must say how the rows can still be read (TASK-4104).
	for _, want := range []string{"Migrate Drop", "2 schedule", "NOT carried", "/data/pad.db",
		"marked\nas migrated, so pad will not open it", "sqlite3 '/data/pad.db'", "item_schedules", "schedule_occurrences", "schedule_occurrence_overrides", "schedule_changes", "schedule_links", "schedule_link_overrides", "pad db backup"} {
		if !strings.Contains(sum.String(), want) {
			t.Errorf("summary lacks %q: %q", want, sum.String())
		}
	}
}

// TASK-4104: the summary's sqlite3 hint is a pasteable command, so a
// source path with a space or a quote (a realistic macOS data path) must
// reach the shell as one argument, unchanged.
func TestTASK4104_MigrateDropSummaryQuotesThePath(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh on PATH")
	}
	path := "/Users/dave/Library/Application Support/it's pad.db"
	var sum bytes.Buffer
	printMigrateDropSummary(&sum, []migrateDropped{{Workspace: "W", Drops: []models.ImportScheduleDrop{{Item: "TASK-1", Reason: "timing:range"}}}}, path)
	out := sum.String()
	i := strings.Index(out, "sqlite3 ")
	j := strings.Index(out, " (tables")
	if i < 0 || j < i {
		t.Fatalf("no sqlite3 hint in %q", out)
	}
	arg := out[i+len("sqlite3 ") : j]
	got, err := exec.Command("sh", "-c", "printf %s "+arg).Output()
	if err != nil {
		t.Fatalf("sh -c on the hint's argument %q: %v", arg, err)
	}
	if string(got) != path {
		t.Errorf("the hint's argument %q reaches the shell as %q, want %q", arg, got, path)
	}
}
