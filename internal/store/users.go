package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/bcrypt"
)

var usernameCleanRe = regexp.MustCompile(`[^a-z0-9-]+`)

// bcryptCost is the cost factor passed to bcrypt.GenerateFromPassword.
// It is a var (not const) so tests can lower it via SetBcryptCostForTesting
// — at the production value of 12, bcrypt takes ~3s per call under the
// race detector and the cumulative cost across the test suite exceeds
// the CI -race timeout (see BUG-1371). Production code MUST NOT mutate
// this directly; the only legitimate writer is a test TestMain.
var bcryptCost = 12

// user SELECT columns — used by all user queries.
const userColumns = `id, email, username, name, password_hash, role, avatar_url, totp_secret, totp_enabled, recovery_codes, plan, plan_expires_at, plan_source, stripe_customer_id, plan_overrides, oauth_providers, password_set, disabled_at, email_verified_at, last_active_at, last_write_at, created_at, updated_at, credential_epoch, kind`

// scanUser scans a user row into a User struct.
// Note: does NOT decrypt the TOTP secret — call store.decryptUserTOTP() after
// scanning if you need the plaintext secret for validation.
func scanUser(row interface{ Scan(...interface{}) error }) (*models.User, error) {
	var u models.User
	var createdAt, updatedAt string

	var disabledAt, emailVerifiedAt, lastActiveAt, lastWriteAt sql.NullString
	err := row.Scan(
		&u.ID, &u.Email, &u.Username, &u.Name, &u.PasswordHash, &u.Role, &u.AvatarURL,
		&u.TOTPSecret, &u.TOTPEnabled, &u.RecoveryCodes,
		&u.Plan, &u.PlanExpiresAt, &u.PlanSource, &u.StripeCustomerID, &u.PlanOverrides, &u.OAuthProviders,
		&u.PasswordSet,
		&disabledAt, &emailVerifiedAt, &lastActiveAt, &lastWriteAt, &createdAt, &updatedAt,
		&u.CredentialEpoch, &u.Kind,
	)
	if disabledAt.Valid {
		u.DisabledAt = disabledAt.String
	}
	if emailVerifiedAt.Valid {
		u.EmailVerifiedAt = emailVerifiedAt.String
	}
	if lastActiveAt.Valid {
		u.LastActiveAt = lastActiveAt.String
	}
	if lastWriteAt.Valid {
		u.LastWriteAt = lastWriteAt.String
	}
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	u.CreatedAt = parseTime(createdAt)
	u.UpdatedAt = parseTime(updatedAt)
	return &u, nil
}

// decryptUserTOTP decrypts the TOTP secret on a User struct in place.
func (s *Store) decryptUserTOTP(u *models.User) error {
	if u == nil || u.TOTPSecret == "" {
		return nil
	}
	decrypted, err := s.decrypt(u.TOTPSecret)
	if err != nil {
		return fmt.Errorf("decrypt user TOTP: %w", err)
	}
	u.TOTPSecret = decrypted
	return nil
}

// ErrReservedAppEmail refuses a person's account, invitation or bootstrap at
// an address in the bot principals' reserved domain (TASK-3392).
var ErrReservedAppEmail = errors.New("this email address is reserved")

// CreateUser creates a new user with a hashed password.
func (s *Store) CreateUser(input models.UserCreate) (*models.User, error) {
	if models.IsReservedAppEmail(input.Email) {
		return nil, ErrReservedAppEmail
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(input.Password), bcryptCost)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}

	role := input.Role
	if role == "" {
		role = "member"
	}

	id := newID()
	ts := now()

	// Email-verification default is SAFE = verified (DR-3). Every creation path
	// yields a verified user unless it explicitly requests unverified via
	// UserCreate.Unverified. Today only the future cloud self-serve signup
	// branch (PLAN-1933 Wave 3) sets that; every current call site inherits
	// verified. A nil interface binds as a NULL column (= unverified).
	var emailVerifiedAt interface{}
	if !input.Unverified {
		emailVerifiedAt = ts
	}

	_, err = s.db.Exec(s.q(`
		INSERT INTO users (id, email, username, name, password_hash, role, password_set, email_verified_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`), id, strings.ToLower(strings.TrimSpace(input.Email)), strings.TrimSpace(input.Username), strings.TrimSpace(input.Name), string(hash), role, true, emailVerifiedAt, ts, ts)
	if err != nil {
		return nil, fmt.Errorf("insert user: %w", err)
	}

	return s.GetUser(id)
}

// GetUser retrieves a user by ID.
func (s *Store) GetUser(id string) (*models.User, error) {
	return s.GetUserQ(s.db, id)
}

// GetUserQ is GetUser parameterized over its executor (see Queryer).
func (s *Store) GetUserQ(q Queryer, id string) (*models.User, error) {
	u, err := scanUser(q.QueryRow(s.q(`SELECT `+userColumns+` FROM users WHERE id = ?`), id))
	if err != nil {
		return nil, fmt.Errorf("get user: %w", err)
	}
	if err := s.decryptUserTOTP(u); err != nil {
		return nil, err
	}
	return u, nil
}

// GetUserByEmail retrieves a user by email address (case-insensitive).
func (s *Store) GetUserByEmail(email string) (*models.User, error) {
	u, err := scanUser(s.db.QueryRow(s.q(`SELECT `+userColumns+` FROM users WHERE email = ?`),
		strings.ToLower(strings.TrimSpace(email))))
	if err != nil {
		return nil, fmt.Errorf("get user by email: %w", err)
	}
	if err := s.decryptUserTOTP(u); err != nil {
		return nil, err
	}
	return u, nil
}

// GetUserByUsername retrieves a user by username (case-insensitive).
func (s *Store) GetUserByUsername(username string) (*models.User, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	if username == "" {
		return nil, nil
	}
	u, err := scanUser(s.db.QueryRow(s.q(`SELECT `+userColumns+` FROM users WHERE LOWER(username) = ?`), username))
	if err != nil {
		return nil, fmt.Errorf("get user by username: %w", err)
	}
	if err := s.decryptUserTOTP(u); err != nil {
		return nil, err
	}
	return u, nil
}

// UpdateUser updates mutable user fields.
func (s *Store) UpdateUser(id string, input models.UserUpdate) (*models.User, error) {
	var sets []string
	var args []interface{}

	if input.Name != nil {
		sets = append(sets, "name = ?")
		args = append(args, strings.TrimSpace(*input.Name))
	}
	if input.Username != nil {
		sets = append(sets, "username = ?")
		args = append(args, strings.TrimSpace(*input.Username))
	}
	if input.Password != nil {
		hash, err := bcrypt.GenerateFromPassword([]byte(*input.Password), bcryptCost)
		if err != nil {
			return nil, fmt.Errorf("hash password: %w", err)
		}
		sets = append(sets, "password_hash = ?")
		args = append(args, string(hash))
		// Explicit password change — mark the user as having a usable password
		// (clears the OAuth placeholder-hash state set by CreateOAuthUser).
		sets = append(sets, "password_set = ?")
		args = append(args, true)
		// A new password is a credential change: a sign-in that checked the
		// old one must not mint a session after it (BUG-3382).
		sets = append(sets, "credential_epoch = credential_epoch + 1")
	}
	if input.AvatarURL != nil {
		sets = append(sets, "avatar_url = ?")
		args = append(args, *input.AvatarURL)
	}

	if len(sets) == 0 {
		return s.GetUser(id)
	}

	sets = append(sets, "updated_at = ?")
	args = append(args, now())
	args = append(args, id)

	query := fmt.Sprintf("UPDATE users SET %s WHERE id = ?", strings.Join(sets, ", "))
	fenced := input.Password != nil && input.ExpectedEpoch != nil
	if fenced {
		query += " AND credential_epoch = ?"
		args = append(args, *input.ExpectedEpoch)
	}
	result, err := s.db.Exec(s.q(query), args...)
	if err != nil {
		return nil, fmt.Errorf("update user: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		if fenced {
			if u, err := s.GetUser(id); err == nil && u != nil {
				return nil, ErrCredentialsChanged
			}
		}
		return nil, sql.ErrNoRows
	}

	u, err := s.GetUser(id)
	if err == nil && u != nil && fenced {
		// The epoch THIS write produced, not whatever a later change left:
		// a session minted on it must not survive a change after this one.
		u.CredentialEpoch = *input.ExpectedEpoch + 1
	}
	return u, err
}

// ValidatePassword checks an email/password combination. Returns the user
// if valid, nil if the credentials are wrong (not an error).
func (s *Store) ValidatePassword(email, password string) (*models.User, error) {
	u, err := s.GetUserByEmail(email)
	if err != nil {
		return nil, err
	}
	// A bot is refused before its credential is looked at, with the same
	// answer as a wrong password (TASK-3392).
	if u == nil || u.IsApp() {
		return nil, nil
	}

	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)); err != nil {
		return nil, nil // wrong password — not an error
	}

	// A successful bcrypt compare with a user-supplied plaintext proves the
	// stored hash is usable for real sign-ins (the random 64-byte placeholder
	// set by CreateOAuthUser cannot be guessed). Auto-upgrade password_set so
	// users who pre-date the password_set column — or who linked OAuth after
	// signing up with email/password — don't get trapped in the OAuth-unlink
	// check. Failure here is non-fatal: login succeeds regardless.
	if !u.PasswordSet {
		if _, err := s.db.Exec(s.q(`UPDATE users SET password_set = ? WHERE id = ?`), true, u.ID); err == nil {
			u.PasswordSet = true
		}
	}

	return u, nil
}

