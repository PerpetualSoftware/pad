package main

import (
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/config"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// TASK-4278: the boot swap to the bounded Postgres pool.

func TestApplyDBRuntimeLimits_BoundsPostgres(t *testing.T) {
	s := storetest.NewPostgres(t) // skips unless PAD_TEST_POSTGRES_URL is set
	startup := s.DB()
	var backend string
	if err := startup.QueryRow(`SHOW server_version`).Scan(&backend); err != nil || backend == "" {
		t.Fatalf("not a real Postgres backend: %q %v", backend, err)
	}

	cfg := &config.Config{DBStatementTimeoutRaw: "21s", DBLockTimeoutRaw: "7s", DBIdleInTxTimeoutRaw: "3m"}
	if err := applyDBRuntimeLimits(s, cfg, "postgres"); err != nil {
		t.Fatalf("applyDBRuntimeLimits: %v", err)
	}
	if err := startup.Ping(); err == nil {
		t.Error("the startup pool is still open after the swap")
	}

	// Under -race: the goroutines a server starts after boot use the swapped
	// handle concurrently, every one on a bounded connection.
	var wg sync.WaitGroup
	errs := make(chan string, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var st, lk, id string
			if err := s.DB().QueryRow(`SELECT current_setting('statement_timeout'), current_setting('lock_timeout'), current_setting('idle_in_transaction_session_timeout')`).Scan(&st, &lk, &id); err != nil {
				errs <- err.Error()
				return
			}
			if st != "21s" || lk != "7s" || id != "3min" {
				errs <- "unbounded connection: " + st + "/" + lk + "/" + id
			}
			if err := s.Ping(); err != nil {
				errs <- err.Error()
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	var n int
	if err := s.DB().QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil || n == 0 {
		t.Errorf("schema_migrations on the runtime pool: %d, %v", n, err)
	}
}

func TestApplyDBRuntimeLimits_DefaultsOnPostgres(t *testing.T) {
	s := storetest.NewPostgres(t)
	if err := applyDBRuntimeLimits(s, &config.Config{}, "postgres"); err != nil {
		t.Fatal(err)
	}
	var st, lk, id string
	if err := s.DB().QueryRow(`SELECT current_setting('statement_timeout'), current_setting('lock_timeout'), current_setting('idle_in_transaction_session_timeout')`).Scan(&st, &lk, &id); err != nil {
		t.Fatal(err)
	}
	if st != "30s" || lk != "10s" || id != "1min" {
		t.Errorf("defaults = %s/%s/%s, want 30s/10s/1min", st, lk, id)
	}
}

func TestApplyDBRuntimeLimits_MalformedRefusesStart(t *testing.T) {
	s := storetest.NewSQLite(t)
	err := applyDBRuntimeLimits(s, &config.Config{DBLockTimeoutRaw: "tuesday"}, "postgres")
	if err == nil || !strings.Contains(err.Error(), "PAD_DB_LOCK_TIMEOUT") || strings.Contains(err.Error(), "tuesday") {
		t.Fatalf("err = %v, want one naming PAD_DB_LOCK_TIMEOUT without echoing the value", err)
	}
}

func TestApplyDBRuntimeLimits_SQLiteUntouched(t *testing.T) {
	s := storetest.NewSQLite(t)
	before := s.DB()
	if err := applyDBRuntimeLimits(s, &config.Config{DBStatementTimeoutRaw: "5s"}, ""); err != nil {
		t.Fatal(err)
	}
	if s.DB() != before {
		t.Error("the SQLite handle was replaced")
	}
	if err := s.Ping(); err != nil {
		t.Error(err)
	}
}

// The swap assigns the store's handle, so it must run single-threaded: after
// the startup backfills, before anything is started or captures s.DB().
func TestBoundRuntimePoolSwapPrecedesEverythingStarted(t *testing.T) {
	raw, err := os.ReadFile("cmd_server.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	swap := strings.Index(src, "applyDBRuntimeLimits(s, cfg, dbDriver)")
	if swap < 0 {
		t.Fatal("cmd_server.go no longer calls applyDBRuntimeLimits(s, cfg, dbDriver)")
	}
	if last := strings.LastIndex(src[:swap], "bootPlanBackfill(s,"); last < 0 {
		t.Error("the swap no longer follows the bootPlanBackfill startup pass")
	}
	for _, after := range []string{
		"RegisterDBCollector(",
		"srv.Serve",
		"Sweeper",
		"Start(",
	} {
		if i := strings.Index(src, after); i >= 0 && i < swap {
			t.Errorf("%q appears before the pool swap", after)
		}
	}
	// The only goroutine allowed to start earlier is the billing offer cache,
	// which talks to the sidecar and never to the store.
	head := src[:swap]
	if n := strings.Count(head, "go func") + strings.Count(head, "go srv.") + strings.Count(head, "go s."); n != 0 {
		t.Errorf("%d goroutine start(s) before the pool swap", n)
	}
	if n := strings.Count(head, "go offers.Run("); n > 1 {
		t.Errorf("unexpected extra goroutines before the swap: %d offers.Run", n)
	}
}
