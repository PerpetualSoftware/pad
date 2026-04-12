-- Index stripe_customer_id for fast lookups during Stripe webhook processing.
-- Only non-empty values need to be indexed (most users won't have a Stripe customer ID).
CREATE INDEX IF NOT EXISTS idx_users_stripe_customer_id ON users(stripe_customer_id) WHERE stripe_customer_id != '';
