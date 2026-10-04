package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
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
// Lock order, every door: the install row first, then the client's grant
// tables, then the bot's users row. The barrier (install row FOR SHARE, then
// token inserts), FencedTx (install row FOR SHARE first) and redeem (install
// row, then the code row) take them in the same order.

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
	return tx.Commit()
}

// DrainInstallDeliveries is the wait between the phases: until the install
// has no unexpired in-flight webhook delivery row (DOC-3371 §5).
//
// TODO(U10): a no-op until app webhooks exist. U10 adds app_delivery_inflight
// and must make this wait, in database time, until no row for installID is
// unexpired; phase 2 must not run before it returns.
func (s *Store) DrainInstallDeliveries(installID string) error { return nil }

// FinishDisable is disable's phase 2: disabling -> inactive.
//
// TODO(U10/U11): suspend the install's webhook and item actions here.
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
	if err := s.setInstallStateTx(tx, installID, InstallActive, false); err != nil {
		return "", time.Time{}, err
	}
	secret, err := s.RotateInstallClientSecretTx(tx, installID)
	if err != nil {
		return "", time.Time{}, err
	}
	discardSecret(&secret)
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
//  2. item actions, pending deliveries and the app webhook: TODO(U10/U11),
//     none exist yet. App attachments are all bound to items and stay
//     (the U7 item-scoped ruling);
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
