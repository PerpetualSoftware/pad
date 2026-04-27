-- Migration 046: Restore documents_fts triggers + rebuild FTS index.
--
-- Background: BUG-822. Some production DBs ended up with the documents_*
-- triggers missing — most likely a transient quirk during migration 025's
-- table-rebuild for the doc_type CHECK constraint change, even though the
-- migration was recorded as applied. Items_fts and comments_fts triggers
-- are unaffected; the issue is isolated to the documents path.
--
-- This migration is intentionally idempotent and safe to apply on any DB:
--   - Fresh installs that ran 025 cleanly already have these triggers; the
--     DROP IF EXISTS + CREATE pair just round-trips.
--   - Affected DBs (missing triggers) get them re-created.
--   - The final 'rebuild' command repopulates the FTS5 internal index from
--     the current documents table so previously-created docs become
--     searchable, even if their inserts never fired the after-insert trigger.

-- 1. Drop existing triggers if present so we can recreate them with
--    consistent definitions. IF EXISTS makes this safe on DBs that don't
--    have them.
DROP TRIGGER IF EXISTS documents_ai;
DROP TRIGGER IF EXISTS documents_au;
DROP TRIGGER IF EXISTS documents_ad;

-- 2. Recreate the three triggers. Bodies match migration 025 / 001 exactly.
CREATE TRIGGER documents_ai AFTER INSERT ON documents BEGIN
    INSERT INTO documents_fts(rowid, title, content, tags)
    VALUES (new.rowid, new.title, new.content, new.tags);
END;

CREATE TRIGGER documents_ad AFTER DELETE ON documents BEGIN
    INSERT INTO documents_fts(documents_fts, rowid, title, content, tags)
    VALUES ('delete', old.rowid, old.title, old.content, old.tags);
END;

CREATE TRIGGER documents_au AFTER UPDATE ON documents BEGIN
    INSERT INTO documents_fts(documents_fts, rowid, title, content, tags)
    VALUES ('delete', old.rowid, old.title, old.content, old.tags);
    INSERT INTO documents_fts(rowid, title, content, tags)
    VALUES (new.rowid, new.title, new.content, new.tags);
END;

-- 3. Rebuild the FTS5 internal index from the current documents table.
-- This recovers searchability for documents that were inserted while the
-- triggers were missing. It's a no-op on DBs that were never broken.
INSERT INTO documents_fts(documents_fts) VALUES ('rebuild');
