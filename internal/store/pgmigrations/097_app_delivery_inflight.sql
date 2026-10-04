-- Migration 097: the app webhook delivery fence (SPEC-6 U10b, TASK-3408).
-- Mirrors SQLite migration 123; see it for the rationale. expires_at is
-- timestamptz, set from now() and compared against now() in SQL.
CREATE TABLE IF NOT EXISTS app_delivery_inflight (
    delivery_id TEXT PRIMARY KEY,
    install_id  TEXT NOT NULL REFERENCES app_installs(id) ON DELETE CASCADE,
    expires_at  TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_app_delivery_inflight_install ON app_delivery_inflight (install_id, expires_at);

-- deliver_from: the first instant an app hook may receive events (codex r5
-- on U10b). Set when the app is handed its secret (redeem), when a disabled
-- install is re-enabled, and when an upgrade changes the subscriptions.
-- Admission refuses an event that occurred earlier, so an event the app
-- skipped while held, disabled or unsubscribed is never delivered later
-- because an owner hook's failure kept its outbox row pending.
ALTER TABLE webhooks ADD COLUMN IF NOT EXISTS deliver_from TEXT;
-- Hooks released under migration 122 (U10a) start delivering from the
-- moment their secret was handed out. Without this they would read NULL and
-- refuse every event until the next redeem (codex r6 on U10b).
UPDATE webhooks SET deliver_from = secret_delivered_at
 WHERE app_install_id IS NOT NULL AND secret_delivered_at IS NOT NULL AND deliver_from IS NULL;
