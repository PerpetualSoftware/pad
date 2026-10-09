-- Migration 132: who asked for a CLI sign-in (TASK-2253).
--
-- The approval page shows the person deciding where the request came from:
-- the requesting IP (clientIP, after TrustedProxyRealIP) and the User-Agent
-- the requester sent, capped at 200 characters. The agent is
-- requester-controlled text and the page labels it so. Both are set once,
-- when the CLI creates the session; rows from before this migration read ''.
--
-- status also gains a fourth value, denied, written by the Deny button
-- (pending -> denied only). It is a TEXT column with no CHECK, so no schema
-- change is needed for it.
ALTER TABLE cli_auth_sessions ADD COLUMN requester_ip TEXT NOT NULL DEFAULT '';
ALTER TABLE cli_auth_sessions ADD COLUMN requester_user_agent TEXT NOT NULL DEFAULT '';
