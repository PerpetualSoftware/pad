package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"
)

// App install lifecycle: two-phase disable and rotate, re-enable, and
// uninstall (SPEC-6 U8c, DOC-3371 §2 and §8; TASK-3397).
//
// Phase 1 (BeginInstallTeardown) holds the install row FOR UPDATE, moves the
// install to its in-between state, bumps auth_epoch and revokes every grant
// the install's client holds. App mutations (FencedTx) and the issuance
// barrier take the same row with a shared lock, so nothing admitted before
// phase 1 is still deciding when it commits, and nothing is admitted after
// it. Then the delivery drain, then phase 2 in its own transaction, which
// re-takes the row and requires the phase-1 state: a crash between the two
// leaves the install in that state, and repeating the owner's call resumes.
//
// Lock order: phase 1 and phase 2 of disable and rotate take the install row
// first, then the client's grant tables and the code rows; the barrier
// (install row FOR SHARE, then token inserts), FencedTx (install row FOR SHARE
// first) and redeem (install row, then the code row) agree. UninstallAppTx
// alone takes the bot's membership and users rows BEFORE the install row, the
// order account deletion's bot purge takes them (see UninstallAppTx).

// Install states (the CHECK in migration 112).
const (
	InstallActive       = "active"
	InstallDisabling    = "disabling"
	InstallInactive     = "inactive"
	InstallUninstalling = "uninstalling"
	InstallUninstalled  = "uninstalled"
)

// InstallStateError: the install is not in a state the operation accepts.
type InstallStateError struct{ State string }

func (e *InstallStateError) Error() string { return "install is " + e.State }

// ErrInstallNotFound: no such install in that workspace.
var ErrInstallNotFound = errors.New("install not found")

// TeardownKind names the operation phase 1 starts.
type TeardownKind int

const (
	TeardownDisable TeardownKind = iota
	TeardownRotate
	TeardownUninstall
)

func (k TeardownKind) phase1State() string {
	if k == TeardownUninstall {
		return InstallUninstalling
	}
	return InstallDisabling
}

func (k TeardownKind) allowedFrom(state string) bool {
	switch k {
	case TeardownUninstall:
		return state == InstallActive || state == InstallInactive || state == InstallDisabling
	default:
		return state == InstallActive
	}
}

// lockInstallTx reads an install of workspaceID FOR UPDATE (Postgres; SQLite
// is BEGIN IMMEDIATE) and returns its state.
func (s *Store) lockInstallTx(tx *sql.Tx, workspaceID, installID string) (string, error) {
	q := `SELECT state FROM app_installs WHERE id = ? AND workspace_id = ?`
	if s.dialect.Driver() == DriverPostgres {
		q += ` FOR UPDATE`
	}
	var state string
	err := tx.QueryRow(s.q(q), installID, workspaceID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrInstallNotFound
	}
	if err != nil {
		return "", fmt.Errorf("lock install: %w", err)
	}
	return state, nil
}

func (s *Store) setInstallStateTx(tx *sql.Tx, installID, state string, bumpEpoch bool) error {
	q := `UPDATE app_installs SET state = ?, updated_at = ? WHERE id = ?`
	if bumpEpoch {
		q = `UPDATE app_installs SET state = ?, updated_at = ?, auth_epoch = auth_epoch + 1 WHERE id = ?`
	}
	if _, err := tx.Exec(s.q(q), state, now(), installID); err != nil {
		return fmt.Errorf("set install state: %w", err)
	}
	return nil
}

// BeginInstallTeardown is phase 1. It is a no-op when the install is already
// in the phase-1 state for this kind (a resumed call): the epoch was bumped
// and the grants revoked by the call that got it there.
func (s *Store) BeginInstallTeardown(workspaceID, installID string, kind TeardownKind) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	state, err := s.lockInstallTx(tx, workspaceID, installID)
	if err != nil {
		return err
	}
	target := kind.phase1State()
	if state == target {
		return nil
	}
	if !kind.allowedFrom(state) {
		return &InstallStateError{State: state}
	}
	// The epoch bump under this lock is what ends in-flight grants: each
	// carries the epoch it read before client auth, and the barrier refuses
	// a stale one (the U5a contract).
	if err := s.setInstallStateTx(tx, installID, target, true); err != nil {
		return err
	}
	if err := s.RevokeInstallClientGrantsTx(tx, installID); err != nil && !errors.Is(err, ErrNoInstallClient) {
		return err
	}
	// Every unconsumed install code dies too: redeeming one would mint a
	// fresh working secret, which rotate and disable exist to end (codex r1).
	if _, err := tx.Exec(s.q(`DELETE FROM app_install_codes WHERE install_id = ? AND consumed_at IS NULL`), installID); err != nil {
		return fmt.Errorf("revoke install codes: %w", err)
	}
	return tx.Commit()
}

