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
        'pattern', '^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$',
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
