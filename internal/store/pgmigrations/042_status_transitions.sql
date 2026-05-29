-- Migration 042 (Postgres): structured status-transition log
-- (PLAN-1628 / TASK-1637).
--
-- Postgres counterpart to migrations/063_status_transitions.sql. Same intent:
-- a structured, queryable record of every item status change — written in the
-- same transaction as the item update, never debounced — so the Reports
-- aggregation can compute the completed-throughput and cycle-time series
-- without parsing the human-readable, debounce-coalesced activities feed.
--
-- Timestamps are stored as RFC3339 TEXT to match item_versions and the rest
-- of the schema (the application layer formats UTC strings via dialect.Now).
--
-- Historical rows are backfilled once at startup by
-- Store.BackfillStatusTransitions (parses activities.metadata.changes).

-- field_key records WHICH select field the transition tracks (usually
-- "status"; a collection may designate another via BoardGroupBy, e.g. hiring
-- Candidates → "stage"). from_status/to_status hold that field's old/new value.
CREATE TABLE IF NOT EXISTS status_transitions (
    id            TEXT PRIMARY KEY,
    item_id       TEXT NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    workspace_id  TEXT NOT NULL REFERENCES workspaces(id),
    collection_id TEXT NOT NULL,
    field_key     TEXT NOT NULL DEFAULT 'status',
    from_status   TEXT NOT NULL DEFAULT '',
    to_status     TEXT NOT NULL,
    created_at    TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_status_transitions_ws_time
    ON status_transitions(workspace_id, created_at);

CREATE INDEX IF NOT EXISTS idx_status_transitions_item
    ON status_transitions(item_id, created_at);
