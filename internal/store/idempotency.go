package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// TASK-3585: idempotency keys for agent writes. See migration 139 for the
// row's life. The rule the whole design rests on: a row moves to `done` only
// inside the write's own transaction, guarded by its claim token, so `done`
// holds exactly when the write committed. A `pending` row whose lease
// expired therefore committed nothing, and taking it over is safe.

// Idempotency claim outcomes.
const (
	IdemClaimed  = "claimed"   // the caller owns the key: do the write
	IdemReplay   = "replay"    // the write committed earlier: answer with Status/Response
	IdemInFlight = "in_flight" // another request holds a live lease on it
	IdemMismatch = "mismatch"  // the key was used for a different request
	IdemUnknown  = "unknown"   // a patch reached an editor and was never confirmed; the caller owns the retry
)

// IdemClaim is the answer to a claim.
type IdemClaim struct {
	Outcome  string
	Token    string
	Status   int
	Response string
	PatchID  string
	ConnID   string
	BaseSeq  int64
	BaseBody string
}

// ErrIdempotencyLeaseLost: the claim was taken over (its lease expired)
// before this write finished. Returned from inside the write's transaction,
// so the write rolls back and the taker-over's write is the only one.
var ErrIdempotencyLeaseLost = errors.New("idempotency key: the claim's lease was taken over before the write committed")

// idemTimeFormat is fixed-width (nanoseconds always present), because the
// lease and TTL comparisons are done on the stored TEXT: RFC3339Nano drops
// trailing zeros, and then "...:00Z" sorts after "...:00.5Z".
const idemTimeFormat = "2006-01-02T15:04:05.000000000Z"

// IdempotencyTTL is how long a key is remembered.
const IdempotencyTTL = 24 * time.Hour

// ClaimIdempotencyKey claims (workspace, user, key) for a request whose
// canonical hash is requestHash, or reports why it cannot.
func (s *Store) ClaimIdempotencyKey(workspaceID, userID, key, route, requestHash string, lease time.Duration) (*IdemClaim, error) {
	now := time.Now().UTC()
	ts := now.Format(idemTimeFormat)
	until := now.Add(lease).Format(idemTimeFormat)
	token := uuid.NewString()
	res, err := s.db.Exec(s.q(`
		INSERT INTO idempotency_keys (workspace_id, user_id, key, route, request_hash, state, claim_token, lease_until, created_at)
		VALUES (?, ?, ?, ?, ?, 'pending', ?, ?, ?)
		ON CONFLICT (workspace_id, user_id, key) DO NOTHING`),
		workspaceID, userID, key, route, requestHash, token, until, ts)
	if err != nil {
		return nil, fmt.Errorf("claim idempotency key: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return &IdemClaim{Outcome: IdemClaimed, Token: token}, nil
	}
	var (
		hash, state                       string
		lease0, resp, patchID, conn, body sql.NullString
		status, baseSeq                   sql.NullInt64
	)
	if err := s.db.QueryRow(s.q(`
		SELECT request_hash, state, lease_until, status, response, patch_id, conn_id, base_seq, base_body
		FROM idempotency_keys WHERE workspace_id = ? AND user_id = ? AND key = ?`),
		workspaceID, userID, key).Scan(&hash, &state, &lease0, &status, &resp, &patchID, &conn, &baseSeq, &body); err != nil {
		return nil, fmt.Errorf("read idempotency key: %w", err)
	}
	if hash != requestHash {
		return &IdemClaim{Outcome: IdemMismatch}, nil
	}
	if state == "done" {
		return &IdemClaim{Outcome: IdemReplay, Status: int(status.Int64), Response: resp.String}, nil
	}
	// pending or unknown: owned by whoever holds a live lease. Take it over
	// only when the lease is absent or expired, with a new token.
	res, err = s.db.Exec(s.q(`
		UPDATE idempotency_keys SET claim_token = ?, lease_until = ?
		WHERE workspace_id = ? AND user_id = ? AND key = ? AND state = ? AND (lease_until IS NULL OR lease_until < ?)`),
		token, until, workspaceID, userID, key, state, ts)
	if err != nil {
		return nil, fmt.Errorf("take over idempotency key: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return &IdemClaim{Outcome: IdemInFlight}, nil
	}
	if state == "unknown" {
		return &IdemClaim{Outcome: IdemUnknown, Token: token, PatchID: patchID.String, ConnID: conn.String, BaseSeq: baseSeq.Int64, BaseBody: body.String}, nil
	}
	return &IdemClaim{Outcome: IdemClaimed, Token: token}, nil
}

// CompleteIdempotencyKeyTx records the answer inside the write's own
// transaction. It fails, rolling the write back, unless this token still
// holds the claim.
func (s *Store) CompleteIdempotencyKeyTx(tx *sql.Tx, workspaceID, userID, key, token string, status int, response string) error {
	res, err := tx.Exec(s.q(`
		UPDATE idempotency_keys SET state = 'done', status = ?, response = ?, lease_until = NULL
		WHERE workspace_id = ? AND user_id = ? AND key = ? AND claim_token = ? AND state IN ('pending', 'unknown')`),
		status, response, workspaceID, userID, key, token)
	if err != nil {
		return fmt.Errorf("complete idempotency key: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrIdempotencyLeaseLost
	}
	return nil
}

// CompleteIdempotencyKey is CompleteIdempotencyKeyTx for a write that has
// no transaction of its own (push, TASK-3585), in a transaction of its own.
func (s *Store) CompleteIdempotencyKey(workspaceID, userID, key, token string, status int, response string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.CompleteIdempotencyKeyTx(tx, workspaceID, userID, key, token, status, response); err != nil {
		return err
	}
	return tx.Commit()
}

// ReleaseIdempotencyKey gives up a claim the write refused, so a corrected
// retry with the same key can proceed (lead ruling: refusals are not
// stored). A pending claim is deleted; an unknown one keeps its record and
// only drops the lease.
func (s *Store) ReleaseIdempotencyKey(workspaceID, userID, key, token string) error {
	if _, err := s.db.Exec(s.q(`DELETE FROM idempotency_keys WHERE workspace_id = ? AND user_id = ? AND key = ? AND claim_token = ? AND state = 'pending'`),
		workspaceID, userID, key, token); err != nil {
		return err
	}
	_, err := s.db.Exec(s.q(`UPDATE idempotency_keys SET lease_until = NULL WHERE workspace_id = ? AND user_id = ? AND key = ? AND claim_token = ? AND state = 'unknown'`),
		workspaceID, userID, key, token)
	return err
}

// MarkIdempotencyKeyUnknown records a patch that reached an open editor and
// was never confirmed: only that connection may resolve it.
func (s *Store) MarkIdempotencyKeyUnknown(workspaceID, userID, key, token, patchID, connID string, baseSeq int64, baseBody string) error {
	_, err := s.db.Exec(s.q(`
		UPDATE idempotency_keys SET state = 'unknown', patch_id = ?, conn_id = ?, base_seq = ?, base_body = ?, lease_until = NULL
		WHERE workspace_id = ? AND user_id = ? AND key = ? AND claim_token = ?`),
		patchID, connID, baseSeq, baseBody, workspaceID, userID, key, token)
	return err
}

// SweepIdempotencyKeys deletes keys older than the TTL.
func (s *Store) SweepIdempotencyKeys(now time.Time) (int64, error) {
	res, err := s.db.Exec(s.q(`DELETE FROM idempotency_keys WHERE created_at < ?`), now.Add(-IdempotencyTTL).UTC().Format(idemTimeFormat))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
