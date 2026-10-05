package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

// App-owned webhooks (SPEC-6 U10a, DOC-3371 §5; TASK-3408).
//
// One webhooks row per install, marked app_install_id. Its URL and events
// come from the install's manifest (consented at install, changed only by a
// reviewed upgrade). Its signing secret is minted here and handed to the app
// ONLY by redeem, which stamps secret_delivered_at; until then the hook is
// HELD and delivers nothing (lead ruling, day 86): an app cannot verify what
// it has no secret for.

// AppWebhookEvent is one declared subscription: an event name and the
// companion collections (by slug) it is limited to.
type AppWebhookEvent struct {
	Name            string
	CollectionSlugs []string
}

// AppWebhookSpec is what an install's manifest declares for its hook.
type AppWebhookSpec struct {
	URL    string
	Events []AppWebhookEvent
}

// appWebhookSubscription is one stored subscription, resolved to collection
// ids so delivery reads no manifest.
type appWebhookSubscription struct {
	Name          string   `json:"name"`
	CollectionIDs []string `json:"collection_ids"`
}

// AppWebhookStatus says what an install's hook is doing, for the owner.
type AppWebhookStatus struct {
	URL string `json:"url"`
	// Status is "awaiting_secret" (held: no redeem has handed the app its
	// signing secret yet) or "active" (deliverable while the install is).
	Status string `json:"status"`
	// Dropped counts deliveries given up undelivered after
	// appWebhookDropAfter (TASK-3408 U10c).
	Dropped int64 `json:"undelivered_dropped"`
}

const (
	AppWebhookAwaitingSecret = "awaiting_secret"
	AppWebhookActive         = "active"
)

func newAppWebhookSecret() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("app webhook secret: %w", err)
	}
	return "padwh_" + hex.EncodeToString(raw), nil
}

