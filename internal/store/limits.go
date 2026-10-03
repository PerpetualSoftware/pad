package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// PlanLimits defines the limits for a billing plan tier.
type PlanLimits struct {
	Workspaces          int `json:"workspaces"`
	ItemsPerWorkspace   int `json:"items_per_workspace"`
	MembersPerWorkspace int `json:"members_per_workspace"`
	APITokens           int `json:"api_tokens"`
	StorageBytes        int `json:"storage_bytes"`
	Webhooks            int `json:"webhooks"`
	AutomatedBackups    int `json:"automated_backups"`
}

// DefaultFreeLimits are the hardcoded fallback limits for the free tier.
// These are used only if the platform_settings table has no stored defaults.
var DefaultFreeLimits = PlanLimits{
	Workspaces:          3,
	ItemsPerWorkspace:   1000,
	MembersPerWorkspace: 3,
	APITokens:           10,
	StorageBytes:        524288000, // 500MB
	Webhooks:            0,
	AutomatedBackups:    0,
}

// DefaultProLimits are the hardcoded fallback limits for the pro tier.
// -1 means unlimited.
var DefaultProLimits = PlanLimits{
	Workspaces:          -1,
	ItemsPerWorkspace:   -1,
	MembersPerWorkspace: -1,
	APITokens:           -1,
	StorageBytes:        10737418240, // 10GB
	Webhooks:            -1,
	AutomatedBackups:    -1,
}

// LimitResult is returned by CheckLimit with the enforcement decision
// and current usage info for the feature.
type LimitResult struct {
	Allowed bool   `json:"allowed"`
	Feature string `json:"feature"`
	Limit   int    `json:"limit"` // -1 means unlimited
	Current int    `json:"current"`
	Plan    string `json:"plan"`

	// Requested is how many of the feature a single operation would add at
	// once, set only where that is more than one — today, ImportWorkspace
	// (BUG-3103). Zero and omitted everywhere else, so every other door's
	// JSON and rendered message are byte-identical to before this field
	// existed.
	//
	// It exists because Current could not answer the question Dave's ruling
	// asks the refusal to answer — "how many would land vs the limit". The
	// obvious shortcut, setting Current to the incoming count, is WRONG and
	// deliberately not taken: Current is published in the API's error
	// `details` and means "how many this workspace holds NOW" at every other
	// door. Overloading it would make a consumer render "150 of 100" for a
	// workspace holding zero items, and the overload is invisible at the call
	// site. Two questions, two fields.
	Requested int `json:"requested,omitempty"`
}

// CheckLimit checks whether a workspace operation is allowed under the
// owner's plan. Resolution order:
//  1. User plan_overrides[feature] — per-user override (if set)
//  2. Platform plan_limits[plan][feature] — DB-stored defaults for the tier
//  3. Hardcoded fallback — safety net if DB config is missing
//
// This is an ADVISORY read on the pool: callers use it for an early refusal.
// The authoritative check is the one a limited insert (WithPlanLimit) takes
// inside its own transaction, under its lock (BUG-2808).
func (s *Store) CheckLimit(workspaceID, feature string) (*LimitResult, error) {
	return s.checkLimitOn(s.db, workspaceID, feature)
}

// CheckLimitTx is CheckLimit with every read routed through the caller's
// transaction instead of an independent connection (PLAN-2357 / DR-16).
//
// The COUNT is where the routing is a correctness matter: a limit check that
// counts on the pool cannot see the caller's own uncommitted inserts, and —
// more importantly — it is not serialized with a concurrent transaction
// holding the workspace's advisory lock. Two copies into a workspace one item
// below its cap would then both read "under the limit" and both commit.
// Counting inside the transaction, after the destination workspace lock is
// held, makes the second copy's COUNT wait for the first to commit and
// observe it.
//
// The workspace-owner, user and plan-limit lookups originally stayed on the
// pool (they are not written by the copy path), but that put pool waits
// inside a critical section that holds both workspace advisory locks — the
// BUG-2409 starvation shape, same as the attachment planner's reads. They now
// run on the transaction too. Visibility is unchanged (READ COMMITTED takes a
// fresh snapshot per statement either way), and every error here aborts the
// copy regardless of which connection the failed read used.
func (s *Store) CheckLimitTx(tx *sql.Tx, workspaceID, feature string) (*LimitResult, error) {
	return s.checkLimitOn(tx, workspaceID, feature)
}

