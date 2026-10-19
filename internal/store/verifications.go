package store

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Verification evidence tied to a revision (TASK-3617). Migration 151 says
// why this is a table and why freshness keys on the criterion's hash. The
// store persists and lists rows; internal/verification judges them.

// ErrVerificationItemGone is the write's answer when the item is not a live
// item of the workspace. Missing, archived and another workspace's are one
// answer, as for ErrReminderItemGone.
var ErrVerificationItemGone = errors.New("verification item is not live in this workspace")

// ErrVerificationItemChanged is the write's answer when the item's seq is no
// longer the one the caller parsed its criteria from. The criterion hashes in
// the batch were computed from that version's body, so storing them against a
// newer one would tie evidence to text it never saw. The caller re-reads and
// tries again.
var ErrVerificationItemChanged = errors.New("item changed since its criteria were read")

// ErrVerificationViaGone is the write's answer when the via item is not a
// live item of the same workspace.
var ErrVerificationViaGone = errors.New("via item is not live in this workspace")

// verificationColumns is the SELECT list scanVerification reads.
const verificationColumns = `v.id, v.item_id, v.criterion_id, v.criterion_hash, v.spec_seq, v.code_revision,
	v.verdict, v.evidence, v.evidence_url, COALESCE(v.via_item_id, ''), v.run_id,
	v.created_by, v.user_id, v.agent_name, v.created_at`

