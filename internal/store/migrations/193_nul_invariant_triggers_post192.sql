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

CREATE TRIGGER IF NOT EXISTS pad_nul193_agent_roles_park_mode_ins
BEFORE INSERT ON agent_roles
FOR EACH ROW WHEN NEW.park_mode IS NOT NULL AND (
			instr(NEW.park_mode, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: agent_roles.park_mode must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul193_agent_roles_park_mode_upd
BEFORE UPDATE OF park_mode ON agent_roles
FOR EACH ROW WHEN NEW.park_mode IS NOT NULL AND (
			instr(NEW.park_mode, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: agent_roles.park_mode must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul193_agent_roles_park_set_at_ins
BEFORE INSERT ON agent_roles
FOR EACH ROW WHEN NEW.park_set_at IS NOT NULL AND (
			instr(NEW.park_set_at, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: agent_roles.park_set_at must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul193_agent_roles_park_set_at_upd
BEFORE UPDATE OF park_set_at ON agent_roles
FOR EACH ROW WHEN NEW.park_set_at IS NOT NULL AND (
			instr(NEW.park_set_at, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: agent_roles.park_set_at must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul193_agent_roles_park_set_by_user_id_ins
BEFORE INSERT ON agent_roles
FOR EACH ROW WHEN NEW.park_set_by_user_id IS NOT NULL AND (
			instr(NEW.park_set_by_user_id, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: agent_roles.park_set_by_user_id must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul193_agent_roles_park_set_by_user_id_upd
BEFORE UPDATE OF park_set_by_user_id ON agent_roles
FOR EACH ROW WHEN NEW.park_set_by_user_id IS NOT NULL AND (
			instr(NEW.park_set_by_user_id, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: agent_roles.park_set_by_user_id must not contain a NUL');
END;

