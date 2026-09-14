package store

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// PROBE (BUG-3072, day 67) — NOT a keeper. Measures the ruling's premise:
// does a pooled autocommit INSERT that is BLOCKED on busy_timeout pick up a
// trigger installed by the transaction it is waiting on?
//
// Three legs. CONTROL-A proves the harness can observe a success at all.
// CONTROL-B reproduces the predecessor's 3.04s-then-nil. TEST is the claim.
const probeTriggerSQL = `CREATE TRIGGER pad_migrated_item_yjs_updates_ins
BEFORE INSERT ON item_yjs_updates
FOR EACH ROW
BEGIN
	SELECT RAISE(ABORT, 'PADMIGRATED: this SQLite database was migrated to PostgreSQL');
END;`

type probeResult struct {
	err     error
	elapsed time.Duration
}

// probeSetup returns two independent handles on ONE file, plus an item id.
func probeSetup(t *testing.T) (a, b *Store, itemID string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "probe.db")
	a, err := New(path)
	if err != nil {
		t.Fatalf("open A: %v", err)
	}
	t.Cleanup(func() { a.Close() })

	ws := createTestWorkspace(t, a, "Probe")
	col := createTestCollection(t, a, ws.ID, "Tasks")
	item := createTestItem(t, a, ws.ID, col.ID, "Task A", "")

	b, err = New(path)
	if err != nil {
		t.Fatalf("open B: %v", err)
	}
	t.Cleanup(func() { b.Close() })
	return a, b, item.ID
}

func probeAppend(b *Store, itemID string) <-chan probeResult {
	ch := make(chan probeResult, 1)
	go func() {
		start := time.Now()
		_, err := b.AppendYjsUpdate(itemID, []byte{1, 2, 3}, "1")
		ch <- probeResult{err: err, elapsed: time.Since(start)}
	}()
	return ch
}

func TestZZProbeControlANoLock(t *testing.T) {
	_, b, itemID := probeSetup(t)
	r := <-probeAppend(b, itemID)
	fmt.Printf("PROBE CONTROL-A (no lock): elapsed=%v err=%v\n", r.elapsed, r.err)
	if r.err != nil {
		t.Fatalf("control leg must succeed, got %v", r.err)
	}
}

func TestZZProbeControlBLockNoTrigger(t *testing.T) {
	a, b, itemID := probeSetup(t)
	tx, err := a.db.Begin() // _txlock=immediate → BEGIN IMMEDIATE
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	ch := probeAppend(b, itemID)
	time.Sleep(3 * time.Second)
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	r := <-ch
	fmt.Printf("PROBE CONTROL-B (lock 3s, NO trigger): elapsed=%v err=%v\n", r.elapsed, r.err)
	if r.err != nil {
		t.Fatalf("control-B must succeed (this is the 3.04s-then-nil case), got %v", r.err)
	}
}

func TestZZProbeTestLockThenTrigger(t *testing.T) {
	a, b, itemID := probeSetup(t)
	tx, err := a.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	ch := probeAppend(b, itemID)
	time.Sleep(3 * time.Second)
	if _, err := tx.Exec(probeTriggerSQL); err != nil {
		t.Fatalf("install trigger inside tx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	r := <-ch
	fmt.Printf("PROBE TEST (lock 3s, trigger installed in tx): elapsed=%v err=%v\n", r.elapsed, r.err)
	if r.err == nil {
		fmt.Println("PROBE VERDICT: PREMISE FALSIFIED — the deferred append SUCCEEDED past the new trigger")
		return
	}
	if strings.Contains(r.err.Error(), "PADMIGRATED") {
		fmt.Println("PROBE VERDICT: PREMISE HOLDS — deferred append refused by the trigger, marker in the message")
		return
	}
	fmt.Printf("PROBE VERDICT: refused but NOT by the marker: %v\n", r.err)
}
