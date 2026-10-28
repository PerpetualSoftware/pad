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

CREATE TRIGGER IF NOT EXISTS pad_nul161_users_timezone_ins
BEFORE INSERT ON users
FOR EACH ROW WHEN NEW.timezone IS NOT NULL AND (
			instr(NEW.timezone, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: users.timezone must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul161_users_timezone_upd
BEFORE UPDATE OF timezone ON users
FOR EACH ROW WHEN NEW.timezone IS NOT NULL AND (
			instr(NEW.timezone, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: users.timezone must not contain a NUL');
END;

