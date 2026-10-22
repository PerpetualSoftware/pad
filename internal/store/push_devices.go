package store

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// MaxPushDevicesPerUser caps the devices one user keeps registered. A register
// that would exceed it evicts the oldest, silently (TASK-3938).
const MaxPushDevicesPerUser = 20

// ErrPushSessionGone is returned by RegisterPushDevice when the session it was
// asked to bind no longer exists or has expired. The handler answers 401: the
// caller's credential died between admission and the write.
var ErrPushSessionGone = errors.New("session is gone or expired")

// PushDevice is one registered push token, bound to the session that
// registered it. The row dies with the session or the user.
type PushDevice struct {
	ID           string
	UserID       string
	SessionID    string
	ConnectionID string
	Platform     string
	Token        string
	AppVersion   string
	CreatedAt    string
	UpdatedAt    string
	// RegSeq is the registration sequence, set by LivePushDevices only.
	RegSeq int64
}

// RegisterPushDevice binds a push token to a session in ONE transaction. When
// this user already holds the token, the row is UPDATED in place: id and
// created_at (the device's first registration, which the dispatcher's
// predates-device gate reads) are kept, the session, connection, platform and
// app version move to the caller, updated_at is refreshed, and the caller's own
// session row for any OTHER token is deleted first (session_id is unique). The
// cap loop is skipped because the row count cannot grow (TASK-4027). Otherwise
// it deletes every row on EITHER unique key (this session's own row, whatever
// its token, and this user's row for this token, whatever its session), evicts
// the oldest devices while the user is at the cap, then inserts. A single-key
// upsert cannot clear the other key and collides with it (TASK-3938).
//
// On Postgres the user row is locked first so two concurrent registers
// serialize and the cap holds. A session that is gone or expired is
// ErrPushSessionGone.
func (s *Store) RegisterPushDevice(userID, sessionID, connectionID, platform, token, appVersion string) (*PushDevice, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("register push device: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if s.dialect.Driver() == DriverPostgres {
		var locked string
		if err := tx.QueryRow(s.q(`SELECT id FROM users WHERE id = ? FOR UPDATE`), userID).Scan(&locked); err != nil {
			return nil, fmt.Errorf("register push device: lock user: %w", err)
		}
	}

	// The session must be live under the same transaction that binds to it.
	// FOR SHARE on Postgres keeps a concurrent logout from deleting it before
	// the insert commits (the FK would refuse the insert, but as a 500).
	sessQ := `SELECT expires_at FROM sessions WHERE id = ? AND user_id = ?`
	if s.dialect.Driver() == DriverPostgres {
		sessQ += ` FOR SHARE`
	}
	var expiresAt string
	switch err := tx.QueryRow(s.q(sessQ), sessionID, userID).Scan(&expiresAt); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrPushSessionGone
	case err != nil:
		return nil, fmt.Errorf("register push device: read session: %w", err)
	}
	if parseTime(expiresAt).Before(time.Now().UTC()) {
		return nil, ErrPushSessionGone
	}

	var keptID, keptCreated string
	switch err := tx.QueryRow(s.q(`SELECT id, created_at FROM push_devices WHERE user_id = ? AND token = ?`),
		userID, token).Scan(&keptID, &keptCreated); {
	case err == nil:
		if _, err := tx.Exec(s.q(`DELETE FROM push_devices WHERE session_id = ? AND id <> ?`), sessionID, keptID); err != nil {
			return nil, fmt.Errorf("register push device: clear session: %w", err)
		}
		ts := now()
		if _, err := tx.Exec(s.q(`
			UPDATE push_devices SET session_id = ?, connection_id = ?, platform = ?, app_version = ?, updated_at = ?, reg_seq = `+s.regSeqUpdateExpr()+`
			WHERE id = ?`), sessionID, connectionID, platform, appVersion, ts, keptID); err != nil {
			return nil, fmt.Errorf("register push device: update: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("register push device: commit: %w", err)
		}
		return &PushDevice{
			ID: keptID, UserID: userID, SessionID: sessionID, ConnectionID: connectionID,
			Platform: platform, Token: token, AppVersion: appVersion, CreatedAt: keptCreated, UpdatedAt: ts,
		}, nil
	case !errors.Is(err, sql.ErrNoRows):
		return nil, fmt.Errorf("register push device: lookup token: %w", err)
	}

	if _, err := tx.Exec(s.q(`DELETE FROM push_devices WHERE session_id = ? OR (user_id = ? AND token = ?)`),
		sessionID, userID, token); err != nil {
		return nil, fmt.Errorf("register push device: clear keys: %w", err)
	}

	for {
		var count int
		if err := tx.QueryRow(s.q(`SELECT COUNT(*) FROM push_devices WHERE user_id = ?`), userID).Scan(&count); err != nil {
			return nil, fmt.Errorf("register push device: count: %w", err)
		}
		if count < MaxPushDevicesPerUser {
			break
		}
		var oldest string
		if err := tx.QueryRow(s.q(`SELECT id FROM push_devices WHERE user_id = ? ORDER BY updated_at, created_at, id LIMIT 1`),
			userID).Scan(&oldest); err != nil {
			return nil, fmt.Errorf("register push device: find oldest: %w", err)
		}
		if _, err := tx.Exec(s.q(`DELETE FROM push_devices WHERE id = ?`), oldest); err != nil {
			return nil, fmt.Errorf("register push device: evict: %w", err)
		}
		// The device id only: a push token is a credential and is never logged.
		slog.Debug("evicted the oldest push device at the cap", "user_id", userID, "device_id", oldest)
	}

	ts := now()
	d := &PushDevice{
		ID: newID(), UserID: userID, SessionID: sessionID, ConnectionID: connectionID,
		Platform: platform, Token: token, AppVersion: appVersion, CreatedAt: ts, UpdatedAt: ts,
	}
	if _, err := tx.Exec(s.q(`
		INSERT INTO push_devices (id, user_id, session_id, connection_id, platform, token, app_version, created_at, updated_at, reg_seq)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, `+s.regSeqInsertExpr()+`)`),
		d.ID, d.UserID, d.SessionID, d.ConnectionID, d.Platform, d.Token, d.AppVersion, d.CreatedAt, d.UpdatedAt); err != nil {
		return nil, fmt.Errorf("register push device: insert: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("register push device: commit: %w", err)
	}
	return d, nil
}

// regSeqUpdateExpr is the SQL value a register writes to reg_seq on a row it
// UPDATEs (TASK-4031). SQLite: MAX+1, race-free because every write
// transaction is BEGIN IMMEDIATE. Postgres: the sequence, which hands two
// concurrent registrants distinct values (the user FOR UPDATE lock covers only
// one user).
func (s *Store) regSeqUpdateExpr() string {
	if s.dialect.Driver() == DriverPostgres {
		return `nextval('push_devices_reg_seq')`
	}
	return `(SELECT COALESCE(MAX(reg_seq), 0) + 1 FROM push_devices)`
}

// regSeqInsertExpr is the value an INSERT writes: the same as the UPDATE's on
// SQLite; on Postgres the column default is nextval, so an explicit call is the
// same thing and keeps the two writers visibly alike.
func (s *Store) regSeqInsertExpr() string { return s.regSeqUpdateExpr() }

// DeletePushDevice removes the device bound to this session under this
// connection id. Absent is not an error: the call is idempotent.
func (s *Store) DeletePushDevice(userID, sessionID, connectionID string) error {
	if _, err := s.db.Exec(s.q(`DELETE FROM push_devices WHERE user_id = ? AND session_id = ? AND connection_id = ?`),
		userID, sessionID, connectionID); err != nil {
		return fmt.Errorf("delete push device: %w", err)
	}
	return nil
}

// ListPushDevices returns userID's devices, oldest first by updated_at.
func (s *Store) ListPushDevices(userID string) ([]PushDevice, error) {
	rows, err := s.db.Query(s.q(`
		SELECT id, user_id, session_id, connection_id, platform, token, app_version, created_at, updated_at
		FROM push_devices WHERE user_id = ? ORDER BY updated_at, created_at, id`), userID)
	if err != nil {
		return nil, fmt.Errorf("list push devices: %w", err)
	}
	defer rows.Close()
	var out []PushDevice
	for rows.Next() {
		var d PushDevice
		if err := rows.Scan(&d.ID, &d.UserID, &d.SessionID, &d.ConnectionID, &d.Platform, &d.Token, &d.AppVersion, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, fmt.Errorf("list push devices: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