// checkLimitOn is the shared body of CheckLimit / CheckLimitTx, parameterized
// over the executor EVERY read runs on (BUG-2409 — see CheckLimitTx).
func (s *Store) checkLimitOn(q Queryer, workspaceID, feature string) (*LimitResult, error) {
	// 1. Look up workspace → owner_id
	var ownerID string
	err := q.QueryRow(s.q(`SELECT owner_id FROM workspaces WHERE id = ?`), workspaceID).Scan(&ownerID)
	if err != nil {
		return nil, fmt.Errorf("check limit: get workspace owner: %w", err)
	}

	// 2. Look up user → plan, plan_overrides
	user, err := s.GetUserQ(q, ownerID)
	if err != nil {
		return nil, fmt.Errorf("check limit: get user: %w", err)
	}
	if user == nil {
		return nil, fmt.Errorf("check limit: owner not found")
	}

	// Self-hosted and pro always allowed
	// Expiry is enforced here, where entitlement is decided (BUG-3356).
	plan := user.EffectivePlan(time.Now())
	if plan == "self-hosted" || plan == "pro" {
		return &LimitResult{Allowed: true, Feature: feature, Limit: -1, Current: 0, Plan: plan}, nil
	}

	// 3. Resolve the limit for this feature
	limit := s.resolveLimitQ(q, plan, feature, user.PlanOverrides)

	// -1 = unlimited
	if limit < 0 {
		return &LimitResult{Allowed: true, Feature: feature, Limit: -1, Current: 0, Plan: plan}, nil
	}

	// 4. Get current count for the feature
	current, err := s.featureCountOn(q, workspaceID, ownerID, feature)
	if err != nil {
		return nil, fmt.Errorf("check limit: count %s: %w", feature, err)
	}

	return &LimitResult{
		Allowed: current < limit,
		Feature: feature,
		Limit:   limit,
		Current: current,
		Plan:    plan,
	}, nil
}

// CheckUserLimit checks a user-level limit (not workspace-scoped), such as
// total workspace count or total API tokens.
//
// This is an ADVISORY read on the pool: callers use it for an early refusal.
// It cannot hold a limit by itself, because nothing stops a concurrent writer
// between this count and the caller's insert (BUG-2808). The insert functions
// enforce the limit authoritatively when passed WithPlanLimit().
func (s *Store) CheckUserLimit(userID, feature string) (*LimitResult, error) {
	return s.checkUserLimitOn(s.db, userID, feature)
}

// checkUserLimitOn is CheckUserLimit against a caller-supplied executor, so a
// transaction can count on its own connection and after its own lock
// (BUG-2409 / BUG-2778: no pool reads inside a held transaction).
func (s *Store) checkUserLimitOn(q Queryer, userID, feature string) (*LimitResult, error) {
	user, err := s.GetUserQ(q, userID)
	if err != nil {
		return nil, fmt.Errorf("check user limit: get user: %w", err)
	}
	if user == nil {
		return nil, fmt.Errorf("check user limit: user not found")
	}

	// Expiry is enforced here, where entitlement is decided (BUG-3356).
	plan := user.EffectivePlan(time.Now())
	if plan == "self-hosted" || plan == "pro" {
		return &LimitResult{Allowed: true, Feature: feature, Limit: -1, Current: 0, Plan: plan}, nil
	}

	limit := s.resolveLimitQ(q, plan, feature, user.PlanOverrides)
	if limit < 0 {
		return &LimitResult{Allowed: true, Feature: feature, Limit: -1, Current: 0, Plan: plan}, nil
	}

	current, err := s.userFeatureCountOn(q, userID, feature)
	if err != nil {
		return nil, fmt.Errorf("check user limit: count %s: %w", feature, err)
	}

	return &LimitResult{
		Allowed: current < limit,
		Feature: feature,
		Limit:   limit,
		Current: current,
		Plan:    plan,
	}, nil
}

// PlanLimitError is the refusal an insert returns when WithPlanLimit() is set
// and the limit, counted under the insert's own lock, is already reached.
// Callers match it with errors.As and answer with the same 403 the advisory
// pre-check writes.
type PlanLimitError struct {
	Result LimitResult
}

