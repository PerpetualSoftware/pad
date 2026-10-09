-- Migration 105 (Postgres): who asked for a CLI sign-in (TASK-2253).
-- Postgres counterpart to migrations/132_cli_auth_requester.sql; the rationale is there.
ALTER TABLE cli_auth_sessions ADD COLUMN IF NOT EXISTS requester_ip TEXT NOT NULL DEFAULT '';
ALTER TABLE cli_auth_sessions ADD COLUMN IF NOT EXISTS requester_user_agent TEXT NOT NULL DEFAULT '';
