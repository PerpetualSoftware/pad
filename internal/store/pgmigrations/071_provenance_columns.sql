-- Migration 071: record WHERE a stored name came from (BUG-2819).
-- Postgres twin of SQLite migration 096; see that file for the rationale and
-- the meaning of each value. Existing rows take 'unknown', never 'caller'.
ALTER TABLE attachments ADD COLUMN IF NOT EXISTS filename_source TEXT NOT NULL DEFAULT 'unknown'
    CHECK (filename_source IN ('caller', 'normalised', 'substituted', 'derived', 'unknown'));
ALTER TABLE mcp_audit_log ADD COLUMN IF NOT EXISTS tool_name_source TEXT NOT NULL DEFAULT 'unknown'
    CHECK (tool_name_source IN ('caller', 'sanitised', 'synthesised', 'unknown'));
