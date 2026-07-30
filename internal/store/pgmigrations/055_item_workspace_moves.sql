-- Migration 055 (Postgres): cross-workspace copy/move provenance
-- (PLAN-2357 DR-2). Postgres counterpart to
-- internal/store/migrations/077_item_workspace_moves.sql — same intent, same
-- columns, same indexes; see that file for the full rationale.
--
-- Two dialect differences, both mechanical:
--   * archived_source is BOOLEAN here and INTEGER in SQLite (the value is
--     written through dialect.BoolToInt, which yields a native bool on
--     Postgres and 0/1 on SQLite).
--   * the partial index predicate is `= TRUE` rather than `= 1`.
CREATE TABLE IF NOT EXISTS item_workspace_moves (
    id                  TEXT    PRIMARY KEY,
    source_workspace_id TEXT    NOT NULL REFERENCES workspaces(id),
    source_item_id      TEXT    NOT NULL,
    target_workspace_id TEXT    NOT NULL REFERENCES workspaces(id),
    target_item_id      TEXT    NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    -- Move (true) vs plain copy (false). Only moves feed the archived-source
    -- "moved to" banner; copy rows are back-pointer material only.
    archived_source     BOOLEAN NOT NULL,
    -- NULLABLE. Deterministic ordering of one source's moves, and nothing
    -- else: created_at is second-precision RFC3339 and cannot break a tie
    -- between two moves in the same second. Holds the workspace-A `seq` the
    -- archive assigned (monotonic within A). NULL for plain copies, which
    -- never archive and never participate in the banner's ordering.
    --
    -- NOT item_collection_moves' delta-sync cursor, despite the resemblance.
    source_seq          BIGINT,
    created_by          TEXT    NOT NULL,
    created_at          TEXT    NOT NULL
);

-- source_item_id intentionally carries NO foreign key: the archived source is
-- exactly the row whose pointer must survive, so it must not cascade.
-- target_item_id does cascade — a pointer at a deleted destination is worse
-- than no pointer. Workspace purge clears both workspace directions
-- explicitly (workspace_purge.go).

-- Archived-source "moved to" lookup. PARTIAL, deliberately NOT UNIQUE:
-- archive → restore → move again legitimately yields a second archived row.
CREATE INDEX IF NOT EXISTS idx_item_workspace_moves_moved_to
    ON item_workspace_moves(source_item_id, source_seq DESC)
    WHERE archived_source = TRUE;

-- Forward lookup over ALL rows (copies included).
CREATE INDEX IF NOT EXISTS idx_item_workspace_moves_source
    ON item_workspace_moves(source_item_id);

-- Back lookup, and the index the target_item_id cascade needs. UNIQUE,
-- unlike the forward index: a destination item is created by exactly one
-- copy, in the same transaction that writes this row, so two rows naming one
-- target is a bug by construction and would silently change which source the
-- back-pointer names. The forward direction is deliberately non-unique — one
-- source fans out to many destinations.
CREATE UNIQUE INDEX IF NOT EXISTS uq_item_workspace_moves_target
    ON item_workspace_moves(target_item_id);
