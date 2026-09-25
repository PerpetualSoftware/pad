-- Migration 096: record WHERE a stored name came from (BUG-2819).
--
-- attachments.filename and mcp_audit_log.tool_name each hold EITHER what the
-- caller sent OR something the server substituted when that was unusable, in
-- the same column with no marker. The caller controls their half, so a caller
-- could send the substituted value itself (a file named "upload.bin", a tool
-- named "(unknown)") and no reader could tell the two apart. For an audit
-- surface that is the whole question.
--
-- filename_source:
--   caller      the caller's name, stored as sent
--   normalised  the caller's name, altered to be storable (runes dropped,
--               trailing dots trimmed, a Windows device name prefixed)
--   substituted the server's name; the caller's left nothing usable
--   derived     a server-built name from a parent row (thumbnail, transform)
--   unknown     written before this column existed
-- tool_name_source:
--   caller      the tool name as sent
--   sanitised   the caller's name, cleaned (it carries the "(sanitised) " mark)
--   synthesised the server's placeholder ("(unknown)" and similar)
--   unknown     written before this column existed
--
-- Existing rows take 'unknown' by DEFAULT, deliberately never 'caller': a row
-- nobody classified must not be claimed as caller-supplied.
--
-- NOTE: no `IF NOT EXISTS` — SQLite's ALTER TABLE ADD COLUMN rejects it.
ALTER TABLE attachments ADD COLUMN filename_source TEXT NOT NULL DEFAULT 'unknown'
    CHECK (filename_source IN ('caller', 'normalised', 'substituted', 'derived', 'unknown'));
ALTER TABLE mcp_audit_log ADD COLUMN tool_name_source TEXT NOT NULL DEFAULT 'unknown'
    CHECK (tool_name_source IN ('caller', 'sanitised', 'synthesised', 'unknown'));
