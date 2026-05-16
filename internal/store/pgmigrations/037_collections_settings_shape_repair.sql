-- IDEA-1489: corrective backfill for collections.settings shape violations.
--
-- pgmigrations/034 (IDEA-1484 / PR #562) hardened collections.settings to
-- NOT NULL DEFAULT '{}'::jsonb but its backfill UPDATE only matched
-- `settings IS NULL`. JSONB columns reject invalid JSON at write time, so
-- the only pre-existing pathologies that could survive are SQL NULL and
-- JSONB-valid-but-wrong-shape (JSONB null, an array, a primitive). The
-- NULL-only filter left every wrong-shape row in place.
--
-- PR #566 established the `jsonb_typeof() != 'object'` widening pattern
-- for pgmigrations/035 + 036 but was scope-bounded out of retroactively
-- repairing pg/034. This migration closes that gap. Idempotent — if no
-- row matches, the UPDATE is a no-op.
UPDATE collections
SET settings = '{}'::jsonb
WHERE settings IS NULL OR jsonb_typeof(settings) != 'object';
