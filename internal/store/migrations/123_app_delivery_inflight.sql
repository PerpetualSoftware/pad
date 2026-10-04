-- Migration 123: the app webhook delivery fence (SPEC-6 U10b, DOC-3371 §5;
-- TASK-3408).
--
-- One row per app webhook ATTEMPT in flight. Admission inserts it in the same
-- transaction that holds the install row and checks the install is active;
-- the attempt deletes it when it ends. Disable phase 2 waits until an install
-- has no unexpired row, so no attempt admitted before phase 1 is still
-- writing when the owner sees "disabled".
--
-- expires_at is DATABASE time, written by the INSERT and compared in SQL,
-- never the application host's clock. ISO-8601 UTC text from strftime, so it
-- compares lexically. A crashed attempt's row simply expires (12 s, two
-- seconds past the attempt's 10 s deadline, which starts before admission).
CREATE TABLE IF NOT EXISTS app_delivery_inflight (
    delivery_id TEXT PRIMARY KEY,
    install_id  TEXT NOT NULL REFERENCES app_installs(id) ON DELETE CASCADE,
    expires_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_app_delivery_inflight_install ON app_delivery_inflight (install_id, expires_at);

-- deliver_from: the first instant an app hook may receive events (codex r5
-- on U10b). Set when the app is handed its secret (redeem), when a disabled
-- install is re-enabled, and when an upgrade changes the subscriptions.
-- Admission refuses an event that occurred earlier, so an event the app
-- skipped while held, disabled or unsubscribed is never delivered later
-- because an owner hook's failure kept its outbox row pending.
ALTER TABLE webhooks ADD COLUMN deliver_from TEXT;
