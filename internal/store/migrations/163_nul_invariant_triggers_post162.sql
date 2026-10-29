-- Layer B of the NUL invariant (DOC-2823 S2), for columns added after 084.
--
-- GENERATED from internal/store/nulcolumns.go. Do not edit by hand — run
--   GEN_NUL_TRIGGERS=1 go test ./internal/store/ -run TestGenerateNULTriggerMigration
-- and commit the result. TestNULTriggersMatchTheList fails if this file and the
-- Go list disagree.
--
-- WHY A SEPARATE FILE (BUG-3108). A trigger can only follow its column. 084
-- runs before these columns exist, and SQLite re-parses every trigger during
-- an ALTER TABLE ... RENAME, so a trigger there naming a later column breaks
-- the next table rebuild. The predicate, the marker and the SQLite-only scope
-- are 084's; its header carries the measurements.

CREATE TRIGGER IF NOT EXISTS pad_nul163_item_schedules_anchor_wall_ins
BEFORE INSERT ON item_schedules
FOR EACH ROW WHEN NEW.anchor_wall IS NOT NULL AND (
			instr(NEW.anchor_wall, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: item_schedules.anchor_wall must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul163_item_schedules_anchor_wall_upd
BEFORE UPDATE OF anchor_wall ON item_schedules
FOR EACH ROW WHEN NEW.anchor_wall IS NOT NULL AND (
			instr(NEW.anchor_wall, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: item_schedules.anchor_wall must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul163_item_schedules_dtend_ins
BEFORE INSERT ON item_schedules
FOR EACH ROW WHEN NEW.dtend IS NOT NULL AND (
			instr(NEW.dtend, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: item_schedules.dtend must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul163_item_schedules_dtend_upd
BEFORE UPDATE OF dtend ON item_schedules
FOR EACH ROW WHEN NEW.dtend IS NOT NULL AND (
			instr(NEW.dtend, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: item_schedules.dtend must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul163_item_schedules_dtstart_ins
BEFORE INSERT ON item_schedules
FOR EACH ROW WHEN NEW.dtstart IS NOT NULL AND (
			instr(NEW.dtstart, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: item_schedules.dtstart must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul163_item_schedules_dtstart_upd
BEFORE UPDATE OF dtstart ON item_schedules
FOR EACH ROW WHEN NEW.dtstart IS NOT NULL AND (
			instr(NEW.dtstart, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: item_schedules.dtstart must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul163_item_schedules_exdates_ins
BEFORE INSERT ON item_schedules
FOR EACH ROW WHEN NEW.exdates IS NOT NULL AND (
			instr(NEW.exdates, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: item_schedules.exdates must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul163_item_schedules_exdates_upd
BEFORE UPDATE OF exdates ON item_schedules
FOR EACH ROW WHEN NEW.exdates IS NOT NULL AND (
			instr(NEW.exdates, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: item_schedules.exdates must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul163_item_schedules_next_wall_ins
BEFORE INSERT ON item_schedules
FOR EACH ROW WHEN NEW.next_wall IS NOT NULL AND (
			instr(NEW.next_wall, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: item_schedules.next_wall must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul163_item_schedules_next_wall_upd
BEFORE UPDATE OF next_wall ON item_schedules
FOR EACH ROW WHEN NEW.next_wall IS NOT NULL AND (
			instr(NEW.next_wall, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: item_schedules.next_wall must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul163_item_schedules_rdates_ins
BEFORE INSERT ON item_schedules
FOR EACH ROW WHEN NEW.rdates IS NOT NULL AND (
			instr(NEW.rdates, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: item_schedules.rdates must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul163_item_schedules_rdates_upd
BEFORE UPDATE OF rdates ON item_schedules
FOR EACH ROW WHEN NEW.rdates IS NOT NULL AND (
			instr(NEW.rdates, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: item_schedules.rdates must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul163_item_schedules_rrule_ins
BEFORE INSERT ON item_schedules
FOR EACH ROW WHEN NEW.rrule IS NOT NULL AND (
			instr(NEW.rrule, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: item_schedules.rrule must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul163_item_schedules_rrule_upd
BEFORE UPDATE OF rrule ON item_schedules
FOR EACH ROW WHEN NEW.rrule IS NOT NULL AND (
			instr(NEW.rrule, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: item_schedules.rrule must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul163_item_schedules_tz_ins
BEFORE INSERT ON item_schedules
FOR EACH ROW WHEN NEW.tz IS NOT NULL AND (
			instr(NEW.tz, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: item_schedules.tz must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul163_item_schedules_tz_upd
BEFORE UPDATE OF tz ON item_schedules
FOR EACH ROW WHEN NEW.tz IS NOT NULL AND (
			instr(NEW.tz, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: item_schedules.tz must not contain a NUL');
END;

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

