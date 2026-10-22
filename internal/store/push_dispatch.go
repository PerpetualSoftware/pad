package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// The push queue's dispatch side (TASK-3939 part A): claiming, settling and
// pruning user_notifications rows, and reading the devices a row may be pushed
// to. The privacy gates themselves (membership, visibility, settings) live in
// the dispatcher; these are the primitives it stands on.
//
// Times. user_notifications.created_at / claimed_at / sent_at,
// push_devices.created_at and sessions.expires_at are all TEXT on BOTH
// dialects, written by now() or time.Format(time.RFC3339) on a UTC time: a
// fixed-width "2006-01-02T15:04:05Z", which sorts as it reads. Comparisons
// here are string comparisons in SQL against a cutoff formatted by cutoffTS,
// exactly as the outbox lease and CleanExpiredSessions already do.

// MaxNotificationAttempts is how many times a row may be released after a
// retryable gateway failure before it is skipped as gateway_failed.
const MaxNotificationAttempts = 5

// Skip reasons the store itself writes.
const SkipReasonGatewayFailed = "gateway_failed"

var (
	// ErrNotificationNotFound is returned when a notification does not exist
	// for the asking user. The two cases are deliberately one error.
	ErrNotificationNotFound = errors.New("notification not found")
	// ErrNotificationSubjectGone is returned when the item a notification is
	// about is missing, soft-deleted, or in an archived collection.
	ErrNotificationSubjectGone = errors.New("notification subject is gone")
	// errEmptyClaimToken guards the claim_token = ? predicates: unclaimed rows
	// carry the empty token, so an empty one would match all of them.
	errEmptyClaimToken = errors.New("notification claim token is empty")
)

// Notification is one user_notifications row.
type Notification struct {
	ID           string
	UserID       string
	WorkspaceID  string
	ItemID       string
	CommentID    string
	Kind         string
	ActorUserID  string
	ActorIsAgent bool
	CreatedAt    string
	PushState    string
	SkipReason   string
	Attempts     int
	SentAt       string
}

const notificationColumns = `id, user_id, workspace_id, item_id, COALESCE(comment_id, ''), kind,
	COALESCE(actor_user_id, ''), actor_is_agent, created_at, push_state, skip_reason, attempts, COALESCE(sent_at, '')`

type rowScanner interface{ Scan(dest ...any) error }

func (s *Store) scanNotification(r rowScanner) (Notification, error) {
	var n Notification
	var agent any
	if err := r.Scan(&n.ID, &n.UserID, &n.WorkspaceID, &n.ItemID, &n.CommentID, &n.Kind,
		&n.ActorUserID, &agent, &n.CreatedAt, &n.PushState, &n.SkipReason, &n.Attempts, &n.SentAt); err != nil {
		return n, err
	}
	switch v := agent.(type) {
	case bool:
		n.ActorIsAgent = v
	case int64:
		n.ActorIsAgent = v != 0
	}
	return n, nil
}

func cutoffTS(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// idPlaceholders returns "?,?,?" for n ids, and the ids as args.
func idPlaceholders(ids []string) (string, []any) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return strings.TrimSuffix(strings.Repeat("?,", len(ids)), ","), args
}

// Claim is the outcome of one ClaimPendingNotifications call: the rows it
// claimed and the per-claim token that now holds them. Token is what
// MarkNotificationsSent, MarkNotificationsSkipped and ReleaseNotifications
// take.
type Claim struct {
	Token string
	Rows  []Notification
}

// notificationIDChunk bounds the ids in one IN list.
const notificationIDChunk = 500

// maxNotificationClaim caps one claim at the gateway's batch size
// (notify.MaxBatch; the store test pins the two together). A limit above it
// claims maxNotificationClaim rows, and a limit <= 0 claims nothing: SQLite
// reads a negative LIMIT as unbounded, and the read-back IN list is not
// chunked (TASK-3939 r4-L3, r5-M1).
const maxNotificationClaim = 100