// DrainInstallDeliveries is the wait between the phases: until the install
// has no unexpired in-flight webhook delivery row, in database time
// (DOC-3371 §5, TASK-3408 U10b). Phase 1 already refuses admission, so the
// set only shrinks. Phase 2 must not run before this returns nil.
//
// It gives up after appDeliveryDrainBound with ErrDrainTimeout: no row can
// outlive its 12 s expiry, so a longer wait means something is wrong with
// time, and the caller must not finish the phase. A cancelled ctx (the
// owner's request went away) stops the wait the same way; a repeated call
// resumes it, as the phases are resumable.
func (s *Store) DrainInstallDeliveries(ctx context.Context, installID string) error {
	deadline := time.NewTimer(appDeliveryDrainBound)
	defer deadline.Stop()
	for {
		n, err := s.liveAppDeliveries(installID)
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		poll := time.NewTimer(appDeliveryDrainPoll)
		select {
		case <-ctx.Done():
			poll.Stop()
			return ctx.Err()
		case <-deadline.C:
			poll.Stop()
			return ErrDrainTimeout
		case <-poll.C:
		}
	}
}

// ErrDrainTimeout: an install still had in-flight deliveries after the
// drain's bound. The phase is not finished; repeating the call resumes it.
var ErrDrainTimeout = errors.New("app deliveries still in flight")

// FinishDisable is disable's phase 2: disabling -> inactive.
//
// The webhook needs nothing here: admission refuses every attempt while the
// install is not active, and the drain between the phases waited out every
// attempt admitted before phase 1 (TASK-3408 U10b). Item actions need nothing
// either: mint and redeem refuse an install that is not active (U11).
func (s *Store) FinishDisable(workspaceID, installID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	state, err := s.lockInstallTx(tx, workspaceID, installID)
	if err != nil {
		return err
	}
	if state == InstallInactive {
		return nil
	}
	if state != InstallDisabling {
		return &InstallStateError{State: state}
	}
	if err := s.setInstallStateTx(tx, installID, InstallInactive, false); err != nil {
		return err
	}
	return tx.Commit()
}

