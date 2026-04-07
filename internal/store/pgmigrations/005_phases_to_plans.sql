-- Rename "Phases" collection to "Plans"
UPDATE collections
SET name = 'Plans',
    slug = 'plans',
    icon = '🗺️',
    prefix = 'PLAN',
    description = 'Plan and track project plans and milestones'
WHERE slug = 'phases';

-- Update convention trigger options: on-phase-start → on-plan-start, on-phase-complete → on-plan-complete
UPDATE collections
SET schema = REPLACE(REPLACE(schema, 'on-phase-start', 'on-plan-start'), 'on-phase-complete', 'on-plan-complete')
WHERE slug = 'conventions';

UPDATE collections
SET schema = REPLACE(REPLACE(schema, 'on-phase-start', 'on-plan-start'), 'on-phase-complete', 'on-plan-complete')
WHERE slug = 'playbooks';

-- Update any convention/playbook items with old trigger names in fields JSON
UPDATE items
SET fields = REPLACE(REPLACE(fields, '"on-phase-start"', '"on-plan-start"'), '"on-phase-complete"', '"on-plan-complete"')
WHERE collection_id IN (SELECT id FROM collections WHERE slug = 'conventions' OR slug = 'playbooks')
AND (fields LIKE '%on-phase-start%' OR fields LIKE '%on-phase-complete%');
