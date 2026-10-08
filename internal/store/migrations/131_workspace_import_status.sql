-- Migration 131: a durable marker for a partly imported workspace (TASK-896).
--
-- A bundle import that hits a data error AFTER the workspace exists keeps the
-- partial workspace for its owner to inspect (the KEEP path in
-- handlers_import_bundle.go). Until now the only record was the in-memory
-- outcome registry, which a restart loses and only the importer's key reads.
--
-- status is 'partial' (the only value written today). note is SERVER-COMPOSED:
-- a fixed category, a short fixed detail, and a correlation id the server log
-- carries with the raw error. It never holds request bytes, paths, hosts or
-- driver text (lead ruling, day 89).
--
-- A side table rather than columns on workspaces: the workspace row is read
-- at nine SELECT sites, and only the workspace GET/list surface this.
-- Instance-local: not exported, never written from a bundle. Cleared by the
-- owner, or with the workspace (ON DELETE CASCADE and the purge list).
CREATE TABLE IF NOT EXISTS workspace_import_status (
    workspace_id TEXT PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
    status       TEXT NOT NULL,
    note         TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL
);
