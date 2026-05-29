-- Migration 063: structured status-transition log (PLAN-1628 / TASK-1637).
--
-- The Reports surface needs a reliable timestamp for WHEN an item entered a
-- given status — specifically a terminal ("completed") status — to compute
-- the completed-throughput and cycle-time series. The activities table
-- records status changes only as a human-readable `metadata.changes` string
-- ("status: open → done") AND is debounce-coalesced, so it cannot be queried
-- reliably for aggregation. This table captures every status transition as
-- a structured, queryable row, written in the SAME transaction as the item
-- update and NEVER debounced.
--
-- Historical rows are backfilled once at startup by
-- Store.BackfillStatusTransitions, which parses activities.metadata.changes
-- — mirroring the BackfillWikiLinks pattern. The Go side owns the parsing
-- because the UTF-8 arrow ("→") and the "key: from → to" grammar are awkward
-- to handle in portable SQL.

-- field_key records WHICH select field the transition tracks. It is almost
-- always "status", but a collection can designate another select field as its
-- workflow/done field via CollectionSettings.BoardGroupBy (e.g. hiring
-- Candidates group by "stage"/"result"). Storing the key per row keeps the
-- table correct even if a collection's BoardGroupBy changes later. The
-- from_status/to_status columns hold that field's old/new value.
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

-- Primary aggregation access path: "transitions in this workspace within a
-- time window", date-bucketed by created_at.
CREATE INDEX IF NOT EXISTS idx_status_transitions_ws_time
    ON status_transitions(workspace_id, created_at);

-- Per-item history: cycle-time joins created_at of the item to its terminal
-- transition.
CREATE INDEX IF NOT EXISTS idx_status_transitions_item
    ON status_transitions(item_id, created_at);
