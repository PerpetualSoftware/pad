-- Migration 124: per-endpoint delivery state for app webhooks (SPEC-6 U10c,
-- DOC-3371 §5; TASK-3408).
--
-- One row per (outbox event, app hook) the drain has decided anything about.
-- A TERMINAL row (delivered, permanent, refused, skipped, dropped) is never
-- retried, so a retry goes only to endpoints still owed, and an event the
-- app was refused (held, disabled, not visible) stays refused. A PENDING row
-- (transient, deferred, rate_limited) keeps the event owed until it turns
-- terminal or is dropped after appWebhookDropAfter.
--
-- Both foreign keys cascade: the delivery state belongs to its outbox row
-- (retention deletes them together) and to its hook (uninstall deletes it).
-- The outbox's own no-foreign-key rule is about its SUBJECTS, not this.
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

-- Deliveries dropped undelivered after appWebhookDropAfter, shown to the
-- owner on the install.
ALTER TABLE webhooks ADD COLUMN dropped_count INTEGER NOT NULL DEFAULT 0;
