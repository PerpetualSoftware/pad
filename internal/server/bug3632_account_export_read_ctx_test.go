package server

import (
	"context"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3632: the account export stopped WAITING for a stalled read at its
// deadline (BUG-3626, BUG-3659), but the read itself ran on under
// context.Background(), holding a goroutine and a pool connection until it
// returned. Every export read is now bound to the deadline, so the stalled
// query is cancelled and its connection goes back while the lock that stalled
// it is still held.
//
// Postgres only: a table lock is what stalls a read there. A SQLite read in
// WAL mode is not blocked by a writer; the store test covers the driver
// cancelling an endless read on both dialects.
func TestBUG3632_AStalledExportReadIsCancelledAndItsConnectionReturned(t *testing.T) {
	for _, tc := range []struct {
		name string
		// table holds the lock; the first export read naming it stalls.
		table string
		// before the body: the export answers 503 rather than aborting.
		preBody bool
	}{
		// GetUserWorkspacesQ, the first read.
		{"workspace listing", "workspaces", true},
		// The app attribution read, inside the snapshot: only it reads
		// app_installs for a human owner.
		{"snapshot", "app_installs", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := accountDeleteServer(t, store.DriverPostgres)
			tok := bug3626Owner(t, srv)
			srv.accountExportHooks.timeout = 300 * time.Millisecond
			db := srv.store.DB()
			// Warm up before locking: the first request from a new IP writes a
			// session-IP-change activity, which a lock on workspaces would
			// stall in the auth middleware, before the export starts.
			if code, body, err := bug3626Export(t, srv, tok); err != nil || code != 200 || !parsesAsCompleteExport(body) {
				t.Fatalf("warm-up export: %d %v\n%s", code, err, body)
			}

			lock, err := db.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatalf("begin lock tx: %v", err)
			}
			t.Cleanup(func() { _ = lock.Rollback() })
			if _, err := lock.Exec(`LOCK TABLE ` + tc.table + ` IN ACCESS EXCLUSIVE MODE`); err != nil {
				t.Fatalf("lock %s: %v", tc.table, err)
			}
			held := db.Stats().InUse // the lock's own connection

			start := time.Now()
			code, body, readErr := bug3626Export(t, srv, tok)
			if elapsed := time.Since(start); elapsed > 5*time.Second {
				t.Fatalf("the export waited %v on a lock on %s; the 300ms deadline must bound it", elapsed, tc.table)
			}
			if readErr == nil && parsesAsCompleteExport(body) {
				t.Fatalf("a stalled export arrived as a valid, complete-looking file:\n%s", body)
			}
			if tc.preBody && code != 503 {
				t.Errorf("a stall before the body should answer 503, got %d %s", code, body)
			}

			// The lock is still held. A read that ran on past the deadline
			// would still be waiting on it, holding its connection.
			deadline := time.Now().Add(3 * time.Second)
			for db.Stats().InUse > held {
				if time.Now().After(deadline) {
					t.Fatalf("%d connection(s) beyond the lock's still in use 3s after the export's deadline: the stalled read on %s was not cancelled",
						db.Stats().InUse-held, tc.table)
				}
				time.Sleep(20 * time.Millisecond)
			}
		})
	}
}
