-- Migration 115: comments.source accepts 'app' (SPEC-6 U2b, TASK-3391).
--
-- An app's comment is stamped source = 'app' (DOC-3371 §4). comments is the
-- only table whose source column carries a CHECK (items, item_versions and
-- activities have none), and SQLite cannot alter a CHECK, so the table is
-- rebuilt with the standard recipe 056 used for items.
--
-- The new table is the live schema verbatim (dumped from sqlite_master at
-- U2a's head: 16 columns, the original CHECK on created_by, and the FKs to
-- items, workspaces, users, activities, the parent self-reference and
-- app_installs), with ONE change: 'app' joins the source CHECK.
--
-- Rowids are copied explicitly: comments_fts is an external-content FTS5
-- table keyed on comments.rowid, and the 'rebuild' below repopulates its
-- index against those rowids. Every id is preserved, so the inbound FK
-- (comment_reactions.comment_id, ON DELETE CASCADE) and the parent_id
-- self-reference, which name the table, rebind to the renamed table. The
-- four indexes, the three FTS triggers and the eight NUL-invariant triggers
-- (084) are dropped with the old table and re-created here verbatim. The
-- 'rebuild' is a repair step only: with rowids copied and comments_fts never
-- dropped, the index already matches. A survival test checks all of this.
--
-- foreign_keys is off for the duration; the migration runner lifts these
-- PRAGMA bookends out of the wrapping transaction (056, IDEA-1485).

PRAGMA foreign_keys = OFF;

DROP TABLE IF EXISTS comments_new;

CREATE TABLE comments_new (
    id           TEXT PRIMARY KEY,
    item_id      TEXT NOT NULL REFERENCES items(id),
    workspace_id TEXT NOT NULL REFERENCES workspaces(id),
    author       TEXT NOT NULL DEFAULT '',
    body         TEXT NOT NULL,
    created_by   TEXT NOT NULL DEFAULT 'user'
                 CHECK (created_by IN ('user', 'agent')),
    source       TEXT NOT NULL DEFAULT 'web'
                 CHECK (source IN ('cli', 'web', 'skill', 'app')),
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL,
    user_id      TEXT REFERENCES users(id),
    activity_id  TEXT REFERENCES activities(id),
    parent_id    TEXT REFERENCES comments(id),
    deleted_at   TEXT,
    imported     INTEGER NOT NULL DEFAULT 0,
    via_app      TEXT REFERENCES app_installs(id) ON DELETE SET NULL
);

INSERT INTO comments_new (
    rowid, id, item_id, workspace_id, author, body, created_by, source,
    created_at, updated_at, user_id, activity_id, parent_id, deleted_at,
    imported, via_app
)
SELECT
    rowid, id, item_id, workspace_id, author, body, created_by, source,
    created_at, updated_at, user_id, activity_id, parent_id, deleted_at,
    imported, via_app
FROM comments;

DROP TABLE comments;
ALTER TABLE comments_new RENAME TO comments;

CREATE INDEX IF NOT EXISTS idx_comments_activity ON comments(activity_id);
CREATE INDEX IF NOT EXISTS idx_comments_item ON comments(item_id, created_at);
CREATE INDEX IF NOT EXISTS idx_comments_parent ON comments(parent_id);
CREATE INDEX IF NOT EXISTS idx_comments_workspace ON comments(workspace_id, created_at);

-- FTS triggers, bodies verbatim from 007_comments.sql.
DROP TRIGGER IF EXISTS comments_fts_insert;
DROP TRIGGER IF EXISTS comments_fts_update;
DROP TRIGGER IF EXISTS comments_fts_delete;

CREATE TRIGGER comments_fts_insert AFTER INSERT ON comments BEGIN
    INSERT INTO comments_fts(rowid, body) VALUES (NEW.rowid, NEW.body);
END;

CREATE TRIGGER comments_fts_update AFTER UPDATE ON comments BEGIN
    INSERT INTO comments_fts(comments_fts, rowid, body) VALUES('delete', OLD.rowid, OLD.body);
    INSERT INTO comments_fts(rowid, body) VALUES (NEW.rowid, NEW.body);
END;

CREATE TRIGGER comments_fts_delete AFTER DELETE ON comments BEGIN
    INSERT INTO comments_fts(comments_fts, rowid, body) VALUES('delete', OLD.rowid, OLD.body);
END;

INSERT INTO comments_fts(comments_fts) VALUES ('rebuild');

-- The eight NUL-invariant triggers (DOC-2823 S2) went with the old table.
-- Re-created here, verbatim from 084, inside this migration's transaction,
-- so no write lands between the DROP and their return; ensureNULTriggers
-- would otherwise restore them only after the migration commits.

CREATE TRIGGER IF NOT EXISTS pad_nul_comments_author_ins
BEFORE INSERT ON comments
FOR EACH ROW WHEN NEW.author IS NOT NULL AND (
			instr(NEW.author, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: comments.author must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_comments_author_upd
BEFORE UPDATE OF author ON comments
FOR EACH ROW WHEN NEW.author IS NOT NULL AND (
			instr(NEW.author, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: comments.author must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_comments_body_ins
BEFORE INSERT ON comments
FOR EACH ROW WHEN NEW.body IS NOT NULL AND (
			instr(NEW.body, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: comments.body must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_comments_body_upd
BEFORE UPDATE OF body ON comments
FOR EACH ROW WHEN NEW.body IS NOT NULL AND (
			instr(NEW.body, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: comments.body must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_comments_created_by_ins
BEFORE INSERT ON comments
FOR EACH ROW WHEN NEW.created_by IS NOT NULL AND (
			instr(NEW.created_by, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: comments.created_by must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_comments_created_by_upd
BEFORE UPDATE OF created_by ON comments
FOR EACH ROW WHEN NEW.created_by IS NOT NULL AND (
			instr(NEW.created_by, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: comments.created_by must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_comments_source_ins
BEFORE INSERT ON comments
FOR EACH ROW WHEN NEW.source IS NOT NULL AND (
			instr(NEW.source, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: comments.source must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_comments_source_upd
BEFORE UPDATE OF source ON comments
FOR EACH ROW WHEN NEW.source IS NOT NULL AND (
			instr(NEW.source, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: comments.source must not contain a NUL');
END;

PRAGMA foreign_keys = ON;
