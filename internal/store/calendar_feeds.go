package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

const MaxCalendarFeeds = 10

var ErrCalendarFeedLimit = errors.New("calendar feed limit reached")

// CalendarFeed contains public metadata only. Credentials cannot be retrieved.
type CalendarFeed struct {
	ID           string   `json:"id"`
	Layer        string   `json:"layer"`
	WorkspaceIDs []string `json:"workspace_ids"`
	Name         string   `json:"name"`
	CreatedAt    string   `json:"created_at"`
	LastUsedAt   *string  `json:"last_used_at"`
}

func calendarFeedHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}
func validCalendarFeedToken(token string) bool {
	if len(token) != 50 || token[:7] != "padics_" {
		return false
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(token[7:])
	return err == nil && len(b) == 32
}
func (s *Store) CreateCalendarFeed(ctx context.Context, userID, layer string, ids []string, name string) (*CalendarFeed, string, error) {
	tx, err := s.beginWorkspaceGroupWrite(ctx, userID)
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback()
	// Take the cap lock first, then recheck account state before minting.
	if err := s.requireActiveUserTx(tx, userID); err != nil {
		return nil, "", err
	}
	var count int
	if err := tx.QueryRowContext(ctx, s.q(`SELECT COUNT(*) FROM calendar_feeds WHERE user_id=? AND revoked_at IS NULL`), userID).Scan(&count); err != nil {
		return nil, "", err
	}
	if count >= MaxCalendarFeeds {
		return nil, "", ErrCalendarFeedLimit
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, "", err
	}
	token := "padics_" + base64.RawURLEncoding.EncodeToString(b)
	if ids == nil {
		ids = []string{}
	}
	data, err := json.Marshal(ids)
	if err != nil {
		return nil, "", err
	}
	f := &CalendarFeed{ID: newID(), Layer: layer, WorkspaceIDs: ids, Name: name, CreatedAt: nowNano()}
	_, err = tx.ExecContext(ctx, s.q(`INSERT INTO calendar_feeds (id,user_id,token_hash,layer,workspace_ids,name,created_at) VALUES (?,?,?,?,?,?,?)`), f.ID, userID, calendarFeedHash(token), layer, string(data), name, f.CreatedAt)
	if err != nil {
		return nil, "", err
	}
	if err := tx.Commit(); err != nil {
		return nil, "", err
	}
	return f, token, nil
}
func scanCalendarFeed(row interface{ Scan(...any) error }) (*CalendarFeed, error) {
	var f CalendarFeed
	var ids string
	if err := row.Scan(&f.ID, &f.Layer, &ids, &f.Name, &f.CreatedAt, &f.LastUsedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(ids), &f.WorkspaceIDs); err != nil {
		return nil, err
	}
	return &f, nil
}
func (s *Store) ListCalendarFeeds(ctx context.Context, userID string) ([]CalendarFeed, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT id,layer,workspace_ids,name,created_at,last_used_at FROM calendar_feeds WHERE user_id=? AND revoked_at IS NULL ORDER BY created_at DESC,id DESC`), userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CalendarFeed{}
	for rows.Next() {
		f, err := scanCalendarFeed(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *f)
	}
	return out, rows.Err()
}
func (s *Store) RevokeCalendarFeed(ctx context.Context, userID, id string) error {
	result, err := s.db.ExecContext(ctx, s.q(`UPDATE calendar_feeds SET revoked_at=? WHERE id=? AND user_id=? AND revoked_at IS NULL`), nowNano(), id, userID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
func (s *Store) RevokeUserCalendarFeeds(userID string) error {
	tx, err := s.beginWorkspaceGroupWrite(context.Background(), userID)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.RevokeUserCalendarFeedsTx(tx, userID); err != nil {
		return err
	}
	return tx.Commit()
}

// RevokeUserCalendarFeedsTx participates in credential revocation with the
// user row locked before feed rows, the same lock order as feed creation.
func (s *Store) RevokeUserCalendarFeedsTx(tx *sql.Tx, userID string) error {
	_, err := tx.Exec(s.q(`UPDATE calendar_feeds SET revoked_at=? WHERE user_id=? AND revoked_at IS NULL`), nowNano(), userID)
	return err
}

// LookupCalendarFeed gates live human ownership in the lookup itself. Invalid
// token shapes return without querying, and no error includes the credential.
func (s *Store) LookupCalendarFeed(ctx context.Context, token string) (*CalendarFeed, *models.User, error) {
	if !validCalendarFeedToken(token) {
		return nil, nil, nil
	}
	var f CalendarFeed
	var ids string
	var u models.User
	err := s.db.QueryRowContext(ctx, s.q(`SELECT f.id,f.layer,f.workspace_ids,f.name,f.created_at,f.last_used_at,u.id,u.username,u.timezone,u.kind
 FROM calendar_feeds f JOIN users u ON u.id=f.user_id
 WHERE f.token_hash=? AND f.revoked_at IS NULL AND u.disabled_at IS NULL AND u.kind='human'`), calendarFeedHash(token)).Scan(&f.ID, &f.Layer, &ids, &f.Name, &f.CreatedAt, &f.LastUsedAt, &u.ID, &u.Username, &u.Timezone, &u.Kind)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if err := json.Unmarshal([]byte(ids), &f.WorkspaceIDs); err != nil {
		return nil, nil, err
	}
	return &f, &u, nil
}

// TouchCalendarFeed uses a conditional write so concurrent polls also write at
// most once an hour. The read-side caller skips even this write when recent.
func (s *Store) TouchCalendarFeed(ctx context.Context, id string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, s.q(`UPDATE calendar_feeds SET last_used_at=? WHERE id=? AND revoked_at IS NULL AND (last_used_at IS NULL OR last_used_at<=?)`), at.UTC().Format(time.RFC3339Nano), id, at.Add(-time.Hour).UTC().Format(time.RFC3339Nano))
	return err
}

// DeleteUserSessionsAndRevokeCalendarFeeds invalidates both bearer credentials
// atomically after password recovery, under the same user lock as creation.
func (s *Store) DeleteUserSessionsAndRevokeCalendarFeeds(userID string) error {
	tx, err := s.beginWorkspaceGroupWrite(context.Background(), userID)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(s.q(`DELETE FROM sessions WHERE user_id=?`), userID); err != nil {
		return err
	}
	if err := s.RevokeUserCalendarFeedsTx(tx, userID); err != nil {
		return err
	}
	return tx.Commit()
}
