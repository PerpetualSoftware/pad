-- Add optional "role" field to the Conventions collection schema.
-- This allows conventions to be scoped to a specific agent role (e.g. "implementer").
-- Conventions without a role value apply to all roles (backward compatible).
--
-- Use JSON-aware mutation so customized schemas still receive the new field even if
-- their field order differs or additional fields have been added.
UPDATE collections
SET schema = json_insert(
    schema,
    '$.fields[#]',
    json('{"key":"role","label":"Role","type":"text"}')
)
WHERE slug = 'conventions'
  AND json_valid(schema)
  AND json_type(schema, '$.fields') = 'array'
  AND NOT EXISTS (
      SELECT 1
      FROM json_each(collections.schema, '$.fields')
      WHERE json_extract(json_each.value, '$.key') = 'role'
  );
