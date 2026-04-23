-- Persist Stripe webhook idempotency across restarts (TASK-696).
-- pad-cloud previously held processed event IDs in RAM, so a restart would
-- re-process every webhook Stripe retries within its 72h window. The sidecar
-- now calls POST /api/v1/admin/stripe-event-processed on pad; pad is the
-- source of truth for whether an event ID has already been handled.
CREATE TABLE IF NOT EXISTS stripe_processed_events (
    event_id     TEXT PRIMARY KEY,
    processed_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_stripe_processed_events_processed_at
    ON stripe_processed_events(processed_at);
