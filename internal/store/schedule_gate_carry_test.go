package store

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// gateBundleRow checks the wire contract independently of the bundle model.
type gateBundleRow struct {
	ID                   string  `json:"id"`
	BlockedItemID        string  `json:"blocked_item_id"`
	BlockerItemID        string  `json:"blocker_item_id"`
	BlockerScheduleID    *string `json:"blocker_schedule_id"`
	BlockerOccurrenceKey string  `json:"blocker_occurrence_key"`
	OpenedAt             *string `json:"opened_at"`
	CreatedAt            string  `json:"created_at"`
}

func bundleGates(t *testing.T, b *models.WorkspaceExport) []gateBundleRow {
	t.Helper()
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Gates []gateBundleRow `json:"schedule_gates"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	return wire.Gates
}

func editGateBundle(t *testing.T, b *models.WorkspaceExport, edit func(map[string]any)) *models.WorkspaceExport {
	t.Helper()
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	edit(wire)
	raw, err = json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	var out models.WorkspaceExport
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return &out
}

func TestTASK4423_R10Bundle(t *testing.T) {
	for _, tc := range []struct {
		name                             string
		plainChild, plainBlocker, opened bool
	}{
		{name: "pending"}, {name: "opened", opened: true}, {name: "gated item without schedule", plainChild: true}, {name: "blocker without schedule", plainBlocker: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLinkFix(t, 2)
			child, blocker := f.items[1], f.items[0]
			if tc.plainChild {
				child = f.another(t, "plain child")
			}
			if tc.plainBlocker {
				blocker = f.another(t, "plain blocker")
			}
			g := f.gate(t, child.ID, blocker.ID, f.gateAccess(child, blocker))
			at := "2030-03-02T12:30:00.123456789+02:00"
			if tc.opened {
				f.exec(t, `UPDATE schedule_gates SET opened_at=? WHERE id=?`, at, g.ID)
			}
			b := f.exportBundle(t)
			gates := bundleGates(t, b)
			if b.Version != 4 || len(gates) != 1 {
				t.Fatalf("version=%d gates=%+v, want version 4 and one gate", b.Version, gates)
			}
			before := gates[0]
			if before.ID != g.ID || before.BlockedItemID != child.ID || before.BlockerItemID != blocker.ID {
				t.Fatalf("gate references: %+v", before)
			}
			if tc.plainBlocker {
				if before.BlockerScheduleID != nil || before.BlockerOccurrenceKey != "" {
					t.Fatalf("plain blocker: %+v", before)
				}
			} else if before.BlockerScheduleID == nil || *before.BlockerScheduleID != f.scheds[0].ID || before.BlockerOccurrenceKey != OccurrenceKey(f.scheds[0], *f.scheds[0].NextAt, *f.scheds[0].NextWall) {
				t.Fatalf("captured occurrence: %+v", before)
			}
			ws, rep := f.importBundle(t, b, "Restored gate")
			if len(rep.DroppedSchedules) != 0 {
				t.Fatalf("drops: %+v", rep.DroppedSchedules)
			}
			again, err := f.s.ExportWorkspace(ws.Slug)
			if err != nil {
				t.Fatal(err)
			}
			after := bundleGates(t, again)
			if len(after) != 1 {
				t.Fatalf("restored gates: %+v", after)
			}
			got := after[0]
			if got.ID == before.ID || got.BlockedItemID == before.BlockedItemID || got.BlockerItemID == before.BlockerItemID || got.CreatedAt != before.CreatedAt || got.BlockerOccurrenceKey != before.BlockerOccurrenceKey || !reflect.DeepEqual(got.OpenedAt, before.OpenedAt) {
				t.Fatalf("gate not remapped or preserved: before=%+v after=%+v", before, got)
			}
			if before.BlockerScheduleID != nil {
				if got.BlockerScheduleID == nil || *got.BlockerScheduleID == *before.BlockerScheduleID || f.count(t, `SELECT COUNT(*) FROM item_schedules WHERE id=? AND item_id=? AND workspace_id=?`, *got.BlockerScheduleID, got.BlockerItemID, ws.ID) != 1 {
					t.Fatalf("blocker schedule not remapped: %+v", got)
				}
			} else if got.BlockerScheduleID != nil {
				t.Fatalf("NULL schedule lost: %+v", got)
			}
			view, err := f.s.ScheduleGates(ws.ID, got.BlockedItemID, models.CommitmentAccess{Readable: map[string]bool{got.BlockedItemID: true, got.BlockerItemID: true}})
			want := models.GateStateWaiting
			if tc.opened {
				want = models.GateStateReleased
			}
			if err != nil || view.State != want {
				t.Fatalf("view=%+v err=%v, want %s", view, err, want)
			}
			if tc.opened && (got.OpenedAt == nil || *got.OpenedAt != at || f.openedAt(t, got.ID).String != at) {
				t.Fatal("opening timestamp changed")
			}
		})
	}
	t.Run("soft deleted blocker", func(t *testing.T) {
		f := newLinkFix(t, 2)
		blocker, child := f.items[0], f.items[1]
		body := "blocker body"
		if _, err := f.s.UpdateItem(blocker.ID, models.ItemUpdate{Content: &body}); err != nil {
			t.Fatal(err)
		}
		f.gate(t, child.ID, blocker.ID, f.allEdit())
		if _, err := f.s.CreateComment(f.ws.ID, blocker.ID, "", models.CommentCreate{Author: "test", Body: "blocker comment"}); err != nil {
			t.Fatal(err)
		}
		if f.count(t, `SELECT COUNT(*) FROM item_versions WHERE item_id=?`, blocker.ID) == 0 {
			t.Fatal("fixture has no blocker versions")
		}
		if f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE item_id=?`, blocker.ID) == 0 {
			t.Fatal("fixture has no blocker history")
		}
		if err := f.s.DeleteItem(blocker.ID); err != nil {
			t.Fatal(err)
		}
		b := f.exportBundle(t)
		gates := bundleGates(t, b)
		if len(gates) != 1 || gates[0].BlockerScheduleID != nil || gates[0].BlockerOccurrenceKey != "" {
			t.Fatalf("deleted blocker gate: %+v", gates)
		}
		var mark string
		raw, _ := json.Marshal(b)
		var wire struct {
			Items []struct {
				ID        string `json:"id"`
				DeletedAt string `json:"deleted_at"`
			} `json:"items"`
		}
		if err := json.Unmarshal(raw, &wire); err != nil {
			t.Fatal(err)
		}
		for _, it := range wire.Items {
			if it.ID == blocker.ID {
				mark = it.DeletedAt
			}
		}
		if mark == "" {
			t.Fatal("deleted blocker row or deleted_at missing")
		}
		for _, sc := range b.Schedules {
			if sc.ItemID == blocker.ID {
				t.Fatal("deleted blocker schedule exported")
			}
		}
		for _, c := range b.Comments {
			if c.ItemID == blocker.ID {
				t.Fatal("deleted blocker comment exported")
			}
		}
		for _, c := range b.ScheduleChanges {
			if c.ItemID == blocker.ID {
				t.Fatal("deleted blocker history exported")
			}
		}
		for _, v := range b.ItemVersions {
			if v.ItemID == blocker.ID {
				t.Fatal("deleted blocker version exported")
			}
		}
		ws, _ := f.importBundle(t, b, "Restored tombstone")
		again, err := f.s.ExportWorkspace(ws.Slug)
		if err != nil {
			t.Fatal(err)
		}
		restored := bundleGates(t, again)
		if len(restored) != 1 {
			t.Fatalf("restored gates=%+v", restored)
		}
		gate := restored[0]
		it, err := f.s.GetItem(gate.BlockerItemID)
		if err != nil || it != nil {
			t.Fatalf("deleted blocker visible: %+v, %v", it, err)
		}
		items, err := f.s.ListItems(ws.ID, models.ItemListParams{})
		if err != nil || len(items) != 1 {
			t.Fatalf("normal items=%+v err=%v", items, err)
		}
		if f.count(t, `SELECT COUNT(*) FROM items WHERE id=? AND deleted_at=?`, gate.BlockerItemID, mark) != 1 {
			t.Fatal("deleted_at changed")
		}
		view, err := f.s.ScheduleGates(ws.ID, gate.BlockedItemID, models.CommitmentAccess{Readable: map[string]bool{gate.BlockedItemID: true}})
		if err != nil || view.State != models.GateStateBlockedNeedsDecision {
			t.Fatalf("view=%+v err=%v", view, err)
		}
	})
	t.Run("soft deleted blocked item", func(t *testing.T) {
		f := newLinkFix(t, 2)
		f.gate(t, f.items[1].ID, f.items[0].ID, f.allEdit())
		if err := f.s.DeleteItem(f.items[1].ID); err != nil {
			t.Fatal(err)
		}
		b := f.exportBundle(t)
		if len(bundleGates(t, b)) != 0 || len(b.Items) != 1 {
			t.Fatalf("deleted child exported: %+v", b)
		}
	})
	t.Run("other soft deleted items stay out", func(t *testing.T) {
		f := newLinkFix(t, 2)
		other := f.another(t, "unreferenced tombstone")
		if err := f.s.DeleteItem(other.ID); err != nil {
			t.Fatal(err)
		}
		b := f.exportBundle(t)
		if len(b.Items) != 2 {
			t.Fatalf("items=%+v", b.Items)
		}
	})
	for _, kind := range []string{"unknown blocked item", "unknown blocker item", "unknown schedule", "foreign schedule", "self gate", "duplicate pair", "duplicate id", "cycle", "dropped schedule"} {
		t.Run(kind, func(t *testing.T) {
			f := newLinkFix(t, 2)
			b := f.exportBundle(t)
			sc := f.scheds[0].ID
			g := gateBundleRow{ID: "source-gate", BlockedItemID: f.items[1].ID, BlockerItemID: f.items[0].ID, BlockerScheduleID: &sc, BlockerOccurrenceKey: "2030-03-04T09:00:00Z|2030-03-04T09:00:00", CreatedAt: "2030-03-01T00:00:00Z"}
			gates := []gateBundleRow{g}
			switch kind {
			case "unknown blocked item":
				gates[0].BlockedItemID = "missing"
			case "unknown blocker item":
				gates[0].BlockerItemID = "missing"
			case "unknown schedule":
				missing := "missing"
				gates[0].BlockerScheduleID = &missing
			case "foreign schedule":
				gates[0].BlockerScheduleID = &f.scheds[1].ID
			case "self gate":
				gates[0].BlockerItemID = gates[0].BlockedItemID
			case "duplicate pair":
				g.ID = "second-gate"
				gates = append(gates, g)
			case "duplicate id", "cycle":
				g.BlockedItemID, g.BlockerItemID = g.BlockerItemID, g.BlockedItemID
				g.BlockerScheduleID = &f.scheds[1].ID
				if kind == "cycle" {
					g.ID = "second-gate"
				}
				gates = append(gates, g)
			case "dropped schedule":
				b.Schedules[0].Kind = "invalid"
				gates[0].BlockerScheduleID = &b.Schedules[0].ID
			}
			b = editGateBundle(t, b, func(w map[string]any) { w["schedule_gates"] = gates })
			before := f.count(t, `SELECT COUNT(*) FROM workspaces`)
			ws, _, err := f.s.ImportWorkspaceWithReport(b, "Invalid gates", "", "api")
			if _, ok := AsValidationError(err); !ok || ws != nil {
				t.Fatalf("ws=%+v err=%v, want validation refusal", ws, err)
			}
			if after := f.count(t, `SELECT COUNT(*) FROM workspaces`); after != before {
				t.Fatalf("workspace leaked: before=%d after=%d", before, after)
			}
		})
	}
	for _, version := range []int{1, 2, 3} {
		t.Run(fmt.Sprintf("version %d without gates", version), func(t *testing.T) {
			f := newSchedFixture(t)
			b := f.exportBundle(t)
			b.Version = version
			ws, _ := f.importBundle(t, b, "Legacy gate-free")
			if f.count(t, `SELECT COUNT(*) FROM schedule_gates WHERE workspace_id=?`, ws.ID) != 0 {
				t.Fatal("legacy bundle acquired gates")
			}
		})
		for _, member := range []string{"gates null", "gates empty", "deleted_at null", "deleted_at empty"} {
			t.Run(fmt.Sprintf("version %d with %s", version, member), func(t *testing.T) {
				f := newSchedFixture(t)
				b := editGateBundle(t, f.exportBundle(t), func(w map[string]any) {
					w["version"] = version
					switch member {
					case "gates null":
						w["schedule_gates"] = nil
					case "gates empty":
						w["schedule_gates"] = []any{}
					case "deleted_at null":
						w["items"].([]any)[0].(map[string]any)["deleted_at"] = nil
					case "deleted_at empty":
						w["items"].([]any)[0].(map[string]any)["deleted_at"] = ""
					}
				})
				before := f.count(t, `SELECT COUNT(*) FROM workspaces`)
				ws, _, err := f.s.ImportWorkspaceWithReport(b, "Invalid version", "", "api")
				if _, ok := AsValidationError(err); !ok || ws != nil || !strings.Contains(err.Error(), "version 4") {
					t.Fatalf("ws=%+v err=%v", ws, err)
				}
				if f.count(t, `SELECT COUNT(*) FROM workspaces`) != before {
					t.Fatal("workspace leaked")
				}
			})
		}
	}
	t.Run("import does not evaluate or notify", func(t *testing.T) {
		f := newLinkFix(t, 1)
		blocker := f.buildItem(t, "open")
		f.gate(t, f.items[0].ID, blocker.ID, f.gateAccess(blocker))
		// Written behind the store, so the gate is still unopened when exported.
		f.exec(t, `UPDATE items SET fields=? WHERE id=?`, `{"status":"shipped"}`, blocker.ID)
		b := f.exportBundle(t)
		before := f.count(t, `SELECT COUNT(*) FROM user_notifications`)
		ws, _ := f.importBundle(t, b, "Succeeded blocker")
		if f.count(t, `SELECT COUNT(*) FROM schedule_gates WHERE workspace_id=? AND opened_at IS NULL`, ws.ID) != 1 {
			t.Fatal("import dropped or opened a gate")
		}
		if f.count(t, `SELECT COUNT(*) FROM user_notifications`) != before {
			t.Fatal("import sent a notification")
		}
	})
}
