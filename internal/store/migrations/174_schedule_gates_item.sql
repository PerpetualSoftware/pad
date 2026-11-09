-- The gated side of a commitment gate is the ITEM, not its schedule (TASK-4353).
-- An item with no commitment can be gated, and deleting or ending either
-- schedule no longer decides whether a gate exists:
--   * blocked_item_id replaces blocked_schedule_id (NOT NULL, the gated item);
--   * blocker_schedule_id is set to NULL when the blocker schedule is deleted,
--     so the gate stays and is judged by the blocker item status;
--   * one gate per gated item and blocker item, and never an item gating itself.
-- SQLite cannot change a column constraint in place, so the table is rebuilt and
-- every row is carried over through the schedule that named its gated item.
PRAGMA foreign_keys = OFF;

CREATE TABLE schedule_gates_new (
    id                     TEXT PRIMARY KEY,
    workspace_id           TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    blocked_item_id        TEXT NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    blocker_item_id        TEXT NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    blocker_schedule_id    TEXT REFERENCES item_schedules(id) ON DELETE SET NULL,
    blocker_occurrence_key TEXT NOT NULL DEFAULT '',
    opened_at              TEXT,
    created_at             TEXT NOT NULL,
    CHECK (blocked_item_id <> blocker_item_id),
    UNIQUE (blocked_item_id, blocker_item_id)
);

INSERT INTO schedule_gates_new
    (id, workspace_id, blocked_item_id, blocker_item_id, blocker_schedule_id, blocker_occurrence_key, opened_at, created_at)
SELECT g.id, g.workspace_id, sc.item_id, g.blocker_item_id, g.blocker_schedule_id, g.blocker_occurrence_key, g.opened_at, g.created_at
FROM schedule_gates g
JOIN item_schedules sc ON sc.id = g.blocked_schedule_id;

DROP TABLE schedule_gates;
ALTER TABLE schedule_gates_new RENAME TO schedule_gates;
CREATE INDEX IF NOT EXISTS idx_schedule_gates_blocker ON schedule_gates(blocker_item_id);
CREATE INDEX IF NOT EXISTS idx_schedule_gates_workspace ON schedule_gates(workspace_id);

PRAGMA foreign_keys = ON;
