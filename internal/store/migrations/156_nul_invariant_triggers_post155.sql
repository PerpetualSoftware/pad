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

CREATE TRIGGER IF NOT EXISTS pad_nul156_push_devices_app_version_ins
BEFORE INSERT ON push_devices
FOR EACH ROW WHEN NEW.app_version IS NOT NULL AND (
			instr(NEW.app_version, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: push_devices.app_version must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul156_push_devices_app_version_upd
BEFORE UPDATE OF app_version ON push_devices
FOR EACH ROW WHEN NEW.app_version IS NOT NULL AND (
			instr(NEW.app_version, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: push_devices.app_version must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul156_push_devices_connection_id_ins
BEFORE INSERT ON push_devices
FOR EACH ROW WHEN NEW.connection_id IS NOT NULL AND (
			instr(NEW.connection_id, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: push_devices.connection_id must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul156_push_devices_connection_id_upd
BEFORE UPDATE OF connection_id ON push_devices
FOR EACH ROW WHEN NEW.connection_id IS NOT NULL AND (
			instr(NEW.connection_id, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: push_devices.connection_id must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul156_push_devices_token_ins
BEFORE INSERT ON push_devices
FOR EACH ROW WHEN NEW.token IS NOT NULL AND (
			instr(NEW.token, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: push_devices.token must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul156_push_devices_token_upd
BEFORE UPDATE OF token ON push_devices
FOR EACH ROW WHEN NEW.token IS NOT NULL AND (
			instr(NEW.token, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: push_devices.token must not contain a NUL');
END;

