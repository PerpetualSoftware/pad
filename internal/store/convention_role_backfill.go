package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/artifact"
	"github.com/PerpetualSoftware/pad/internal/collections"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// PLAN-3598, lead ruling: a convention's role is its agent-role COLUMN, the
// validated reference bootstrap labels convention_index with, not the
// free-text `role` field the skill docs used to filter on. Conventions that
// carry only the field would drop out of every role-scoped read, so their
// field value is copied into the column, once, where it names a live role.
//
// Additive only, each pinned by a test in convention_role_backfill_test.go:
//   - a field value that is exactly a live role's slug in the item's
//     workspace is copied into an EMPTY column;
//   - a set column is never overwritten, whatever the field says;
//   - the field is never cleared or rewritten;
//   - a value that names no live role is reported, not guessed at.
//
// A copy is a visible role change, so unlike BUG-2788's silent id backfill it
// moves the item's seq: delta sync (/items/changes?since=<seq>) keys on seq,
// and a silent write would leave every local-first client showing no role
// until a full re-fetch. Each copy is one conditional UPDATE, guarded on the
// column still being empty and on the seq as read, so a concurrent writer is
// never overwritten (that row is picked up by the next boot). Its activity
// row is attributed to the system, carries the seq stamp, and emits no
// webhook or outbox event.
//
// It runs on EVERY boot and selects by content, like the bidi filename
// backfill: a copied row no longer matches (its column is set), and an
// invalid value never self-heals, so it is reported on every boot. That is
// why the log is a per-boot summary with details for the first
// conventionRoleInvalidLogLimit invalid rows, while each copy is logged in
// full by the run that makes it.

// conventionRoleInvalidLogLimit caps the per-boot detail lines for values
// that name no live role (lead ruling).
const conventionRoleInvalidLogLimit = 20

// ConventionRoleInvalid is one convention whose `role` field names no live
// role in its workspace.
type ConventionRoleInvalid struct {
	WorkspaceID string
	ItemID      string
	Value       string
}

// BackfillConventionRolesResult reports what the backfill did, for the
// startup log line.
type BackfillConventionRolesResult struct {
	Copied int
	// Invalid counts every invalid row this boot; InvalidSample holds the
	// first conventionRoleInvalidLogLimit of them.
	Invalid       int
	InvalidSample []ConventionRoleInvalid
	// Raced counts rows whose column or seq changed between the read and the
	// write; the next boot reconsiders them.
	Raced int
}

// BackfillConventionRoles copies a convention's `role` field into its empty
// agent-role column where the field names a live role. See the comment
// above for the rules.
func (s *Store) BackfillConventionRoles() (*BackfillConventionRolesResult, error) {
	res := &BackfillConventionRolesResult{}

	wsRows, err := s.db.Query(s.q(`SELECT id FROM workspaces WHERE deleted_at IS NULL ORDER BY id`))
	if err != nil {
		return res, fmt.Errorf("convention role backfill: list workspaces: %w", err)
	}
	var workspaceIDs []string
	for wsRows.Next() {
		var id string
		if err := wsRows.Scan(&id); err != nil {
			_ = wsRows.Close()
			return res, fmt.Errorf("convention role backfill: scan workspace: %w", err)
		}
		workspaceIDs = append(workspaceIDs, id)
	}
	if err := wsRows.Close(); err != nil {
		return res, fmt.Errorf("convention role backfill: list workspaces: %w", err)
	}

	for _, wsID := range workspaceIDs {
		if err := s.backfillConventionRolesInWorkspace(wsID, res); err != nil {
			return res, err
		}
	}
	return res, nil
}

type conventionRoleCandidate struct {
	id, value string
	seq       int64
}

