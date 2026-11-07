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
-- are those of 084; its header carries the measurements.

CREATE TRIGGER IF NOT EXISTS pad_nul168_item_schedules_outcome_ins
BEFORE INSERT ON item_schedules
FOR EACH ROW WHEN NEW.outcome IS NOT NULL AND (
			instr(NEW.outcome, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: item_schedules.outcome must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul168_item_schedules_outcome_upd
BEFORE UPDATE OF outcome ON item_schedules
FOR EACH ROW WHEN NEW.outcome IS NOT NULL AND (
			instr(NEW.outcome, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: item_schedules.outcome must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul168_schedule_changes_actor_ins
BEFORE INSERT ON schedule_changes
FOR EACH ROW WHEN NEW.actor IS NOT NULL AND (
			instr(NEW.actor, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: schedule_changes.actor must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul168_schedule_changes_actor_upd
BEFORE UPDATE OF actor ON schedule_changes
FOR EACH ROW WHEN NEW.actor IS NOT NULL AND (
			instr(NEW.actor, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: schedule_changes.actor must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul168_schedule_changes_after_json_ins
BEFORE INSERT ON schedule_changes
FOR EACH ROW WHEN NEW.after_json IS NOT NULL AND (
			instr(NEW.after_json, char(0)) > 0
			OR (json_valid(NEW.after_json) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.after_json)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: schedule_changes.after_json must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul168_schedule_changes_after_json_upd
BEFORE UPDATE OF after_json ON schedule_changes
FOR EACH ROW WHEN NEW.after_json IS NOT NULL AND (
			instr(NEW.after_json, char(0)) > 0
			OR (json_valid(NEW.after_json) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.after_json)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: schedule_changes.after_json must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul168_schedule_changes_before_json_ins
BEFORE INSERT ON schedule_changes
FOR EACH ROW WHEN NEW.before_json IS NOT NULL AND (
			instr(NEW.before_json, char(0)) > 0
			OR (json_valid(NEW.before_json) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.before_json)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: schedule_changes.before_json must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul168_schedule_changes_before_json_upd
BEFORE UPDATE OF before_json ON schedule_changes
FOR EACH ROW WHEN NEW.before_json IS NOT NULL AND (
			instr(NEW.before_json, char(0)) > 0
			OR (json_valid(NEW.before_json) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.before_json)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: schedule_changes.before_json must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul168_schedule_changes_occurrence_key_ins
BEFORE INSERT ON schedule_changes
FOR EACH ROW WHEN NEW.occurrence_key IS NOT NULL AND (
			instr(NEW.occurrence_key, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: schedule_changes.occurrence_key must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul168_schedule_changes_occurrence_key_upd
BEFORE UPDATE OF occurrence_key ON schedule_changes
FOR EACH ROW WHEN NEW.occurrence_key IS NOT NULL AND (
			instr(NEW.occurrence_key, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: schedule_changes.occurrence_key must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul168_schedule_changes_reason_ins
BEFORE INSERT ON schedule_changes
FOR EACH ROW WHEN NEW.reason IS NOT NULL AND (
			instr(NEW.reason, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: schedule_changes.reason must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul168_schedule_changes_reason_upd
BEFORE UPDATE OF reason ON schedule_changes
FOR EACH ROW WHEN NEW.reason IS NOT NULL AND (
			instr(NEW.reason, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: schedule_changes.reason must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul168_schedule_changes_role_ins
BEFORE INSERT ON schedule_changes
FOR EACH ROW WHEN NEW.role IS NOT NULL AND (
			instr(NEW.role, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: schedule_changes.role must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul168_schedule_changes_role_upd
BEFORE UPDATE OF role ON schedule_changes
FOR EACH ROW WHEN NEW.role IS NOT NULL AND (
			instr(NEW.role, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: schedule_changes.role must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul168_schedule_occurrence_overrides_occurrence_key_ins
BEFORE INSERT ON schedule_occurrence_overrides
FOR EACH ROW WHEN NEW.occurrence_key IS NOT NULL AND (
			instr(NEW.occurrence_key, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: schedule_occurrence_overrides.occurrence_key must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul168_schedule_occurrence_overrides_occurrence_key_upd
BEFORE UPDATE OF occurrence_key ON schedule_occurrence_overrides
FOR EACH ROW WHEN NEW.occurrence_key IS NOT NULL AND (
			instr(NEW.occurrence_key, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: schedule_occurrence_overrides.occurrence_key must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul168_schedule_occurrence_overrides_role_ins
BEFORE INSERT ON schedule_occurrence_overrides
FOR EACH ROW WHEN NEW.role IS NOT NULL AND (
			instr(NEW.role, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: schedule_occurrence_overrides.role must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul168_schedule_occurrence_overrides_role_upd
BEFORE UPDATE OF role ON schedule_occurrence_overrides
FOR EACH ROW WHEN NEW.role IS NOT NULL AND (
			instr(NEW.role, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: schedule_occurrence_overrides.role must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul168_schedule_occurrence_overrides_value_wall_ins
BEFORE INSERT ON schedule_occurrence_overrides
FOR EACH ROW WHEN NEW.value_wall IS NOT NULL AND (
			instr(NEW.value_wall, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: schedule_occurrence_overrides.value_wall must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul168_schedule_occurrence_overrides_value_wall_upd
BEFORE UPDATE OF value_wall ON schedule_occurrence_overrides
FOR EACH ROW WHEN NEW.value_wall IS NOT NULL AND (
			instr(NEW.value_wall, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: schedule_occurrence_overrides.value_wall must not contain a NUL');
END;

