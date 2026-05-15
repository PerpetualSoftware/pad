-- IDEA-1484: harden collections.settings to NOT NULL DEFAULT '{}'::jsonb.
-- The DEFAULT clause was already '{}'::jsonb in 001_initial.sql, so SET
-- DEFAULT here is a no-op idempotency belt; SET NOT NULL is the load-bearing
-- change. See also BUG-1482 / PR #561.
--
-- The defensive `sql.NullString` scans in collections.go / export.go remain
-- in place and will be reverted in a separate follow-up PR after this
-- migration has rolled out everywhere.

UPDATE collections SET settings = '{}'::jsonb WHERE settings IS NULL;
ALTER TABLE collections ALTER COLUMN settings SET NOT NULL;
ALTER TABLE collections ALTER COLUMN settings SET DEFAULT '{}'::jsonb;
