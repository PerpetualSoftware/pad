-- Add disabled_at column for account deactivation (soft-disable).
-- NULL = active, non-NULL = disabled at that time.
ALTER TABLE users ADD COLUMN disabled_at TEXT DEFAULT NULL;
