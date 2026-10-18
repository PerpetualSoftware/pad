package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Verification evidence tied to a revision (TASK-3617). Migration 150 says
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
			// The target passed its check above in this transaction, so the
			// row that failed is the via item's.
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
