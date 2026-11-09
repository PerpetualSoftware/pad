-- Rebuild the occurrence CHECK while preserving all history and identities.
PRAGMA foreign_keys = OFF;

CREATE TABLE schedule_occurrences_new (
    id              TEXT PRIMARY KEY,
    schedule_id     TEXT NOT NULL REFERENCES item_schedules(id) ON DELETE CASCADE,
    occurrence_at   TEXT NOT NULL,
    occurrence_wall TEXT NOT NULL,
    state           TEXT NOT NULL CHECK (state IN ('done','skipped','moved','missed','lapsed')),
    comment_id      TEXT,
    actor           TEXT NOT NULL DEFAULT '',
    resolved_at     TEXT NOT NULL,
    actual_source   TEXT CHECK (actual_source IN ('reported','recorded_now','legacy_unknown')),
    actual_done_at  TEXT CHECK (actual_done_at IS NULL OR (actual_source IS NOT NULL AND actual_source IN ('reported','recorded_now'))),
    UNIQUE (schedule_id, occurrence_at)
);

INSERT INTO schedule_occurrences_new
    (id,schedule_id,occurrence_at,occurrence_wall,state,comment_id,actor,resolved_at,actual_source,actual_done_at)
SELECT id,schedule_id,occurrence_at,occurrence_wall,state,comment_id,actor,resolved_at,actual_source,actual_done_at
FROM schedule_occurrences;

DROP TABLE schedule_occurrences;
ALTER TABLE schedule_occurrences_new RENAME TO schedule_occurrences;
CREATE UNIQUE INDEX uq_schedule_occurrences_allday_wall
    ON schedule_occurrences(schedule_id, occurrence_wall) WHERE length(occurrence_wall) = 10;

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
