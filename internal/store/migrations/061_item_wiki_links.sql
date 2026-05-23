-- Migration 061: server-side reverse index for [[...]] wiki-links
-- (PLAN-1593 / TASK-1594).
--
-- Today the [[...]] syntax is parsed only at render time on the client.
-- There's no way to ask "who links to TASK-5?" without a full-text scan.
-- This migration adds the materialized index that the parse-on-save
-- bookkeeping (internal/links/extract.go + items.go write paths) will
-- populate.
--
-- Phase 1 (this migration + TASK-1594) only stores 'ref'-kind rows. The
-- schema accommodates all five wiki-link forms (ref / title / collection-
-- qualified title / cross-workspace ref) up-front so Phase 2 doesn't
-- require a follow-up ALTER. See PLAN-1593's "Schema (sketch)" section.
--
-- Field notes:
--   * target_kind ∈ {'ref','title','workspace_ref'}. Constrained by a
--     CHECK so a typo from the application layer fails loudly instead
--     of silently inserting unqueryable rows.
--   * target_workspace_id is NULL for same-workspace links. The
--     (workspace_id, ref) shape on the composite index supports the
--     cross-workspace reverse lookup Phase 2 will add without a
--     schema change.
--   * target_item_id is NULL when the link couldn't be resolved at
--     parse time (broken ref like [[NOTAREAL-99]] or a title that
--     doesn't match any item yet). Persisting unresolved rows
--     intentionally — feeds a future broken-links report and lets
--     a later resolver run pick them up.
--   * position is the byte offset of the match in the source item's
--     content. Used for the ~80-char snippet the handler returns
--     and for stable per-source ordering when an item links to the
--     same target multiple times.
--
-- The CASCADE on source_item_id matches every other items-referencing
-- table — when an item is hard-deleted (or soft-deleted then GC'd) its
-- outgoing links disappear with it. Targets are not FK'd because a
-- broken-ref row legitimately has no target row to reference.

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
