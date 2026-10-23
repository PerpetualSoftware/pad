-- Migration 158: chat conversations (TASK-4038, PLAN-4037 phase 2a unit 1).
--
-- conversations are global (no workspace_id), numbered per owner. A direct
-- conversation is a person's chat with one of their agents (dm_key is
-- 'agent:<owner>:<agent_key>'). Messages are ordered by a per-conversation
-- seq taken from conversations.last_seq under the row lock. A delete
-- tombstones: the row and its seq stay, the body is cleared. There is no edit.
-- context_workspace_id is ON DELETE SET NULL so a workspace purge leaves the
-- message and drops only the now-false pointer.

CREATE TABLE IF NOT EXISTS conversations (
    id            TEXT PRIMARY KEY,
    owner_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    number        INTEGER NOT NULL CHECK (number >= 1),
    kind          TEXT NOT NULL DEFAULT 'direct' CHECK (kind IN ('direct', 'group')),
    title         TEXT NOT NULL DEFAULT '',
    dm_key        TEXT UNIQUE,
    last_seq      BIGINT NOT NULL DEFAULT 0 CHECK (last_seq >= 0),
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL,
    UNIQUE (owner_user_id, number)
);

CREATE TABLE IF NOT EXISTS conversation_members (
    conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    user_id         TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    agent_key       TEXT NOT NULL DEFAULT '',
    read_seq        BIGINT NOT NULL DEFAULT 0 CHECK (read_seq >= 0),
    delivered_seq   BIGINT NOT NULL DEFAULT 0 CHECK (delivered_seq >= 0),
    added_at        TEXT NOT NULL,
    PRIMARY KEY (conversation_id, user_id, agent_key)
);
CREATE INDEX IF NOT EXISTS idx_conversation_members_user ON conversation_members(user_id, agent_key);

CREATE TABLE IF NOT EXISTS conversation_messages (
    id                   TEXT PRIMARY KEY,
    conversation_id      TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    seq                  BIGINT NOT NULL CHECK (seq >= 1),
    -- author_user_id and deleted_by_user_id carry no FK on purpose (TASK-4038):
    -- on Postgres a FK to users deadlocks with the account-delete cascade
    -- (a send holds the conversations row, then wants KEY SHARE on users). The
    -- owner cascade removes every 2a message. 2b, when cross-user messages
    -- arrive, owes an author de-identify step in eraseUserTx, like comments.user_id.
    author_user_id       TEXT NOT NULL,
    author_agent_key     TEXT NOT NULL DEFAULT '',
    source               TEXT NOT NULL DEFAULT '',
    body                 TEXT NOT NULL,
    reply_to_seq         BIGINT,
    context_workspace_id TEXT REFERENCES workspaces(id) ON DELETE SET NULL,
    op_key               TEXT NOT NULL DEFAULT '',
    created_at           TEXT NOT NULL,
    deleted_at           TEXT,
    deleted_by_user_id   TEXT,
    deleted_by_agent_key TEXT NOT NULL DEFAULT '',
    UNIQUE (conversation_id, seq),
    CHECK (reply_to_seq IS NULL OR (reply_to_seq >= 1 AND reply_to_seq < seq)),
    CHECK ((deleted_at IS NULL AND body <> '') OR (deleted_at IS NOT NULL AND body = ''))
);
CREATE INDEX IF NOT EXISTS idx_conversation_messages_context ON conversation_messages(context_workspace_id);
CREATE UNIQUE INDEX IF NOT EXISTS uq_conversation_messages_op_key
    ON conversation_messages(conversation_id, author_user_id, author_agent_key, op_key) WHERE op_key <> '';
