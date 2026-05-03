-- Migration 028 (Postgres): MCP audit log table.
--
-- Postgres counterpart to migrations/049_mcp_audit.sql. Schema
-- intentionally mirrors the SQLite version; the only dialect tweak is
-- INTEGER → INT for latency_ms (Postgres preference, behaviour
-- identical) and the inline FK syntax. See the SQLite file for the
-- full design rationale + the spec-deviation explanation.

CREATE TABLE IF NOT EXISTS mcp_audit_log (
    id            TEXT PRIMARY KEY,
    timestamp     TEXT NOT NULL,
    user_id       TEXT NOT NULL REFERENCES users(id),
    workspace_id  TEXT REFERENCES workspaces(id),
    token_kind    TEXT NOT NULL,
    token_ref     TEXT NOT NULL,
    tool_name     TEXT NOT NULL,
    args_hash     TEXT NOT NULL,
    result_status TEXT NOT NULL,
    error_kind    TEXT,
    latency_ms    INT NOT NULL,
    request_id    TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS mcp_audit_user_time_idx
    ON mcp_audit_log(user_id, timestamp DESC);

CREATE INDEX IF NOT EXISTS mcp_audit_token_time_idx
    ON mcp_audit_log(token_kind, token_ref, timestamp DESC);

CREATE INDEX IF NOT EXISTS mcp_audit_timestamp_idx
    ON mcp_audit_log(timestamp);