// upsertAppWebhookTx makes the install's hook match spec, on the caller's
// transaction (provisioning, upgrade, the backfill). A nil spec, or one with
// no events or no URL, removes the hook. An existing hook keeps its secret
// and its delivered state: a reviewed upgrade changes what it subscribes to,
// not who can verify it.
func (s *Store) upsertAppWebhookTx(tx *sql.Tx, workspaceID, installID string, spec *AppWebhookSpec) error {
	if spec == nil || spec.URL == "" || len(spec.Events) == 0 {
		return s.deleteAppWebhookTx(tx, installID)
	}
	subs := make([]appWebhookSubscription, 0, len(spec.Events))
	for _, e := range spec.Events {
		sub := appWebhookSubscription{Name: e.Name, CollectionIDs: []string{}}
		for _, slug := range e.CollectionSlugs {
			var id string
			err := tx.QueryRow(s.q(`SELECT id FROM collections WHERE workspace_id = ? AND slug = ? AND via_app = ? AND deleted_at IS NULL`),
				workspaceID, slug, installID).Scan(&id)
			if errors.Is(err, sql.ErrNoRows) {
				return &ProvisionConflictError{Collection: slug, Field: "events",
					Detail: fmt.Sprintf("the event %q names a companion collection this install does not hold", e.Name)}
			}
			if err != nil {
				return fmt.Errorf("app webhook: resolve %q: %w", slug, err)
			}
			sub.CollectionIDs = append(sub.CollectionIDs, id)
		}
		subs = append(subs, sub)
	}
	events, err := json.Marshal(subs)
	if err != nil {
		return err
	}
	ts := now()
	// A changed subscription list moves deliver_from: an event that occurred
	// before the owner consented to it is never delivered (codex r5).
	res, err := tx.Exec(s.q(`UPDATE webhooks SET url = ?, deliver_from = CASE WHEN events = ? THEN deliver_from ELSE ? END, events = ?, updated_at = ? WHERE app_install_id = ?`),
		spec.URL, string(events), ts, string(events), ts, installID)
	if err != nil {
		return fmt.Errorf("app webhook: update: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	secret, err := newAppWebhookSecret()
	if err != nil {
		return err
	}
	enc, err := s.encrypt(secret)
	if err != nil {
		return fmt.Errorf("app webhook: encrypt secret: %w", err)
	}
	discardSecret(&secret)
	if _, err := tx.Exec(s.q(`INSERT INTO webhooks (id, workspace_id, url, secret, events, active, created_at, updated_at, failure_count, app_install_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?)`),
		newID(), workspaceID, spec.URL, enc, string(events), s.dialect.BoolToInt(true), ts, ts, installID); err != nil {
		return fmt.Errorf("app webhook: insert: %w", err)
	}
	return nil
}

func (s *Store) deleteAppWebhookTx(tx *sql.Tx, installID string) error {
	if _, err := tx.Exec(s.q(`DELETE FROM webhooks WHERE app_install_id = ?`), installID); err != nil {
		return fmt.Errorf("app webhook: delete: %w", err)
	}
	return nil
}

// rotateAppWebhookSecretTx mints a new signing secret for the install's hook,
// marks it delivered (the caller is handing it to the app now: redeem), and
// returns it. ("", nil) when the install has no hook.
func (s *Store) rotateAppWebhookSecretTx(tx *sql.Tx, installID string) (string, error) {
	var id string
	err := tx.QueryRow(s.q(`SELECT id FROM webhooks WHERE app_install_id = ?`), installID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("app webhook: read: %w", err)
	}
	secret, err := newAppWebhookSecret()
	if err != nil {
		return "", err
	}
	enc, err := s.encrypt(secret)
	if err != nil {
		return "", fmt.Errorf("app webhook: encrypt secret: %w", err)
	}
	ts := now()
	if _, err := tx.Exec(s.q(`UPDATE webhooks SET secret = ?, secret_delivered_at = ?, deliver_from = ?, updated_at = ? WHERE id = ?`), enc, ts, ts, ts, id); err != nil {
		return "", fmt.Errorf("app webhook: rotate secret: %w", err)
	}
	return secret, nil
}

// holdAppWebhookTx replaces the install's hook secret with one nobody holds
// and clears secret_delivered_at, so nothing is delivered until a redeem
// hands the app a new one (rotate). A no-op when the install has no hook.
func (s *Store) holdAppWebhookTx(tx *sql.Tx, installID string) error {
	secret, err := newAppWebhookSecret()
	if err != nil {
		return err
	}
	enc, err := s.encrypt(secret)
	discardSecret(&secret)
	if err != nil {
		return fmt.Errorf("app webhook: encrypt secret: %w", err)
	}
	if _, err := tx.Exec(s.q(`UPDATE webhooks SET secret = ?, secret_delivered_at = NULL, updated_at = ? WHERE app_install_id = ?`), enc, now(), installID); err != nil {
		return fmt.Errorf("app webhook: hold: %w", err)
	}
	return nil
}

// GetAppWebhookStatus reports an install's hook for the owner; nil when the
// install has none.
func (s *Store) GetAppWebhookStatus(installID string) (*AppWebhookStatus, error) {
	var url string
	var delivered sql.NullString
	var dropped int64
	err := s.db.QueryRow(s.q(`SELECT url, secret_delivered_at, dropped_count FROM webhooks WHERE app_install_id = ?`), installID).Scan(&url, &delivered, &dropped)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("app webhook status: %w", err)
	}
	st := &AppWebhookStatus{URL: url, Status: AppWebhookAwaitingSecret, Dropped: dropped}
	if delivered.Valid && delivered.String != "" {
		st.Status = AppWebhookActive
	}
	return st, nil
}

// InstallNeedingWebhook is an install that may need a backfilled hook (the
// backfill's work list; EnsureAppWebhook re-decides under the lock).
type InstallNeedingWebhook struct {
	InstallID   string
	WorkspaceID string
}

// ListInstallsWithoutWebhook returns active and inactive installs that have a
// stored manifest and no hook row: those provisioned before U10 (TASK-3408).
func (s *Store) ListInstallsWithoutWebhook() ([]InstallNeedingWebhook, error) {
	rows, err := s.db.Query(s.q(`SELECT i.id, i.workspace_id FROM app_installs i
		WHERE i.state IN ('active', 'inactive') AND i.manifest IS NOT NULL AND i.manifest != ''
		AND NOT EXISTS (SELECT 1 FROM webhooks w WHERE w.app_install_id = i.id)
		ORDER BY i.id`))
	if err != nil {
		return nil, fmt.Errorf("list installs without webhook: %w", err)
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

// AppWebhookSpecFunc derives the hook spec from a stored manifest (the
// server's appmanifest translation); nil when it declares no hook.
type AppWebhookSpecFunc func(manifestJSON string) (*AppWebhookSpec, error)

// EnsureAppWebhook creates the hook an already-installed manifest declares
// (the backfill). Under the install row lock it re-checks the state, that
// there is still no hook, and re-reads the manifest: the list the caller
// worked from may be stale (another instance's backfill or a reviewed
// upgrade can land in between; codex r1 on U10a), and an existing hook is
// never touched. It creates the hook HELD (awaiting its secret): this app was
// never handed one, and gets it only at its next redeem (lead ruling, day 86).
func (s *Store) EnsureAppWebhook(workspaceID, installID string, specOf AppWebhookSpecFunc) error {
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
	if err := tx.QueryRow(s.q(`SELECT COUNT(*) FROM webhooks WHERE app_install_id = ?`), installID).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	var manifest sql.NullString
	if err := tx.QueryRow(s.q(`SELECT manifest FROM app_installs WHERE id = ?`), installID).Scan(&manifest); err != nil {
		return err
	}
	if !manifest.Valid || manifest.String == "" {
		return nil
	}
	spec, err := specOf(manifest.String)
	if err != nil {
		return err
	}
	if spec == nil {
		return nil
	}
	if err := s.upsertAppWebhookTx(tx, workspaceID, installID, spec); err != nil {
		return err
	}
	return tx.Commit()
}
