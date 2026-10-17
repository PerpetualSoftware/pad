package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// BUG-3632: Query and QueryRow on a *sql.DB or *sql.Tx run the driver call
// under context.Background(), so a read that never finishes was never
// cancelled and kept its connection. WithContext hands the driver the
// caller's context instead.

// bug3632EndlessQuery never returns on its own: an unbounded recursive CTE on
// SQLite, a long sleep on Postgres.
func bug3632EndlessQuery(s *Store) string {
	if s.dialect.Driver() == DriverPostgres {
		return `SELECT 1 FROM pg_sleep(60)`
	}
	return `WITH RECURSIVE r(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM r) SELECT MAX(n) FROM r`
}

// bug3632AwaitIdle waits for the pool to have no connection in use, and
// fails if one is still held after 3s.
func bug3632AwaitIdle(t *testing.T, s *Store, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for s.db.Stats().InUse != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%s: %d connection(s) still in use 3s after the deadline; the read was not cancelled", what, s.db.Stats().InUse)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestBUG3632_WithContextCancelsAReadAtTheDeadline(t *testing.T) {
	s := testStore(t)
	query := bug3632EndlessQuery(s)

	for _, tc := range []struct {
		name string
		run  func(ctx context.Context) error
	}{
		{"pool Query", func(ctx context.Context) error {
			rows, err := WithContext(ctx, s.db).Query(query)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
			}
			return rows.Err()
		}},
		{"pool QueryRow", func(ctx context.Context) error {
			var n any
			return s.QContext(ctx).QueryRow(query).Scan(&n)
		}},
		{"snapshot QueryRow", func(ctx context.Context) error {
			tx, err := s.BeginReadSnapshot(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			var n any
			return WithContext(ctx, tx).QueryRow(query).Scan(&n)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- tc.run(ctx) }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("an endless read returned no error; it should fail at the deadline")
				}
				// The export reads a timeout by errors.Is, so a driver error
				// that did not wrap the context's would answer 500, not 503.
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("a read cancelled at the deadline returned %T %v, which does not wrap context.DeadlineExceeded", err, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the read ran 5s past a 200ms deadline; it was not cancelled")
			}
			bug3632AwaitIdle(t, s, tc.name)
		})
	}
}
