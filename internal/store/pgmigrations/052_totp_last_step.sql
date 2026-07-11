-- Add totp_last_step for single-use TOTP enforcement (BUG-2054).
-- Postgres mirror of migrations/074_totp_last_step.sql. BIGINT because the
-- step counter (unix_time / 30) grows unbounded over time; NULL = no code
-- consumed yet, so no backfill is required. See the SQLite migration for the
-- full rationale.
ALTER TABLE users ADD COLUMN IF NOT EXISTS totp_last_step BIGINT;
