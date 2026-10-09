-- Migration 134: the highest op-log id a compaction snapshot covers (TASK-3531).
--
-- The dormancy sweep can replace an item's whole op-log with ONE frame, the
-- Yjs state of the replayed document, instead of deleting it. Yjs identity
-- (clientID, clock) survives that, so a tab that slept past the sweep can
-- merge its unsent edits; a deleted log forces it to refresh. Join reads
-- this column: a resume cursor at or below it is inside the snapshot and is
-- admitted, where today an empty or pruned log means force_refresh.
--
-- yjs_snapshot_op_id is the snapshot row itself. Join admits a covered
-- cursor only while that row still exists, so a path that deletes the op-log
-- (a direct write, a restore, a schema rebuild) can never leave a stale
-- yjs_compacted_through admitting a tab onto a rebuilt document, whether or
-- not that path remembers to clear these columns. The known ones do.
--
-- NULL means never compacted. Written only by CompactItemOpLog.
ALTER TABLE items ADD COLUMN yjs_compacted_through INTEGER;
ALTER TABLE items ADD COLUMN yjs_snapshot_op_id INTEGER;
