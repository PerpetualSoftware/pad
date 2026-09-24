-- Migration 095: restore items_fts and rebuild it once, to repair rows a
-- workspace import double-indexed (BUG-2758).
--
-- items_fts is an EXTERNAL-CONTENT FTS5 table (content='items',
-- content_rowid='rowid') maintained by the items_fts_insert / _update /
-- _delete triggers (005_collections.sql, recreated verbatim by 056). Until
-- BUG-2758, ImportWorkspace and RemapAttachmentReferencesInWorkspace ran a
-- post-commit pass that inserted every imported item into items_fts a SECOND
-- time, on top of the trigger's insert (a third time when the bundle had
-- attachments). Every SQLite database that has ever imported a workspace
-- therefore holds duplicate postings, and FTS5's integrity-check reports the
-- table malformed.
--
-- The migration RECREATES the index rather than repairing it in place, so
-- the state it leaves does not depend on the state it meets. Everything in
-- items_fts is derived from items, so dropping it loses nothing:
--   - the table is dropped and created with 005's definition, verbatim. A
--     damaged database may lack it, or hold one with another definition (a
--     contentless variant, other columns), and a bare 'rebuild' fails the
--     migration, and so startup, on each of those;
--   - the three triggers are recreated with 005's bodies (dropping the table
--     does not drop them, and a database may have lost one), so the index is
--     maintained again from here on;
--   - 'rebuild', FTS5's own command for an external-content table, populates
--     the new index from items: exactly what the triggers would have built.
--
-- Precedent: 056_items_jsonb_not_null.sql recreated the same three triggers
-- verbatim from 005 and rebuilt the index the same way. 005's definitions are
-- still current: the only later migrations to touch items_fts are 046
-- (documents_fts only) and 056 (the triggers, verbatim), so recreating from
-- 005 regresses nothing.
--
-- Cost: measured with the sqlite3 CLI at 1.0 s for 9,766 items and 10.5 s for
-- a synthetic 97,660 (see slowSQLiteMigrations in store.go, which logs a line
-- before this runs). It runs at startup, before the server listens.
--
-- SQLite only by construction: this directory is the SQLite chain.
-- Postgres runs pgmigrations/, has no items_fts, and keeps search_vector as a
-- trigger-maintained column on items, which the removed pass never touched.

DROP TABLE IF EXISTS items_fts;

CREATE VIRTUAL TABLE items_fts USING fts5(
  title, content, tags,
  content='items',
  content_rowid='rowid'
);

DROP TRIGGER IF EXISTS items_fts_insert;
DROP TRIGGER IF EXISTS items_fts_update;
DROP TRIGGER IF EXISTS items_fts_delete;

CREATE TRIGGER items_fts_insert AFTER INSERT ON items BEGIN
  INSERT INTO items_fts(rowid, title, content, tags) VALUES (NEW.rowid, NEW.title, NEW.content, NEW.tags);
END;

CREATE TRIGGER items_fts_update AFTER UPDATE ON items BEGIN
  INSERT INTO items_fts(items_fts, rowid, title, content, tags) VALUES('delete', OLD.rowid, OLD.title, OLD.content, OLD.tags);
  INSERT INTO items_fts(rowid, title, content, tags) VALUES (NEW.rowid, NEW.title, NEW.content, NEW.tags);
END;

CREATE TRIGGER items_fts_delete AFTER DELETE ON items BEGIN
  INSERT INTO items_fts(items_fts, rowid, title, content, tags) VALUES('delete', OLD.rowid, OLD.title, OLD.content, OLD.tags);
END;

INSERT INTO items_fts(items_fts) VALUES('rebuild');
