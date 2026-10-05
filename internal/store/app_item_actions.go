package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/PerpetualSoftware/pad/internal/appmanifest"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// App item actions and context codes (SPEC-6 U11, DOC-3371 §6; TASK-3414).
//
// An install's manifest declares item actions: a labelled link-out offered
// on items of some of its companion collections. A viewer's click MINTS a
// single-use context code bound to (install, auth_epoch, item, action,
// action revision, viewer) and is sent to {base_url}{path}?pad_context=code;
// the app REDEEMS the code with its service token and receives the item.
//
// LOCKS (lead ruling 3, round-4 R4-3). Mint and redeem take, in this order,
// the install row FOR SHARE, the item row FOR SHARE and the action row FOR
// SHARE (Postgres; SQLite's BEGIN IMMEDIATE serializes). Every mutation that
// can invalidate a code takes those rows exclusively, in the same order:
//   - disable, rotate and uninstall hold the install row FOR UPDATE (U8c);
//   - an upgrade holds the install row FOR UPDATE, then UPDATEs the action
//     rows (syncAppItemActionsTx);
//   - item move and delete UPDATE the item row, which takes its row lock (FOR
//     NO KEY UPDATE, conflicting with FOR SHARE). The human paths hold no
//     install row, so they cannot form a cycle with install -> item; the app
//     path takes the install row first (FencedTx), the same order.
//
// EVERY refusal of a mint or a redeem is ErrContextCodeRefused, which the
// HTTP layer answers with one 404 body: unknown, expired, consumed, another
// install's, moved, deleted, upgraded, disabled, no longer visible. A redeem
// CONSUMES the code before it checks anything, so a mismatch burns it.

// ErrContextCodeRefused is every refusal of a mint or a redeem.
var ErrContextCodeRefused = errors.New("context code refused")

// contextCodeTTLSeconds is a code's lifetime: long enough for the browser
// round trip to the app and the app's redeem call, short enough that a code
// in a log or a history entry is useless by the time anyone reads it (§6).
const contextCodeTTLSeconds = 120

// AppActionSpec is one manifest item action, by companion collection SLUG.
type AppActionSpec struct {
	Key             string
	Label           string
	Path            string
	CollectionSlugs []string
}

type appActionRow struct {
	id, key, label, path string
	collections          []string
	revision             int
	active               bool
}

func (s *Store) loadAppActionsTx(tx *sql.Tx, installID string) (map[string]appActionRow, error) {
	rows, err := tx.Query(s.q(`SELECT id, action_key, label, path, collection_ids, revision, active FROM app_item_actions WHERE install_id = ?`), installID)
	if err != nil {
		return nil, fmt.Errorf("app actions: read: %w", err)
	}
	defer rows.Close()
	out := map[string]appActionRow{}
	for rows.Next() {
		var r appActionRow
		var colls string
		if err := rows.Scan(&r.id, &r.key, &r.label, &r.path, &colls, &r.revision, &r.active); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(colls), &r.collections); err != nil {
			return nil, fmt.Errorf("app actions: collections of %s: %w", r.key, err)
		}
		out[r.key] = r
	}
	return out, rows.Err()
}

// syncAppItemActionsTx makes the install's action rows match specs, on the
// caller's transaction (provisioning, upgrade, the backfill), which holds the
// install row. A changed action (label, path or collections) gets a new
// revision; a removed one is kept inactive, so a code minted for it before is
// refused; a re-added one is reactivated with a new revision.
func (s *Store) syncAppItemActionsTx(tx *sql.Tx, workspaceID, installID string, specs []AppActionSpec) error {
	existing, err := s.loadAppActionsTx(tx, installID)
	if err != nil {
		return err
	}
	ts := now()
	seen := map[string]bool{}
	for _, spec := range specs {
		seen[spec.Key] = true
		ids := make([]string, 0, len(spec.CollectionSlugs))
		for _, slug := range spec.CollectionSlugs {
			var id string
			err := tx.QueryRow(s.q(`SELECT id FROM collections WHERE workspace_id = ? AND slug = ? AND via_app = ? AND deleted_at IS NULL`),
				workspaceID, slug, installID).Scan(&id)
			if errors.Is(err, sql.ErrNoRows) {
				return &ProvisionConflictError{Collection: slug, Field: "item_actions",
					Detail: fmt.Sprintf("the item action %q names a companion collection this install does not hold", spec.Key)}
			}
			if err != nil {
				return fmt.Errorf("app actions: resolve %q: %w", slug, err)
			}
			ids = append(ids, id)
		}
		colls, _ := json.Marshal(ids)
		old, ok := existing[spec.Key]
		if !ok {
			if _, err := tx.Exec(s.q(`INSERT INTO app_item_actions (id, install_id, action_key, label, path, collection_ids, revision, active, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, ?)`),
				newID(), installID, spec.Key, spec.Label, spec.Path, string(colls), s.dialect.BoolToInt(true), ts, ts); err != nil {
				return fmt.Errorf("app actions: insert %q: %w", spec.Key, err)
			}
			continue
		}
		if old.active && old.label == spec.Label && old.path == spec.Path && slices.Equal(old.collections, ids) {
			continue
		}
		if _, err := tx.Exec(s.q(`UPDATE app_item_actions SET label = ?, path = ?, collection_ids = ?, revision = revision + 1, active = ?, updated_at = ? WHERE id = ?`),
			spec.Label, spec.Path, string(colls), s.dialect.BoolToInt(true), ts, old.id); err != nil {
			return fmt.Errorf("app actions: update %q: %w", spec.Key, err)
		}
	}
	for key, old := range existing {
		if seen[key] || !old.active {
			continue
		}
		if _, err := tx.Exec(s.q(`UPDATE app_item_actions SET active = ?, revision = revision + 1, updated_at = ? WHERE id = ?`),
			s.dialect.BoolToInt(false), ts, old.id); err != nil {
			return fmt.Errorf("app actions: retire %q: %w", key, err)
		}
	}
	return nil
}