// ClaimPendingNotifications claims up to limit pending rows for token: rows
// at least as old as settleCutoff (so a burst settles before it is pushed) and
// not held by a live claim (claimed_at older than leaseCutoff, or never).
// Each candidate is claimed by its own conditional UPDATE carrying the same
// availability predicate, so two claimers get disjoint sets. Oldest first.
//
// The result is ONLY the rows this call claimed: a subset of this call's
// candidate ids. It never exceeds limit and never includes a row the token
// already held from an earlier call (TASK-3939 r1-H1). A held row whose lease
// lapsed is a candidate again and is returned with claimed_at refreshed.
//
// expireCutoff is the expiry age's lower bound (TASK-4032 r5 H-Q1): a row
// created before it is never selected, so the claim does not depend on the
// expiry sweep having run and succeeded. It is the exact complement of the
// dispatcher's rule for dropping a parked entry (created before the same
// second-truncated cutoff). The zero time bounds nothing.
//
// exclude lists ids that are never selected (TASK-4032 codex r1 M1): the
// dispatcher passes the rows it already delivered but could not record, so
// they cannot fill the batch ahead of rows that still need a push. nil or
// empty excludes nothing. The list is one bind parameter per id; the caller
// keeps it to its 1000-entry cap, far under both dialects' limits.
//
// Each call stamps its OWN claim token: the caller's token plus a per-call
// random nonce, returned as Claim.Token. Mark, skip and release take that
// token, never the caller's. Reusing a caller token across calls, even in the
// same second (claimed_at has second precision), is therefore safe by
// construction: two such calls can never read back each other's rows
// (TASK-3939 r2-F1).
func (s *Store) ClaimPendingNotifications(token string, limit int, settleCutoff, leaseCutoff, expireCutoff time.Time, exclude []string) (Claim, error) {
	if token == "" {
		return Claim{}, errEmptyClaimToken
	}
	if limit <= 0 {
		return Claim{}, nil // no rows, no write: nothing to settle
	}
	limit = min(limit, maxNotificationClaim)
	settle, lease, expire := cutoffTS(settleCutoff), cutoffTS(leaseCutoff), cutoffTS(expireCutoff)
	notIn := ""
	args := []any{settle, expire, lease}
	if len(exclude) > 0 {
		ph, exArgs := idPlaceholders(exclude)
		notIn = " AND id NOT IN (" + ph + ")"
		args = append(args, exArgs...)
	}
	args = append(args, limit)
	rows, err := s.db.Query(s.q(`
		SELECT id FROM user_notifications
		WHERE push_state = 'pending'
		  AND created_at <= ? AND created_at >= ?
		  AND (claimed_at IS NULL OR claimed_at < ?)`+notIn+`
		ORDER BY created_at, id
		LIMIT ?`), args...)
	if err != nil {
		return Claim{}, fmt.Errorf("claim notifications: select candidates: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return Claim{}, fmt.Errorf("claim notifications: scan candidate: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return Claim{}, fmt.Errorf("claim notifications: candidates: %w", err)
	}
	rows.Close()
	if len(ids) == 0 {
		return Claim{}, nil
	}
	return s.claimNotificationIDs(token, ids, settle, expire, lease, now())
}

// claimNotificationIDs is the ARBITER: one conditional UPDATE per candidate,
// each carrying its own availability predicate, stamped with ts; it then reads
// back the candidates that now carry this call's claim token AND ts. Split out so a test
// can hand it a stale candidate list (a row another claimer took since the
// select) and a fixed ts, which the public entry point's own select would
// already have filtered.
func (s *Store) claimNotificationIDs(callerToken string, ids []string, settle, expire, lease, ts string) (Claim, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return Claim{}, fmt.Errorf("claim notifications: nonce: %w", err)
	}
	token := callerToken + "." + hex.EncodeToString(nonce[:])
	tx, err := s.db.Begin()
	if err != nil {
		return Claim{}, fmt.Errorf("claim notifications: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, id := range ids {
		if _, err := tx.Exec(s.q(`
			UPDATE user_notifications
			SET claim_token = ?, claimed_at = ?
			WHERE id = ?
			  AND push_state = 'pending'
			  AND created_at <= ? AND created_at >= ?
			  AND (claimed_at IS NULL OR claimed_at < ?)`), token, ts, id, settle, expire, lease); err != nil {
			return Claim{}, fmt.Errorf("claim notification: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Claim{}, fmt.Errorf("claim notifications: %w", err)
	}

	ph, idArgs := idPlaceholders(ids)
	args := append(idArgs, token, ts)
	out, err := s.db.Query(s.q(`SELECT `+notificationColumns+`
		FROM user_notifications
		WHERE id IN (`+ph+`) AND claim_token = ? AND claimed_at = ? AND push_state = 'pending'
		ORDER BY created_at, id`), args...)
	if err != nil {
		return Claim{}, fmt.Errorf("claim notifications: read back: %w", err)
	}
	defer out.Close()
	var claimed []Notification
	for out.Next() {
		n, err := s.scanNotification(out)
		if err != nil {
			return Claim{}, fmt.Errorf("claim notifications: scan: %w", err)
		}
		claimed = append(claimed, n)
	}
	if err := out.Err(); err != nil {
		return Claim{}, fmt.Errorf("claim notifications: read back: %w", err)
	}
	return Claim{Token: token, Rows: claimed}, nil
}

// ExpireStaleNotifications turns pending rows created before createdCutoff
// into 'expired': a push that late is noise. A row under a live claim (claimed
// after leaseCutoff) is left to its claimer. Returns how many rows expired.
func (s *Store) ExpireStaleNotifications(createdCutoff, leaseCutoff time.Time) (int64, error) {
	res, err := s.db.Exec(s.q(`
		UPDATE user_notifications
		SET push_state = 'expired', claim_token = '', claimed_at = NULL
		WHERE push_state = 'pending'
		  AND created_at < ?
		  AND (claimed_at IS NULL OR claimed_at < ?)`), cutoffTS(createdCutoff), cutoffTS(leaseCutoff))
	if err != nil {
		return 0, fmt.Errorf("expire notifications: %w", err)
	}
	return res.RowsAffected()
}

// settleNotifications runs one UPDATE per chunk of ids, each conditioned on
// the claim and on the row still being pending, so a claimer whose lease
// lapsed cannot stamp a row another instance now holds. It returns the rows
// changed. Every settle is under the claim EXCEPT recording a delivery
// (RecordNotificationsSent): a push that went out is true whoever holds the row.
func (s *Store) settleNotifications(token string, ids []string, set string, setArgs ...any) (int64, error) {
	if token == "" {
		return 0, errEmptyClaimToken
	}
	var total int64
	for start := 0; start < len(ids); start += notificationIDChunk {
		end := start + notificationIDChunk
		if end > len(ids) {
			end = len(ids)
		}
		ph, idArgs := idPlaceholders(ids[start:end])
		args := append(append([]any{}, setArgs...), token)
		args = append(args, idArgs...)
		res, err := s.db.Exec(s.q(`UPDATE user_notifications SET `+set+`
			WHERE claim_token = ? AND push_state = 'pending' AND id IN (`+ph+`)`), args...)
		if err != nil {
			return total, fmt.Errorf("settle notifications: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("settle notifications: %w", err)
		}
		total += n
	}
	return total, nil
}

// MarkNotificationsSent records ids as pushed. token is Claim.Token. A row not
// (or no longer) pending under it is left alone. The dispatcher does not use
// it: SENT is the one deliberate exception to settle-under-the-claim, and is
// recorded by id with RecordNotificationsSent (push_dispatch_settle.go).
func (s *Store) MarkNotificationsSent(token string, ids []string) (int64, error) {
	return s.settleNotifications(token, ids, `push_state = 'sent', sent_at = ?`, now())
}

// MarkNotificationsSkipped records ids as deliberately not pushed, with the
// reason. Same guard as MarkNotificationsSent.
func (s *Store) MarkNotificationsSkipped(token string, ids []string, reason string) (int64, error) {
	return s.settleNotifications(token, ids, `push_state = 'skipped', skip_reason = ?`, reason)
}

// ReleaseNotifications hands claimed rows back after a retryable failure:
// the claim is cleared and attempts goes up by one. A row reaching
// MaxNotificationAttempts is skipped as gateway_failed in the SAME statement,
// so no window leaves it pending past its last attempt. The row is claimable
// again at once; the dispatcher uses ReleaseNotificationsBackoff instead.
func (s *Store) ReleaseNotifications(token string, ids []string) (int64, error) {
	return s.settleNotifications(token, ids, releaseSet(`NULL`))
}

// ReleaseNotificationsBackoff is ReleaseNotifications that stamps claimed_at
// with the release time instead of clearing it, so the row stays unavailable
// to ClaimPendingNotifications for one lease: a retry's delay equals the
// lease, and MaxNotificationAttempts span minutes and not seconds (TASK-3939
// part B2a). claim_token is still cleared.
func (s *Store) ReleaseNotificationsBackoff(token string, ids []string) (int64, error) {
	return s.settleNotifications(token, ids, releaseSet(`?`), now())
}

func releaseSet(claimedAt string) string {
	return fmt.Sprintf(`
		push_state = CASE WHEN attempts + 1 >= %d THEN 'skipped' ELSE 'pending' END,
		skip_reason = CASE WHEN attempts + 1 >= %d THEN '%s' ELSE skip_reason END,
		attempts = attempts + 1,
		claim_token = '',
		claimed_at = %s`, MaxNotificationAttempts, MaxNotificationAttempts, SkipReasonGatewayFailed, claimedAt)
}

// CountNotificationsSentSince is how many of userID's rows were pushed at or
// after since: the rate cap's input. sent_at is stored at whole seconds, so a
// row in since's own second cannot be placed before or after since; truncating
// the cutoff counts that whole second, which errs toward fewer pushes, never
// past the cap (TASK-3939 B2a codex r2).
func (s *Store) CountNotificationsSentSince(userID string, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRow(s.q(`
		SELECT COUNT(*) FROM user_notifications
		WHERE user_id = ? AND push_state = 'sent' AND sent_at >= ?`), userID, cutoffTS(since)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count sent notifications: %w", err)
	}
	return n, nil
}

// PruneNotifications deletes rows created before olderThan whose push_state is
// sent, skipped or expired. It NEVER deletes a pending row, of any age: a
// pending row is either still owed a push or about to be expired by
// ExpireStaleNotifications, and the retention sweep must not be what loses it.
func (s *Store) PruneNotifications(olderThan time.Time) (int64, error) {
	res, err := s.db.Exec(s.q(`
		DELETE FROM user_notifications
		WHERE created_at < ? AND push_state <> 'pending'`), cutoffTS(olderThan))
	if err != nil {
		return 0, fmt.Errorf("prune notifications: %w", err)
	}
	return res.RowsAffected()
}

// LivePushDevices returns userID's devices a push may go to at now, earliest
// created first (ties by id), so element 0 is the earliest-created live device.
//
// A device is live when the session that registered it still exists, belongs
// to the same user, and has not expired. And when another user holds the same
// token (push_devices is unique on (user_id, token), not on token), only the
// newest LIVE registrant owns it: A's notification never goes to a phone B
// has since signed into. "Newest" is the latest registration by reg_seq
// (TASK-4031), a unique sequence every register advances (INSERT and the
// same-user in-place UPDATE) and a session rebind does not. It is not
// updated_at, which has second resolution and tied a same-second reclaim to the
// random id, nor created_at, which a same-user re-register keeps (TASK-4027).
func (s *Store) LivePushDevices(userID string, at time.Time) ([]PushDevice, error) {
	ts := cutoffTS(at)
	rows, err := s.db.Query(s.q(`
		SELECT d.id, d.user_id, d.session_id, d.connection_id, d.platform, d.token, d.app_version, d.created_at, d.updated_at, d.reg_seq
		FROM push_devices d
		JOIN sessions s ON s.id = d.session_id AND s.user_id = d.user_id AND s.expires_at >= ?
		WHERE d.user_id = ?
		  AND NOT EXISTS (
		    SELECT 1 FROM push_devices d2
		    JOIN sessions s2 ON s2.id = d2.session_id AND s2.user_id = d2.user_id AND s2.expires_at >= ?
		    WHERE d2.token = d.token AND d2.user_id <> d.user_id
		      AND d2.reg_seq > d.reg_seq)
		ORDER BY d.created_at, d.id`), ts, userID, ts)
	if err != nil {
		return nil, fmt.Errorf("live push devices: %w", err)
	}
	defer rows.Close()
	var out []PushDevice
	for rows.Next() {
		var d PushDevice
		if err := rows.Scan(&d.ID, &d.UserID, &d.SessionID, &d.ConnectionID, &d.Platform, &d.Token, &d.AppVersion, &d.CreatedAt, &d.UpdatedAt, &d.RegSeq); err != nil {
			return nil, fmt.Errorf("live push devices: scan: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DeletePushDeviceByID removes one device row. Absent is not an error.
func (s *Store) DeletePushDeviceByID(id string) error {
	if _, err := s.db.Exec(s.q(`DELETE FROM push_devices WHERE id = ?`), id); err != nil {
		return fmt.Errorf("delete push device: %w", err)
	}
	return nil
}

// DeletePushDeviceIfCurrent removes the device row only while it is still the
// registration that was SENT to: same id, same token AND same reg_seq. The
// dispatcher's invalid_token handler uses it: the verdict is about one
// registration, and the row may have been registered since. A re-register by
// the same user with the same token updates the row in place, keeping id and
// token, so those two cannot tell it apart; only reg_seq, which every register
// advances (TASK-4031), can. The token check stays for a row whose token
// changed. Absent or advanced is not an error.
func (s *Store) DeletePushDeviceIfCurrent(id, token string, regSeq int64) error {
	if _, err := s.db.Exec(s.q(`DELETE FROM push_devices WHERE id = ? AND token = ? AND reg_seq = ?`), id, token, regSeq); err != nil {
		return fmt.Errorf("delete push device: %w", err)
	}
	return nil
}

// GetNotificationForUser reads one of userID's notifications. Another user's
// id and a missing id are the same ErrNotificationNotFound.
func (s *Store) GetNotificationForUser(userID, id string) (*Notification, error) {
	n, err := s.scanNotification(s.db.QueryRow(s.q(`SELECT `+notificationColumns+`
		FROM user_notifications WHERE id = ? AND user_id = ?`), id, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotificationNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get notification: %w", err)
	}
	return &n, nil
}

// NotificationSubject returns the live item a notification is about. An item
// that is missing or soft-deleted, or whose collection is archived, is
// ErrNotificationSubjectGone: user_notifications.item_id carries no foreign
// key, so the row can outlive its subject.
func (s *Store) NotificationSubject(itemID string) (*models.Item, error) {
	var one int
	err := s.db.QueryRow(s.q(`
		SELECT 1 FROM items i JOIN collections c ON c.id = i.collection_id
		WHERE i.id = ? AND i.deleted_at IS NULL AND c.deleted_at IS NULL`), itemID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotificationSubjectGone
	}
	if err != nil {
		return nil, fmt.Errorf("notification subject: %w", err)
	}
	// includeDeleted: the join above is the liveness gate, and is the only one.
	item, err := s.getItemScanQ(s.db, itemID, true)
	if err != nil {
		return nil, fmt.Errorf("notification subject: %w", err)
	}
	if item == nil {
		return nil, ErrNotificationSubjectGone
	}
	return item, nil
}
