package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4278: the session path and account deletion run under the
// request's context. All of it against a REAL Postgres (pgOnly asserts the
// backend with SHOW server_version); effects are counted with raw SQL.

// lockSessionsTable holds ACCESS EXCLUSIVE on sessions, so any read of it
// blocks until the returned release runs. A backstop releases it after 4 s so
// a regression fails the test instead of hanging it.
func lockSessionsTable(t *testing.T, s *Store) (release func()) {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`LOCK TABLE sessions IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	timer := time.AfterFunc(4*time.Second, func() { _ = tx.Rollback() })
	return func() { timer.Stop(); _ = tx.Rollback() }
}

func TestSessionContext_CancelStopsValidate(t *testing.T) {
	t.Parallel()
	s := pgOnly(t)
	u, err := s.CreateUser(models.UserCreate{Email: "sv@test.com", Name: "SV", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := s.CreateSession(u.ID, "web", "127.0.0.1", "ua", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	release := lockSessionsTable(t, s)
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	info, err := s.ValidateSessionContext(ctx, tok)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("ValidateSessionContext took %s after a cancel at 200ms: the query was not stopped", elapsed)
	}
	if info != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("got (%v, %v), want (nil, context canceled)", info, err)
	}
	release()
	if info, err := s.ValidateSessionContext(context.Background(), tok); err != nil || info == nil {
		t.Fatalf("control: after the lock is gone Validate = (%v, %v), want the session", info, err)
	}
}

func TestSessionContext_CancelStopsRenewUpdate(t *testing.T) {
	t.Parallel()
	s := pgOnly(t)
	u, err := s.CreateUser(models.UserCreate{Email: "sr@test.com", Name: "SR", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := s.CreateSession(u.ID, "web", "127.0.0.1", "ua", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// Past the renewal threshold (under half of the 1 h window left), so the
	// renew reaches its UPDATE.
	if _, err := s.db.Exec(`UPDATE sessions SET expires_at = $1 WHERE user_id = $2`,
		time.Now().UTC().Add(5*time.Minute).Format(time.RFC3339), u.ID); err != nil {
		t.Fatal(err)
	}
	// A row lock the UPDATE waits on; the renew's SELECT is not blocked by it.
	holder, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	timer := time.AfterFunc(4*time.Second, func() { _ = holder.Rollback() })
	defer timer.Stop()
	defer holder.Rollback()
	if _, err := holder.Exec(`SELECT 1 FROM sessions WHERE user_id = $1 FOR UPDATE`, u.ID); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	_, renewed, err := s.RenewSessionIfStaleContext(ctx, tok)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("RenewSessionIfStaleContext took %s after a cancel at 200ms: the UPDATE was not stopped", elapsed)
	}
	if renewed || !errors.Is(err, context.Canceled) {
		t.Fatalf("got (renewed=%v, %v), want (false, context canceled)", renewed, err)
	}
}

// deleteFixture is a user who owns a workspace with an item they authored, and
// holds a session: everything the deletion's early writes touch.
type deleteFixture struct {
	userID, wsID, itemID string
}

func newDeleteFixture(t *testing.T, s *Store) deleteFixture {
	t.Helper()
	u, err := s.CreateUser(models.UserCreate{Email: "del@test.com", Name: "Del", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatal(err)
	}
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Del WS", OwnerID: u.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddWorkspaceMember(ws.ID, u.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	coll := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, coll.ID, "Authored", "")
	if _, err := s.db.Exec(`UPDATE items SET created_by_user_id = $1 WHERE id = $2`, u.ID, item.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSession(u.ID, "web", "127.0.0.1", "ua", time.Hour); err != nil {
		t.Fatal(err)
	}
	return deleteFixture{userID: u.ID, wsID: ws.ID, itemID: item.ID}
}

// holdItemRow locks the authored item's row in another transaction, which the
// deletion's "detach authored items" statement then waits on, AFTER its
// earlier writes (the workspace soft-delete among them) have been made.
func holdItemRow(t *testing.T, s *Store, itemID string, backstop time.Duration) (release func()) {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`SELECT 1 FROM items WHERE id = $1 FOR UPDATE`, itemID); err != nil {
		t.Fatal(err)
	}
	timer := time.AfterFunc(backstop, func() { _ = tx.Rollback() })
	return func() { timer.Stop(); _ = tx.Rollback() }
}

// assertNothingDeleted counts, with raw SQL, that the whole deletion rolled
// back: the user, the session, the membership, the authored attribution, and
// the workspace still live.
func assertNothingDeleted(t *testing.T, s *Store, f deleteFixture) {
	t.Helper()
	for _, c := range []struct {
		what, q string
		want    int
	}{
		{"users", `SELECT count(*) FROM users WHERE id = $1`, 1},
		{"sessions", `SELECT count(*) FROM sessions WHERE user_id = $1`, 1},
		{"memberships", `SELECT count(*) FROM workspace_members WHERE user_id = $1`, 1},
		{"items still attributed to the user", `SELECT count(*) FROM items WHERE created_by_user_id = $1`, 1},
		{"live owned workspaces", `SELECT count(*) FROM workspaces WHERE owner_id = $1 AND deleted_at IS NULL`, 1},
	} {
		if got := pgCount(t, s, c.q, f.userID); got != c.want {
			t.Errorf("%s = %d after the rolled-back deletion, want %d", c.what, got, c.want)
		}
	}
}

func TestDeleteAccountCtx_DeadlineRollsBack(t *testing.T) {
	t.Parallel()
	s := pgOnly(t)
	f := newDeleteFixture(t, s)
	release := holdItemRow(t, s, f.itemID, 6*time.Second)
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := s.DeleteAccountAtomicReportCtx(ctx, f.userID)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("deletion with a 600ms deadline ran %s", elapsed)
	}
	if err == nil || !IsDBBusy(err) {
		t.Fatalf("err = %v, want a retryable busy outcome (IsDBBusy)", err)
	}
	release()
	assertNothingDeleted(t, s, f)
}

func TestDeleteAccountCtx_CancelStopsBlockedStatement(t *testing.T) {
	t.Parallel()
	s := pgOnly(t)
	f := newDeleteFixture(t, s)
	release := holdItemRow(t, s, f.itemID, 6*time.Second)
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	start := time.Now()
	err := func() error { _, e := s.DeleteAccountAtomicReportCtx(ctx, f.userID); return e }()
	// 45 s statement/lock timeouts apply here, so only the ctx reaching the
	// blocked statement can end it this fast.
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("a cancelled deletion kept running for %s", elapsed)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context canceled", err)
	}
	release()
	assertNothingDeleted(t, s, f)
}

// TestDeleteAccountCtx_NoRetryOnceDone: a deadlock retry loop must not run
// another attempt, or sleep, once the context is done.
func TestDeleteAccountCtx_NoRetryOnceDone(t *testing.T) {
	t.Parallel()
	s := testStoreSQLite(t)
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	err := s.retryOnDeadlockCtx(ctx, "t", func() error {
		attempts++
		cancel()
		return errInjectedDeadlock
	})
	if attempts != 1 || !errors.Is(err, errInjectedDeadlock) {
		t.Fatalf("attempts = %d, err = %v; want exactly 1 attempt and the deadlock error", attempts, err)
	}
	// Control: a live context still retries.
	attempts = 0
	_ = s.retryOnDeadlockCtx(context.Background(), "t", func() error { attempts++; return errInjectedDeadlock })
	if attempts != deadlockRetryAttempts {
		t.Fatalf("control attempts = %d, want %d", attempts, deadlockRetryAttempts)
	}
}

func TestDeleteAccountCtx_WaitsOutPurge(t *testing.T) {
	t.Parallel()
	// The runtime bounds: a plain statement would die at 300 ms. The deletion
	// raises its own, so it waits out a purge-style holder that outlasts them.
	s := pgBounded(t, RuntimeLimits{Statement: 30 * time.Second, Lock: rtBound})
	f := newDeleteFixture(t, s)
	release := holdItemRow(t, s, f.itemID, 2*time.Second) // scaled stand-in for the ~43 s purge hold
	defer release()

	// Control: the same wait on a plain bounded statement fails.
	_, ctrlErr := s.db.Exec(`UPDATE items SET created_by_user_id = NULL WHERE created_by_user_id = $1`, f.userID)
	if pgErrCode(ctrlErr) != "55P03" {
		t.Fatalf("control: plain statement behind the holder = %v, want 55P03", ctrlErr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	if _, err := s.DeleteAccountAtomicReportCtx(ctx, f.userID); err != nil {
		t.Fatalf("deletion behind a 2 s holder: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 1500*time.Millisecond {
		t.Fatalf("deletion took %s: it did not wait for the holder", elapsed)
	}
	for _, q := range []string{
		`SELECT count(*) FROM users WHERE id = $1`,
		`SELECT count(*) FROM sessions WHERE user_id = $1`,
		`SELECT count(*) FROM workspace_members WHERE user_id = $1`,
		`SELECT count(*) FROM items WHERE created_by_user_id = $1`,
		`SELECT count(*) FROM workspaces WHERE owner_id = $1 AND deleted_at IS NULL`,
	} {
		if n := pgCount(t, s, q, f.userID); n != 0 {
			t.Errorf("%s = %d after the deletion, want 0", q, n)
		}
	}
}
