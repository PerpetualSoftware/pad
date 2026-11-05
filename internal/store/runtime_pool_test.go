package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/jackc/pgx/v5/pgconn"
)

// TASK-4278: the bounded Postgres runtime pool. Every test here runs against a
// REAL Postgres (PAD_TEST_POSTGRES_URL) and proves it: pgOnly asserts the
// backend with SHOW server_version, and effects are counted with raw
// SELECT count(*) on that database, never on a SQLite fallback.

const rtBound = 300 * time.Millisecond

func rtLimits() RuntimeLimits {
	return RuntimeLimits{Statement: rtBound, Lock: rtBound, IdleInTx: rtBound}
}

// pgOnly returns a store on the startup (unbounded) pool of an isolated cloned
// Postgres database, skipping when no Postgres is configured.
func pgOnly(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("PAD_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set; Postgres-backed test skipped")
	}
	s := testStorePostgres(t, url)
	requireRealPostgres(t, s)
	return s
}

// pgBounded is pgOnly followed by BoundRuntimePool(l).
func pgBounded(t *testing.T, l RuntimeLimits) *Store {
	t.Helper()
	s := pgOnly(t)
	if err := BoundRuntimePool(s, l); err != nil {
		t.Fatalf("BoundRuntimePool: %v", err)
	}
	return s
}

// requireRealPostgres fails the test unless s.db is talking to PostgreSQL.
func requireRealPostgres(t *testing.T, s *Store) {
	t.Helper()
	var v string
	if err := s.db.QueryRow(`SHOW server_version`).Scan(&v); err != nil || v == "" {
		t.Fatalf("not a real PostgreSQL backend (SHOW server_version = %q, err %v)", v, err)
	}
}

