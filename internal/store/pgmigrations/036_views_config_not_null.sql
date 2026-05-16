-- IDEA-1486: harden views.config to NOT NULL DEFAULT '{}'::jsonb.
-- See also IDEA-1484 / PR #562 (collections.settings precedent at
-- pgmigrations/034_collections_settings_not_null.sql) and migration 035
-- in the same PR for the parallel items.fields / items.tags hardening.
--
-- The DEFAULT clause was already '{}'::jsonb in 001_initial.sql:200, so
-- SET DEFAULT here is a no-op idempotency belt; SET NOT NULL is the
-- load-bearing change.

UPDATE views SET config = '{}'::jsonb WHERE config IS NULL;
ALTER TABLE views ALTER COLUMN config SET NOT NULL;
ALTER TABLE views ALTER COLUMN config SET DEFAULT '{}'::jsonb;
