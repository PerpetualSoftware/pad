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

INSERT INTO documents_new SELECT * FROM documents;

DROP TABLE documents;

ALTER TABLE documents_new RENAME TO documents;
