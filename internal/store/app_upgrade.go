package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// App upgrade (SPEC-6 U8b2, DOC-3371 §2 "Upgrade"; TASK-3397).
//
// An upgrade stages like an install (the pending record, upgrade_of = the
// install), is previewed with a diff, and is applied by UpgradeAppInstall in
// ONE transaction that re-derives the normalization and the diff under locks
// (the U8b rule: the comparison is the re-check).

// InstallUpgradeState is what an upgrade diffs against.
type InstallUpgradeState struct {
	ID              string
	WorkspaceID     string
	Origin          string
	State           string
	ManifestSHA256  string
	ManifestJSON    string
	ServiceAccess   string
	DelegatedAccess string
	DigestsJSON     string
	BotUserID       string
}

// ErrInstallMoved: the install changed since the upgrade was previewed (its
// manifest, or its state), so the reviewed diff no longer describes it.
var ErrInstallMoved = errors.New("install changed since the upgrade was previewed")

// GetInstallUpgradeStateQ reads an install of workspaceID on q.
func (s *Store) GetInstallUpgradeStateQ(q Queryer, workspaceID, installID string) (*InstallUpgradeState, error) {
	var st InstallUpgradeState
	var sha, manifest, svc, del, bot sql.NullString
	err := q.QueryRow(s.q(`SELECT id, workspace_id, origin, state, manifest_sha256, manifest, service_access, delegated_access, digests, bot_user_id
		FROM app_installs WHERE id = ? AND workspace_id = ?`), installID, workspaceID).
		Scan(&st.ID, &st.WorkspaceID, &st.Origin, &st.State, &sha, &manifest, &svc, &del, &st.DigestsJSON, &bot)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInstallNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read install: %w", err)
	}
	st.ManifestSHA256, st.ManifestJSON, st.ServiceAccess, st.DelegatedAccess, st.BotUserID = sha.String, manifest.String, svc.String, del.String, bot.String
	return &st, nil
}

// UpgradeSchemaAdd appends one optional field to an existing companion.
type UpgradeSchemaAdd struct {
	CollectionSlug string
	Field          models.FieldDef
}

// UpgradePlan is what an upgrade writes, derived inside the transaction.
type UpgradePlan struct {
	ManifestSHA256  string
	ManifestVersion string
	ManifestJSON    string
	DigestsJSON     string
	ServiceAccess   string
	DelegatedAccess string
	RedirectURIs    []string
	SourcePack      string
	// NewCollections are companion collections the upgrade adds.
	NewCollections []ProvisionCollection
	// SchemaAdds are optional fields appended to existing companions.
	SchemaAdds []UpgradeSchemaAdd
	// Released are companion slugs released to the workspace (data kept).
	Released []string
	// Renamed maps an existing companion's slug to its new display name.
	Renamed map[string]string
	// Artifacts land as NEW drafts (changed or added); nothing is overwritten.
	Artifacts []ProvisionArtifact
	// Restrictive: the upgrade narrows what the app may do (an access level
	// narrowed, or a companion released). The install's epoch is bumped, so
	// a write admitted before the upgrade cannot commit after it and every
	// token issued before it is refused (codex r1 on U8b2).
	Restrictive bool
	// Webhook is the new manifest's hook (events by companion SLUG); nil
	// removes it. An existing hook keeps its secret and delivered state; a
	// new one starts HELD until a redeem (U10a).
	Webhook *AppWebhookSpec
}

// UpgradeRequest identifies the upgrade being confirmed.
type UpgradeRequest struct {
	PendingID, WorkspaceID, OwnerID, InstallID string
	// ManifestSHA256 is the new manifest the owner reviewed.
	ManifestSHA256 string
	// FromManifestSHA256 is the installed manifest the diff was computed
	// against; the install must still be at it.
	FromManifestSHA256 string
}

// UpgradeDeriveFunc re-derives the upgrade on the transaction and compares it
// with the reviewed preview, returning what to write. Read-only.
type UpgradeDeriveFunc func(q Queryer, install *InstallUpgradeState) (*UpgradePlan, error)

