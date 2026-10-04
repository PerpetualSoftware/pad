-- Migration 097: the app webhook delivery fence (SPEC-6 U10b, TASK-3408).
-- Mirrors SQLite migration 123; see it for the rationale. expires_at is
-- timestamptz, set from now() and compared against now() in SQL.
CREATE TABLE IF NOT EXISTS app_delivery_inflight (
    delivery_id TEXT PRIMARY KEY,
    install_id  TEXT NOT NULL REFERENCES app_installs(id) ON DELETE CASCADE,
    expires_at  TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_app_delivery_inflight_install ON app_delivery_inflight (install_id, expires_at);
