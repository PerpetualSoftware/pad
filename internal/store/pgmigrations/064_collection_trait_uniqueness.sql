-- TASK-2710, Postgres half. Mirrors
-- internal/store/migrations/087_collection_trait_uniqueness.sql — see that
-- file for why the invariant exists and why the de-duplication pass that must
-- precede it lives in Go rather than in this migration.
--
-- traits is JSONB here (SQLite stores TEXT), so the extraction operator
-- differs: ->> yields the value as text, which matches json_extract's result
-- type on the SQLite side and keeps both indexes keyed on the same thing.

CREATE UNIQUE INDEX IF NOT EXISTS idx_collections_artifact_kind_per_workspace
    ON collections(workspace_id, ((traits -> 'artifact_kind' ->> 'kind')))
    WHERE (traits -> 'artifact_kind' ->> 'kind') IS NOT NULL
      AND (traits -> 'artifact_kind' ->> 'kind') <> ''
      AND deleted_at IS NULL;

CREATE UNIQUE INDEX IF NOT EXISTS idx_collections_invocation_field_per_workspace
    ON collections(workspace_id)
    WHERE (traits ->> 'invocation_field') IS NOT NULL
      AND (traits ->> 'invocation_field') <> ''
      AND deleted_at IS NULL;
