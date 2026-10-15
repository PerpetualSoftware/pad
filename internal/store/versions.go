package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/PerpetualSoftware/pad/internal/diff"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// VersionThrottleInterval is the minimum time between version snapshots
// during continuous editing. Edits within this window are consolidated.
const VersionThrottleInterval = 1 * time.Hour

// ShouldCreateVersion determines whether a new version snapshot should be created,
// based on time since last version, actor/source changes, and content type.
func (s *Store) ShouldCreateVersion(documentID, actor, source string) (bool, error) {
	return s.shouldCreateVersionQ(s.db, documentID, actor, source)
}

// shouldCreateVersionQ is ShouldCreateVersion against a caller-supplied
// executor. UpdateDocument calls it from INSIDE its transaction, and a read
// issued against the pool from there needs a second connection while the
// first is held — the application-level deadlock BUG-2778's other half is
// about. This is the more reachable instance of the two: it fires on any
// content edit, not only a rename.
func (s *Store) shouldCreateVersionQ(q rowQueryer, documentID, actor, source string) (bool, error) {
	latest, err := s.getLatestVersionRawQ(q, documentID)
	if err != nil {
		return false, err
	}

	// No versions yet — always create the first one
	if latest == nil {
		return true, nil
	}

	// Actor changed (user ↔ agent) — always snapshot
	if latest.CreatedBy != actor {
		return true, nil
	}

	// Source changed (web ↔ cli ↔ skill) — always snapshot
	if latest.Source != source {
		return true, nil
	}

	// Throttle: only create if enough time has passed
	elapsed := time.Since(latest.CreatedAt)
	return elapsed >= VersionThrottleInterval, nil
}

// GetLatestVersion returns the most recent version for a document with full content
// (diffs are resolved against current document content).
func (s *Store) GetLatestVersion(documentID string) (*models.Version, error) {
	return s.getLatestVersionRaw(documentID)
}

// getLatestVersionRaw returns the most recent version without resolving diffs.
func (s *Store) getLatestVersionRaw(documentID string) (*models.Version, error) {
	return s.getLatestVersionRawQ(s.db, documentID)
}

