package server

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// syncBuffer is a minimal concurrency-safe io.Writer so a test can read a
// slog buffer written from a background sweeper goroutine without racing.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func countEmailVerificationTokens(t *testing.T, srv *Server) int {
	t.Helper()
	var n int
	if err := srv.store.DB().QueryRow(`SELECT COUNT(*) FROM email_verification_tokens`).Scan(&n); err != nil {
		t.Fatalf("count email_verification_tokens: %v", err)
	}
	return n
}

// mintUsedVerificationToken creates an unverified user, mints a verification
// token, and consumes it — leaving a single used (reaper-eligible) row.
func mintUsedVerificationToken(t *testing.T, srv *Server, email string) {
	t.Helper()
	u, err := srv.store.CreateUser(models.UserCreate{
		Email: email, Name: "Unv", Password: "password123", Unverified: true,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	token, err := srv.store.CreateEmailVerification(u.ID)
	if err != nil {
		t.Fatalf("CreateEmailVerification: %v", err)
	}
	if _, err := srv.store.ConsumeEmailVerification(token); err != nil {
		t.Fatalf("ConsumeEmailVerification: %v", err)
	}
}

// TestTokenReaper_SweepsViaLoop drives the real ticker: a used verification
// token row is deleted by a live reaper loop, then Stop() drains it cleanly.
// Interval is tight (10ms) purely to land the sweep inside the test window —
// the production default is 1h.
func TestTokenReaper_SweepsViaLoop(t *testing.T) {
	srv := testServer(t)

	mintUsedVerificationToken(t, srv, "reaper-loop@example.com")
	if got := countEmailVerificationTokens(t, srv); got != 1 {
		t.Fatalf("precondition: want 1 used row, got %d", got)
	}

	srv.SetTokenReaperConfig(10 * time.Millisecond)
	srv.StartTokenReaper()
	t.Cleanup(srv.Stop)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if countEmailVerificationTokens(t, srv) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("after 2s: reaper did not delete the used token row (still %d)", countEmailVerificationTokens(t, srv))
}

// TestTokenReaper_TickIsSafeWithNoRows: a tick on an empty database is a clean
// no-op (all four cleaners run without error).
func TestTokenReaper_TickIsSafeWithNoRows(t *testing.T) {
	srv := testServer(t)
	// Should not panic or error; nothing to assert beyond "doesn't blow up".
	srv.runTokenReaperTick()
}

// TestTokenReaper_StartIsIdempotent confirms StartTokenReaper twice only spawns
// one goroutine — without the running-flag guard, Stop() would hang forever on
// the second goroutine that never received its stop signal.
func TestTokenReaper_StartIsIdempotent(t *testing.T) {
	srv := testServer(t)

	// 1h interval = the loop never actually fires during the test; we only
	// exercise the running-flag check + clean shutdown.
	srv.SetTokenReaperConfig(1 * time.Hour)
	srv.StartTokenReaper()
	srv.StartTokenReaper() // second call must be a no-op
	srv.StartTokenReaper() // third too

	done := make(chan struct{})
	go func() {
		srv.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop() hung — a duplicate reaper goroutine was likely spawned")
	}

	// Stop is safe to call again (loop already stopped).
	srv.stopTokenReaper()
}

// TestTokenReaper_RecoversPanic pins BUG-2071: the long-running sweeper loops
// spawn their own s.bg-tracked goroutine (they can't route through goAsync
// without breaking their stop-channel lifecycle), so each needs its own
// deferred recover(). A panic inside a sweeper tick must NOT crash the whole
// single-binary server — it must be logged with a stack, the goroutine must
// unwind cleanly with s.bg.Done() still firing, and Stop() must still drain.
//
// A Server built with a nil store makes the reaper tick panic for real: the
// first cleaner (CleanExpiredEmailVerifications) dereferences the nil
// *store.Store. That exercises the actual token-reaper goroutine + tick + the
// deferred recoverSweeper end-to-end, mirroring TestServer_goAsync_RecoversPanic
// for the loop-shaped sweepers.
func TestTokenReaper_RecoversPanic(t *testing.T) {
	var logBuf syncBuffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	defer slog.SetDefault(prevLogger)

	// New(nil) wires rateLimiters (so Stop() is safe) but leaves store nil,
	// so the very first reaper tick panics on the nil-pointer deref.
	srv := New(nil)

	// Tight interval so the panicking tick lands inside the test window; the
	// production default is 1h.
	srv.SetTokenReaperConfig(5 * time.Millisecond)
	srv.StartTokenReaper()

	// Deterministic proof the recover path executed: wait for the panic log.
	// If recover() were missing, the re-panic would have already killed the
	// process (and this test binary) before we could observe anything.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		logged := logBuf.String()
		if strings.Contains(logged, "background sweeper panicked") &&
			strings.Contains(logged, "token-reaper") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if logged := logBuf.String(); !strings.Contains(logged, "background sweeper panicked") ||
		!strings.Contains(logged, "token-reaper") {
		t.Fatalf("expected a recovered-panic log from the token-reaper sweeper, got: %s", logged)
	}

	// The goroutine's deferred s.bg.Done() must still fire after the recover,
	// so Stop() drains rather than hanging.
	stopReturned := make(chan struct{})
	go func() {
		srv.Stop()
		close(stopReturned)
	}()
	select {
	case <-stopReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("Server.Stop() did not return within 2s after a panicking reaper tick")
	}
}

// TestTokenReaper_NotStartedIsCleanShutdown: a server that never started the
// reaper still stops cleanly (stopTokenReaper is a no-op when not running).
// Guards the "does not leak goroutines in unit tests" invariant — the reaper
// only runs when explicitly started (from cmd/pad/main.go), never from New.
func TestTokenReaper_NotStartedIsCleanShutdown(t *testing.T) {
	srv := testServer(t)
	// testServer registers srv.Stop via t.Cleanup; calling stop on the
	// never-started reaper here must be harmless.
	srv.stopTokenReaper()
}
