-- Resolved occurrences keep the role offsets in force when they were resolved
-- (TASK-4714), so a later series offset change no longer moves history.
--
-- The roles_ prefix is deliberate: an unprefixed target_days here would be
-- captured by the unqualified item_schedules references in correlated
-- subqueries over schedule_occurrences.
--
-- Table-level CHECKs need a rebuild on SQLite, as in 173. Every index and
-- NUL guard on the table is re-created below in the same transaction.
PRAGMA foreign_keys = OFF;

CREATE TABLE schedule_occurrences_new (
    id                      TEXT PRIMARY KEY,
    schedule_id             TEXT NOT NULL REFERENCES item_schedules(id) ON DELETE CASCADE,
    occurrence_at           TEXT NOT NULL,
    occurrence_wall         TEXT NOT NULL,
    state                   TEXT NOT NULL CHECK (state IN ('done','skipped','moved','missed','lapsed')),
    comment_id              TEXT,
    actor                   TEXT NOT NULL DEFAULT '',
    resolved_at             TEXT NOT NULL,
    actual_source           TEXT CHECK (actual_source IN ('reported','recorded_now','legacy_unknown')),
    actual_done_at          TEXT CHECK (actual_done_at IS NULL OR (actual_source IS NOT NULL AND actual_source IN ('reported','recorded_now'))),
    roles_target_days       INTEGER,
    roles_target_secs       INTEGER,
    roles_earliest_days     INTEGER,
    roles_earliest_secs     INTEGER,
    roles_deadline_days     INTEGER,
    roles_deadline_secs     INTEGER,
    roles_deadline_behavior TEXT,
    roles_occupied_days     INTEGER,
    roles_occupied_secs     INTEGER,
    UNIQUE (schedule_id, occurrence_at),
    CONSTRAINT schedule_occurrences_roles_target_check CHECK ((roles_target_days IS NULL) = (roles_target_secs IS NULL)),
    CONSTRAINT schedule_occurrences_roles_earliest_check CHECK ((roles_earliest_days IS NULL) = (roles_earliest_secs IS NULL)),
    CONSTRAINT schedule_occurrences_roles_deadline_check CHECK ((roles_deadline_days IS NULL) = (roles_deadline_secs IS NULL)),
    CONSTRAINT schedule_occurrences_roles_deadline_behavior_check CHECK (
        (roles_deadline_behavior IS NULL OR roles_deadline_behavior IN ('lapses','stays_owed'))
        AND ((roles_deadline_days IS NULL) = (roles_deadline_behavior IS NULL))),
    CONSTRAINT schedule_occurrences_roles_occupied_check CHECK ((roles_occupied_days IS NULL) = (roles_occupied_secs IS NULL)),
    CONSTRAINT schedule_occurrences_roles_state_check CHECK (state <> 'moved' OR roles_target_days IS NULL)
);

INSERT INTO schedule_occurrences_new
    (id,schedule_id,occurrence_at,occurrence_wall,state,comment_id,actor,resolved_at,actual_source,actual_done_at)
SELECT id,schedule_id,occurrence_at,occurrence_wall,state,comment_id,actor,resolved_at,actual_source,actual_done_at
FROM schedule_occurrences;

DROP TABLE schedule_occurrences;
ALTER TABLE schedule_occurrences_new RENAME TO schedule_occurrences;
CREATE UNIQUE INDEX uq_schedule_occurrences_allday_wall
    ON schedule_occurrences(schedule_id, occurrence_wall) WHERE length(occurrence_wall) = 10;

-- Backfill: every resolved row that is not moved takes the offsets of its
-- schedule now, which is what it displays today. A moved row stays unstamped.
UPDATE schedule_occurrences SET
    (roles_target_days, roles_target_secs, roles_earliest_days, roles_earliest_secs,
     roles_deadline_days, roles_deadline_secs, roles_deadline_behavior,
     roles_occupied_days, roles_occupied_secs) =
    (SELECT sc.target_days, sc.target_secs, sc.earliest_days, sc.earliest_secs,
            sc.deadline_days, sc.deadline_secs, sc.deadline_behavior,
            sc.occupied_days, sc.occupied_secs
     FROM item_schedules sc WHERE sc.id = schedule_occurrences.schedule_id)
WHERE state <> 'moved';

-- Restore the NUL guards dropped by the rebuild in the same transaction.
CREATE TRIGGER IF NOT EXISTS pad_nul163_schedule_occurrences_actor_ins
BEFORE INSERT ON schedule_occurrences
FOR EACH ROW WHEN NEW.actor IS NOT NULL AND (
			instr(NEW.actor, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: schedule_occurrences.actor must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul163_schedule_occurrences_actor_upd
BEFORE UPDATE OF actor ON schedule_occurrences
FOR EACH ROW WHEN NEW.actor IS NOT NULL AND (
			instr(NEW.actor, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: schedule_occurrences.actor must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul163_schedule_occurrences_occurrence_wall_ins
BEFORE INSERT ON schedule_occurrences
FOR EACH ROW WHEN NEW.occurrence_wall IS NOT NULL AND (
			instr(NEW.occurrence_wall, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: schedule_occurrences.occurrence_wall must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul163_schedule_occurrences_occurrence_wall_upd
BEFORE UPDATE OF occurrence_wall ON schedule_occurrences
FOR EACH ROW WHEN NEW.occurrence_wall IS NOT NULL AND (
			instr(NEW.occurrence_wall, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: schedule_occurrences.occurrence_wall must not contain a NUL');
END;

PRAGMA foreign_keys = ON;