func (e *PlanLimitError) Error() string {
	return fmt.Sprintf("plan limit reached for %s: %d of %d", e.Result.Feature, e.Result.Current, e.Result.Limit)
}

// MintOption configures a limited insert: CreateWorkspace, ImportWorkspace and
// CreateAPIToken (user-scoped), and CreateItem, AddWorkspaceMember and
// CreateWebhook (workspace-scoped).
type MintOption func(*mintOptions)

type mintOptions struct {
	planLimit bool
}

// WithPlanLimit makes the insert enforce the owner's plan limit for what it
// creates, in the same transaction as the write (BUG-2808). The server passes
// it in cloud mode only; self-hosted callers, migrations and the CLI's database
// import pass nothing and are unchanged.
func WithPlanLimit() MintOption {
	return func(o *mintOptions) { o.planLimit = true }
}

func resolveMintOptions(opts []MintOption) mintOptions {
	var o mintOptions
	for _, f := range opts {
		f(&o)
	}
	return o
}

// enforceUserLimitTx is the authoritative user-scoped check. It runs inside
// the insert's transaction, and in this order:
//
//  1. Lock the owner's users row. On Postgres this is FOR NO KEY UPDATE,
//     which every limited user-scoped insert takes, so two of them for one
//     owner run one after the other and the second counts the first's
//     committed row. FOR NO KEY UPDATE rather than FOR UPDATE: it is the
//     weakest lock that still conflicts with itself, and it does NOT conflict
//     with FOR KEY SHARE, the lock a foreign-key check takes on the parent
//     row. Every insert that references this user (items, comments, sessions,
//     activity) takes that FK lock, and FOR UPDATE would stall all of them
//     for as long as the limited insert is open. It does conflict with a
//     plain UPDATE of the row (last_active_at, plan changes), which waits
//     for the commit; that wait is the cost, and it is bounded by keeping
//     the lock late in the transaction.
//     On SQLite the transaction is already BEGIN IMMEDIATE (the DSN's
//     _txlock=immediate), which serialises every writer, and the row-locking
//     clause would be a syntax error there.
//  2. Count on the transaction, after the lock. Under READ COMMITTED each
//     statement takes a fresh snapshot, so this count sees everything
//     committed before the lock was granted.
//
// pendingOwn is the number of rows THIS transaction has already inserted that
// the count will include: 1 for the workspace mints, which insert first and
// then lock (a single lock order for both, and a short hold for the import);
// 0 for CreateAPIToken, which locks before it inserts.
//
// A missing owner is an error, not a pass: the caller asked for a limit and
// there is no plan to read it from.
func (s *Store) enforceUserLimitTx(tx *sql.Tx, userID, feature string, pendingOwn int) error {
	lock := `SELECT id FROM users WHERE id = ?`
	if s.dialect.Driver() == DriverPostgres {
		lock += ` FOR NO KEY UPDATE`
	}
	var id string
	if err := tx.QueryRow(s.q(lock), userID).Scan(&id); err != nil {
		return fmt.Errorf("plan limit: lock owner %s: %w", userID, err)
	}
	res, err := s.checkUserLimitOn(tx, userID, feature)
	if err != nil {
		return err
	}
	if res.Limit >= 0 {
		res.Current -= pendingOwn
		res.Allowed = res.Current < res.Limit
	}
	if !res.Allowed {
		return &PlanLimitError{Result: *res}
	}
	return nil
}

// enforceWorkspaceLimitTx is the authoritative workspace-scoped check. The
// caller must already hold a lock that every limited insert of this feature
// takes, and must not have inserted its own row yet: the count is taken as it
// stands, so a reached cap refuses.
//
// Which lock that is depends on the feature. Items count under the workspace
// seq lock (acquireWorkspaceSeqLock), which every item insert and restore
// already takes.
// Members and webhooks take acquirePlanLimitLock, a key used by nothing else.
// The reads run on the transaction, never the pool (BUG-2409).
func (s *Store) enforceWorkspaceLimitTx(tx *sql.Tx, workspaceID, feature string) error {
	res, err := s.checkLimitOn(tx, workspaceID, feature)
	if err != nil {
		return err
	}
	if !res.Allowed {
		return &PlanLimitError{Result: *res}
	}
	return nil
}

