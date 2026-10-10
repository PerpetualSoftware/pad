-- Migration 135: system collections hold reference material (PLAN-3535).
--
-- CollectionSettings.tracks_work: false makes a collection REFERENCE material,
-- whose items leave parent progress, close blocking, Insights and open-work
-- counts. Absent means work, today's behaviour, so only the collections whose
-- answer changes are written: the system ones (Conventions, Playbooks), as
-- Dave ruled. Existing Docs collections stay work (owners can flip them); a
-- NEW template Docs collection is created as reference.
--
-- Only rows whose settings parse as a JSON object are touched; anything else
-- keeps reading as work. The CASE fixes the evaluation order: json_type and
-- json_extract error on malformed input, and a WHERE clause does not promise
-- to test json_valid first.
UPDATE collections
SET settings = json_set(COALESCE(NULLIF(TRIM(settings), ''), '{}'), '$.tracks_work', json('false'))
WHERE is_system = 1
  AND CASE
        WHEN json_valid(COALESCE(NULLIF(TRIM(settings), ''), '{}')) THEN
          json_type(COALESCE(NULLIF(TRIM(settings), ''), '{}')) = 'object'
          AND json_extract(COALESCE(NULLIF(TRIM(settings), ''), '{}'), '$.tracks_work') IS NULL
        ELSE 0
      END;