func (s *Store) deleteAppItemActionsTx(tx *sql.Tx, installID string) error {
	if _, err := tx.Exec(s.q(`DELETE FROM app_item_actions WHERE install_id = ?`), installID); err != nil {
		return fmt.Errorf("app actions: delete: %w", err)
	}
	if _, err := tx.Exec(s.q(`DELETE FROM app_context_codes WHERE install_id = ?`), installID); err != nil {
		return fmt.Errorf("app actions: delete codes: %w", err)
	}
	return nil
}

// ItemAppAction is one action offered on an item, for the item pane.
type ItemAppAction struct {
	InstallID string `json:"install_id"`
	AppTitle  string `json:"app_title"`
	ActionKey string `json:"action_key"`
	Label     string `json:"label"`
}

// ListItemAppActions returns the active actions of the workspace's active
// installs that are offered on collectionID. The caller has already decided
// the viewer can see the item; a click is re-checked at mint.
func (s *Store) ListItemAppActions(workspaceID, collectionID string) ([]ItemAppAction, error) {
	rows, err := s.db.Query(s.q(`SELECT a.install_id, a.action_key, a.label, a.collection_ids, i.manifest
		FROM app_item_actions a JOIN app_installs i ON i.id = a.install_id
		WHERE i.workspace_id = ? AND i.state = 'active' AND a.active = ?
		ORDER BY i.created_at, a.action_key`), workspaceID, s.dialect.BoolToInt(true))
	if err != nil {
		return nil, fmt.Errorf("list item app actions: %w", err)
	}
	defer rows.Close()
	out := []ItemAppAction{}
	for rows.Next() {
		var a ItemAppAction
		var colls string
		var manifest sql.NullString
		if err := rows.Scan(&a.InstallID, &a.ActionKey, &a.Label, &colls, &manifest); err != nil {
			return nil, err
		}
		var ids []string
		if err := json.Unmarshal([]byte(colls), &ids); err != nil || !slices.Contains(ids, collectionID) {
			continue
		}
		a.AppTitle = manifestField(manifest.String, "title")
		out = append(out, a)
	}
	return out, rows.Err()
}

// manifestField reads one top-level string from a stored manifest.
func manifestField(manifestJSON, key string) string {
	var m map[string]any
	if json.Unmarshal([]byte(manifestJSON), &m) != nil {
		return ""
	}
	v, _ := m[key].(string)
	return v
}

// ContextVisibleFunc decides, on q, whether viewerID can see item: the human
// item read's own rule, with the viewer's current memberships and grants
// (lead ruling, day 88). Supplied by the server.
type ContextVisibleFunc func(q Queryer, item *models.Item, viewerID string) (bool, error)

// contextLocksQ takes the §6 locks in order on q (install, item, action) and
// returns what the checks need. Every miss is ErrContextCodeRefused.
type contextLocked struct {
	state, workspaceID, manifest string
	epoch                        int64
	item                         *models.Item
	action                       appActionRow
}

