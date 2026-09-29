-- Migration 101: where a user's plan came from (TASK-3295, PLAN-3291 DR-6).
--
-- 'manual' (an operator, or any caller that names no source) or 'stripe'
-- (the pad-cloud sidecar); 'apple' / 'google' are reserved for store billing.
-- SetUserPlan enforces the rule in its own UPDATE: a write that lowers the plan
-- to free applies only when its source is the one that set the current plan,
-- or the plan is already free; any other write applies and takes the source
-- over. No CHECK constraint: the store validates the vocabulary, and widening
-- a CHECK on SQLite would cost a table rebuild.
ALTER TABLE users ADD COLUMN plan_source TEXT NOT NULL DEFAULT 'manual';

-- A pro user who already has a Stripe customer got pro from Stripe. Leaving
-- them 'manual' would make their Stripe cancellation (a 'free' from source
-- stripe) a refused lowering, keeping them on pro after they stopped paying.
UPDATE users SET plan_source = 'stripe'
WHERE plan = 'pro' AND COALESCE(stripe_customer_id, '') <> '';
