-- Migration 129 (Postgres): per-user Chat setting (TASK-4315).
-- chat_enabled is a display preference: 0 = off (the default), 1 = on.
-- NOT NULL, so no NULL ever means never set.
ALTER TABLE users ADD COLUMN chat_enabled INTEGER NOT NULL DEFAULT 0;

-- One-time carry-over: a user who has sent a Chat message themselves keeps
-- Chat. An agent message is stored under the owner user id with a non-empty
-- author_agent_key, so only the empty key counts as the human. Tombstoned
-- messages count. Existence only, bodies are never read; it only sets 1, so
-- a re-run changes nothing.
UPDATE users SET chat_enabled = 1 WHERE EXISTS (SELECT 1 FROM conversation_messages m WHERE m.author_user_id = users.id AND m.author_agent_key = '');
