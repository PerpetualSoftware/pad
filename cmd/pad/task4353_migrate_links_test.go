package main

import (
	"bytes"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// linkSignatures renders a bundle's links with each end named by its item's
// title, so a workspace and its migrated copy compare equal although every
// schedule id was minted again.
func linkSignatures(t *testing.T, b *models.WorkspaceExport) []string {
	t.Helper()
	title := map[string]string{}
	for _, it := range b.Items {
		title[it.ID] = it.Title
	}
	byID := map[string]string{}
	for _, sc := range b.Schedules {
		byID[sc.ID] = title[sc.ItemID]
	}
	var out []string
	for _, l := range b.ScheduleLinks {
		l.ParentScheduleID, l.ChildScheduleID = byID[l.ParentScheduleID], byID[l.ChildScheduleID]
		raw, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(raw))
	}
	sort.Strings(out)
	return out
}

// TASK-4353: `pad db migrate-to-pg` rides the workspace bundle, so the links
// between schedules and their per-occurrence overrides must reach the
// destination with the ends remapped, and a link the destination refuses must be
// named rather than lost silently. The source is SQLite either way; the
// destination runs on both drivers (Postgres skips without PAD_TEST_POSTGRES_URL).
func TestTASK4353_MigrateCarriesLinks(t *testing.T) {
	for _, b := range []struct {
		name string
		open func(*testing.T) *store.Store
	}{{"SQLite", storetest.NewSQLite}, {"Postgres", storetest.NewPostgres}} {
		t.Run(b.name, func(t *testing.T) {
			src := storetest.NewSQLite(t)
			ws, err := src.CreateWorkspace(models.WorkspaceCreate{Name: "Migrate 4353"})
			if err != nil {
				t.Fatal(err)
			}
			col, err := src.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks",
				Schema: `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","done"],"default":"open","required":true}]}`})
			if err != nil {
				t.Fatal(err)
			}
			access := models.CommitmentAccess{Readable: map[string]bool{}, Editable: map[string]bool{}}
			var items []*models.Item
			for i, title := range []string{"Order", "Install", "Inspect"} {
				it, err := src.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: title})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := src.CreateCommitmentSchedule(ws.ID, it.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2030-03-0" + string(rune('4'+i)) + "T09:00", Tz: "UTC"}, models.CommitmentRolesInput{}); err != nil {
					t.Fatal(err)
				}
				items = append(items, it)
				access.Readable[it.ID], access.Editable[it.ID] = true, true
			}
			civil := models.ScheduleLinkOffset{Mode: "civil", Days: 2, ClockSecs: 9 * 3600, Zone: "America/New_York"}
			if _, err := src.CreateScheduleLink(ws.ID, models.ScheduleLinkCreate{ParentItemID: items[0].ID, ParentRole: "target", ChildItemID: items[1].ID, ChildRole: "target", Offset: civil}, access); err != nil {
				t.Fatal(err)
			}
			policy := models.LinkCancelChildOccurrence
			if _, err := src.CreateScheduleLink(ws.ID, models.ScheduleLinkCreate{ParentItemID: items[1].ID, ParentRole: "target", ChildItemID: items[2].ID, ChildRole: "target",
				Offset: models.ScheduleLinkOffset{Mode: "elapsed", ElapsedSecs: -3600}, CancelPolicy: policy}, access); err != nil {
				t.Fatal(err)
			}
			// Dragging the child keeps the link at the new offset for that occurrence: an override row.
			if _, err := src.MoveLinkedDate(ws.ID, models.LinkedMoveInput{ItemID: items[2].ID, Role: "target", To: "2030-03-12T09:00:00Z", Actor: "user", Access: access}); err != nil {
				t.Fatal(err)
			}
			data, err := src.ExportWorkspace(ws.Slug)
			if err != nil {
				t.Fatal(err)
			}
			if data.Version != 3 || len(data.ScheduleLinks) != 2 {
				t.Fatalf("the bundle is version %d with %d links, want version 3 with 2", data.Version, len(data.ScheduleLinks))
			}
			overrides := 0
			for _, l := range data.ScheduleLinks {
				overrides += len(l.Overrides)
			}
			if overrides != 1 {
				t.Fatalf("the bundle carries %d link overrides, want the one the drag wrote", overrides)
			}

			dst := b.open(t)
			var out bytes.Buffer
			got, drops, err := migrateImportWorkspaceReporting(dst, data, &out)
			if err != nil {
				t.Fatalf("migrate import: %v", err)
			}
			if len(drops) != 0 {
				t.Fatalf("the migration dropped %+v", drops)
			}
			again, err := dst.ExportWorkspace(got.Slug)
			if err != nil {
				t.Fatal(err)
			}
			if want, have := linkSignatures(t, data), linkSignatures(t, again); len(want) != 2 || !reflect.DeepEqual(want, have) {
				t.Errorf("links differ after the migration:\n src %q\n dst %q", want, have)
			}
			// The migrated links are the ones a live cascade reads.
			dstItems, err := dst.ListItems(got.ID, models.ItemListParams{})
			if err != nil || len(dstItems) != 3 {
				t.Fatalf("destination items %v, %v", dstItems, err)
			}
			dstAccess := models.CommitmentAccess{Readable: map[string]bool{}, Editable: map[string]bool{}}
			var order string
			for _, it := range dstItems {
				dstAccess.Readable[it.ID], dstAccess.Editable[it.ID] = true, true
				if it.Title == "Order" {
					order = it.ID
				}
			}
			links, err := dst.ListScheduleLinks(got.ID, order, dstAccess)
			if err != nil || len(links) != 1 || links[0].Offset != civil {
				t.Fatalf("links of Order after the migration: %+v, %v", links, err)
			}
		})
	}

	t.Run("a refused link is named", func(t *testing.T) {
		src := storetest.NewSQLite(t)
		ws, err := src.CreateWorkspace(models.WorkspaceCreate{Name: "Migrate 4353 drop"})
		if err != nil {
			t.Fatal(err)
		}
		col, err := src.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks",
			Schema: `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","done"],"default":"open","required":true}]}`})
		if err != nil {
			t.Fatal(err)
		}
		for _, title := range []string{"A", "B"} {
			it, err := src.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: title})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := src.CreateCommitmentSchedule(ws.ID, it.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2030-03-04T09:00", Tz: "UTC"}, models.CommitmentRolesInput{}); err != nil {
				t.Fatal(err)
			}
		}
		data, err := src.ExportWorkspace(ws.Slug)
		if err != nil {
			t.Fatal(err)
		}
		data.ScheduleLinks = []models.ScheduleLinkExport{{ParentScheduleID: data.Schedules[0].ID, ParentRole: "target", ChildScheduleID: "gone", ChildRole: "target",
			Offset: models.ScheduleLinkOffset{Mode: "elapsed", ElapsedSecs: 60}, CancelPolicy: models.LinkWaitingOn, CreatedAt: "2030-03-01T00:00:00Z", UpdatedAt: "2030-03-01T00:00:00Z"}}
		var out bytes.Buffer
		_, drops, err := migrateImportWorkspaceReporting(storetest.NewSQLite(t), data, &out)
		if err != nil {
			t.Fatal(err)
		}
		if len(drops) != 1 || drops[0].Reason != "links:orphan" || !strings.Contains(out.String(), "(links:orphan)") {
			t.Fatalf("drops %+v, output %q", drops, out.String())
		}
	})
}

