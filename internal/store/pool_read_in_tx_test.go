package store

import (
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2778, second half. A read issued against the connection POOL from
// inside an open transaction needs a SECOND connection while the first is
// still held. Under a saturated pool that second connection never arrives,
// and the transaction cannot finish — so it never releases what it holds.
// The deadlock is in the application, not the database: no SQLSTATE names it,
// no lock timeout breaks it, and the request simply hangs.
//
// `uniqueSlugExcluding` was doing exactly that from two in-transaction
// callers (the item update, under the workspace seq and parent-children
// locks; the document rename, under the lock this bug added). It now takes
// the executor.
//
// THE INSTRUMENT: a one-connection pool. That makes the hazard deterministic
// rather than load-dependent — with a single connection, the pool is
// saturated by definition the moment a transaction is open, so a pool read
// inside one cannot ever succeed. Against the unfixed code these calls block
// until the test's timeout; with the fix they use the transaction they are
// already inside and return immediately.
func TestPoolReadInsideTransaction_RenameAndItemUpdateDoNotStall(t *testing.T) {
	// Runs on BOTH dialects. An earlier version skipped Postgres on the
	// theory that the instrument was SQLite-shaped, which left the
	// content-edit leg — the most reachable instance of the hazard — never
	// exercised against the pool the production deployment actually uses
	// (codex round 4). A one-connection pool is a property of database/sql,
	// not of either driver.
	s := testStore(t)
	ws := createTestWorkspace(t, s, "PoolRead")
	coll := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, coll.ID, "Item title", "")
	doc := createTestDoc(t, s, ws.ID, "Doc title", "body")

	// One connection: any pool read from inside a transaction now waits for a
	// connection that only the transaction itself could free.
	s.db.SetMaxOpenConns(1)

	for _, tc := range []struct {
		name string
		run  func() error
	}{
		{"document rename", func() error {
			title := "Doc title renamed"
			_, err := s.UpdateDocument(doc.ID, models.DocumentUpdate{Title: &title})
			return err
		}},
		{"item title update", func() error {
			title := "Item title renamed"
			_, err := s.UpdateItemWithParentLink(item.ID, models.ItemUpdate{Title: &title}, nil, nil)
			return err
		}},
		// A title-only item update never reaches the version check, so this
		// leg is what covers shouldCreateItemVersion — the instance the first
		// sweep missed precisely because the leg above looked like coverage
		// of "the item path" (codex round 7).
		{"item content edit", func() error {
			content := "edited item body"
			_, err := s.UpdateItemWithParentLink(item.ID, models.ItemUpdate{Content: &content}, nil, nil)
			return err
		}},
		// A FIELD update reaches doneFieldKey, which reads the collection —
		// an INDIRECT pool read that a grep for `s.db` inside transaction
		// bodies cannot see, and that neither leg above touches (codex round
		// 8). Three legs on the item path, each reaching a different read.
		{"item field update", func() error {
			fields := `{"status":"done"}`
			_, err := s.UpdateItemWithParentLink(item.ID, models.ItemUpdate{Fields: &fields}, nil, nil)
			return err
		}},
		// A MOVE resolves the done key against the TARGET collection through
		// the same helper, from a different transaction.
		{"item move", func() error {
			other := createTestCollection(t, s, ws.ID, "Archive")
			_, err := s.MoveItem(item.ID, other.ID, `{}`)
			return err
		}},
		// The version check is the MORE reachable instance of the same
		// hazard: it fires on any content edit, not only on a rename, and the
		// two legs above miss it entirely (codex round 3). A content-only
		// PATCH takes no rename lock and still runs a read inside the open
		// transaction.
		{"document content edit", func() error {
			body := "edited body"
			_, err := s.UpdateDocument(doc.ID, models.DocumentUpdate{Content: &body})
			return err
		}},
	} {
		done := make(chan error, 1)
		go func() { done <- tc.run() }()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("%s: %v", tc.name, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s stalled with a one-connection pool — a read inside the transaction went to the pool instead of the transaction", tc.name)
		}
	}
}
