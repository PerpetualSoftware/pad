-- Migration 152 (Postgres): idempotency_keys.base_seq holds an item seq,
-- which is BIGINT (032); 110 declared it INTEGER. SQLite needs no counterpart
-- (its INTEGER is 64-bit).
SET LOCAL lock_timeout = '5s';
ALTER TABLE idempotency_keys ALTER COLUMN base_seq TYPE BIGINT;
