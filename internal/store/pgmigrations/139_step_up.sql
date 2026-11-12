ALTER TABLE sessions ADD COLUMN IF NOT EXISTS step_up_until TEXT;

CREATE TABLE IF NOT EXISTS step_up_codes (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    session_id TEXT NOT NULL,
    code_hash TEXT NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0,
    expires_at TEXT NOT NULL,
    used_at TEXT,
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_step_up_codes_user_created ON step_up_codes(user_id, created_at);
