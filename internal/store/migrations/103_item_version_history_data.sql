-- Migration 103: what a History needs from a version row (PLAN-2348 U2).
--
-- user_id is NOT added here: migration 012 added item_versions.user_id
-- (REFERENCES users(id)) and nothing ever wrote it. U2 starts writing it, so
-- rows before U2 read NULL, as do system rows (recovery).
--
-- lines_added / lines_removed: the line counts of the change THIS ROW'S WRITE
-- recorded, computed when the server holds both bodies. NULL means unknown
-- (every legacy row), never zero.
--
-- is_create: the row was written by item create, so it holds the body AS
-- CREATED; an update row holds the body BEFORE its edit. The diff endpoint
-- needs the difference to pair the right bodies. Backfilled for legacy rows:
-- the create path writes the item and its version with one timestamp, and
-- only when the item has content, so the first row sharing the item's
-- created_at is its create row.
ALTER TABLE item_versions ADD COLUMN lines_added INTEGER;
ALTER TABLE item_versions ADD COLUMN lines_removed INTEGER;
ALTER TABLE item_versions ADD COLUMN is_create INTEGER NOT NULL DEFAULT 0;

UPDATE item_versions SET is_create = 1
WHERE version_seq = 1
  AND created_at = (SELECT i.created_at FROM items i WHERE i.id = item_versions.item_id);
