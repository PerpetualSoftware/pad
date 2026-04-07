-- Rename "Phases" collection to "Plans"
UPDATE collections
SET name = 'Plans',
    slug = 'plans',
    icon = '🗺️',
    prefix = 'PLAN',
    description = 'Plan and track project plans and milestones'
WHERE slug = 'phases';

-- Update convention trigger options: on-phase-start → on-plan-start, on-phase-complete → on-plan-complete
-- Update the conventions collection schema to use new trigger names
UPDATE collections
SET schema = REPLACE(REPLACE(schema, 'on-phase-start', 'on-plan-start'), 'on-phase-complete', 'on-plan-complete')
WHERE slug = 'conventions';

-- Update the playbooks collection schema similarly
UPDATE collections
SET schema = REPLACE(REPLACE(schema, 'on-phase-start', 'on-plan-start'), 'on-phase-complete', 'on-plan-complete')
WHERE slug = 'playbooks';

-- Update any convention items that have trigger=on-phase-start or on-phase-complete in their fields JSON
UPDATE items
SET fields = REPLACE(REPLACE(fields, '"on-phase-start"', '"on-plan-start"'), '"on-phase-complete"', '"on-plan-complete"')
WHERE collection_id IN (SELECT id FROM collections WHERE slug = 'conventions' OR slug = 'playbooks')
AND (fields LIKE '%on-phase-start%' OR fields LIKE '%on-phase-complete%');

-- Update document types: phase-plan → plan
UPDATE documents
SET doc_type = 'plan'
WHERE doc_type = 'phase-plan';
