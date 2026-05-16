-- IDEA-1489: corrective backfill for collections.settings shape violations.
--
-- Migration 055 (IDEA-1484 / PR #562) hardened collections.settings to
-- NOT NULL DEFAULT '{}' but its backfill UPDATE only matched
-- `settings IS NULL`. Any pre-existing row with a wrong-shape value
-- (empty string, "null" literal, "[]" array, non-JSON garbage) survived
-- the filter — NOT NULL is satisfied but the contract IDEA-1484 set up
-- (collections.settings is always a JSON object) is violated.
--
-- PR #566 established the per-driver `json_valid()` / `json_type()`
-- widening pattern for the four migrations it touched (056, 057, pg/035,
-- pg/036) but was scope-bounded out of retroactively repairing
-- 055 / pg/034. This migration closes that gap.
--
-- Idempotent: if no row matches the predicate (the expected steady-state
-- on most deployments), the UPDATE is a no-op. The boundary normalizers
-- in `UpdateCollection`, `ImportWorkspace`, and `flexJSONToString` (from
-- PR #566) cover future writes, so re-corruption post-migration is not
-- expected.
UPDATE collections
SET settings = '{}'
WHERE settings IS NULL
   OR json_valid(settings) = 0
   OR json_type(settings) != 'object';
