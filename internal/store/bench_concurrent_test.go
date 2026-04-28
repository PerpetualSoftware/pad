package store

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestConcurrentWritePerformance measures how each backend handles concurrent
// writers. SQLite serializes writes (SQLITE_BUSY under contention); PostgreSQL
// supports true concurrent writes. This test quantifies the difference.
//
// Runs against both SQLite and PostgreSQL (when PAD_TEST_POSTGRES_URL is set).
func TestConcurrentWritePerformance(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping load test in short mode")
	}

	pgURL := os.Getenv("PAD_TEST_POSTGRES_URL")

	// Always test SQLite.
	t.Run("SQLite", func(t *testing.T) {
		dir := t.TempDir()
		s, err := New(filepath.Join(dir, "test.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		runConcurrentWriteTest(t, s, "SQLite")
	})

	// Test PostgreSQL if available.
	if pgURL != "" {
		t.Run("PostgreSQL", func(t *testing.T) {
			s := testStorePostgres(t, pgURL)
			runConcurrentWriteTest(t, s, "PostgreSQL")
		})
	}
}

func runConcurrentWriteTest(t *testing.T, s *Store, label string) {
	t.Helper()

	// Seed a workspace + collection.
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "loadtest"})
	if err != nil {
		t.Fatal(err)
	}
	coll, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name:   "Tasks",
		Schema: `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","done"],"default":"open","required":true}]}`,
	})
	if err != nil {
		t.Fatal(err)
	}

	concurrencyLevels := []int{1, 5, 10, 25, 50}
	opsPerWorker := 10

	t.Logf("\n%s — Concurrent Write Performance", label)
	t.Logf("%-12s | %-8s | %-10s | %-10s | %-10s | %s", "Workers", "Total", "Errors", "Throughput", "Avg", "p95")
	t.Logf("%-12s-+%-10s+%-12s+%-12s+%-12s+%s", "------------", "----------", "------------", "------------", "------------", "----------")

	for _, workers := range concurrencyLevels {
		var (
			wg        sync.WaitGroup
			errors    atomic.Int64
			latencies = make([]time.Duration, 0, workers*opsPerWorker)
			mu        sync.Mutex
		)

		start := time.Now()

		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(workerID int) {
				defer wg.Done()
				for i := 0; i < opsPerWorker; i++ {
					opStart := time.Now()
					title := fmt.Sprintf("Worker %d Item %d", workerID, i)
					_, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{
						Title:   title,
						Content: fmt.Sprintf("Content for %s — testing concurrent writes.", title),
						Fields:  `{"status":"open"}`,
					})
					elapsed := time.Since(opStart)

					mu.Lock()
					latencies = append(latencies, elapsed)
					mu.Unlock()

					if err != nil {
						errors.Add(1)
						mu.Lock()
						if errors.Load() <= 3 {
							t.Logf("  [worker %d] error: %v", workerID, err)
						}
						mu.Unlock()
					}
				}
			}(w)
		}

		wg.Wait()
		totalTime := time.Since(start)
		totalOps := workers * opsPerWorker
		errCount := errors.Load()
		throughput := float64(totalOps) / totalTime.Seconds()

		// Calculate avg and p95.
		avg := avgDuration(latencies)
		p95 := percentileDuration(latencies, 0.95)

		t.Logf("%-12d | %-8d | %-10d | %-10.1f/s | %-10s | %s",
			workers, totalOps, errCount, throughput, avg.Truncate(time.Microsecond), p95.Truncate(time.Microsecond))

		if errCount > 0 && label == "PostgreSQL" {
			t.Errorf("PostgreSQL had %d errors with %d concurrent writers — should handle this cleanly", errCount, workers)
		}
	}
}

func avgDuration(durations []time.Duration) time.Duration {
	if len(durations) == 0 {
		return 0
	}
	var total time.Duration
	for _, d := range durations {
		total += d
	}
	return total / time.Duration(len(durations))
}

func percentileDuration(durations []time.Duration, p float64) time.Duration {
	if len(durations) == 0 {
		return 0
	}
	// Simple sort-based percentile (good enough for test sizes).
	sorted := make([]time.Duration, len(durations))
	copy(sorted, durations)
	// Insertion sort — small slices.
	for i := 1; i < len(sorted); i++ {
		key := sorted[i]
		j := i - 1
		for j >= 0 && sorted[j] > key {
			sorted[j+1] = sorted[j]
			j--
		}
		sorted[j+1] = key
	}
	idx := int(float64(len(sorted)-1) * p)
	return sorted[idx]
}

// TestConcurrentMixedReadWrite simulates a realistic workload: mostly reads with
// some writes, to check for contention under typical usage patterns.
func TestConcurrentMixedReadWrite(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping load test in short mode")
	}

	s := testStore(t)

	// Seed initial data.
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "mixed-load"})
	if err != nil {
		t.Fatal(err)
	}
	coll, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name:   "Tasks",
		Schema: `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","done"],"default":"open","required":true}]}`,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Pre-seed 50 items.
	for i := 0; i < 50; i++ {
		s.CreateItem(ws.ID, coll.ID, models.ItemCreate{
			Title:   fmt.Sprintf("Seed Item %d", i),
			Content: "Seeded content for mixed workload test.",
			Fields:  `{"status":"open"}`,
		})
	}

	const (
		readers    = 20
		writers    = 5
		opsPerUser = 20
		duration   = 3 * time.Second
	)

	var (
		readOps, writeOps   atomic.Int64
		readErrs, writeErrs atomic.Int64
		wg                  sync.WaitGroup
	)

	deadline := time.Now().Add(duration)

	// Readers: list items.
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				_, err := s.ListItems(ws.ID, models.ItemListParams{Limit: 20})
				readOps.Add(1)
				if err != nil {
					readErrs.Add(1)
				}
			}
		}()
	}

	// Writers: create items.
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			i := 0
			for time.Now().Before(deadline) {
				_, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{
					Title:   fmt.Sprintf("Mixed Writer %d Item %d", id, i),
					Content: "Content from mixed workload writer.",
					Fields:  `{"status":"open"}`,
				})
				writeOps.Add(1)
				if err != nil {
					writeErrs.Add(1)
				}
				i++
			}
		}(w)
	}

	wg.Wait()

	driver := string(s.D().Driver())
	t.Logf("\n%s — Mixed Read/Write (%d readers, %d writers, %s)", driver, readers, writers, duration)
	t.Logf("  Reads:  %d ops (%d errors)", readOps.Load(), readErrs.Load())
	t.Logf("  Writes: %d ops (%d errors)", writeOps.Load(), writeErrs.Load())
	t.Logf("  Read throughput:  %.0f ops/s", float64(readOps.Load())/duration.Seconds())
	t.Logf("  Write throughput: %.0f ops/s", float64(writeOps.Load())/duration.Seconds())

	// PostgreSQL should have zero errors.
	if driver == "postgres" && (readErrs.Load() > 0 || writeErrs.Load() > 0) {
		t.Errorf("PostgreSQL had errors under mixed workload: %d read, %d write", readErrs.Load(), writeErrs.Load())
	}
}
