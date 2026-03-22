-- Comments on items
CREATE TABLE IF NOT EXISTS comments (
    id           TEXT PRIMARY KEY,
    item_id      TEXT NOT NULL REFERENCES items(id),
    workspace_id TEXT NOT NULL REFERENCES workspaces(id),
    author       TEXT NOT NULL DEFAULT '',
    body         TEXT NOT NULL,
    created_by   TEXT NOT NULL DEFAULT 'user'
                 CHECK (created_by IN ('user', 'agent')),
    source       TEXT NOT NULL DEFAULT 'web'
                 CHECK (source IN ('cli', 'web', 'skill')),
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_comments_item ON comments(item_id, created_at);
CREATE INDEX IF NOT EXISTS idx_comments_workspace ON comments(workspace_id, created_at);

-- FTS for comment bodies
CREATE VIRTUAL TABLE IF NOT EXISTS comments_fts USING fts5(
    body,
    content='comments',
    content_rowid='rowid',
    tokenize='porter unicode61'
);

CREATE TRIGGER IF NOT EXISTS comments_fts_insert AFTER INSERT ON comments BEGIN
    INSERT INTO comments_fts(rowid, body) VALUES (NEW.rowid, NEW.body);
END;

CREATE TRIGGER IF NOT EXISTS comments_fts_update AFTER UPDATE ON comments BEGIN
    INSERT INTO comments_fts(comments_fts, rowid, body) VALUES('delete', OLD.rowid, OLD.body);
    INSERT INTO comments_fts(rowid, body) VALUES (NEW.rowid, NEW.body);
END;

CREATE TRIGGER IF NOT EXISTS comments_fts_delete AFTER DELETE ON comments BEGIN
    INSERT INTO comments_fts(comments_fts, rowid, body) VALUES('delete', OLD.rowid, OLD.body);
END;
