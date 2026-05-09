-- Migration 052: items.content_flushed_at + items.content_flushed_op_log_id
-- watermarks for op-log GC safety (TASK-1309).
--
-- The collab op-log GC sweeper deletes a dormant item's entire op-log
-- on the assumption that items.content is canonical (captures
-- everything the op-log has). That assumption holds iff items.content
-- was actually updated AFTER every op-log row was appended.
--
-- The 5s collab-snapshot flush is best-effort: a browser/process death
-- between an op-log append and the next flush leaves the op-log as the
-- only durable record of those edits. Without a flush watermark, the
-- GC would happily delete the op-log on the 24h dormancy threshold and
-- silently lose the unsaved edits.
--
-- `content_flushed_op_log_id` is the AUTHORITATIVE GC watermark:
-- MAX(item_yjs_updates.id) covered by the most recent server-driven
-- full-content write to items.content. Strict id comparison in the
-- sweeper (`MAX(u.id) <= i.content_flushed_op_log_id`) avoids both
-- second-granularity timestamp false-positives (Codex round 4 [P1])
-- AND the browser-flush-stamps-stale-content race (Codex round 5
-- [P1]: only server-driven writes advance this column).
--
-- `content_flushed_at` is a SECONDARY informational timestamp. It is
-- updated on every content write (including browser flushes) and
-- exists for human-readable diagnostics ("when was this last
-- flushed?"). The GC does NOT consult it.
--
-- NULL on either column means "never flushed since this column
-- existed". The GC treats NULL as "don't touch" — pre-migration
-- items keep their op-logs forever until a future content update
-- bumps the watermark. Per Codex review of TASK-1309 round 2 [P1].

ALTER TABLE items ADD COLUMN content_flushed_at TEXT;

-- content_flushed_op_log_id is the AUTHORITATIVE op-log GC watermark.
-- It records the highest item_yjs_updates.id covered by the most
-- recent items.content write. The sweeper requires
-- `MAX(op-log.id) <= items.content_flushed_op_log_id` before
-- pruning — strict id comparison, monotonic, no clock-skew or
-- second-granularity-equality issues that wall-clock timestamps
-- carry. Per Codex review of TASK-1309 round 4 [P1].
--
-- content_flushed_at (TEXT timestamp) above is retained for human-
-- readable diagnostics; it is NOT consulted by the sweeper.
ALTER TABLE items ADD COLUMN content_flushed_op_log_id INTEGER;

-- Backfill: only items that DON'T already have op-log rows can be
-- safely declared "flushed" via the updated_at backfill. For items
-- WITH op-log rows, we can't tell from migration time whether
-- updated_at was bumped by a content change (which would have
-- captured the ops into items.content) or by a metadata-only PATCH
-- (which would not have). Backfilling those would risk certifying
-- an unflushed op as flushed and letting the GC sweeper delete the
-- only durable copy of an edit. Per Codex review of TASK-1309
-- round 3 [P1].
--
-- Items WITHOUT op-log rows have nothing the sweeper would touch;
-- backfilling their watermark is harmless and unblocks future
-- content updates from setting it.
--
-- Items WITH op-log rows stay at NULL until the first post-migration
-- content update (Store.UpdateItem with input.Content != nil) sets
-- the watermark. Until then the sweeper can't prune them — that's
-- the safe default.
UPDATE items
SET content_flushed_at = updated_at
WHERE content != ''
  AND NOT EXISTS (
    SELECT 1 FROM item_yjs_updates u WHERE u.item_id = items.id
  );

-- content_flushed_op_log_id backfill: items with no op-log rows can
-- be safely set to 0 (vacuously covers nothing, but they have no
-- rows anyway — never appear in the candidate query). Items WITH
-- op-log rows stay NULL until the first post-migration content
-- write, by which time UpdateItem stamps the column with
-- MAX(item_yjs_updates.id) via subquery.
UPDATE items
SET content_flushed_op_log_id = 0
WHERE content != ''
  AND NOT EXISTS (
    SELECT 1 FROM item_yjs_updates u WHERE u.item_id = items.id
  );
