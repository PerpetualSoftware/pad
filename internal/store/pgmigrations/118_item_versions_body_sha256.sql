-- Migration 118 (Postgres): item_versions.body_sha256 (BUG-3820).
-- Postgres counterpart to migrations/148_item_versions_body_sha256.sql; the rationale is there.
ALTER TABLE item_versions ADD COLUMN IF NOT EXISTS body_sha256 TEXT;
