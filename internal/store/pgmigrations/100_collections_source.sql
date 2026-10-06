-- Postgres counterpart to migrations/126_collections_source.sql (BUG-3447):
-- record how a collection was created. Existing rows default to '' (unknown,
-- never treated as agent-created); new collections set 'web' | 'cli' | 'mcp'.
ALTER TABLE collections ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT '';
