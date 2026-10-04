-- Migration 094: app upgrades stage like installs (SPEC-6 U8b2, TASK-3397).
-- Mirrors SQLite migration 120; see it for the rationale.
ALTER TABLE app_install_pending ADD COLUMN IF NOT EXISTS upgrade_of TEXT REFERENCES app_installs(id) ON DELETE CASCADE;
