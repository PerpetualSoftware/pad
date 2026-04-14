-- Add disabled_at column for account deactivation (soft-disable).
-- NULL = active, non-NULL = disabled at that time.
ALTER TABLE users ADD COLUMN IF NOT EXISTS disabled_at TEXT DEFAULT NULL;
