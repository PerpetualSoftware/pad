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
// transaction it re-verifies every state-dependent fact the preview relied
// on, under the locks every writer of that state takes, then writes the
// install, its companion collections, its bot, its client, its artifacts and
// its install code, and deletes the pending record. Any refusal rolls the
// whole transaction back and leaves the pending record for a retry or a
// discard.

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
	Key          string
	CollectionID string
	// CollectionSchema is the destination schema the normalization used,
	// byte for byte. Every other field below was derived from it, so
	// provisioning refuses if it moved (codex round 1).
	CollectionSchema string
	Title            string
	Content          string
	Fields           map[string]any
	// UniqueKeys are the field keys whose values must be free in the
	// destination collection: invocation_slug (a database index) and every
	// unique-scoped field.
	UniqueKeys []string
	// RelationTargets maps each relation field (scalar or multi) the item
	// stores RESOLVED values in to that field's declared collection slug. Values the
	// preview carried unresolved are not listed: they named nothing then and
	// are stored as text either way.
	RelationTargets  map[string]string
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
	Collections     []ProvisionCollection
	Artifacts       []ProvisionArtifact
}

// ProvisionResult is what a successful provisioning wrote.
type ProvisionResult struct {
	InstallID   string
	InstallCode string
	ExpiresAt   time.Time
	BotUserID   string
	Items       []*models.Item
}

