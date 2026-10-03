-- Migration 081: mark rows a workspace import wrote (BUG-3379). Mirrors
-- SQLite migration 107; see it for the rationale.
ALTER TABLE comments ADD COLUMN IF NOT EXISTS imported INTEGER NOT NULL DEFAULT 0;
ALTER TABLE item_versions ADD COLUMN IF NOT EXISTS imported INTEGER NOT NULL DEFAULT 0;
ALTER TABLE attachments ADD COLUMN IF NOT EXISTS imported INTEGER NOT NULL DEFAULT 0;