// FinishRotate is rotate's phase 2: disabling -> active with a new client
// secret (discarded: the app receives one through redeem) and a new install
// code, returned once.
func (s *Store) FinishRotate(workspaceID, installID string) (string, time.Time, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", time.Time{}, err
	}
	defer tx.Rollback()
	state, err := s.lockInstallTx(tx, workspaceID, installID)
	if err != nil {
		return "", time.Time{}, err
	}
	if state != InstallDisabling {
		return "", time.Time{}, &InstallStateError{State: state}
	}
	// A second bump: a token request that read the phase-1 epoch while the
	// install was disabling, and authenticated with the OLD secret, would
	// otherwise commit a live token once this makes the install active
	// (codex r1). Its grant carries the phase-1 epoch, which this ends.
	if err := s.setInstallStateTx(tx, installID, InstallActive, true); err != nil {
		return "", time.Time{}, err
	}
	secret, err := s.RotateInstallClientSecretTx(tx, installID)
	if err != nil {
		return "", time.Time{}, err
	}
	discardSecret(&secret)
	// The webhook's secret is replaced and the hook HELD again, like the
	// client secret: the old one may be what leaked, and the app gets the
	// new one only by redeeming this code (TASK-3408 U10b).
	if err := s.holdAppWebhookTx(tx, installID); err != nil {
		return "", time.Time{}, err
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

// ReenableInstall: inactive -> active. The epoch is NEVER moved back (DOC-3371
// §2): every token issued before the disable stays dead, and the app obtains
// fresh ones.
func (s *Store) ReenableInstall(workspaceID, installID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	state, err := s.lockInstallTx(tx, workspaceID, installID)
	if err != nil {
		return err
	}
	if state == InstallActive {
		return nil
	}
	if state != InstallInactive {
		return &InstallStateError{State: state}
	}
	if err := s.setInstallStateTx(tx, installID, InstallActive, false); err != nil {
		return err
	}
	// Events from the disabled period are never delivered after it, even
	// when an owner hook's failure kept them pending (codex r5 on U10b).
	if _, err := tx.Exec(s.q(`UPDATE webhooks SET deliver_from = ?, updated_at = ? WHERE app_install_id = ?`), now(), now(), installID); err != nil {
		return fmt.Errorf("re-enable: webhook: %w", err)
	}
	return tx.Commit()
}

// uninstallHookAfterStep, when set by a test, runs after each UninstallAppTx
// step; a non-nil return aborts the transaction (the atomicity test).
var uninstallHookAfterStep func(step string) error

// UninstallAppTx is the uninstall's phase 2 (DOC-3371 §8 step 3): ONE
// transaction, child-first, uninstalling -> uninstalled.
//
//  1. the install client: its oauth_connections by request_id first, then
//     its bindings and grants, then the client row (U5a's
//     DeleteInstallClientTx);
//  2. the app webhook is deleted (U10a); no delivery is in flight, because
//     admission refuses an uninstalling install and the drain ran first
//     (U10b). Item actions and their context codes are deleted (U11). App
//     attachments are all bound to items and stay (the U7 item-scoped
//     ruling);
//  3. the bot: its membership (member_collection_access cascades), its
//     sessions, API tokens and OAuth credentials, and the bot is DISABLED;
//  4. the install becomes the tombstone, state 'uninstalled'.
//
// The bot's users row is KEPT, disabled, and the install row is never
// deleted: items, comments, versions, links and events keep via_app and the
// bot's user id, and display resolves the app's name from the tombstone.
// This differs on purpose from the account-deletion purge
// (purgeAppPrincipalsOfOwnedWorkspacesTx), which ERASES bots because their
// workspace is itself going away (lead ruling, day 86).
func (s *Store) UninstallAppTx(workspaceID, installID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Lock order (Postgres): the bot's membership row, then its users row,
	// then the install row. Account deletion purging this bot takes them in
	// that order (membership, user, then the install row through the
	// bot_user_id FK action); taking the install row first deadlocked with it
	// (codex r1). The users row is FOR NO KEY UPDATE, which does not conflict
	// with the FK KEY SHARE a fenced app write takes on it while holding the
	// install row FOR SHARE. SQLite: BEGIN IMMEDIATE.
	if s.dialect.Driver() == DriverPostgres {
		var pre sql.NullString
		if err := tx.QueryRow(s.q(`SELECT bot_user_id FROM app_installs WHERE id = ? AND workspace_id = ?`), installID, workspaceID).Scan(&pre); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("uninstall: read bot: %w", err)
		}
		if pre.Valid && pre.String != "" {
			if err := lockRowsForShare(tx, s.q(`SELECT user_id FROM workspace_members WHERE workspace_id = ? AND user_id = ? FOR UPDATE`), workspaceID, pre.String); err != nil {
				return fmt.Errorf("uninstall: lock bot membership: %w", err)
			}
			if err := lockRowsForShare(tx, s.q(`SELECT id FROM users WHERE id = ? FOR NO KEY UPDATE`), pre.String); err != nil {
				return fmt.Errorf("uninstall: lock bot: %w", err)
			}
		}
	}
	if uninstallHookAfterStep != nil {
		if err := uninstallHookAfterStep("bot-locks"); err != nil {
			return err
		}
	}
	state, err := s.lockInstallTx(tx, workspaceID, installID)
	if err != nil {
		return err
	}
	if state == InstallUninstalled {
		return nil
	}
	if state != InstallUninstalling {
		return &InstallStateError{State: state}
	}
	step := func(name string) error {
		if uninstallHookAfterStep != nil {
			return uninstallHookAfterStep(name)
		}
		return nil
	}

	if err := s.DeleteInstallClientTx(tx, installID); err != nil && !errors.Is(err, ErrNoInstallClient) {
		return fmt.Errorf("uninstall: client: %w", err)
	}
	if err := step("client"); err != nil {
		return err
	}

	// The hook: the tombstone install row keeps no way to be called.
	if err := s.deleteAppWebhookTx(tx, installID); err != nil {
		return fmt.Errorf("uninstall: %w", err)
	}
	if err := step("webhook"); err != nil {
		return err
	}
	// Item actions and their unredeemed context codes (U11).
	if err := s.deleteAppItemActionsTx(tx, installID); err != nil {
		return fmt.Errorf("uninstall: %w", err)
	}
	if err := step("actions"); err != nil {
		return err
	}

	var bot sql.NullString
	if err := tx.QueryRow(s.q(`SELECT bot_user_id FROM app_installs WHERE id = ?`), installID).Scan(&bot); err != nil {
		return fmt.Errorf("uninstall: read bot: %w", err)
	}
	if bot.Valid && bot.String != "" {
		if _, err := tx.Exec(s.q(`DELETE FROM workspace_members WHERE workspace_id = ? AND user_id = ?`), workspaceID, bot.String); err != nil {
			return fmt.Errorf("uninstall: bot membership: %w", err)
		}
		if err := step("membership"); err != nil {
			return err
		}
		if err := s.disableUserAndRevokeAccessTx(tx, bot.String); err != nil {
			return fmt.Errorf("uninstall: bot: %w", err)
		}
		if err := step("bot"); err != nil {
			return err
		}
	}

	if err := s.setInstallStateTx(tx, installID, InstallUninstalled, false); err != nil {
		return err
	}
	if err := step("state"); err != nil {
		return err
	}
	return tx.Commit()
}

