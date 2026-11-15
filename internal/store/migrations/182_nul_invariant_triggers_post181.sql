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

CREATE TRIGGER IF NOT EXISTS pad_nul182_schedule_reopen_checkpoints_after_json_ins
BEFORE INSERT ON schedule_reopen_checkpoints
FOR EACH ROW WHEN NEW.after_json IS NOT NULL AND (
			instr(NEW.after_json, char(0)) > 0
			OR (json_valid(NEW.after_json) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.after_json)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: schedule_reopen_checkpoints.after_json must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul182_schedule_reopen_checkpoints_after_json_upd
BEFORE UPDATE OF after_json ON schedule_reopen_checkpoints
FOR EACH ROW WHEN NEW.after_json IS NOT NULL AND (
			instr(NEW.after_json, char(0)) > 0
			OR (json_valid(NEW.after_json) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.after_json)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: schedule_reopen_checkpoints.after_json must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul182_schedule_reopen_checkpoints_before_json_ins
BEFORE INSERT ON schedule_reopen_checkpoints
FOR EACH ROW WHEN NEW.before_json IS NOT NULL AND (
			instr(NEW.before_json, char(0)) > 0
			OR (json_valid(NEW.before_json) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.before_json)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: schedule_reopen_checkpoints.before_json must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul182_schedule_reopen_checkpoints_before_json_upd
BEFORE UPDATE OF before_json ON schedule_reopen_checkpoints
FOR EACH ROW WHEN NEW.before_json IS NOT NULL AND (
			instr(NEW.before_json, char(0)) > 0
			OR (json_valid(NEW.before_json) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.before_json)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: schedule_reopen_checkpoints.before_json must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul182_schedule_reopen_checkpoints_blocked_code_ins
BEFORE INSERT ON schedule_reopen_checkpoints
FOR EACH ROW WHEN NEW.blocked_code IS NOT NULL AND (
			instr(NEW.blocked_code, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: schedule_reopen_checkpoints.blocked_code must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul182_schedule_reopen_checkpoints_blocked_code_upd
BEFORE UPDATE OF blocked_code ON schedule_reopen_checkpoints
FOR EACH ROW WHEN NEW.blocked_code IS NOT NULL AND (
			instr(NEW.blocked_code, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: schedule_reopen_checkpoints.blocked_code must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul182_schedule_reopen_checkpoints_occurrence_key_ins
BEFORE INSERT ON schedule_reopen_checkpoints
FOR EACH ROW WHEN NEW.occurrence_key IS NOT NULL AND (
			instr(NEW.occurrence_key, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: schedule_reopen_checkpoints.occurrence_key must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul182_schedule_reopen_checkpoints_occurrence_key_upd
BEFORE UPDATE OF occurrence_key ON schedule_reopen_checkpoints
FOR EACH ROW WHEN NEW.occurrence_key IS NOT NULL AND (
			instr(NEW.occurrence_key, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: schedule_reopen_checkpoints.occurrence_key must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul182_schedule_reopen_checkpoints_operation_ins
BEFORE INSERT ON schedule_reopen_checkpoints
FOR EACH ROW WHEN NEW.operation IS NOT NULL AND (
			instr(NEW.operation, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: schedule_reopen_checkpoints.operation must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul182_schedule_reopen_checkpoints_operation_upd
BEFORE UPDATE OF operation ON schedule_reopen_checkpoints
FOR EACH ROW WHEN NEW.operation IS NOT NULL AND (
			instr(NEW.operation, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: schedule_reopen_checkpoints.operation must not contain a NUL');
END;

