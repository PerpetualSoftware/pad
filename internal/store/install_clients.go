package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Install clients and the issuance barrier (SPEC-6 U5a, TASK-3394,
// DOC-3371 §4 Credentials).
//
// Every installed app has exactly one confidential OAuth client. It
// authenticates with a secret (only its bcrypt hash is stored, which is what
// fosite's default hasher compares), it may only ever hold the app API
// audience, and every credential it is issued is persisted through
// installIssuanceBarrierTx: under the install row's lock, after re-checking
// the install, with a binding row that records the install's epoch.
//
// A bot principal (users.kind = 'app') holds a credential through this door
// and no other (lead ruling R1): a service token of its OWN install's client.
// Every other insert still passes requireActiveUserTx, which refuses a bot.
//
// The lifecycle primitives take the caller's transaction (U8's provisioning,
// disable, rotate and uninstall each stay one transaction), are keyed by the
// install, and never open a transaction or write through the pool.

var (
	// ErrInstallNotActive refuses a credential for an install that is not
	// active, or does not exist.
	ErrInstallNotActive = errors.New("app install is not active")
	// ErrInstallClientDisabled refuses a credential for a disabled client.
	ErrInstallClientDisabled = errors.New("app install client is disabled")
	// ErrInstallTokenSubject refuses an install-client credential whose
	// subject is not that install's own bot.
	ErrInstallTokenSubject = errors.New("an install client's service token belongs to its own app principal only")
	// ErrInstallDelegatedUnsupported refuses delegated (code + PKCE) install
	// grants until TASK-3399 (U5b) brings them through the barrier.
	ErrInstallDelegatedUnsupported = errors.New("delegated install grants are not supported yet")
	// ErrNoInstallClient means the install has no client.
	ErrNoInstallClient = errors.New("app install has no client")
)

// InstallClientScope is the one scope an install client holds and is granted.
// Access is not a scope: it comes from the install's manifest per token kind
// (§4 step 4), which the app API middleware reads.
const InstallClientScope = "app"

// SetAppAPIAudience records the app API resource (the only audience an
// install client may hold). The server sets it at startup from its origin;
// with none set, no install client can be created.
func (s *Store) SetAppAPIAudience(aud string) { s.appAPIAudience = strings.TrimSpace(aud) }

// AppAPIAudience returns the configured app API resource, or "".
func (s *Store) AppAPIAudience() string { return s.appAPIAudience }

// appPrincipalEmail is the address CreateAppUserTx gives an install's bot.
func appPrincipalEmail(installID string) string {
	return "app+" + strings.ToLower(strings.TrimSpace(installID)) + "@" + models.AppPrincipalEmailDomain
}

func newInstallClientSecret() (string, string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("install client secret: %w", err)
	}
	secret := "padapp_" + hex.EncodeToString(raw)
	hash, err := bcrypt.GenerateFromPassword([]byte(secret), bcryptCost)
	if err != nil {
		return "", "", fmt.Errorf("install client secret: hash: %w", err)
	}
	return secret, string(hash), nil
}

// CreateInstallClientTx creates the install's one confidential client and
// returns its id and secret. The secret is returned here only; its hash is
// stored. The client holds exactly the app API audience and the "app" scope,
// and may use client_credentials, authorization_code and refresh_token.
func (s *Store) CreateInstallClientTx(tx *sql.Tx, installID string, redirectURIs []string) (string, string, error) {
	if s.appAPIAudience == "" {
		return "", "", errors.New("create install client: the app API audience is not configured")
	}
	var origin, state string
	err := tx.QueryRow(s.q(`SELECT origin, state FROM app_installs WHERE id = ?`), installID).Scan(&origin, &state)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && state == "uninstalled") {
		return "", "", ErrInstallNotActive
	}
	if err != nil {
		return "", "", fmt.Errorf("create install client: read install: %w", err)
	}
	if redirectURIs == nil {
		redirectURIs = []string{}
	}
	redirects, _ := json.Marshal(redirectURIs)
	grants, _ := json.Marshal([]string{"client_credentials", "authorization_code", "refresh_token"})
	responses, _ := json.Marshal([]string{"code"})
	scopes, _ := json.Marshal([]string{InstallClientScope})
	audiences, _ := json.Marshal([]string{s.appAPIAudience})
	secret, hash, err := newInstallClientSecret()
	if err != nil {
		return "", "", err
	}
	id := newID()
	if _, err := tx.Exec(s.q(`
		INSERT INTO oauth_clients (id, name, redirect_uris, grant_types, response_types,
		    token_endpoint_auth_method, scopes, public, created_at,
		    client_secret_hash, allowed_audiences, app_install_id)
		VALUES (?, ?, ?, ?, ?, 'client_secret_basic', ?, ?, ?, ?, ?, ?)`),
		id, origin, string(redirects), string(grants), string(responses), string(scopes), false, now(),
		hash, string(audiences), installID); err != nil {
		return "", "", fmt.Errorf("create install client: %w", err)
	}
	return id, secret, nil
}

