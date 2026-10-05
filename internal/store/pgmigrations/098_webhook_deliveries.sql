-- Migration 098: per-endpoint delivery state for app webhooks (SPEC-6 U10c,
-- TASK-3408). Mirrors SQLite migration 124; see it for the rationale.
CREATE TABLE IF NOT EXISTS webhook_deliveries (
    outbox_event_id TEXT NOT NULL REFERENCES event_outbox(id) ON DELETE CASCADE,
    webhook_id      TEXT NOT NULL REFERENCES webhooks(id) ON DELETE CASCADE,
    status          TEXT NOT NULL,
    attempts        INTEGER NOT NULL DEFAULT 0,
    last_error      TEXT,
    updated_at      TEXT NOT NULL,
    PRIMARY KEY (outbox_event_id, webhook_id)
);
CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_hook ON webhook_deliveries (webhook_id, status);
ALTER TABLE webhooks ADD COLUMN IF NOT EXISTS dropped_count INTEGER NOT NULL DEFAULT 0;
