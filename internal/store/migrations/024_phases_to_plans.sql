-- Rename "Phases" collection to "Plans"
-- Guard against slug collision: only rename if 'plans' doesn't already exist in the same workspace
UPDATE collections
SET name = 'Plans',
    slug = 'plans',
    icon = '🗺️',
    prefix = 'PLAN',
    description = 'Plan and track project plans and milestones'
WHERE slug = 'phases'
AND NOT EXISTS (
    SELECT 1 FROM collections c2
    WHERE c2.workspace_id = collections.workspace_id AND c2.slug = 'plans'
);

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

-- Note: document type 'phase-plan' → 'plan' is handled in 025_doc_type_plan.sql
-- which recreates the table with the new CHECK constraint first.