// ListUsers returns all users.
func (s *Store) ListUsers() ([]models.User, error) {
	rows, err := s.db.Query(s.q(`SELECT ` + userColumns + ` FROM users WHERE kind = 'human' ORDER BY created_at ASC`))
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	var result []models.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		_ = s.decryptUserTOTP(u) // Best-effort decrypt for list (TOTP secret is json:"-" anyway)
		result = append(result, *u)
	}
	return result, rows.Err()
}

// AdminUserSearchParams holds parameters for the admin user search query.
// Filters apply ANDed; nil pointer fields mean "no filter" (distinguished
// from the zero value so e.g. Disabled=false can mean "show enabled users
// only" rather than "no filter").
type AdminUserSearchParams struct {
	Query  string // Search in email, name, username
	Plan   string // Filter by plan (free, pro, self-hosted)
	Role   string // Filter by role (admin, member)
	Limit  int    // Max results (default 50, max 200)
	Offset int    // Pagination offset

	// Sort key. One of: email, last_write, last_active, storage, workspaces,
	// created. Empty defaults to "created" desc to preserve legacy order.
	Sort string
	// Order direction: "asc" or "desc". Empty defaults to desc.
	Order string

	// ActiveWithinDays filters to users whose last_write_at is within the
	// last N days. nil = no filter. Zero days is invalid (treated as nil).
	ActiveWithinDays *int
	// HasWorkspaces: nil = no filter, true = workspace_count > 0,
	// false = workspace_count == 0.
	HasWorkspaces *bool
	// Disabled: nil = no filter, true = disabled_at IS NOT NULL, false = IS NULL.
	Disabled *bool
}

// AdminUserListEntry is one row returned by SearchUsers: the User model
// plus the cheap aggregations the admin user table renders at a glance.
// PLAN-1542 / TASK-1544.
type AdminUserListEntry struct {
	models.User
	// WorkspaceCount is the number of non-deleted workspaces this user owns.
	WorkspaceCount int `json:"workspace_count"`
	// StorageBytes is the SUM(size_bytes) of all non-deleted attachments
	// across workspaces this user owns. Matches WorkspaceStorageUsage's
	// definition (includes derived blobs like thumbnails).
	StorageBytes int64 `json:"storage_bytes"`
	// Status is a computed bucket for at-a-glance triage. One of:
	// "disabled", "no-workspace", "inactive", "active". Precedence:
	// disabled > no-workspace > inactive (>=30d since last write or never)
	// > active.
	Status string `json:"status"`
}

// AdminUserSearchResult holds the paginated search results.
type AdminUserSearchResult struct {
	Users []AdminUserListEntry `json:"users"`
	Total int                  `json:"total"`
}

// ComputeAdminUserStatusValue is the exported wrapper around
// computeAdminUserStatus. Used by handlers that compute the status pill
// for a single-user response (e.g. after a PATCH refresh).
// PLAN-1542 / TASK-1548.
func ComputeAdminUserStatusValue(disabledAt, lastWriteAt string, workspaceCount int) string {
	return computeAdminUserStatus(disabledAt, lastWriteAt, workspaceCount)
}

// UserStorageUsage returns the SUM(size_bytes) of all non-deleted
// attachments across workspaces owned by this user. Matches the
// per-workspace WorkspaceStorageUsage definition. PLAN-1542 / TASK-1548
// (Codex review on PR #603 — single-user endpoint needs the same
// aggregate as the list endpoint so row-merge keeps the value fresh).
func (s *Store) UserStorageUsage(userID string) (int64, error) {
	var total sql.NullInt64
	err := s.db.QueryRow(s.q(`
		SELECT COALESCE(SUM(a.size_bytes), 0)
		FROM attachments a
		JOIN workspaces w ON w.id = a.workspace_id
		WHERE w.owner_id = ? AND w.deleted_at IS NULL AND a.deleted_at IS NULL
	`), userID).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("user storage usage: %w", err)
	}
	return total.Int64, nil
}

// computeAdminUserStatus derives the at-a-glance status pill from the
// underlying fields. Precedence is intentional: a disabled user with no
// workspace is still "disabled" first, because that's the most actionable
// signal for the admin. Exported for unit testing.
func computeAdminUserStatus(disabledAt, lastWriteAt string, workspaceCount int) string {
	if disabledAt != "" {
		return "disabled"
	}
	if workspaceCount == 0 {
		return "no-workspace"
	}
	if lastWriteAt == "" {
		return "inactive"
	}
	t, err := time.Parse(time.RFC3339, lastWriteAt)
	if err != nil {
		// Malformed timestamp — treat as inactive rather than crashing the
		// list view. The row is still useful; the status pill just isn't
		// claiming "active" for a value we can't parse.
		return "inactive"
	}
	if time.Since(t) > 30*24*time.Hour {
		return "inactive"
	}
	return "active"
}

// SearchUsers returns a filtered, paginated list of users for admin management.
//
// Each row carries the User model plus two cheap aggregations the admin
// table renders at a glance: workspace_count (non-deleted workspaces owned)
// and storage_bytes (SUM of non-deleted attachments across owned workspaces).
// Both are computed via LEFT JOIN against pre-grouped subqueries so the
// aggregation doesn't multiply the user count.
//
// Filters and pagination are pushed into SQL to avoid loading all users
// into memory; sort/filter/pagination is documented on AdminUserSearchParams.
// PLAN-1542 / TASK-1544.
func (s *Store) SearchUsers(params AdminUserSearchParams) (*AdminUserSearchResult, error) {
	if params.Limit <= 0 || params.Limit > 200 {
		params.Limit = 50
	}
	if params.Offset < 0 {
		params.Offset = 0
	}

	// The admin user list is people only (TASK-3392).
	where := []string{"u.kind = 'human'"}
	var args []interface{}

	if params.Query != "" {
		q := "%" + strings.ToLower(params.Query) + "%"
		where = append(where, "(LOWER(u.email) LIKE ? OR LOWER(u.name) LIKE ? OR LOWER(u.username) LIKE ?)")
		args = append(args, q, q, q)
	}
	if params.Plan != "" {
		where = append(where, "u.plan = ?")
		args = append(args, params.Plan)
	}
	if params.Role != "" {
		where = append(where, "u.role = ?")
		args = append(args, params.Role)
	}
	if params.Disabled != nil {
		if *params.Disabled {
			where = append(where, "u.disabled_at IS NOT NULL")
		} else {
			where = append(where, "u.disabled_at IS NULL")
		}
	}
	if params.ActiveWithinDays != nil && *params.ActiveWithinDays > 0 {
		cutoff := time.Now().UTC().Add(-time.Duration(*params.ActiveWithinDays) * 24 * time.Hour).Format(time.RFC3339)
		where = append(where, "u.last_write_at >= ?")
		args = append(args, cutoff)
	}
	if params.HasWorkspaces != nil {
		if *params.HasWorkspaces {
			where = append(where, "COALESCE(wc.cnt, 0) > 0")
		} else {
			where = append(where, "COALESCE(wc.cnt, 0) = 0")
		}
	}

	whereClause := ""
	if len(where) > 0 {
		whereClause = "WHERE " + strings.Join(where, " AND ")
	}

	// LEFT JOIN against grouped subqueries (not bare workspaces/attachments)
	// so the row count stays one-per-user even when a user owns 50 workspaces
	// with thousands of attachments each.
	const workspaceCountJoin = `
		LEFT JOIN (
			SELECT owner_id, COUNT(*) AS cnt
			FROM workspaces
			WHERE deleted_at IS NULL
			GROUP BY owner_id
		) wc ON wc.owner_id = u.id`
	const storageBytesJoin = `
		LEFT JOIN (
			SELECT w.owner_id, COALESCE(SUM(a.size_bytes), 0) AS bytes
			FROM workspaces w
			JOIN attachments a ON a.workspace_id = w.id AND a.deleted_at IS NULL
			WHERE w.deleted_at IS NULL
			GROUP BY w.owner_id
		) sb ON sb.owner_id = u.id`

	// Page query needs both aggregations (the row carries them). The count
	// query only needs the workspace_count join when HasWorkspaces filtering
	// is active — skipping the storage SUM avoids scanning every attachment
	// on every list call (Codex review on PR #599).
	fromClause := "FROM users u" + workspaceCountJoin + storageBytesJoin
	countFromClause := "FROM users u"
	if params.HasWorkspaces != nil {
		countFromClause += workspaceCountJoin
	}

	countQuery := s.q("SELECT COUNT(*) " + countFromClause + " " + whereClause)
	var total int
	if err := s.db.QueryRow(countQuery, args...).Scan(&total); err != nil {
		return nil, fmt.Errorf("search users count: %w", err)
	}

	// Resolve sort. Allow-list to prevent injection; default matches the
	// pre-T1544 behavior (created_at DESC).
	orderBy := adminUserSortClause(params.Sort, params.Order)

	// Build the column list: alias the userColumns onto `u.` so scanUser
	// keeps working. The two aggregation columns come after.
	aliasedUserCols := prefixColumns(userColumns, "u.")
	query := s.q(`SELECT ` + aliasedUserCols + `, COALESCE(wc.cnt, 0), COALESCE(sb.bytes, 0) ` + fromClause + ` ` + whereClause + ` ORDER BY ` + orderBy + ` LIMIT ? OFFSET ?`)
	fullArgs := append(args, params.Limit, params.Offset)
	rows, err := s.db.Query(query, fullArgs...)
	if err != nil {
		return nil, fmt.Errorf("search users: %w", err)
	}
	defer rows.Close()

	entries := make([]AdminUserListEntry, 0)
	for rows.Next() {
		var entry AdminUserListEntry
		var createdAt, updatedAt string
		var disabledAt, emailVerifiedAt, lastActiveAt, lastWriteAt sql.NullString
		var workspaceCount int
		var storageBytes int64
		if err := rows.Scan(
			&entry.ID, &entry.Email, &entry.Username, &entry.Name, &entry.PasswordHash, &entry.Role, &entry.AvatarURL,
			&entry.TOTPSecret, &entry.TOTPEnabled, &entry.RecoveryCodes,
			&entry.Plan, &entry.PlanExpiresAt, &entry.PlanSource, &entry.StripeCustomerID, &entry.PlanOverrides, &entry.OAuthProviders,
			&entry.PasswordSet,
			&disabledAt, &emailVerifiedAt, &lastActiveAt, &lastWriteAt, &createdAt, &updatedAt,
			&entry.CredentialEpoch, &entry.Kind,
			&workspaceCount, &storageBytes,
		); err != nil {
			return nil, fmt.Errorf("search users scan: %w", err)
		}
		if disabledAt.Valid {
			entry.DisabledAt = disabledAt.String
		}
		if emailVerifiedAt.Valid {
			entry.EmailVerifiedAt = emailVerifiedAt.String
		}
		if lastActiveAt.Valid {
			entry.LastActiveAt = lastActiveAt.String
		}
		if lastWriteAt.Valid {
			entry.LastWriteAt = lastWriteAt.String
		}
		entry.CreatedAt = parseTime(createdAt)
		entry.UpdatedAt = parseTime(updatedAt)
		entry.WorkspaceCount = workspaceCount
		entry.StorageBytes = storageBytes
		entry.Status = computeAdminUserStatus(entry.DisabledAt, entry.LastWriteAt, entry.WorkspaceCount)
		_ = s.decryptUserTOTP(&entry.User)
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("search users rows: %w", err)
	}

	return &AdminUserSearchResult{
		Users: entries,
		Total: total,
	}, nil
}