// UpgradeAppInstall applies a reviewed upgrade in ONE transaction.
//
// Locks (Postgres), in this order: the owner's users row FOR SHARE (as
// provisioning, against account deletion); the pending row FOR UPDATE; the
// bot's membership row FOR UPDATE (UninstallAppTx's order, since this
// transaction updates the bot's role); the INSTALL row FOR UPDATE, BEFORE the workspace lock, because a fenced app
// write holds the install row FOR SHARE and then takes the workspace lock
// (an item create), so the opposite order would be a cycle; then the
// workspace seq lock; then every collection row of the workspace FOR NO KEY
// UPDATE (a consistent read for the derivation, and this transaction updates
// some of them). SQLite: BEGIN IMMEDIATE. The lock rule is ProvisionAppInstall's:
// locks break dependency cycles, they do not cover every read.
func (s *Store) UpgradeAppInstall(req UpgradeRequest, derive UpgradeDeriveFunc) ([]*models.Item, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("upgrade app: begin: %w", err)
	}
	defer tx.Rollback()
	pg := s.dialect.Driver() == DriverPostgres
	forUpdate, forShare := "", ""
	if pg {
		forUpdate, forShare = " FOR UPDATE", " FOR SHARE"
	}

	var id string
	if err := tx.QueryRow(s.q(`SELECT id FROM users WHERE id = ?`+forShare), req.OwnerID).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotWorkspaceOwner
		}
		return nil, fmt.Errorf("upgrade app: lock owner: %w", err)
	}

	var ws, owner, state, manifestSHA, expires string
	var upgradeOf sql.NullString
	err = tx.QueryRow(s.q(`SELECT workspace_id, owner_id, state, COALESCE(manifest_sha256, ''), expires_at, upgrade_of
		FROM app_install_pending WHERE id = ?`+forUpdate), req.PendingID).Scan(&ws, &owner, &state, &manifestSHA, &expires, &upgradeOf)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPendingNotStaged
	}
	if err != nil {
		return nil, fmt.Errorf("upgrade app: lock pending: %w", err)
	}
	if ws != req.WorkspaceID || owner != req.OwnerID || state != "staged" || expires <= timeText(time.Now()) || upgradeOf.String != req.InstallID {
		return nil, ErrPendingNotStaged
	}
	if manifestSHA != req.ManifestSHA256 {
		return nil, ErrPendingManifestChanged
	}

	// The bot's membership row before the install row: the order
	// UninstallAppTx takes them in (this transaction UPDATEs the bot's role
	// later), so the two cannot form a cycle whatever the install's state.
	if pg {
		var bot sql.NullString
		if err := tx.QueryRow(s.q(`SELECT bot_user_id FROM app_installs WHERE id = ? AND workspace_id = ?`), req.InstallID, req.WorkspaceID).Scan(&bot); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("upgrade app: read bot: %w", err)
		}
		if bot.Valid && bot.String != "" {
			if err := lockRowsForShare(tx, s.q(`SELECT user_id FROM workspace_members WHERE workspace_id = ? AND user_id = ? FOR UPDATE`), req.WorkspaceID, bot.String); err != nil {
				return nil, fmt.Errorf("upgrade app: lock bot membership: %w", err)
			}
		}
	}
	if _, err := s.lockInstallTx(tx, req.WorkspaceID, req.InstallID); err != nil {
		return nil, err
	}
	install, err := s.GetInstallUpgradeStateQ(tx, req.WorkspaceID, req.InstallID)
	if err != nil {
		return nil, err
	}
	if install.ManifestSHA256 != req.FromManifestSHA256 || (install.State != InstallActive && install.State != InstallInactive) {
		return nil, ErrInstallMoved
	}

	if err := s.acquireWorkspaceSeqLock(tx, req.WorkspaceID); err != nil {
		return nil, err
	}
	if pg {
		if err := lockRowsForShare(tx, s.q(`SELECT id FROM collections WHERE workspace_id = ? ORDER BY id FOR NO KEY UPDATE`), req.WorkspaceID); err != nil {
			return nil, fmt.Errorf("upgrade app: lock collections: %w", err)
		}
	}
	if err := s.requireWorkspaceOwnerTx(tx, req.WorkspaceID, req.OwnerID); err != nil {
		return nil, err
	}

	plan, err := derive(tx, install)
	if err != nil {
		return nil, err
	}

	// The install row; a restrictive upgrade also bumps the epoch, under the
	// install row lock this transaction holds (the U5a contract).
	epochBump := ""
	if plan.Restrictive {
		epochBump = ", auth_epoch = auth_epoch + 1"
	}
	if _, err := tx.Exec(s.q(`UPDATE app_installs SET manifest_sha256 = ?, manifest_version = ?, manifest = ?, digests = ?,
		service_access = ?, delegated_access = ?, updated_at = ?`+epochBump+` WHERE id = ?`),
		plan.ManifestSHA256, plan.ManifestVersion, plan.ManifestJSON, plan.DigestsJSON,
		plan.ServiceAccess, plan.DelegatedAccess, now(), req.InstallID); err != nil {
		return nil, fmt.Errorf("upgrade app: install row: %w", err)
	}

	// The bot: its membership role follows the service access. U6a reads the
	// access per request, so the new level applies from the next request.
	if install.BotUserID != "" && plan.ServiceAccess != install.ServiceAccess {
		role := "viewer"
		if plan.ServiceAccess == "write" {
			role = "editor"
		}
		if _, err := tx.Exec(s.q(`UPDATE workspace_members SET role = ? WHERE workspace_id = ? AND user_id = ?`), role, req.WorkspaceID, install.BotUserID); err != nil {
			return nil, fmt.Errorf("upgrade app: bot role: %w", err)
		}
	}

	// The client's redirect URIs.
	if plan.RedirectURIs != nil {
		b, _ := json.Marshal(plan.RedirectURIs)
		if _, err := tx.Exec(s.q(`UPDATE oauth_clients SET redirect_uris = ? WHERE app_install_id = ?`), string(b), req.InstallID); err != nil {
			return nil, fmt.Errorf("upgrade app: redirect uris: %w", err)
		}
	}

	// Released companions: no longer the app's (via_app cleared, so the U6a
	// ceiling drops them), out of the bot's access list; the collection and
	// its items stay as an ordinary collection (lead ruling 1). Human
	// members' access is unchanged.
	for _, slug := range plan.Released {
		var collID string
		err := tx.QueryRow(s.q(`SELECT id FROM collections WHERE workspace_id = ? AND slug = ? AND via_app = ?`), req.WorkspaceID, slug, req.InstallID).Scan(&collID)
		if errors.Is(err, sql.ErrNoRows) {
			// Never skipped: a companion that no longer resolves by its slug
			// would keep via_app and the bot's access (codex r1 on U8b2).
			return nil, &ProvisionConflictError{Collection: slug, Field: "slug", Detail: "this installed companion no longer resolves (renamed or deleted); restore its slug to upgrade the app"}
		}
		if err != nil {
			return nil, fmt.Errorf("upgrade app: release %q: %w", slug, err)
		}
		if _, err := tx.Exec(s.q(`UPDATE collections SET via_app = NULL, updated_at = ? WHERE id = ?`), now(), collID); err != nil {
			return nil, fmt.Errorf("upgrade app: release %q: %w", slug, err)
		}
		if install.BotUserID != "" {
			if _, err := tx.Exec(s.q(`DELETE FROM member_collection_access WHERE workspace_id = ? AND user_id = ? AND collection_id = ?`), req.WorkspaceID, install.BotUserID, collID); err != nil {
				return nil, fmt.Errorf("upgrade app: release %q access: %w", slug, err)
			}
		}
	}

	// Additive optional fields on existing companions, appended to the
	// CURRENT schema. The derivation refused a key the collection already
	// holds. Nothing to reindex: no existing item has a value for a new key.
	bySlug := map[string][]models.FieldDef{}
	for _, add := range plan.SchemaAdds {
		bySlug[add.CollectionSlug] = append(bySlug[add.CollectionSlug], add.Field)
	}
	for slug, fields := range bySlug {
		if err := s.CheckAdditiveFieldsQ(tx, req.WorkspaceID, req.InstallID, slug, fields); err != nil {
			return nil, err
		}
	}
	for _, add := range plan.SchemaAdds {
		var collID, schemaJSON string
		if err := tx.QueryRow(s.q(`SELECT id, schema FROM collections WHERE workspace_id = ? AND slug = ? AND via_app = ?`), req.WorkspaceID, add.CollectionSlug, req.InstallID).Scan(&collID, &schemaJSON); err != nil {
			return nil, fmt.Errorf("upgrade app: schema of %q: %w", add.CollectionSlug, err)
		}
		var schema models.CollectionSchema
		if err := json.Unmarshal([]byte(schemaJSON), &schema); err != nil {
			return nil, fmt.Errorf("upgrade app: schema of %q: %w", add.CollectionSlug, err)
		}
		for _, f := range schema.Fields {
			if f.Key == add.Field.Key {
				return nil, &ProvisionConflictError{Collection: add.CollectionSlug, Field: add.Field.Key, Detail: "the collection already has this field"}
			}
		}
		schema.Fields = append(schema.Fields, add.Field)
		b, err := json.Marshal(schema)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(s.q(`UPDATE collections SET schema = ?, updated_at = ? WHERE id = ?`), string(b), now(), collID); err != nil {
			return nil, fmt.Errorf("upgrade app: schema of %q: %w", add.CollectionSlug, err)
		}
	}

	for slug, name := range plan.Renamed {
		if _, err := tx.Exec(s.q(`UPDATE collections SET name = ?, updated_at = ? WHERE workspace_id = ? AND slug = ? AND via_app = ?`), name, now(), req.WorkspaceID, slug, req.InstallID); err != nil {
			return nil, fmt.Errorf("upgrade app: rename %q: %w", slug, err)
		}
	}

	// New companions, granted to the bot.
	for _, c := range plan.NewCollections {
		id, err := s.createCollectionTx(tx, req.WorkspaceID, models.CollectionCreate{Name: c.Name, Slug: c.Slug, Schema: c.Schema}, req.InstallID)
		if errors.Is(err, ErrAppCollectionSlugTaken) {
			return nil, &ProvisionConflictError{Collection: c.Key, Field: "slug", Detail: fmt.Sprintf("a collection %q now exists in this workspace", c.Slug)}
		}
		if err != nil {
			return nil, fmt.Errorf("upgrade app: create collection %q: %w", c.Slug, err)
		}
		if install.BotUserID != "" {
			if _, err := tx.Exec(s.q(`INSERT INTO member_collection_access (workspace_id, user_id, collection_id, created_at) VALUES (?, ?, ?, ?)`),
				req.WorkspaceID, install.BotUserID, id, now()); err != nil {
				return nil, fmt.Errorf("upgrade app: grant %q: %w", c.Slug, err)
			}
		}
	}

	// The hook follows the new manifest, after its companions are settled.
	if err := s.upsertAppWebhookTx(tx, req.WorkspaceID, req.InstallID, plan.Webhook); err != nil {
		return nil, err
	}

	// Changed and added artifacts land as NEW drafts; the installed item is
	// never overwritten.
	var items []*models.Item
	for _, a := range plan.Artifacts {
		fieldsJSON, err := json.Marshal(a.Fields)
		if err != nil {
			return nil, err
		}
		item, err := s.createItemTxWithID(tx, newID(), req.WorkspaceID, a.CollectionID, models.ItemCreate{
			Title: a.Title, Content: a.Content, Fields: string(fieldsJSON),
			CreatedBy: "user", Source: "web", ActorUserID: req.OwnerID,
		}, mintOptions{})
		if err != nil {
			if isUniqueViolation(err) {
				return nil, &ProvisionConflictError{Artifact: a.Key, Detail: "it collides with an existing item (duplicate slug or invocation slug)"}
			}
			return nil, fmt.Errorf("upgrade app: artifact %q: %w", a.Key, err)
		}
		if _, err := tx.Exec(s.q(`UPDATE items SET source_pack = ?, source_artifact_sha256 = ?, installed_sha256 = ? WHERE id = ?`),
			plan.SourcePack, a.RawSHA256, a.NormalizedSHA256, item.ID); err != nil {
			return nil, fmt.Errorf("upgrade app: stamp artifact %q: %w", a.Key, err)
		}
		items = append(items, item)
	}

	if _, err := tx.Exec(s.q(`DELETE FROM app_install_pending WHERE id = ?`), req.PendingID); err != nil {
		return nil, fmt.Errorf("upgrade app: delete pending: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("upgrade app: commit: %w", err)
	}
	return items, nil
}

