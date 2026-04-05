package store

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/xarmian/pad/internal/diff"
	"github.com/xarmian/pad/internal/models"
)

// VersionThrottleInterval is the minimum time between version snapshots
// during continuous editing. Edits within this window are consolidated.
const VersionThrottleInterval = 1 * time.Hour

// ShouldCreateVersion determines whether a new version snapshot should be created,
// based on time since last version, actor/source changes, and content type.
func (s *Store) ShouldCreateVersion(documentID, actor, source string) (bool, error) {
	latest, err := s.getLatestVersionRaw(documentID)
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
	var v models.Version
	var createdAt string
	var isDiff bool
	err := s.db.QueryRow(s.q(`
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
	rows, err := s.db.Query(s.q(`
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
	return versions, nil
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