// adminUserSortClause maps the public sort key + order into a safe ORDER BY
// expression. Hard-coded allow-list to avoid SQL injection; an unknown key
// falls back to the legacy default (created_at DESC).
func adminUserSortClause(key, order string) string {
	dir := "DESC"
	if strings.EqualFold(order, "asc") {
		dir = "ASC"
	}
	switch key {
	case "email":
		return "u.email " + dir
	case "last_write":
		// NULL last writes go to the bottom in DESC order, top in ASC. The
		// COALESCE forces nulls to sort opposite-extreme so the page is
		// usable either way.
		if dir == "DESC" {
			return "u.last_write_at DESC NULLS LAST, u.created_at DESC"
		}
		return "u.last_write_at ASC NULLS LAST, u.created_at ASC"
	case "last_active":
		if dir == "DESC" {
			return "u.last_active_at DESC NULLS LAST, u.created_at DESC"
		}
		return "u.last_active_at ASC NULLS LAST, u.created_at ASC"
	case "storage":
		return "COALESCE(sb.bytes, 0) " + dir + ", u.created_at DESC"
	case "workspaces":
		return "COALESCE(wc.cnt, 0) " + dir + ", u.created_at DESC"
	case "created", "":
		return "u.created_at " + dir
	default:
		return "u.created_at DESC"
	}
}

// prefixColumns rewrites a comma-separated SELECT list with the given
// prefix, e.g. "id, email" with "u." → "u.id, u.email". Small helper to
// keep userColumns as a single source of truth even when we need to
// alias the users table in a JOIN.
func prefixColumns(cols, prefix string) string {
	parts := strings.Split(cols, ",")
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = prefix + strings.TrimSpace(p)
	}
	return strings.Join(out, ", ")
}

// UserCount returns the total number of registered users.
func (s *Store) UserCount() (int, error) {
	var count int
	// People only (TASK-3392): an installed app's bot is not a user of the
	// instance. All 19 callers are census rows on TASK-3392.
	err := s.db.QueryRow(s.q("SELECT COUNT(*) FROM users WHERE kind = 'human'")).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count users: %w", err)
	}
	return count, nil
}

// BillingAggregates is the set of users-table aggregates returned by
// CountBillingAggregates: per-plan customer counts and the number of
// new pro signups since a given cutoff. Intentionally narrow — the admin
// billing dashboard is the only consumer today (PLAN-825).
type BillingAggregates struct {
	// CustomersByPlan maps plan slug ("free" / "pro" / "self-hosted") to
	// user count. Users with an empty plan column are bucketed as "free"
	// so the result matches handleAdminStats' presentation.
	CustomersByPlan map[string]int
	// NewProSignups is the count of users with plan='pro' whose
	// created_at is strictly after the supplied cutoff.
	NewProSignups int
}

// CountBillingAggregates returns the per-plan customer counts and the
// count of new "pro" signups since `since`. Implemented as two scalar
// SQL queries so the admin Billing dashboard does not have to materialise
// every users row + decrypt every TOTP secret on each refresh
// (Codex round 1, MEDIUM, PR for TASK-827).
//
// `since` is compared lexicographically against the stored RFC3339
// created_at strings — that matches the rest of the store, where times
// are stored as RFC3339 strings (see store.now / store.parseTime). For
// any caller outside the test suite this is just time.Now().UTC()
// minus the desired window.
func (s *Store) CountBillingAggregates(since time.Time) (*BillingAggregates, error) {
	out := &BillingAggregates{CustomersByPlan: map[string]int{}}

	// GROUP BY must match the projected expression — grouping on the raw
	// `plan` column would split '' and 'free' into two result rows that
	// both scan as "free" in Go and overwrite each other in the map,
	// silently underreporting the free-tier count (Codex round 2).
	//
	// Counted by EFFECTIVE plan (BUG-3356): a plan past its expiry is free.
	// The expiry is decided in Go by the same models.User.EffectivePlan the
	// limit checks use, not compared in SQL, because a stored RFC3339 value
	// may carry an offset and does not sort lexically against now. Rows are
	// grouped by (plan, expiry) first, so this reads one row per distinct
	// expiry, not one per user.
	now := time.Now()
	rows, err := s.db.Query(s.q(`SELECT COALESCE(plan, ''), COALESCE(plan_expires_at, ''), COUNT(*)
		FROM users
		WHERE kind = 'human'
		GROUP BY COALESCE(plan, ''), COALESCE(plan_expires_at, '')`))
	if err != nil {
		return nil, fmt.Errorf("count users by plan: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var u models.User
		var count int
		if err := rows.Scan(&u.Plan, &u.PlanExpiresAt, &count); err != nil {
			return nil, fmt.Errorf("scan users-by-plan row: %w", err)
		}
		out.CustomersByPlan[u.EffectivePlan(now)] += count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate users-by-plan: %w", err)
	}

	cutoff := since.UTC().Format(time.RFC3339)
	proRows, err := s.db.Query(
		s.q(`SELECT COALESCE(plan_expires_at, ''), COUNT(*) FROM users WHERE plan = 'pro' AND kind = 'human' AND created_at > ?
			GROUP BY COALESCE(plan_expires_at, '')`),
		cutoff,
	)
	if err != nil {
		return nil, fmt.Errorf("count new pro signups: %w", err)
	}
	defer proRows.Close()
	for proRows.Next() {
		u := models.User{Plan: "pro"}
		var count int
		if err := proRows.Scan(&u.PlanExpiresAt, &count); err != nil {
			return nil, fmt.Errorf("scan new pro signups: %w", err)
		}
		if u.EffectivePlan(now) == "pro" {
			out.NewProSignups += count
		}
	}
	if err := proRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate new pro signups: %w", err)
	}

	return out, nil
}

