-- Rename doc_type 'phase-plan' to 'plan' in the CHECK constraint.
-- PostgreSQL supports dropping and re-adding constraints.

-- Update existing rows first
UPDATE documents SET doc_type = 'plan' WHERE doc_type = 'phase-plan';

-- Drop the old constraint and add the new one
ALTER TABLE documents DROP CONSTRAINT IF EXISTS documents_doc_type_check;
ALTER TABLE documents ADD CONSTRAINT documents_doc_type_check
    CHECK (doc_type IN ('roadmap','plan','architecture','ideation',
                        'feature-spec','notes','prompt-library','reference'));
