package store_test

import (
	"sync"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// TASK-4745: a provider identity the login refused as not linked, held for
// the account's next full sign-in.

func pendingLinkUser(t *testing.T, s *store.Store, email string, unverified ...bool) *models.User {
	t.Helper()
	u, err := s.CreateUser(models.UserCreate{Email: email, Name: "Owner", Password: "correct-horse-battery-staple", Role: "member",
		Unverified: len(unverified) > 0 && unverified[0]})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func pendingLinkRows(t *testing.T, s *store.Store, userID string) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRow(s.D().Rebind(`SELECT COUNT(*) FROM oauth_pending_links WHERE user_id = ?`), userID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestOAuthPendingLinks_Lifecycle(t *testing.T) {
	stepUpBackends(t, func(t *testing.T, s *store.Store) {
		u := pendingLinkUser(t, s, "life@example.com")
		tok, err := s.CreateOAuthPendingLink(u.ID, "google", "g-life", "life@example.com")
		if err != nil {
			t.Fatal(err)
		}
		if len(tok) != len("padpl_")+64 || tok[:6] != "padpl_" {
			t.Fatalf("token shape: %q", tok)
		}
		// The plaintext is not stored.
		var stored string
		if err := s.DB().QueryRow(s.D().Rebind(`SELECT token_hash FROM oauth_pending_links WHERE user_id = ?`), u.ID).Scan(&stored); err != nil || stored == tok {
			t.Fatalf("stored hash %q (%v)", stored, err)
		}
		// Peek reads without spending.
		for i := 0; i < 2; i++ {
			pl, err := s.PeekOAuthPendingLink(tok)
			if err != nil || pl == nil || pl.UserID != u.ID || pl.Provider != "google" || pl.Subject != "g-life" || pl.Email != "life@example.com" {
				t.Fatalf("peek %d: %+v %v", i, pl, err)
			}
		}
		// A newer token kills the older one.
		newer, err := s.CreateOAuthPendingLink(u.ID, "google", "g-life", "life@example.com")
		if err != nil {
			t.Fatal(err)
		}
		if pl, _ := s.PeekOAuthPendingLink(tok); pl != nil {
			t.Fatal("an older token survived a newer one")
		}
		pl, err := s.ConsumeOAuthPendingLink(newer)
		if err != nil || pl == nil || pl.UserID != u.ID {
			t.Fatalf("consume: %+v %v", pl, err)
		}
		if pl, _ := s.ConsumeOAuthPendingLink(newer); pl != nil {
			t.Fatal("a token consumed twice")
		}
		if pl, _ := s.PeekOAuthPendingLink("padpl_unknown"); pl != nil {
			t.Fatal("an unknown token peeked")
		}
		// Expired: neither peeks nor consumes, and the reaper removes it.
		exp, err := s.CreateOAuthPendingLink(u.ID, "github", "gh-life", "life@example.com")
		if err != nil {
			t.Fatal(err)
		}
		stepUpExec(t, s, `UPDATE oauth_pending_links SET expires_at = ? WHERE used_at IS NULL`, "2000-01-01T00:00:00Z")
		if pl, _ := s.PeekOAuthPendingLink(exp); pl != nil {
			t.Fatal("an expired token peeked")
		}
		if pl, _ := s.ConsumeOAuthPendingLink(exp); pl != nil {
			t.Fatal("an expired token consumed")
		}
		if err := s.CleanExpiredOAuthPendingLinks(); err != nil {
			t.Fatal(err)
		}
		if n := pendingLinkRows(t, s, u.ID); n != 0 {
			t.Fatalf("reaper left %d rows", n)
		}
	})
}

func TestOAuthPendingLinks_ConcurrentConsumeOneWins(t *testing.T) {
	stepUpBackends(t, func(t *testing.T, s *store.Store) {
		u := pendingLinkUser(t, s, "race@example.com")
		tok, err := s.CreateOAuthPendingLink(u.ID, "google", "g-race", "race@example.com")
		if err != nil {
			t.Fatal(err)
		}
		const n = 8
		var wg sync.WaitGroup
		var mu sync.Mutex
		wins, errs := 0, 0
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				pl, err := s.ConsumeOAuthPendingLink(tok)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					errs++
				}
				if pl != nil {
					wins++
				}
			}()
		}
		close(start)
		wg.Wait()
		if wins != 1 || errs != 0 {
			t.Fatalf("wins=%d errors=%d, want exactly one win and no errors", wins, errs)
		}
	})
}

func TestOAuthPendingLinks_ClaimDeletesRows(t *testing.T) {
	stepUpBackends(t, func(t *testing.T, s *store.Store) {
		// A claim takes only a never-verified account. No token is issued
		// for one (TASK-4745), but a row written directly stands for any
		// that exists when the claim lands.
		u := pendingLinkUser(t, s, "claimed@example.com", true)
		if _, err := s.CreateOAuthPendingLink(u.ID, "github", "gh-squat", "claimed@example.com"); err != nil {
			t.Fatal(err)
		}
		if n := pendingLinkRows(t, s, u.ID); n != 1 {
			t.Fatalf("precondition: %d rows", n)
		}
		if _, err := s.ClaimAccountByProvider(u.ID, "google", "g-owner", "Owner"); err != nil {
			t.Fatal(err)
		}
		if n := pendingLinkRows(t, s, u.ID); n != 0 {
			t.Fatalf("a claim left %d pending links", n)
		}
	})
}
