-- Migration 112: an opaque worker id on the item execution lease (TASK-3613).
-- Mirrors SQLite migration 142; see it for the rationale. NULL for name-only
-- and pre-existing leases.
ALTER TABLE items ADD COLUMN IF NOT EXISTS lease_worker_id TEXT;