func (s *Store) backfillConventionRolesInWorkspace(wsID string, res *BackfillConventionRolesResult) error {
	traited, err := s.ListTraitedCollections(wsID)
	if err != nil {
		return fmt.Errorf("convention role backfill: traits for %s: %w", wsID, err)
	}
	coll := collections.FindByArtifactKind(traited, string(artifact.KindConvention))
	if coll == nil {
		return nil
	}

	fieldsExpr := "fields"
	if s.dialect.Driver() == DriverPostgres {
		fieldsExpr = "fields::text"
	}
	rows, err := s.db.Query(s.q(`SELECT id, seq, `+fieldsExpr+` FROM items
		WHERE workspace_id = ? AND collection_id = ? AND agent_role_id IS NULL AND deleted_at IS NULL
		ORDER BY id`), wsID, coll.ID)
	if err != nil {
		return fmt.Errorf("convention role backfill: select in %s: %w", wsID, err)
	}
	var cands []conventionRoleCandidate
	for rows.Next() {
		var c conventionRoleCandidate
		var fields sql.NullString
		if err := rows.Scan(&c.id, &c.seq, &fields); err != nil {
			_ = rows.Close()
			return fmt.Errorf("convention role backfill: scan in %s: %w", wsID, err)
		}
		c.value = conventionRoleFieldValue(fields.String)
		if c.value != "" {
			cands = append(cands, c)
		}
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("convention role backfill: select in %s: %w", wsID, err)
	}

	for _, c := range cands {
		roleID, roleName, err := s.liveRoleBySlug(wsID, c.value)
		if err != nil {
			return err
		}
		if roleID == "" {
			res.Invalid++
			if len(res.InvalidSample) < conventionRoleInvalidLogLimit {
				res.InvalidSample = append(res.InvalidSample, ConventionRoleInvalid{WorkspaceID: wsID, ItemID: c.id, Value: c.value})
			}
			continue
		}
		newSeq, err := s.copyConventionRole(wsID, c, roleID, roleName)
		if errors.Is(err, errConventionRoleRaced) {
			res.Raced++
			continue
		}
		if err != nil {
			return err
		}
		res.Copied++
		slog.Info("convention role backfill: copied role field into the role column",
			"workspace_id", wsID, "item_id", c.id, "role", c.value, "role_id", roleID,
			"prev_seq", c.seq, "seq", newSeq)
	}
	return nil
}

// conventionRoleFieldValue is the item's `role` field as a trimmed string,
// or "" when it is absent, empty, not a string, or the blob does not decode.
func conventionRoleFieldValue(fields string) string {
	if fields == "" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(fields), &m); err != nil {
		return ""
	}
	v, _ := m["role"].(string)
	return strings.TrimSpace(v)
}

// liveRoleBySlug resolves an exact role slug in the workspace. A role id, a
// display name or a differently cased slug is not a match: the field is free
// text, and only a value that is unambiguously a slug is copied.
func (s *Store) liveRoleBySlug(wsID, slug string) (id, name string, err error) {
	err = s.db.QueryRow(s.q(`SELECT id, name FROM agent_roles WHERE workspace_id = ? AND slug = ?`), wsID, slug).Scan(&id, &name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("convention role backfill: resolve role %q in %s: %w", slug, wsID, err)
	}
	return id, name, nil
}

var errConventionRoleRaced = errors.New("convention changed since it was read")

// copyConventionRole sets the column and moves the seq in one conditional
// UPDATE, then records the system activity row in the same transaction.
func (s *Store) copyConventionRole(wsID string, c conventionRoleCandidate, roleID, roleName string) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("convention role backfill: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := s.acquireWorkspaceSeqLock(tx, wsID); err != nil {
		return 0, err
	}
	r, err := tx.Exec(s.q(`UPDATE items SET agent_role_id = ?, seq = `+nextWorkspaceSeqSubquery+`, updated_at = ?
		WHERE id = ? AND workspace_id = ? AND agent_role_id IS NULL AND seq = ? AND deleted_at IS NULL`),
		roleID, wsID, now(), c.id, wsID, c.seq)
	if err != nil {
		return 0, fmt.Errorf("convention role backfill: update %s: %w", c.id, err)
	}
	n, err := r.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("convention role backfill: rows affected %s: %w", c.id, err)
	}
	if n != 1 {
		return 0, errConventionRoleRaced
	}
	var newSeq int64
	if err := tx.QueryRow(s.q(`SELECT seq FROM items WHERE id = ?`), c.id).Scan(&newSeq); err != nil {
		return 0, fmt.Errorf("convention role backfill: read seq %s: %w", c.id, err)
	}
	meta, err := json.Marshal(map[string]any{
		"changes":           "role: — → " + roleName,
		"backfill":          "convention_role_field",
		ActivityMetaPrevSeq: c.seq,
		ActivityMetaSeq:     newSeq,
	})
	if err != nil {
		return 0, err
	}
	if _, err := s.createActivityQ(tx, activityForConventionRoleBackfill(wsID, c.id, string(meta))); err != nil {
		return 0, fmt.Errorf("convention role backfill: activity %s: %w", c.id, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("convention role backfill: commit %s: %w", c.id, err)
	}
	return newSeq, nil
}

func activityForConventionRoleBackfill(wsID, itemID, meta string) models.Activity {
	return models.Activity{
		WorkspaceID: wsID,
		DocumentID:  itemID,
		Action:      "updated",
		Actor:       "system",
		Source:      "system",
		Metadata:    meta,
	}
}
