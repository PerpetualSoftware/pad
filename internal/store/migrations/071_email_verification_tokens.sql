-- Email verification tokens (time-limited, single-use, stored as SHA-256 hashes).
-- Clones the password_reset_tokens shape (migration 015). PLAN-1933 Wave 2 / TASK-1936.
-- Deltas vs password_resets: 24h TTL and `padver_` prefix live in the store layer
-- (email_verification.go), not the DDL — the table shape is identical.
CREATE TABLE IF NOT EXISTS email_verification_tokens (
    id          TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL REFERENCES users(id),
    token_hash  TEXT NOT NULL,
    expires_at  TEXT NOT NULL,
    used_at     TEXT,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE INDEX IF NOT EXISTS idx_email_verification_tokens_token_hash ON email_verification_tokens(token_hash);
CREATE INDEX IF NOT EXISTS idx_email_verification_tokens_user_id ON email_verification_tokens(user_id);
