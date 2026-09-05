-- IDEA-2883, Postgres half. Mirrors
-- internal/store/migrations/086_item_reminders_composite_fk.sql — see the
-- SQLite file for why the constraint exists, why there is no ON UPDATE
-- CASCADE, and why disagreeing rows are dropped rather than repaired.
--
-- Postgres CAN add the constraint in place, so there is no table rebuild
-- here; the two dialects still end in the same shape.

-- The parent key. Measured, not assumed: Postgres accepts a plain unique
-- INDEX (not only a unique CONSTRAINT) as a composite FK target.
CREATE UNIQUE INDEX IF NOT EXISTS idx_items_id_workspace ON items(id, workspace_id);

-- The repair pass, matching the SQLite copy's JOIN filter: a reminder whose
-- workspace disagrees with its item's, or whose item is gone, would fail the
-- ADD CONSTRAINT below and take a deployment's startup with it.
DELETE FROM item_reminders r
WHERE NOT EXISTS (
    SELECT 1 FROM items i
    WHERE i.id = r.item_id AND i.workspace_id = r.workspace_id
);

-- Drop the single-column FK that 062 created inline. Discovered by SHAPE
-- rather than by name: Postgres auto-names it item_reminders_item_id_fkey,
-- but auto-names collide-and-suffix, so a deployment could carry a different
-- one and a name-keyed DROP ... IF EXISTS would silently leave it in place.
-- Harmless if it stayed — the composite FK is strictly stronger — but then
-- the two dialects would not actually agree, and the next reader comparing
-- them would be reading a lie.
DO $$
DECLARE
    conname_found TEXT;
BEGIN
    SELECT conname INTO conname_found
    FROM pg_constraint
    WHERE conrelid = 'item_reminders'::regclass
      AND contype = 'f'
      AND confrelid = 'items'::regclass
      AND conkey = ARRAY[(SELECT attnum FROM pg_attribute
                          WHERE attrelid = 'item_reminders'::regclass AND attname = 'item_id')]::smallint[];
    IF conname_found IS NOT NULL THEN
        EXECUTE format('ALTER TABLE item_reminders DROP CONSTRAINT %I', conname_found);
    END IF;
END
$$;

-- IF NOT EXISTS is not available for ADD CONSTRAINT, and re-running is not a
-- concern: schema_migrations makes this file run once, inside the same
-- transaction as its bookkeeping row.
ALTER TABLE item_reminders
    ADD CONSTRAINT item_reminders_item_workspace_fkey
    FOREIGN KEY (item_id, workspace_id) REFERENCES items(id, workspace_id) ON DELETE CASCADE;
