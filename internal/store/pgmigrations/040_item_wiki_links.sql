-- Postgres mirror of SQLite migration 061 (PLAN-1593 / TASK-1594).
-- See internal/store/migrations/061_item_wiki_links.sql for the field-
-- by-field rationale; this file diverges only where SQLite vs Postgres
-- syntax requires it.
--
-- Differences from the SQLite migration:
--   * No backtick-style identifier quoting needed.
--   * Partial-index WHERE clauses use the same syntax in both engines
--     (both support it) so the index definitions are byte-identical
--     apart from formatting.

CREATE TABLE IF NOT EXISTS item_wiki_links (
    source_item_id      TEXT NOT NULL,
    target_kind         TEXT NOT NULL CHECK (target_kind IN ('ref','title','workspace_ref')),
    target_workspace_id TEXT,
    target_item_id      TEXT,
    target_ref          TEXT,
    target_title        TEXT,
    display_text        TEXT,
    position            INTEGER NOT NULL,
    FOREIGN KEY (source_item_id) REFERENCES items(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_wiki_links_source
    ON item_wiki_links(source_item_id);

CREATE INDEX IF NOT EXISTS idx_wiki_links_target_item
    ON item_wiki_links(target_item_id)
    WHERE target_item_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_wiki_links_target_ref
    ON item_wiki_links(target_workspace_id, target_ref)
    WHERE target_ref IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_wiki_links_target_title
    ON item_wiki_links(target_title)
    WHERE target_title IS NOT NULL;
