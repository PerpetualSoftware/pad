package store

import (
	"errors"
	"os"
	"testing"
	"time"
)

func TestOneShotReminderConcurrentLifecyclePostgres(t *testing.T) {
	url := os.Getenv("PAD_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set")
	}
	for _, action := range []string{"rearm", "ack", "delete", "archive"} {
		t.Run(action, func(t *testing.T) {
			s := testStorePostgres(t, url)
			ws := createTestWorkspace(t, s, "Concurrent reminders")
			col := createTestCollection(t, s, ws.ID, "Tasks")
			item := createTestItem(t, s, ws.ID, col.ID, "Run once", "")
			id := armReminder(t, s, ws.ID, item.ID, past)
			holder, err := s.db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer holder.Rollback()
			if _, err := holder.Exec(s.q(`SELECT 1 FROM items WHERE id=? FOR UPDATE`), item.ID); err != nil {
				t.Fatal(err)
			}
			fireDone := make(chan error, 1)
			writeDone := make(chan error, 1)
			go func() { _, err := s.fireOneReminder(id, nowTS()); fireDone <- err }()
			waitForLockWait(t, s, "FOR NO KEY UPDATE OF", fireDone)
			go func() {
				var err error
				switch action {
				case "rearm":
					_, err = s.RearmReminder(ws.ID, id, future)
				case "ack":
					_, err = s.AckReminder(ws.ID, id)
				case "delete":
					_, err = s.DeleteReminder(ws.ID, id)
				case "archive":
					_, err = s.db.Exec(s.q(`UPDATE items SET deleted_at=? WHERE id=?`), now(), item.ID)
				}
				writeDone <- err
			}()
			if err := holder.Commit(); err != nil {
				t.Fatal(err)
			}
			for _, done := range []chan error{fireDone, writeDone} {
				select {
				case err := <-done:
					if err != nil && !errors.Is(err, ErrOccurrenceNotFired) && !errors.Is(err, ErrScheduleItemGone) {
						t.Fatal(err)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("concurrent lifecycle did not finish")
				}
			}
			if n := s.countRows(t, `SELECT COUNT(*) FROM event_outbox WHERE event_type='item.reminder_due'`); n > 1 {
				t.Fatalf("duplicate fire: %d", n)
			}
			r, err := s.GetReminder(ws.ID, id)
			if err != nil {
				t.Fatal(err)
			}
			if action == "rearm" && (r == nil || r.RemindAt != future || !r.Armed()) {
				t.Fatalf("rearm lost: %+v", r)
			}
			if action == "delete" && r != nil {
				t.Fatal("delete lost")
			}
		})
	}
}