// CreateOAuthUser creates a user from an OAuth provider with a random unusable password.
// OAuth users can later set a password via the password reset flow if they want.
func (s *Store) CreateOAuthUser(email, name, avatarURL string) (*models.User, error) {
	// Generate a random 64-byte password the user will never use
	if models.IsReservedAppEmail(email) {
		return nil, ErrReservedAppEmail
	}
	randomPwd := make([]byte, 64)
	if _, err := rand.Read(randomPwd); err != nil {
		return nil, fmt.Errorf("generate random password: %w", err)
	}

	hash, err := bcrypt.GenerateFromPassword(randomPwd, bcryptCost)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}

	id := newID()
	ts := now()

	username := GenerateUsername(name, email)
	username, err = s.EnsureUniqueUsername(username)
	if err != nil {
		return nil, fmt.Errorf("generate username: %w", err)
	}

	// OAuth users are always email-verified (the provider asserted the address);
	// this matches DR-3's "OAuth = verified" and the SAFE default.
	_, err = s.db.Exec(s.q(`
		INSERT INTO users (id, email, username, name, password_hash, role, avatar_url, email_verified_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`), id, strings.ToLower(strings.TrimSpace(email)), username, strings.TrimSpace(name), string(hash), "member", avatarURL, ts, ts, ts)
	if err != nil {
		return nil, fmt.Errorf("insert oauth user: %w", err)
	}

	return s.GetUser(id)
}

// AddOAuthProvider adds a provider to the user's oauth_providers list.
// No-op if the provider is already linked. It takes the account lock, like
// every provider write (TASK-3351).
func (s *Store) AddOAuthProvider(userID, provider string) error {
	if err := s.LinkOAuthProvider(userID, provider, ""); err != nil {
		return fmt.Errorf("add oauth provider: %w", err)
	}
	return nil
}

// RemoveOAuthProvider removes a provider from the user's oauth_providers
// list, and forgets the provider account it was bound to, so a relink may
// bind a different one (TASK-3351). Under the account lock, so a sign-in
// that read the link before this commits cannot re-bind afterwards.
func (s *Store) RemoveOAuthProvider(userID, provider string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("remove oauth provider: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	providers, found, err := s.lockUserProvidersTx(tx, userID)
	if err != nil {
		return fmt.Errorf("remove oauth provider: %w", err)
	}
	if !found {
		return fmt.Errorf("remove oauth provider: user not found")
	}
	var filtered []string
	for _, p := range providers {
		if p != provider {
			filtered = append(filtered, p)
		}
	}
	if err := s.writeUserProvidersTx(tx, userID, filtered); err != nil {
		return fmt.Errorf("remove oauth provider: %w", err)
	}
	if _, err := tx.Exec(s.q(`DELETE FROM user_oauth_identities WHERE user_id = ? AND provider = ?`), userID, provider); err != nil {
		return fmt.Errorf("remove oauth provider: forget subject: %w", err)
	}
	return tx.Commit()
}

// ErrLastAdmin is returned when a role change would leave zero admins.
var ErrLastAdmin = fmt.Errorf("cannot demote the last admin")

// AdminUserMetrics carries the windowed engagement metrics rendered by the
// admin user modal's Overview tab (T1553). Values are intentionally a
// small handful of cheap signals; per-request API tracking is filed as a
// follow-up (IDEA-1556). PLAN-1542 / TASK-1547.
type AdminUserMetrics struct {
	// DaysSinceWrite is days since users.last_write_at, rounded down.
	// nil when the user has never had a write recorded.
	DaysSinceWrite *int `json:"days_since_write"`
	// Writes7d is the count of write activities (items + comments)
	// authored in the last 7 days.
	Writes7d int `json:"writes_7d"`
	// CollectionsTouched30d is the count of DISTINCT collection_ids
	// touched by this user's item-write activities in the last 30 days.
	CollectionsTouched30d int `json:"collections_touched_30d"`
}

// GetUserMetrics computes the AdminUserMetrics bundle. Cheap by design:
//   - days_since_write reads users.last_write_at directly (one row)
//   - writes_7d is a COUNT over activities (indexed on user_id, created_at)
//   - collections_touched_30d JOINs activities to items to pull collection_id
//     (items.last_modified_by is an attribution string, not a user FK — see
//     the T1543 architecture note — so we go through activities.user_id).
//
// PLAN-1542 / TASK-1547. Caching is intentionally not implemented here;
// the queries are all index-backed scalar aggregations, and a per-user
// short cache lives more naturally at the handler boundary if needed.
func (s *Store) GetUserMetrics(userID string) (*AdminUserMetrics, error) {
	now := time.Now().UTC()
	cutoff7d := now.Add(-7 * 24 * time.Hour).Format(time.RFC3339)
	cutoff30d := now.Add(-30 * 24 * time.Hour).Format(time.RFC3339)

	out := &AdminUserMetrics{}

	// days_since_write
	var lwa sql.NullString
	if err := s.db.QueryRow(s.q(`SELECT last_write_at FROM users WHERE id = ?`), userID).Scan(&lwa); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("user not found")
		}
		return nil, fmt.Errorf("get user metrics last_write_at: %w", err)
	}
	if lwa.Valid && lwa.String != "" {
		if t, err := time.Parse(time.RFC3339, lwa.String); err == nil {
			d := int(now.Sub(t).Hours() / 24)
			if d < 0 {
				d = 0
			}
			out.DaysSinceWrite = &d
		}
	}

	// writes_7d — write-class activities by this user in the last 7d.
	writeActions := []string{"created", "updated", "archived", "restored", "moved", "commented"}
	placeholders := make([]string, len(writeActions))
	args := make([]interface{}, 0, len(writeActions)+2)
	args = append(args, userID, cutoff7d)
	for i, a := range writeActions {
		placeholders[i] = "?"
		args = append(args, a)
	}
	writesQuery := s.q(`
		SELECT COUNT(*)
		FROM activities
		WHERE user_id = ? AND created_at >= ?
		  AND action IN (` + strings.Join(placeholders, ", ") + `)
	`)
	if err := s.db.QueryRow(writesQuery, args...).Scan(&out.Writes7d); err != nil {
		return nil, fmt.Errorf("get user metrics writes_7d: %w", err)
	}

	// collections_touched_30d — DISTINCT collection_id of items the user
	// has authored writes for in the last 30d. Restrict to item-scoped
	// activities (document_id IS NOT NULL); commented activities live on
	// the same document so they count too.
	args = args[:0]
	args = append(args, userID, cutoff30d)
	for _, a := range writeActions {
		args = append(args, a)
	}
	collQuery := s.q(`
		SELECT COUNT(DISTINCT i.collection_id)
		FROM activities a
		JOIN items i ON i.id = a.document_id
		WHERE a.user_id = ? AND a.created_at >= ?
		  AND a.action IN (` + strings.Join(placeholders, ", ") + `)
		  AND i.deleted_at IS NULL
	`)
	if err := s.db.QueryRow(collQuery, args...).Scan(&out.CollectionsTouched30d); err != nil {
		return nil, fmt.Errorf("get user metrics collections_touched_30d: %w", err)
	}

	return out, nil
}

// TouchUserActivity updates last_active_at for a user, throttled to avoid
// write amplification. Only writes if the stored value is older than 5 minutes.
// Accepts a context so callers can bound the write duration.
func (s *Store) TouchUserActivity(ctx context.Context, userID string) {
	ts := now()
	// Conditional update: only write if NULL or older than 5 minutes
	s.db.ExecContext(ctx, s.q(`
		UPDATE users SET last_active_at = ?
		WHERE id = ? AND (last_active_at IS NULL OR last_active_at < ?)
	`), ts, userID, throttleTime(ts))
}

// TouchUserWrite updates last_write_at for a user, throttled to avoid write
// amplification. Only writes if the stored value is older than 5 minutes.
// Best-effort: errors are swallowed (matches TouchUserActivity).
//
// Unlike last_active_at (which fires on any authenticated request), this is
// only called from write-path handler sites — item create/update/delete/move,
// comment authored, attachment uploaded. See handlers_documents.go's
// logActivityWithMetaReturningID and handlers_attachments.go's upload handler.
//
// Silent no-op on empty userID so callers don't have to guard.
func (s *Store) TouchUserWrite(ctx context.Context, userID string) {
	if userID == "" {
		return
	}
	ts := now()
	s.db.ExecContext(ctx, s.q(`
		UPDATE users SET last_write_at = ?
		WHERE id = ? AND (last_write_at IS NULL OR last_write_at < ?)
	`), ts, userID, throttleTime(ts))
}

// throttleTime returns a timestamp 5 minutes before the given RFC3339 time string.
func throttleTime(ts string) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts
	}
	return t.Add(-5 * time.Minute).Format(time.RFC3339)
}

// DisableUser soft-disables a user account by setting disabled_at.
func (s *Store) DisableUser(userID string) error {
	_, err := s.db.Exec(s.q(`UPDATE users SET disabled_at = ?, updated_at = ?, credential_epoch = credential_epoch + 1 WHERE id = ?`),
		now(), now(), userID)
	if err != nil {
		return fmt.Errorf("disable user: %w", err)
	}
	return nil
}

