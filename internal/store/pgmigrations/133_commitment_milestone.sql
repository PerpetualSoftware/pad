SET LOCAL lock_timeout = '5s';
-- Milestone forecasts and outcomes (TASK-4358). A forecast is a hand-set
-- estimate beside the schedule, never one of its roles: it lives in its own
-- table so the deadline evaluator can never read it. A milestone selects
-- children whose success (or an explicit waiver) is its outcome.
CREATE TABLE IF NOT EXISTS schedule_forecasts (
    schedule_id TEXT PRIMARY KEY REFERENCES item_schedules(id) ON DELETE CASCADE,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    item_id TEXT NOT NULL,
    value TEXT NOT NULL,
    set_by TEXT NOT NULL DEFAULT '',
    set_by_kind TEXT NOT NULL CHECK (set_by_kind IN ('user','agent')),
    set_at TEXT NOT NULL,
    source TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS schedule_forecast_events (
    id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    item_id TEXT NOT NULL,
    schedule_id TEXT NOT NULL,
    action TEXT NOT NULL CHECK (action IN ('set','clear')),
    value TEXT NOT NULL DEFAULT '',
    prev_value TEXT NOT NULL DEFAULT '',
    set_by TEXT NOT NULL DEFAULT '',
    set_by_kind TEXT NOT NULL CHECK (set_by_kind IN ('user','agent')),
    source TEXT NOT NULL DEFAULT '',
    recorded_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_schedule_forecast_events_schedule ON schedule_forecast_events(schedule_id, recorded_at);
CREATE TABLE IF NOT EXISTS milestone_children (
    id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    schedule_id TEXT NOT NULL REFERENCES item_schedules(id) ON DELETE CASCADE,
    child_item_id TEXT NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK (kind IN ('child','waiver')),
    reason TEXT NOT NULL DEFAULT '' CHECK (kind = 'child' OR reason <> ''),
    added_by TEXT NOT NULL DEFAULT '',
    added_at TEXT NOT NULL,
    removed_by TEXT,
    removed_at TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_milestone_children_active ON milestone_children(schedule_id, child_item_id) WHERE removed_at IS NULL;
CREATE TABLE IF NOT EXISTS milestone_completion_evidence (
    id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    schedule_id TEXT NOT NULL,
    occurrence_key TEXT NOT NULL,
    child_item_id TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('child','waiver')),
    close_state TEXT NOT NULL CHECK (close_state IN ('open','done','abandoned','missing')),
    transition_seq INTEGER NOT NULL DEFAULT 0,
    done_value TEXT NOT NULL DEFAULT '',
    recorded_at TEXT NOT NULL,
    UNIQUE (schedule_id, occurrence_key, child_item_id)
);
