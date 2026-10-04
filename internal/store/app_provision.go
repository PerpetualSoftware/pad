package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// App install provisioning (SPEC-6 U8b, DOC-3371 §2 steps 6-8, §3; TASK-3397).
//
// The preview (U8a) is ADVISORY: it may refuse what provisioning would
// accept, never the reverse. ProvisionAppInstall is the guarantee. In ONE
// transaction it takes the locks every writer of the state the preview's
// normalization reads must wait on, re-derives that normalization on the
// transaction through the caller's ProvisionDeriveFunc (which compares it
// with the reviewed digests), then writes the install, its companion
// collections, its bot, its client, its artifacts and its install code, and
// deletes the pending record. Any refusal rolls the whole transaction back
// and leaves the pending record for a retry or a discard. The lock table is
// on ProvisionAppInstall.

// InstallCodeTTL is how long an install code can be redeemed.
const InstallCodeTTL = 10 * time.Minute

var (
	// ErrAppCollectionSlugTaken: a companion collection's declared slug is not
	// free when its collection is created.
	ErrAppCollectionSlugTaken = errors.New("app collection slug taken")
	// ErrPendingNotStaged: the pending record is gone, expired, not staged, or
	// not this owner's in this workspace.
	ErrPendingNotStaged = errors.New("pending install is not staged")
	// ErrPendingManifestChanged: the confirm names a manifest hash other than
	// the one staged, so the owner did not review this record.
	ErrPendingManifestChanged = errors.New("pending install manifest differs from the reviewed one")
	// ErrInstallCodeInvalid: the code is unknown, expired, consumed, or its
	// install is not active. One error for all four, deliberately.
	ErrInstallCodeInvalid = errors.New("invalid install code")
)

// ProvisionConflictError is a provisioning-time refusal: a fact the preview
// checked no longer holds. It names the artifact (or collection), the field,
// and what it collided with, so the owner can act on it (lead ruling, U8b).
type ProvisionConflictError struct {
	// Artifact is the manifest key of the artifact, or "" for a collection.
	Artifact string
	// Collection is the manifest key of the companion collection, when the
	// conflict is about one.
	Collection string
	Field      string
	// Detail says what collided or which check failed.
	Detail string
}

func (e *ProvisionConflictError) Error() string {
	subject := "artifact " + quote(e.Artifact)
	if e.Collection != "" {
		subject = "collection " + quote(e.Collection)
	}
	if e.Field != "" {
		return fmt.Sprintf("%s, field %q: %s", subject, e.Field, e.Detail)
	}
	return fmt.Sprintf("%s: %s", subject, e.Detail)
}

func quote(s string) string { return fmt.Sprintf("%q", s) }

// ProvisionCollection is one companion collection to create or adopt.
type ProvisionCollection struct {
	Key, Slug, Name, Schema string
	// Adopt: the preview found this slug held by a collection an install of
	// the same origin created. Provisioning re-checks that and creates
	// nothing for it.
	Adopt bool
}

// ProvisionArtifact is one artifact to insert, exactly as the preview
// normalized it.
type ProvisionArtifact struct {
	Key              string
	CollectionID     string
	Title            string
	Content          string
	Fields           map[string]any
	RawSHA256        string
	NormalizedSHA256 string
}

// ProvisionRequest is everything provisioning writes, computed by the caller
// from the pending record alone.
type ProvisionRequest struct {
	PendingID, WorkspaceID, OwnerID, Origin string
	// ManifestSHA256 is the hash the owner reviewed (the confirm body).
	ManifestSHA256  string
	ManifestVersion string
	ManifestJSON    string
	AppTitle        string
	ServiceAccess   string
	DelegatedAccess string
	RedirectURIs    []string
	SourcePack      string
	DigestsJSON     string
	// Collections and Artifacts are filled from the derivation, inside the
	// transaction; anything a caller sets here is replaced.
	Collections []ProvisionCollection
	Artifacts   []ProvisionArtifact
}

// ProvisionResult is what a successful provisioning wrote.
type ProvisionResult struct {
	InstallID   string
	InstallCode string
	ExpiresAt   time.Time
	BotUserID   string
	Items       []*models.Item
}

// ProvisionDerived is what the caller's derivation returns from inside the
// provisioning transaction: the collections and artifacts to write, exactly
// as the normalization produced them on the transaction.
type ProvisionDerived struct {
	Collections []ProvisionCollection
	Artifacts   []ProvisionArtifact
}

// ProvisionDeriveFunc re-runs the install's normalization on q (the
// provisioning transaction) and compares it with what the owner reviewed.
// A difference is the caller's typed error, returned unchanged. It must only
// READ: the write-capture test asserts a derivation writes nothing.
type ProvisionDeriveFunc func(q Queryer) (*ProvisionDerived, error)

