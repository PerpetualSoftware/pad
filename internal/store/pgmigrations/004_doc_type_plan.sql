-- Rename doc_type 'phase-plan' to 'plan' in the CHECK constraint.
-- PostgreSQL supports dropping and re-adding constraints.
--
-- The old constraint goes FIRST (BUG-3752): 001's CHECK rejects 'plan', and
-- Postgres checks each row as it is updated, so updating before the drop
-- failed on any database holding a phase-plan document, on every startup.
-- The runner wraps the file in one transaction, so no window exists without
-- a CHECK. Databases that already applied 004 never run it again: the runner
-- tracks migrations by name.
ALTER TABLE documents DROP CONSTRAINT IF EXISTS documents_doc_type_check;

UPDATE documents SET doc_type = 'plan' WHERE doc_type = 'phase-plan';

ALTER TABLE documents ADD CONSTRAINT documents_doc_type_check
    CHECK (doc_type IN ('roadmap','plan','architecture','ideation',
                        'feature-spec','notes','prompt-library','reference'));
