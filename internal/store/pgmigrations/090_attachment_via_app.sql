-- Migration 090: which app install uploaded an attachment (SPEC-6 U7, TASK-3396).
-- Mirrors SQLite migration 116; see it for the rationale.
ALTER TABLE attachments ADD COLUMN IF NOT EXISTS via_app TEXT REFERENCES app_installs(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS idx_attachments_via_app_live
    ON attachments (via_app, size_bytes)
    WHERE via_app IS NOT NULL AND parent_id IS NULL AND deleted_at IS NULL;
