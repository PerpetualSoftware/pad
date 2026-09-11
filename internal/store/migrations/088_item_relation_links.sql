-- Migration 088: materialised reverse index for `relation` field values
-- (PLAN-2857 U5 / TASK-2997).
--
-- A relation field stores the target item's id inside the source item's
-- `fields` JSON blob. Asking the reverse question — "which items point at
-- TASK-5?" — has no answer today short of scanning every blob.
--
-- WHY MATERIALISED RATHER THAN DERIVED AT READ (ratified Q3). The derived
-- query is an unindexed `json_extract(fields,'$.color') = ?` scan; no index
-- can be added for a USER-DEFINED key without a per-key migration; and on a
-- `multi_relation` array it silently matches nothing rather than failing.
-- That is the same argument migration 061 made for wiki-links, on the same
-- table, against the same alternative.
--
-- WHY A NEW TABLE rather than extending item_wiki_links: that table's
-- target_kind is CHECK-constrained to three CONTENT-link kinds, and its
-- position / display_text columns are content-shaped. A field-valued edge
-- has no position and no display override — but it does have a source FIELD
-- KEY, which nothing in that schema can carry.
--
-- Field notes:
--   * source_field_key is the schema key the value sits under. It is what
--     makes this table not item_wiki_links, and it is what lets the reverse
--     side say "referenced by X via `owner`" rather than just "referenced".
--   * target_item_id is NOT FK'd, matching 061's reasoning: a dangling
--     reference is a row worth keeping and rendering honestly on the SOURCE
--     side. Note that such a row is INERT for the reverse query — no
--     target-side lookup can ever find it — so it exists for the source
--     side and a future broken-references report, not for the count.
--   * ordinal is the position within a `multi_relation` array (U4, not yet
--     landed). Single-valued relations are always 0. Present up-front so U4
--     does not need an ALTER.
--   * workspace_id is denormalised from the source item so the reverse query
--     can scope without a join, exactly as the visibility filter needs it.
--
--
-- NUL INVARIANT (migration 084's census). All four TEXT columns here are
-- recorded as unprotected rather than given their own triggers, and the reason
-- is a derivation, not an omission:
--
--   * target_item_id is extracted from `items.fields`, and source_field_key
--     from `collections.schema`. BOTH of those columns are classJSON-protected
--     already, so a NUL cannot be in the value this table copies.
--   * source_item_id and workspace_id are server-generated ids.
--
-- The derivation is TRANSACTIONAL, which is the part worth stating: every
-- write hook runs after its own row's INSERT/UPDATE inside the same
-- transaction, so the protecting trigger has already fired and rejected a
-- NUL-bearing blob before this table sees anything derived from it. A hook
-- moved to run BEFORE its row's write would break that, and the census would
-- not notice — it checks columns, not ordering.
-- CASCADE on source_item_id matches every other items-referencing table: an
-- item's OUTGOING edges disappear when it is hard-deleted. A SOFT delete
-- leaves the rows in place and the reverse query filters on the source's
-- deleted_at, so a restore brings the edges back without re-deriving them.

CREATE TABLE IF NOT EXISTS item_relation_links (
    source_item_id   TEXT NOT NULL,
    source_field_key TEXT NOT NULL,
    target_item_id   TEXT NOT NULL,
    workspace_id     TEXT NOT NULL,
    ordinal          INTEGER NOT NULL DEFAULT 0,
    FOREIGN KEY (source_item_id) REFERENCES items(id) ON DELETE CASCADE
);

-- The reverse lookup this table exists for.
CREATE INDEX IF NOT EXISTS idx_relation_links_target
    ON item_relation_links(target_item_id);

-- The write hook's own key: every write replaces one (source, field) pair's
-- rows wholesale, so this is the delete's index as well as the forward read's.
CREATE INDEX IF NOT EXISTS idx_relation_links_source_field
    ON item_relation_links(source_item_id, source_field_key);