// requireActiveUserTx is the one gate every credential-minting insert passes
// (BUG-3349): it reads the user's row inside the insert's own transaction,
// FOR SHARE on Postgres, and refuses with ErrUserDisabled when the account is
// disabled or gone. FOR SHARE conflicts with the row lock a disable's UPDATE
// and an account deletion take, so a mint and a disable serialize. Either the
// mint commits first and the disable revokes it, or the disable commits
// first and the mint sees it. Neither locks the other's rows first, so there
// is no cycle. SQLite serializes writers.
//
// A caller that also takes the users row FOR NO KEY UPDATE (the plan-limit
// lock, enforceUserLimitTx) must take THAT first: FOR SHARE then an upgrade
// lets two concurrent mints each hold the share and wait on the other's
// (a deadlock). After the stronger lock, this read waits on nothing.
//
// A caller that UPDATEs the users row itself should not call this at all:
// make the UPDATE conditional on `disabled_at IS NULL` and treat zero rows as
// ErrUserDisabled, which decides it under the UPDATE's own lock with no share
// to upgrade from. ConsumeEmailVerification does exactly that. TestEveryCredentialInsertRequires
// AnActiveUser holds every insert into a credential table to calling it.
func (s *Store) requireActiveUserTx(tx *sql.Tx, userID string) error {
	q := `SELECT CASE WHEN disabled_at IS NULL THEN 0 ELSE 1 END, kind FROM users WHERE id = ?`
	if s.dialect.Driver() == DriverPostgres {
		q += ` FOR SHARE`
	}
	var disabled int
	var kind string
	switch err := tx.QueryRow(s.q(q), userID).Scan(&disabled, &kind); {
	case errors.Is(err, sql.ErrNoRows):
		// Gone, which callers may also want to tell apart (an account
		// deletion that won the race), so it matches both.
		return fmt.Errorf("%w: %w", ErrUserDisabled, sql.ErrNoRows)
	case err != nil:
		return fmt.Errorf("read user: %w", err)
	case disabled == 1:
		return ErrUserDisabled
	case kind == models.UserKindApp:
		// No person's credential is ever minted for a bot (TASK-3392). It
		// matches ErrUserDisabled too, so every caller already refuses it.
		return fmt.Errorf("%w: %w", ErrUserDisabled, ErrAppPrincipal)
	}
	return nil
}

// DisableUserAndRevokeAccess disables the account and ends every credential
// it holds, in one transaction (BUG-3349): sessions, API tokens, and OAuth
// (MCP) grants, both the connection rows and every access and refresh token
// issued to the user. Disabling used to delete sessions only, so a PAT or an
// MCP connection kept working, and re-enabling would have revived them.
// Nothing here is restored by EnableUser: the user signs in, mints tokens
// and reconnects apps again.
func (s *Store) DisableUserAndRevokeAccess(userID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("disable user: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.disableUserAndRevokeAccessTx(tx, userID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("disable user: commit: %w", err)
	}
	return nil
}

// disableUserAndRevokeAccessTx is DisableUserAndRevokeAccess on the caller's
// transaction: the app uninstall disables the install's bot inside
// UninstallAppTx (TASK-3397, U8c).
func (s *Store) disableUserAndRevokeAccessTx(tx *sql.Tx, userID string) error {
	ts := now()
	stmts := []struct {
		what, query string
		args        []any
	}{
		{"disable", `UPDATE users SET disabled_at = ?, updated_at = ?, credential_epoch = credential_epoch + 1 WHERE id = ?`, []any{ts, ts, userID}},
		{"delete sessions", `DELETE FROM sessions WHERE user_id = ?`, []any{userID}},
		{"delete api tokens", `DELETE FROM api_tokens WHERE user_id = ?`, []any{userID}},
		{"revoke oauth access tokens", `UPDATE oauth_access_tokens SET active = ? WHERE subject = ?`, []any{s.dialect.BoolToInt(false), userID}},
		{"revoke oauth refresh tokens", `UPDATE oauth_refresh_tokens SET active = ? WHERE subject = ?`, []any{s.dialect.BoolToInt(false), userID}},
		// An authorization code not yet exchanged carries no subject column,
		// but shares its request id with the connection row the consent step
		// wrote: deactivate it before that row goes, or a re-enable would let
		// it exchange (codex review). PKCE rows ride the same request id.
		{"revoke oauth authorization codes", `UPDATE oauth_authorization_codes SET active = ? WHERE request_id IN (SELECT request_id FROM oauth_connections WHERE user_id = ?)`, []any{s.dialect.BoolToInt(false), userID}},
		{"delete oauth pkce requests", `DELETE FROM oauth_pkce_requests WHERE request_id IN (SELECT request_id FROM oauth_connections WHERE user_id = ?)`, []any{userID}},
		// A delegated grant to an installed app has no connection row; its
		// unexchanged code and PKCE row are found through its binding
		// (TASK-3399), or a re-enable would let the code exchange.
		{"revoke delegated app codes", `UPDATE oauth_authorization_codes SET active = ? WHERE request_id IN (SELECT request_id FROM app_token_bindings WHERE delegated_user_id = ?)`, []any{s.dialect.BoolToInt(false), userID}},
		{"delete delegated app pkce requests", `DELETE FROM oauth_pkce_requests WHERE request_id IN (SELECT request_id FROM app_token_bindings WHERE delegated_user_id = ?)`, []any{userID}},
		{"delete oauth connections", `DELETE FROM oauth_connections WHERE user_id = ?`, []any{userID}},
	}
	for _, st := range stmts {
		if _, err := tx.Exec(s.q(st.query), st.args...); err != nil {
			return fmt.Errorf("disable user: %s: %w", st.what, err)
		}
	}
	return nil
}

// EnableUser re-enables a disabled user account by clearing disabled_at.
func (s *Store) EnableUser(userID string) error {
	_, err := s.db.Exec(s.q(`UPDATE users SET disabled_at = NULL, updated_at = ? WHERE id = ?`),
		now(), userID)
	if err != nil {
		return fmt.Errorf("enable user: %w", err)
	}
	return nil
}

// SetUserRole updates a user's role (admin or member).
// When demoting an admin to member, the update is conditional: it only
// proceeds if at least one other admin exists, preventing a race where
// two concurrent demotions could leave zero admins.
func (s *Store) SetUserRole(userID, role string) error {
	var result sql.Result
	var err error

	// A bot is never an admin (TASK-3392).
	if role == "admin" {
		if err := s.refuseAppPrincipalQ(s.db, userID); err != nil {
			return err
		}
	}

	if role == "member" {
		// Atomic guard: only demote if another admin remains.
		result, err = s.db.Exec(s.q(`
			UPDATE users SET role = ?, updated_at = ?
			WHERE id = ? AND (
				role != 'admin'
				OR (SELECT COUNT(*) FROM users WHERE role = 'admin' AND kind = 'human' AND id != ?) > 0
			)
		`), role, now(), userID, userID)
	} else {
		result, err = s.db.Exec(s.q(`UPDATE users SET role = ?, updated_at = ? WHERE id = ?`),
			role, now(), userID)
	}
	if err != nil {
		return fmt.Errorf("set user role: %w", err)
	}

	n, _ := result.RowsAffected()
	if n == 0 {
		return ErrLastAdmin
	}
	return nil
}

// DeleteUser permanently deletes a user by ID.
func (s *Store) DeleteUser(id string) error {
	_, err := s.db.Exec(s.q(`DELETE FROM users WHERE id = ?`), id)
	if err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	return nil
}

// DeleteAccountAtomic deletes a user and all their owned workspaces in a single
// transaction. If any step fails, the entire operation is rolled back and no data
// is modified. This prevents orphaned workspaces from partial deletions.
//
// Every table with a foreign key to users(id) is handled here so the final
// DELETE FROM users can't 500 on an FK constraint (TASK-1959). Three postures,
// mirroring the FK audit:
//
//   - De-identify (UPDATE ... SET NULL): audit/history rows that should survive
//     with their identity dropped — the comments.user_id posture from TASK-509.
//     They live on in soft-deleted owned workspaces and in other workspaces the
//     user contributed to.
//   - Delete: rows the user solely owns or that are transient/audit and not
//     worth de-identifying (sessions, tokens, memberships, sent invitations,
//     issued grants, created share links, MCP audit, OAuth connections).
//   - Cascade: rows the schema already removes/nulls at DELETE FROM users
//     time — item_stars, user_report_layouts, user_workspace_tabs,
//     {collection,item}_grants.user_id
//     (ON DELETE CASCADE); items.assigned_user_id and activities.user_id
//     (ON DELETE SET NULL; activities gained its FK action in migrations
//     072/050).
//
// On Postgres a row referencing the user can still commit after the cleanup
// statement for its table and before the final DELETE FROM users (a login,
// a share-link view, a grant the user issues in another tab): the users-row
// lock here is NO KEY UPDATE, which a referencing insert's FOR KEY SHARE does
// not wait for. The delete then fails its foreign key (BUG-3289). Such a
// failure rolls the whole attempt back, so it is retried from the top, where
// the cleanup sees the new row. Taking the users row FOR UPDATE instead
// would make those inserts wait, but a writer that locks a row this
// transaction writes later (a share-link view of the user's own link) and
// then references the user would deadlock against it (measured: 40P01).
//
// The retried body is database-only: every statement runs in the attempt's
// transaction, and nothing outside it (files, email, events, billing) is
// touched here, so a rolled-back attempt leaves nothing behind and a retry
// repeats no external effect. The account handler's Stripe cancel runs once,
// BEFORE this call, on purpose (TASK-690: a cancel that fails must stop the
// delete); a retry here does not repeat it, and succeeding on a retry is
// what keeps a late reference from reaching the handler's partial-delete
// branch.
//
// SQLite never retries: BEGIN IMMEDIATE serialises every writer, so no
// reference can land inside the transaction, and its errors carry no
// SQLSTATE for lateUserReference to match.
func (s *Store) DeleteAccountAtomic(userID string) error {
	return s.deleteAccountAtomic(userID, nil)
}

