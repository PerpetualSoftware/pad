SET LOCAL lock_timeout = '5s';
ALTER TABLE item_schedules
    ADD COLUMN IF NOT EXISTS target_days INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS target_secs INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS earliest_days INTEGER,
    ADD COLUMN IF NOT EXISTS earliest_secs INTEGER CHECK ((earliest_days IS NULL) = (earliest_secs IS NULL)),
    ADD COLUMN IF NOT EXISTS deadline_days INTEGER,
    ADD COLUMN IF NOT EXISTS deadline_secs INTEGER CHECK ((deadline_days IS NULL) = (deadline_secs IS NULL)),
    ADD COLUMN IF NOT EXISTS deadline_behavior TEXT CHECK (deadline_behavior IN ('lapses','stays_owed')) CHECK ((deadline_days IS NULL) = (deadline_behavior IS NULL)),
    ADD COLUMN IF NOT EXISTS occupied_days INTEGER,
    ADD COLUMN IF NOT EXISTS occupied_secs INTEGER CHECK ((occupied_days IS NULL) = (occupied_secs IS NULL)),
    ADD COLUMN IF NOT EXISTS outcome TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS roles_origin TEXT NOT NULL DEFAULT 'legacy_backfill' CHECK (roles_origin IN ('legacy_backfill','native'));
ALTER TABLE schedule_occurrences
    ADD COLUMN IF NOT EXISTS actual_source TEXT CHECK (actual_source IN ('reported','recorded_now','legacy_unknown')),
    ADD COLUMN IF NOT EXISTS actual_done_at TEXT CHECK (actual_done_at IS NULL OR (actual_source IS NOT NULL AND actual_source IN ('reported','recorded_now')));
UPDATE item_schedules SET deadline_days = 0, deadline_secs = 0, deadline_behavior = 'stays_owed'
WHERE kind = 'deadline_hard' AND roles_origin = 'legacy_backfill';
UPDATE item_schedules SET occupied_days = substr(dtend,1,10)::date - substr(dtstart,1,10)::date,
    occupied_secs = CASE WHEN length(dtstart) = 10 THEN 0 ELSE EXTRACT(EPOCH FROM (substr(dtend,12,8)::time - substr(dtstart,12,8)::time))::int END
WHERE kind = 'window' AND roles_origin = 'legacy_backfill';
UPDATE schedule_occurrences SET actual_source = 'legacy_unknown' WHERE actual_source IS NULL AND actual_done_at IS NULL;
CREATE TABLE IF NOT EXISTS schedule_occurrence_overrides (
    id TEXT PRIMARY KEY,
    schedule_id TEXT NOT NULL REFERENCES item_schedules(id) ON DELETE CASCADE,
    occurrence_key TEXT NOT NULL,
    role TEXT NOT NULL CHECK (role IN ('target','earliest','deadline','occupied_until')),
    value_at TEXT NOT NULL,
    value_wall TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE (schedule_id, occurrence_key, role)
);
CREATE TABLE IF NOT EXISTS schedule_changes (
    id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    item_id TEXT NOT NULL,
    schedule_id TEXT NOT NULL,
    occurrence_key TEXT,
    change_type TEXT NOT NULL CHECK (change_type IN ('create','update','delete','move','snooze','done','skip','missed_recorded','end','import','carry')),
    role TEXT CHECK (role IN ('target','earliest','deadline','occupied_until')),
    before_json TEXT,
    after_json TEXT,
    actor TEXT NOT NULL DEFAULT '',
    reason TEXT NOT NULL DEFAULT '',
    reason_source TEXT NOT NULL DEFAULT 'system' CHECK (reason_source IN ('system','user')),
    root_change_id TEXT NOT NULL,
    parent_change_id TEXT,
    recorded_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_schedule_changes_schedule ON schedule_changes(schedule_id, recorded_at);
CREATE INDEX IF NOT EXISTS idx_schedule_changes_item ON schedule_changes(workspace_id, item_id, recorded_at);
