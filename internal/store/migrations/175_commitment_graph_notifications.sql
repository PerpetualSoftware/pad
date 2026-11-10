-- Widen notification kinds for commitment graph effects.
PRAGMA foreign_keys = OFF;

DROP TABLE IF EXISTS user_notifications_new;

CREATE TABLE user_notifications_new (
    id             TEXT PRIMARY KEY,
    user_id        TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    workspace_id   TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    item_id        TEXT NOT NULL,
    comment_id     TEXT,
    kind           TEXT NOT NULL CHECK (kind IN ('assigned', 'comment', 'reply', 'reminder', 'mention', 'unassigned', 'renegotiation_refused', 'deadline_approval_requested', 'commitment_moved', 'commitment_moved_past_deadline', 'commitment_gate_opened')),
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

INSERT INTO user_notifications_new (id, user_id, workspace_id, item_id, comment_id, kind, actor_user_id, actor_is_agent, dedupe_key, created_at, push_state, skip_reason, claim_token, claimed_at, attempts, sent_at)
SELECT id, user_id, workspace_id, item_id, comment_id, kind, actor_user_id, actor_is_agent, dedupe_key, created_at, push_state, skip_reason, claim_token, claimed_at, attempts, sent_at FROM user_notifications;

DROP TABLE user_notifications;
ALTER TABLE user_notifications_new RENAME TO user_notifications;

CREATE INDEX IF NOT EXISTS idx_user_notifications_state ON user_notifications(push_state, created_at);
CREATE INDEX IF NOT EXISTS idx_user_notifications_sent ON user_notifications(user_id, sent_at);
CREATE INDEX IF NOT EXISTS idx_user_notifications_workspace ON user_notifications(workspace_id);
CREATE INDEX IF NOT EXISTS idx_user_notifications_actor ON user_notifications(actor_user_id);

PRAGMA foreign_keys = ON;
