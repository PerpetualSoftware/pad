-- Migration 096: app-owned webhooks (SPEC-6 U10a, TASK-3408).
-- Mirrors SQLite migration 122; see it for the rationale.
ALTER TABLE webhooks ADD COLUMN IF NOT EXISTS app_install_id TEXT REFERENCES app_installs(id) ON DELETE CASCADE;
ALTER TABLE webhooks ADD COLUMN IF NOT EXISTS secret_delivered_at TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS idx_webhooks_app_install ON webhooks (app_install_id) WHERE app_install_id IS NOT NULL;
