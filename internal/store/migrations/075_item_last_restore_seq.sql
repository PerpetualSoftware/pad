-- Add last_restore_seq: the durable per-item restore boundary (BUG-2264).
-- Records the item.seq assigned by the most recent version RESTORE. Version
-- restore prunes the per-item Yjs op-log and reseeds peers from the restored
-- items.content; a client whose Y.Doc seeded from PRE-restore content
-- (announced as ?content_seq= on WS connect) must be force_refreshed on Join so
-- it can't re-push the stale document. That fence was in-memory only, so a
-- server RESTART with a surviving cursor-0 pre-restore browser tab wasn't fenced
-- on reconnect. Persisting the boundary here makes the Join fence survive a
-- restart: the restore writes this column in the SAME transaction as the
-- content write + op-log prune (atomic), and Join reads it from the row.
--
-- NOTE: no `IF NOT EXISTS` — SQLite's ALTER TABLE ADD COLUMN rejects it.
-- Nullable with no default; NULL = never restored (no fence), so no backfill is
-- needed and existing items keep working unchanged.
ALTER TABLE items ADD COLUMN last_restore_seq INTEGER;

-- restore_boundary_op_id: the DURABLE op-log-id restore boundary — pre-prune
-- MAX(item_yjs_updates.id)+1 at the most recent restore. The collab-snapshot
-- flush gate rejects a PATCH whose op_log_cursor is below this boundary (a
-- pre-restore Y.Doc). That gate used the IN-MEMORY RoomManager.RestoreBoundary,
-- so after a server restart a surviving pre-restore tab's stale flush wasn't
-- fenced. Persisting it (stamped in the restore's own tx, atomic with the
-- content write + op-log prune) lets the gate read it from the row after a
-- restart. op-log ids are monotonic across prunes, so this value stays valid.
-- Nullable; NULL = never restored (no fence).
ALTER TABLE items ADD COLUMN restore_boundary_op_id INTEGER;
