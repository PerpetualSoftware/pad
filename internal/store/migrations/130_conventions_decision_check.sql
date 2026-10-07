-- TASK-3119: add the optional `decision_check` field (on/off) to every
-- workspace's conventions collection. It switches a convention's
-- decision-provider check ("Possibly breaks CONVE-N") off. ABSENT reads as
-- on, so the field has no default: one would be written into every new
-- convention and make activated built-ins read as diverged (TASK-3462).
--
-- Keyed on the convention TRAIT, not the slug, so a renamed conventions
-- collection gets it too (BUG-2702). JSON-aware and idempotent, like 018: a
-- customised schema keeps its fields and order, and one that already has the
-- key is left alone.
UPDATE collections
SET schema = json_insert(
    schema,
    '$.fields[#]',
    json('{"key":"decision_check","label":"Convention check","type":"select","options":["on","off"]}')
)
WHERE json_valid(traits)
  AND json_extract(traits, '$.artifact_kind.kind') = 'convention'
  AND json_valid(schema)
  AND json_type(schema, '$.fields') = 'array'
  AND NOT EXISTS (
      SELECT 1
      FROM json_each(collections.schema, '$.fields')
      WHERE json_extract(json_each.value, '$.key') = 'decision_check'
  );