// ErrNotWorkspaceOwner: the confirming caller is no longer an owner of the
// workspace when the provisioning transaction checks.
var ErrNotWorkspaceOwner = errors.New("not a workspace owner")

// ProvisionAppInstall provisions an app install in one transaction.
//
// THE RE-CHECK IS A RE-DERIVATION (lead ruling, day 86). The transaction does
// not re-verify a list of facts the preview relied on; two review rounds
// showed that list is never complete. It runs the same normalization on the
// transaction (derive) and the caller compares the result with the reviewed
// digests and changes list.
//
// LOCKS BREAK DEPENDENCY CYCLES; THEY DO NOT COVER EVERY READ (lead ruling,
// day 86, superseding "lock every read": that created deadlock cycles with
// member removal and account deletion and could never cover an inserted
// grant or a request-cached role; codex rounds 3-4). A writer W that changes
// state provisioning READ, but reads nothing provisioning WRITES and
// overwrites nothing it writes, serializes as "provision, then W": the
// outcome equals a history in which the owner confirmed a moment earlier,
// which is the guarantee every other write door gives. Only a writer that
// ALSO reads or overwrites what provisioning writes forms a cycle, and that
// writer must be held off.
//
// The locks, Postgres, in this fixed order:
//
//   - 0. The confirming owner's users row, FOR SHARE. Account deletion of that
//     owner is a REAL cycle (codex round 5): its bot scan reads the bot and
//     membership provisioning writes, and it deletes the owner provisioning
//     read. Its first statement locks this row FOR NO KEY UPDATE, so either it
//     waits for the commit and then sees the bot, or provisioning waits and
//     finds the pending row gone with the user. Taken first because that
//     deletion reaches the pending row through its FK cascade (users, then
//     pending: the order ReservePendingInstall takes too).
//   - 1. The app_install_pending row, FOR UPDATE: the record being consumed
//     (a concurrent confirm, discard or sweep).
//   - 2. The workspace seq lock (advisory): item writers (create, update,
//     delete, move, restore) and slug allocation read the item and slug space
//     provisioning writes into, so they are a cycle.
//   - 3. Every collections row of the workspace, FOR NO KEY UPDATE: one
//     consistent view of the collections for a multi-statement derivation
//     under READ COMMITTED (kind to destination, schemas, companion slugs,
//     relation target collections). NO KEY UPDATE rather than SHARE because
//     an adoption UPDATEs the adopted row's via_app later in this transaction,
//     and a SHARE lock upgraded under concurrency deadlocks; it still does not
//     conflict with the FK KEY SHARE an item insert takes.
//
// Raced on purpose, each serializing as "provision first":
//   - collection archive, trait and rename: they read no item provisioning
//     inserts (and an archive racing lock 3 waits for the commit anyway);
//   - membership, role, collection-access and grant writers, and an admin
//     role change: provisioning reads visibility and never writes it;
//   - account deletion of ANOTHER user (an issued grant's author, a relation
//     target's author): it updates their authored items and deletes their
//     grants and memberships, none of which provisioning writes.
//
// The owner-role read is a plain read for the same reason. SQLite needs no
// lock beyond its own: BEGIN IMMEDIATE holds the database write lock for the
// whole transaction, so nothing else commits while the derivation runs.
//
// Any refusal rolls the whole transaction back and leaves the pending record
// for a retry or a discard.
func (s *Store) ProvisionAppInstall(req ProvisionRequest, derive ProvisionDeriveFunc) (*ProvisionResult, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("provision app: begin: %w", err)
	}
	defer tx.Rollback()
	pg := s.dialect.Driver() == DriverPostgres

	// 0. The confirming owner's users row, first (see the lock list).
	if pg {
		var id string
		if err := tx.QueryRow(s.q(`SELECT id FROM users WHERE id = ? FOR SHARE`), req.OwnerID).Scan(&id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, ErrNotWorkspaceOwner
			}
			return nil, fmt.Errorf("provision app: lock owner: %w", err)
		}
	}

	// 1. The pending record: live, staged, this owner's, this workspace's,
	// and the very manifest the owner reviewed.
	// upgrade_of IS NULL: an upgrade's pending record is never provisioned as
	// a fresh install (U8b2); it confirms through UpgradeAppInstall only.
	lockQ := `SELECT workspace_id, owner_id, origin, state, COALESCE(manifest_sha256, ''), expires_at FROM app_install_pending WHERE id = ? AND upgrade_of IS NULL`
	if pg {
		lockQ += ` FOR UPDATE`
	}
	var ws, owner, origin, state, manifestSHA, expires string
	err = tx.QueryRow(s.q(lockQ), req.PendingID).Scan(&ws, &owner, &origin, &state, &manifestSHA, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPendingNotStaged
	}
	if err != nil {
		return nil, fmt.Errorf("provision app: lock pending: %w", err)
	}
	if ws != req.WorkspaceID || owner != req.OwnerID || state != "staged" || origin != req.Origin || expires <= timeText(time.Now()) {
		return nil, ErrPendingNotStaged
	}
	if manifestSHA != req.ManifestSHA256 {
		return nil, ErrPendingManifestChanged
	}

	// 2. The workspace lock, first of the workspace-scoped locks.
	if err := s.acquireWorkspaceSeqLock(tx, req.WorkspaceID); err != nil {
		return nil, err
	}

	// 3. Every collection row of the workspace (Postgres).
	if pg {
		if err := lockRowsForShare(tx, s.q(`SELECT id FROM collections WHERE workspace_id = ? ORDER BY id FOR NO KEY UPDATE`), req.WorkspaceID); err != nil {
			return nil, fmt.Errorf("provision app: lock collections: %w", err)
		}
	}

	if err := s.requireWorkspaceOwnerTx(tx, req.WorkspaceID, req.OwnerID); err != nil {
		return nil, err
	}
	derived, err := derive(tx)
	if err != nil {
		return nil, err
	}
	req.Collections, req.Artifacts = derived.Collections, derived.Artifacts

	// The install row.
	installID := newID()
	ts := now()
	if _, err := tx.Exec(s.q(`INSERT INTO app_installs (id, workspace_id, origin, state, auth_epoch,
		    manifest_sha256, manifest_version, manifest, service_access, delegated_access, digests, created_at, updated_at)
		VALUES (?, ?, ?, 'active', 1, ?, ?, ?, ?, ?, ?, ?, ?)`),
		installID, req.WorkspaceID, req.Origin, req.ManifestSHA256, req.ManifestVersion, req.ManifestJSON,
		req.ServiceAccess, req.DelegatedAccess, req.DigestsJSON, ts, ts); err != nil {
		return nil, fmt.Errorf("provision app: insert install: %w", err)
	}

	// Companion collections, created or adopted.
	var companionIDs []string
	for _, c := range req.Collections {
		if c.Adopt {
			// Adoption re-stamps the companion to this install; the derivation
			// already refused unless its holder is uninstalled (lead ruling,
			// day 86). Items' and comments' via_app stay as written: they are
			// attribution, not ownership.
			var id string
			if err := tx.QueryRow(s.q(`SELECT id FROM collections WHERE workspace_id = ? AND slug = ?`), req.WorkspaceID, c.Slug).Scan(&id); err != nil {
				return nil, fmt.Errorf("provision app: adopt %q: %w", c.Slug, err)
			}
			if _, err := tx.Exec(s.q(`UPDATE collections SET via_app = ? WHERE id = ?`), installID, id); err != nil {
				return nil, fmt.Errorf("provision app: adopt %q: %w", c.Slug, err)
			}
			companionIDs = append(companionIDs, id)
			continue
		}
		id, err := s.createCollectionTx(tx, req.WorkspaceID, models.CollectionCreate{Name: c.Name, Slug: c.Slug, Schema: c.Schema}, installID)
		if errors.Is(err, ErrAppCollectionSlugTaken) {
			// A backstop: the derivation already compared this collection's
			// adopt/create decision with the reviewed one.
			return nil, &ProvisionConflictError{Collection: c.Key, Field: "slug", Detail: fmt.Sprintf("a collection %q now exists in this workspace", c.Slug)}
		}
		if err != nil {
			return nil, fmt.Errorf("provision app: create collection %q: %w", c.Slug, err)
		}
		companionIDs = append(companionIDs, id)
	}

	// The bot: its principal, its membership by declared service access, and
	// visibility over exactly the companion collections (§3).
	bot, err := s.CreateAppUserTx(tx, installID, req.AppTitle)
	if err != nil {
		return nil, fmt.Errorf("provision app: %w", err)
	}
	if _, err := tx.Exec(s.q(`UPDATE app_installs SET bot_user_id = ? WHERE id = ?`), bot.ID, installID); err != nil {
		return nil, fmt.Errorf("provision app: bind bot: %w", err)
	}
	botRole := "viewer"
	if req.ServiceAccess == "write" {
		botRole = "editor"
	}
	if err := s.addAppPrincipalMemberTx(tx, req.WorkspaceID, bot.ID, botRole); err != nil {
		return nil, fmt.Errorf("provision app: bot membership: %w", err)
	}
	if err := s.setMemberCollectionAccessTx(tx, req.WorkspaceID, bot.ID, "specific", companionIDs); err != nil {
		return nil, fmt.Errorf("provision app: bot collection access: %w", err)
	}

	// The install client. Its first secret is discarded unseen: redeem
	// rotates and returns the one the app holds (lead ruling, U8b Q2).
	if _, secret, err := s.CreateInstallClientTx(tx, installID, req.RedirectURIs); err != nil {
		return nil, fmt.Errorf("provision app: %w", err)
	} else {
		discardSecret(&secret)
	}

	// The artifacts, in manifest order, as drafts through the owner create's
	// transactional path, stamped with their pack and both digests.
	result := &ProvisionResult{InstallID: installID, BotUserID: bot.ID}
	for _, a := range req.Artifacts {
		fieldsJSON, err := json.Marshal(a.Fields)
		if err != nil {
			return nil, fmt.Errorf("provision app: artifact %q fields: %w", a.Key, err)
		}
		item, err := s.createItemTxWithID(tx, newID(), req.WorkspaceID, a.CollectionID, models.ItemCreate{
			Title: a.Title, Content: a.Content, Fields: string(fieldsJSON),
			CreatedBy: "user", Source: "web", ActorUserID: req.OwnerID,
		}, mintOptions{})
		if err != nil {
			if isUniqueViolation(err) {
				return nil, &ProvisionConflictError{Artifact: a.Key, Detail: "it collides with an existing item (duplicate slug or invocation slug)"}
			}
			return nil, fmt.Errorf("provision app: artifact %q: %w", a.Key, err)
		}
		if _, err := tx.Exec(s.q(`UPDATE items SET source_pack = ?, source_artifact_sha256 = ?, installed_sha256 = ? WHERE id = ?`),
			req.SourcePack, a.RawSHA256, a.NormalizedSHA256, item.ID); err != nil {
			return nil, fmt.Errorf("provision app: stamp artifact %q: %w", a.Key, err)
		}
		result.Items = append(result.Items, item)
	}

	code, expiresAt, err := s.mintInstallCodeTx(tx, installID)
	if err != nil {
		return nil, err
	}
	result.InstallCode, result.ExpiresAt = code, expiresAt

	if _, err := tx.Exec(s.q(`DELETE FROM app_install_pending WHERE id = ?`), req.PendingID); err != nil {
		return nil, fmt.Errorf("provision app: delete pending: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("provision app: commit: %w", err)
	}
	return result, nil
}

