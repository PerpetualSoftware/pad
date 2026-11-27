-- Migration 194: a WebView session names the web session it was minted from.
--
-- A short-lived kind = 'webview' session is minted from a web session sent as
-- a bearer and is accepted only as the WebView cookie. It lives and dies with
-- its parent: deleting the parent (logout, rotation, expiry sweep, account
-- deletion) deletes every child through the cascade. Every existing row is a
-- top-level session and keeps a NULL parent.
ALTER TABLE sessions ADD COLUMN parent_session_id TEXT REFERENCES sessions(id) ON DELETE CASCADE;
CREATE INDEX IF NOT EXISTS idx_sessions_parent ON sessions(parent_session_id);
