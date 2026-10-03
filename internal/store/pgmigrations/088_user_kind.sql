-- Migration 088: a principal kind on users (SPEC-6 U4, TASK-3392).
-- Mirrors SQLite migration 114; see it for the rationale.
ALTER TABLE users ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'human' CHECK (kind IN ('human', 'app'));
