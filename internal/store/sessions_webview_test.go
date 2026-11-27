package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// webViewStoreBackends runs fn on SQLite and, when PAD_TEST_POSTGRES_URL is
// set, on Postgres too: the parent read and the cascade differ per engine.
func webViewStoreBackends(t *testing.T, fn func(t *testing.T, s *Store)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) { fn(t, testStoreSQLite(t)) })
	t.Run("postgres", func(t *testing.T) {
		pgURL := os.Getenv("PAD_TEST_POSTGRES_URL")
		if pgURL == "" {
			t.Skip("PAD_TEST_POSTGRES_URL not set; Postgres-backed leg skipped")
		}
		fn(t, testStorePostgres(t, pgURL))
	})
}

func countSessions(t *testing.T, s *Store, userID string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(s.q(`SELECT COUNT(*) FROM sessions WHERE user_id = ?`), userID).Scan(&n); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	return n
}

// A child is minted only from a live web session of the same user. Every
// other parent is ErrSessionGone, and nothing is inserted.
func TestCreateWebViewSessionParentGone(t *testing.T) {
	webViewStoreBackends(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		u := createTestUser(t, s, "webview-parent@test.com", "Parent", "password123")
		other := createTestUser(t, s, "webview-other@test.com", "Other", "password123")

		webTok, err := s.CreateSession(u.ID, "web", "127.0.0.1", "UA/1", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		web, err := s.ValidateSession(webTok)
		if err != nil || web == nil {
			t.Fatalf("validate web parent: %v %v", web, err)
		}

		// The control: a live web parent mints a child.
		child, exp, err := s.CreateWebViewSession(ctx, web.ID, u.ID, "127.0.0.1", "UA/1", 15*time.Minute, false)
		if err != nil {
			t.Fatalf("mint from a live web parent: %v", err)
		}
		if !strings.HasPrefix(child, "padsess_") || exp.IsZero() {
			t.Fatalf("mint returned %q, %v", child, exp)
		}
		info, err := s.ValidateSession(child)
		if err != nil || info == nil {
			t.Fatalf("validate child: %v %v", info, err)
		}
		if info.Kind != SessionKindWebView || info.ParentID != web.ID || info.RenewTTL != 0 || info.SignInStamp != nil {
			t.Fatalf("child row: kind=%q parent=%q renew=%v stamp=%v", info.Kind, info.ParentID, info.RenewTTL, info.SignInStamp)
		}

		cliTok, err := s.CreateSession(u.ID, "cli", "127.0.0.1", "UA/1", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		cli, _ := s.ValidateSession(cliTok)
		otherTok, err := s.CreateSession(other.ID, "web", "127.0.0.1", "UA/1", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		otherSess, _ := s.ValidateSession(otherTok)
		expiredTok, err := s.CreateSession(u.ID, "web", "127.0.0.1", "UA/1", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		expired, _ := s.ValidateSession(expiredTok)
		if _, err := s.db.Exec(s.q(`UPDATE sessions SET expires_at = ? WHERE id = ?`),
			time.Now().UTC().Add(-time.Minute).Format(time.RFC3339), expired.ID); err != nil {
			t.Fatal(err)
		}
		deletedTok, err := s.CreateSession(u.ID, "web", "127.0.0.1", "UA/1", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		deleted, _ := s.ValidateSession(deletedTok)
		if err := s.DeleteSession(deletedTok); err != nil {
			t.Fatal(err)
		}

		before := countSessions(t, s, u.ID)
		for _, c := range []struct {
			name, parentID, userID string
		}{
			{"deleted parent", deleted.ID, u.ID},
			{"expired parent", expired.ID, u.ID},
			{"cli parent", cli.ID, u.ID},
			{"webview child as parent", info.ID, u.ID},
			{"another user's parent", otherSess.ID, u.ID},
			{"no such id", "no-such-session", u.ID},
		} {
			tok, _, err := s.CreateWebViewSession(ctx, c.parentID, c.userID, "127.0.0.1", "UA/1", 15*time.Minute, true)
			if !errors.Is(err, ErrSessionGone) || tok != "" {
				t.Errorf("%s: token %q, err %v; want ErrSessionGone", c.name, tok, err)
			}
		}
		if after := countSessions(t, s, u.ID); after != before {
			t.Fatalf("a refused mint changed the session count: %d -> %d", before, after)
		}
		// replace_previous on a refused mint deleted nothing either.
		if got, _ := s.ValidateSession(child); got == nil {
			t.Fatal("a refused mint with replace_previous removed the live child")
		}

		// A disabled account mints nothing.
		if err := s.DisableUserAndRevokeAccess(u.ID); err != nil {
			t.Fatalf("disable: %v", err)
		}
		if _, _, err := s.CreateWebViewSession(ctx, web.ID, u.ID, "127.0.0.1", "UA/1", 15*time.Minute, false); err == nil {
			t.Fatal("a disabled account minted a child")
		}
	})
}

// Deleting the parent deletes every child (ON DELETE CASCADE), and a
// rotation of the user's sessions removes children with the rest.
func TestWebViewChildCascadesWithParent(t *testing.T) {
	webViewStoreBackends(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		u := createTestUser(t, s, "webview-cascade@test.com", "Cascade", "password123")
		parentTok, err := s.CreateSession(u.ID, "web", "127.0.0.1", "UA/1", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		parent, _ := s.ValidateSession(parentTok)
		a, _, err := s.CreateWebViewSession(ctx, parent.ID, u.ID, "127.0.0.1", "UA/1", 15*time.Minute, false)
		if err != nil {
			t.Fatal(err)
		}
		b, _, err := s.CreateWebViewSession(ctx, parent.ID, u.ID, "127.0.0.1", "UA/1", 15*time.Minute, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteSession(parentTok); err != nil {
			t.Fatal(err)
		}
		for _, tok := range []string{a, b} {
			if info, err := s.ValidateSession(tok); err != nil || info != nil {
				t.Fatalf("child survived its parent: %v %v", info, err)
			}
		}
		if n := countSessions(t, s, u.ID); n != 0 {
			t.Fatalf("%d session rows left after the parent was deleted", n)
		}
	})
}
