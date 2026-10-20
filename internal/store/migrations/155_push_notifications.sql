-- Migration 155: push notifications (TASK-3937, PLAN-3908 phase 2 unit 1).
--
-- push_devices: one row per signed-in app session that registers a push
-- token (the register endpoint lands with TASK-3938; nothing writes this
-- table yet). Every token is an FCM registration token (iOS routes through FCM),
-- so there is no environment column: FCM picks the APNs gateway itself.
-- platform stays because the FCM payload differs per platform. The row dies
-- with its session or its user.
--
-- user_notifications: the per-event push queue. Nothing writes it yet: the
-- enqueue is TASK-3937 part B, and it is to write a row ONLY for a user who
-- has at least one device, on the transaction of the write that caused it.
-- dedupe_key is what makes that enqueue idempotent per (user, key). item_id
-- and comment_id carry no foreign key: the dispatcher is to skip a row whose
-- subject is gone, and retention to remove the rest. Retention is NOT in this
-- migration: the 7-day sweep lands with the dispatcher (TASK-3939), and until
-- then nothing deletes a queue row. workspace_id cascades, so a purged
-- workspace leaves no queue behind. actor_user_id is indexed because deleting
-- a user runs its ON DELETE SET NULL against this table.
--
-- user_notification_settings: a person's recipient-side filters. No row means
-- the defaults. The dispatcher (TASK-3939) is to apply them; the enqueue
-- (part B) is not to.
--
-- workspace_read_cursors (149) stays separate: it must exist for every user,
-- with or without a device, and this queue is to be deleted after 7 days.
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
    actor_is_agent INTEGER NOT NULL DEFAULT 0 CHECK (actor_is_agent IN (0, 1)),
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
    push_enabled            INTEGER NOT NULL DEFAULT 1 CHECK (push_enabled IN (0, 1)),
    notify_assigned         INTEGER NOT NULL DEFAULT 1 CHECK (notify_assigned IN (0, 1)),
    notify_comments         INTEGER NOT NULL DEFAULT 1 CHECK (notify_comments IN (0, 1)),
    notify_replies          INTEGER NOT NULL DEFAULT 1 CHECK (notify_replies IN (0, 1)),
    notify_reminders        INTEGER NOT NULL DEFAULT 1 CHECK (notify_reminders IN (0, 1)),
    notify_mentions         INTEGER NOT NULL DEFAULT 1 CHECK (notify_mentions IN (0, 1)),
    include_agent_activity  INTEGER NOT NULL DEFAULT 0 CHECK (include_agent_activity IN (0, 1)),
    updated_at              TEXT NOT NULL
);
