-- Migration 045 (Postgres): durable cross-collection move log (BUG-1675).
--
-- Postgres counterpart to migrations/066_item_collection_moves.sql. Same
-- intent: /items-changes must emit a moved-out tombstone when an item moves
-- from a collection a restricted member CAN see into one they can't, and that
-- signal must be durable + atomic with the move rather than derived from the
-- best-effort post-commit activities row (which a delta poll can race or a
-- failed write can drop, stranding the unauthorized item in the client cache).
--
-- Written in the SAME transaction as the move (Store.MoveItemWithPreCheck),
-- carrying the workspace-scoped seq the move assigned.
CREATE TABLE IF NOT EXISTS item_collection_moves (
    id                 TEXT   PRIMARY KEY,
    workspace_id       TEXT   NOT NULL REFERENCES workspaces(id),
    item_id            TEXT   NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    from_collection_id TEXT   NOT NULL,
    to_collection_id   TEXT   NOT NULL,
    seq                BIGINT NOT NULL,
    created_at         TEXT   NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_item_collection_moves_ws_seq
    ON item_collection_moves(workspace_id, seq);

CREATE INDEX IF NOT EXISTS idx_item_collection_moves_item
    ON item_collection_moves(item_id);
