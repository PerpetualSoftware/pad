-- Migration 120 (Postgres): decision_jobs.not_before (TASK-3916).
-- Postgres counterpart to migrations/150_decision_jobs_not_before.sql; the rationale is there.
ALTER TABLE decision_jobs ADD COLUMN IF NOT EXISTS not_before TEXT;
