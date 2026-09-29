-- Migration 075 (Postgres): where a user's plan came from (TASK-3295, PLAN-3291 DR-6).
-- Postgres counterpart to migrations/101_user_plan_source.sql; the rationale is there.
ALTER TABLE users ADD COLUMN IF NOT EXISTS plan_source TEXT NOT NULL DEFAULT 'manual';

UPDATE users SET plan_source = 'stripe'
WHERE plan = 'pro' AND COALESCE(stripe_customer_id, '') <> '';
