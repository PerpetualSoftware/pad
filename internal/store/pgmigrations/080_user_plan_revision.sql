-- Migration 080: order Stripe-derived plan writes (BUG-3356). Mirrors SQLite
-- migration 106; see it for the rationale.
ALTER TABLE users ADD COLUMN IF NOT EXISTS plan_revision BIGINT NOT NULL DEFAULT 0;
ALTER TABLE users ADD COLUMN IF NOT EXISTS plan_subscription_id TEXT NOT NULL DEFAULT '';