// lockRowsForShare runs a FOR SHARE select and drains it: the locks are taken
// as the rows are read.
func lockRowsForShare(tx *sql.Tx, query string, args ...any) error {
	rows, err := tx.Query(query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
	}
	return rows.Err()
}

// mintInstallCodeTx mints a fresh install code for installID: 128 random
// bits, stored only as their SHA-256, redeemable once within InstallCodeTTL.
func (s *Store) mintInstallCodeTx(tx *sql.Tx, installID string) (string, time.Time, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, fmt.Errorf("install code: %w", err)
	}
	code := "padic_" + base64.RawURLEncoding.EncodeToString(raw)
	expiresAt := time.Now().Add(InstallCodeTTL).UTC().Truncate(time.Second)
	if _, err := tx.Exec(s.q(`INSERT INTO app_install_codes (code_sha256, install_id, expires_at, created_at) VALUES (?, ?, ?, ?)`),
		installCodeHash(code), installID, timeText(expiresAt), now()); err != nil {
		return "", time.Time{}, fmt.Errorf("install code: insert: %w", err)
	}
	return code, expiresAt, nil
}

func installCodeHash(code string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(code)))
	return hex.EncodeToString(sum[:])
}

// discardSecret drops a secret nobody may see. Go strings are immutable, so
// this cannot scrub the bytes; it makes the discard explicit at the call
// site and keeps the value out of every later statement.
func discardSecret(s *string) { *s = "" }

