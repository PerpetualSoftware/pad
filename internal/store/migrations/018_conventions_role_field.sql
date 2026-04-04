-- Add optional "role" field to the Conventions collection schema.
-- This allows conventions to be scoped to a specific agent role (e.g. "implementer").
-- Conventions without a role value apply to all roles (backward compatible).

-- Add the role field to existing conventions schemas (insert before the closing ])
UPDATE collections
SET schema = REPLACE(
    schema,
    '{"key":"priority","label":"Priority","type":"select","options":["must","should","nice-to-have"]}]',
    '{"key":"priority","label":"Priority","type":"select","options":["must","should","nice-to-have"]},{"key":"role","label":"Role","type":"text"}]'
)
WHERE slug = 'conventions'
  AND schema LIKE '%"priority"%'
  AND schema NOT LIKE '%"role"%';
