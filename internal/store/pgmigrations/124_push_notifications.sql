-- Migration 124 (Postgres): push notifications (TASK-3937).
-- Postgres counterpart to migrations/155_push_notifications.sql; the rationale is there.
CREATE TABLE IF NOT EXISTS push_devices (
    id            TEXT PRIMARY KEY,
    user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    session_id    TEXT NOT NULL UNIQUE REFERENCES sessions(id) ON DELETE CASCADE,
    connection_id TEXT NOT NULL,
    platform      TEXT NOT NULL CHECK (platform IN ('ios', 'android')),
    token         TEXT NOT NULL,
    app_version   TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL,
    UNIQUE (user_id, token)
);

CREATE INDEX IF NOT EXISTS idx_push_devices_user ON push_devices(user_id);

CREATE TABLE IF NOT EXISTS user_notifications (
    id             TEXT PRIMARY KEY,
    user_id        TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    workspace_id   TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    item_id        TEXT NOT NULL,
    comment_id     TEXT,
    kind           TEXT NOT NULL CHECK (kind IN ('assigned', 'comment', 'reply', 'reminder', 'mention')),
    actor_user_id  TEXT REFERENCES users(id) ON DELETE SET NULL,
    actor_is_agent BOOLEAN NOT NULL DEFAULT FALSE,
    dedupe_key     TEXT NOT NULL,
    created_at     TEXT NOT NULL,
    push_state     TEXT NOT NULL DEFAULT 'pending' CHECK (push_state IN ('pending', 'sent', 'skipped', 'expired')),
    skip_reason    TEXT NOT NULL DEFAULT '',
    claim_token    TEXT NOT NULL DEFAULT '',
    claimed_at     TEXT,
    attempts       INTEGER NOT NULL DEFAULT 0,
    sent_at        TEXT,
    UNIQUE (user_id, dedupe_key)
);

CREATE INDEX IF NOT EXISTS idx_user_notifications_state ON user_notifications(push_state, created_at);
CREATE INDEX IF NOT EXISTS idx_user_notifications_sent ON user_notifications(user_id, sent_at);
CREATE INDEX IF NOT EXISTS idx_user_notifications_workspace ON user_notifications(workspace_id);
CREATE INDEX IF NOT EXISTS idx_user_notifications_actor ON user_notifications(actor_user_id);

CREATE TABLE IF NOT EXISTS user_notification_settings (
    user_id                 TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    push_enabled            BOOLEAN NOT NULL DEFAULT TRUE,
    notify_assigned         BOOLEAN NOT NULL DEFAULT TRUE,
    notify_comments         BOOLEAN NOT NULL DEFAULT TRUE,
    notify_replies          BOOLEAN NOT NULL DEFAULT TRUE,
    notify_reminders        BOOLEAN NOT NULL DEFAULT TRUE,
    notify_mentions         BOOLEAN NOT NULL DEFAULT TRUE,
    include_agent_activity  BOOLEAN NOT NULL DEFAULT FALSE,
    updated_at              TEXT NOT NULL
);