// InstallClientIDTx returns the install's client id.
func (s *Store) InstallClientIDTx(tx *sql.Tx, installID string) (string, error) {
	var id string
	err := tx.QueryRow(s.q(`SELECT id FROM oauth_clients WHERE app_install_id = ?`), installID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoInstallClient
	}
	if err != nil {
		return "", fmt.Errorf("read install client: %w", err)
	}
	return id, nil
}

// RotateInstallClientSecretTx replaces the client's secret and returns the
// new one. Tokens already issued are the caller's business: rotate bumps the
// install epoch and calls RevokeInstallClientGrantsTx.
func (s *Store) RotateInstallClientSecretTx(tx *sql.Tx, installID string) (string, error) {
	clientID, err := s.InstallClientIDTx(tx, installID)
	if err != nil {
		return "", err
	}
	secret, hash, err := newInstallClientSecret()
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(s.q(`UPDATE oauth_clients SET client_secret_hash = ? WHERE id = ?`), hash, clientID); err != nil {
		return "", fmt.Errorf("rotate install client secret: %w", err)
	}
	return secret, nil
}

// installClientGrantTables are the client's grant tables, children first.
var installClientGrantTables = []string{"oauth_pkce_requests", "oauth_authorization_codes", "oauth_refresh_tokens", "oauth_access_tokens"}

// RevokeInstallClientGrantsTx deletes every code, PKCE row, access and
// refresh token the install's client holds, with their bindings. The caller
// holds the install row FOR UPDATE and bumps the epoch; this leaves the
// epoch and the delegated users' oauth_connections alone (they sign in
// again after a rotate).
func (s *Store) RevokeInstallClientGrantsTx(tx *sql.Tx, installID string) error {
	clientID, err := s.InstallClientIDTx(tx, installID)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(s.q(`DELETE FROM app_token_bindings WHERE client_id = ?`), clientID); err != nil {
		return fmt.Errorf("revoke install grants: bindings: %w", err)
	}
	for _, table := range installClientGrantTables {
		if _, err := tx.Exec(s.q(`DELETE FROM `+table+` WHERE client_id = ?`), clientID); err != nil {
			return fmt.Errorf("revoke install grants: %s: %w", table, err)
		}
	}
	return nil
}

// DeleteInstallClientTx removes the install's client and everything hanging
// off it, children first (DOC-3371 §8): the request ids are captured from the
// grant tables BEFORE anything is deleted and their oauth_connections removed
// (their workspace rows cascade), then the bindings, the grants, the client.
func (s *Store) DeleteInstallClientTx(tx *sql.Tx, installID string) error {
	clientID, err := s.InstallClientIDTx(tx, installID)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(s.q(`
		DELETE FROM oauth_connections WHERE request_id IN (
			SELECT request_id FROM oauth_authorization_codes WHERE client_id = ?
			UNION SELECT request_id FROM oauth_access_tokens WHERE client_id = ?
			UNION SELECT request_id FROM oauth_refresh_tokens WHERE client_id = ?
			UNION SELECT request_id FROM oauth_pkce_requests WHERE client_id = ?
		)`), clientID, clientID, clientID, clientID); err != nil {
		return fmt.Errorf("delete install client: connections: %w", err)
	}
	if err := s.RevokeInstallClientGrantsTx(tx, installID); err != nil {
		return err
	}
	if _, err := tx.Exec(s.q(`DELETE FROM oauth_clients WHERE id = ?`), clientID); err != nil {
		return fmt.Errorf("delete install client: %w", err)
	}
	return nil
}

