-- Migration 059: per-OAuth-connection state (PLAN-1519 / TASK-1520 — Phase A foundation).
--
-- IDEA-1517 §2 promotes connection-level state — name, scope flags, mutable
-- workspace allow-list — out of `session.Extra["allowed_workspaces"]` (which
-- lives on EVERY row of oauth_access_tokens / oauth_refresh_tokens /
-- oauth_authorization_codes / oauth_pkce_requests and gets re-minted on
-- every refresh-token rotation) into dedicated tables keyed by `request_id`
-- (the OAuth grant chain identifier preserved across rotations — see
-- migration 048 oauth_*.request_id and internal/store/oauth.go).
--
-- Why `request_id`, not a separate connection ID: the grant chain ID is
-- the one identifier fosite preserves across refresh-token rotation, so
-- using it as the join key makes the connection-level state survive
-- rotation natively without any reconciliation logic.
--
-- This migration creates the empty tables and indexes. WRITE-PATH wiring
-- (consent screen → /authorize/decide → INSERT into oauth_connections) +
-- READ-PATH switch on /console/connected-apps come in Phase C. Phase A's
-- only behavioural change is the dual-read introspection gate at
-- internal/server/middleware_mcp_auth.go — which consults BOTH the legacy
-- session.Extra path AND any rows in these new tables, OR-merging the
-- allow-lists. New tables stay empty until Phase C, so the dual-read is
-- a no-op until the write path lands.

-- ============================================================
-- 1. oauth_connections — one row per OAuth grant chain
-- ============================================================
--
-- request_id is the OAuth grant chain identifier (fosite Requester.GetID(),
-- stable across refresh-token rotation). One row per "connection" the user
-- authorized via consent — that's the granularity the /console/connected-apps
-- page (TASK-954) already deduplicates by. Created in /authorize/decide
-- (Phase C); deleted on full chain revocation (Phase C explicit DELETE
-- after RevokeRefreshTokenFamily / RevokeAccessTokenFamily).
--
-- name defaults to '' so pre-migration rows (none expected here — Phase A
-- ships before any writes) and rows created by future code paths that
-- don't supply a name still satisfy NOT NULL. The connections-page UI
-- in Phase D will prompt the user to rename any '' row on next visit.
--
-- Scope flags all default to ON, matching IDEA-1517 §2a's "default-on at
-- /authorize" semantic — the common case is "this assistant works
-- everywhere across my pad." Users who want narrower scope toggle them
-- off either at consent time or post-hoc in /console/connected-apps.
--
-- updated_at: ON UPDATE trigger / app-level touch lives in the store
-- methods (`SetScopeFlags`, `RenameConnection`, etc.) — SQLite doesn't
-- support ON UPDATE clauses so we set updated_at explicitly on every
-- mutation. Postgres mirror does the same for consistency.
CREATE TABLE IF NOT EXISTS oauth_connections (
    request_id                  TEXT PRIMARY KEY,
    user_id                     TEXT NOT NULL,
    name                        TEXT NOT NULL DEFAULT '',
    may_create_workspaces       INTEGER NOT NULL DEFAULT 1,
    all_current_workspaces      INTEGER NOT NULL DEFAULT 1,
    include_future_workspaces   INTEGER NOT NULL DEFAULT 1,
    created_at                  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    updated_at                  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE INDEX IF NOT EXISTS idx_oauth_connections_user
    ON oauth_connections(user_id);

-- ============================================================
-- 2. oauth_connection_workspaces — mutable allow-list join table
-- ============================================================
--
-- Only consulted when oauth_connections.all_current_workspaces = 0. When
-- the flag is on, the connection covers every workspace the user is a
-- member of and the join table is irrelevant — `all_current_workspaces`
-- IS the wildcard semantic. (No `["*"]` sentinel in the new shape; the
-- legacy session.Extra wildcard maps to the flag during Phase C backfill.)
--
-- added_by records which path inserted the row. Useful for the Phase D
-- connections-page UI ("added by you" vs "auto-added when agent created
-- this workspace" vs "auto-added because Include Future is on") and for
-- security audits of which scope path produced the access. Allowed
-- values: 'user' | 'agent-create' | 'include-future' | 'claim' (Phase B
-- claim-code redemption). Stored as TEXT rather than an enum because
-- SQLite has no enum type and the cross-driver constraint isn't worth a
-- separate lookup table.
--
-- workspace_id is NOT a foreign key to workspaces. The workspaces table
-- has no DELETE-CASCADE chain into auth state today, and adding one would
-- create a tight cross-domain coupling between workspace deletion and
-- OAuth state cleanup. Instead, stale rows here are harmless — the
-- introspection gate joins through workspaces on read, so a missing
-- workspace_id simply yields zero rows. Phase C may add an explicit
-- cleanup hook on workspace delete; not in scope for the foundation.
--
-- PK (request_id, workspace_id) guarantees no duplicates. FK on
-- request_id ON DELETE CASCADE so revoking the connection sweeps its
-- allow-list entries in one statement.
CREATE TABLE IF NOT EXISTS oauth_connection_workspaces (
    request_id    TEXT NOT NULL,
    workspace_id  TEXT NOT NULL,
    added_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    added_by      TEXT NOT NULL DEFAULT 'user',
    PRIMARY KEY (request_id, workspace_id),
    FOREIGN KEY (request_id) REFERENCES oauth_connections(request_id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_oauth_conn_workspaces_ws
    ON oauth_connection_workspaces(workspace_id);