// getLatestVersionRawQ is getLatestVersionRaw against a caller-supplied
// executor, so an in-transaction caller reads through its own transaction.
func (s *Store) getLatestVersionRawQ(q rowQueryer, documentID string) (*models.Version, error) {
	var v models.Version
	var createdAt string
	var isDiff bool
	err := q.QueryRow(s.q(`
		SELECT id, document_id, content, change_summary, created_by, source, is_diff, created_at
		FROM versions
		WHERE document_id = ?
		ORDER BY created_at DESC
		LIMIT 1
	`), documentID).Scan(&v.ID, &v.DocumentID, &v.Content, &v.ChangeSummary, &v.CreatedBy, &v.Source, &isDiff, &createdAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	v.IsDiff = isDiff
	v.CreatedAt = parseTime(createdAt)
	return &v, nil
}

func (s *Store) ListVersions(documentID string) ([]models.Version, error) {
	return s.listVersionsQ(s.db, documentID)
}

// listVersionsQ is ListVersions against a caller-supplied executor, so the
// snapshot read (BUG-3833) lists the rows inside its own transaction.
func (s *Store) listVersionsQ(q rowsQueryer, documentID string) ([]models.Version, error) {
	if s.beforeVersionsRead != nil {
		s.beforeVersionsRead(documentID)
	}
	rows, err := q.Query(s.q(`
		SELECT id, document_id, content, change_summary, created_by, source, is_diff, created_at
		FROM versions
		WHERE document_id = ?
		ORDER BY created_at DESC
	`), documentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var versions []models.Version
	for rows.Next() {
		var v models.Version
		var createdAt string
		var isDiff bool
		if err := rows.Scan(&v.ID, &v.DocumentID, &v.Content, &v.ChangeSummary, &v.CreatedBy, &v.Source, &isDiff, &createdAt); err != nil {
			return nil, err
		}
		v.IsDiff = isDiff
		v.CreatedAt = parseTime(createdAt)
		versions = append(versions, v)
	}
	return versions, rows.Err()
}

// ListVersionsResolved returns versions with full content (diffs resolved).
// Requires the current document content to reconstruct diff-based versions.
func (s *Store) ListVersionsResolved(documentID, currentContent string) ([]models.Version, error) {
	versions, err := s.ListVersions(documentID)
	if err != nil {
		return nil, err
	}
	resolveVersionChain(currentContent, versions)
	return versions, nil
}

// ListVersionsResolvedSnapshot reads the document's body and its versions in
// one read snapshot, and resolves the versions from that body (BUG-3833, the
// legacy-documents twin of BUG-3814's ListItemVersionsResolvedSnapshot).
//
// A body read separately is wrong whenever a content write lands between the
// two reads: the write adds a reverse patch against the NEW body while the
// walk starts from the OLD one, and diff.ApplyPatch does not check a patch's
// base. A handler that read the document for its workspace check must
// therefore not pass that document's Content to ListVersionsResolved.
//
// The body read carries GetDocument's deleted_at filter, so a document
// deleted after the handler's check has no versions to show: the result is
// empty, and the caller answers as for a missing version.
func (s *Store) ListVersionsResolvedSnapshot(ctx context.Context, documentID string) ([]models.Version, error) {
	tx, err := s.beginReadSnapshot(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin version snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var current string
	err = tx.QueryRow(s.q(`SELECT content FROM documents WHERE id = ? AND deleted_at IS NULL`), documentID).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	versions, err := s.listVersionsQ(tx, documentID)
	if err != nil {
		return nil, err
	}
	resolveVersionChain(current, versions)
	return versions, nil
}

// GetVersionResolvedSnapshot is GetVersionResolved over one read snapshot
// (BUG-3833). Returns nil if not found.
func (s *Store) GetVersionResolvedSnapshot(ctx context.Context, id, documentID string) (*models.Version, error) {
	versions, err := s.ListVersionsResolvedSnapshot(ctx, documentID)
	if err != nil {
		return nil, err
	}
	for i := range versions {
		if versions[i].ID == id {
			return &versions[i], nil
		}
	}
	return nil, nil
}

// resolveVersionChain resolves a document's versions, newest first, in place
// from currentContent.
func resolveVersionChain(currentContent string, versions []models.Version) {
	// Resolve diffs: walk from newest to oldest, applying reverse patches.
	// The newest diff version patches current content → that version's content.
	// The next older diff patches that result → its content, etc.
	content := currentContent
	for i := range versions {
		if !versions[i].IsDiff {
			// Full content — use as-is, and this becomes the base for older diffs
			content = versions[i].Content
			continue
		}
		// Apply reverse patch: content at this point → version's content
		resolved, err := diff.ApplyPatch(content, versions[i].Content)
		if err != nil {
			// If patch fails, mark it but don't break the whole list
			versions[i].Content = fmt.Sprintf("[patch error: %v]", err)
			versions[i].IsDiff = false
			continue
		}
		versions[i].Content = resolved
		versions[i].IsDiff = false
		content = resolved
	}
}

func (s *Store) GetVersion(id string) (*models.Version, error) {
	var v models.Version
	var createdAt string
	var isDiff bool
	err := s.db.QueryRow(s.q(`
		SELECT id, document_id, content, change_summary, created_by, source, is_diff, created_at
		FROM versions
		WHERE id = ?
	`), id).Scan(&v.ID, &v.DocumentID, &v.Content, &v.ChangeSummary, &v.CreatedBy, &v.Source, &isDiff, &createdAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	v.IsDiff = isDiff
	v.CreatedAt = parseTime(createdAt)
	return &v, nil
}

// GetVersionResolved returns a version with full content reconstructed.
// For diff-based versions, this requires walking the version chain.
func (s *Store) GetVersionResolved(id, documentID, currentContent string) (*models.Version, error) {
	// Get all versions to find and resolve the target
	versions, err := s.ListVersionsResolved(documentID, currentContent)
	if err != nil {
		return nil, err
	}
	for _, v := range versions {
		if v.ID == id {
			return &v, nil
		}
	}
	return nil, nil
}
