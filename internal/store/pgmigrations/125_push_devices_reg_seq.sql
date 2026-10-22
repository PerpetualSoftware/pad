-- Migration 125 (Postgres): push_devices.reg_seq (TASK-4031).
-- Postgres counterpart to migrations/157_push_devices_reg_seq.sql; the rationale is there.
CREATE SEQUENCE IF NOT EXISTS push_devices_reg_seq;
ALTER TABLE push_devices ADD COLUMN IF NOT EXISTS reg_seq BIGINT NOT NULL DEFAULT 0;
UPDATE push_devices p SET reg_seq = r.rn
FROM (SELECT id, ROW_NUMBER() OVER (ORDER BY updated_at, id) AS rn FROM push_devices) r
WHERE r.id = p.id;
SELECT setval('push_devices_reg_seq', GREATEST((SELECT COALESCE(MAX(reg_seq), 0) FROM push_devices), 1));
ALTER TABLE push_devices ALTER COLUMN reg_seq SET DEFAULT nextval('push_devices_reg_seq');
CREATE UNIQUE INDEX IF NOT EXISTS idx_push_devices_reg_seq ON push_devices(reg_seq);
