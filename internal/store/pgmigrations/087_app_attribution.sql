-- Migration 087: which app install made a write (SPEC-6 U2a, TASK-3390).
-- Mirrors SQLite migration 113; see it for the rationale.
ALTER TABLE items ADD COLUMN IF NOT EXISTS via_app TEXT REFERENCES app_installs(id) ON DELETE SET NULL;
ALTER TABLE items ADD COLUMN IF NOT EXISTS created_via_app TEXT REFERENCES app_installs(id) ON DELETE SET NULL;
ALTER TABLE comments ADD COLUMN IF NOT EXISTS via_app TEXT REFERENCES app_installs(id) ON DELETE SET NULL;
ALTER TABLE activities ADD COLUMN IF NOT EXISTS via_app TEXT REFERENCES app_installs(id) ON DELETE SET NULL;
ALTER TABLE item_versions ADD COLUMN IF NOT EXISTS via_app TEXT REFERENCES app_installs(id) ON DELETE SET NULL;
ALTER TABLE item_links ADD COLUMN IF NOT EXISTS via_app TEXT REFERENCES app_installs(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS idx_items_created_via_app ON items (created_via_app) WHERE created_via_app IS NOT NULL;
