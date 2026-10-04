-- Migration 095: the consented access of a delegated app grant (SPEC-6 U5b,
-- TASK-3399). Mirrors SQLite migration 121; see it for the rationale.
ALTER TABLE app_token_bindings ADD COLUMN IF NOT EXISTS delegated_access TEXT;
ALTER TABLE app_token_bindings ADD COLUMN IF NOT EXISTS delegated_user_id TEXT REFERENCES users(id) ON DELETE CASCADE;
CREATE INDEX IF NOT EXISTS idx_app_token_bindings_delegated_user ON app_token_bindings (delegated_user_id);
ALTER TABLE app_token_bindings ADD COLUMN IF NOT EXISTS delegated_credential_epoch BIGINT;
ALTER TABLE app_token_bindings ADD COLUMN IF NOT EXISTS delegated_member_since TEXT;
ALTER TABLE app_token_bindings ADD COLUMN IF NOT EXISTS revoked_at TEXT;
