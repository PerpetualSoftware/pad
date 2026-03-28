-- User management: users, sessions, workspace members

CREATE TABLE IF NOT EXISTS users (
    id            TEXT PRIMARY KEY,
    email         TEXT NOT NULL UNIQUE,
    name          TEXT NOT NULL,
    password_hash TEXT NOT NULL,
    role          TEXT NOT NULL DEFAULT 'member'
                  CHECK (role IN ('admin', 'member')),
    avatar_url    TEXT DEFAULT '',
    created_at    TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at    TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_users_email ON users(email);

CREATE TABLE IF NOT EXISTS sessions (
    id          TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL REFERENCES users(id),
    token_hash  TEXT NOT NULL,
    device_info TEXT DEFAULT '',
    expires_at  TEXT NOT NULL,
    created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_sessions_token_hash ON sessions(token_hash);
CREATE INDEX IF NOT EXISTS idx_sessions_user_id ON sessions(user_id);
CREATE INDEX IF NOT EXISTS idx_sessions_expires_at ON sessions(expires_at);

CREATE TABLE IF NOT EXISTS workspace_members (
    workspace_id TEXT NOT NULL REFERENCES workspaces(id),
    user_id      TEXT NOT NULL REFERENCES users(id),
    role         TEXT NOT NULL DEFAULT 'editor'
                 CHECK (role IN ('owner', 'editor', 'viewer')),
    created_at   TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (workspace_id, user_id)
);

CREATE INDEX IF NOT EXISTS idx_workspace_members_user ON workspace_members(user_id);

-- Add user_id to api_tokens (nullable for migration — existing tokens get assigned later)
ALTER TABLE api_tokens ADD COLUMN user_id TEXT REFERENCES users(id);

-- Add user_id columns to items for proper user attribution
ALTER TABLE items ADD COLUMN created_by_user_id TEXT REFERENCES users(id);
ALTER TABLE items ADD COLUMN last_modified_by_user_id TEXT REFERENCES users(id);

-- Add user_id to comments
ALTER TABLE comments ADD COLUMN user_id TEXT REFERENCES users(id);

-- Add user_id to activities
ALTER TABLE activities ADD COLUMN user_id TEXT REFERENCES users(id);

-- Add user_id to item_links
ALTER TABLE item_links ADD COLUMN user_id TEXT REFERENCES users(id);

-- Add user_id to item_versions
ALTER TABLE item_versions ADD COLUMN user_id TEXT REFERENCES users(id);
