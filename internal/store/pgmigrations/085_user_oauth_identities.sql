-- Migration 085: provider account subjects (TASK-3351).
-- Mirrors SQLite migration 111; see it for the rationale.
CREATE TABLE IF NOT EXISTS user_oauth_identities (
    provider   TEXT NOT NULL,
    subject    TEXT NOT NULL,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TEXT NOT NULL,
    PRIMARY KEY (provider, subject)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_user_oauth_identities_user_provider
    ON user_oauth_identities (user_id, provider);
