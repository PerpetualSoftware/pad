-- Migration 148: item_versions.body_sha256 (BUG-3820).
--
-- The hex sha256 of the full body a version row stands for: the older body
-- for a reverse-patch row, the stored content for a full-body row. The
-- reverse-patch walk checks every resolved body against it, because
-- diff.ApplyPatch matches fuzzily and a patch applied to the wrong base can
-- return a wrong body with no error. NULL for rows written before this
-- migration and for imported rows whose bundle carried none. Never
-- backfilled: a legacy row's true body cannot be known.
ALTER TABLE item_versions ADD COLUMN body_sha256 TEXT;
