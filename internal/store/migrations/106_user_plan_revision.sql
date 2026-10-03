-- Migration 106: order Stripe-derived plan writes (BUG-3356). The sidecar
-- re-fetches the customer's subscriptions before each write and sends the
-- fetch time as a monotonic revision; SetUserPlan applies a stripe write
-- only when its revision is newer than the one stored, so an older fetch
-- landing late cannot restore an entitlement a newer one removed.
-- 0 = no revision recorded (every row before this, and every manual write).
ALTER TABLE users ADD COLUMN plan_revision INTEGER NOT NULL DEFAULT 0;
-- The Stripe subscription the stored plan was derived from ('' = none).
ALTER TABLE users ADD COLUMN plan_subscription_id TEXT NOT NULL DEFAULT '';