// InstallState returns an install's state, for the HTTP layer's answers.
func (s *Store) InstallState(workspaceID, installID string) (string, error) {
	var state string
	err := s.db.QueryRow(s.q(`SELECT state FROM app_installs WHERE id = ? AND workspace_id = ?`), installID, workspaceID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrInstallNotFound
	}
	return state, err
}

// WorkspaceInstall is one install as the owner's Apps list shows it (SPEC-6
// U9a, TASK-3413). AppName is the install's bot display name (the app's
// title), else its origin, as the console's app connections name it.
type WorkspaceInstall struct {
	ID        string
	Origin    string
	AppName   string
	Version   string
	State     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ListWorkspaceInstalls returns every install of the workspace, the
// uninstalled tombstones included, newest first.
func (s *Store) ListWorkspaceInstalls(workspaceID string) ([]WorkspaceInstall, error) {
	rows, err := s.db.Query(s.q(`
		SELECT i.id, i.origin, COALESCE(u.name, ''), COALESCE(i.manifest_version, ''), i.state, i.created_at, i.updated_at
		FROM app_installs i
		LEFT JOIN users u ON u.id = i.bot_user_id
		WHERE i.workspace_id = ?
		ORDER BY i.created_at DESC, i.id`), workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list workspace installs: %w", err)
	}
	defer rows.Close()
	out := []WorkspaceInstall{}
	for rows.Next() {
		var in WorkspaceInstall
		var created, updated string
		if err := rows.Scan(&in.ID, &in.Origin, &in.AppName, &in.Version, &in.State, &created, &updated); err != nil {
			return nil, fmt.Errorf("scan workspace install: %w", err)
		}
		if in.AppName == "" {
			in.AppName = in.Origin
		}
		in.CreatedAt, in.UpdatedAt = parseTime(created), parseTime(updated)
		out = append(out, in)
	}
	return out, rows.Err()
}

// InstallArtifactItem is a live item provisioned from an app's companion pack
// (SPEC-6 U9b, TASK-3413): its id and the pack stamp, origin@version.
type InstallArtifactItem struct {
	ItemID     string
	SourcePack string
}

// ListInstallArtifactItems returns the workspace's live items stamped with a
// pack from origin (source_pack = origin@version), oldest first. A prefix is
// compared with substr, not LIKE, so an origin's own characters never act as
// a pattern; its length is in CHARACTERS, which is what substr counts on
// both dialects (an origin's host is not punycoded, so it may be non-ASCII).
//
// The stamp names the app, not one install. The install conflict check
// allows one non-uninstalled install per origin in a workspace, so the only
// other install these rows can come from is an uninstalled one of the same
// app, whose items a reinstall is meant to show (codex U9b r1).
func (s *Store) ListInstallArtifactItems(workspaceID, origin string) ([]InstallArtifactItem, error) {
	prefix := origin + "@"
	rows, err := s.db.Query(s.q(`
		SELECT id, source_pack FROM items
		WHERE workspace_id = ? AND deleted_at IS NULL AND source_pack IS NOT NULL
		  AND substr(source_pack, 1, ?) = ?
		ORDER BY created_at, id`), workspaceID, utf8.RuneCountInString(prefix), prefix)
	if err != nil {
		return nil, fmt.Errorf("list install artifact items: %w", err)
	}
	defer rows.Close()
	out := []InstallArtifactItem{}
	for rows.Next() {
		var a InstallArtifactItem
		if err := rows.Scan(&a.ItemID, &a.SourcePack); err != nil {
			return nil, fmt.Errorf("scan install artifact item: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// InstallOrigin returns an install's origin.
func (s *Store) InstallOrigin(workspaceID, installID string) (string, error) {
	var origin string
	err := s.db.QueryRow(s.q(`SELECT origin FROM app_installs WHERE id = ? AND workspace_id = ?`), installID, workspaceID).Scan(&origin)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrInstallNotFound
	}
	return origin, err
}
