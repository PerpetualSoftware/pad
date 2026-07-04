package server

import (
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

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
