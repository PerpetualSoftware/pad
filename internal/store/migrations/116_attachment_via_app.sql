-- Migration 116: which app install uploaded an attachment (SPEC-6 U7, TASK-3396).
--
-- An app upload is created bound to a companion item, with uploaded_by = the
-- acting identity and via_app = the install (DOC-3371 §4, day-85 revision).
-- A thumbnail inherits its original's via_app under the parent-row lock, as it
-- inherits item_id (BUG-3385).
--
-- The install caps (2,000 files, 1 GiB) are a RECOUNT of the install's live
-- ORIGINAL rows under the install-row lock, not persisted counters, so the
-- index below covers exactly that predicate and carries size_bytes for the SUM.
-- Same reference shape as 113: app_installs rows are never deleted.
ALTER TABLE attachments ADD COLUMN via_app TEXT REFERENCES app_installs(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS idx_attachments_via_app_live
    ON attachments (via_app, size_bytes)
    WHERE via_app IS NOT NULL AND parent_id IS NULL AND deleted_at IS NULL;