// DeleteAccountAtomicReport is DeleteAccountAtomic that also reports the
// workspaces whose grants the user had ISSUED, read from the rows the
// transaction itself deleted (TASK-3365, codex r2 on PR2): a list read before
// the call could miss a grant created in between and deleted by it. The
// caller kicks every connection on those workspaces after the commit.
func (s *Store) DeleteAccountAtomicReport(userID string) ([]string, error) {
	var issued []string
	if err := s.deleteAccountAtomic(userID, &issued); err != nil {
		return nil, err
	}
	return issued, nil
}

func (s *Store) deleteAccountAtomic(userID string, issuedGrantWorkspaces *[]string) error {
	for attempt := 1; ; attempt++ {
		err := s.deleteAccountAtomicOnce(userID, issuedGrantWorkspaces)
		constraint, late := lateUserReference(err)
		if !late {
			return err
		}
		if attempt == deleteAccountAttempts {
			slog.Warn("delete account: a reference to the user kept landing after cleanup; giving up",
				"user_id", userID, "constraint", constraint, "attempts", attempt)
			return err
		}
		slog.Info("delete account: a reference to the user landed after cleanup; retrying",
			"user_id", userID, "constraint", constraint, "attempt", attempt)
	}
}

// deleteAccountAttempts bounds DeleteAccountAtomic's attempts. Each retry
// needs a new referencing row to commit inside one attempt's window, so a
// third failure means something is writing references in a loop.
const deleteAccountAttempts = 3

// lateUserReferenceConstraints are the foreign keys to users with no ON
// DELETE action, the only ones DELETE FROM users can fail on. A late
// reference is retried only through one of these, so a constraint added
// without being listed here fails the deletion as before, and
// TestLateUserReferenceConstraints_MatchSchema fails until it is listed.
var lateUserReferenceConstraints = map[string]bool{
	"api_tokens_user_id_fkey":                true,
	"collection_grants_granted_by_fkey":      true,
	"email_verification_tokens_user_id_fkey": true,
	"fk_items_created_by_user":               true,
	"fk_items_modified_by_user":              true,
	"item_grants_granted_by_fkey":            true,
	"mcp_audit_log_user_id_fkey":             true,
	"password_reset_tokens_user_id_fkey":     true,
	"sessions_user_id_fkey":                  true,
	"share_link_views_viewer_user_id_fkey":   true,
	"share_links_created_by_fkey":            true,
	"workspace_invitations_invited_by_fkey":  true,
	"workspace_members_user_id_fkey":         true,
}

// finalUserDeleteError marks an error from the final DELETE FROM users, so
// a foreign-key violation from any earlier statement is never retried. Its
// text is the error this step always returned.
type finalUserDeleteError struct{ err error }

func (e *finalUserDeleteError) Error() string {
	return "delete account: delete user: " + e.err.Error()
}

func (e *finalUserDeleteError) Unwrap() error { return e.err }

// lateUserReference reports whether err is the final DELETE FROM users
// failing SQLSTATE 23503 on one of lateUserReferenceConstraints, matched on
// the driver's code and constraint name rather than on message text, and
// names the constraint.
func lateUserReference(err error) (string, bool) {
	var final *finalUserDeleteError
	if !errors.As(err, &final) {
		return "", false
	}
	var pgErr *pgconn.PgError
	if !errors.As(final.err, &pgErr) || pgErr.Code != "23503" {
		return "", false
	}
	return pgErr.ConstraintName, lateUserReferenceConstraints[pgErr.ConstraintName]
}

// issuedGrantWorkspaces, when non-nil, is RESET and refilled by each
// attempt, so after a retry it holds only the committing attempt's rows.
// deleteAccountAfterUserLockHook, when set by a test, runs once account
// deletion holds the account lock (TASK-3351 lock-order test).
var deleteAccountAfterUserLockHook func()

func (s *Store) deleteAccountAtomicOnce(userID string, issuedGrantWorkspaces *[]string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("delete account: begin tx: %w", err)
	}
	defer tx.Rollback()

	ts := now()

	// Account deletions serialise against each other on Postgres (BUG-3286).
	// Each one writes rows through a predicate on ITS user (tabs of its owned
	// workspaces, grants it issued, share links it created, items it
	// authored) that another user's deletion reaches through that user's FK
	// cascade or SET NULL at DELETE FROM users. With a mirror-image pair of
	// such rows two deletions each hold what the other needs, and neither
	// knows the other's id up front, so no row order can fix it.
	// delete_account_lockorder_test.go has one ingredient per subtest, and
	// all five deadlocked before this lock. It is taken before anything else
	// and only here, so a transaction waiting on it holds nothing, and it
	// locks no row, so the users-row-first rule below is unchanged. SQLite's
	// BEGIN IMMEDIATE already serialises every writer.
	if s.dialect.Driver() == DriverPostgres {
		if _, err := tx.Exec("SELECT pg_advisory_xact_lock(hashtext('pad:account-deletion'))"); err != nil {
			return fmt.Errorf("delete account: serialise: %w", err)
		}
	}

	// 0. Lock the user's row FIRST, with the lock every limited mint of a
	// user-scoped row takes (enforceUserLimitTx, BUG-2808). The owned set
	// below is read under it, so it is exact (BUG-3099): a mint that
	// committed before the lock is in the set, and one still open waits
	// here, then finds no user row when this commits and rolls back. The
	// set used to be read by the HANDLER before this transaction began, and
	// from memberships, so a workspace minted in between, or owned but not
	// yet joined, survived as a live workspace of a deleted user. On SQLite
	// the transaction is already BEGIN IMMEDIATE, which serialises every
	// writer, and the row-locking clause would be a syntax error there.
	//
	// Deadlock-free against the mint because the mint's only write before
	// its own lock is the workspaces row, which takes no lock on users (no
	// foreign key from workspaces.owner_id on Postgres): it holds nothing
	// this transaction waits for.
	lock := `SELECT id FROM users WHERE id = ?`
	if s.dialect.Driver() == DriverPostgres {
		lock += ` FOR NO KEY UPDATE`
	}
	var lockedID string
	if err := tx.QueryRow(s.q(lock), userID).Scan(&lockedID); err != nil {
		return fmt.Errorf("delete account: lock user: %w", err)
	}
	if deleteAccountAfterUserLockHook != nil {
		deleteAccountAfterUserLockHook()
	}

	// 0b. Delete the grants the user issued BEFORE the tabs below (BUG-3288).
	// A revoke of such a grant that removes the grantee's last access to one
	// of these workspaces takes the grant row and then the grantee's tab row
	// there, which is the row step 1 deletes. Deleting the grants after the
	// tabs took the two rows in the opposite order, and the two transactions
	// deadlocked (40P01, delete_account_writer_lockorder_test.go). Now both
	// take the grant row first, so whichever gets it second waits holding
	// nothing the other needs.
	// RETURNING, so the workspaces reported are exactly the rows deleted.
	issuedSeen := map[string]bool{}
	for _, stmt := range []struct{ what, query string }{
		{"delete issued collection grants", "DELETE FROM collection_grants WHERE granted_by = ? RETURNING workspace_id"},
		{"delete issued item grants", "DELETE FROM item_grants WHERE granted_by = ? RETURNING workspace_id"},
	} {
		rows, err := tx.Query(s.q(stmt.query), userID)
		if err != nil {
			return fmt.Errorf("delete account: %s: %w", stmt.what, err)
		}
		for rows.Next() {
			var wsID string
			if err := rows.Scan(&wsID); err != nil {
				rows.Close()
				return fmt.Errorf("delete account: %s: %w", stmt.what, err)
			}
			issuedSeen[wsID] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("delete account: %s: %w", stmt.what, err)
		}
		rows.Close()
	}
	if issuedGrantWorkspaces != nil {
		*issuedGrantWorkspaces = (*issuedGrantWorkspaces)[:0]
		for wsID := range issuedSeen {
			*issuedGrantWorkspaces = append(*issuedGrantWorkspaces, wsID)
		}
	}

	// 1. Soft-delete every workspace the user OWNS. They keep their rows (and
	// every item/activity/comment within) so the data stays recoverable; only
	// the user identity is hard-removed below.
	if _, err := tx.Exec(s.q(`
		UPDATE workspaces SET deleted_at = ?, updated_at = ?
		WHERE owner_id = ? AND deleted_at IS NULL
	`), ts, ts, userID); err != nil {
		return fmt.Errorf("delete account: delete owned workspaces: %w", err)
	}
	// Other users' tabs for those workspaces go with them, as on any soft
	// delete (TASK-3256). Every owned workspace is soft-deleted by now.
	//
	// Unlike soft delete, this does NOT bump the holders' tabs revisions
	// (BUG-3285). Bumping locks each holder's users row, and this transaction
	// already holds the deleting user's row (step 0), so it would take users
	// rows out of id order: an account deletion racing a soft delete or a
	// tab write by a holder could deadlock, where before it locked no other
	// user's row. (Two account deletions no longer overlap; see the advisory
	// lock above.) The
	// bump would buy nothing here anyway: these workspaces are soft-deleted
	// in this same transaction, so every tabs answer from now on filters
	// them out on read whatever its revision.
	if _, err := tx.Exec(s.q(`
		DELETE FROM user_workspace_tabs
		WHERE workspace_id IN (SELECT id FROM workspaces WHERE owner_id = ?)
	`), userID); err != nil {
		return fmt.Errorf("delete account: delete tabs of owned workspaces: %w", err)
	}

	// 1b. The bots of installs in those workspaces go with them, with every
	// credential and reference they hold (lead ruling on TASK-3392: no
	// orphan principals). A bot is a member of its install's workspace only.
	// Deleting a bot locks its users row; nothing a person does locks one
	// (a bot holds no session), and account deletions do not overlap (the
	// advisory lock above), so this adds no lock-order hazard.
	if err := s.purgeAppPrincipalsOfOwnedWorkspacesTx(tx, userID); err != nil {
		return fmt.Errorf("delete account: %w", err)
	}

	if err := s.eraseUserTx(tx, userID); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("delete account: commit: %w", err)
	}
	return nil
}

