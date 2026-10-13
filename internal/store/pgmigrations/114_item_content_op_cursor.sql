-- Migration 114: BUG-3744, the op-log cursor the stored items.content covers.
-- Mirrors SQLite migration 144; see it for the rationale. (113 is BUG-3678's.)
ALTER TABLE items ADD COLUMN IF NOT EXISTS content_op_cursor BIGINT NOT NULL DEFAULT 0;