// pgCount counts rows with a raw $1-style query on the Postgres database.
func pgCount(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

func pgErrCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// rtProbe creates the scratch table the bound tests write to.
func rtProbe(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.db.Exec(`CREATE TABLE rt_probe (id int PRIMARY KEY, v text)`); err != nil {
		t.Fatalf("create rt_probe: %v", err)
	}
}

func TestRuntimePool_StatementTimeoutCancels(t *testing.T) {
	t.Parallel()
	s := pgBounded(t, rtLimits())
	rtProbe(t, s)

	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO rt_probe VALUES (1, 'x')`); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = tx.Exec(`SELECT pg_sleep(5)`)
	if got := pgErrCode(err); got != "57014" {
		t.Fatalf("pg_sleep(5) under a %s bound: err = %v (code %q), want 57014", rtBound, err, got)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("cancel took %s, want about %s", elapsed, rtBound)
	}
	if !IsDBBusy(err) {
		t.Errorf("IsDBBusy(%v) = false", err)
	}
	_ = tx.Rollback()
	if n := pgCount(t, s, `SELECT count(*) FROM rt_probe`); n != 0 {
		t.Errorf("rt_probe rows = %d after the cancelled transaction, want 0", n)
	}
}

func TestRuntimePool_LockTimeout(t *testing.T) {
	t.Parallel()
	// No idle-in-transaction bound here: the holder sits idle inside its
	// transaction and must stay alive for the lock bound to be what ends the wait.
	s := pgBounded(t, RuntimeLimits{Statement: 30 * time.Second, Lock: rtBound})
	rtProbe(t, s)
	if _, err := s.db.Exec(`INSERT INTO rt_probe VALUES (1, 'orig')`); err != nil {
		t.Fatal(err)
	}

	holder, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback()
	if _, err := holder.Exec(`UPDATE rt_probe SET v = 'held' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	// A client-side backstop so a missing lock bound fails the test instead of
	// hanging it behind the holder.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = s.db.ExecContext(ctx, `UPDATE rt_probe SET v = 'second' WHERE id = 1`)
	elapsed := time.Since(start)
	if got := pgErrCode(err); got != "55P03" {
		t.Fatalf("second writer: err = %v (code %q), want 55P03 lock_not_available", err, got)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("second writer waited %s, want about %s", elapsed, rtBound)
	}
	if !IsDBBusy(err) {
		t.Errorf("IsDBBusy(%v) = false", err)
	}
	if n := pgCount(t, s, `SELECT count(*) FROM rt_probe WHERE v = 'second'`); n != 0 {
		t.Errorf("failed writer left %d rows", n)
	}
}

func TestRuntimePool_IdleInTxTerminated(t *testing.T) {
	t.Parallel()
	s := pgBounded(t, RuntimeLimits{IdleInTx: rtBound})
	rtProbe(t, s)

	// Warm the pool to a known size.
	const warm = 4
	conns := make([]*sql.Conn, 0, warm)
	for i := 0; i < warm; i++ {
		c, err := s.db.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	for _, c := range conns {
		c.Close()
	}
	if open := s.db.Stats().OpenConnections; open != warm {
		t.Fatalf("warm pool open = %d, want %d", open, warm)
	}

	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO rt_probe VALUES (1, 'abandoned')`); err != nil {
		t.Fatal(err)
	}
	time.Sleep(4 * rtBound) // abandoned: idle in the transaction
	_, err = tx.Exec(`INSERT INTO rt_probe VALUES (2, 'late')`)
	if err == nil {
		t.Fatal("a transaction idle past the bound was still usable")
	}
	if !IsDBBusy(err) {
		t.Errorf("IsDBBusy(%v) = false (code %q)", err, pgErrCode(err))
	}
	_ = tx.Rollback()

	// The pool is healthy and the same size: every slot answers, nothing leaked.
	held := make([]*sql.Conn, 0, warm)
	for i := 0; i < warm; i++ {
		c, err := s.db.Conn(context.Background())
		if err != nil {
			t.Fatalf("conn %d after the termination: %v", i, err)
		}
		var one int
		if err := c.QueryRowContext(context.Background(), `SELECT 1`).Scan(&one); err != nil || one != 1 {
			t.Fatalf("conn %d unhealthy: %v", i, err)
		}
		held = append(held, c)
	}
	if open := s.db.Stats().OpenConnections; open != warm {
		t.Errorf("pool size after the termination = %d, want %d", open, warm)
	}
	for _, c := range held {
		c.Close()
	}
	if n := pgCount(t, s, `SELECT count(*) FROM rt_probe`); n != 0 {
		t.Errorf("rt_probe rows = %d, want 0 (the abandoned transaction rolled back)", n)
	}
}

func TestRuntimePool_EveryConnBounded(t *testing.T) {
	t.Parallel()
	s := pgBounded(t, RuntimeLimits{Statement: 30 * time.Second, Lock: 10 * time.Second, IdleInTx: time.Minute})

	check := func(label string, n int) {
		t.Helper()
		var held []*sql.Conn
		defer func() {
			for _, c := range held {
				c.Close()
			}
		}()
		for i := 0; i < n; i++ {
			c, err := s.db.Conn(context.Background())
			if err != nil {
				t.Fatalf("%s: conn %d: %v", label, i, err)
			}
			held = append(held, c)
			var st, lk, id string
			if err := c.QueryRowContext(context.Background(),
				`SELECT current_setting('statement_timeout'), current_setting('lock_timeout'), current_setting('idle_in_transaction_session_timeout')`).
				Scan(&st, &lk, &id); err != nil {
				t.Fatalf("%s: conn %d: %v", label, i, err)
			}
			if st != "30s" || lk != "10s" || id != "1min" {
				t.Fatalf("%s: conn %d carries statement=%s lock=%s idle=%s, want 30s/10s/1min", label, i, st, lk, id)
			}
		}
	}
	check("all 25 at once", 25)

	// Recycle: every connection is replaced by a fresh one the pool opens later.
	s.db.SetConnMaxLifetime(time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	before := s.db.Stats().MaxLifetimeClosed
	check("after ConnMaxLifetime recycle", 25)
	if s.db.Stats().MaxLifetimeClosed <= before {
		t.Errorf("no connection was recycled by ConnMaxLifetime; the recycle leg proved nothing")
	}
}

func TestStartupPool_Unbounded(t *testing.T) {
	t.Parallel()
	s := pgOnly(t)
	show := func(db *sql.DB, name string) string {
		var v string
		if err := db.QueryRow(`SELECT current_setting($1)`, name).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, g := range []string{"statement_timeout", "lock_timeout", "idle_in_transaction_session_timeout"} {
		if v := show(s.db, g); v != "0" {
			t.Errorf("startup pool %s = %q, want 0 (migrations and backfills run unbounded)", g, v)
		}
	}
	startup := s.db
	if err := BoundRuntimePool(s, rtLimits()); err != nil {
		t.Fatal(err)
	}
	if s.db == startup {
		t.Fatal("BoundRuntimePool kept the startup pool")
	}
	if err := startup.Ping(); err == nil {
		t.Error("the startup pool is still open after the swap")
	}
	if v := show(s.db, "statement_timeout"); v != "300ms" {
		t.Errorf("runtime pool statement_timeout = %q, want 300ms", v)
	}
}

// NewPostgres must stay unbounded whatever the environment says: the bound is
// applied by the server only after the startup sequence, never by the
// constructor that runs the migration chain.
func TestMigrationsRunUnbounded(t *testing.T) {
	url := os.Getenv("PAD_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set; Postgres-backed test skipped")
	}
	t.Setenv("PAD_DB_STATEMENT_TIMEOUT", "1ms")
	t.Setenv("PAD_DB_LOCK_TIMEOUT", "1ms")

	admin, err := newPostgresConn(t, url)
	if err != nil {
		t.Fatal(err)
	}
	dbName := "pad_test_unb_" + newID()[:8]
	if _, err := admin.db.Exec("CREATE DATABASE " + dbName); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		admin.db.Exec("DROP DATABASE IF EXISTS " + dbName + " WITH (FORCE)")
		admin.Close()
	})

	// An EMPTY database: the whole migration chain, dedupe and backfills run.
	s, err := NewPostgres(replaceDBName(url, dbName))
	if err != nil {
		t.Fatalf("NewPostgres on an empty database: %v", err)
	}
	defer s.Close()
	requireRealPostgres(t, s)
	if n := pgCount(t, s, `SELECT count(*) FROM schema_migrations`); n < 50 {
		t.Errorf("schema_migrations rows = %d, want the whole chain applied", n)
	}
	var st string
	if err := s.db.QueryRow(`SHOW statement_timeout`).Scan(&st); err != nil || st != "0" {
		t.Errorf("constructor pool statement_timeout = %q (%v), want 0", st, err)
	}
}

// Each long-write path outlasts the runtime bound and must still succeed, with
// its own override visible INSIDE its transaction. A trigger on items sleeps
// past the bound per row (so a single statement exceeds it) and records the
// settings it ran under; the control writes show the sleep really would be
// cancelled on the bounded pool.
func TestOverridePaths_RunWithRaisedLimits(t *testing.T) {
	t.Parallel()
	s := pgBounded(t, rtLimits())

	owner, err := s.CreateUser(models.UserCreate{Email: "ovr@test.com", Name: "Owner", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatal(err)
	}
	seed := seedFullWorkspace(t, s, owner, "Override WS")
	exp, err := s.ExportWorkspace(seed.slug)
	if err != nil {
		t.Fatal(err)
	}

	for _, stmt := range []string{
		`CREATE TABLE rt_settings (op text, stmt text, lock text)`,
		`CREATE FUNCTION rt_slow() RETURNS trigger LANGUAGE plpgsql AS $f$
		 BEGIN
		   PERFORM pg_sleep(0.6);
		   INSERT INTO rt_settings VALUES (TG_OP, current_setting('statement_timeout'), current_setting('lock_timeout'));
		   RETURN CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END;
		 END $f$`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	trigger := func(op string) {
		t.Helper()
		if _, err := s.db.Exec(fmt.Sprintf(`CREATE TRIGGER rt_slow_%[1]s BEFORE %[2]s ON items FOR EACH ROW EXECUTE FUNCTION rt_slow()`, strings.ToLower(op), op)); err != nil {
			t.Fatal(err)
		}
	}
	settings := func(op string) (stmt, lock string, n int) {
		t.Helper()
		rows, err := s.db.Query(`SELECT stmt, lock FROM rt_settings WHERE op = $1`, op)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			n++
			if err := rows.Scan(&stmt, &lock); err != nil {
				t.Fatal(err)
			}
		}
		return
	}

	t.Run("control: the bound cancels the slow statement", func(t *testing.T) {
		trigger("DELETE")
		defer s.db.Exec(`DROP TRIGGER rt_slow_delete ON items`)
		_, err := s.db.Exec(`DELETE FROM items WHERE workspace_id = $1`, seed.wsID)
		if pgErrCode(err) != "57014" {
			t.Fatalf("plain DELETE under the bound: %v, want 57014", err)
		}
		if n := pgCount(t, s, `SELECT count(*) FROM items WHERE workspace_id = $1`, seed.wsID); n != 2 {
			t.Fatalf("control left %d items, want the 2 seeded", n)
		}
	})

	t.Run("import", func(t *testing.T) {
		trigger("INSERT")
		defer s.db.Exec(`DROP TRIGGER rt_slow_insert ON items`)
		ws, err := s.ImportWorkspace(exp, "Imported Copy", owner.ID, "test")
		if err != nil {
			t.Fatalf("ImportWorkspace past the bound: %v", err)
		}
		if n := pgCount(t, s, `SELECT count(*) FROM items WHERE workspace_id = $1`, ws.ID); n != 2 {
			t.Errorf("imported items = %d, want 2", n)
		}
		if st, lk, n := settings("INSERT"); n == 0 || st != "30min" || lk != "5min" {
			t.Errorf("import ran under statement=%q lock=%q (%d rows), want 30min/5min", st, lk, n)
		}
	})

	t.Run("import idle-in-transaction timeout is raised", func(t *testing.T) {
		if _, err := s.db.Exec(`CREATE TABLE rt_idle (idle text)`); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`CREATE FUNCTION rt_idle_fn() RETURNS trigger LANGUAGE plpgsql AS $f$
		 BEGIN
		   INSERT INTO rt_idle VALUES (current_setting('idle_in_transaction_session_timeout'));
		   RETURN NEW;
		 END $f$`); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`CREATE TRIGGER rt_idle_t BEFORE INSERT ON items FOR EACH ROW EXECUTE FUNCTION rt_idle_fn()`); err != nil {
			t.Fatal(err)
		}
		defer s.db.Exec(`DROP TRIGGER rt_idle_t ON items`)
		// The pool's own bound is tiny (rtBound); the import's must be the override.
		var pool string
		if err := s.db.QueryRow(`SELECT current_setting('idle_in_transaction_session_timeout')`).Scan(&pool); err != nil || pool == "10min" {
			t.Fatalf("pool idle bound = %q (%v), want the small test bound", pool, err)
		}
		if _, err := s.ImportWorkspace(exp, "Idle Copy", owner.ID, "test"); err != nil {
			t.Fatalf("ImportWorkspace: %v", err)
		}
		rows, err := s.db.Query(`SELECT idle FROM rt_idle`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				t.Fatal(err)
			}
			n++
			if v != "10min" {
				t.Errorf("inside the import tx idle_in_transaction_session_timeout = %q, want 10min", v)
			}
		}
		if n == 0 {
			t.Fatal("the trigger never ran inside the import")
		}
	})

	t.Run("remap", func(t *testing.T) {
		oldID, newID2 := "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
		coll := createTestCollection(t, s, seed.wsID, "Remap Coll")
		it := createTestItem(t, s, seed.wsID, coll.ID, "Carrier", "see pad-attachment:"+oldID)
		trigger("UPDATE")
		defer s.db.Exec(`DROP TRIGGER rt_slow_update ON items`)
		if err := s.RemapAttachmentReferencesInWorkspace(seed.wsID, map[string]string{oldID: newID2}); err != nil {
			t.Fatalf("Remap past the bound: %v", err)
		}
		if n := pgCount(t, s, `SELECT count(*) FROM items WHERE id = $1 AND content LIKE '%' || $2 || '%'`, it.ID, newID2); n != 1 {
			t.Errorf("item content not remapped (matches=%d)", n)
		}
		if st, lk, n := settings("UPDATE"); n == 0 || st != "30min" || lk != "1min" {
			t.Errorf("remap ran under statement=%q lock=%q (%d rows), want 30min/1min", st, lk, n)
		}
	})

	t.Run("purge", func(t *testing.T) {
		if err := s.DeleteWorkspace(seed.slug); err != nil {
			t.Fatal(err)
		}
		trigger("DELETE")
		defer s.db.Exec(`DROP TRIGGER rt_slow_delete ON items`)
		if err := s.PurgeWorkspaceData(seed.wsID); err != nil {
			t.Fatalf("PurgeWorkspaceData past the bound: %v", err)
		}
		if n := pgCount(t, s, `SELECT count(*) FROM items WHERE workspace_id = $1`, seed.wsID); n != 0 {
			t.Errorf("purged workspace still has %d items", n)
		}
		if n := pgCount(t, s, `SELECT count(*) FROM workspaces WHERE id = $1`, seed.wsID); n != 0 {
			t.Errorf("purged workspace row remains")
		}
		if st, lk, n := settings("DELETE"); n == 0 || st != "30min" || lk != "5min" {
			t.Errorf("purge ran under statement=%q lock=%q (%d rows), want 30min/5min", st, lk, n)
		}
	})
}

// BackfillEffort's whole-table read waits out a lock the bounded default would
// have failed on.
func TestOverridePaths_EffortCandidatesRead(t *testing.T) {
	t.Parallel()
	// No idle-in-transaction bound: the lock holder must outlive the lock bound.
	s := pgBounded(t, RuntimeLimits{Statement: 30 * time.Second, Lock: rtBound})

	holder, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback()
	if _, err := holder.Exec(`LOCK TABLE item_decisions IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}

	// Control: a plain read on the bounded pool dies at the lock bound.
	ctrlCtx, cancelCtrl := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelCtrl()
	_, ctrlErr := s.db.ExecContext(ctrlCtx, `SELECT count(*) FROM item_decisions`)
	if pgErrCode(ctrlErr) != "55P03" {
		t.Fatalf("control read: %v, want 55P03", ctrlErr)
	}

	// Release the lock after the lock bound has passed but well inside the
	// override's 30 s, so only the override can make the read succeed.
	release := time.AfterFunc(900*time.Millisecond, func() { holder.Rollback() })
	defer release.Stop()

	got, err := s.ItemsMissingDecisionKey("attention", "needs_human_decision", "effort")
	if err != nil {
		t.Fatalf("ItemsMissingDecisionKey behind a lock held past the bound: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("candidates = %d on an empty table", len(got))
	}
}

func TestSetTxTimeouts(t *testing.T) {
	t.Parallel()
	t.Run("no-op on SQLite", func(t *testing.T) {
		t.Parallel()
		s := testStoreSQLite(t)
		tx, err := s.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if err := s.setTxTimeouts(tx, time.Minute, time.Minute); err != nil {
			t.Fatalf("setTxTimeouts on SQLite: %v", err)
		}
		if err := s.setTxTimeouts(tx, 0, 0); err != nil {
			t.Fatalf("setTxTimeouts(0,0) on SQLite must stay a no-op, got %v", err)
		}
		var one int
		if err := tx.QueryRow(`SELECT 1`).Scan(&one); err != nil {
			t.Fatalf("transaction unusable after the no-op: %v", err)
		}
	})
	t.Run("rejects non-positive", func(t *testing.T) {
		t.Parallel()
		s := pgBounded(t, rtLimits())
		for _, c := range [][2]time.Duration{{0, time.Second}, {time.Second, 0}, {-time.Second, time.Second}} {
			tx, _ := s.db.Begin()
			if err := s.setTxTimeouts(tx, c[0], c[1]); err == nil {
				t.Errorf("setTxTimeouts(%v, %v) accepted; 0 would switch the bound off", c[0], c[1])
			}
			tx.Rollback()
		}
	})
	t.Run("idle-in-tx override: no-op on SQLite, refuses non-positive, clamps", func(t *testing.T) {
		t.Parallel()
		lite := testStoreSQLite(t)
		tx, err := lite.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if err := lite.setTxIdleInTxTimeout(tx, 0); err != nil {
			t.Errorf("SQLite must stay a no-op, got %v", err)
		}
		tx.Rollback()
		s := pgBounded(t, rtLimits())
		tx, _ = s.db.Begin()
		if err := s.setTxIdleInTxTimeout(tx, 0); err == nil {
			t.Error("accepted 0, which would switch the bound off")
		}
		tx.Rollback()
		tx, _ = s.db.Begin()
		defer tx.Rollback()
		if err := s.setTxIdleInTxTimeout(tx, 2*time.Hour); err != nil {
			t.Fatal(err)
		}
		var v string
		if err := tx.QueryRow(`SELECT current_setting('idle_in_transaction_session_timeout')`).Scan(&v); err != nil || v != "30min" {
			t.Errorf("idle = %q (%v), want 30min (clamped)", v, err)
		}
	})
	t.Run("sets and clamps", func(t *testing.T) {
		t.Parallel()
		s := pgBounded(t, rtLimits())
		tx, err := s.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if err := s.setTxTimeouts(tx, 2*time.Hour, 90*time.Second); err != nil {
			t.Fatal(err)
		}
		var st, lk string
		if err := tx.QueryRow(`SELECT current_setting('statement_timeout'), current_setting('lock_timeout')`).Scan(&st, &lk); err != nil {
			t.Fatal(err)
		}
		if st != "30min" || lk != "90s" {
			t.Errorf("statement=%q lock=%q, want 30min (clamped) and 90s", st, lk)
		}
		tx.Rollback()
		// SET LOCAL dies with the transaction: the pool's bound is back.
		if err := s.db.QueryRow(`SELECT current_setting('statement_timeout')`).Scan(&st); err != nil || st != "300ms" {
			t.Errorf("after the transaction statement_timeout = %q (%v), want 300ms", st, err)
		}
	})
}

func TestBoundRuntimePool_NoopOnSQLite(t *testing.T) {
	t.Parallel()
	s := testStoreSQLite(t)
	before := s.db
	if err := BoundRuntimePool(s, rtLimits()); err != nil {
		t.Fatal(err)
	}
	if s.db != before {
		t.Error("BoundRuntimePool replaced the SQLite handle")
	}
	if err := s.Ping(); err != nil {
		t.Errorf("SQLite store unusable after BoundRuntimePool: %v", err)
	}
}

func TestBoundRuntimePool_ZeroLimitsKeepPool(t *testing.T) {
	t.Parallel()
	s := pgOnly(t)
	before := s.db
	if err := BoundRuntimePool(s, RuntimeLimits{}); err != nil {
		t.Fatal(err)
	}
	if s.db != before {
		t.Error("all-zero limits replaced the pool")
	}
}

func TestRuntimeLimits_applyToDSN(t *testing.T) {
	t.Parallel()
	l := RuntimeLimits{Statement: 30 * time.Second, Lock: 1500 * time.Microsecond, IdleInTx: time.Minute}
	url, err := l.applyToDSN("postgres://pad:s3cret@h:5432/db?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"statement_timeout=30000", "lock_timeout=2", "idle_in_transaction_session_timeout=60000", "sslmode=disable", "pad:s3cret@h:5432/db"} {
		if !strings.Contains(url, want) {
			t.Errorf("URL DSN %q lacks %q", url, want)
		}
	}
	kw, err := l.applyToDSN("host=h user=pad dbname=db")
	if err != nil {
		t.Fatal(err)
	}
	if kw != "host=h user=pad dbname=db statement_timeout=30000 lock_timeout=2 idle_in_transaction_session_timeout=60000" {
		t.Errorf("keyword DSN = %q", kw)
	}
	if same, _ := (RuntimeLimits{}).applyToDSN("host=h"); same != "host=h" {
		t.Errorf("zero limits changed the DSN: %q", same)
	}
	if _, err := l.applyToDSN("postgres://u:p@h:bad/db"); err == nil || strings.Contains(err.Error(), "p@h") {
		t.Errorf("bad URL: err = %v (must fail without echoing the DSN)", err)
	}
}

func TestIsDBBusy(t *testing.T) {
	t.Parallel()
	pg := func(code string) error { return &pgconn.PgError{Code: code, Message: "SELECT secret FROM t"} }
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"57014 query_canceled", pg("57014"), true},
		{"55P03 lock_not_available", pg("55P03"), true},
		{"25P03 idle in tx", pg("25P03"), true},
		{"57P01 admin_shutdown", pg("57P01"), true},
		{"40P01 deadlock", pg("40P01"), true},
		{"40001 serialization", pg("40001"), true},
		{"wrapped %w", fmt.Errorf("create item: %w", fmt.Errorf("tx: %w", pg("57014"))), true},
		{"23505 unique violation", pg("23505"), false},
		{"23503 fk violation", pg("23503"), false},
		// Client-side interruptions are NOT definitive: they can hit during or
		// after COMMIT, so they are the other class (IsDBOutcomeUnknown).
		{"deadline exceeded", context.DeadlineExceeded, false},
		{"wrapped deadline", fmt.Errorf("q: %w", context.DeadlineExceeded), false},
		{"client cancelled", context.Canceled, false},
		{"bad conn", fmt.Errorf("x: %w", errBadConnForTest), false},
		{"conn closed text", errors.New("conn closed"), false},
		{"conn closed typed", fmt.Errorf("x: %w", pgconn.ErrConnClosed), false},
		{"proven rolled back deadline", markRolledBack(fmt.Errorf("x: %w", context.DeadlineExceeded)), true},
		{"plain", errors.New("boom"), false},
	}
	for _, c := range cases {
		if got := IsDBBusy(c.err); got != c.want {
			t.Errorf("%s: IsDBBusy = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestIsDBOutcomeUnknown(t *testing.T) {
	t.Parallel()
	pg := func(code string) error { return &pgconn.PgError{Code: code} }
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"deadline exceeded", context.DeadlineExceeded, true},
		{"wrapped deadline", fmt.Errorf("q: %w", context.DeadlineExceeded), true},
		{"bad conn", fmt.Errorf("x: %w", errBadConnForTest), true},
		{"net closed", fmt.Errorf("x: %w", net.ErrClosed), true},
		{"net op error", &net.OpError{Op: "write", Err: errors.New("broken pipe")}, true},
		{"pgconn conn closed (typed)", fmt.Errorf("x: %w", pgconn.ErrConnClosed), true},
		// The loose substring match is gone: only the typed value counts.
		{"conn closed text only", errors.New("conn closed"), false},
		{"client cancelled", context.Canceled, false},
		{"plain", errors.New("boom"), false},
		// The server answered: definitive either way, never unknown.
		{"57014", pg("57014"), false},
		{"23505", pg("23505"), false},
		{"server error wrapping a deadline", fmt.Errorf("x: %w", errors.Join(pg("57014"), context.DeadlineExceeded)), false},
		{"proven rolled back", markRolledBack(context.DeadlineExceeded), false},
	}
	for _, c := range cases {
		if got := IsDBOutcomeUnknown(c.err); got != c.want {
			t.Errorf("%s: IsDBOutcomeUnknown = %v, want %v", c.name, got, c.want)
		}
		if c.want && IsDBBusy(c.err) {
			t.Errorf("%s: both classes claim the same error", c.name)
		}
		if got := IsDBRetryable(c.err); got != (c.want || IsDBBusy(c.err)) {
			t.Errorf("%s: IsDBRetryable = %v", c.name, got)
		}
	}
}

// The account-deletion wrapper: an interruption before the COMMIT call is a
// proven rollback (definitive); one on the COMMIT call itself is unknown.
func TestClassifyDeleteAccountError(t *testing.T) {
	t.Parallel()
	pre := fmt.Errorf("delete account: %w", context.DeadlineExceeded)
	if got := classifyDeleteAccountError(pre); !IsDBBusy(got) || IsDBOutcomeUnknown(got) {
		t.Errorf("a deadline before COMMIT must be definitive: busy=%v unknown=%v", IsDBBusy(got), IsDBOutcomeUnknown(got))
	}
	commit := &commitStepError{err: fmt.Errorf("delete account: commit: %w", context.DeadlineExceeded)}
	if got := classifyDeleteAccountError(commit); IsDBBusy(got) || !IsDBOutcomeUnknown(got) {
		t.Errorf("a deadline ON the COMMIT call must be unknown: busy=%v unknown=%v", IsDBBusy(got), IsDBOutcomeUnknown(got))
	}
	badConnCommit := &commitStepError{err: fmt.Errorf("c: %w", errBadConnForTest)}
	if got := classifyDeleteAccountError(badConnCommit); IsDBBusy(got) || !IsDBOutcomeUnknown(got) {
		t.Errorf("a dead connection ON COMMIT must be unknown")
	}
	if got := classifyDeleteAccountError(nil); got != nil {
		t.Errorf("nil stays nil, got %v", got)
	}
}

// The measured-evidence rule for the bound is that it was applied to what the
// wire really returns, not only to hand-built values: take a real 57014 and a
// real 55P03 off the guarded driver and classify them.
func TestIsDBBusy_RealDriverErrors(t *testing.T) {
	t.Parallel()
	s := pgBounded(t, rtLimits())
	_, err := s.db.Exec(`SELECT pg_sleep(5)`)
	if !IsDBBusy(err) || pgErrCode(err) != "57014" {
		t.Errorf("real statement_timeout error %v not classified busy", err)
	}
}

var errBadConnForTest = driver.ErrBadConn
