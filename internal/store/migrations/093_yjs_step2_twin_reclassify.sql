-- Migration 093: re-queue step2/update op-log rows for classification (BUG-3136).
--
-- Migration 091's classifier treated a SyncStep2 and an update carrying the
-- same payload as different frames, so an open tab's step2 answer to a joining
-- tab stayed content-bearing and held the item "pending" although it carried
-- the same bytes as the seed update. The classifier now also matches that
-- subtype twin. Rows already classified keep their old verdict until they are
-- re-examined, so this clears content_hash on every content-bearing step2 and
-- update row, and the startup backfill (BackfillYjsContentBearing) reclassifies
-- them in id order under the new rule.
--
-- Only content-bearing rows are re-queued: the new rule can only turn a
-- bearing row non-bearing, never the reverse. content_bearing itself is left
-- at 1 until the backfill decides, which is the safe direction.
UPDATE item_yjs_updates
   SET content_hash = NULL
 WHERE content_bearing = 1
   AND substr(update_data, 1, 2) IN (x'0001', x'0002');