// IssueInstallCode mints a new install code for an ACTIVE install in
// workspaceID: the owner's recovery when the app lost its redeem response
// (lead ruling, U8b Q2). Earlier unconsumed codes stay valid until they
// expire; each redeem rotates the secret.
func (s *Store) IssueInstallCode(workspaceID, installID string) (string, time.Time, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", time.Time{}, err
	}
	defer tx.Rollback()
	lockQ := `SELECT state FROM app_installs WHERE id = ? AND workspace_id = ?`
	if s.dialect.Driver() == DriverPostgres {
		lockQ += ` FOR UPDATE`
	}
	var state string
	err = tx.QueryRow(s.q(lockQ), installID, workspaceID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && state != "active") {
		return "", time.Time{}, ErrInstallNotActive
	}
	if err != nil {
		return "", time.Time{}, fmt.Errorf("issue install code: %w", err)
	}
	code, exp, err := s.mintInstallCodeTx(tx, installID)
	if err != nil {
		return "", time.Time{}, err
	}
	if err := tx.Commit(); err != nil {
		return "", time.Time{}, err
	}
	return code, exp, nil
}

// RedeemedInstall is what redeem returns to the app, exactly once.
type RedeemedInstall struct {
	InstallID    string
	ClientID     string
	ClientSecret string
}

