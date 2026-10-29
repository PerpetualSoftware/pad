-- item_schedules: one recurring or dated time trigger per item (TASK-4102, DOC-4098).
--
-- A schedule is the RULE; the outstanding occurrence is a MANAGED row in
-- item_reminders (schedule_id set), so the existing tick, arbiter, pending
-- list and outbox keep working unchanged. One managed row per schedule is the
-- one-outstanding-occurrence invariant, enforced by uq_item_reminders_schedule.
--
-- Walls (dtstart, dtend, rdates, exdates, next_wall, anchor_wall) are zone-less
-- readings in the schedule.Wall text form: "2026-11-02" (all-day) or
-- "2026-11-02T09:00:00". Instants (next_at, next_override_at, ended_at, ...)
-- are RFC3339 UTC written by Go.
CREATE TABLE IF NOT EXISTS item_schedules (
    id               TEXT PRIMARY KEY,
    workspace_id     TEXT NOT NULL,
    item_id          TEXT NOT NULL,
    kind             TEXT NOT NULL CHECK (kind IN ('deadline_hard','deadline_soft','check','window')),
    dtstart          TEXT NOT NULL,
    dtend            TEXT,                       -- windows only (same all-day-ness as dtstart)
    tz               TEXT,                       -- IANA; NULL iff floating
    floating         INTEGER NOT NULL DEFAULT 0 CHECK (floating IN (0,1)),
    rrule            TEXT,                       -- schedule.NormalizeRule output; NULL = one occurrence at dtstart
    rdates           TEXT NOT NULL DEFAULT '[]', -- JSON array of walls (schedule.Spec.RDates)
    exdates          TEXT NOT NULL DEFAULT '[]', -- same, Spec.ExDates
    anchor           TEXT NOT NULL DEFAULT 'calendar' CHECK (anchor IN ('calendar','completion')),
    audience         TEXT NOT NULL DEFAULT 'both' CHECK (audience IN ('human','agent','both')),
    lead_minutes     INTEGER NOT NULL DEFAULT 0 CHECK (lead_minutes >= 0 AND lead_minutes <= 44640),
    -- the one outstanding occurrence (NULL when ended)
    next_at          TEXT,                       -- UTC. pinned: the occurrence instant. floating: the EARLIEST
                                                 -- instant any zone could give next_wall (read at UTC+14)
    next_wall        TEXT,                       -- the wall reading of the outstanding occurrence
    next_override_at TEXT,                       -- UTC; snooze/move replaces the base instant before the lead
    anchor_wall      TEXT,                       -- re-anchor: wall of the last fired occurrence, NULL = use dtstart
    ended_at         TEXT,
    -- NULL = healthy. Otherwise why the stored rule could not be advanced, never
    -- third-party text: a schedule.Code (syntax, unknown_part, duplicate_part,
    -- missing_part, range, combination, unsupported, unsatisfiable, zone, wall) or
    -- a cap code (cap_scan, cap_stall, cap_time, cap_window, cap_instances).
    -- Cleared by any timing edit that compiles.
    error_code       TEXT,
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL,
    UNIQUE (item_id),
    FOREIGN KEY (item_id, workspace_id) REFERENCES items(id, workspace_id) ON DELETE CASCADE,
    CHECK ((floating = 1 AND tz IS NULL) OR (floating = 0 AND tz IS NOT NULL AND tz <> '')),
    CHECK (kind = 'window' OR dtend IS NULL),
    CHECK (kind <> 'window' OR dtend IS NOT NULL),
    CHECK (ended_at IS NULL OR next_at IS NULL)
);
CREATE INDEX IF NOT EXISTS idx_item_schedules_next
    ON item_schedules(next_at) WHERE next_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_item_schedules_ws_next
    ON item_schedules(workspace_id, next_at) WHERE next_at IS NOT NULL;

-- Resolved occurrences (written by TASK-4103). The identity is the UTC instant,
-- not the wall: a wall repeats across a fall-back on bare HOURLY/MINUTELY.
CREATE TABLE IF NOT EXISTS schedule_occurrences (
    id              TEXT PRIMARY KEY,
    schedule_id     TEXT NOT NULL REFERENCES item_schedules(id) ON DELETE CASCADE,
    occurrence_at   TEXT NOT NULL,
    -- for an all-day occurrence the date is ALSO an identity: At is local
    -- midnight and shifts when the zone changes
    occurrence_wall TEXT NOT NULL,
    state           TEXT NOT NULL CHECK (state IN ('done','skipped','moved','missed')),
    comment_id      TEXT,                        -- no FK: a comment delete must not block or cascade here
    actor           TEXT NOT NULL DEFAULT '',
    resolved_at     TEXT NOT NULL,
    UNIQUE (schedule_id, occurrence_at)
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_schedule_occurrences_allday_wall
    ON schedule_occurrences(schedule_id, occurrence_wall) WHERE length(occurrence_wall) = 10;

-- The managed reminder row: schedule_id is the only marker.
ALTER TABLE item_reminders ADD COLUMN schedule_id TEXT REFERENCES item_schedules(id) ON DELETE CASCADE;
CREATE UNIQUE INDEX IF NOT EXISTS uq_item_reminders_schedule
    ON item_reminders(schedule_id) WHERE schedule_id IS NOT NULL;
