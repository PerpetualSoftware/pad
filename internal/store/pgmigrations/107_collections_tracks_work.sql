-- Migration 107 (Postgres): system collections hold reference material (PLAN-3535).
-- Postgres counterpart to migrations/135_collections_tracks_work.sql; the rationale is there.
UPDATE collections
SET settings = COALESCE(settings, '{}'::jsonb) || '{"tracks_work": false}'::jsonb
WHERE is_system = TRUE
  AND jsonb_typeof(COALESCE(settings, '{}'::jsonb)) = 'object'
  AND NOT (COALESCE(settings, '{}'::jsonb) ? 'tracks_work');