// acquirePlanLimitLock takes the Postgres advisory transaction lock that
// serialises the limited inserts of one workspace-scoped feature (BUG-2808).
// It is used for members and webhooks, whose inserts take no other workspace
// lock. It is deliberately NOT the workspace seq lock: these inserts do not
// touch item_number or seq, and sharing that key would queue them behind every
// item mutation in the workspace.
//
// LOCK ORDERING: callers take it before any other lock in the transaction, and
// no other code takes this key. So a transaction waiting on it holds nothing,
// and no cycle can pass through it. hashtext is 32-bit, so the key can collide
// with any other advisory key (a seq key, another namespace's key, another
// plan-limit key). The worst case is then a spurious wait: a cycle would also
// need that key's holder to wait on a lock this insert holds, and the insert
// holds only FK key-share locks and its own new rows' index entries, which no
// such holder's locks conflict with. The full reasoning, with the list of
// holders, is on BUG-2808's trail (checkpoints 7 and 11).
//
// On SQLite, BEGIN IMMEDIATE already serialises every writer, and this is a
// no-op.
func (s *Store) acquirePlanLimitLock(tx *sql.Tx, workspaceID, feature string) error {
	if s.dialect.Driver() != DriverPostgres {
		return nil
	}
	if _, err := tx.Exec("SELECT pg_advisory_xact_lock(hashtext('pad:plan-limit:' || $1 || ':' || $2))", feature, workspaceID); err != nil {
		return fmt.Errorf("acquire plan limit lock: %w", err)
	}
	return nil
}

// resolveLimit resolves the limit for a feature using the three-tier resolution:
// user overrides → DB-stored plan defaults → hardcoded fallback.
func (s *Store) resolveLimit(plan, feature, overridesJSON string) int {
	return s.resolveLimitQ(s.db, plan, feature, overridesJSON)
}

// resolveLimitQ is resolveLimit parameterized over its executor (see
// Queryer) — the plan-limit platform setting is a read, and CheckLimitTx
// must not touch the pool (BUG-2409).
func (s *Store) resolveLimitQ(q Queryer, plan, feature, overridesJSON string) int {
	// 1. Check per-user overrides
	if overridesJSON != "" {
		var overrides map[string]int
		if err := json.Unmarshal([]byte(overridesJSON), &overrides); err == nil {
			if v, ok := overrides[feature]; ok {
				return v
			}
		}
	}

	// 2. Check DB-stored plan defaults
	settingKey := "plan_limits_" + plan + "_" + feature
	if val, err := s.GetPlatformSettingQ(q, settingKey); err == nil && val != "" {
		if v, err := strconv.Atoi(val); err == nil {
			return v
		}
	}

	// 3. Hardcoded fallback
	return hardcodedLimit(plan, feature)
}

// featureCountOn returns the current count for a workspace-scoped feature,
// parameterized over the query surface so the same COUNTs serve both
// CheckLimit (the pool) and CheckLimitTx (a caller's transaction).
func (s *Store) featureCountOn(q rowQueryer, workspaceID, ownerID, feature string) (int, error) {
	var count int
	var err error

	switch feature {
	case "items_per_workspace":
		err = q.QueryRow(s.q(`SELECT COUNT(*) FROM items WHERE workspace_id = ? AND deleted_at IS NULL`), workspaceID).Scan(&count)
	case "members_per_workspace":
		// Bots are a seat only if the Q2 decision says so (TASK-3392).
		if appPrincipalMemberPolicy.CountsAsSeat {
			err = q.QueryRow(s.q(`SELECT COUNT(*) FROM workspace_members WHERE workspace_id = ?`), workspaceID).Scan(&count)
		} else {
			err = q.QueryRow(s.q(`SELECT COUNT(*) FROM workspace_members wm JOIN users u ON u.id = wm.user_id
				WHERE wm.workspace_id = ? AND u.kind = 'human'`), workspaceID).Scan(&count)
		}
	case "webhooks":
		err = q.QueryRow(s.q(`SELECT COUNT(*) FROM webhooks WHERE workspace_id = ?`), workspaceID).Scan(&count)
	default:
		return 0, fmt.Errorf("unknown workspace feature: %s", feature)
	}

	return count, err
}