// ProvisionAppInstall provisions an app install in one transaction. See the
// file comment.
func (s *Store) ProvisionAppInstall(req ProvisionRequest) (*ProvisionResult, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("provision app: begin: %w", err)
	}
	defer tx.Rollback()

	// 1. The pending record: live, staged, this owner's, this workspace's,
	// and the very manifest the owner reviewed.
	lockQ := `SELECT workspace_id, owner_id, origin, state, COALESCE(manifest_sha256, ''), expires_at FROM app_install_pending WHERE id = ?`
	if s.dialect.Driver() == DriverPostgres {
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

	// 2. The workspace lock every item, collection and slug writer takes, so
	// nothing re-checked below can move before commit.
	if err := s.acquireWorkspaceSeqLock(tx, req.WorkspaceID); err != nil {
		return nil, err
	}

	// 3a. Companion collections: a created slug must still be free, and an
	// adopted one must still be this origin's.
	for _, c := range req.Collections {
		var holderOrigin sql.NullString
		var found bool
		err := tx.QueryRow(s.q(`SELECT a.origin FROM collections c LEFT JOIN app_installs a ON a.id = c.via_app
			WHERE c.workspace_id = ? AND c.slug = ?`), req.WorkspaceID, c.Slug).Scan(&holderOrigin)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return nil, fmt.Errorf("provision app: collection slug %q: %w", c.Slug, err)
		default:
			found = true
		}
		if c.Adopt && (!found || holderOrigin.String != req.Origin) {
			return nil, &ProvisionConflictError{Collection: c.Key, Field: "slug",
				Detail: fmt.Sprintf("the collection %q this app was going to adopt is no longer one an install of %s created", c.Slug, req.Origin)}
		}
		if !c.Adopt && found {
			return nil, &ProvisionConflictError{Collection: c.Key, Field: "slug",
				Detail: fmt.Sprintf("a collection %q now exists in this workspace", c.Slug)}
		}
	}

	// 4. The install row.
	installID := newID()
	ts := now()
	if _, err := tx.Exec(s.q(`INSERT INTO app_installs (id, workspace_id, origin, state, auth_epoch,
		    manifest_sha256, manifest_version, manifest, service_access, delegated_access, digests, created_at, updated_at)
		VALUES (?, ?, ?, 'active', 1, ?, ?, ?, ?, ?, ?, ?, ?)`),
		installID, req.WorkspaceID, req.Origin, req.ManifestSHA256, req.ManifestVersion, req.ManifestJSON,
		req.ServiceAccess, req.DelegatedAccess, req.DigestsJSON, ts, ts); err != nil {
		return nil, fmt.Errorf("provision app: insert install: %w", err)
	}

	// 5. Companion collections, created or adopted.
	var companionIDs []string
	for _, c := range req.Collections {
		if c.Adopt {
			var id string
			if err := tx.QueryRow(s.q(`SELECT id FROM collections WHERE workspace_id = ? AND slug = ?`), req.WorkspaceID, c.Slug).Scan(&id); err != nil {
				return nil, fmt.Errorf("provision app: adopt %q: %w", c.Slug, err)
			}
			companionIDs = append(companionIDs, id)
			continue
		}
		id, err := s.createCollectionTx(tx, req.WorkspaceID, models.CollectionCreate{Name: c.Name, Slug: c.Slug, Schema: c.Schema}, installID)
		if errors.Is(err, ErrAppCollectionSlugTaken) {
			return nil, &ProvisionConflictError{Collection: c.Key, Field: "slug", Detail: fmt.Sprintf("a collection %q now exists in this workspace", c.Slug)}
		}
		if err != nil {
			return nil, fmt.Errorf("provision app: create collection %q: %w", c.Slug, err)
		}
		companionIDs = append(companionIDs, id)
	}

	// 6. The bot: its principal, its membership by declared service access,
	// and visibility over exactly the companion collections (§3).
	bot, err := s.CreateAppUserTx(tx, installID, req.AppTitle)
	if err != nil {
		return nil, fmt.Errorf("provision app: %w", err)
	}
	if _, err := tx.Exec(s.q(`UPDATE app_installs SET bot_user_id = ? WHERE id = ?`), bot.ID, installID); err != nil {
		return nil, fmt.Errorf("provision app: bind bot: %w", err)
	}
	role := "viewer"
	if req.ServiceAccess == "write" {
		role = "editor"
	}
	if err := s.addAppPrincipalMemberTx(tx, req.WorkspaceID, bot.ID, role); err != nil {
		return nil, fmt.Errorf("provision app: bot membership: %w", err)
	}
	if err := s.setMemberCollectionAccessTx(tx, req.WorkspaceID, bot.ID, "specific", companionIDs); err != nil {
		return nil, fmt.Errorf("provision app: bot collection access: %w", err)
	}

	// 7. The install client. Its first secret is discarded unseen: redeem
	// rotates and returns the one the app holds (lead ruling, U8b Q2).
	if _, secret, err := s.CreateInstallClientTx(tx, installID, req.RedirectURIs); err != nil {
		return nil, fmt.Errorf("provision app: %w", err)
	} else {
		discardSecret(&secret)
	}

	// 3b + 8. Each artifact is re-checked against the transaction's own view
	// (which includes the artifacts inserted before it, in manifest order) and
	// then inserted as a draft through the owner create's transactional path.
	result := &ProvisionResult{InstallID: installID, BotUserID: bot.ID}
	for _, a := range req.Artifacts {
		if err := s.recheckProvisionArtifactTx(tx, req.WorkspaceID, a); err != nil {
			return nil, err
		}
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

	// 10. The install code.
	code, expiresAt, err := s.mintInstallCodeTx(tx, installID)
	if err != nil {
		return nil, err
	}
	result.InstallCode, result.ExpiresAt = code, expiresAt

	// 11. The pending record and its blobs (ON DELETE CASCADE).
	if _, err := tx.Exec(s.q(`DELETE FROM app_install_pending WHERE id = ?`), req.PendingID); err != nil {
		return nil, fmt.Errorf("provision app: delete pending: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("provision app: commit: %w", err)
	}
	return result, nil
}

// recheckProvisionArtifactTx re-verifies, on the provisioning transaction,
// every workspace-state fact the preview relied on for one artifact.
func (s *Store) recheckProvisionArtifactTx(tx *sql.Tx, workspaceID string, a ProvisionArtifact) error {
	// The schema first: UniqueKeys, RelationTargets and the normalized fields
	// all derive from it. A schema edit that moves it takes the workspace
	// lock this transaction holds, so this read is the committed schema.
	var schema string
	err := tx.QueryRow(s.q(`SELECT schema FROM collections WHERE id = ? AND deleted_at IS NULL`), a.CollectionID).Scan(&schema)
	if errors.Is(err, sql.ErrNoRows) {
		return &ProvisionConflictError{Artifact: a.Key, Detail: "its destination collection no longer exists"}
	}
	if err != nil {
		return fmt.Errorf("provision app: destination schema: %w", err)
	}
	if schema != a.CollectionSchema {
		return &ProvisionConflictError{Artifact: a.Key, Detail: "its destination collection's schema changed since the review"}
	}
	for _, key := range a.UniqueKeys {
		raw, ok := a.Fields[key]
		if !ok || raw == nil {
			continue
		}
		val, isText := raw.(string)
		if !isText || val == "" {
			// The import's unique check (checkUniqueFields) compares text
			// values only, and the preview refuses a non-text invocation_slug.
			continue
		}
		var n int
		if key == "invocation_slug" {
			// The index's own expression, as InvocationSlugIndexTaken.
			expr := `json_extract(fields, '$.invocation_slug')`
			if s.dialect.Driver() == DriverPostgres {
				expr = `(fields->>'invocation_slug')`
			}
			if err := tx.QueryRow(s.q(`SELECT COUNT(*) FROM items WHERE collection_id = ? AND deleted_at IS NULL
				AND `+expr+` IS NOT NULL AND `+expr+` != '' AND `+expr+` = ?`), a.CollectionID, val).Scan(&n); err != nil {
				return fmt.Errorf("provision app: invocation slug check: %w", err)
			}
		} else {
			cond, args := s.dialect.JSONFieldEquals("i.fields", key, val)
			args = append([]any{a.CollectionID}, args...)
			if err := tx.QueryRow(s.q(`SELECT COUNT(*) FROM items i WHERE i.collection_id = ? AND i.deleted_at IS NULL AND `+cond), args...).Scan(&n); err != nil {
				return fmt.Errorf("provision app: unique check %q: %w", key, err)
			}
		}
		if n > 0 {
			holder := s.uniqueHolderRefTx(tx, a.CollectionID, key, val)
			return &ProvisionConflictError{Artifact: a.Key, Field: key,
				Detail: fmt.Sprintf("the value %q is already held by %s", val, holder)}
		}
	}
	for key, collSlug := range a.RelationTargets {
		var vals []string
		switch v := a.Fields[key].(type) {
		case string:
			vals = []string{v}
		case []any:
			for _, e := range v {
				str, ok := e.(string)
				if !ok {
					return &ProvisionConflictError{Artifact: a.Key, Field: key, Detail: "a reference in it is not text"}
				}
				vals = append(vals, str)
			}
		case nil:
		default:
			return &ProvisionConflictError{Artifact: a.Key, Field: key, Detail: "its value is not a reference"}
		}
		for _, val := range vals {
			if val == "" {
				continue
			}
			item, err := s.resolveRelationTargetQ(tx, workspaceID, val)
			if err != nil {
				return fmt.Errorf("provision app: relation %q: %w", key, err)
			}
			if item == nil || item.CollectionSlug != collSlug {
				return &ProvisionConflictError{Artifact: a.Key, Field: key,
					Detail: fmt.Sprintf("the item %q it referenced is no longer in %q", val, collSlug)}
			}
		}
	}
	return nil
}

// uniqueHolderRefTx names the item holding a unique value, for the refusal.
// The caller is the workspace owner, who can see every item, so naming it is
// no existence oracle. Falls back to a generic phrase if the read fails.
func (s *Store) uniqueHolderRefTx(tx *sql.Tx, collectionID, key, val string) string {
	var cond string
	var args []any
	if key == "invocation_slug" {
		expr := `json_extract(i.fields, '$.invocation_slug')`
		if s.dialect.Driver() == DriverPostgres {
			expr = `(i.fields->>'invocation_slug')`
		}
		cond, args = expr+` = ?`, []any{val}
	} else {
		cond, args = s.dialect.JSONFieldEquals("i.fields", key, val)
	}
	args = append([]any{collectionID}, args...)
	var prefix string
	var number int64
	err := tx.QueryRow(s.q(`SELECT c.prefix, i.item_number FROM items i JOIN collections c ON c.id = i.collection_id
		WHERE i.collection_id = ? AND i.deleted_at IS NULL AND `+cond+` ORDER BY i.item_number LIMIT 1`), args...).Scan(&prefix, &number)
	if err != nil {
		return "an existing item"
	}
	return fmt.Sprintf("%s-%d", prefix, number)
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
