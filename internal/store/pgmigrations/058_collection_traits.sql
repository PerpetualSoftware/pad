-- Collection kernel traits — Postgres mirror of
-- migrations/080_collection_traits.sql. See the SQLite migration for the full
-- rationale (BUG-2702; why traits get their own column rather than a key
-- inside `schema`; why the backfill is necessarily slug-keyed).
--
-- Note the guard differs from the SQLite migration's: JSONB cannot be compared
-- against '' (Postgres rejects it as invalid JSON input), and the column is
-- NOT NULL with a default, so the empty-string arm is both unreachable and
-- illegal here.
--
-- JSONB, matching this table's other JSON blob columns on Postgres
-- (collections.schema and collections.settings are both JSONB here, even
-- though they are TEXT in the SQLite schema). Go scans JSONB into a string
-- exactly as it already does for settings, so the store's scan path is
-- unchanged; the gain is that Postgres validates the JSON at write time,
-- which is the right floor for a column whose parse failure silently
-- degrades a collection to "declares nothing".
ALTER TABLE collections ADD COLUMN IF NOT EXISTS traits JSONB NOT NULL DEFAULT '{}';

UPDATE collections
SET traits = '{"bootstrap_include":[{"mode":"bodies","filter":{"status":"active","trigger":"always"},"key":"conventions"},{"mode":"metadata","filter":{"status":"active"},"key":"convention_index"}],"artifact_kind":{"kind":"convention"}}'
WHERE slug = 'conventions'
  AND (traits IS NULL OR traits = '{}'::jsonb);

UPDATE collections
SET traits = '{"bootstrap_include":[{"mode":"metadata","key":"playbooks"}],"invocation_field":"invocation_slug","artifact_kind":{"kind":"playbook"}}'
WHERE slug = 'playbooks'
  AND (traits IS NULL OR traits = '{}'::jsonb);