// eraseUserTx removes one account and every reference to it, in the caller's
// transaction: the de-identify, outbox scrub, delete and final delete steps of
// account deletion. Account deletion runs it for the account and for each bot
// it purges, so a purged bot leaves exactly what a deleted person leaves.
func (s *Store) eraseUserTx(tx *sql.Tx, userID string) error {
	// exec runs one cleanup statement keyed on userID, wrapping the error with
	// context. Each statement clears a reference to the user so the final
	// DELETE FROM users can't trip a foreign-key constraint.
	exec := func(what, query string) error {
		if _, err := tx.Exec(s.q(query), userID); err != nil {
			return fmt.Errorf("delete account: %s: %w", what, err)
		}
		return nil
	}

	// 2. De-identify audit/history rows (preserve row, drop identity). These
	// FKs are RESTRICT on SQLite and either RESTRICT (items) or absent
	// (comments/item_links/item_versions/comment_reactions) on Postgres —
	// nulling here keeps the delete safe and the data clean on both dialects.
	deidentify := []struct{ what, query string }{
		{"detach authored items", "UPDATE items SET created_by_user_id = NULL WHERE created_by_user_id = ?"},
		{"detach modified items", "UPDATE items SET last_modified_by_user_id = NULL WHERE last_modified_by_user_id = ?"},
		{"detach comments", "UPDATE comments SET user_id = NULL WHERE user_id = ?"},
		{"detach comment reactions", "UPDATE comment_reactions SET user_id = NULL WHERE user_id = ?"},
		{"detach item links", "UPDATE item_links SET user_id = NULL WHERE user_id = ?"},
		{"detach item versions", "UPDATE item_versions SET user_id = NULL WHERE user_id = ?"},
		{"detach share-link views", "UPDATE share_link_views SET viewer_user_id = NULL WHERE viewer_user_id = ?"},
	}
	for _, stmt := range deidentify {
		if err := exec(stmt.what, stmt.query); err != nil {
			return err
		}
	}

	// 2b. Erase the user's id from frozen outbox payloads and subject_ids.
	// The de-identify list above reaches LIVE rows only; outbox payloads froze
	// the id at emit time and would otherwise keep it legible — in workspaces
	// the user didn't own — until TASK-2714's retention window closes on the
	// row. Dave's ruling on TASK-2719: "delete my account" means prompt
	// erasure, not a bounded window.
	if err := s.ScrubOutboxUserRefsTx(tx, userID); err != nil {
		return fmt.Errorf("delete account: %w", err)
	}

	// 3. Delete rows the user owns or that are transient/audit. CASCADE cleans
	// the dependents: member_collection_access (off workspace_members),
	// share_link_views (off deleted share_links), oauth_connection_workspaces
	// (off oauth_connections).
	deletes := []struct{ what, query string }{
		{"delete sessions", "DELETE FROM sessions WHERE user_id = ?"},
		{"delete api tokens", "DELETE FROM api_tokens WHERE user_id = ?"},
		{"delete workspace memberships", "DELETE FROM workspace_members WHERE user_id = ?"},
		{"delete sent invitations", "DELETE FROM workspace_invitations WHERE invited_by = ?"},
		{"delete password reset tokens", "DELETE FROM password_reset_tokens WHERE user_id = ?"},
		{"delete email verification tokens", "DELETE FROM email_verification_tokens WHERE user_id = ?"},
		{"delete created share links", "DELETE FROM share_links WHERE created_by = ?"},
		{"delete mcp audit log", "DELETE FROM mcp_audit_log WHERE user_id = ?"},
		{"delete oauth connections", "DELETE FROM oauth_connections WHERE user_id = ?"},
	}
	for _, stmt := range deletes {
		if err := exec(stmt.what, stmt.query); err != nil {
			return err
		}
	}

	// 4. Delete the user record. Remaining references cascade (see doc comment).
	if _, err := tx.Exec(s.q("DELETE FROM users WHERE id = ?"), userID); err != nil {
		return &finalUserDeleteError{err: err}
	}

	return nil
}

// --- Username backfill ---

// GenerateUsername derives a URL-safe username from a display name.
// Falls back to the email local part if the name produces an empty result.
func GenerateUsername(name, email string) string {
	// Lowercase and replace spaces/special chars with hyphens
	u := strings.ToLower(strings.TrimSpace(name))
	u = usernameCleanRe.ReplaceAllString(u, "-")

	// Collapse consecutive hyphens, strip leading/trailing
	for strings.Contains(u, "--") {
		u = strings.ReplaceAll(u, "--", "-")
	}
	u = strings.Trim(u, "-")

	// Truncate to 39 chars (GitHub-style limit)
	if len(u) > 39 {
		u = u[:39]
		u = strings.TrimRight(u, "-")
	}

	// Fall back to email local part
	if u == "" && email != "" {
		local := strings.Split(email, "@")[0]
		u = strings.ToLower(local)
		u = usernameCleanRe.ReplaceAllString(u, "-")
		u = strings.Trim(u, "-")
		if len(u) > 39 {
			u = u[:39]
			u = strings.TrimRight(u, "-")
		}
	}

	if u == "" {
		u = "user"
	}
	return u
}

// EnsureUniqueUsername takes a candidate username and returns a unique variant
// by appending -2, -3, etc. if the candidate already exists in the database.
func (s *Store) EnsureUniqueUsername(base string) (string, error) {
	username := base
	suffix := 2
	for {
		existing, err := s.GetUserByUsername(username)
		if err != nil {
			return "", fmt.Errorf("check username uniqueness: %w", err)
		}
		if existing == nil {
			return username, nil
		}
		username = fmt.Sprintf("%s-%d", base, suffix)
		suffix++
	}
}