// userFeatureCountOn returns the current count for a user-scoped feature,
// read on the given executor.
func (s *Store) userFeatureCountOn(q rowQueryer, userID, feature string) (int, error) {
	var count int
	var err error

	switch feature {
	case "workspaces":
		// Note: this count includes soft-deleted workspaces by design — see IDEA-1611
		// in docapp for the open question on whether this should change post-MVBP.
		err = q.QueryRow(s.q(`SELECT COUNT(*) FROM workspaces WHERE owner_id = ?`), userID).Scan(&count)
	case "api_tokens":
		err = q.QueryRow(s.q(`SELECT COUNT(*) FROM api_tokens WHERE user_id = ?`), userID).Scan(&count)
	default:
		return 0, fmt.Errorf("unknown user feature: %s", feature)
	}

	return count, err
}

// hardcodedLimit returns the hardcoded fallback limit for a feature on a given plan.
func hardcodedLimit(plan, feature string) int {
	var limits PlanLimits
	switch plan {
	case "pro":
		limits = DefaultProLimits
	default:
		limits = DefaultFreeLimits
	}

	switch feature {
	case "workspaces":
		return limits.Workspaces
	case "items_per_workspace":
		return limits.ItemsPerWorkspace
	case "members_per_workspace":
		return limits.MembersPerWorkspace
	case "api_tokens":
		return limits.APITokens
	case "storage_bytes":
		return limits.StorageBytes
	case "webhooks":
		return limits.Webhooks
	case "automated_backups":
		return limits.AutomatedBackups
	default:
		slog.Warn("unknown plan limit feature — denying by default", "feature", feature, "plan", plan)
		return 0 // Unknown features denied (fail closed)
	}
}

// SeedPlanLimits writes the default plan limits to platform_settings if they
// don't already exist. Called on server startup. Idempotent — existing values
// are not overwritten, so admin changes via the UI are preserved.
func (s *Store) SeedPlanLimits() error {
	plans := map[string]PlanLimits{
		"free": DefaultFreeLimits,
		"pro":  DefaultProLimits,
	}

	features := []string{
		"workspaces", "items_per_workspace", "members_per_workspace",
		"api_tokens", "storage_bytes", "webhooks", "automated_backups",
	}

	for planName, limits := range plans {
		limitsMap := map[string]int{
			"workspaces":            limits.Workspaces,
			"items_per_workspace":   limits.ItemsPerWorkspace,
			"members_per_workspace": limits.MembersPerWorkspace,
			"api_tokens":            limits.APITokens,
			"storage_bytes":         limits.StorageBytes,
			"webhooks":              limits.Webhooks,
			"automated_backups":     limits.AutomatedBackups,
		}

		for _, feature := range features {
			key := "plan_limits_" + planName + "_" + feature
			existing, err := s.GetPlatformSetting(key)
			if err != nil {
				return fmt.Errorf("seed plan limits: check %s: %w", key, err)
			}
			if existing != "" {
				continue // Already set — don't overwrite admin changes
			}
			if err := s.SetPlatformSetting(key, strconv.Itoa(limitsMap[feature])); err != nil {
				return fmt.Errorf("seed plan limits: set %s: %w", key, err)
			}
		}
	}

	return nil
}

// Plan sources: who set a user's plan (TASK-3295, PLAN-3291 DR-6). 'apple'
// and 'google' are reserved for store billing and not accepted yet.
const (
	PlanSourceManual = "manual"
	PlanSourceStripe = "stripe"
)

// ValidPlanSource reports whether src is a plan source SetUserPlan accepts.
func ValidPlanSource(src string) bool {
	return src == PlanSourceManual || src == PlanSourceStripe
}

// PlanWrite is one write to a user's plan. Source is required: every caller
// says who is setting the plan, because the lowering rule is decided by it.
type PlanWrite struct {
	Plan      string
	ExpiresAt string
	Source    string
	// Force applies the write even when it would lower the plan from a
	// source other than the one that set it. Only an operator's explicit
	// choice (the admin user update) sets it.
	Force bool
	// Revision orders writes derived from Stripe (BUG-3356): when it is
	// above zero the write applies only if it is greater than the stored
	// plan_revision, which it then replaces. Zero is no revision (a manual
	// write, or a sidecar that predates it) and leaves the stored one alone.
	Revision int64
	// SubscriptionID is the Stripe subscription the plan was derived from,
	// stored alongside a revisioned write.
	SubscriptionID string
}

