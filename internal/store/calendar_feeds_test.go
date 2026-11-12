package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestCalendarFeedSecretsCapAndRevocation(t *testing.T) {
	s := testStore(t)
	a, b := tabsUser(t, s, "Feeda"), tabsUser(t, s, "Feedb")
	ctx := context.Background()
	f, token, err := s.CreateCalendarFeed(ctx, a.ID, "mine", nil, "Personal")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, "padics_") || len(token) != 50 {
		t.Fatal("invalid token shape")
	}
	h := sha256.Sum256([]byte(token))
	var stored string
	if err := s.db.QueryRow(s.q(`SELECT token_hash FROM calendar_feeds WHERE id=?`), f.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != hex.EncodeToString(h[:]) {
		t.Fatal("token not hashed")
	}
	rows, err := s.db.Query(`SELECT * FROM calendar_feeds`)
	if err != nil {
		t.Fatal(err)
	}
	cols, _ := rows.Columns()
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		for _, v := range vals {
			if v == token || stringValue(v) == token {
				t.Fatal("plaintext token stored")
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	for i := 1; i < 10; i++ {
		if _, _, err := s.CreateCalendarFeed(ctx, a.ID, "agent", nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.CreateCalendarFeed(ctx, a.ID, "mine", nil, ""); !errors.Is(err, ErrCalendarFeedLimit) {
		t.Fatalf("cap: %v", err)
	}
	if err := s.RevokeCalendarFeed(ctx, b.ID, f.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("ownership: %v", err)
	}
	if err := s.RevokeCalendarFeed(ctx, a.ID, f.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateCalendarFeed(ctx, a.ID, "mine", nil, ""); err != nil {
		t.Fatal(err)
	}
	other, _, err := s.CreateCalendarFeed(ctx, b.ID, "agent", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeUserCalendarFeeds(a.ID); err != nil {
		t.Fatal(err)
	}
	feeds, err := s.ListCalendarFeeds(ctx, a.ID)
	if err != nil || len(feeds) != 0 {
		t.Fatalf("revoke: %d %v", len(feeds), err)
	}
	feeds, err = s.ListCalendarFeeds(ctx, b.ID)
	if err != nil || len(feeds) != 1 || feeds[0].ID != other.ID {
		t.Fatal("other user revoked")
	}
	wire, _ := json.Marshal(feeds)
	if strings.Contains(string(wire), "token") || strings.Contains(string(wire), "hash") {
		t.Fatal("secret metadata exposed")
	}
}
func stringValue(v any) string {
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return ""
}

func TestCalendarFeedDisableAndDelete(t *testing.T) {
	for _, op := range []string{"disable", "delete"} {
		t.Run(op, func(t *testing.T) {
			s := testStore(t)
			u := tabsUser(t, s, "Feedlife")
			_, token, err := s.CreateCalendarFeed(context.Background(), u.ID, "mine", nil, "")
			if err != nil {
				t.Fatal(err)
			}
			if op == "disable" {
				err = s.DisableUserAndRevokeAccess(u.ID)
			} else {
				err = s.DeleteAccountAtomic(u.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			f, _, err := s.LookupCalendarFeed(context.Background(), token)
			if err != nil || f != nil {
				t.Fatal("feed survived lifecycle")
			}
			var n int
			query := `SELECT COUNT(*) FROM calendar_feeds WHERE user_id=?`
			if op == "disable" {
				query += ` AND revoked_at IS NULL`
			}
			if err := s.db.QueryRow(s.q(query), u.ID).Scan(&n); err != nil || n != 0 {
				t.Fatalf("remaining %d: %v", n, err)
			}
		})
	}
}

func TestCalendarFeedMintRequiresLiveHuman(t *testing.T) {
	for _, state := range []string{"disabled", "app"} {
		t.Run(state, func(t *testing.T) {
			s := testStore(t)
			u := tabsUser(t, s, "Feedmint")
			query := `UPDATE users SET disabled_at='2031-03-10' WHERE id=?`
			if state == "app" {
				query = `UPDATE users SET kind='app' WHERE id=?`
			}
			if _, err := s.db.Exec(s.q(query), u.ID); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.CreateCalendarFeed(context.Background(), u.ID, "mine", nil, ""); !errors.Is(err, ErrUserDisabled) {
				t.Fatalf("%s mint: %v", state, err)
			}
		})
	}
}

// A claim keeps the users row, so no cascade fires: the previous holder's
// feed URLs must stop working, or they would show the claimant's workspaces.
func TestCalendarFeedRevokedByAccountClaim(t *testing.T) {
	s := testStore(t)
	u := createUnverifiedUser(t, s, "feedclaim@example.com")
	_, token, err := s.CreateCalendarFeed(context.Background(), u.ID, "mine", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimAccountByProvider(u.ID, "google", "", ""); err != nil {
		t.Fatalf("claim: %v", err)
	}
	f, _, err := s.LookupCalendarFeed(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	if f != nil {
		t.Fatal("the previous holder's feed still resolves after the claim")
	}
}
