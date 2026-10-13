-- BUG-3744: the op-log cursor the stored items.content covers. A browser
-- snapshot records its own cursor here and is refused when it is below the
-- stored one, so a delayed flush from a tab that had seen fewer ops cannot
-- overwrite a body built from more. A server-driven full write records
-- MAX(item_yjs_updates.id). Unlike content_flushed_op_log_id (the GC
-- watermark, which advances only when a cursor equals MAX), this moves on
-- every accepted content write. 0 for existing rows: nothing is refused until
-- a write records a cursor. (143 is BUG-3678's.)
ALTER TABLE items ADD COLUMN content_op_cursor INTEGER NOT NULL DEFAULT 0;