// Reasons a PlanWrite was refused.
const (
	// PlanRefusedSource: it would lower the plan from a source other than
	// the one that set it (PLAN-3291 DR-6).
	PlanRefusedSource = "source"
	// PlanRefusedStaleRevision: its revision is not newer than the one the
	// stored plan came from (BUG-3356).
	PlanRefusedStaleRevision = "stale_revision"
)

// PlanWriteResult says whether a PlanWrite applied, and the plan and source
// the user holds after it, whichever way it went.
type PlanWriteResult struct {
	Applied bool
	Plan    string
	Source  string
	// Reason is set when Applied is false: PlanRefusedSource or
	// PlanRefusedStaleRevision.
	Reason string
}

// SetUserPlan writes a user's billing plan under the plan-source rule
// (PLAN-3291 DR-6), enforced here rather than by callers:
//
//   - a write that LOWERS the plan (new plan free, current plan neither free
//     nor blank) applies only if its source is the current plan_source, or it
//     is forced;
//   - every other write applies and takes plan_source over.
//
// So a Stripe cancellation cannot clobber a plan an operator granted, and an
// operator can still lower anything by forcing. The rule is the UPDATE's own
// WHERE clause, so it is decided atomically against the row as it stands. A
// refused write changes nothing, plan_expires_at included, and is not an
// error: Applied is false.
//
// A write carrying a Revision must also be newer than the stored one
// (BUG-3356), in the same WHERE clause, so two Stripe-derived writes that
// arrive out of order cannot leave the older one standing.
func (s *Store) SetUserPlan(userID string, w PlanWrite) (PlanWriteResult, error) {
	if !ValidPlanSource(w.Source) {
		return PlanWriteResult{}, fmt.Errorf("set user plan: invalid plan source %q", w.Source)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return PlanWriteResult{}, fmt.Errorf("set user plan: %w", err)
	}
	defer tx.Rollback()

	force := 0 // bound as an int: a bare boolean placeholder is dialect-sensitive
	if w.Force {
		force = 1
	}
	if w.Revision < 0 {
		return PlanWriteResult{}, fmt.Errorf("set user plan: negative revision %d", w.Revision)
	}
	if w.Revision > 0 && w.Source != PlanSourceStripe {
		// A manual write fences with the current time; a revision of its own
		// would undercut the fence.
		return PlanWriteResult{}, fmt.Errorf("set user plan: a revision is only valid on a stripe write")
	}

	// Read the row under its lock first (Postgres FOR UPDATE; SQLite's
	// transaction already holds the write lock), so a refusal's reason is
	// decided from the same row version the UPDATE below is judged against.
	lockSQL := `SELECT plan, plan_source, plan_revision FROM users WHERE id = ?`
	if s.dialect.Driver() == DriverPostgres {
		lockSQL += ` FOR UPDATE`
	}
	var heldPlan, heldSource string
	var heldRevision int64
	err = tx.QueryRow(s.q(lockSQL), userID).Scan(&heldPlan, &heldSource, &heldRevision)
	if err == sql.ErrNoRows {
		return PlanWriteResult{}, fmt.Errorf("set user plan: user %s not found", userID)
	}
	if err != nil {
		return PlanWriteResult{}, fmt.Errorf("set user plan: %w", err)
	}

	// A manual write fences Stripe writes already in flight (BUG-3356): it
	// raises the stored revision to now, in the same unix-ms the sidecar's
	// fetch time uses, so a fetch taken before the operator's decision
	// cannot land after it and undo it. A later fetch still applies, under
	// the source rule. This compares two clocks, pad's and the sidecar's,
	// which run on the same host today; skew between them narrows or
	// widens the fence by that much.
	var fence int64
	if w.Source == PlanSourceManual {
		fence = time.Now().UnixMilli()
	}

	// Revisions are unix milliseconds, beyond int4: every placeholder they
	// bind to is cast, or Postgres infers int4 from the literal it is
	// compared with and refuses the value.
	res, err := tx.Exec(s.q(`
		UPDATE users SET plan = ?, plan_expires_at = ?, plan_source = ?, updated_at = ?,
		       plan_revision = CASE
		           WHEN CAST(? AS BIGINT) > 0 THEN CAST(? AS BIGINT)
		           WHEN CAST(? AS BIGINT) > plan_revision THEN CAST(? AS BIGINT)
		           ELSE plan_revision END,
		       plan_subscription_id = CASE WHEN CAST(? AS BIGINT) > 0 THEN ? ELSE plan_subscription_id END
		WHERE id = ? AND (? = 1 OR ? <> 'free' OR plan IN ('', 'free') OR plan_source = ?)
		  AND (CAST(? AS BIGINT) = 0 OR CAST(? AS BIGINT) > plan_revision)`),
		w.Plan, w.ExpiresAt, w.Source, now(),
		w.Revision, w.Revision, fence, fence,
		w.Revision, w.SubscriptionID,
		userID, force, w.Plan, w.Source,
		w.Revision, w.Revision)
	if err != nil {
		return PlanWriteResult{}, fmt.Errorf("set user plan: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return PlanWriteResult{}, fmt.Errorf("set user plan: %w", err)
	}

	out := PlanWriteResult{Applied: n > 0, Plan: heldPlan, Source: heldSource}
	if out.Applied {
		out.Plan, out.Source = w.Plan, w.Source
	} else {
		out.Reason = PlanRefusedSource
		if w.Revision > 0 && w.Revision <= heldRevision {
			out.Reason = PlanRefusedStaleRevision
		}
	}
	if err := tx.Commit(); err != nil {
		return PlanWriteResult{}, fmt.Errorf("set user plan: %w", err)
	}
	return out, nil
}

// SetUserPlanOverrides updates per-user limit overrides (JSON string).
func (s *Store) SetUserPlanOverrides(userID, overridesJSON string) error {
	_, err := s.db.Exec(s.q(`UPDATE users SET plan_overrides = ?, updated_at = ? WHERE id = ?`),
		overridesJSON, now(), userID)
	if err != nil {
		return fmt.Errorf("set user plan overrides: %w", err)
	}
	return nil
}

// BackfillUserPlans sets the plan for all users that have an empty or default plan.
// In cloud mode, call with "free" to ensure all users have a plan set.
// In self-hosted mode, call with "self-hosted" to remove all limits.
//
// A plan it writes is the system's, not Stripe's, so it takes plan_source
// over as manual, like any other write that does not lower (TASK-3295).
// Otherwise a free user carrying a stripe source would become self-hosted
// still labelled stripe.
func (s *Store) BackfillUserPlans(targetPlan string) error {
	var err error
	if targetPlan == "self-hosted" {
		// Self-hosted: override free and empty plans to self-hosted
		_, err = s.db.Exec(s.q(`UPDATE users SET plan = ?, plan_source = ?, updated_at = ? WHERE plan IN ('', 'free')`),
			targetPlan, PlanSourceManual, now())
	} else {
		// Cloud: only fill in empty plans, don't override existing values
		_, err = s.db.Exec(s.q(`UPDATE users SET plan = ?, plan_source = ?, updated_at = ? WHERE plan = ''`),
			targetPlan, PlanSourceManual, now())
	}
	if err != nil {
		return fmt.Errorf("backfill user plans: %w", err)
	}
	return nil
}

// SetUserStripeCustomerID stores the Stripe customer ID for a user.
func (s *Store) SetUserStripeCustomerID(userID, customerID string) error {
	_, err := s.db.Exec(s.q(`UPDATE users SET stripe_customer_id = ?, updated_at = ? WHERE id = ?`),
		customerID, now(), userID)
	if err != nil {
		return fmt.Errorf("set stripe customer id: %w", err)
	}
	return nil
}

// GetUserByStripeCustomerID retrieves a user by their Stripe customer ID.
// Returns nil if no user is found with the given customer ID.
func (s *Store) GetUserByStripeCustomerID(customerID string) (*models.User, error) {
	customerID = strings.TrimSpace(customerID)
	if customerID == "" {
		return nil, nil
	}
	u, err := scanUser(s.db.QueryRow(s.q(`SELECT `+userColumns+` FROM users WHERE stripe_customer_id = ?`), customerID))
	if err != nil {
		return nil, fmt.Errorf("get user by stripe customer id: %w", err)
	}
	if err := s.decryptUserTOTP(u); err != nil {
		return nil, err
	}
	return u, nil
}
