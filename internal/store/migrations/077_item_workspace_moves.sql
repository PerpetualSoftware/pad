-- Migration 077: cross-workspace copy/move provenance (PLAN-2357 DR-2).
--
-- Records "this item was copied (or moved) from workspace A to workspace B".
-- Written in the SAME transaction as the copy (and, on a move, the archive of
-- the source), so the pointer can never disagree with the data.
--
-- Why a table rather than items.content or items.fields: content carries
-- collab/Yjs op-log semantics and mutating it reads as a lie after a restore;
-- fields is schema-validated per collection and has no reserved-key escape
-- hatch. Decisively, mutating resolution excludes archived items, so a
-- content- or field-based pointer would have to be written BEFORE the archive
-- with no way to patch it afterward. A row in the same tx has no such
-- ordering constraint.
--
-- Two lookups, both hot, both indexed below:
--   forward — WHERE source_item_id = ?  ("where did this go?")
--   back    — WHERE target_item_id = ?  ("where did this come from?")
--
-- Shape precedent is item_collection_moves (migrations/066), but ONLY the
-- shape. That table's `seq` is a workspace delta-sync cursor; `source_seq`
-- here is a per-source move ordinal with an entirely different job (below).
CREATE TABLE IF NOT EXISTS item_workspace_moves (
    id                  TEXT    PRIMARY KEY,
    source_workspace_id TEXT    NOT NULL REFERENCES workspaces(id),
    source_item_id      TEXT    NOT NULL,
    target_workspace_id TEXT    NOT NULL REFERENCES workspaces(id),
    target_item_id      TEXT    NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    -- Move (true) vs plain copy (false). Only moves feed the archived-source
    -- "moved to" banner; copy rows are back-pointer material only. Written
    -- through dialect.BoolToInt, hence INTEGER here and BOOLEAN in the
    -- Postgres counterpart.
    archived_source     INTEGER NOT NULL,
    -- NULLABLE, and it exists for exactly one reason: deterministic ordering
    -- of one source's moves. A source can be moved, restored, and moved
    -- again, so the banner must pick the NEWEST archived_source row — and
    -- created_at cannot break the tie because timestamps in this schema are
    -- second-precision RFC3339. This stores the workspace-A `seq` that the
    -- archive assigned: monotonic within A, and the source always lives in A,
    -- so it totally orders that source's moves. Plain copies never archive
    -- and so have no A seq; they are NULL, and since the banner reads only
    -- archived_source rows, NULLs never participate in the ordering.
    --
    -- This is NOT item_collection_moves' delta-sync cursor. Do not wire it to
    -- any workspace cursor.
    source_seq          BIGINT,
    created_by          TEXT    NOT NULL,
    created_at          TEXT    NOT NULL
);

-- FK / cascade choices, which deliberately differ from item_collection_moves:
--
--   source_item_id has NO foreign key and therefore no cascade. 066 cascades
--   on item_id because a hard-deleted item has no meaningful move history;
--   here the inverse holds — the archived source is precisely the row whose
--   pointer must survive. Per-item hard delete does not currently exist (only
--   workspace purge), so this is future-proofing, but the direction matters.
--
--   target_item_id DOES cascade: if the destination item is gone, a pointer
--   at it is worse than no pointer.
--
--   Both workspace columns are plain RESTRICT references. Workspace purge
--   clears them explicitly (workspace_purge.go), in BOTH directions, so a
--   purge of either side removes the row.
--
--   created_by is unconstrained (like item_links.created_by) so account
--   deletion has nothing to cascade or NULL here.

-- The archived-source "moved to" lookup: newest archived row for one source.
-- PARTIAL, and deliberately NOT UNIQUE — archive → restore → move again
-- legitimately produces a second archived row for the same source, so a
-- uniqueness constraint would be wrong.
CREATE INDEX IF NOT EXISTS idx_item_workspace_moves_moved_to
    ON item_workspace_moves(source_item_id, source_seq DESC)
    WHERE archived_source = 1;

-- Forward lookup over ALL rows (copies included); the partial index above
-- cannot serve it.
CREATE INDEX IF NOT EXISTS idx_item_workspace_moves_source
    ON item_workspace_moves(source_item_id);

-- Back lookup, and the index the target_item_id cascade needs.
--
-- UNIQUE, unlike the forward index above, and the asymmetry is the point: a
-- destination item is created by exactly one copy, and its provenance row is
-- written in that same transaction, so two rows naming one target is a bug by
-- construction. Left unenforced, a duplicate would silently change which
-- source the back-pointer names. The forward direction has no such
-- constraint — one source legitimately fans out to many destinations, and
-- archive → restore → move again legitimately repeats.
CREATE UNIQUE INDEX IF NOT EXISTS uq_item_workspace_moves_target
    ON item_workspace_moves(target_item_id);
