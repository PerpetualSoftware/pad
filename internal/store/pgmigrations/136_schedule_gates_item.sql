SET LOCAL lock_timeout = '5s';
-- The gated side of a commitment gate is the ITEM, not its schedule (TASK-4353).
-- An item with no commitment can be gated, and deleting or ending either
-- schedule no longer decides whether a gate exists:
--   * blocked_item_id replaces blocked_schedule_id (NOT NULL, the gated item);
--   * blocker_schedule_id is set to NULL when the blocker schedule is deleted,
--     so the gate stays and is judged by the blocker item status;
--   * one gate per gated item and blocker item, and never an item gating itself.
-- The foreign keys are added NOT VALID and validated after, so the lock on items
-- is held only for the catalog change, not for a scan of the gates.
ALTER TABLE schedule_gates ADD COLUMN blocked_item_id TEXT;
UPDATE schedule_gates g SET blocked_item_id = sc.item_id FROM item_schedules sc WHERE sc.id = g.blocked_schedule_id;
ALTER TABLE schedule_gates ALTER COLUMN blocked_item_id SET NOT NULL;
ALTER TABLE schedule_gates DROP COLUMN blocked_schedule_id;
ALTER TABLE schedule_gates ADD CONSTRAINT schedule_gates_blocked_item_id_fkey
    FOREIGN KEY (blocked_item_id) REFERENCES items(id) ON DELETE CASCADE NOT VALID;
ALTER TABLE schedule_gates VALIDATE CONSTRAINT schedule_gates_blocked_item_id_fkey;
ALTER TABLE schedule_gates ADD CONSTRAINT schedule_gates_blocked_blocker_key UNIQUE (blocked_item_id, blocker_item_id);
ALTER TABLE schedule_gates ADD CONSTRAINT schedule_gates_not_self CHECK (blocked_item_id <> blocker_item_id);
ALTER TABLE schedule_gates DROP CONSTRAINT schedule_gates_blocker_schedule_id_fkey;
ALTER TABLE schedule_gates ADD CONSTRAINT schedule_gates_blocker_schedule_id_fkey
    FOREIGN KEY (blocker_schedule_id) REFERENCES item_schedules(id) ON DELETE SET NULL NOT VALID;
ALTER TABLE schedule_gates VALIDATE CONSTRAINT schedule_gates_blocker_schedule_id_fkey;
