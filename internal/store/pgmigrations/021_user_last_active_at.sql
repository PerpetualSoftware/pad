-- Track when users were last active on the platform.
-- NULL = never active (or created before this migration).
ALTER TABLE users ADD COLUMN IF NOT EXISTS last_active_at TEXT DEFAULT NULL;
