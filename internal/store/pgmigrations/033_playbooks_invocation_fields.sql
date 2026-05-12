-- Add optional `invocation_slug` and `arguments` fields to the Playbooks
-- collection schema for every existing workspace. Foundational migration for
-- PLAN-1377 — playbooks become first-class invokable procedures. Mirrors
-- internal/store/migrations/054_playbooks_invocation_fields.sql for the
-- SQLite path; uniqueness on invocation_slug is enforced at the application
-- layer (see internal/server/handlers_items.go checkUniqueFields).
UPDATE collections
SET schema = jsonb_set(
    schema,
    '{fields}',
    (schema->'fields') || jsonb_build_object(
        'key', 'invocation_slug',
        'label', 'Invocation slug',
        'type', 'text',
        'pattern', '^[a-z0-9][a-z0-9-]*[a-z0-9]$',
        'unique_scope', 'workspace_collection'
    )
)
WHERE slug = 'playbooks'
  AND jsonb_typeof(schema->'fields') = 'array'
  AND NOT EXISTS (
      SELECT 1
      FROM jsonb_array_elements(schema->'fields') f
      WHERE f->>'key' = 'invocation_slug'
  );

UPDATE collections
SET schema = jsonb_set(
    schema,
    '{fields}',
    (schema->'fields') || jsonb_build_object(
        'key', 'arguments',
        'label', 'Arguments',
        'type', 'json'
    )
)
WHERE slug = 'playbooks'
  AND jsonb_typeof(schema->'fields') = 'array'
  AND NOT EXISTS (
      SELECT 1
      FROM jsonb_array_elements(schema->'fields') f
      WHERE f->>'key' = 'arguments'
  );

-- Atomically prevent two concurrent writers from inserting items with the
-- same invocation_slug within the same collection. The application-layer
-- pre-check in handlers_items.go gives users a friendly error message, but
-- it is a TOCTOU race on its own; this partial unique index is the actual
-- guard. NULL and empty-string slugs are excluded so the index only
-- constrains rows that opt into invocation routing. deleted_at IS NULL so
-- soft-deleted items don't block reuse of their old slug.
CREATE UNIQUE INDEX IF NOT EXISTS idx_items_invocation_slug_per_collection
    ON items(collection_id, (fields->>'invocation_slug'))
    WHERE fields->>'invocation_slug' IS NOT NULL
      AND fields->>'invocation_slug' <> ''
      AND deleted_at IS NULL;
