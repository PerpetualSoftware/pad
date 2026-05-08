-- Migration 050: Yjs collaborative-editing operation log (PLAN-1248 TASK-1252).
--
-- Persistence layer for the dumb-relay WebSocket server in PLAN-1248. Every
-- Y.Doc binary update (whether produced by a browser editor, a future
-- designated-applier conversion of a CLI/API content edit, etc.) is appended
-- here so:
--
--   1. A reconnecting client can replay updates since its last known ID,
--   2. A cold room can rebuild its in-memory Y.Doc from disk,
--   3. The schema-version stamp catches Tiptap minor bumps that change the
--      underlying ProseMirror schema (TASK-1268's snapshot-and-rebuild path).
--
-- Markdown remains the canonical content source — `items.content` is the
-- materialised view kept fresh by the 5s idle flush. This op-log is the
-- *operational* substrate for live collaboration; it can be pruned (and
-- the canonical markdown survives untouched in items.content).
--
-- Schema notes:
--   id              Monotonic. AUTOINCREMENT (not just INTEGER PRIMARY KEY)
--                   so a deleted row's rowid is never reused — clients hold
--                   IDs across disconnects and must not see "the same id"
--                   meaning two different ops. Postgres mirror uses BIGSERIAL.
--   item_id         FK with ON DELETE CASCADE. Deleting an item must reclaim
--                   its op-log; callers don't need to manually purge.
--   update_data     Raw Yjs binary update (BLOB on SQLite, BYTEA on Postgres).
--                   Format is whatever Y.encodeStateAsUpdate / mergeUpdates
--                   produces — opaque to the server.
--   schema_version  Stamp set at append time. The current SCHEMA_VERSION
--                   constant is owned by the client (web/src/lib/collab/);
--                   a mismatch on connect triggers TASK-1268's rebuild flow.
--   created_at      ISO8601 UTC TEXT, the cross-dialect convention used
--                   everywhere else in pad (see migrations/047_attachments.sql).
--                   Drives PruneYjsUpdatesBefore.

CREATE TABLE IF NOT EXISTS item_yjs_updates (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    item_id         TEXT NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    update_data     BLOB NOT NULL,
    schema_version  TEXT NOT NULL,
    created_at      TEXT NOT NULL
);

-- Hot path: load all updates for a room, ordered by id, optionally above a
-- cursor. The index covers WHERE item_id = ? AND id > ? ORDER BY id ASC
-- without a separate sort.
CREATE INDEX IF NOT EXISTS idx_yjs_updates_item_id
    ON item_yjs_updates(item_id, id);

-- Prune path: DELETE FROM item_yjs_updates WHERE item_id = ? AND created_at < ?
-- benefits from a (item_id, created_at) index; we already get item_id
-- locality from the index above plus the FK constraint, and prune is a
-- background operation so a single index is enough. If the prune sweeper
-- ever shows up as hot, add (item_id, created_at).
