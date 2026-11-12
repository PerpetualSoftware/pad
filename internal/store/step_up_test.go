package store_test

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

func stepUpBackends(t *testing.T, f func(*testing.T, *store.Store)) {
	t.Helper()
	for _, b := range []struct {
		name string
		open func(*testing.T) *store.Store
	}{
		{"SQLite", storetest.NewSQLite}, {"Postgres", storetest.NewPostgres},
	} {
		t.Run(b.name, func(t *testing.T) { f(t, b.open(t)) })
	}
}
func stepUpUser(t *testing.T, s *store.Store, email string) (*models.User, *store.SessionInfo, string) {
	t.Helper()
	u, err := s.CreateUser(models.UserCreate{Email: email, Name: "Owner", Password: "correct-horse-battery-staple", Role: "member"})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := s.CreateSession(u.ID, "web", "", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.ValidateSession(tok)
	if err != nil || info == nil {
		t.Fatalf("session: %v", err)
	}
	return u, info, tok
}
func stepUpExec(t *testing.T, s *store.Store, q string, args ...any) {
	t.Helper()
	if _, err := s.DB().Exec(s.D().Rebind(q), args...); err != nil {
		t.Fatal(err)
	}
}

func TestStepUpCodeLifecycle(t *testing.T) {
	stepUpBackends(t, func(t *testing.T, s *store.Store) {
		u, session, _ := stepUpUser(t, s, "codes@example.com")
		id, code, expiry, err := s.CreateStepUpCode(u.ID, session.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !regexp.MustCompile(`^[0-9]{6}$`).MatchString(code) || time.Until(expiry) < 9*time.Minute || time.Until(expiry) > 10*time.Minute {
			t.Fatal("bad code shape or lifetime")
		}
		var hash string
		if err := s.DB().QueryRow(s.D().Rebind(`SELECT code_hash FROM step_up_codes WHERE id=?`), id).Scan(&hash); err != nil {
			t.Fatal(err)
		}
		if hash == code || len(hash) != 64 {
			t.Fatal("code not hashed")
		}
		// A different session cannot consume this code.
		if ok, err := s.VerifyStepUpCode(u.ID, "another-session", code); err != nil || ok {
			t.Fatalf("session isolation: %v %v", ok, err)
		}
		wrong := "000000"
		if wrong == code {
			wrong = "000001"
		}
		for i := 0; i < 5; i++ {
			if ok, err := s.VerifyStepUpCode(u.ID, session.ID, wrong); err != nil || ok {
				t.Fatalf("wrong attempt %d: %v %v", i, ok, err)
			}
		}
		if ok, err := s.VerifyStepUpCode(u.ID, session.ID, code); err != nil || ok {
			t.Fatalf("exhausted: %v %v", ok, err)
		}
		_, fresh, _, err := s.CreateStepUpCode(u.ID, session.ID)
		if err != nil {
			t.Fatal(err)
		}
		if ok, err := s.VerifyStepUpCode(u.ID, session.ID, code); err != nil || ok {
			t.Fatalf("superseded: %v %v", ok, err)
		}
		if ok, err := s.VerifyStepUpCode(u.ID, session.ID, fresh); err != nil || !ok {
			t.Fatalf("fresh: %v %v", ok, err)
		}
		if ok, err := s.VerifyStepUpCode(u.ID, session.ID, fresh); err != nil || ok {
			t.Fatalf("replay: %v %v", ok, err)
		}
		id, expired, _, err := s.CreateStepUpCode(u.ID, session.ID)
		if err != nil {
			t.Fatal(err)
		}
		stepUpExec(t, s, `UPDATE step_up_codes SET expires_at=? WHERE id=?`, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339), id)
		if ok, err := s.VerifyStepUpCode(u.ID, session.ID, expired); err != nil || ok {
			t.Fatalf("expired: %v %v", ok, err)
		}
		_, _, _, err = s.CreateStepUpCode(u.ID, session.ID)
		var limited *store.ErrStepUpSendLimit
		if !errors.As(err, &limited) || limited.RetryAfter <= 0 || limited.RetryAfter > time.Hour {
			t.Fatalf("send limit: %v", err)
		}
		// A failed delivery is retired, not refunded: it still counts toward
		// the hourly cap, so repeated failures cannot loop sends.
		if err := s.RetireStepUpCode(id); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := s.CreateStepUpCode(u.ID, session.ID); !errors.As(err, &limited) {
			t.Fatalf("retired send refunded the cap: %v", err)
		}
		// Logging out does not reset the user-wide rate limit.
		stepUpExec(t, s, `DELETE FROM sessions WHERE id=?`, session.ID)
		if _, _, _, err := s.CreateStepUpCode(u.ID, "new-session"); !errors.As(err, &limited) {
			t.Fatalf("logout reset send cap: %v", err)
		}
		stepUpExec(t, s, `UPDATE step_up_codes SET created_at=? WHERE user_id=?`, time.Now().Add(-3*time.Hour).UTC().Format(time.RFC3339), u.ID)
		if err := s.CleanExpiredStepUpCodes(); err != nil {
			t.Fatal(err)
		}
		var n int
		if err := s.DB().QueryRow(`SELECT COUNT(*) FROM step_up_codes`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("cleanup: %d %v", n, err)
		}
	})
}
func TestStepUpConcurrentConsume(t *testing.T) {
	stepUpBackends(t, func(t *testing.T, s *store.Store) {
		u, session, _ := stepUpUser(t, s, "consume@example.com")
		_, code, _, err := s.CreateStepUpCode(u.ID, session.ID)
		if err != nil {
			t.Fatal(err)
		}
		var wins atomic.Int32
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				ok, err := s.VerifyStepUpCode(u.ID, session.ID, code)
				if err != nil {
					t.Error(err)
				}
				if ok {
					wins.Add(1)
				}
			}()
		}
		close(start)
		wg.Wait()
		if wins.Load() != 1 {
			t.Fatalf("consumed %d times", wins.Load())
		}
	})
}
func TestStepUpConcurrentSendLimit(t *testing.T) {
	stepUpBackends(t, func(t *testing.T, s *store.Store) {
		u, session, _ := stepUpUser(t, s, "send@example.com")
		var wins atomic.Int32
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, _, _, err := s.CreateStepUpCode(u.ID, session.ID)
				if err == nil {
					wins.Add(1)
				} else {
					var limited *store.ErrStepUpSendLimit
					if !errors.As(err, &limited) {
						t.Error(err)
					}
				}
			}()
		}
		close(start)
		wg.Wait()
		if wins.Load() != 3 {
			t.Fatalf("sent %d times", wins.Load())
		}
	})
}
func TestStepUpSessionGrantDroppedByRotation(t *testing.T) {
	stepUpBackends(t, func(t *testing.T, s *store.Store) {
		u, session, tok := stepUpUser(t, s, "grant@example.com")
		until := time.Now().UTC().Add(10 * time.Minute).Truncate(time.Second)
		if err := s.GrantSessionStepUp(session.ID, until); err != nil {
			t.Fatal(err)
		}
		// Grant renewal must keep the original deadline.
		if err := s.GrantSessionStepUp(session.ID, until.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		info, err := s.ValidateSession(tok)
		if err != nil || info.StepUpUntil == nil || !info.StepUpUntil.Equal(until) {
			t.Fatalf("grant: %v %v", info, err)
		}
		rotated, err := s.RotateUserSessions(u.ID, u.CredentialEpoch, session.ID, "web", "", "", time.Hour, session.CreatedAt)
		if err != nil {
			t.Fatal(err)
		}
		if old, err := s.ValidateSession(tok); err != nil || old != nil {
			t.Fatalf("old session: %v %v", old, err)
		}
		info, err = s.ValidateSession(rotated)
		if err != nil || info == nil || info.StepUpUntil != nil {
			t.Fatalf("rotated grant: %v %v", info, err)
		}
	})
}
func TestUpdateUserAPITokenCASAndOwnerScope(t *testing.T) {
	stepUpBackends(t, func(t *testing.T, s *store.Store) {
		u, _, _ := stepUpUser(t, s, "token@example.com")
		other, _, _ := stepUpUser(t, s, "foreign@example.com")
		tok, err := s.CreateAPIToken(u.ID, models.APITokenCreate{Name: "Key", Scopes: `["read"]`}, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{tok.ID, "unknown"} {
			got, err := s.GetUserAPIToken(id, other.ID)
			if err != nil || got != nil {
				t.Fatalf("owner scoped lookup: %v %v", got, err)
			}
		}
		name, scopes := "Renamed", `["*"]`
		updated, err := s.UpdateUserAPIToken(tok.ID, u.ID, &name, &scopes, tok.Scopes)
		if err != nil {
			t.Fatal(err)
		}
		if updated.Name != name || updated.Scopes != scopes || updated.Prefix != tok.Prefix {
			t.Fatalf("update metadata: %+v", updated)
		}
		if _, err := s.UpdateUserAPIToken(tok.ID, u.ID, nil, &scopes, tok.Scopes); !errors.Is(err, store.ErrTokenScopesChanged) {
			t.Fatalf("stale CAS: %v", err)
		}
		for _, id := range []string{tok.ID, "unknown"} {
			if _, err := s.UpdateUserAPIToken(id, other.ID, &name, nil, scopes); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("owner update %s: %v", id, err)
			}
		}
		// Expiry does not stop editing; revocation does.
		stepUpExec(t, s, `UPDATE api_tokens SET expires_at=? WHERE id=?`, time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), tok.ID)
		if _, err := s.UpdateUserAPIToken(tok.ID, u.ID, &name, nil, scopes); err != nil {
			t.Fatal(err)
		}
		valid, err := s.ValidateToken(tok.Token)
		if err != nil || valid != nil {
			t.Fatal("edit changed token expiry")
		}
		if err := s.DeleteUserAPIToken(tok.ID, u.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.UpdateUserAPIToken(tok.ID, u.ID, &name, nil, scopes); !errors.Is(err, sql.ErrNoRows) {
			t.Fatal(fmt.Sprint("revoked: ", err))
		}

		// A multi-scope key: the handler passes the scopes it read back as the
		// expected value, whatever text form the backend returns them in.
		multi, err := s.CreateAPIToken(u.ID, models.APITokenCreate{Name: "Multi", Scopes: `["read","write"]`}, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		read, err := s.GetUserAPIToken(multi.ID, u.ID)
		if err != nil || read == nil {
			t.Fatalf("multi read: %v %v", read, err)
		}
		narrowed := `["read"]`
		got, err := s.UpdateUserAPIToken(multi.ID, u.ID, nil, &narrowed, read.Scopes)
		if err != nil {
			t.Fatalf("multi CAS: %v", err)
		}
		if got.Scopes != narrowed {
			t.Fatalf("multi narrowed scopes: %q", got.Scopes)
		}
	})
}

func TestStepUpEnableTOTPRecordsEnrolment(t *testing.T) {
	stepUpBackends(t, func(t *testing.T, s *store.Store) {
		u, _, _ := stepUpUser(t, s, "totp-pin@example.com")
		for _, secret := range []string{"JBSWY3DPEHPK3PXP", "JBSWY3DPEHPK3PXQ"} {
			if err := s.SetTOTPSecret(u.ID, secret); err != nil {
				t.Fatal(err)
			}
			start := time.Now().UTC().Truncate(time.Second)
			if err := s.EnableTOTP(u.ID, secret, ""); err != nil {
				t.Fatal(err)
			}
			var raw sql.NullString
			if err := s.DB().QueryRow(s.D().Rebind(`SELECT totp_enabled_at FROM users WHERE id=?`), u.ID).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			enabledAt, err := time.Parse(time.RFC3339, raw.String)
			if err != nil || enabledAt.Before(start) || enabledAt.After(time.Now()) || enabledAt.Location() != time.UTC {
				t.Fatalf("enrolment timestamp: %q %v", raw.String, err)
			}
			read, err := s.GetUser(u.ID)
			if err != nil || read == nil || read.TOTPEnabledAt == nil || !read.TOTPEnabledAt.Equal(enabledAt) || !read.TOTPEnabled {
				t.Fatalf("read enrolled user: %+v %v", read, err)
			}
			if err := s.DisableTOTP(u.ID); err != nil {
				t.Fatal(err)
			}
			// A stale timestamp must be overwritten on the next enrolment.
			stepUpExec(t, s, `UPDATE users SET totp_enabled_at=? WHERE id=?`, start.Add(-time.Hour).Format(time.RFC3339), u.ID)
		}
	})
}

func TestStepUpSessionSignedInAt(t *testing.T) {
	stepUpBackends(t, func(t *testing.T, s *store.Store) {
		u, session, tok := stepUpUser(t, s, "sign-in@example.com")
		var raw sql.NullString
		if err := s.DB().QueryRow(s.D().Rebind(`SELECT signed_in_at FROM sessions WHERE id=?`), session.ID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if !raw.Valid || raw.String != session.CreatedAt.UTC().Format(time.RFC3339) || !session.SignedInAt.Equal(session.CreatedAt) {
			t.Fatalf("new session sign-in: raw=%v session=%+v", raw, session)
		}

		// Pre-migration rows read their own creation time as the sign-in time.
		stepUpExec(t, s, `UPDATE sessions SET signed_in_at=NULL WHERE id=?`, session.ID)
		legacy, err := s.ValidateSession(tok)
		if err != nil || legacy == nil || !legacy.SignedInAt.Equal(legacy.CreatedAt) {
			t.Fatalf("legacy sign-in: %+v %v", legacy, err)
		}

		for _, legacyRow := range []bool{true, false} {
			t.Run(fmt.Sprintf("rotation_legacy_%t", legacyRow), func(t *testing.T) {
				var signedInAt any
				want := session.CreatedAt
				if !legacyRow {
					want = session.CreatedAt.Add(-time.Hour)
					signedInAt = want.UTC().Format(time.RFC3339)
				}
				stepUpExec(t, s, `UPDATE sessions SET signed_in_at=? WHERE id=?`, signedInAt, session.ID)
				replacement, err := s.RotateUserSessions(u.ID, u.CredentialEpoch, session.ID, "web", "", "", time.Hour, session.CreatedAt)
				if err != nil {
					t.Fatal(err)
				}
				session, err = s.ValidateSession(replacement)
				if err != nil || session == nil || !session.SignedInAt.Equal(want) {
					t.Fatalf("rotated sign-in: %+v want %v: %v", session, want, err)
				}
			})
		}
	})
}