// backfillUsernames generates usernames for existing users that don't have one.
// Idempotent: skips users who already have a non-empty username.
func (s *Store) backfillUsernames() error {
	// Find users with empty username
	rows, err := s.db.Query(s.q(`SELECT id, name, email FROM users WHERE username = '' OR username IS NULL`))
	if err != nil {
		return fmt.Errorf("find users without username: %w", err)
	}
	defer rows.Close()

	type userRow struct {
		id, name, email string
	}
	var users []userRow
	for rows.Next() {
		var u userRow
		if err := rows.Scan(&u.id, &u.name, &u.email); err != nil {
			return fmt.Errorf("scan user: %w", err)
		}
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	if len(users) == 0 {
		return nil // Nothing to backfill
	}

	// Collect all existing usernames to detect collisions
	existing := make(map[string]bool)
	existingRows, err := s.db.Query(s.q(`SELECT username FROM users WHERE username != ''`))
	if err != nil {
		return fmt.Errorf("list existing usernames: %w", err)
	}
	defer existingRows.Close()
	for existingRows.Next() {
		var u string
		if err := existingRows.Scan(&u); err != nil {
			return err
		}
		existing[strings.ToLower(u)] = true
	}

	for _, u := range users {
		base := GenerateUsername(u.name, u.email)
		username := base

		// Handle collisions: append -2, -3, etc.
		suffix := 2
		for existing[username] {
			username = fmt.Sprintf("%s-%d", base, suffix)
			suffix++
		}

		existing[username] = true

		_, err := s.db.Exec(s.q(`UPDATE users SET username = ?, updated_at = ? WHERE id = ?`),
			username, now(), u.id)
		if err != nil {
			return fmt.Errorf("set username for user %s: %w", u.id, err)
		}
	}

	return nil
}

// --- TOTP 2FA ---

// SetTOTPSecret stores the TOTP secret for a user (before 2FA is verified).
// The secret is encrypted at rest if an encryption key is configured.
//
// It also clears totp_last_step: that single-use watermark (BUG-2054) is a
// time-step counter that belongs to the OLD secret, so a fresh secret must
// start with a clean slate — otherwise a re-enrolled authenticator's
// current-window code could be spuriously rejected until time advances past
// the stale watermark.
func (s *Store) SetTOTPSecret(userID, secret string) error {
	encrypted, err := s.encrypt(secret)
	if err != nil {
		return fmt.Errorf("encrypt totp secret: %w", err)
	}
	_, err = s.db.Exec(s.q(`UPDATE users SET totp_secret = ?, totp_last_step = NULL, updated_at = ? WHERE id = ?`), encrypted, now(), userID)
	if err != nil {
		return fmt.Errorf("set totp secret: %w", err)
	}
	return nil
}

// EnableTOTP atomically enables 2FA for a user and stores hashed recovery codes.
// The expectedSecret is the plaintext secret — it's compared against the stored
// (possibly encrypted) value to prevent TOCTOU races.
func (s *Store) EnableTOTP(userID, expectedSecret, hashedRecoveryCodes string) error {
	// Read the stored (possibly encrypted) secret to compare
	var storedSecret string
	err := s.db.QueryRow(s.q(`SELECT totp_secret FROM users WHERE id = ? AND totp_enabled = ?`),
		userID, s.dialect.BoolToInt(false)).Scan(&storedSecret)
	if err != nil {
		return fmt.Errorf("enable totp: read secret: %w", err)
	}

	// Decrypt stored secret for comparison
	decrypted, err := s.decrypt(storedSecret)
	if err != nil {
		return fmt.Errorf("enable totp: decrypt stored secret: %w", err)
	}
	if decrypted != expectedSecret {
		return fmt.Errorf("enable totp: secret mismatch or user not found")
	}

	// Update — use the stored (encrypted) value in WHERE for atomicity
	result, err := s.db.Exec(s.q(
		`UPDATE users SET totp_enabled = ?, recovery_codes = ?, updated_at = ?
		 WHERE id = ? AND totp_secret = ? AND totp_enabled = ?`),
		s.dialect.BoolToInt(true), hashedRecoveryCodes, now(), userID, storedSecret, s.dialect.BoolToInt(false))
	if err != nil {
		return fmt.Errorf("enable totp: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("enable totp: concurrent modification or user not found")
	}
	return nil
}

// DisableTOTP disables 2FA and clears the secret and recovery codes. It also
// clears the totp_last_step single-use watermark (BUG-2054) so it doesn't
// outlive the secret it belonged to and reject a future re-enrollment's codes.
func (s *Store) DisableTOTP(userID string) error {
	_, err := s.db.Exec(s.q(`UPDATE users SET totp_enabled = ?, totp_secret = '', recovery_codes = '', totp_last_step = NULL, updated_at = ? WHERE id = ?`),
		s.dialect.BoolToInt(false), now(), userID)
	if err != nil {
		return fmt.Errorf("disable totp: %w", err)
	}
	return nil
}

// ConsumeTOTPStep atomically records a TOTP time-step as consumed, enforcing
// single-use semantics for login codes (BUG-2054). It succeeds only if step is
// strictly greater than the user's stored totp_last_step (or none is stored
// yet), persisting the new value in the SAME guarded UPDATE. Two concurrent
// verifications for the same step therefore can't both win: the first advances
// totp_last_step and the second's WHERE clause no longer matches. Mirrors the
// compare-and-set pattern in UpdateSessionIPIfEquals.
//
// expectedSecret is the (decrypted) secret the caller validated the code
// against; the update is additionally gated on the stored secret STILL being
// that one. This closes a TOCTOU where a login validates, then the user
// disables / re-enrolls 2FA (clearing the watermark, migration 074), and this
// write would otherwise stamp the OLD secret's step back over the fresh secret
// — spuriously rejecting the new authenticator's next code. If the secret
// changed underfoot the login is refused (it validated against a secret that
// no longer exists), which is the correct outcome.
//
// Returns true if this call claimed the step (caller may proceed), false if the
// step was already consumed (a replay) or the secret changed — either way the
// caller must reject as an invalid code.
func (s *Store) ConsumeTOTPStep(userID, expectedSecret string, step int64) (bool, error) {
	// BEGIN IMMEDIATE (the store's _txlock) serializes this read-then-write
	// against a concurrent DisableTOTP/SetTOTPSecret on SQLite; on Postgres the
	// secret-equality guard in the UPDATE's WHERE provides the same protection.
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("consume totp step: begin: %w", err)
	}
	defer tx.Rollback()

	var storedSecret string
	err = tx.QueryRow(s.q(`SELECT totp_secret FROM users WHERE id = ?`), userID).Scan(&storedSecret)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("consume totp step: read secret: %w", err)
	}
	decrypted, err := s.decrypt(storedSecret)
	if err != nil {
		return false, fmt.Errorf("consume totp step: decrypt secret: %w", err)
	}
	if decrypted != expectedSecret {
		// Secret rotated out from under this login between validation and now.
		return false, nil
	}

	// Guard on the exact stored (possibly encrypted) ciphertext for atomicity,
	// the same technique EnableTOTP uses, so a secret change racing between the
	// SELECT and the UPDATE also fails the CAS instead of stamping a stale step.
	res, err := tx.Exec(s.q(`
		UPDATE users
		SET totp_last_step = ?
		WHERE id = ? AND totp_secret = ? AND (totp_last_step IS NULL OR totp_last_step < ?)
	`), step, userID, storedSecret, step)
	if err != nil {
		return false, fmt.Errorf("consume totp step: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		// Some drivers don't report RowsAffected reliably. Err on the safe
		// side: treat as "not the winner" so a replay is never accepted.
		return false, nil
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("consume totp step: commit: %w", err)
	}
	return n > 0, nil
}

// ConsumeRecoveryCode validates and removes a single recovery code.
// Recovery codes are stored as SHA-256 hashes. The provided plaintext
// code is hashed before comparison. Uses a transaction to prevent
// concurrent consumption of the same code.
func (s *Store) ConsumeRecoveryCode(userID, code string) (bool, error) {
	// Hash the input code for comparison against stored hashes
	inputHash := sha256.Sum256([]byte(code))
	inputHashStr := hex.EncodeToString(inputHash[:])

	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var recoveryCodes string
	err = tx.QueryRow(s.q(`SELECT recovery_codes FROM users WHERE id = ?`), userID).Scan(&recoveryCodes)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("select recovery codes: %w", err)
	}

	codes := strings.Split(recoveryCodes, "\n")
	var remaining []string
	found := false
	for _, c := range codes {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if !found && c == inputHashStr {
			found = true
			continue // consume this one
		}
		remaining = append(remaining, c)
	}

	if !found {
		return false, nil
	}

	// Use optimistic locking: include the original recovery_codes in the WHERE
	// clause so a concurrent transaction that already consumed a code will cause
	// this UPDATE to match 0 rows, preventing double-spend.
	result, err := tx.Exec(s.q(`UPDATE users SET recovery_codes = ?, updated_at = ? WHERE id = ? AND recovery_codes = ?`),
		strings.Join(remaining, "\n"), now(), userID, recoveryCodes)
	if err != nil {
		return false, fmt.Errorf("consume recovery code: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		// Another request consumed or modified the codes concurrently
		return false, nil
	}
	return true, tx.Commit()
}

// appPrincipalPasswordHash is every bot principal's password_hash. It is not a
// bcrypt string, so bcrypt refuses to compare against it and no password can
// ever match. The kind gate is what refuses a bot at every sign-in door; this
// is the second lock, never the first.
const appPrincipalPasswordHash = "!app-principal:no-password"

// CreateAppUserTx creates the bot principal an installed app acts as when it
// acts as itself (SPEC-6 §3, TASK-3392). It is the only path that writes
// kind = 'app': CreateUser and CreateOAuthUser refuse the reserved domain.
// It takes the install transaction, which creates the bot, its membership and
// the install client together. The bot has no password (password_set false),
// no verified email, and an address no person can hold:
// app+<install-id>@apps.pad.invalid.
func (s *Store) CreateAppUserTx(tx *sql.Tx, installID, displayName string) (*models.User, error) {
	installID = strings.TrimSpace(installID)
	if installID == "" {
		return nil, errors.New("create app user: install id is required")
	}
	name := strings.TrimSpace(displayName)
	if name == "" {
		return nil, errors.New("create app user: display name is required")
	}
	email := "app+" + strings.ToLower(installID) + "@" + models.AppPrincipalEmailDomain
	id := newID()
	base := "app-" + strings.ToLower(installID)
	if len(base) > 16 {
		base = base[:16]
	}
	username, err := s.uniqueUsernameTx(tx, base, id)
	if err != nil {
		return nil, fmt.Errorf("create app user: %w", err)
	}
	ts := now()
	if _, err := tx.Exec(s.q(`
		INSERT INTO users (id, email, username, name, password_hash, role, password_set, kind, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 'member', ?, 'app', ?, ?)
	`), id, email, username, name, appPrincipalPasswordHash, false, ts, ts); err != nil {
		return nil, fmt.Errorf("create app user: insert: %w", err)
	}
	return s.GetUserQ(tx, id)
}
