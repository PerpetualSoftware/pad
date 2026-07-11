package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
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
const userColumns = `id, email, username, name, password_hash, role, avatar_url, totp_secret, totp_enabled, recovery_codes, plan, plan_expires_at, stripe_customer_id, plan_overrides, oauth_providers, password_set, disabled_at, email_verified_at, last_active_at, last_write_at, created_at, updated_at`

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
		&u.Plan, &u.PlanExpiresAt, &u.StripeCustomerID, &u.PlanOverrides, &u.OAuthProviders,
		&u.PasswordSet,
		&disabledAt, &emailVerifiedAt, &lastActiveAt, &lastWriteAt, &createdAt, &updatedAt,
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

// CreateUser creates a new user with a hashed password.
func (s *Store) CreateUser(input models.UserCreate) (*models.User, error) {
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
	u, err := scanUser(s.db.QueryRow(s.q(`SELECT `+userColumns+` FROM users WHERE id = ?`), id))
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
	result, err := s.db.Exec(s.q(query), args...)
	if err != nil {
		return nil, fmt.Errorf("update user: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return nil, sql.ErrNoRows
	}

	return s.GetUser(id)
}

// ValidatePassword checks an email/password combination. Returns the user
// if valid, nil if the credentials are wrong (not an error).
func (s *Store) ValidatePassword(email, password string) (*models.User, error) {
	u, err := s.GetUserByEmail(email)
	if err != nil {
		return nil, err
	}
	if u == nil {
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
	rows, err := s.db.Query(s.q(`SELECT ` + userColumns + ` FROM users ORDER BY created_at ASC`))
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

	var where []string
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
			&entry.Plan, &entry.PlanExpiresAt, &entry.StripeCustomerID, &entry.PlanOverrides, &entry.OAuthProviders,
			&entry.PasswordSet,
			&disabledAt, &emailVerifiedAt, &lastActiveAt, &lastWriteAt, &createdAt, &updatedAt,
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
	err := s.db.QueryRow(s.q("SELECT COUNT(*) FROM users")).Scan(&count)
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
	rows, err := s.db.Query(s.q(`SELECT COALESCE(NULLIF(plan, ''), 'free') AS plan, COUNT(*)
		FROM users
		GROUP BY COALESCE(NULLIF(plan, ''), 'free')`))
	if err != nil {
		return nil, fmt.Errorf("count users by plan: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var plan string
		var count int
		if err := rows.Scan(&plan, &count); err != nil {
			return nil, fmt.Errorf("scan users-by-plan row: %w", err)
		}
		out.CustomersByPlan[plan] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate users-by-plan: %w", err)
	}

	cutoff := since.UTC().Format(time.RFC3339)
	if err := s.db.QueryRow(
		s.q(`SELECT COUNT(*) FROM users WHERE plan = 'pro' AND created_at > ?`),
		cutoff,
	).Scan(&out.NewProSignups); err != nil {
		return nil, fmt.Errorf("count new pro signups: %w", err)
	}

	return out, nil
}

// CreateOAuthUser creates a user from an OAuth provider with a random unusable password.
// OAuth users can later set a password via the password reset flow if they want.
func (s *Store) CreateOAuthUser(email, name, avatarURL string) (*models.User, error) {
	// Generate a random 64-byte password the user will never use
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
// No-op if the provider is already linked.
func (s *Store) AddOAuthProvider(userID, provider string) error {
	user, err := s.GetUser(userID)
	if err != nil {
		return fmt.Errorf("add oauth provider: %w", err)
	}
	if user == nil {
		return fmt.Errorf("add oauth provider: user not found")
	}

	if user.HasOAuthProvider(provider) {
		return nil // Already linked
	}

	providers := user.GetOAuthProviders()
	providers = append(providers, provider)
	data, err := json.Marshal(providers)
	if err != nil {
		return fmt.Errorf("add oauth provider: marshal: %w", err)
	}

	_, err = s.db.Exec(s.q(`UPDATE users SET oauth_providers = ?, updated_at = ? WHERE id = ?`),
		string(data), now(), userID)
	if err != nil {
		return fmt.Errorf("add oauth provider: %w", err)
	}
	return nil
}

// RemoveOAuthProvider removes a provider from the user's oauth_providers list.
func (s *Store) RemoveOAuthProvider(userID, provider string) error {
	user, err := s.GetUser(userID)
	if err != nil {
		return fmt.Errorf("remove oauth provider: %w", err)
	}
	if user == nil {
		return fmt.Errorf("remove oauth provider: user not found")
	}

	providers := user.GetOAuthProviders()
	var filtered []string
	for _, p := range providers {
		if p != provider {
			filtered = append(filtered, p)
		}
	}

	var val string
	if len(filtered) > 0 {
		data, err := json.Marshal(filtered)
		if err != nil {
			return fmt.Errorf("remove oauth provider: marshal: %w", err)
		}
		val = string(data)
	}

	_, err = s.db.Exec(s.q(`UPDATE users SET oauth_providers = ?, updated_at = ? WHERE id = ?`),
		val, now(), userID)
	if err != nil {
		return fmt.Errorf("remove oauth provider: %w", err)
	}
	return nil
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
	_, err := s.db.Exec(s.q(`UPDATE users SET disabled_at = ?, updated_at = ? WHERE id = ?`),
		now(), now(), userID)
	if err != nil {
		return fmt.Errorf("disable user: %w", err)
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

	if role == "member" {
		// Atomic guard: only demote if another admin remains.
		result, err = s.db.Exec(s.q(`
			UPDATE users SET role = ?, updated_at = ?
			WHERE id = ? AND (
				role != 'admin'
				OR (SELECT COUNT(*) FROM users WHERE role = 'admin' AND id != ?) > 0
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
//     time — item_stars, user_report_layouts, {collection,item}_grants.user_id
//     (ON DELETE CASCADE); items.assigned_user_id and activities.user_id
//     (ON DELETE SET NULL; activities gained its FK action in migrations
//     072/050).
func (s *Store) DeleteAccountAtomic(userID string, ownedWorkspaceSlugs []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("delete account: begin tx: %w", err)
	}
	defer tx.Rollback()

	ts := now()

	// 1. Soft-delete all owned workspaces. They keep their rows (and every
	// item/activity/comment within) so the data stays recoverable; only the
	// user identity is hard-removed below.
	for _, slug := range ownedWorkspaceSlugs {
		if _, err := tx.Exec(s.q(`
			UPDATE workspaces SET deleted_at = ?, updated_at = ?
			WHERE slug = ? AND deleted_at IS NULL
		`), ts, ts, slug); err != nil {
			return fmt.Errorf("delete account: delete workspace %s: %w", slug, err)
		}
	}

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
		{"delete issued collection grants", "DELETE FROM collection_grants WHERE granted_by = ?"},
		{"delete issued item grants", "DELETE FROM item_grants WHERE granted_by = ?"},
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
	if err := exec("delete user", "DELETE FROM users WHERE id = ?"); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("delete account: commit: %w", err)
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
