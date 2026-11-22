-- Migration 148 (Postgres): per-user day start (TASK-4699).
-- day_start_minute is the minute of the day (0 = 00:00, 1439 = 23:59) the
-- user wants their day to start at. NULL = never set, read as 09:00.
ALTER TABLE users ADD COLUMN day_start_minute INTEGER CHECK (day_start_minute IS NULL OR (day_start_minute >= 0 AND day_start_minute < 1440));
