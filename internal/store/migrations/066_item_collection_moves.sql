-- Migration 066: durable cross-collection move log (BUG-1675).
--
-- /items-changes must emit a "moved-out" tombstone when an item moves from a
-- collection a restricted member CAN see into one they can't, otherwise the
-- now-unauthorized row lingers in the client's local cache until a full
-- rebootstrap. Detecting that needs the item's PRIOR collection, which the
-- items table doesn't retain.
--
-- The activities table records moves, but it is written AFTER the move commits
-- and best-effort (errors discarded), so a delta poll racing the audit write —
-- or a failed write — could advance the client cursor past the move seq and
-- strand the item forever. This table captures every collection move as a
-- structured row written in the SAME transaction as the move (Store.
-- MoveItemWithPreCheck), carrying the workspace-scoped seq the move assigned —
-- so the moved-out query is durable, atomic, and fully indexable.
CREATE TABLE IF NOT EXISTS item_collection_moves (
    id                 TEXT   PRIMARY KEY,
    workspace_id       TEXT   NOT NULL REFERENCES workspaces(id),
    item_id            TEXT   NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    from_collection_id TEXT   NOT NULL,
    to_collection_id   TEXT   NOT NULL,
    seq                BIGINT NOT NULL,
    created_at         TEXT   NOT NULL
);

-- Primary access path for the moved-out query: walk a workspace's moves with
-- seq > cursor.
CREATE INDEX IF NOT EXISTS idx_item_collection_moves_ws_seq
    ON item_collection_moves(workspace_id, seq);

-- Per-item join target (items i ON i.id = m.item_id) for the current-collection
-- visibility check.
CREATE INDEX IF NOT EXISTS idx_item_collection_moves_item
    ON item_collection_moves(item_id);
