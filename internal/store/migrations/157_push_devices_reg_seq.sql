-- Migration 157: push_devices.reg_seq, the registration sequence (TASK-4031).
-- When two users hold one push token, the latest registration owns it. That was
-- decided by updated_at (second resolution) then id (a random uuid), so a
-- same-second reclaim could go to the previous registrant. reg_seq is strictly
-- increasing across every register (INSERT and same-user UPDATE), unique, and
-- compared by the dispatcher instead. Backfill preserves the old ordering
-- (updated_at, then id) so no current owner changes.
ALTER TABLE push_devices ADD COLUMN reg_seq INTEGER NOT NULL DEFAULT 0;
WITH r AS (SELECT id AS did, ROW_NUMBER() OVER (ORDER BY updated_at, id) AS rn FROM push_devices)
UPDATE push_devices SET reg_seq = (SELECT rn FROM r WHERE r.did = push_devices.id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_push_devices_reg_seq ON push_devices(reg_seq);
