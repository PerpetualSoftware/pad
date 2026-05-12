-- Add optional `invocation_slug` and `arguments` fields to the Playbooks
-- collection schema for every existing workspace. Foundational migration for
-- PLAN-1377 — playbooks become first-class invokable procedures.
--
-- invocation_slug: kebab-case identifier that enables /pad <slug> direct
-- invocation. Nullable — playbooks meant only for trigger-based auto-load
-- (e.g. trigger=on-release checklists) don't need a slug. Uniqueness is
-- enforced at the application layer (see internal/server/handlers_items.go
-- checkUniqueFields) since the JSON-stored fields map can't carry a SQL
-- UNIQUE constraint.
--
-- arguments: JSON array of {name, type, required, default, description}
-- specs declaring the playbook's argument contract.
--
-- Use JSON-aware mutation so customized schemas still receive the new fields
-- even if their field order differs or additional fields have been added.
UPDATE collections
SET schema = json_insert(
    schema,
    '$.fields[#]',
    json('{"key":"invocation_slug","label":"Invocation slug","type":"text","pattern":"^[a-z0-9][a-z0-9-]*[a-z0-9]$","unique_scope":"workspace_collection"}')
)
WHERE slug = 'playbooks'
  AND json_valid(schema)
  AND json_type(schema, '$.fields') = 'array'
  AND NOT EXISTS (
      SELECT 1
      FROM json_each(collections.schema, '$.fields')
      WHERE json_extract(json_each.value, '$.key') = 'invocation_slug'
  );

UPDATE collections
SET schema = json_insert(
    schema,
    '$.fields[#]',
    json('{"key":"arguments","label":"Arguments","type":"json"}')
)
WHERE slug = 'playbooks'
  AND json_valid(schema)
  AND json_type(schema, '$.fields') = 'array'
  AND NOT EXISTS (
      SELECT 1
      FROM json_each(collections.schema, '$.fields')
      WHERE json_extract(json_each.value, '$.key') = 'arguments'
  );

-- Atomically prevent two concurrent writers from inserting items with the
-- same invocation_slug within the same collection. The application-layer
-- pre-check in handlers_items.go gives users a friendly error message, but
-- it is a TOCTOU race on its own; this partial unique index is the actual
-- guard. NULL and empty-string slugs are excluded so the index only
-- constrains rows that opt into invocation routing. deleted_at IS NULL so
-- soft-deleted items don't block reuse of their old slug.
CREATE UNIQUE INDEX IF NOT EXISTS idx_items_invocation_slug_per_collection
    ON items(collection_id, json_extract(fields, '$.invocation_slug'))
    WHERE json_extract(fields, '$.invocation_slug') IS NOT NULL
      AND json_extract(fields, '$.invocation_slug') != ''
      AND deleted_at IS NULL;
