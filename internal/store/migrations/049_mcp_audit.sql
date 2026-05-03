-- Migration 049: MCP audit log table (PLAN-943 TASK-960).
--
-- Persistent log of every MCP tool call. Drives the connected-apps
-- "last used" + "30-day calls" columns (TASK-954), enables forensics,
-- and gives users + admins visibility into what their agent has been
-- doing. One row per request — both successful and rejected calls
-- are recorded so a brute-force / abuse pattern is visible.
--
-- Spec deviation from the original task body: the spec called for
-- `token_id UUID NOT NULL REFERENCES oauth_tokens(id)`, but pad's
-- actual OAuth schema (migrations/048_oauth.sql) has no oauth_tokens
-- table — instead there are oauth_access_tokens / oauth_refresh_tokens
-- keyed by signature, sharing a request_id chain identifier preserved
-- across rotations. The natural unit of "a connection" is therefore
-- the request_id. Additionally MCP requests can authenticate via PAT
-- (api_tokens.id) or OAuth (oauth_access_tokens.request_id), so a
-- single FK can't model both. We use:
--
--   - token_kind TEXT NOT NULL — 'oauth' or 'pat'
--   - token_ref  TEXT NOT NULL — the OAuth request_id, or the PAT id
--
-- No FK because the parent table varies. Both possible parents have
-- their own indexes; cleanup of stranded audit rows is handled by the
-- 90-day retention sweeper rather than by FK cascade.
--
-- Timestamps are TEXT (ISO8601 UTC) to match the cross-dialect
-- contract used everywhere else in pad — see migrations/048_oauth.sql
-- for the same pattern.

CREATE TABLE IF NOT EXISTS mcp_audit_log (
    id            TEXT PRIMARY KEY,                 -- UUID
    timestamp     TEXT NOT NULL,                    -- ISO8601 UTC, when the request started
    user_id       TEXT NOT NULL REFERENCES users(id),
    workspace_id  TEXT REFERENCES workspaces(id),   -- NULL for tools that don't target a workspace (pad_meta etc.)
    token_kind    TEXT NOT NULL,                    -- 'oauth' | 'pat'
    token_ref     TEXT NOT NULL,                    -- oauth.request_id (chain) OR api_tokens.id
    tool_name     TEXT NOT NULL,                    -- JSON-RPC method or tools/call params.name
    args_hash     TEXT NOT NULL,                    -- SHA-256 of canonical-JSON args; "" when no args
    result_status TEXT NOT NULL,                    -- 'ok' | 'error' | 'denied'
    error_kind    TEXT,                             -- only when result_status != 'ok'
    latency_ms    INTEGER NOT NULL,
    request_id    TEXT NOT NULL                     -- correlates with server logs (X-Request-ID)
);

-- (user_id, timestamp DESC) — drives the per-user audit query and
-- the "list my MCP activity" surface in connected-apps.
CREATE INDEX IF NOT EXISTS mcp_audit_user_time_idx
    ON mcp_audit_log(user_id, timestamp DESC);

-- (token_kind, token_ref, timestamp DESC) — drives per-connection
-- last-used + 30-day-count aggregates that TASK-954 reads to populate
-- each row of the connected-apps page.
CREATE INDEX IF NOT EXISTS mcp_audit_token_time_idx
    ON mcp_audit_log(token_kind, token_ref, timestamp DESC);

-- (timestamp) — drives the 90-day retention sweeper. Without this the
-- DELETE WHERE timestamp < ? scan grows linearly with table size.
CREATE INDEX IF NOT EXISTS mcp_audit_timestamp_idx
    ON mcp_audit_log(timestamp);