// CheckCompanionsResolveQ refuses unless every installed companion slug still
// names a live collection stamped via_app = installID. An owner rename or
// delete of a companion is drift the upgrade cannot map (it addresses
// companions by slug), so it refuses rather than guessing (codex r1 on U8b2).
func (s *Store) CheckCompanionsResolveQ(q Queryer, workspaceID, installID string, slugs []string) error {
	for _, slug := range slugs {
		var n int
		if err := q.QueryRow(s.q(`SELECT COUNT(*) FROM collections WHERE workspace_id = ? AND slug = ? AND via_app = ? AND deleted_at IS NULL`),
			workspaceID, slug, installID).Scan(&n); err != nil {
			return fmt.Errorf("check companion %q: %w", slug, err)
		}
		if n == 0 {
			return &ProvisionConflictError{Collection: slug, Field: "slug",
				Detail: "this installed companion no longer resolves (renamed or deleted in this workspace); restore its slug to upgrade the app"}
		}
	}
	return nil
}

// CheckAdditiveFieldsQ refuses an additive field that is not safe for the
// collection's EXISTING items, decided against the current data (codex r1 on
// U8b2): a key the schema already declares; a key items already hold values
// under (human writes accept undeclared keys, so a new unique or relation
// field would start out violated or unindexed); and a field that would change
// the collection's done field (DoneFieldKey also reads settings, so a plain
// select named by board_group_by would reclassify every existing item).
func (s *Store) CheckAdditiveFieldsQ(q Queryer, workspaceID, installID, slug string, fields []models.FieldDef) error {
	var collID, schemaJSON, settingsJSON string
	err := q.QueryRow(s.q(`SELECT id, schema, settings FROM collections WHERE workspace_id = ? AND slug = ? AND via_app = ? AND deleted_at IS NULL`),
		workspaceID, slug, installID).Scan(&collID, &schemaJSON, &settingsJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return &ProvisionConflictError{Collection: slug, Field: "slug", Detail: "this installed companion no longer resolves (renamed or deleted)"}
	}
	if err != nil {
		return fmt.Errorf("check additive fields of %q: %w", slug, err)
	}
	var schema models.CollectionSchema
	if err := json.Unmarshal([]byte(schemaJSON), &schema); err != nil {
		return fmt.Errorf("check additive fields of %q: schema: %w", slug, err)
	}
	var settings models.CollectionSettings
	if settingsJSON != "" {
		_ = json.Unmarshal([]byte(settingsJSON), &settings) // an unreadable settings blob reads as defaults, as everywhere else
	}
	before := models.DoneFieldKey(schema, settings)
	next := schema
	for _, f := range fields {
		for _, have := range schema.Fields {
			if have.Key == f.Key {
				return &ProvisionConflictError{Collection: slug, Field: f.Key, Detail: "the collection already declares this field"}
			}
		}
		if !isValidFieldKey(f.Key) {
			return &ProvisionConflictError{Collection: slug, Field: f.Key, Detail: "not a valid field key"}
		}
		var n int
		expr := s.dialect.JSONExtractText("fields", f.Key)
		// ARCHIVED items count too: RestoreItem clears deleted_at without
		// re-validating fields, so an archived value would come back
		// unchecked under the new field (codex r2 on U8b2).
		if err := q.QueryRow(s.q(`SELECT COUNT(*) FROM items WHERE collection_id = ? AND `+expr+` IS NOT NULL`), collID).Scan(&n); err != nil {
			return fmt.Errorf("check additive field %q: %w", f.Key, err)
		}
		if n > 0 {
			return &ProvisionConflictError{Collection: slug, Field: f.Key, Detail: fmt.Sprintf("%d existing items (archived ones included) already hold values under this key; declaring it now would leave them unchecked", n)}
		}
		next.Fields = append(next.Fields, f)
	}
	if after := models.DoneFieldKey(next, settings); after != before {
		return &ProvisionConflictError{Collection: slug, Field: after,
			Detail: fmt.Sprintf("adding it would change the collection's done field from %q to %q (its board_group_by setting), reclassifying existing items", before, after)}
	}
	return nil
}
