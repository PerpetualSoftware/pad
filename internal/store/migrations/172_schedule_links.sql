-- Time links between schedules, per-occurrence link overrides, and commitment
-- gates (TASK-4353, DOC-4348 unit 5). A link makes one role date of a child
-- schedule follow one role date of a parent schedule in the same workspace.
-- Deleting either schedule removes its links and gates.
CREATE TABLE IF NOT EXISTS schedule_links (
    id                 TEXT PRIMARY KEY,
    workspace_id       TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    parent_schedule_id TEXT NOT NULL REFERENCES item_schedules(id) ON DELETE CASCADE,
    parent_role        TEXT NOT NULL CHECK (parent_role IN ('earliest','target','deadline','occupied_until')),
    child_schedule_id  TEXT NOT NULL REFERENCES item_schedules(id) ON DELETE CASCADE,
    child_role         TEXT NOT NULL CHECK (child_role IN ('earliest','target','deadline','occupied_until')),
    offset_mode        TEXT NOT NULL CHECK (offset_mode IN ('elapsed','civil')),
    offset_elapsed     INTEGER NOT NULL DEFAULT 0,
    offset_days        INTEGER NOT NULL DEFAULT 0,
    offset_clock       INTEGER NOT NULL DEFAULT 0 CHECK (offset_clock >= 0 AND offset_clock < 86400),
    offset_all_day     INTEGER NOT NULL DEFAULT 0 CHECK (offset_all_day IN (0, 1)),
    offset_zone        TEXT NOT NULL DEFAULT '',
    cancel_policy      TEXT NOT NULL DEFAULT 'waiting_on' CHECK (cancel_policy IN ('waiting_on','cancel_child_occurrence')),
    created_at         TEXT NOT NULL,
    updated_at         TEXT NOT NULL,
    CHECK (parent_schedule_id <> child_schedule_id),
    UNIQUE (child_schedule_id, child_role)
);
CREATE INDEX IF NOT EXISTS idx_schedule_links_parent ON schedule_links(parent_schedule_id);
CREATE INDEX IF NOT EXISTS idx_schedule_links_workspace ON schedule_links(workspace_id);

CREATE TABLE IF NOT EXISTS schedule_link_overrides (
    id                  TEXT PRIMARY KEY,
    link_id             TEXT NOT NULL REFERENCES schedule_links(id) ON DELETE CASCADE,
    occurrence_key      TEXT NOT NULL,
    offset_mode         TEXT CHECK (offset_mode IN ('elapsed','civil')),
    offset_elapsed      INTEGER NOT NULL DEFAULT 0,
    offset_days         INTEGER NOT NULL DEFAULT 0,
    offset_clock        INTEGER NOT NULL DEFAULT 0 CHECK (offset_clock >= 0 AND offset_clock < 86400),
    offset_all_day      INTEGER NOT NULL DEFAULT 0 CHECK (offset_all_day IN (0, 1)),
    offset_zone         TEXT NOT NULL DEFAULT '',
    no_longer_needed_at TEXT,
    created_at          TEXT NOT NULL,
    updated_at          TEXT NOT NULL,
    UNIQUE (link_id, occurrence_key)
);

CREATE TABLE IF NOT EXISTS schedule_gates (
    id                     TEXT PRIMARY KEY,
    workspace_id           TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    blocked_schedule_id    TEXT NOT NULL REFERENCES item_schedules(id) ON DELETE CASCADE,
    blocker_item_id        TEXT NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    blocker_schedule_id    TEXT REFERENCES item_schedules(id) ON DELETE CASCADE,
    blocker_occurrence_key TEXT NOT NULL DEFAULT '',
    opened_at              TEXT,
    created_at             TEXT NOT NULL,
    CHECK (blocker_schedule_id IS NULL OR blocker_schedule_id <> blocked_schedule_id),
    UNIQUE (blocked_schedule_id, blocker_item_id)
);
CREATE INDEX IF NOT EXISTS idx_schedule_gates_blocker ON schedule_gates(blocker_item_id);
CREATE INDEX IF NOT EXISTS idx_schedule_gates_workspace ON schedule_gates(workspace_id);
