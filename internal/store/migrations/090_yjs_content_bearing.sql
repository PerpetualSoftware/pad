-- Migration 090: mark op-log rows that cannot change the document (BUG-3124).
--
-- content_state (BUG-3000) reads "the op-log holds a row above
-- items.content_flushed_op_log_id". The relay persists EVERY y-protocol sync
-- frame, including SyncStep1 state-vector requests and a reconnecting
-- provider's byte-identical full-state re-sends, and a tab never re-flushes an
-- unchanged document, so those rows sat above the watermark forever and the
-- item read as pending with nothing pending. Measured day 76: 470 items read
-- pending; of the 12,713 rows above their watermarks, 6,112 were SyncStep1 and
-- 11,400 were byte-identical to an earlier row of the same item.
--
-- content_bearing = 0 marks a row that provably cannot change the document;
-- the predicate counts only content-bearing rows. Rows stay PERSISTED — replay,
-- GC and every durability check still see all of them (the lead's ruling:
-- mark, do not stop persisting; the re-send loop is BUG-3134's hypothesis).
--
-- content_hash is the sha256 of the whole frame, for the byte-identical
-- check. NULL means "not yet classified": the default of 1 is the safe
-- direction (a row nobody has examined keeps the item pending), and the
-- startup backfill (BackfillYjsContentBearing) fills legacy rows in id order.
--
-- NOTE: no `IF NOT EXISTS` — SQLite's ALTER TABLE ADD COLUMN rejects it.
ALTER TABLE item_yjs_updates ADD COLUMN content_bearing INTEGER NOT NULL DEFAULT 1;
ALTER TABLE item_yjs_updates ADD COLUMN content_hash TEXT;

CREATE INDEX IF NOT EXISTS idx_yjs_updates_item_hash
    ON item_yjs_updates(item_id, content_hash);
