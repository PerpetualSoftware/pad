-- Migration 103 (Postgres): the conventions `decision_check` field (TASK-3119).
-- Postgres counterpart to migrations/130_conventions_decision_check.sql; the
-- rationale is there. Keyed on the convention trait, idempotent.
UPDATE collections
SET schema = jsonb_set(
    schema,
    '{fields}',
    (schema->'fields') || jsonb_build_object(
        'key', 'decision_check',
        'label', 'Convention check',
        'type', 'select',
        'options', jsonb_build_array('on', 'off')
    )
)
WHERE traits->'artifact_kind'->>'kind' = 'convention'
  AND jsonb_typeof(schema->'fields') = 'array'
  AND NOT EXISTS (
      SELECT 1
      FROM jsonb_array_elements(schema->'fields') f
      WHERE f->>'key' = 'decision_check'
  );
