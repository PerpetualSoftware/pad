-- Rename "Phases" collection to "Plans"
-- Guard against slug collision: only rename if 'plans' doesn't already exist in the same workspace
UPDATE collections c
SET name = 'Plans',
    slug = 'plans',
    icon = '🗺️',
    prefix = 'PLAN',
    description = 'Plan and track project plans and milestones'
WHERE c.slug = 'phases'
AND NOT EXISTS (
    SELECT 1 FROM collections c2
    WHERE c2.workspace_id = c.workspace_id AND c2.slug = 'plans'
);

-- Update convention trigger options: on-phase-start → on-plan-start, on-phase-complete → on-plan-complete
-- Cast JSONB to text for REPLACE, then back to JSONB
UPDATE collections
SET schema = REPLACE(REPLACE(schema::text, 'on-phase-start', 'on-plan-start'), 'on-phase-complete', 'on-plan-complete')::jsonb
WHERE slug = 'conventions';

UPDATE collections
SET schema = REPLACE(REPLACE(schema::text, 'on-phase-start', 'on-plan-start'), 'on-phase-complete', 'on-plan-complete')::jsonb
WHERE slug = 'playbooks';

-- Update any convention/playbook items with old trigger names in fields JSON
-- Cast JSONB to text for REPLACE/LIKE, then back to JSONB
UPDATE items
SET fields = REPLACE(REPLACE(fields::text, '"on-phase-start"', '"on-plan-start"'), '"on-phase-complete"', '"on-plan-complete"')::jsonb
WHERE collection_id IN (SELECT id FROM collections WHERE slug = 'conventions' OR slug = 'playbooks')
AND (fields::text LIKE '%on-phase-start%' OR fields::text LIKE '%on-phase-complete%');
