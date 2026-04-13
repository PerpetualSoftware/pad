-- CLI auth sessions: browser-based CLI login flow.
-- The CLI creates a pending session, the user approves it in the browser,
-- and the CLI polls until a token is available.
CREATE TABLE IF NOT EXISTS cli_auth_sessions (
    code       TEXT PRIMARY KEY,
    status     TEXT NOT NULL DEFAULT 'pending',  -- pending, approved, expired
    token      TEXT,                              -- session token, set on approval
    user_id    TEXT,                              -- set on approval
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_cli_auth_sessions_expires ON cli_auth_sessions(expires_at);