// An aggregate drop stands for many records (links or override rows left out past
// an import ceiling). The migration output and its closing summary must show the
// number, not "1" and "(item not imported)", because the source is abandoned
// after the migration and the operator decides from these lines what to read back.
func TestTASK4353_MigrateReportCountsAggregateDrops(t *testing.T) {
	drops := []models.ImportScheduleDrop{
		{Item: "TASK-4", Reason: "links:cycle"},
		{Reason: "links:bundle_limit", Count: 7000},
		{Reason: "link_overrides:bundle_limit", Count: 120},
	}
	var lines bytes.Buffer
	printImportScheduleDrops(&lines, drops)
	for _, want := range []string{"dropped schedule: TASK-4 (links:cycle)", "7000", "links:bundle_limit", "120", "link_overrides:bundle_limit"} {
		if !strings.Contains(lines.String(), want) {
			t.Errorf("per-drop output lacks %q: %q", want, lines.String())
		}
	}
	if strings.Contains(lines.String(), "item not imported") {
		t.Errorf("an aggregate is not an item that failed to import: %q", lines.String())
	}
	var sum bytes.Buffer
	printMigrateDropSummary(&sum, []migrateDropped{{Workspace: "Big", Drops: drops}}, "/data/pad.db")
	for _, want := range []string{"7121 schedule record(s)", "  Big: 7121"} {
		if !strings.Contains(sum.String(), want) {
			t.Errorf("summary lacks %q: %q", want, sum.String())
		}
	}
}

// An aggregate of ONE record (exactly 10,001 links, or a single override row past
// a ceiling) is still an aggregate: it has no item, and saying "(item not
// imported)" for it would be false. Count is how a drop says so, so any nonzero
// Count is an aggregate, in the import output and in the migrate summary.
func TestTASK4353_MigrateReportCountsAOneRecordAggregate(t *testing.T) {
	drops := []models.ImportScheduleDrop{{Reason: "links:bundle_limit", Count: 1}}
	var lines bytes.Buffer
	printImportScheduleDrops(&lines, drops)
	if !strings.Contains(lines.String(), "dropped schedule records: 1 (links:bundle_limit)") || strings.Contains(lines.String(), "item not imported") {
		t.Errorf("per-drop output for an aggregate of one: %q", lines.String())
	}
	var single bytes.Buffer
	printImportScheduleDrops(&single, []models.ImportScheduleDrop{{Reason: "item:not_imported"}})
	if !strings.Contains(single.String(), "dropped schedule: (item not imported) (item:not_imported)") {
		t.Errorf("a single drop with no item still says so: %q", single.String())
	}
	var sum bytes.Buffer
	printMigrateDropSummary(&sum, []migrateDropped{{Workspace: "Edge", Drops: append(drops, models.ImportScheduleDrop{Item: "TASK-3", Reason: "links:cycle"})}}, "/data/pad.db")
	for _, want := range []string{"2 schedule record(s)", "  Edge: 2"} {
		if !strings.Contains(sum.String(), want) {
			t.Errorf("summary lacks %q: %q", want, sum.String())
		}
	}
}
