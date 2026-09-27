-- Migration 097: keep the unflushed edits a schema-version rebuild used to delete (BUG-3244).
--
-- A collab join at a new editor schema version rebuilds the item's op-log
-- (collab.RoomManager.maybeRebuildOnSchemaMismatch): rows written under the old
-- schema cannot replay into a new-schema document, so they leave the op-log.
-- Before this table they were DELETED, including the content-bearing rows above
-- items.content_flushed_op_log_id, which are edits items.content never
-- received. content_state then read clean, so the stale body looked current.
--
-- Those rows are now MOVED here first. Nothing replays them: this table is not
-- item_yjs_updates, so no op-log reader (replay, cursors, GC, applier, watermark
-- stamp) can see them. They are kept for recovery (TASK-3246), are readable as
-- raw updates, and hold the item's content_state at superseded_set_aside until
-- someone recovers or explicitly discards them.
--
--   op_log_id       the row's id in item_yjs_updates before the move, so the
--                   original order and provenance survive.
--   update_data     the frame, byte for byte (BLOB/BYTEA, like the op-log's).
--   schema_version  the era the frame was written under; the decoder that
--                   recovers it is chosen by this.
--   created_at      when the frame was first persisted.
--   set_aside_at    when the rebuild moved it.

CREATE TABLE IF NOT EXISTS item_yjs_updates_set_aside (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    item_id         TEXT NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    op_log_id       INTEGER NOT NULL,
    update_data     BLOB NOT NULL,
    schema_version  TEXT NOT NULL,
    created_at      TEXT NOT NULL,
    set_aside_at    TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_yjs_set_aside_item_id
    ON item_yjs_updates_set_aside(item_id, id);