// RedeemInstallCode consumes a code and rotates the install client's secret
// in one transaction. Every failure is ErrInstallCodeInvalid.
//
// Lock order is the install row first, then the code row: the order disable,
// rotate and uninstall (U8c) take, so a redeem racing them cannot deadlock.
// The code is read unlocked only to learn its install, and re-read under the
// lock before anything is decided.
//
// The epoch is not bumped. A redeem replaces the secret; ending the grants
// issued under an earlier one is what disable and rotate do (U8c).
func (s *Store) RedeemInstallCode(code string) (*RedeemedInstall, error) {
	hash := installCodeHash(code)
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var installID string
	err = tx.QueryRow(s.q(`SELECT install_id FROM app_install_codes WHERE code_sha256 = ?`), hash).Scan(&installID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInstallCodeInvalid
	}
	if err != nil {
		return nil, fmt.Errorf("redeem install code: %w", err)
	}
	forUpdate := ""
	if s.dialect.Driver() == DriverPostgres {
		forUpdate = ` FOR UPDATE`
	}
	var state string
	if err := tx.QueryRow(s.q(`SELECT state FROM app_installs WHERE id = ?`+forUpdate), installID).Scan(&state); err != nil || state != "active" {
		return nil, ErrInstallCodeInvalid
	}
	var expires string
	var consumed sql.NullString
	if err := tx.QueryRow(s.q(`SELECT expires_at, consumed_at FROM app_install_codes WHERE code_sha256 = ?`+forUpdate), hash).Scan(&expires, &consumed); err != nil {
		return nil, ErrInstallCodeInvalid
	}
	if consumed.Valid || expires <= timeText(time.Now()) {
		return nil, ErrInstallCodeInvalid
	}
	if _, err := tx.Exec(s.q(`UPDATE app_install_codes SET consumed_at = ? WHERE code_sha256 = ?`), now(), hash); err != nil {
		return nil, fmt.Errorf("redeem install code: consume: %w", err)
	}
	secret, err := s.RotateInstallClientSecretTx(tx, installID)
	if err != nil {
		return nil, fmt.Errorf("redeem install code: %w", err)
	}
	clientID, err := s.InstallClientIDTx(tx, installID)
	if err != nil {
		return nil, fmt.Errorf("redeem install code: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &RedeemedInstall{InstallID: installID, ClientID: clientID, ClientSecret: secret}, nil
}

// InstallForLiveCode reads, unlocked, which install a code names, for the
// redeem route's per-install limiter, and only for a code that could still
// redeem (unconsumed, unexpired, install active). A dead code then answers
// exactly like an unknown one, never with a 429, so the limiter says nothing
// about which codes exist and an old code cannot throttle a fresh one (codex
// round 1). It decides nothing: RedeemInstallCode re-reads under its locks.
func (s *Store) InstallForLiveCode(code string) (string, bool) {
	var id string
	err := s.db.QueryRow(s.q(`SELECT c.install_id FROM app_install_codes c JOIN app_installs a ON a.id = c.install_id
		WHERE c.code_sha256 = ? AND c.consumed_at IS NULL AND c.expires_at > ? AND a.state = 'active'`),
		installCodeHash(code), timeText(time.Now())).Scan(&id)
	if err != nil {
		return "", false
	}
	return id, true
}

// requireWorkspaceOwnerTx refuses unless userID is an owner of workspaceID,
// read on tx. A plain read on purpose: see the lock comment on
// ProvisionAppInstall.
func (s *Store) requireWorkspaceOwnerTx(tx *sql.Tx, workspaceID, userID string) error {
	q := `SELECT role FROM workspace_members WHERE workspace_id = ? AND user_id = ?`
	var role string
	err := tx.QueryRow(s.q(q), workspaceID, userID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && role != "owner") {
		return ErrNotWorkspaceOwner
	}
	if err != nil {
		return fmt.Errorf("provision app: owner role: %w", err)
	}
	return nil
}
