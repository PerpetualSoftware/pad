-- Add plan/billing columns to users table for cloud mode billing.
-- Plan is account-level: workspaces inherit from their owner's plan.
ALTER TABLE users ADD COLUMN plan TEXT NOT NULL DEFAULT 'free';
ALTER TABLE users ADD COLUMN plan_expires_at TEXT DEFAULT '';
ALTER TABLE users ADD COLUMN stripe_customer_id TEXT DEFAULT '';
ALTER TABLE users ADD COLUMN plan_overrides TEXT DEFAULT '';
