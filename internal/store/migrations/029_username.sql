-- Add username column to users table.
-- Nullable initially (empty string = not yet set).
-- TASK-482 will backfill existing users and add NOT NULL constraint.
ALTER TABLE users ADD COLUMN username TEXT DEFAULT '';
CREATE UNIQUE INDEX IF NOT EXISTS idx_users_username ON users(username) WHERE username != '';
