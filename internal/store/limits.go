package store

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/xarmian/pad/internal/models"
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
	Workspaces:          5,
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
	Limit   int    `json:"limit"`   // -1 means unlimited
	Current int    `json:"current"`
	Plan    string `json:"plan"`
}

// CheckLimit checks whether a workspace operation is allowed under the
// owner's plan. Resolution order:
//  1. User plan_overrides[feature] — per-user override (if set)
//  2. Platform plan_limits[plan][feature] — DB-stored defaults for the tier
//  3. Hardcoded fallback — safety net if DB config is missing
func (s *Store) CheckLimit(workspaceID, feature string) (*LimitResult, error) {
	// 1. Look up workspace → owner_id
	var ownerID string
	err := s.db.QueryRow(s.q(`SELECT owner_id FROM workspaces WHERE id = ?`), workspaceID).Scan(&ownerID)
	if err != nil {
		return nil, fmt.Errorf("check limit: get workspace owner: %w", err)
	}

	// 2. Look up user → plan, plan_overrides
	user, err := s.GetUser(ownerID)
	if err != nil {
		return nil, fmt.Errorf("check limit: get user: %w", err)
	}
	if user == nil {
		return nil, fmt.Errorf("check limit: owner not found")
	}

	// Self-hosted and pro always allowed
	plan := user.Plan
	if plan == "" {
		plan = "free"
	}
	if plan == "self-hosted" || plan == "pro" {
		return &LimitResult{Allowed: true, Feature: feature, Limit: -1, Current: 0, Plan: plan}, nil
	}

	// 3. Resolve the limit for this feature
	limit := s.resolveLimit(plan, feature, user.PlanOverrides)

	// -1 = unlimited
	if limit < 0 {
		return &LimitResult{Allowed: true, Feature: feature, Limit: -1, Current: 0, Plan: plan}, nil
	}

	// 4. Get current count for the feature
	current, err := s.featureCount(workspaceID, ownerID, feature)
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
func (s *Store) CheckUserLimit(userID, feature string) (*LimitResult, error) {
	user, err := s.GetUser(userID)
	if err != nil {
		return nil, fmt.Errorf("check user limit: get user: %w", err)
	}
	if user == nil {
		return nil, fmt.Errorf("check user limit: user not found")
	}

	plan := user.Plan
	if plan == "" {
		plan = "free"
	}
	if plan == "self-hosted" || plan == "pro" {
		return &LimitResult{Allowed: true, Feature: feature, Limit: -1, Current: 0, Plan: plan}, nil
	}

	limit := s.resolveLimit(plan, feature, user.PlanOverrides)
	if limit < 0 {
		return &LimitResult{Allowed: true, Feature: feature, Limit: -1, Current: 0, Plan: plan}, nil
	}

	current, err := s.userFeatureCount(userID, feature)
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

// resolveLimit resolves the limit for a feature using the three-tier resolution:
// user overrides → DB-stored plan defaults → hardcoded fallback.
func (s *Store) resolveLimit(plan, feature, overridesJSON string) int {
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
	if val, err := s.GetPlatformSetting(settingKey); err == nil && val != "" {
		if v, err := strconv.Atoi(val); err == nil {
			return v
		}
	}

	// 3. Hardcoded fallback
	return hardcodedLimit(plan, feature)
}

// featureCount returns the current count for a workspace-scoped feature.
func (s *Store) featureCount(workspaceID, ownerID, feature string) (int, error) {
	var count int
	var err error

	switch feature {
	case "items_per_workspace":
		err = s.db.QueryRow(s.q(`SELECT COUNT(*) FROM items WHERE workspace_id = ? AND deleted_at IS NULL`), workspaceID).Scan(&count)
	case "members_per_workspace":
		err = s.db.QueryRow(s.q(`SELECT COUNT(*) FROM workspace_members WHERE workspace_id = ?`), workspaceID).Scan(&count)
	case "webhooks":
		err = s.db.QueryRow(s.q(`SELECT COUNT(*) FROM webhooks WHERE workspace_id = ?`), workspaceID).Scan(&count)
	default:
		return 0, fmt.Errorf("unknown workspace feature: %s", feature)
	}

	return count, err
}

// userFeatureCount returns the current count for a user-scoped feature.
func (s *Store) userFeatureCount(userID, feature string) (int, error) {
	var count int
	var err error

	switch feature {
	case "workspaces":
		err = s.db.QueryRow(s.q(`SELECT COUNT(*) FROM workspaces WHERE owner_id = ?`), userID).Scan(&count)
	case "api_tokens":
		err = s.db.QueryRow(s.q(`SELECT COUNT(*) FROM api_tokens WHERE user_id = ?`), userID).Scan(&count)
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

// SetUserPlan updates a user's billing plan.
func (s *Store) SetUserPlan(userID, plan, expiresAt string) error {
	_, err := s.db.Exec(s.q(`UPDATE users SET plan = ?, plan_expires_at = ?, updated_at = ? WHERE id = ?`),
		plan, expiresAt, now(), userID)
	if err != nil {
		return fmt.Errorf("set user plan: %w", err)
	}
	return nil
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
func (s *Store) BackfillUserPlans(targetPlan string) error {
	var err error
	if targetPlan == "self-hosted" {
		// Self-hosted: override free and empty plans to self-hosted
		_, err = s.db.Exec(s.q(`UPDATE users SET plan = ?, updated_at = ? WHERE plan IN ('', 'free')`),
			targetPlan, now())
	} else {
		// Cloud: only fill in empty plans, don't override existing values
		_, err = s.db.Exec(s.q(`UPDATE users SET plan = ?, updated_at = ? WHERE plan = ''`),
			targetPlan, now())
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
