package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Pending app installs (SPEC-6 U8a, DOC-3371 §2 "Staging caps"; TASK-3397).
//
// A reservation is taken BEFORE any network fetch, in one transaction that
// locks the owner's users row (FOR NO KEY UPDATE, the enforceUserLimitTx
// shape) and then the instance-wide pending-bytes advisory lock, and counts:
// at most PendingPerOwner live reservations per owner, and at most
// PendingInstanceBytes of live reserved bytes on the instance. It charges the
// FULL PendingInstallBytes. The charge is kept, whatever has been staged so
// far, until no further bytes can arrive (FinishPendingInstall); only then is
// it reduced to the bytes actually staged, under the instance lock. Releasing
// capacity mid-fetch would let the remaining artifacts of concurrent installs
// together exceed the instance cap.
//
// "Live" is expires_at in the future. A fetching reservation expires shortly
// after the fetch deadline, a staged one after an hour; the sweep deletes
// expired ones, and the blobs go with them (ON DELETE CASCADE). On SQLite
// BEGIN IMMEDIATE serializes all of this, and the lock clauses are skipped.
//
// LOCK ORDER: owner row, then the instance lock. The other transactions here
// take the instance lock alone, so no cycle forms.

// Staging caps (DOC-3371 §2).
const (
	PendingInstallBytes  int64 = 4 << 20
	PendingInstanceBytes int64 = 64 << 20
	PendingPerOwner            = 3
	PendingFetchTTL            = 2 * time.Minute
	PendingStagedTTL           = time.Hour
)

// PendingManifestKey is the blob key the manifest is staged under.
const PendingManifestKey = "$manifest"

var (
	// ErrPendingLimit: the owner already has PendingPerOwner live pending
	// installs, or the instance's pending bytes are at their cap. The
	// refusal carries no counts.
	ErrPendingLimit = errors.New("app install: too many pending installs")
	// ErrPendingOverCap: staging a blob would exceed the install's byte cap.
	ErrPendingOverCap = errors.New("app install: the pack exceeds its size cap")
	// ErrPendingNotFound: no live pending install with that id for that
	// workspace and owner.
	ErrPendingNotFound = errors.New("app install: pending install not found")
)

// PendingInstall is a pending-install record.
type PendingInstall struct {
	ID             string
	WorkspaceID    string
	OwnerID        string
	Origin         string
	State          string
	ReservedBytes  int64
	ManifestSHA256 string
	Preview        string
	ExpiresAt      time.Time
	// UpgradeOf is the install an upgrade would change; "" for a fresh
	// install (U8b2).
	UpgradeOf string
}

// PendingBlob is one staged blob: the manifest or an artifact.
type PendingBlob struct {
	Key    string
	URL    string
	SHA256 string
	Data   []byte
}

