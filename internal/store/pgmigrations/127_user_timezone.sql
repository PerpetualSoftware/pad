-- Migration 127 (Postgres): user timezone setting (TASK-4101).
-- Postgres counterpart to migrations/160_user_timezone.sql.
-- NULL means never set; readers compute in UTC.
ALTER TABLE users ADD COLUMN timezone TEXT;
