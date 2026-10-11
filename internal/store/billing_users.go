package store

import "fmt"

// BillingUser is one user the Pad Cloud reconcile sweep re-syncs (TASK-3564):
// someone pad holds a Stripe link for, or whose plan Stripe set.
type BillingUser struct {
	UserID           string `json:"user_id"`
	StripeCustomerID string `json:"stripe_customer_id"`
	Plan             string `json:"plan"`
	PlanSource       string `json:"plan_source"`
	PlanExpiresAt    string `json:"plan_expires_at"`
}

// ListBillingUsers pages through human users with a stored Stripe customer id
// or a stripe-sourced plan, ordered by id: the users after afterID, at most
// limit of them. Both conditions, because each finds a case the other misses:
// a link pad holds for a customer Stripe no longer bills, and a stripe plan
// whose link was cleared. App principals never pay and are excluded.
func (s *Store) ListBillingUsers(afterID string, limit int) ([]BillingUser, error) {
	if limit < 1 {
		return nil, fmt.Errorf("list billing users: limit must be positive")
	}
	rows, err := s.db.Query(s.q(`
		SELECT id, COALESCE(stripe_customer_id, ''), COALESCE(plan, ''), COALESCE(plan_source, ''), COALESCE(plan_expires_at, '')
		FROM users
		WHERE kind = 'human'
		  AND (COALESCE(stripe_customer_id, '') <> '' OR plan_source = ?)
		  AND id > ?
		ORDER BY id ASC
		LIMIT ?`), PlanSourceStripe, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("list billing users: %w", err)
	}
	defer rows.Close()
	out := []BillingUser{}
	for rows.Next() {
		var u BillingUser
		if err := rows.Scan(&u.UserID, &u.StripeCustomerID, &u.Plan, &u.PlanSource, &u.PlanExpiresAt); err != nil {
			return nil, fmt.Errorf("list billing users (scan): %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