func (s *Store) contextLocksTx(tx *sql.Tx, installID, itemID, actionKey string) (*contextLocked, error) {
	share := ""
	if s.dialect.Driver() == DriverPostgres {
		share = " FOR SHARE"
	}
	var c contextLocked
	var manifest sql.NullString
	err := tx.QueryRow(s.q(`SELECT state, workspace_id, auth_epoch, manifest FROM app_installs WHERE id = ?`+share), installID).
		Scan(&c.state, &c.workspaceID, &c.epoch, &manifest)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrContextCodeRefused
	}
	if err != nil {
		return nil, fmt.Errorf("context code: install: %w", err)
	}
	c.manifest = manifest.String
	var id string
	err = tx.QueryRow(s.q(`SELECT id FROM items WHERE id = ? AND deleted_at IS NULL`+share), itemID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrContextCodeRefused
	}
	if err != nil {
		return nil, fmt.Errorf("context code: item: %w", err)
	}
	if c.item, err = s.getItemTx(tx, itemID); err != nil || c.item == nil {
		if err != nil {
			return nil, err
		}
		return nil, ErrContextCodeRefused
	}
	var colls string
	err = tx.QueryRow(s.q(`SELECT id, action_key, label, path, collection_ids, revision, active FROM app_item_actions WHERE install_id = ? AND action_key = ?`+share),
		installID, actionKey).Scan(&c.action.id, &c.action.key, &c.action.label, &c.action.path, &colls, &c.action.revision, &c.action.active)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrContextCodeRefused
	}
	if err != nil {
		return nil, fmt.Errorf("context code: action: %w", err)
	}
	if err := json.Unmarshal([]byte(colls), &c.action.collections); err != nil {
		return nil, fmt.Errorf("context code: action collections: %w", err)
	}
	return &c, nil
}

// contextAdmissibleQ is the shared check, under the locks, for mint and
// redeem: install active in the item's workspace, item in one of the
// install's CURRENT companion collections that the action is offered on,
// action active, viewer can see the item.
func (s *Store) contextAdmissibleQ(q Queryer, c *contextLocked, installID, viewerID string, visible ContextVisibleFunc) error {
	if c.state != InstallActive || c.item.WorkspaceID != c.workspaceID {
		return ErrContextCodeRefused
	}
	if !c.action.active || !slices.Contains(c.action.collections, c.item.CollectionID) {
		return ErrContextCodeRefused
	}
	var n int
	if err := q.QueryRow(s.q(`SELECT COUNT(*) FROM collections WHERE id = ? AND via_app = ? AND deleted_at IS NULL`),
		c.item.CollectionID, installID).Scan(&n); err != nil {
		return fmt.Errorf("context code: companion: %w", err)
	}
	if n == 0 {
		return ErrContextCodeRefused
	}
	var live int
	if err := q.QueryRow(s.q(`SELECT COUNT(*) FROM workspaces WHERE id = ? AND deleted_at IS NULL`), c.workspaceID).Scan(&live); err != nil {
		return fmt.Errorf("context code: workspace: %w", err)
	}
	if live == 0 {
		return ErrContextCodeRefused
	}
	ok, err := visible(q, c.item, viewerID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrContextCodeRefused
	}
	return nil
}

// MintedContext is a minted code and where to send it.
type MintedContext struct {
	Code    string
	BaseURL string
	Path    string
}

