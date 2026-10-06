-- Migration 101 (Postgres): which one-time UI suggestions a user has dismissed (TASK-3452).
-- Postgres counterpart to migrations/127_user_ui_dismissals.sql; the rationale is there.
ALTER TABLE users ADD COLUMN IF NOT EXISTS ui_dismissals TEXT NOT NULL DEFAULT '[]';
