-- IDEA-1486: harden items.fields and items.tags to NOT NULL DEFAULT.
-- See also IDEA-1484 / PR #562 (collections.settings precedent at
-- pgmigrations/034_collections_settings_not_null.sql).
--
-- The DEFAULT clauses were already '{}'::jsonb / '[]'::jsonb in
-- 001_initial.sql (lines 131-132), so SET DEFAULT here is a no-op
-- idempotency belt; SET NOT NULL is the load-bearing change.
--
-- Both columns sit on the same table — one ALTER TABLE per column,
-- atomic-as-a-group makes sense, so they share a single migration file.
-- views.config NOT NULL lands in 036 to keep per-table blast radius
-- independent.

-- Backfill: repair any row whose fields / tags violates the post-
-- migration shape contract. The JSONB column type rejects invalid
-- JSON at write time, so the only pre-existing pathologies that
-- could survive are SQL NULL and JSONB-valid-but-wrong-shape (e.g.
-- a JSONB null, an array where an object is required, a primitive).
-- Codex R2 P1: the original NULL-only filter would have left wrong-
-- shape rows in place, weakening the IDEA-1488 ceiling — the
-- handler-layer shape validators reject those shapes on input but
-- pre-existing rows would have slipped past.
UPDATE items SET fields = '{}'::jsonb WHERE fields IS NULL OR jsonb_typeof(fields) != 'object';
UPDATE items SET tags   = '[]'::jsonb WHERE tags   IS NULL OR jsonb_typeof(tags)   != 'array';
ALTER TABLE items ALTER COLUMN fields SET NOT NULL;
ALTER TABLE items ALTER COLUMN tags SET NOT NULL;
ALTER TABLE items ALTER COLUMN fields SET DEFAULT '{}'::jsonb;
ALTER TABLE items ALTER COLUMN tags SET DEFAULT '[]'::jsonb;