// MintContextCode mints a code for viewerID's click on actionKey of
// installID for itemID in workspaceID, after the same checks redeem makes,
// under the same locks (lead ruling, day 88).
func (s *Store) MintContextCode(workspaceID, installID, actionKey, itemID, viewerID string, visible ContextVisibleFunc) (*MintedContext, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	c, err := s.contextLocksTx(tx, installID, itemID, actionKey)
	if err != nil {
		return nil, err
	}
	if c.workspaceID != workspaceID {
		return nil, ErrContextCodeRefused
	}
	if err := s.contextAdmissibleQ(tx, c, installID, viewerID, visible); err != nil {
		return nil, err
	}
	// The validated origin, not the raw field: validation trims base_url
	// while it derives the origin, but the stored manifest keeps the
	// original string (codex r1 on U11). Refused before a code is written.
	base, err := appmanifest.NormalizeOrigin(manifestField(c.manifest, "base_url"))
	if err != nil {
		return nil, ErrContextCodeRefused
	}
	raw := make([]byte, 16) // 128 bits (§6)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("context code: %w", err)
	}
	code := "padctx_" + hex.EncodeToString(raw)
	expires := fmt.Sprintf(`strftime('%%Y-%%m-%%dT%%H:%%M:%%fZ', 'now', '+%d seconds')`, contextCodeTTLSeconds)
	if s.dialect.Driver() == DriverPostgres {
		expires = fmt.Sprintf(`now() + interval '%d seconds'`, contextCodeTTLSeconds)
	}
	if _, err := tx.Exec(s.q(`INSERT INTO app_context_codes (code_sha256, install_id, auth_epoch, item_id, action_id, action_revision, viewer_user_id, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, `+expires+`)`),
		contextCodeHash(code), installID, c.epoch, itemID, c.action.id, c.action.revision, viewerID); err != nil {
		return nil, fmt.Errorf("context code: insert: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &MintedContext{Code: code, BaseURL: base, Path: c.action.path}, nil
}

func contextCodeHash(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

// RedeemedContext is what a redeem hands the app: the action and the item,
// read under the locks. Never the viewer (§6: the code names an item).
type RedeemedContext struct {
	ActionKey string
	Item      *models.Item
}

// RedeemContextCode consumes code for installID, whose token carries epoch,
// and returns the item it names if, under the §6 locks, everything the code
// was bound to still holds. The code is consumed FIRST and stays consumed
// whatever the outcome (a mismatch burns it).
func (s *Store) RedeemContextCode(installID string, epoch int64, code string, visible ContextVisibleFunc) (*RedeemedContext, error) {
	hash := contextCodeHash(code)
	// What the code names, read first to know which rows to lock. The
	// consume below decides whether it is still live.
	var codeInstall, itemID, actionID, viewerID string
	var codeEpoch int64
	var revision int
	err := s.db.QueryRow(s.q(`SELECT install_id, auth_epoch, item_id, action_id, action_revision, viewer_user_id FROM app_context_codes WHERE code_sha256 = ?`), hash).
		Scan(&codeInstall, &codeEpoch, &itemID, &actionID, &revision, &viewerID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrContextCodeRefused
	}
	if err != nil {
		return nil, fmt.Errorf("redeem context: %w", err)
	}
	var actionKey string
	if err := s.db.QueryRow(s.q(`SELECT action_key FROM app_item_actions WHERE id = ?`), actionID).Scan(&actionKey); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// The action row is gone (uninstall): burn the code and refuse.
			_ = s.burnContextCode(hash)
			return nil, ErrContextCodeRefused
		}
		return nil, fmt.Errorf("redeem context: %w", err)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	c, lockErr := s.contextLocksTx(tx, codeInstall, itemID, actionKey)
	if lockErr != nil && !errors.Is(lockErr, ErrContextCodeRefused) {
		return nil, lockErr
	}
	// Consume under the locks, whatever follows. Only an unconsumed,
	// unexpired code is consumed here; a second redeem finds nothing to
	// consume and is refused.
	live := `expires_at > strftime('%Y-%m-%dT%H:%M:%fZ', 'now')`
	if s.dialect.Driver() == DriverPostgres {
		live = `expires_at > now()`
	}
	res, err := tx.Exec(s.q(`UPDATE app_context_codes SET consumed_at = ? WHERE code_sha256 = ? AND consumed_at IS NULL AND `+live), now(), hash)
	if err != nil {
		return nil, fmt.Errorf("redeem context: consume: %w", err)
	}
	consumed, _ := res.RowsAffected()
	refuse := func() (*RedeemedContext, error) {
		if err := tx.Commit(); err != nil { // the consume stands
			return nil, err
		}
		return nil, ErrContextCodeRefused
	}
	if consumed == 0 {
		// Already consumed or expired: burn it if it was merely expired.
		if _, err := tx.Exec(s.q(`UPDATE app_context_codes SET consumed_at = ? WHERE code_sha256 = ? AND consumed_at IS NULL`), now(), hash); err != nil {
			return nil, fmt.Errorf("redeem context: burn: %w", err)
		}
		return refuse()
	}
	if lockErr != nil {
		return refuse()
	}
	if redeemContextHook != nil {
		redeemContextHook()
	}
	// Bound to THIS install and epoch; the token's epoch is the install's.
	if codeInstall != installID || c.epoch != codeEpoch || epoch != codeEpoch {
		return refuse()
	}
	if c.action.id != actionID || c.action.revision != revision {
		return refuse()
	}
	if err := s.contextAdmissibleQ(tx, c, installID, viewerID, visible); err != nil {
		if errors.Is(err, ErrContextCodeRefused) {
			return refuse()
		}
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &RedeemedContext{ActionKey: actionKey, Item: c.item}, nil
}

// redeemContextHook runs inside RedeemContextCode's transaction, with
// every lock held and the code consumed; tests start a competing mutation
// there. nil in production.
var redeemContextHook func()

// BurnContextCode consumes installID's code without redeeming it (an attempt
// refused before the redeem, a delegated token's). Another install's code is
// left alone.
func (s *Store) BurnContextCode(installID, code string) error {
	_, err := s.db.Exec(s.q(`UPDATE app_context_codes SET consumed_at = ? WHERE code_sha256 = ? AND install_id = ? AND consumed_at IS NULL`),
		now(), contextCodeHash(code), installID)
	if err != nil {
		return fmt.Errorf("burn context code: %w", err)
	}
	return nil
}

// burnContextCode marks a code consumed outside a redeem transaction.
func (s *Store) burnContextCode(hash string) error {
	_, err := s.db.Exec(s.q(`UPDATE app_context_codes SET consumed_at = ? WHERE code_sha256 = ? AND consumed_at IS NULL`), now(), hash)
	return err
}

// PruneContextCodes deletes codes that are consumed or expired, in database
// time. A code is single-use and lives two minutes; nothing reads one after.
func (s *Store) PruneContextCodes() (int64, error) {
	q := `DELETE FROM app_context_codes WHERE consumed_at IS NOT NULL OR expires_at <= strftime('%Y-%m-%dT%H:%M:%fZ', 'now')`
	if s.dialect.Driver() == DriverPostgres {
		q = `DELETE FROM app_context_codes WHERE consumed_at IS NOT NULL OR expires_at <= now()`
	}
	res, err := s.db.Exec(q)
	if err != nil {
		return 0, fmt.Errorf("prune context codes: %w", err)
	}
	return res.RowsAffected()
}

// AppActionSpecFunc derives the action specs from a stored manifest (the
// server's appmanifest translation).
type AppActionSpecFunc func(manifestJSON string) ([]AppActionSpec, error)

// ListInstallsWithoutActions returns active and inactive installs with a
// stored manifest and no action rows: those provisioned before U11. A work
// list only; EnsureAppItemActions re-decides under the install lock.
func (s *Store) ListInstallsWithoutActions() ([]InstallNeedingWebhook, error) {
	rows, err := s.db.Query(s.q(`SELECT i.id, i.workspace_id FROM app_installs i
		WHERE i.state IN ('active', 'inactive') AND i.manifest IS NOT NULL AND i.manifest != ''
		AND NOT EXISTS (SELECT 1 FROM app_item_actions a WHERE a.install_id = i.id)
		ORDER BY i.id`))
	if err != nil {
		return nil, fmt.Errorf("list installs without actions: %w", err)
	}
	defer rows.Close()
	var out []InstallNeedingWebhook
	for rows.Next() {
		var r InstallNeedingWebhook
		if err := rows.Scan(&r.InstallID, &r.WorkspaceID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// EnsureAppItemActions writes the action rows an already-installed manifest
// declares (the backfill). Under the install row lock it re-checks the
// state, that there are still no rows, and re-reads the manifest, so a list
// that went stale (an upgrade, another instance's backfill) changes nothing.
func (s *Store) EnsureAppItemActions(workspaceID, installID string, specOf AppActionSpecFunc) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	state, err := s.lockInstallTx(tx, workspaceID, installID)
	if err != nil {
		return err
	}
	if state != InstallActive && state != InstallInactive {
		return nil
	}
	var n int
	if err := tx.QueryRow(s.q(`SELECT COUNT(*) FROM app_item_actions WHERE install_id = ?`), installID).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	var manifest sql.NullString
	if err := tx.QueryRow(s.q(`SELECT manifest FROM app_installs WHERE id = ?`), installID).Scan(&manifest); err != nil {
		return err
	}
	specs, err := specOf(manifest.String)
	if err != nil {
		return err
	}
	if len(specs) == 0 {
		return nil
	}
	if err := s.syncAppItemActionsTx(tx, workspaceID, installID, specs); err != nil {
		return err
	}
	return tx.Commit()
}

// ViewerRoleQ is the role a bearer caller would be admitted with to
// workspaceID (the server's crossWorkspaceRole with isBearer set), read on q
// so the context-code transactions do not take a second connection (codex r1
// on U11): the member's role; nothing for a platform admin who is not a
// member (no admin bypass for an app's viewer); "guest" for a non-member
// holding a grant; otherwise "".
func (s *Store) ViewerRoleQ(q Queryer, workspaceID string, user *models.User) (string, error) {
	member, err := s.GetWorkspaceMemberQ(q, workspaceID, user.ID)
	if err != nil {
		return "", err
	}
	if member != nil {
		return member.Role, nil
	}
	if user.Role == "admin" {
		return "", nil
	}
	has, err := s.UserHasGrantsInWorkspaceQ(q, workspaceID, user.ID)
	if err != nil {
		return "", err
	}
	if has {
		return "guest", nil
	}
	return "", nil
}
