-- Migration 084: a per-account credential epoch (BUG-3382, TASK-3351).
-- Mirrors SQLite migration 110; see it for the rationale.
ALTER TABLE users ADD COLUMN IF NOT EXISTS credential_epoch BIGINT NOT NULL DEFAULT 0;
