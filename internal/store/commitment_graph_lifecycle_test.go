package store

import (
	"context"
	"database/sql"
	"github.com/PerpetualSoftware/pad/internal/models"
	"testing"
)

// An occurrence armed while its item is held must still fire once released,
// even when the managed row last fired for an earlier occurrence.
func TestTASK4423_R09HeldRowRearms(t *testing.T) {
	f := newLinkFix(t, 1)
	blocker := f.buildItem(t, "open")
	f.gate(t, f.items[0].ID, blocker.ID, f.gateAccess(blocker))
	f.exec(t, `UPDATE item_reminders SET fired_at=? WHERE schedule_id=?`, "2026-01-01T00:00:00Z", f.scheds[0].ID)
	tx, err := f.s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := f.s.armManagedReminderTx(tx, f.reload(t, f.items[0].ID), "2026-01-02T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	const nowTS = "2030-06-01T00:00:00Z"
	fired, err := f.s.FireDueReminders(nowTS, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(fired) != 0 {
		t.Fatalf("fired while held: %+v", fired)
	}
	fields := `{"status":"shipped"}`
	if _, err := f.s.UpdateItem(blocker.ID, models.ItemUpdate{Fields: &fields}); err != nil {
		t.Fatal(err)
	}
	fired, err = f.s.FireDueReminders(nowTS, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(fired) != 1 || fired[0].ItemID != f.items[0].ID {
		t.Fatalf("after release: %+v", fired)
	}
}

func TestTASK4423_R09GateLifecycle(t *testing.T) {
	t.Run("fenced writer", graphFencedGateTest)
	t.Run("done then reopened without read", func(t *testing.T) {
		f := newLinkFix(t, 1)
		graphGateRecipients(t, f)
		blocker := f.buildItem(t, "open")
		g := f.gate(t, f.items[0].ID, blocker.ID, f.gateAccess(blocker))
		fields := `{"status":"shipped"}`
		if _, err := f.s.UpdateItem(blocker.ID, models.ItemUpdate{Fields: &fields}); err != nil {
			t.Fatal(err)
		}
		if !f.openedAt(t, g.ID).Valid {
			t.Fatal("Done did not durably open gate")
		}
		if n := f.count(t, `SELECT COUNT(*) FROM user_notifications WHERE kind='commitment_gate_opened' AND dedupe_key=?`, "commitment-gate:"+g.ID); n != 1 {
			t.Fatalf("notice immediately after Done=%d", n)
		}
		fields = `{"status":"open"}`
		if _, err := f.s.UpdateItem(blocker.ID, models.ItemUpdate{Fields: &fields}); err != nil {
			t.Fatal(err)
		}
		if !f.openedAt(t, g.ID).Valid {
			t.Fatal("reopen closed gate")
		}
		if n := f.count(t, `SELECT COUNT(*) FROM user_notifications WHERE kind='commitment_gate_opened' AND dedupe_key=?`, "commitment-gate:"+g.ID); n != 1 {
			t.Fatalf("opening notices=%d", n)
		}
	})

	t.Run("late completion of lapsed occurrence", func(t *testing.T) {
		f := newLinkFix(t, 2)
		graphGateRecipients(t, f)
		f.deadline(t, 0, "lapses")
		at := rt(*f.reload(t, f.items[0].ID).NextAt)
		g := f.gate(t, f.items[1].ID, f.items[0].ID, f.allEdit())
		f.eval(t, farFuture, 0)
		if f.openedAt(t, g.ID).Valid {
			t.Fatal("lapse opened gate")
		}
		if _, err := f.s.resolveOccurrenceAt(f.ws.ID, f.items[0].ID, ResolveInput{OccurrenceAt: at, State: "done", Actor: "user"}, rt(farFuture)); err != nil {
			t.Fatal(err)
		}
		if !f.openedAt(t, g.ID).Valid {
			t.Fatal("late completion did not open gate")
		}
		if f.count(t, `SELECT COUNT(*) FROM user_notifications WHERE kind='commitment_gate_opened'`) != 1 {
			t.Fatal("late completion notice missing")
		}
	})
	t.Run("captured occurrence", func(t *testing.T) {
		f := newLinkFix(t, 2)
		f.recurring(t, 0)
		g := f.gate(t, f.items[1].ID, f.items[0].ID, f.allEdit())
		f.resolve(t, 0, "done")
		if !f.openedAt(t, g.ID).Valid {
			t.Fatal("resolve did not durably open gate")
		}
	})
	t.Run("read is pure", func(t *testing.T) {
		f := newLinkFix(t, 2)
		g := f.gate(t, f.items[1].ID, f.items[0].ID, f.allEdit())
		sc := f.reload(t, f.items[0].ID)
		f.exec(t, `INSERT INTO schedule_occurrences (id,schedule_id,occurrence_at,occurrence_wall,state,actor,resolved_at) VALUES (?,?,?,?,?,?,?)`, newID(), sc.ID, *sc.NextAt, *sc.NextWall, "done", "user", nowNano())
		before := renegotiateRows(t, f.s, "schedule_gates", "") + renegotiateRows(t, f.s, "user_notifications", "")
		f.gates(t, 1, f.allEdit())
		if after := renegotiateRows(t, f.s, "schedule_gates", "") + renegotiateRows(t, f.s, "user_notifications", ""); before != after {
			t.Fatal("gate read changed persisted rows")
		}
		if f.openedAt(t, g.ID).Valid {
			t.Fatal("ScheduleGates wrote opened_at")
		}
	})
	t.Run("created against success", func(t *testing.T) {
		f := newLinkFix(t, 1)
		graphGateRecipients(t, f)
		blocker := f.buildItem(t, "shipped")
		g := f.gate(t, f.items[0].ID, blocker.ID, f.gateAccess(blocker))
		if !f.openedAt(t, g.ID).Valid {
			t.Fatal("already successful blocker did not open gate")
		}
		if n := f.count(t, `SELECT COUNT(*) FROM user_notifications WHERE kind='commitment_gate_opened'`); n != 1 {
			t.Fatalf("notices=%d", n)
		}
	})
	t.Run("move writer", func(t *testing.T) {
		f := newLinkFix(t, 1)
		blocker := f.another(t, "blocker")
		dest := f.buildItem(t, "open")
		g := f.gate(t, f.items[0].ID, blocker.ID, f.gateAccess(blocker))
		if _, err := f.s.MoveItem(blocker.ID, dest.CollectionID, `{"status":"shipped"}`); err != nil {
			t.Fatal(err)
		}
		if !f.openedAt(t, g.ID).Valid {
			t.Fatal("move writer did not open gate")
		}
	})
	t.Run("rollback", func(t *testing.T) {
		f := newLinkFix(t, 1)
		graphGateRecipients(t, f)
		blocker := f.buildItem(t, "open")
		g := f.gate(t, f.items[0].ID, blocker.ID, f.gateAccess(blocker))
		tx, err := f.s.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err := tx.Exec(f.s.q(`UPDATE items SET fields=? WHERE id=?`), `{"status":"shipped"}`, blocker.ID); err != nil {
			t.Fatal(err)
		}
		if err := f.s.openGatesForBlockerTx(tx, f.ws.ID, blocker.ID); err != nil {
			t.Fatal(err)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		if f.openedAt(t, g.ID).Valid || f.count(t, `SELECT COUNT(*) FROM user_notifications WHERE kind='commitment_gate_opened'`) != 0 {
			t.Fatal("rolled-back gate effects survived")
		}
	})
	t.Run("done then deleted and bundled without gate read", func(t *testing.T) {
		f := newLinkFix(t, 2)
		f.recurring(t, 0)
		g := f.gate(t, f.items[1].ID, f.items[0].ID, f.allEdit())
		f.resolve(t, 0, "done")
		opened := f.openedAt(t, g.ID)
		if !opened.Valid {
			t.Fatal("resolve left gate closed")
		}
		if err := f.s.DeleteItem(f.items[0].ID); err != nil {
			t.Fatal(err)
		}
		bundle, err := f.s.ExportWorkspace(f.ws.Slug)
		if err != nil {
			t.Fatal(err)
		}
		copy, err := f.s.ImportWorkspace(bundle, "Gate copy", "", "test")
		if err != nil {
			t.Fatal(err)
		}
		var child string
		var at sql.NullString
		if err := f.s.db.QueryRow(f.s.q(`SELECT blocked_item_id,opened_at FROM schedule_gates WHERE workspace_id=?`), copy.ID).Scan(&child, &at); err != nil {
			t.Fatal(err)
		}
		if at != opened {
			t.Fatalf("bundle lost opening: %v", at)
		}
		v, err := f.s.ScheduleGates(copy.ID, child, models.CommitmentAccess{Readable: map[string]bool{child: true}})
		if err != nil || v.State != models.GateStateReleased {
			t.Fatalf("copy=%+v %v", v, err)
		}
	})
	for _, state := range []string{"failed", "discarded", "skipped", "lapsed"} {
		t.Run("unsuccessful/"+state, func(t *testing.T) {
			f := newLinkFix(t, 2)
			if state == "failed" || state == "discarded" {
				blocker := f.buildItem(t, "open")
				g := f.gate(t, f.items[0].ID, blocker.ID, f.gateAccess(blocker))
				fields := `{"status":"` + state + `"}`
				if _, err := f.s.UpdateItem(blocker.ID, models.ItemUpdate{Fields: &fields}); err != nil {
					t.Fatal(err)
				}
				if f.openedAt(t, g.ID).Valid {
					t.Fatal("unsuccessful item opened gate")
				}
				return
			}
			f.recurring(t, 0)
			g := f.gate(t, f.items[1].ID, f.items[0].ID, f.allEdit())
			if state == "skipped" {
				f.resolve(t, 0, "skip")
				f.resolve(t, 0, "done")
			} else {
				f.deadline(t, 0, "lapses")
				f.eval(t, farFuture, 0)
			}
			if f.openedAt(t, g.ID).Valid {
				t.Fatal("unsuccessful captured occurrence opened gate")
			}
		})
	}

}

func graphGateRecipients(t *testing.T, f *linkFix) *models.User {
	t.Helper()
	u := notifyUser(t, f.s, "gate-recipient@example.com")
	if err := f.s.AddWorkspaceMember(f.ws.ID, u.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	f.exec(t, `UPDATE workspaces SET owner_id=? WHERE id=?`, u.ID, f.ws.ID)
	addDevice(t, f.s, u.ID)
	return u
}

func graphFencedGateTest(t *testing.T) {
	f := newFenceFixture(t)
	blocker := createTestItem(t, f.s, f.ws.ID, f.companion.ID, "App blocker", "")
	child := createTestItem(t, f.s, f.ws.ID, f.other.ID, "Child", "")
	f.s.mustExec(t, `UPDATE items SET created_via_app=? WHERE id=?`, f.install, blocker.ID)
	acc := models.CommitmentAccess{Readable: map[string]bool{blocker.ID: true, child.ID: true}, Editable: map[string]bool{blocker.ID: true, child.ID: true}}
	g, err := f.s.CreateScheduleGate(f.ws.ID, child.ID, blocker.ID, acc)
	if err != nil {
		t.Fatal(err)
	}
	actor := notifyUser(t, f.s, "fenced-gate@example.com")
	key := []byte("gate-test-key")
	tx, err := f.s.BeginFenced(context.Background(), f.spec())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.UpdateItemFields(blocker.ID, map[string]any{"status": "done"}, AppItemETag(key, f.install, blocker.ID, blocker.Seq), key, FencedActor{Kind: "user", UserID: actor.ID}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var at sql.NullString
	if err := f.s.db.QueryRow(f.s.q(`SELECT opened_at FROM schedule_gates WHERE id=?`), g.ID).Scan(&at); err != nil {
		t.Fatal(err)
	}
	if !at.Valid {
		t.Fatal("fenced writer did not open gate")
	}
}
