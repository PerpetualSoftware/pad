-- Collections + Items model (v2)

CREATE TABLE IF NOT EXISTS collections (
  id           TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL REFERENCES workspaces(id),
  name         TEXT NOT NULL,
  slug         TEXT NOT NULL,
  icon         TEXT DEFAULT '',
  description  TEXT DEFAULT '',
  schema       TEXT NOT NULL DEFAULT '{"fields":[]}',
  settings     TEXT DEFAULT '{}',
  sort_order   INTEGER DEFAULT 0,
  is_default   INTEGER DEFAULT 0,
  created_at   TEXT NOT NULL,
  updated_at   TEXT NOT NULL,
  deleted_at   TEXT,
  UNIQUE(workspace_id, slug)
);

CREATE TABLE IF NOT EXISTS items (
  id               TEXT PRIMARY KEY,
  workspace_id     TEXT NOT NULL REFERENCES workspaces(id),
  collection_id    TEXT NOT NULL REFERENCES collections(id),
  title            TEXT NOT NULL,
  slug             TEXT NOT NULL,
  content          TEXT DEFAULT '',
  fields           TEXT DEFAULT '{}',
  tags             TEXT DEFAULT '[]',
  pinned           INTEGER DEFAULT 0,
  sort_order       INTEGER DEFAULT 0,
  parent_id        TEXT REFERENCES items(id),
  created_by       TEXT DEFAULT 'user',
  last_modified_by TEXT DEFAULT 'user',
  source           TEXT DEFAULT 'web',
  created_at       TEXT NOT NULL,
  updated_at       TEXT NOT NULL,
  deleted_at       TEXT,
  UNIQUE(workspace_id, slug)
);

CREATE INDEX IF NOT EXISTS idx_items_collection ON items(collection_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_items_workspace ON items(workspace_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_items_parent ON items(parent_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_items_updated ON items(updated_at) WHERE deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS item_links (
  id           TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL,
  source_id    TEXT NOT NULL REFERENCES items(id),
  target_id    TEXT NOT NULL REFERENCES items(id),
  link_type    TEXT DEFAULT 'related',
  created_by   TEXT DEFAULT 'user',
  created_at   TEXT NOT NULL,
  UNIQUE(source_id, target_id, link_type)
);

CREATE INDEX IF NOT EXISTS idx_links_source ON item_links(source_id);
CREATE INDEX IF NOT EXISTS idx_links_target ON item_links(target_id);

CREATE TABLE IF NOT EXISTS views (
  id            TEXT PRIMARY KEY,
  workspace_id  TEXT NOT NULL REFERENCES workspaces(id),
  collection_id TEXT REFERENCES collections(id),
  name          TEXT NOT NULL,
  slug          TEXT NOT NULL,
  view_type     TEXT NOT NULL,
  config        TEXT DEFAULT '{}',
  sort_order    INTEGER DEFAULT 0,
  is_default    INTEGER DEFAULT 0,
  created_at    TEXT NOT NULL,
  updated_at    TEXT NOT NULL,
  UNIQUE(workspace_id, slug)
);

-- item_versions (version history for items, mirrors versions table structure)
CREATE TABLE IF NOT EXISTS item_versions (
  id             TEXT PRIMARY KEY,
  item_id        TEXT NOT NULL REFERENCES items(id),
  content        TEXT NOT NULL,
  change_summary TEXT NOT NULL DEFAULT '',
  created_by     TEXT NOT NULL DEFAULT 'user',
  source         TEXT NOT NULL DEFAULT 'web',
  is_diff        INTEGER NOT NULL DEFAULT 0,
  created_at     TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_item_versions_item ON item_versions(item_id, created_at);

-- FTS for items
CREATE VIRTUAL TABLE IF NOT EXISTS items_fts USING fts5(
  title, content, tags,
  content='items',
  content_rowid='rowid'
);

CREATE TRIGGER IF NOT EXISTS items_fts_insert AFTER INSERT ON items BEGIN
  INSERT INTO items_fts(rowid, title, content, tags) VALUES (NEW.rowid, NEW.title, NEW.content, NEW.tags);
END;

CREATE TRIGGER IF NOT EXISTS items_fts_update AFTER UPDATE ON items BEGIN
  INSERT INTO items_fts(items_fts, rowid, title, content, tags) VALUES('delete', OLD.rowid, OLD.title, OLD.content, OLD.tags);
  INSERT INTO items_fts(rowid, title, content, tags) VALUES (NEW.rowid, NEW.title, NEW.content, NEW.tags);
END;

CREATE TRIGGER IF NOT EXISTS items_fts_delete AFTER DELETE ON items BEGIN
  INSERT INTO items_fts(items_fts, rowid, title, content, tags) VALUES('delete', OLD.rowid, OLD.title, OLD.content, OLD.tags);
END;
