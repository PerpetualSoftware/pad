-- BUG-3744: the op-log cursor the stored items.content covers. A browser
-- snapshot records its own cursor here and is refused when it is below the
-- stored one, so a delayed flush from a tab that had seen fewer ops cannot
-- overwrite a body built from more. A server-driven full write records
-- MAX(item_yjs_updates.id). Unlike content_flushed_op_log_id (the GC
-- watermark, which advances only when a cursor equals MAX), this moves on
-- every accepted content write. Existing rows start at their GC watermark,
-- which a stored body always covers (it is stamped only by a write that saw
-- every row up to it), so a delayed pre-migration snapshot below it is refused
-- too. (143 is BUG-3678's.)
ALTER TABLE items ADD COLUMN content_op_cursor INTEGER NOT NULL DEFAULT 0;
UPDATE items SET content_op_cursor = content_flushed_op_log_id WHERE content_flushed_op_log_id > 0;