func (s *Store) lockPendingBytes(tx *sql.Tx) error {
	if s.dialect.Driver() != DriverPostgres {
		return nil
	}
	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock(hashtext('pad:app-pending-bytes'))`); err != nil {
		return fmt.Errorf("pending install: instance lock: %w", err)
	}
	return nil
}

func timeText(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// ReservePendingInstall takes a reservation for an install the owner is about
// to fetch, or refuses with ErrPendingLimit. No fetch may begin without one.
func (s *Store) ReservePendingInstall(workspaceID, ownerID, origin string) (*PendingInstall, error) {
	return s.reservePending(workspaceID, ownerID, origin, "")
}

// ReservePendingUpgrade reserves a pending record for an upgrade of
// installID, under the same caps as an install (U8b2).
func (s *Store) ReservePendingUpgrade(workspaceID, ownerID, origin, installID string) (*PendingInstall, error) {
	return s.reservePending(workspaceID, ownerID, origin, installID)
}

func (s *Store) reservePending(workspaceID, ownerID, origin, upgradeOf string) (*PendingInstall, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("pending install: begin: %w", err)
	}
	defer tx.Rollback()

	ownerQ := `SELECT id FROM users WHERE id = ?`
	if s.dialect.Driver() == DriverPostgres {
		ownerQ += ` FOR NO KEY UPDATE`
	}
	var got string
	if err := tx.QueryRow(s.q(ownerQ), ownerID).Scan(&got); err != nil {
		return nil, fmt.Errorf("pending install: lock owner: %w", err)
	}
	if err := s.lockPendingBytes(tx); err != nil {
		return nil, err
	}
	nowT := time.Now()
	nowS := timeText(nowT)
	var mine int
	if err := tx.QueryRow(s.q(`SELECT COUNT(*) FROM app_install_pending WHERE owner_id = ? AND expires_at > ?`), ownerID, nowS).Scan(&mine); err != nil {
		return nil, fmt.Errorf("pending install: count owner: %w", err)
	}
	var instance int64
	if err := tx.QueryRow(s.q(`SELECT COALESCE(SUM(reserved_bytes), 0) FROM app_install_pending WHERE expires_at > ?`), nowS).Scan(&instance); err != nil {
		return nil, fmt.Errorf("pending install: sum instance: %w", err)
	}
	if mine >= PendingPerOwner || instance+PendingInstallBytes > PendingInstanceBytes {
		return nil, ErrPendingLimit
	}
	p := &PendingInstall{
		ID: newID(), WorkspaceID: workspaceID, OwnerID: ownerID, Origin: origin, State: "fetching",
		ReservedBytes: PendingInstallBytes, ExpiresAt: nowT.Add(PendingFetchTTL).UTC().Truncate(time.Second),
		UpgradeOf: upgradeOf,
	}
	var upg any
	if upgradeOf != "" {
		upg = upgradeOf
	}
	if _, err := tx.Exec(s.q(`INSERT INTO app_install_pending (id, workspace_id, owner_id, origin, state, reserved_bytes, expires_at, created_at, updated_at, upgrade_of)
		VALUES (?, ?, ?, ?, 'fetching', ?, ?, ?, ?, ?)`),
		p.ID, workspaceID, ownerID, origin, PendingInstallBytes, timeText(p.ExpiresAt), nowS, nowS, upg); err != nil {
		return nil, fmt.Errorf("pending install: insert: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("pending install: commit: %w", err)
	}
	return p, nil
}

// StagePendingBlob writes one fetched blob into a fetching reservation. The
// staged total may not exceed the reservation's charge (the per-install
// cap), which is never reduced while fetching.
func (s *Store) StagePendingBlob(pendingID string, b PendingBlob) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("pending blob: begin: %w", err)
	}
	defer tx.Rollback()
	q := `SELECT reserved_bytes FROM app_install_pending WHERE id = ? AND state = 'fetching' AND expires_at > ?`
	if s.dialect.Driver() == DriverPostgres {
		q += ` FOR UPDATE`
	}
	var reserved int64
	switch err := tx.QueryRow(s.q(q), pendingID, timeText(time.Now())).Scan(&reserved); {
	case errors.Is(err, sql.ErrNoRows):
		return ErrPendingNotFound
	case err != nil:
		return fmt.Errorf("pending blob: lock: %w", err)
	}
	var staged int64
	if err := tx.QueryRow(s.q(`SELECT COALESCE(SUM(size_bytes), 0) FROM app_install_pending_blobs WHERE pending_id = ?`), pendingID).Scan(&staged); err != nil {
		return fmt.Errorf("pending blob: sum: %w", err)
	}
	if staged+int64(len(b.Data)) > reserved {
		return ErrPendingOverCap
	}
	if _, err := tx.Exec(s.q(`INSERT INTO app_install_pending_blobs (pending_id, key, url, sha256, size_bytes, data) VALUES (?, ?, ?, ?, ?, ?)`),
		pendingID, b.Key, b.URL, b.SHA256, len(b.Data), b.Data); err != nil {
		return fmt.Errorf("pending blob: insert: %w", err)
	}
	return tx.Commit()
}

// FinishPendingInstall ends the fetch: under the instance lock, the charge is
// reduced to the bytes actually staged, the record is marked staged with its
// manifest hash and preview, and it is kept for PendingStagedTTL.
func (s *Store) FinishPendingInstall(pendingID, manifestSHA256, preview string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("pending install: begin: %w", err)
	}
	defer tx.Rollback()
	if err := s.lockPendingBytes(tx); err != nil {
		return err
	}
	var staged int64
	if err := tx.QueryRow(s.q(`SELECT COALESCE(SUM(size_bytes), 0) FROM app_install_pending_blobs WHERE pending_id = ?`), pendingID).Scan(&staged); err != nil {
		return fmt.Errorf("pending install: sum: %w", err)
	}
	nowT := time.Now()
	res, err := tx.Exec(s.q(`UPDATE app_install_pending SET state = 'staged', reserved_bytes = ?, manifest_sha256 = ?, preview = ?, expires_at = ?, updated_at = ?
		WHERE id = ? AND state = 'fetching' AND expires_at > ?`),
		staged, manifestSHA256, preview, timeText(nowT.Add(PendingStagedTTL)), timeText(nowT), pendingID, timeText(nowT))
	if err != nil {
		return fmt.Errorf("pending install: finish: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrPendingNotFound
	}
	return tx.Commit()
}

// DeletePendingInstall removes a reservation and every staged blob: a failed
// or cancelled fetch, or an owner discarding a preview. A missing record is
// not an error.
func (s *Store) DeletePendingInstall(pendingID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("pending install: begin: %w", err)
	}
	defer tx.Rollback()
	if err := s.lockPendingBytes(tx); err != nil {
		return err
	}
	if _, err := tx.Exec(s.q(`DELETE FROM app_install_pending WHERE id = ?`), pendingID); err != nil {
		return fmt.Errorf("pending install: delete: %w", err)
	}
	return tx.Commit()
}

// SweepExpiredPendingInstalls deletes every expired reservation and its
// blobs, returning how many records it removed.
func (s *Store) SweepExpiredPendingInstalls() (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("pending sweep: begin: %w", err)
	}
	defer tx.Rollback()
	if err := s.lockPendingBytes(tx); err != nil {
		return 0, err
	}
	res, err := tx.Exec(s.q(`DELETE FROM app_install_pending WHERE expires_at <= ?`), timeText(time.Now()))
	if err != nil {
		return 0, fmt.Errorf("pending sweep: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, tx.Commit()
}

// GetPendingInstall returns a LIVE pending install of this owner in this
// workspace, or ErrPendingNotFound (another owner's, another workspace's, an
// expired and a missing one all answer the same).
func (s *Store) GetPendingInstall(pendingID, workspaceID, ownerID string) (*PendingInstall, error) {
	var p PendingInstall
	var manifest, preview, upgradeOf sql.NullString
	var expires string
	err := s.db.QueryRow(s.q(`SELECT id, workspace_id, owner_id, origin, state, reserved_bytes, manifest_sha256, preview, expires_at, upgrade_of
		FROM app_install_pending WHERE id = ? AND workspace_id = ? AND owner_id = ? AND expires_at > ?`),
		pendingID, workspaceID, ownerID, timeText(time.Now())).
		Scan(&p.ID, &p.WorkspaceID, &p.OwnerID, &p.Origin, &p.State, &p.ReservedBytes, &manifest, &preview, &expires, &upgradeOf)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPendingNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get pending install: %w", err)
	}
	p.ManifestSHA256, p.Preview, p.ExpiresAt, p.UpgradeOf = manifest.String, preview.String, parseTime(expires), upgradeOf.String
	return &p, nil
}

// PendingInstallBlobs returns a pending install's staged blobs, by key.
func (s *Store) PendingInstallBlobs(pendingID string) (map[string]PendingBlob, error) {
	rows, err := s.db.Query(s.q(`SELECT key, url, sha256, data FROM app_install_pending_blobs WHERE pending_id = ?`), pendingID)
	if err != nil {
		return nil, fmt.Errorf("pending blobs: %w", err)
	}
	defer rows.Close()
	out := map[string]PendingBlob{}
	for rows.Next() {
		var b PendingBlob
		if err := rows.Scan(&b.Key, &b.URL, &b.SHA256, &b.Data); err != nil {
			return nil, err
		}
		out[b.Key] = b
	}
	return out, rows.Err()
}

// SlugHolder describes the collection already holding a slug: the origin
// and state of the app install that created it, both "" when no install did.
type SlugHolder struct {
	Origin       string
	InstallState string
	// InstallID is the creating install ("" when none): an upgrade treats its
	// own companions as existing, not as conflicts (U8b2).
	InstallID string
}

// CollectionSlugOwners reports, for each of slugs that already names a
// collection in the workspace (live or soft-deleted, since the unique index
// covers both), the app install that created it. Slugs absent from the result
// are free. It backs the install conflict check (DOC-3371 §2 step 5, L6).
func (s *Store) CollectionSlugOwners(workspaceID string, slugs []string) (map[string]SlugHolder, error) {
	return s.CollectionSlugOwnersQ(s.db, workspaceID, slugs)
}

// CollectionSlugOwnersQ is CollectionSlugOwners on a caller-supplied executor.
func (s *Store) CollectionSlugOwnersQ(q Queryer, workspaceID string, slugs []string) (map[string]SlugHolder, error) {
	out := map[string]SlugHolder{}
	if len(slugs) == 0 {
		return out, nil
	}
	in := strings.TrimSuffix(strings.Repeat("?,", len(slugs)), ",")
	args := []any{workspaceID}
	for _, sl := range slugs {
		args = append(args, sl)
	}
	rows, err := q.Query(s.q(`SELECT c.slug, COALESCE(a.origin, ''), COALESCE(a.state, ''), COALESCE(a.id, '') FROM collections c
		LEFT JOIN app_installs a ON a.id = c.via_app
		WHERE c.workspace_id = ? AND c.slug IN (`+in+`)`), args...)
	if err != nil {
		return nil, fmt.Errorf("collection slug owners: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var slug string
		var h SlugHolder
		if err := rows.Scan(&slug, &h.Origin, &h.InstallState, &h.InstallID); err != nil {
			return nil, err
		}
		out[slug] = h
	}
	return out, rows.Err()
}

// InvocationSlugIndexTaken reports whether a live item in collectionID would
// collide with slug in idx_items_invocation_slug_per_collection, using the
// index's OWN expression on each dialect (SQLite json_extract, Postgres
// ->>), so it agrees with the index on every stored shape, arrays and
// objects included (TASK-3397, codex round 8). It backs the app installer's
// preview of a defaulted slug.
func (s *Store) InvocationSlugIndexTaken(collectionID, slug string) (bool, error) {
	return s.InvocationSlugIndexTakenQ(s.db, collectionID, slug)
}

// InvocationSlugIndexTakenQ is InvocationSlugIndexTaken on a caller-supplied
// executor.
func (s *Store) InvocationSlugIndexTakenQ(q Queryer, collectionID, slug string) (bool, error) {
	expr := `json_extract(fields, '$.invocation_slug')`
	if s.dialect.Driver() == DriverPostgres {
		expr = `(fields->>'invocation_slug')`
	}
	var n int
	err := q.QueryRow(s.q(`SELECT COUNT(*) FROM items WHERE collection_id = ? AND deleted_at IS NULL
		AND `+expr+` IS NOT NULL AND `+expr+` != '' AND `+expr+` = ?`), collectionID, slug).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("invocation slug index lookup: %w", err)
	}
	return n > 0, nil
}

// ItemsWithFieldValueQ returns up to limit live items of collectionID in
// workspaceID whose field key equals value, with ListItems' own Fields-filter
// predicate (JSONFieldEquals, deleted_at IS NULL), on a caller-supplied
// executor. It is the in-transaction form of the two ListItems lookups the
// importer's normalization makes (the invocation_slug probe and the unique
// check), so a normalization on the provisioning transaction applies the
// same predicate the import does (TASK-3397, U8b).
func (s *Store) ItemsWithFieldValueQ(q Queryer, workspaceID, collectionID, key, value string, limit int) ([]FieldValueHolder, error) {
	// ListItems SKIPS a filter whose key fails isValidFieldKey, matching every
	// item of the collection. Mirrored, not tightened: this must answer what
	// the import's own lookup answers.
	cond, args := "1=1", []any(nil)
	if isValidFieldKey(key) {
		cond, args = s.dialect.JSONFieldEquals("i.fields", key, value)
	}
	args = append([]any{workspaceID, collectionID}, args...)
	args = append(args, limit)
	rows, err := q.Query(s.q(`SELECT i.id, i.slug FROM items i
		WHERE i.workspace_id = ? AND i.collection_id = ? AND i.deleted_at IS NULL AND `+cond+`
		ORDER BY i.id LIMIT ?`), args...)
	if err != nil {
		return nil, fmt.Errorf("items with field value: %w", err)
	}
	defer rows.Close()
	var out []FieldValueHolder
	for rows.Next() {
		var h FieldValueHolder
		if err := rows.Scan(&h.ID, &h.Slug); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// FieldValueHolder is an item ItemsWithFieldValueQ found.
type FieldValueHolder struct{ ID, Slug string }
