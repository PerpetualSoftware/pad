-- Postgres mirror of migrations/047_attachments.sql (TASK-869).
-- See [[Attachments — architecture & migration design]] (DOC-865) for context.

CREATE TABLE IF NOT EXISTS attachments (
    id            TEXT PRIMARY KEY,
    workspace_id  TEXT NOT NULL,
    item_id       TEXT,
    uploaded_by   TEXT NOT NULL,
    storage_key   TEXT NOT NULL,
    content_hash  TEXT NOT NULL,
    mime_type     TEXT NOT NULL,
    size_bytes    BIGINT NOT NULL,
    filename      TEXT NOT NULL,
    width         INTEGER,
    height        INTEGER,
    parent_id     TEXT,
    variant       TEXT,
    created_at    TEXT NOT NULL,
    deleted_at    TEXT
);

CREATE INDEX IF NOT EXISTS idx_attach_workspace ON attachments(workspace_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_attach_item      ON attachments(item_id)      WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_attach_hash      ON attachments(content_hash);
CREATE INDEX IF NOT EXISTS idx_attach_parent    ON attachments(parent_id)    WHERE parent_id IS NOT NULL;
