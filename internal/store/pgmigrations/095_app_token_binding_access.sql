-- Migration 095: the consented access of a delegated app grant (SPEC-6 U5b,
-- TASK-3399). Mirrors SQLite migration 121; see it for the rationale.
ALTER TABLE app_token_bindings ADD COLUMN IF NOT EXISTS delegated_access TEXT;