// InsertItemVerifications stores one batch of verdicts on one item, in one
// transaction, all stamped with the same created_at. Each row's ItemID,
// ViaItemID and RunID are the caller's; ID and CreatedAt are minted here.
//
// Every row is asserted by its own INSERT ... SELECT, the way CreateReminder
// asserts its item: the item must be live, in this workspace, and still at
// specSeq; the via item, when named, must be live in the same workspace. A
// batch where any row fails is rolled back whole.
func (s *Store) InsertItemVerifications(workspaceID, itemID string, specSeq int64, rows []models.ItemVerification) ([]models.ItemVerification, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	if err := s.checkVerificationTargetTx(tx, workspaceID, itemID, specSeq); err != nil {
		return nil, err
	}

	at := decisionTime(time.Now())
	out := make([]models.ItemVerification, 0, len(rows))
	for _, r := range rows {
		r.ID = newID()
		r.ItemID = itemID
		r.SpecSeq = specSeq
		r.CreatedAt = at
		var via any
		if r.ViaItemID != "" {
			via = r.ViaItemID
		}
		res, err := tx.Exec(s.q(`
			INSERT INTO item_verifications
				(id, workspace_id, item_id, criterion_id, criterion_hash, spec_seq, code_revision,
				 verdict, evidence, evidence_url, via_item_id, run_id, created_by, user_id, agent_name, created_at)
			SELECT ?, i.workspace_id, i.id, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
			FROM items i
			WHERE i.id = ? AND i.workspace_id = ? AND i.deleted_at IS NULL AND i.seq = ?
			  AND (? = '' OR EXISTS (
				SELECT 1 FROM items vi
				WHERE vi.id = ? AND vi.workspace_id = i.workspace_id AND vi.deleted_at IS NULL
			  ))`),
			r.ID, r.CriterionID, r.CriterionHash, specSeq, r.CodeRevision,
			r.Verdict, r.Evidence, r.EvidenceURL, via, r.RunID, r.CreatedBy, r.UserID, r.AgentName, at,
			itemID, workspaceID, specSeq,
			r.ViaItemID, r.ViaItemID,
		)
		if err != nil {
			return nil, fmt.Errorf("insert item verification %s: %w", r.CriterionID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("insert item verification %s: %w", r.CriterionID, err)
		}
		if n == 0 {
			// Either predicate can be the one that failed. The check above
			// does not pin the item: on Postgres (READ COMMITTED) a writer
			// can commit a new seq between it and this INSERT (TASK-3617 r1).
			// Re-check the target first, so a moved item retries and a gone
			// one is a 404; only when it still holds is the via item at fault.
			if err := s.checkVerificationTargetTx(tx, workspaceID, itemID, specSeq); err != nil {
				return nil, err
			}
			return nil, ErrVerificationViaGone
		}
		out = append(out, r)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// checkVerificationTargetTx tells a gone item from a changed one, so the
// write can answer 404 for the first and retry the second.
func (s *Store) checkVerificationTargetTx(tx *sql.Tx, workspaceID, itemID string, specSeq int64) error {
	var seq int64
	err := tx.QueryRow(s.q(`SELECT seq FROM items WHERE id = ? AND workspace_id = ? AND deleted_at IS NULL`),
		itemID, workspaceID).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrVerificationItemGone
	}
	if err != nil {
		return fmt.Errorf("verification target: %w", err)
	}
	if seq != specSeq {
		return ErrVerificationItemChanged
	}
	return nil
}

// ListItemVerifications returns an item's verdicts, oldest first (created_at,
// then id). With latestOnly, it returns only the newest row per criterion,
// which is what a reader judging "is this criterion verified" needs; the
// history is for the audit.
func (s *Store) ListItemVerifications(workspaceID, itemID string, latestOnly bool) ([]models.ItemVerification, error) {
	query := `SELECT ` + verificationColumns + ` FROM item_verifications v
		WHERE v.item_id = ? AND v.workspace_id = ?`
	if latestOnly {
		query += `
		  AND NOT EXISTS (
			SELECT 1 FROM item_verifications n
			WHERE n.item_id = v.item_id AND n.criterion_id = v.criterion_id
			  AND (n.created_at > v.created_at OR (n.created_at = v.created_at AND n.id > v.id))
		  )`
	}
	query += ` ORDER BY v.created_at, v.id`
	rows, err := s.db.Query(s.q(query), itemID, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list item verifications: %w", err)
	}
	defer rows.Close()
	var out []models.ItemVerification
	for rows.Next() {
		var v models.ItemVerification
		if err := rows.Scan(&v.ID, &v.ItemID, &v.CriterionID, &v.CriterionHash, &v.SpecSeq, &v.CodeRevision,
			&v.Verdict, &v.Evidence, &v.EvidenceURL, &v.ViaItemID, &v.RunID,
			&v.CreatedBy, &v.UserID, &v.AgentName, &v.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan item verification: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

var verificationCriterionRe = regexp.MustCompile(`^AC-[0-9]+$`)

// verificationImportable checks a bundle's verification row against every
// rule a live write enforces, and returns its created_at in the fixed-width
// form the latest-row ordering compares as text. A bundle is not necessarily
// one this server wrote, so the check runs on every row, and it runs BEFORE
// the insert because a refused statement on Postgres takes the whole import
// with it. NUL and invalid UTF-8 are refused here for the same reason:
// Postgres rejects them natively, and the SQLite trigger would abort too.
func verificationImportable(v models.VerificationExport) (string, bool) {
	if !verificationCriterionRe.MatchString(v.CriterionID) || !models.ValidVerdict(v.Verdict) || v.SpecSeq < 0 {
		return "", false
	}
	if len(v.CriterionHash) != 64 || !isLowerHex(v.CriterionHash) {
		return "", false
	}
	if v.CodeRevision != "" && !models.IsFullCommitSHA(v.CodeRevision) {
		return "", false
	}
	if _, err := uuid.Parse(v.RunID); err != nil {
		return "", false
	}
	if len(v.Evidence) > 2048 || len(v.EvidenceURL) > 2048 || len(v.CreatedBy) > 256 || len(v.AgentName) > 256 {
		return "", false
	}
	for _, s := range []string{v.Evidence, v.EvidenceURL, v.CreatedBy, v.AgentName} {
		if strings.ContainsRune(s, 0) || !utf8.ValidString(s) {
			return "", false
		}
	}
	if v.EvidenceURL != "" {
		u, err := url.Parse(v.EvidenceURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return "", false
		}
	}
	t, err := time.Parse(time.RFC3339Nano, v.CreatedAt)
	if err != nil {
		return "", false
	}
	return decisionTime(t), true
}

func isLowerHex(s string) bool {
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