// installIssuanceBarrierTx is the issuance barrier (DOC-3371 §4). For a
// request whose client is an install client it does the whole persistence in
// the caller's transaction and reports handled; for any other client it does
// nothing and the ordinary path (requireActiveUserTx) runs.
//
// In order, under one transaction:
//   - lock the install row (FOR UPDATE on Postgres; SQLite's single writer
//     serializes it), the order disable and rotate take it in, before any
//     users row;
//   - refuse unless the install is active and the client not disabled;
//   - refuse unless the subject is the install's own bot (a service token):
//     the ONE door through which a bot holds a credential (lead ruling R1).
//     Delegated grants are refused until TASK-3399;
//   - insert the token row and its binding, carrying the epoch read under
//     the lock. Introspection refuses a binding whose epoch is not current,
//     so a disable or rotate that bumps the epoch ends this token even when
//     it raced the issuance.
func (s *Store) installIssuanceBarrierTx(tx *sql.Tx, table string, req models.OAuthRequest, requestedStr string) (bool, error) {
	var installID, disabledAt sql.NullString
	err := tx.QueryRow(s.q(`SELECT app_install_id, disabled_at FROM oauth_clients WHERE id = ?`), req.ClientID).Scan(&installID, &disabledAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil // the insert's own FK refuses an unknown client
	}
	if err != nil {
		return false, fmt.Errorf("oauth: read client: %w", err)
	}
	if !installID.Valid || installID.String == "" {
		return false, nil
	}
	lockQ := `SELECT workspace_id, state, auth_epoch FROM app_installs WHERE id = ?`
	if s.dialect.Driver() == DriverPostgres {
		lockQ += ` FOR UPDATE`
	}
	var workspaceID, state string
	var epoch int64
	err = tx.QueryRow(s.q(lockQ), installID.String).Scan(&workspaceID, &state, &epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return true, ErrInstallNotActive
	}
	if err != nil {
		return true, fmt.Errorf("oauth: lock install: %w", err)
	}
	if state != "active" {
		return true, ErrInstallNotActive
	}
	if disabledAt.Valid && disabledAt.String != "" {
		return true, ErrInstallClientDisabled
	}
	if table != "oauth_access_tokens" && table != "oauth_refresh_tokens" {
		return true, ErrInstallDelegatedUnsupported
	}
	var botKind string
	var botDisabled sql.NullString
	err = tx.QueryRow(s.q(`SELECT kind, disabled_at FROM users WHERE id = ? AND email = ?`),
		req.Subject, appPrincipalEmail(installID.String)).Scan(&botKind, &botDisabled)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (botKind != models.UserKindApp || botDisabled.Valid)) {
		return true, ErrInstallTokenSubject
	}
	if err != nil {
		return true, fmt.Errorf("oauth: read install principal: %w", err)
	}
	if err := s.insertOAuthRequestRowTx(tx, table, req, requestedStr); err != nil {
		return true, err
	}
	var bound int64
	err = tx.QueryRow(s.q(`SELECT auth_epoch FROM app_token_bindings WHERE request_id = ?`), req.RequestID).Scan(&bound)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.Exec(s.q(`
			INSERT INTO app_token_bindings (request_id, client_id, install_id, workspace_id, auth_epoch, auth_kind, created_at)
			VALUES (?, ?, ?, ?, ?, 'service', ?)`),
			req.RequestID, req.ClientID, installID.String, workspaceID, epoch, now()); err != nil {
			return true, fmt.Errorf("oauth: write token binding: %w", err)
		}
	case err != nil:
		return true, fmt.Errorf("oauth: read token binding: %w", err)
	case bound != epoch:
		// A later persistence of a family bound under an earlier epoch.
		return true, ErrInstallNotActive
	}
	return true, nil
}

// AppPrincipalForInstall returns the install's bot: the kind='app' user at
// the address CreateAppUserTx gave it, or nil when there is none.
func (s *Store) AppPrincipalForInstall(installID string) (*models.User, error) {
	u, err := s.GetUserByEmail(appPrincipalEmail(installID))
	if err != nil || u == nil || !u.IsApp() {
		return nil, err
	}
	return u, nil
}

// AppTokenBinding is the binding the issuance barrier wrote for a token
// family.
type AppTokenBinding struct {
	RequestID   string
	ClientID    string
	InstallID   string
	WorkspaceID string
	AuthEpoch   int64
	AuthKind    string
}

// AppTokenState is what introspection needs, read in one statement: the
// family's binding, its install's current state and epoch, and whether its
// client is disabled.
type AppTokenState struct {
	Binding        AppTokenBinding
	InstallState   string
	InstallEpoch   int64
	ClientDisabled bool
}

// GetAppTokenState reads a token family's binding with its install and
// client, or nil when the family has no binding.
func (s *Store) GetAppTokenState(requestID string) (*AppTokenState, error) {
	var st AppTokenState
	var disabledAt sql.NullString
	err := s.db.QueryRow(s.q(`
		SELECT b.request_id, b.client_id, b.install_id, b.workspace_id, b.auth_epoch, b.auth_kind,
		       i.state, i.auth_epoch, c.disabled_at
		FROM app_token_bindings b
		JOIN app_installs i ON i.id = b.install_id
		JOIN oauth_clients c ON c.id = b.client_id
		WHERE b.request_id = ?`), requestID).Scan(
		&st.Binding.RequestID, &st.Binding.ClientID, &st.Binding.InstallID, &st.Binding.WorkspaceID,
		&st.Binding.AuthEpoch, &st.Binding.AuthKind, &st.InstallState, &st.InstallEpoch, &disabledAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read app token state: %w", err)
	}
	st.ClientDisabled = disabledAt.Valid && disabledAt.String != ""
	return &st, nil
}
