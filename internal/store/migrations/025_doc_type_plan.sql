-- Rename doc_type 'phase-plan' to 'plan' in the CHECK constraint.
-- SQLite does not support ALTER CHECK, so we recreate the table.

CREATE TABLE documents_new (
    id               TEXT PRIMARY KEY,
    workspace_id     TEXT NOT NULL REFERENCES workspaces(id),
    title            TEXT NOT NULL,
    slug             TEXT NOT NULL,
    content          TEXT NOT NULL DEFAULT '',
    doc_type         TEXT NOT NULL DEFAULT 'notes'
                     CHECK (doc_type IN ('roadmap','plan','architecture','ideation',
                                         'feature-spec','notes','prompt-library','reference')),
    status           TEXT NOT NULL DEFAULT 'draft'
                     CHECK (status IN ('draft','active','completed','archived')),
    tags             TEXT NOT NULL DEFAULT '[]',
    pinned           INTEGER NOT NULL DEFAULT 0,
    sort_order       INTEGER NOT NULL DEFAULT 0,
    created_by       TEXT NOT NULL DEFAULT 'user'
                     CHECK (created_by IN ('user','agent')),
    last_modified_by TEXT NOT NULL DEFAULT 'user'
                     CHECK (last_modified_by IN ('user','agent')),
    source           TEXT NOT NULL DEFAULT 'web'
                     CHECK (source IN ('cli','web','skill')),
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL,
    deleted_at       TEXT,

    UNIQUE(workspace_id, slug),
    UNIQUE(workspace_id, title)
);

-- Copy data, renaming 'phase-plan' to 'plan' during the insert
INSERT INTO documents_new
SELECT id, workspace_id, title, slug, content,
       CASE WHEN doc_type = 'phase-plan' THEN 'plan' ELSE doc_type END,
       status, tags, pinned, sort_order, created_by, last_modified_by,
       source, created_at, updated_at, deleted_at
FROM documents;

DROP TABLE documents;

ALTER TABLE documents_new RENAME TO documents;

-- Recreate indexes (originally from 001_initial.sql)
CREATE INDEX IF NOT EXISTS idx_documents_workspace ON documents(workspace_id);
CREATE INDEX IF NOT EXISTS idx_documents_type ON documents(workspace_id, doc_type);
CREATE INDEX IF NOT EXISTS idx_documents_status ON documents(workspace_id, status);
CREATE INDEX IF NOT EXISTS idx_documents_updated ON documents(workspace_id, updated_at);

-- Recreate FTS triggers (originally from 001_initial.sql)
CREATE TRIGGER IF NOT EXISTS documents_ai AFTER INSERT ON documents BEGIN
    INSERT INTO documents_fts(rowid, title, content, tags)
    VALUES (new.rowid, new.title, new.content, new.tags);
END;

CREATE TRIGGER IF NOT EXISTS documents_ad AFTER DELETE ON documents BEGIN
    INSERT INTO documents_fts(documents_fts, rowid, title, content, tags)
    VALUES ('delete', old.rowid, old.title, old.content, old.tags);
END;

CREATE TRIGGER IF NOT EXISTS documents_au AFTER UPDATE ON documents BEGIN
    INSERT INTO documents_fts(documents_fts, rowid, title, content, tags)
    VALUES ('delete', old.rowid, old.title, old.content, old.tags);
    INSERT INTO documents_fts(rowid, title, content, tags)
    VALUES (new.rowid, new.title, new.content, new.tags);
END;

-- Rebuild FTS index to match potentially renamed doc_type values
INSERT INTO documents_fts(documents_fts) VALUES ('rebuild');
