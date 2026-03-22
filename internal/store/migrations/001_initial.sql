-- Enable WAL mode
PRAGMA journal_mode=WAL;

CREATE TABLE IF NOT EXISTS workspaces (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    slug        TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    settings    TEXT NOT NULL DEFAULT '{}',
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    deleted_at  TEXT
);

CREATE TABLE IF NOT EXISTS documents (
    id               TEXT PRIMARY KEY,
    workspace_id     TEXT NOT NULL REFERENCES workspaces(id),
    title            TEXT NOT NULL,
    slug             TEXT NOT NULL,
    content          TEXT NOT NULL DEFAULT '',
    doc_type         TEXT NOT NULL DEFAULT 'notes'
                     CHECK (doc_type IN ('roadmap','phase-plan','architecture','ideation',
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

CREATE TABLE IF NOT EXISTS versions (
    id              TEXT PRIMARY KEY,
    document_id     TEXT NOT NULL REFERENCES documents(id),
    content         TEXT NOT NULL,
    change_summary  TEXT NOT NULL DEFAULT '',
    created_by      TEXT NOT NULL DEFAULT 'user'
                    CHECK (created_by IN ('user','agent')),
    source          TEXT NOT NULL DEFAULT 'web'
                    CHECK (source IN ('cli','web','skill')),
    created_at      TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS activities (
    id            TEXT PRIMARY KEY,
    workspace_id  TEXT NOT NULL REFERENCES workspaces(id),
    document_id   TEXT,
    action        TEXT NOT NULL
                  CHECK (action IN ('created','updated','archived',
                                    'restored','read','searched')),
    actor         TEXT NOT NULL CHECK (actor IN ('user','agent')),
    source        TEXT NOT NULL CHECK (source IN ('cli','web','skill')),
    metadata      TEXT NOT NULL DEFAULT '{}',
    created_at    TEXT NOT NULL
);

-- Full-text search
CREATE VIRTUAL TABLE IF NOT EXISTS documents_fts USING fts5(
    title,
    content,
    tags,
    content='documents',
    content_rowid='rowid',
    tokenize='porter unicode61'
);

-- Triggers to keep FTS in sync
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

-- Indexes
CREATE INDEX IF NOT EXISTS idx_documents_workspace ON documents(workspace_id);
CREATE INDEX IF NOT EXISTS idx_documents_type ON documents(workspace_id, doc_type);
CREATE INDEX IF NOT EXISTS idx_documents_status ON documents(workspace_id, status);
CREATE INDEX IF NOT EXISTS idx_documents_updated ON documents(workspace_id, updated_at);
CREATE INDEX IF NOT EXISTS idx_versions_document ON versions(document_id, created_at);
CREATE INDEX IF NOT EXISTS idx_activities_workspace ON activities(workspace_id, created_at);
CREATE INDEX IF NOT EXISTS idx_activities_document ON activities(document_id, created_at);
