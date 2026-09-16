package store

import (
	"context"
	"os"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2792, Postgres half. AddCreatedWorkspaceIfPermitted decides and writes
// in one statement. On Postgres that is not enough by itself: READ COMMITTED
// gives an INSERT ... SELECT a snapshot taken at statement start, and the
// foreign key's FOR KEY SHARE on the parent does not conflict with
// SetScopeFlags' non-key UPDATE. So a revocation can commit INSIDE the
// statement's lifetime without the statement noticing. FOR SHARE on the
// connection row is what serialises the two. Each test below constructs one
// ordering. "Blocked" is observed in pg_stat_activity (waitForLockWait) and is
// never inferred from elapsed time.
//
// SQLite is excluded: the statement runs under the database write lock there,
// so neither interleaving can be built. Its handler-level leg is
// TestAutoAddCreatorConnection_RevokedMidFlight_NotAdded.

// createdWorkspaceInsertNeedle is a fragment of the statement text that only
// AddCreatedWorkspaceIfPermitted's Postgres INSERT carries.
const createdWorkspaceInsertNeedle = "(SELECT c.request_id"

func pgOAuthConnFixture(t *testing.T) (s *Store, requestID, workspaceID string) {
	t.Helper()
	pgURL := os.Getenv("PAD_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set — the interleaving only exists on Postgres")
	}
	s = testStorePostgres(t, pgURL)
	u, err := s.CreateUser(models.UserCreate{
		Email: "bug2792@example.com", Name: "B", Password: "pw-bug2792-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	requestID = "req-bug2792"
	if err := s.CreateOAuthConnection(OAuthConnection{
		RequestID: requestID, UserID: u.ID, Name: "App", MayCreateWorkspaces: true,
	}); err != nil {
		t.Fatalf("CreateOAuthConnection: %v", err)
	}
	workspaceID, _ = seedWorkspaceForConn(t, s, "bug2792-ws")
	return s, requestID, workspaceID
}

func mustAllowed(t *testing.T, s *Store, requestID, workspaceID string) bool {
	t.Helper()
	ok, err := s.IsConnectionWorkspaceAllowed(requestID, workspaceID)
	if err != nil {
		t.Fatalf("IsConnectionWorkspaceAllowed: %v", err)
	}
	return ok
}

// Ordering 1: the revocation holds the connection row first. The insert must
// wait on that row rather than read around it. Once the revocation commits,
// the insert's WHERE clause is re-evaluated against the committed row and no
// longer matches.
func TestAddCreatedWorkspaceIfPermitted_PG_WaitsOnUncommittedRevocation(t *testing.T) {
	t.Parallel()
	s, requestID, workspaceID := pgOAuthConnFixture(t)

	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin revocation tx: %v", err)
	}
	defer tx.Rollback() // no-op after Commit
	if _, err := tx.Exec(s.q(
		`UPDATE oauth_connections SET may_create_workspaces = ? WHERE request_id = ?`,
	), false, requestID); err != nil {
		t.Fatalf("revoke in tx: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- s.AddCreatedWorkspaceIfPermitted(requestID, workspaceID) }()
	waitForLockWait(t, s, createdWorkspaceInsertNeedle, done)

	if err := tx.Commit(); err != nil {
		t.Fatalf("commit revocation: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("AddCreatedWorkspaceIfPermitted: %v", err)
	}
	if mustAllowed(t, s, requestID, workspaceID) {
		t.Error("workspace joined the allow-list although the revocation committed before the insert could proceed")
	}
}

// Control for ordering 1: the same wait on an uncommitted UPDATE of the same
// row, but one that leaves the flag alone. The insert must still land.
// Without this leg, a statement that never inserts would pass the test above.
func TestAddCreatedWorkspaceIfPermitted_PG_WaitsOnUnrelatedUpdateThenInserts(t *testing.T) {
	t.Parallel()
	s, requestID, workspaceID := pgOAuthConnFixture(t)

	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin rename tx: %v", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(s.q(
		`UPDATE oauth_connections SET name = ? WHERE request_id = ?`,
	), "Renamed", requestID); err != nil {
		t.Fatalf("rename in tx: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- s.AddCreatedWorkspaceIfPermitted(requestID, workspaceID) }()
	waitForLockWait(t, s, createdWorkspaceInsertNeedle, done)

	if err := tx.Commit(); err != nil {
		t.Fatalf("commit rename: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("AddCreatedWorkspaceIfPermitted: %v", err)
	}
	if !mustAllowed(t, s, requestID, workspaceID) {
		t.Error("workspace is not on the allow-list although creation power was never withdrawn")
	}
}

// Ordering 2: this is the race as filed, compressed into one statement. The
// insert has already taken its snapshot and read the flag as true. A
// revocation then arrives before the row is written. A BEFORE INSERT trigger
// pauses the statement at exactly that point, parked on an advisory lock the
// test holds.
//
// With FOR SHARE the revocation must WAIT for the insert to commit, so the
// row exists before the revocation returns. That is the same ordering as a
// creation that finished just before the user revoked. Without FOR SHARE the
// revocation commits while the insert is parked, and the insert then writes
// the row anyway. waitForLockWait on the revocation catches that, because the
// revocation completes instead of blocking.
func TestAddCreatedWorkspaceIfPermitted_PG_RevocationWaitsOnInFlightInsert(t *testing.T) {
	t.Parallel()
	s, requestID, workspaceID := pgOAuthConnFixture(t)
	ctx := context.Background()

	if _, err := s.db.Exec(`
		CREATE FUNCTION bug2792_pause() RETURNS trigger AS $$
		BEGIN
			PERFORM pg_advisory_lock(2792);
			PERFORM pg_advisory_unlock(2792);
			RETURN NEW;
		END $$ LANGUAGE plpgsql`); err != nil {
		t.Fatalf("create pause function: %v", err)
	}
	if _, err := s.db.Exec(`
		CREATE TRIGGER bug2792_pause BEFORE INSERT ON oauth_connection_workspaces
		FOR EACH ROW EXECUTE FUNCTION bug2792_pause()`); err != nil {
		t.Fatalf("create pause trigger: %v", err)
	}

	// A session-level advisory lock belongs to one connection, so hold a
	// dedicated one for both the lock and the unlock.
	holder, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatalf("acquire holder conn: %v", err)
	}
	defer holder.Close()
	if _, err := holder.ExecContext(ctx, `SELECT pg_advisory_lock(2792)`); err != nil {
		t.Fatalf("take advisory lock: %v", err)
	}
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		if _, err := holder.ExecContext(ctx, `SELECT pg_advisory_unlock(2792)`); err != nil {
			t.Errorf("release advisory lock: %v", err)
		}
	}
	defer release()

	insertDone := make(chan error, 1)
	go func() { insertDone <- s.AddCreatedWorkspaceIfPermitted(requestID, workspaceID) }()
	// Parked in the trigger: the snapshot is taken and the row passed the WHERE.
	waitForLockWait(t, s, createdWorkspaceInsertNeedle, insertDone)

	revokeDone := make(chan error, 1)
	go func() { revokeDone <- s.SetScopeFlags(requestID, false, false, false) }()
	waitForLockWait(t, s, "SET may_create_workspaces", revokeDone)

	release()
	if err := <-insertDone; err != nil {
		t.Fatalf("AddCreatedWorkspaceIfPermitted: %v", err)
	}
	if err := <-revokeDone; err != nil {
		t.Fatalf("SetScopeFlags: %v", err)
	}

	conn, err := s.GetOAuthConnection(requestID)
	if err != nil {
		t.Fatalf("GetOAuthConnection: %v", err)
	}
	if conn.MayCreateWorkspaces {
		t.Fatal("revocation did not land")
	}
	// The row is correct here. It was written before the revocation could
	// commit, and the assertion that carries this leg is the
	// waitForLockWait on the revocation above.
	if !mustAllowed(t, s, requestID, workspaceID) {
		t.Error("insert that held the row first did not land")
	}
}

// A missing connection is a silent no-op, not an FK error: the WHERE clause
// selects nothing, so no row reaches the foreign key.
func TestAddCreatedWorkspaceIfPermitted_MissingConnectionIsNoop(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	wsID, _ := seedWorkspaceForConn(t, s, "bug2792-missing")
	if err := s.AddCreatedWorkspaceIfPermitted("req-absent", wsID); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if mustAllowed(t, s, "req-absent", wsID) {
		t.Error("row written for a connection that does not exist")
	}
}

// Both dialects: flag off → no row; flag on → row with added_by=agent-create;
// a repeat insert is a no-op rather than an error.
func TestAddCreatedWorkspaceIfPermitted_FlagDecides(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	u, err := s.CreateUser(models.UserCreate{
		Email: "bug2792-flag@example.com", Name: "F", Password: "pw-bug2792-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	for _, c := range []struct {
		req   string
		may   bool
		wants bool
	}{{"req-flag-on", true, true}, {"req-flag-off", false, false}} {
		if err := s.CreateOAuthConnection(OAuthConnection{
			RequestID: c.req, UserID: u.ID, Name: c.req, MayCreateWorkspaces: c.may,
		}); err != nil {
			t.Fatalf("CreateOAuthConnection(%s): %v", c.req, err)
		}
		wsID, _ := seedWorkspaceForConn(t, s, "bug2792-"+c.req)
		for i := 0; i < 2; i++ {
			if err := s.AddCreatedWorkspaceIfPermitted(c.req, wsID); err != nil {
				t.Fatalf("%s attempt %d: %v", c.req, i, err)
			}
		}
		if got := mustAllowed(t, s, c.req, wsID); got != c.wants {
			t.Errorf("%s: allowed = %v, want %v", c.req, got, c.wants)
		}
		if c.wants {
			var addedBy string
			if err := s.db.QueryRow(s.q(
				`SELECT added_by FROM oauth_connection_workspaces WHERE request_id = ? AND workspace_id = ?`,
			), c.req, wsID).Scan(&addedBy); err != nil {
				t.Fatalf("read added_by: %v", err)
			}
			if addedBy != AddedByAgentCreate {
				t.Errorf("added_by = %q, want %q", addedBy, AddedByAgentCreate)
			}
		}
	}
}
