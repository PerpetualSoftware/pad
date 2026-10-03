-- Migration 107: mark rows a workspace import wrote (BUG-3379).
--
-- Import keeps the export's comment authors, version kinds and attachment
-- uploaders verbatim (BUG-3372 ruling (1)): it restores history. But nothing
-- distinguished those rows from authenticated authorship, so an importer
-- could present a fabricated trail. Nothing already stored can tell them
-- apart either: user_id is NULL after an account deletion too, the export
-- supplies created_at, and import writes no activity. So the import door
-- sets imported = 1 on what it writes, and nothing else ever does. Rows
-- imported before this migration stay 0: there is nothing to backfill from.
--
-- INTEGER on both dialects (0/1), so the flag binds the same way everywhere.
ALTER TABLE comments ADD COLUMN imported INTEGER NOT NULL DEFAULT 0;
ALTER TABLE item_versions ADD COLUMN imported INTEGER NOT NULL DEFAULT 0;
ALTER TABLE attachments ADD COLUMN imported INTEGER NOT NULL DEFAULT 0;
